package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"
)

// SFS2X system-controller action ids actually used by this game (dossier §04.1).
// actionHandshake=0 is confirmed present in the game's own Smartfox2xLw.decompiled.cs
// (SendHandshakeRequest/HandshakeRequest) but was never exercised by this Go
// client until the sfs2x-api@1.8.6 comparison pass -- see DoHandshake.
const (
	actionHandshake     = 0
	actionLogin         = 1
	actionCallExtension = 13
	actionPingPong      = 29
)

const (
	controllerSystem    = 0
	controllerExtension = 1
)

// Envelope is the decoded outer {c,a,p} wrapper for both directions.
type Envelope struct {
	Controller byte
	Action     int16
	Content    *SFSObject
}

// GameConn is a raw (plaintext, connectionType 0) SFS2X socket connection.
type GameConn struct {
	conn   net.Conn
	reader *bufio.Reader
	wmu    sync.Mutex

	stopHeartbeat chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

// writeTimeout bounds every socket write via SendEnvelope. Without it, a
// half-open connection can block Write indefinitely while holding wmu --
// which also wedges the heartbeat goroutine (it shares the same mutex),
// turning a stalled connection into a silent, unbounded hang instead of a
// surfaced error.
const writeTimeout = 10 * time.Second

func DialGame(addr string, timeout time.Duration) (*GameConn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &GameConn{
		conn:   conn,
		reader: bufio.NewReaderSize(conn, 64*1024),
	}, nil
}

// Close is idempotent: the underlying net.Conn.Close() call, like the stopHeartbeat channel
// close, lives inside closeOnce so it only actually runs once no matter how many times or how
// concurrently Close() is called (e.g. StartHeartbeat's own error-branch c.Close() racing the
// main goroutine's error-path c.Close() after a failed blocked read). Its result is captured into
// closeErr from inside the Once and returned on every call -- including calls after the first,
// which skip the Do body entirely -- so the return value is genuinely idempotent too: a second
// Close() reports the same outcome as the first, rather than a spurious "use of closed network
// connection" from re-invoking the socket's Close() a second time. sync.Once.Do's happens-before
// guarantee (every Do call blocks until the function, if any, has finished running) makes reading
// closeErr after Do returns race-free even when multiple goroutines call Close() concurrently.
func (c *GameConn) Close() error {
	c.closeOnce.Do(func() {
		if c.stopHeartbeat != nil {
			close(c.stopHeartbeat)
		}
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// SendEnvelope builds the outer {c,a,p} SFSObject, serializes, frames, and
// writes it to the socket. Safe for concurrent use (heartbeat + main flow).
func (c *GameConn) SendEnvelope(controller byte, action int16, content *SFSObject) error {
	outer := NewSFSObject()
	outer.PutByte("c", controller)
	outer.PutShort("a", action)
	outer.PutSFSObject("p", content)

	body, err := EncodeObject(outer)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	packet, err := EncodePacket(body)
	if err != nil {
		return fmt.Errorf("encode packet: %w", err)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	_, err = c.conn.Write(packet)
	c.conn.SetWriteDeadline(time.Time{})
	return err
}

// SendExtension sends a client->server `cmd` extension request, matching
// SFSNetwork.SendMessage(cmd, ...) -- dossier §04/§06.
func (c *GameConn) SendExtension(cmd string, params *SFSObject) error {
	if params == nil {
		params = NewSFSObject()
	}
	extContent := NewSFSObject()
	extContent.PutUtfString("c", cmd)
	extContent.PutInt("r", -1)
	extContent.PutSFSObject("p", params)
	return c.SendEnvelope(controllerExtension, actionCallExtension, extContent)
}

// ReadEnvelope blocks until the next framed packet arrives and decodes it.
func (c *GameConn) ReadEnvelope() (*Envelope, error) {
	body, err := ReadPacket(c.reader)
	if err != nil {
		return nil, err
	}
	obj, err := DecodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	env := &Envelope{}
	if v, ok := obj.Get("c"); ok {
		if b, ok := v.Val.(byte); ok {
			env.Controller = b
		}
	}
	if v, ok := obj.Get("a"); ok {
		if s, ok := v.Val.(int16); ok {
			env.Action = s
		}
	}
	if v, ok := obj.Get("p"); ok {
		if p, ok := v.Val.(*SFSObject); ok {
			env.Content = p
		}
	}
	return env, nil
}

// ExtensionMessage is a decoded server->client `cmd` push/response.
type ExtensionMessage struct {
	Cmd    string
	Params *SFSObject
}

// AsExtension interprets an Envelope as an extension message (controller=1),
// mirroring MessageDispather's split of the "p" content into cmd + params.
func (e *Envelope) AsExtension() (*ExtensionMessage, bool) {
	if e.Controller != controllerExtension || e.Content == nil {
		return nil, false
	}
	cmd := e.Content.GetString("c")
	var params *SFSObject
	if v, ok := e.Content.Get("p"); ok {
		if p, ok := v.Val.(*SFSObject); ok {
			params = p
		}
	}
	if params == nil {
		params = NewSFSObject()
	}
	return &ExtensionMessage{Cmd: cmd, Params: params}, true
}

// benignErrorCodes are server errorCode values documented (in the callers' own doc comments) as
// a normal, well-formed "nothing to do right now" outcome rather than a real failure: a
// production cooldown, an already-claimed daily reward, a visitor that hasn't arrived yet.
// Logged at Warn (still worth seeing) but not treated as a failure for aggregation/exit-code
// purposes, so a routine daily re-run doesn't make -collect's exit code permanently noisy.
//
// Each errorCode is scoped to the specific cmd(s) it's actually documented for (see the inline
// comments below) -- classifyResponse only treats an errorCode as benign when msg.Cmd is one of
// its listed cmds. Without this scoping, a genuine failure on an unrelated command that happens
// to reuse one of these numeric errorCode values would be silently reclassified from
// outcomeFailure to outcomeBenign, undermining the aggregated-error/exit-code signal every
// orchestration loop (CollectAll/ClaimAllMail/GreetVisitors/ClaimAllianceGifts) relies on.
var benignErrorCodes = map[string][]string{
	"602026":             {"building.production.collect"},                     // buildings.go: "In production, please be patient."
	"120289":             {"vip.add.login.score", "vip.get.every.day.reward"}, // vip.go: "no score"/"no reward" -- already claimed today
	"visitor_err_coming": {"visitor.operate"},                                 // visitors.go: visitor not yet arrived/greetable
	"120471":             {"al.science.donate"},                               // alliance.go: al.science.donate cooldown -- "Donate science CD time is not finish"
}

// commandOutcome classifies a collect/claim response into one of three buckets: a real success,
// a benign no-op (an expected cooldown/already-claimed/not-yet-arrived errorCode, or -- for
// building.production.collect specifically -- a status=0 response with no errorCode; buildings.go's
// own doc comments treat status=1, not just errorCode-absence, as the real proof a collection
// succeeded, and docs/live-validation.mdx only ever observed status=1 or errorCode=602026 from that
// command). Other command families have no documented evidence of ever emitting a status field, so
// the status=0 heuristic does not apply to them and they fall through to outcomeSuccess instead.
type commandOutcome int

const (
	outcomeSuccess commandOutcome = iota
	outcomeBenign
	outcomeFailure
)

// classifyResponse determines a response's outcome and, for a benign or real failure, the
// errorCode (empty string for the building.production.collect status=0-with-no-errorCode benign
// case). An errorCode present in benignErrorCodes is only outcomeBenign when msg.Cmd matches one
// of that code's documented cmds (see benignErrorCodes' doc comment) -- mirroring how the
// status=0 heuristic above is itself scoped to building.production.collect -- so a genuine
// failure on an unrelated cmd that happens to share one of these numeric errorCode values still
// falls through to outcomeFailure. This is the single place both logCommandResult and sendAndWait
// derive their behavior from, so the two can never drift out of sync with each other.
//
// Round 29: the status check below uses requireFieldType (buildings.go), not a bare
// Has("status")+GetInt("status")==0 pair, for the same reason requireFieldType exists at all --
// GetInt silently coerces ANY non-int-shaped value to int32(0), so a present-but-wrong-typed
// status field used to satisfy Has()+GetInt()==0 exactly like a genuine status=0 would, folding a
// malformed/wrong-typed response into this same benign bucket. requireFieldType treats a
// wrong-typed status exactly like a missing one -- false here, falling through to outcomeSuccess
// -- so only a status field that actually decoded as an int, and is genuinely 0, takes this
// branch. See TestClassifyResponseWrongTypedStatusIsNotBenign (conn_test.go).
func classifyResponse(msg *ExtensionMessage) (commandOutcome, string) {
	ec, has := msg.Params.Get("errorCode")
	if !has {
		if msg.Cmd == "building.production.collect" &&
			requireFieldType(msg.Params, "status", "building.production.collect", sfsFieldKindInt) &&
			msg.Params.GetInt("status") == 0 {
			return outcomeBenign, ""
		}
		return outcomeSuccess, ""
	}
	code := fmt.Sprintf("%v", ec.Val)
	if cmds, ok := benignErrorCodes[code]; ok && slices.Contains(cmds, msg.Cmd) {
		return outcomeBenign, code
	}
	return outcomeFailure, code
}

// logCommandResult logs a collect/claim command's response at a severity that reflects whether
// it actually succeeded: Info on success, Warn on a recognized/expected failure, Error on
// anything else.
func logCommandResult(label string, msg *ExtensionMessage) {
	switch outcome, code := classifyResponse(msg); outcome {
	case outcomeSuccess:
		slog.Info(label, "cmd", msg.Cmd, "response", msg.Params.String())
	case outcomeBenign:
		if code != "" {
			slog.Warn(label+" no-op (expected)", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.String())
		} else {
			slog.Warn(label+" no-op (status=0, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.String())
		}
	case outcomeFailure:
		slog.Error(label+" failed", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.StringRedacted())
	}
}

const defaultCmdTimeout = 8 * time.Second

// sendStageError wraps a failure from sendAndWait's send stage (conn.SendExtension, which
// ultimately calls SendEnvelope's c.conn.Write under a writeTimeout write deadline) so it can
// never be confused with waitForCmd's benign wait-stage timeout outcome (deadlineExceededError,
// login.go). Per Go's net.Conn contract, a deadline-exceeded Write returns a *net.OpError that
// already satisfies net.Error with Timeout()==true -- identical, as far as the
// errors.As(&netErr)+!netErr.Timeout() early-abort checks in buildings.go/mail.go/alliance.go/
// visitors.go are concerned, to sendAndWait's own ordinary "no matching response within
// defaultCmdTimeout" outcome. Left unwrapped, a connection so broken it can't even send a request
// would be silently treated as "the response just hasn't arrived yet, keep going" instead of "the
// connection is dead, abort" -- backwards from what actually happened: SendEnvelope's write
// deadline exists specifically to bound how long a half-open connection can hang (see its own doc
// comment), and a write-side failure means the send itself never got out, not that a well-sent
// request's response was merely slow.
//
// A failed send is unconditionally treated as a genuine connection failure regardless of the
// underlying cause (write-deadline exceeded, connection reset, or even a local encode error from
// deeper in SendExtension/SendEnvelope) -- unlike packet.go's deadConnError, which only activates
// for the specific EOF/ErrUnexpectedEOF shapes wrapIfClosed recognizes, this wrapper is applied
// unconditionally to sendAndWait's entire send-stage branch. Mirrors deadConnError's net.Error-
// shaping technique (a small unexported struct forcing Timeout()==false/Temporary()==false) and
// wraps -- never replaces -- the original error via Unwrap, so errors.Is/errors.As against the
// underlying cause (e.g. a specific *net.OpError) keep working through this wrapper. Only applied
// to the write-error branch below; waitForCmd's read/wait-side timeout behavior is untouched.
//
// Round 29: also used by DoHandshake's own send-stage branch (its c.SendEnvelope call, below) --
// an independent send path that shares SendEnvelope/writeTimeout with sendAndWait but was missed
// when this type was introduced, leaving DoHandshake's send failures unwrapped while its
// read-side branches (the wall-clock deadline check, and ReadEnvelope failures via packet.go's
// wrapIfClosed/deadConnError) were already hardened. See
// TestDoHandshakeSendFailureIsNonTimeoutNetError (conn_handshake_test.go).
type sendStageError struct {
	err error
}

func (e sendStageError) Error() string { return "send: " + e.err.Error() }
func (e sendStageError) Unwrap() error { return e.err }
func (sendStageError) Timeout() bool   { return false }
func (sendStageError) Temporary() bool { return false }

// sendAndWait sends a command and waits for its response, logging the outcome via
// logCommandResult and returning an error if the send/wait itself failed or classifyResponse
// says the response was a genuine failure. This is the single dedup point for the near-identical
// send+wait+log block that used to be copy-pasted at (almost) every collect/claim call site
// across this file and buildings.go/mail.go/alliance.go/vip.go/visitors.go. waitCmds defaults
// to [sendCmd]; pass an explicit value when the response arrives under a different command name
// (e.g. mail.go's push.chat.get.system.mails).
func sendAndWait(conn *GameConn, label, sendCmd string, params *SFSObject, waitCmds ...string) (*ExtensionMessage, error) {
	if err := conn.SendExtension(sendCmd, params); err != nil {
		wrapped := sendStageError{err: err}
		slog.Error(label+" send failed", "cmd", sendCmd, "error", wrapped)
		return nil, wrapped
	}
	if len(waitCmds) == 0 {
		waitCmds = []string{sendCmd}
	}
	msg, err := waitForCmd(conn, defaultCmdTimeout, waitCmds...)
	if err != nil {
		slog.Error(label+" no response", "cmd", sendCmd, "error", err)
		return nil, err
	}
	logCommandResult(label, msg)
	if outcome, code := classifyResponse(msg); outcome == outcomeFailure {
		return msg, fmt.Errorf("%s: errorCode=%v", label, code)
	}
	return msg, nil
}

// DoHandshake sends the vanilla SFS2X pre-Login Handshake request
// (HandshakeRequest.KEY_API="api", KEY_CLIENT_TYPE="cl") and waits for the
// response. Confirmed present in this game's own decompiled SDK
// (Smartfox2xLw.decompiled.cs:4363 SendHandshakeRequest, called from
// OnSocketConnect before any Login) but this Go client never sent it --
// every login attempt so far (guest, email-bind, and every reconnect
// variant) skipped straight to Login. Experimental: guest/email-bind work
// fine without it, so it's not obviously required, but it's cheap to send
// and untested for the still-open init-push/reconnect-block questions.
// api="1.7.8" cl="Unity" match the game's own SDK defaults exactly
// (Smartfox2xLw.decompiled.cs:3613-3621).
func (c *GameConn) DoHandshake(timeout time.Duration) (*SFSObject, error) {
	req := NewSFSObject()
	req.PutUtfString("api", "1.7.8")
	req.PutUtfString("cl", "Unity")
	if err := c.SendEnvelope(controllerSystem, actionHandshake, req); err != nil {
		// sendStageError (above): forces Timeout()==false even if the underlying write failure
		// itself reports Timeout()==true (e.g. SendEnvelope's own writeTimeout deadline), so this
		// branch can never be confused with the wall-clock-deadline-elapsed branch below, which
		// legitimately IS a Timeout()==true net.Error (deadlineExceededError). Round 29: this
		// send-stage branch was the one DoHandshake error path still missing this treatment --
		// see TestDoHandshakeSendFailureIsNonTimeoutNetError (conn_handshake_test.go).
		return nil, sendStageError{err: fmt.Errorf("send handshake: %w", err)}
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// deadlineExceededError (login.go): satisfies net.Error with Timeout()==true, same as
			// waitFor's identical wall-clock-elapsed-after-a-skipped-envelope branch (round 23).
			// A bare fmt.Errorf here would reproduce that exact bug in this independent read loop
			// -- see TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope below.
			return nil, deadlineExceededError{}
		}
		c.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := c.ReadEnvelope()
		if err != nil {
			return nil, fmt.Errorf("read handshake response: %w", err)
		}
		if env.Controller == controllerSystem && env.Action == actionHandshake {
			if env.Content == nil {
				return nil, fmt.Errorf("HANDSHAKE FAILED: response had no p payload")
			}
			if ec, ok := env.Content.Get("ec"); ok {
				// Wrapped in ErrAuthRejected (defined in errors.go) so callers can
				// distinguish "server actively rejected this handshake" (ec present)
				// from a bare dial/timeout/I/O failure above, which stay unwrapped --
				// same pattern as login.go's LOGIN FAILED and crossserver.go's
				// CROSS-SERVER LOGIN FAILED errors.
				return nil, fmt.Errorf("HANDSHAKE FAILED: ec=%v full=%s: %w", ec.Val, env.Content.StringRedacted(), ErrAuthRejected)
			}
			return env.Content, nil
		}
		// Anything else this early (unlikely, but be tolerant) is logged and skipped rather
		// than treated as a protocol violation. Content may legitimately be nil here too.
		// Uses StringRedacted (not String): a skipped envelope can legitimately carry a live
		// credential -- e.g. an out-of-order push.account.login.new-shaped payload arriving
		// before the real handshake response -- and this is exactly the site round-11's
		// credential_leak_lint_test.go doc comment named as a known, deliberately-uncaught
		// gap (contentStr was a local variable, not an inline .String() call, so the lint
		// regex missed it). See sfsobject.go's sensitiveSFSKeys/StringRedacted and
		// TestDoHandshakeSkipRedactsCredentialFields (conn_handshake_test.go) for coverage.
		contentStr := "<nil>"
		if env.Content != nil {
			contentStr = env.Content.StringRedacted()
		}
		slog.Info("skipped envelope while waiting for handshake", "controller", env.Controller, "action", env.Action, "content", contentStr)
	}
}

// StartHeartbeat launches the PingPongRequest loop (dossier §04: every
// 4000ms, {clientTime: ms}), required to avoid the ~12s server-perceived
// timeout while we wait on slower steps (e.g. the user fetching an email
// verification code).
func (c *GameConn) StartHeartbeat(interval time.Duration, start time.Time) {
	c.stopHeartbeat = make(chan struct{})
	stopCh := c.stopHeartbeat // snapshot: avoids racing Close()'s concurrent access to the field
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				pp := NewSFSObject()
				pp.PutLong("clientTime", time.Since(start).Milliseconds())
				if err := c.SendEnvelope(controllerSystem, actionPingPong, pp); err != nil {
					slog.Error("heartbeat send failed -- closing connection", "error", err)
					c.Close()
					return
				}
			}
		}
	}()
}
