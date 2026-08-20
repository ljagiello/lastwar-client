package session

import (
	"bufio"
	"errors"
	"fmt"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"
)

// SFS2X system-controller action ids actually used by this game (dossier §04.1).
// ActionHandshake=0 is confirmed present in the game's own Smartfox2xLw.decompiled.cs
// (SendHandshakeRequest/HandshakeRequest) but was never exercised by this Go
// client until the sfs2x-api@1.8.6 comparison pass -- see DoHandshake.
const (
	ActionHandshake     = 0
	ActionLogin         = 1
	ActionCallExtension = 13
	ActionPingPong      = 29
)

const (
	ControllerSystem    = 0
	ControllerExtension = 1
)

// Envelope is the decoded outer {c,a,p} wrapper for both directions.
type Envelope struct {
	Controller byte
	Action     int16
	Content    *sfs.SFSObject
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

// NewGameConnForTest wraps an already-connected net.Conn (a net.Pipe end or a fake) in a GameConn,
// mirroring DialGame's own field setup but over a caller-supplied transport. It exists so tests in
// other internal packages -- which cannot reach GameConn's unexported fields -- can build a
// GameConn around a fake connection.
func NewGameConnForTest(c net.Conn) *GameConn {
	return &GameConn{conn: c, reader: bufio.NewReaderSize(c, 4096)}
}

// RawConn returns the underlying net.Conn. SetRawConn replaces it. Both are low-level hooks used by
// tests in other internal packages to inject raw bytes, close the transport out from under a
// GameConn, or wrap it in a failure-injecting net.Conn -- none of which they can do through
// GameConn's unexported field directly.
func (c *GameConn) RawConn() net.Conn     { return c.conn }
func (c *GameConn) SetRawConn(n net.Conn) { c.conn = n }

func (c *GameConn) Close() error {
	c.closeOnce.Do(func() {
		if c.stopHeartbeat != nil {
			close(c.stopHeartbeat)
		}
		c.closeErr = c.conn.Close()
	})
	return c.closeErr
}

// SetReadDeadline sets the read deadline on the underlying connection. Callers in higher packages
// (the login/cross-server handshakes and FetchBuildings' init-push loop) use it to bound or clear
// a read without reaching into GameConn's unexported net.Conn.
func (c *GameConn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// SendEnvelope builds the outer {c,a,p} sfs.SFSObject, serializes, frames, and
// writes it to the socket. Safe for concurrent use (heartbeat + main flow).
func (c *GameConn) SendEnvelope(controller byte, action int16, content *sfs.SFSObject) error {
	outer := sfs.NewSFSObject()
	outer.PutByte("c", controller)
	outer.PutShort("a", action)
	outer.PutSFSObject("p", content)

	body, err := sfs.EncodeObject(outer)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	packet, err := sfs.EncodePacket(body)
	if err != nil {
		return fmt.Errorf("encode packet: %w", err)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(writeTimeout)); err != nil {
		return fmt.Errorf("set write deadline: %w", err)
	}
	_, err = c.conn.Write(packet)
	_ = c.conn.SetWriteDeadline(time.Time{})
	return err
}

// SendExtension sends a client->server `cmd` extension request, matching
// SFSNetwork.SendMessage(cmd, ...) -- dossier §04/§06.
func (c *GameConn) SendExtension(cmd string, params *sfs.SFSObject) error {
	if params == nil {
		params = sfs.NewSFSObject()
	}
	extContent := sfs.NewSFSObject()
	extContent.PutUtfString("c", cmd)
	extContent.PutInt("r", -1)
	extContent.PutSFSObject("p", params)
	return c.SendEnvelope(ControllerExtension, ActionCallExtension, extContent)
}

// ReadEnvelope blocks until the next framed packet arrives and decodes it.
func (c *GameConn) ReadEnvelope() (*Envelope, error) {
	body, err := sfs.ReadPacket(c.reader)
	if err != nil {
		return nil, err
	}
	obj, err := sfs.DecodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	env := &Envelope{}
	// Round-40 fix: all three fields below used to silently coerce a present-but-wrong-typed
	// value to its Go zero value with zero diagnostic, unlike every other field-read site in this
	// codebase (gsl.go's gsl.FindServerInfo, buildings.go's RequireFieldType, etc.). This mattered in
	// a real, not just theoretical, way: Controller's zero value (0) is ControllerSystem, and
	// Action's zero value (0) is ActionHandshake -- so a wrong-typed "c"/"a" used to be
	// indistinguishable from a genuine system/Handshake envelope, spuriously satisfying
	// DoHandshake's success check below. A genuinely-absent field stays silent by the same
	// convention this codebase applies everywhere else -- only wrong-typed values warn here.
	if v, ok := obj.Get("c"); ok {
		if b, ok := v.Val.(byte); ok {
			env.Controller = b
		} else {
			slog.Warn("ReadEnvelope: c field is present but not a byte", "type", fmt.Sprintf("%T", v.Val))
		}
	}
	if v, ok := obj.Get("a"); ok {
		if s, ok := v.Val.(int16); ok {
			env.Action = s
		} else {
			slog.Warn("ReadEnvelope: a field is present but not an int16", "type", fmt.Sprintf("%T", v.Val))
		}
	}
	if v, ok := obj.Get("p"); ok {
		if p, ok := v.Val.(*sfs.SFSObject); ok {
			env.Content = p
		} else {
			slog.Warn("ReadEnvelope: p field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
		}
	}
	return env, nil
}

// ExtensionMessage is a decoded server->client `cmd` push/response.
type ExtensionMessage struct {
	Cmd    string
	Params *sfs.SFSObject
}

// AsExtension interprets an Envelope as an extension message (controller=1),
// mirroring MessageDispather's split of the "p" content into cmd + params.
func (e *Envelope) AsExtension() (*ExtensionMessage, bool) {
	if e.Controller != ControllerExtension || e.Content == nil {
		return nil, false
	}
	// Round-43 fix: cmd used to come from a plain GetString("c"), which silently coerces a
	// present-but-wrong-typed "c" to "" with zero diagnostic -- unlike the "p" field two lines
	// below (already hardened) and unlike ReadEnvelope's own identical c/a/p triple one level up
	// (round-40 fix, whose own doc comment describes this exact bug class). A coerced cmd=""
	// matches none of the real dispatch keys every caller of AsExtension checks against
	// (waitForInitPush's "init", WaitForCmd's wantCmds, FetchBuildings' switch), so a genuinely-
	// arrived, well-formed response with a wrong-typed "c" field was silently treated as an
	// unrecognized push and logged only at Debug -- indistinguishable from an ordinary timeout,
	// with no signal that a response actually arrived but was misclassified.
	var cmd string
	if v, ok := e.Content.Get("c"); ok {
		if s, ok := v.Val.(string); ok {
			cmd = s
		} else {
			slog.Warn("AsExtension: c field is present but not a string", "type", fmt.Sprintf("%T", v.Val))
		}
	}
	var params *sfs.SFSObject
	if v, ok := e.Content.Get("p"); ok {
		if p, ok := v.Val.(*sfs.SFSObject); ok {
			params = p
		} else {
			slog.Warn("AsExtension: p field is present but not an object", "cmd", cmd, "type", fmt.Sprintf("%T", v.Val))
		}
	}
	if params == nil {
		params = sfs.NewSFSObject()
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
// falls through to outcomeFailure. This is the single place both logCommandResult and SendAndWait
// derive their behavior from, so the two can never drift out of sync with each other.
//
// Round 29: the status check below uses RequireFieldType (buildings.go), not a bare
// Has("status")+GetInt("status")==0 pair, for the same reason RequireFieldType exists at all --
// GetInt silently coerces ANY non-int-shaped value to int32(0), so a present-but-wrong-typed
// status field used to satisfy Has()+GetInt()==0 exactly like a genuine status=0 would, folding a
// malformed/wrong-typed response into this same benign bucket. RequireFieldType treats a
// wrong-typed status exactly like a missing one -- false here, falling through to outcomeSuccess
// -- so only a status field that actually decoded as an int, and is genuinely 0, takes this
// branch. See TestClassifyResponseWrongTypedStatusIsNotBenign (conn_test.go).
func classifyResponse(msg *ExtensionMessage) (commandOutcome, string) {
	ec, has := msg.Params.Get("errorCode")
	if !has {
		if msg.Cmd == "building.production.collect" &&
			RequireFieldType(msg.Params, "status", "building.production.collect", SFSFieldKindInt) &&
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

const DefaultCmdTimeout = 8 * time.Second

// SendStageError wraps a failure from SendAndWait's send stage (conn.SendExtension, which
// ultimately calls SendEnvelope's c.conn.Write under a writeTimeout write deadline) so it can
// never be confused with WaitForCmd's benign wait-stage timeout outcome (DeadlineExceededError,
// login.go). Per Go's net.Conn contract, a deadline-exceeded Write returns a *net.OpError that
// already satisfies net.Error with Timeout()==true -- identical, as far as the
// errors.As(&netErr)+!netErr.Timeout() early-abort checks in buildings.go/mail.go/alliance.go/
// visitors.go are concerned, to SendAndWait's own ordinary "no matching response within
// DefaultCmdTimeout" outcome. Left unwrapped, a connection so broken it can't even send a request
// would be silently treated as "the response just hasn't arrived yet, keep going" instead of "the
// connection is dead, abort" -- backwards from what actually happened: SendEnvelope's write
// deadline exists specifically to bound how long a half-open connection can hang (see its own doc
// comment), and a write-side failure means the send itself never got out, not that a well-sent
// request's response was merely slow.
//
// A failed send is unconditionally treated as a genuine connection failure regardless of the
// underlying cause (write-deadline exceeded, connection reset, or even a local encode error from
// deeper in SendExtension/SendEnvelope) -- unlike packet.go's sfs.DeadConnError, which only activates
// for the specific EOF/ErrUnexpectedEOF shapes sfs.WrapIfClosed recognizes, this wrapper is applied
// unconditionally to SendAndWait's entire send-stage branch. Mirrors sfs.DeadConnError's net.Error-
// shaping technique (a small unexported struct forcing Timeout()==false/Temporary()==false) and
// wraps -- never replaces -- the original error via Unwrap, so errors.Is/errors.As against the
// underlying cause (e.g. a specific *net.OpError) keep working through this wrapper. Only applied
// to the write-error branch below; WaitForCmd's read/wait-side timeout behavior is untouched.
//
// Round 29: also used by DoHandshake's own send-stage branch (its c.SendEnvelope call, below) --
// an independent send path that shares SendEnvelope/writeTimeout with SendAndWait but was missed
// when this type was introduced, leaving DoHandshake's send failures unwrapped while its
// read-side branches (the wall-clock deadline check, and ReadEnvelope failures via packet.go's
// sfs.WrapIfClosed/sfs.DeadConnError) were already hardened. See
// TestDoHandshakeSendFailureIsNonTimeoutNetError (conn_handshake_test.go).
type SendStageError struct {
	Err error
}

func (e SendStageError) Error() string { return "send: " + e.Err.Error() }
func (e SendStageError) Unwrap() error { return e.Err }
func (SendStageError) Timeout() bool   { return false }
func (SendStageError) Temporary() bool { return false }

// SendAndWait sends a command and waits for its response, logging the outcome via
// logCommandResult and returning an error if the send/wait itself failed or classifyResponse
// says the response was a genuine failure. This is the single dedup point for the near-identical
// send+wait+log block that used to be copy-pasted at (almost) every collect/claim call site
// across this file and buildings.go/mail.go/alliance.go/vip.go/visitors.go. waitCmds defaults
// to [sendCmd]; pass an explicit value when the response arrives under a different command name
// (e.g. mail.go's push.chat.get.system.mails).
func SendAndWait(conn *GameConn, label, sendCmd string, params *sfs.SFSObject, waitCmds ...string) (*ExtensionMessage, error) {
	if err := conn.SendExtension(sendCmd, params); err != nil {
		wrapped := SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", sendCmd, "error", wrapped)
		return nil, wrapped
	}
	if len(waitCmds) == 0 {
		waitCmds = []string{sendCmd}
	}
	msg, err := WaitForCmd(conn, DefaultCmdTimeout, waitCmds...)
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
func (c *GameConn) DoHandshake(timeout time.Duration) (*sfs.SFSObject, error) {
	req := sfs.NewSFSObject()
	req.PutUtfString("api", "1.7.8")
	req.PutUtfString("cl", "Unity")
	if err := c.SendEnvelope(ControllerSystem, ActionHandshake, req); err != nil {
		// SendStageError (above): forces Timeout()==false even if the underlying write failure
		// itself reports Timeout()==true (e.g. SendEnvelope's own writeTimeout deadline), so this
		// branch can never be confused with the wall-clock-deadline-elapsed branch below, which
		// legitimately IS a Timeout()==true net.Error (DeadlineExceededError). Round 29: this
		// send-stage branch was the one DoHandshake error path still missing this treatment --
		// see TestDoHandshakeSendFailureIsNonTimeoutNetError (conn_handshake_test.go).
		return nil, SendStageError{Err: fmt.Errorf("send handshake: %w", err)}
	}
	deadline := time.Now().Add(timeout)
	consecutiveDecodeFailures := 0
	nonMatchingEnvelopes := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// DeadlineExceededError (login.go): satisfies net.Error with Timeout()==true, same as
			// WaitFor's identical wall-clock-elapsed-after-a-skipped-envelope branch (round 23).
			// A bare fmt.Errorf here would reproduce that exact bug in this independent read loop
			// -- see TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope below.
			return nil, DeadlineExceededError{}
		}
		_ = c.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := c.ReadEnvelope()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// A genuine per-read timeout -- return it immediately, matching this
				// function's pre-existing behavior and WaitFor's identical round-49
				// reasoning (login.go) for not merely looping back to the top-of-loop
				// deadline check here.
				return nil, fmt.Errorf("read handshake response: %w", err)
			}
			if ContainsNonTimeoutNetError(err) {
				return nil, fmt.Errorf("read handshake response: %w", err)
			}
			// Round-49 fix: a plain, non-net.Error ReadEnvelope failure (e.g. a
			// sfs.DecodeObject parse failure on one malformed/unrelated push) means
			// sfs.ReadPacket already fully consumed that frame's bytes off the wire before
			// sfs.DecodeObject ever ran -- the stream stays in sync, so this is not
			// evidence the connection is dead, mirroring login.go's identical
			// waitForInitPush/WaitFor fixes and buildings.go's FetchBuildings fix.
			// Previously this loop returned immediately on ANY such error instead of
			// simply skipping the one malformed push and continuing to wait for the
			// real handshake response, the same tolerance this loop already extends to
			// a successfully-decoded-but-non-matching envelope a few lines below.
			consecutiveDecodeFailures++
			if consecutiveDecodeFailures > MaxConsecutiveDecodeFailures {
				// sfs.DeadConnError (packet.go): round-51 fix -- mirrors login.go's identical
				// WaitFor/waitForInitPush fix. This give-up error is never itself a net.Error
				// by construction (reached only after both the Timeout()==true check and
				// ContainsNonTimeoutNetError(err) above already ruled that out), so without
				// this wrap it was silently misclassified as a benign failure by any caller
				// checking ContainsNonTimeoutNetError/errors.As(&netErr)&&!netErr.Timeout().
				return nil, sfs.NewDeadConnError(fmt.Errorf("DoHandshake: %d consecutive malformed/undecodable envelopes, giving up: %w", consecutiveDecodeFailures, err))
			}
			slog.Warn("DoHandshake: failed to read/decode an envelope while waiting; continuing to wait, not treating this as a dead connection", "error", err, "consecutiveDecodeFailures", consecutiveDecodeFailures)
			continue
		}
		consecutiveDecodeFailures = 0
		if env.Controller == ControllerSystem && env.Action == ActionHandshake {
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
		// MaxNonMatchingEnvelopesPerWait (login.go doc comment): bounds how many well-formed
		// but non-matching envelopes this loop will tolerate before giving up -- checked BEFORE
		// the StringRedacted() formatting/Info-log below so a peer flooding this loop with
		// irrelevant traffic can't force unbounded formatting cost on the very iteration that
		// finally gives up. Benign give-up (DeadlineExceededError, matching this function's own
		// wall-clock-deadline-elapsed branch above), not sfs.DeadConnError -- a stream of
		// well-formed-but-irrelevant traffic isn't itself evidence of a dead connection.
		nonMatchingEnvelopes++
		if nonMatchingEnvelopes > MaxNonMatchingEnvelopesPerWait {
			slog.Warn("DoHandshake: too many well-formed but non-matching envelopes processed, giving up", "nonMatchingEnvelopes", nonMatchingEnvelopes)
			return nil, DeadlineExceededError{}
		}
		// Anything else this early (unlikely, but be tolerant) is logged and skipped rather
		// than treated as a protocol violation. Content may legitimately be nil here too.
		// Uses StringRedacted (not String): a skipped envelope can legitimately carry a live
		// credential -- e.g. an out-of-order push.account.login.new-shaped payload arriving
		// before the real handshake response -- and this is exactly the site round-11's
		// credential_leak_lint_test.go doc comment named as a known, deliberately-uncaught
		// gap (contentStr was a local variable, not an inline .String() call, so the lint
		// regex missed it). See sfsobject.go's sfs.SensitiveSFSKeys/StringRedacted and
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
				pp := sfs.NewSFSObject()
				pp.PutLong("clientTime", time.Since(start).Milliseconds())
				if err := c.SendEnvelope(ControllerSystem, ActionPingPong, pp); err != nil {
					// SendStageError: consistency with SendAndWait/DoHandshake/the login-path send
					// sites -- this error is never returned across a function boundary or inspected
					// for Timeout() today (it's logged and the connection is closed unconditionally
					// either way), so wrapping it has no behavioral effect now, but keeps the
					// invariant "every direct send-stage error in this package is SendStageError-
					// wrapped" true package-wide for any future caller that does inspect it.
					slog.Error("heartbeat send failed -- closing connection", "error", SendStageError{Err: err})
					_ = c.Close()
					return
				}
			}
		}
	}()
}
