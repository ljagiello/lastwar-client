package session

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

// SFS2X system-controller action ids (dossier §04.1). ActionHandshake=0 is the vanilla SFS2X
// handshake. The 1.0.370 client never sends it: its system-message handler takes only ids 1
// (Login) and 29 (ping) (BaseUtils.decompiled.cs:32344-32357), and DoHandshake keeps it only as
// the off-by-default -handshake experiment.
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

	// repliesTo is the `_id` of this connection's request that the envelope was the first to
	// echo, or 0 (see GameConn.outstanding, StaleReply).
	repliesTo int32
}

// GameConn is a raw (plaintext, connectionType 0) SFS2X socket connection.
type GameConn struct {
	conn    net.Conn
	reader  *bufio.Reader
	arrival *arrivalReader // under reader: when the socket last gave bytes (nil in a bare literal)
	wmu     sync.Mutex

	// extSent counts SendExtension calls on this connection; SendExtension derives each request's
	// `_id` from it (see loginFutureID). A fresh GameConn per dial gives the per-connection reset
	// the real client's FutureManager.reset() does on reconnect.
	extSent atomic.Int32

	// lastCmd is the cmd of the most recent SendExtension, logged beside each score push so a
	// push.act.score.obtain can be tied to the action that earned it (see logScorePush).
	lastCmd atomic.Pointer[string]

	// ended is set once ReadEnvelope sees a terminal server push (see SessionEndedError). From
	// then on every read and send on this connection returns it, so nothing else goes out on a
	// session the server has ended, whichever loop happens to be running.
	ended atomic.Pointer[SessionEndedError]

	// skipClientPings makes ReadEnvelope drop PingPong requests. Only the fake-server side of
	// testsupport.go's ServeFakeGameServer/NewInMemoryGameDial sets it, so fake handlers that read
	// "the next envelope" as Login keep working now that StartHeartbeat pings before Login.
	skipClientPings bool

	// outstanding holds the `_id`s of requests sent on this connection that no message has
	// echoed yet. The first message to echo one takes it out and is marked as its reply
	// (Envelope.repliesTo), which is what StaleReply checks.
	outMu       sync.Mutex
	outstanding map[int32]struct{}

	// fakeServer marks the server end of a test pair (testsupport.go). Its SendExtension stamps
	// no `_id` of its own: a fake answers with the request's `_id` (ReadAndReply) or with none,
	// so its own send count never makes a reply look like an earlier request's (StaleReply).
	fakeServer bool

	// appVersion is the appVersion the Login on this connection claimed (SetAppVersion).
	appVersion atomic.Pointer[string]

	// deviceID is the deviceId the Login on this connection sent (SetDeviceID), for telling a
	// push.account.device.del about this device from one about another.
	deviceID atomic.Pointer[string]

	// forwardSid, when set, is the zone number every frame carries after the 0x04 header bit
	// (SetForwardServerID).
	forwardSid atomic.Pointer[uint16]

	// shumeiID is the last shumeiId a push.shumei.id on this connection carried (noteShumeiID,
	// ShumeiID), and onShumeiID is called with each new one (OnShumeiID).
	shumeiID   atomic.Pointer[string]
	onShumeiID atomic.Pointer[func(string)]

	// clock is the server time the pongs on this connection carry (clock.go, ServerNow).
	clock serverClock

	// framesRead counts the frames read off this connection, pongSeen is set by the first pong,
	// and silentFor is how long the waits since the last frame were blocked without one arriving.
	// WaitFor's watchdog (noteSilentWait) reads them.
	framesRead atomic.Uint64
	pongSeen   atomic.Bool
	silentFor  atomic.Int64 // time.Duration

	stopHeartbeat chan struct{}
	closeOnce     sync.Once
	closeErr      error
}

// arrivalReader sits between the socket and the bufio.Reader and stamps when a socket read last
// returned bytes. A frame's last byte arrives in the read that completes it, so right after
// sfs.ReadPacket returns, at is when that frame came off the socket, even if it then waited in
// the buffer while the flow decoded an earlier frame. The client stamps each packet in its
// receive thread (FinishRecvPacket, BaseUtils.decompiled.cs:32810-32812); Go reads on the main
// flow, so a frame that waited in the kernel between waits is still stamped late.
type arrivalReader struct {
	r  io.Reader
	at time.Time
}

func (a *arrivalReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.at = time.Now()
	}
	return n, err
}

// newGameConn wraps c with a read buffer of bufSize over an arrivalReader.
func newGameConn(c net.Conn, bufSize int) *GameConn {
	a := &arrivalReader{r: c}
	return &GameConn{conn: c, reader: bufio.NewReaderSize(a, bufSize), arrival: a}
}

// loginFutureID is the `_id` the SFS Login request carries (BuildLoginParams' FutureID, always 1).
// The real client draws Login's `_id` and every later request's from one counter,
// FutureManager.getFutureId() (Interlocked.Increment, Assembly-CSharp.decompiled.cs:672057-672066),
// so the first extension request after Login is 2.
const loginFutureID = 1

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
	return newGameConn(conn, 64*1024), nil
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
	return newGameConn(c, 4096)
}

// RawConn returns the underlying net.Conn. SetRawConn replaces it. Both are low-level hooks used by
// tests in other internal packages to inject raw bytes, close the transport out from under a
// GameConn, or wrap it in a failure-injecting net.Conn -- none of which they can do through
// GameConn's unexported field directly.
func (c *GameConn) RawConn() net.Conn     { return c.conn }
func (c *GameConn) SetRawConn(n net.Conn) { c.conn = n }

// SetAppVersion records the appVersion the Login on this connection claimed, so game code can
// compare with the version this session presents, as the real client compares with its own
// (CS.GameEntry.Sdk.Version). The auth package sets it once the Login is accepted.
func (c *GameConn) SetAppVersion(v string) { c.appVersion.Store(&v) }

// AppVersion returns what SetAppVersion recorded: "" for a nil connection or one whose Login
// recorded none.
func (c *GameConn) AppVersion() string {
	if c == nil {
		return ""
	}
	if v := c.appVersion.Load(); v != nil {
		return *v
	}
	return ""
}

// SetDeviceID records the deviceId the Login on this connection sent, the client's DEVICE_ID
// setting (GetDeviceUid, Assembly-CSharp.decompiled.cs:73399-73416). A push.account.device.del
// naming it ends the session (ErrDeviceRemoved); until it is set, no device push does.
func (c *GameConn) SetDeviceID(id string) { c.deviceID.Store(&id) }

// DeviceID returns what SetDeviceID recorded, or "".
func (c *GameConn) DeviceID() string {
	if c == nil {
		return ""
	}
	if v := c.deviceID.Load(); v != nil {
		return *v
	}
	return ""
}

// SetForwardServerID makes every later frame on this connection, pings included, carry the
// forward-server-id header: bit 0x04 and sid, the zone number (sfs.EncodeRequestPacket). The real
// client does this when client_config switch 29 is on (Assembly-CSharp.decompiled.cs:10119-10123).
// Off unless called; the server accepts frames without it.
func (c *GameConn) SetForwardServerID(sid uint16) { c.forwardSid.Store(&sid) }

func (c *GameConn) Close() error {
	c.closeOnce.Do(func() {
		c.logClock()
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
// writes it to the socket. Safe for concurrent use (heartbeat + main flow). After a terminal
// server push it writes nothing and returns that push's SessionEndedError.
func (c *GameConn) SendEnvelope(controller byte, action int16, content *sfs.SFSObject) error {
	if e := c.ended.Load(); e != nil {
		return e
	}
	outer := sfs.NewSFSObject()
	outer.PutByte("c", controller)
	outer.PutShort("a", action)
	outer.PutSFSObject("p", content)

	body, err := sfs.EncodeObject(outer)
	if err != nil {
		return fmt.Errorf("encode envelope: %w", err)
	}
	var packet []byte
	if c.fakeServer {
		packet, err = sfs.EncodePacket(body) // a test server's frames have no client send limit
	} else {
		var sid uint16
		fwd := c.forwardSid.Load()
		if fwd != nil {
			sid = *fwd
		}
		packet, err = sfs.EncodeRequestPacket(body, fwd != nil, sid)
	}
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
// SFSNetwork.SendMessage(cmd, ...) -- dossier §04/§06. It is SendExtensionID without the id.
func (c *GameConn) SendExtension(cmd string, params *sfs.SFSObject) error {
	_, err := c.SendExtensionID(cmd, params)
	return err
}

// SendExtensionID sends a client->server `cmd` extension request and returns the `_id` it
// carries, for matching the reply (WaitForReply, StaleReply).
//
// Every request carries an Int `_id` from one per-connection sequence that continues after
// Login's 1, as NetworkManager.SendLuaMessage and BaseMessage.CSSetData do
// (PutInt("_id", getFutureId()), Assembly-CSharp.decompiled.cs:77022-77024, 81210-81212). The
// server echoes `_id` and adds `_time` on the response, which the client reads off every reply
// (MessageFactory, Assembly-CSharp.decompiled.cs:82052-82056); live, every real-client request
// carries one except login.ext. A caller that already set `_id` keeps it (login.go's login.init
// fallback sends 2) and gets that value back (0 if it is not an Int), but the sequence still
// advances so the next request never reuses that number. The caller's params are copied, not
// modified, so a params object can be reused across sends.
func (c *GameConn) SendExtensionID(cmd string, params *sfs.SFSObject) (int32, error) {
	id := c.extSent.Add(1) + loginFutureID
	withID := sfs.NewSFSObject()
	if params != nil {
		for _, k := range params.Keys() {
			v, _ := params.Get(k)
			withID.PutValue(k, v)
		}
	}
	switch v, ok := withID.Get("_id"); {
	case ok:
		id, _ = v.Val.(int32)
	case c.fakeServer:
		id = 0
	default:
		withID.PutInt("_id", id)
	}
	extContent := sfs.NewSFSObject()
	extContent.PutUtfString("c", cmd)
	extContent.PutInt("r", -1)
	extContent.PutSFSObject("p", withID)
	c.lastCmd.Store(&cmd)
	if id > 0 {
		c.outMu.Lock()
		if c.outstanding == nil {
			c.outstanding = make(map[int32]struct{})
		}
		c.outstanding[id] = struct{}{}
		c.outMu.Unlock()
	}
	return id, c.SendEnvelope(ControllerExtension, ActionCallExtension, extContent)
}

// noteReply marks env as the reply to the outstanding request whose `_id` its params echo, if
// any, and takes that id out of outstanding, so only the first message to echo an id counts.
func (c *GameConn) noteReply(env *Envelope) {
	if env.Controller != ControllerExtension || env.Content == nil {
		return
	}
	pv, ok := env.Content.Get("p")
	if !ok {
		return
	}
	params, _ := pv.Val.(*sfs.SFSObject)
	v, ok := params.Get("_id")
	if !ok || !SFSFieldKindAccepts(SFSFieldKindInt, v.Val) {
		return
	}
	id := params.GetInt("_id")
	c.outMu.Lock()
	defer c.outMu.Unlock()
	if _, ok := c.outstanding[id]; ok {
		delete(c.outstanding, id)
		env.repliesTo = id
	}
}

// ReadEnvelope blocks until the next framed packet arrives and decodes it.
//
// A terminal server push (push.user.off, push.server.stop, init.error) comes back as a
// *SessionEndedError instead of an envelope, and so does every later call. Detecting it here
// rather than in each read loop means WaitFor/SendAndWait, DoHandshake, login's waitForInitPush
// and game's FetchInit all stop on it through the dead-connection check they already make.
func (c *GameConn) ReadEnvelope() (*Envelope, error) {
	for {
		env, err := c.readEnvelope()
		if err == nil && c.skipClientPings && env.Controller == ControllerSystem && env.Action == ActionPingPong {
			continue
		}
		return env, err
	}
}

func (c *GameConn) readEnvelope() (*Envelope, error) {
	if e := c.ended.Load(); e != nil {
		return nil, e
	}
	body, err := sfs.ReadPacket(c.reader)
	if err != nil {
		return nil, err
	}
	recv := time.Now() // a pong's arrival, before decoding (notePong)
	if c.arrival != nil && !c.arrival.at.IsZero() {
		recv = c.arrival.at
	}
	c.framesRead.Add(1)
	c.silentFor.Store(0)
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
	if err := c.endIfTerminalPush(env); err != nil {
		return nil, err
	}
	c.noteReply(env)
	c.notePong(env, recv)
	c.logScorePush(env)
	c.noteShumeiID(env)
	return env, nil
}

// shumeiIDPushCmd is the server's push of the ShumeiId the client caches
// (PushShumeiIdMessage.lua:8-21).
const shumeiIDPushCmd = "push.shumei.id"

// noteShumeiID keeps the shumeiId of a push.shumei.id that carries no errorCode, as the client
// caches every non-empty one in PlayerPrefs LW_CacheShumeiDeviceId and sends it as ta lw_shumei_id
// (PushShumeiIdMessage.lua:10-18). Nothing waits for the push, so it is noted here, where every
// read passes, whichever loop reads it (FetchInit drops pushes it does not wait for). A new id goes
// to the OnShumeiID callback at once, as the client saves it in the handler
// (PushShumeiIdMessage.lua:16-17, Setting:SetString then Setting:Save()).
func (c *GameConn) noteShumeiID(env *Envelope) {
	if env.Controller != ControllerExtension || env.Content == nil {
		return
	}
	if v, ok := env.Content.Get("c"); !ok || v.Val != shumeiIDPushCmd {
		return
	}
	var params *sfs.SFSObject
	if pv, ok := env.Content.Get("p"); ok {
		params, _ = pv.Val.(*sfs.SFSObject)
	}
	if params == nil || params.Has("errorCode") {
		return
	}
	if id := params.GetString("shumeiId"); id != "" {
		prev := c.shumeiID.Swap(&id)
		if fn := c.onShumeiID.Load(); fn != nil && (prev == nil || *prev != id) {
			(*fn)(id)
		}
	}
}

// OnShumeiID sets fn to be called with every new shumeiId a push.shumei.id brings on this
// connection from now on, on the goroutine that reads the push, and calls it at once with one
// already noted (a push read before fn was set, e.g. during the Login). A nil fn clears it.
func (c *GameConn) OnShumeiID(fn func(string)) {
	if fn == nil {
		c.onShumeiID.Store(nil)
		return
	}
	c.onShumeiID.Store(&fn)
	if id := c.ShumeiID(); id != "" {
		fn(id)
	}
}

// ShumeiID returns the last shumeiId the server pushed on this connection (push.shumei.id), or ""
// when none came.
func (c *GameConn) ShumeiID() string {
	if c == nil {
		return ""
	}
	if v := c.shumeiID.Load(); v != nil {
		return *v
	}
	return ""
}

// scorePushCmd is the server's "points earned" push for score events: Arms Race and, with type
// 15, the Alliance Duel (PushActScoreObtainMessage.lua:6-16). Nothing waits for it, so it is
// logged here, where every read passes, with the last request sent: one live day of these lines
// shows which step (start, finish or claim) books a duel action's points (docs/alliance-duel.mdx).
const scorePushCmd = "push.act.score.obtain"

func (c *GameConn) logScorePush(env *Envelope) {
	if env.Controller != ControllerExtension || env.Content == nil {
		return
	}
	v, ok := env.Content.Get("c")
	if !ok || v.Val != scorePushCmd {
		return
	}
	var params *sfs.SFSObject
	if pv, ok := env.Content.Get("p"); ok {
		params, _ = pv.Val.(*sfs.SFSObject)
	}
	last := ""
	if p := c.lastCmd.Load(); p != nil {
		last = *p
	}
	slog.Info("activity score obtained", "afterCmd", last, "push", params.StringRedacted())
}

// endIfTerminalPush records and returns a SessionEndedError when env is a terminal server push,
// and returns nil for anything else. It reads c and p directly rather than through AsExtension so
// a wrong-typed field is warned about once, by the caller's own AsExtension, not twice.
func (c *GameConn) endIfTerminalPush(env *Envelope) error {
	if env.Controller != ControllerExtension || env.Content == nil {
		return nil
	}
	v, ok := env.Content.Get("c")
	if !ok {
		return nil
	}
	cmd, ok := v.Val.(string)
	if !ok {
		return nil
	}
	var params *sfs.SFSObject
	if pv, ok := env.Content.Get("p"); ok {
		params, _ = pv.Val.(*sfs.SFSObject)
	}
	ended := terminalPush(cmd, params, c.DeviceID())
	if ended == nil {
		if cmd == pushDeviceDelCmd {
			// An errorCode, another device of the account, or this one before SetDeviceID: the
			// client then shows a tip or refreshes its device list (PushAccountDeviceDelMessage.lua:
			// 10-12, 22).
			slog.Warn("push.account.device.del does not name this session's device; the run goes on",
				"ownDeviceKnown", c.DeviceID() != "", "push", params.StringRedacted())
		}
		return nil
	}
	if ec, ok := params.Get("errorCode"); ok && ec.Val != nil {
		ended.Code = fmt.Sprintf("%v", ec.Val)
	}
	c.ended.CompareAndSwap(nil, ended)
	slog.Error("server ended the session; stopping this run without reconnecting",
		"cmd", cmd, "errorCode", ended.Code, "push", params.StringRedacted())
	return c.ended.Load()
}

// ExtensionMessage is a decoded server->client `cmd` push/response.
type ExtensionMessage struct {
	Cmd    string
	Params *sfs.SFSObject

	repliesTo int32 // Envelope.repliesTo
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
	return &ExtensionMessage{Cmd: cmd, Params: params, repliesTo: e.repliesTo}, true
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
	"120471":             {"al.science.donate"},                               // alliance.go: donation charges used up (useNum 0); server text "Donate science CD time is not finish"
	// alliance.go: live on every 02:00 UTC run, the day reset, when donations stop working for a
	// while. A live observation only: 120836 is in neither Lua 835, tables 40413 nor the 1.0.370
	// C#, whose donate handler just shows the code as a tip (AlScienceDonateMessage.lua:15-17).
	"120836": {"al.science.donate"},
}

// RegisterBenignErrorCode marks errorCode as an expected no-op for the given cmds, with the same
// cmd scoping as benignErrorCodes. Feature files call it from init() for their own "already
// claimed" codes, so each feature owns its codes instead of editing the map above. Not safe for
// use after init.
func RegisterBenignErrorCode(code string, cmds ...string) {
	for _, c := range cmds {
		if !slices.Contains(benignErrorCodes[code], c) {
			benignErrorCodes[code] = append(benignErrorCodes[code], c)
		}
	}
}

// commandOutcome classifies a collect/claim response into one of three buckets: a real success,
// a benign no-op (an expected cooldown/already-claimed/not-yet-arrived errorCode, or -- for
// building.production.collect specifically -- a response with no errorCode whose status says
// nothing was collected; see collectStatusIsNoOp). Other command families have no documented
// evidence of ever emitting a status field, so the status rule does not apply to them and they
// fall through to outcomeSuccess instead.
type commandOutcome int

const (
	outcomeSuccess commandOutcome = iota
	outcomeBenign
	outcomeFailure
)

// collectStatusIsNoOp reports whether a building.production.collect response that carries no
// errorCode still collected nothing. It follows the real client exactly:
// ProductLineManager.HandleCollect does `if (message.status or -1) == -1` → tip 602026 ("Production
// in progress. Please wait.") and returns without touching the building
// (DataCenter/ProductLine/ProductLineManager.lua:596-606). So a missing (or null) status and
// status -1 are no-ops, the same outcome as errorCode 602026. Every other value -- 1 on every live
// success so far, and 0, which has never been seen live -- takes the client's success path.
//
// Before 1.0.364's Lua was read, this treated status=0 as the no-op and a missing status as success,
// a guess with no evidence behind it either way. Only the log level depends on this: SendAndWait
// returns an error for outcomeFailure alone, so no exit code or abort decision changes.
//
// A present but wrong-typed status (a string, a double) is not -1 in Lua either -- Lua's == does
// not coerce -- so it is success here too, with a Warn. Round 29's rule still holds: GetInt's
// zero-value coercion is never trusted without a type check, so a malformed status cannot land in
// the benign bucket (TestClassifyResponseWrongTypedStatusIsNotBenign, conn_test.go).
func collectStatusIsNoOp(params *sfs.SFSObject) bool {
	v, ok := params.Get("status")
	if !ok || v.Val == nil {
		return true
	}
	if !SFSFieldKindAccepts(SFSFieldKindInt, v.Val) {
		WarnIfWrongTypedField(params, "status", "building.production.collect status check", SFSFieldKindInt)
		return false
	}
	return params.GetInt("status") == -1
}

// classifyResponse determines a response's outcome and, for a benign or real failure, the
// errorCode (empty string for the building.production.collect no-errorCode benign case, see
// collectStatusIsNoOp). An errorCode present in benignErrorCodes is only outcomeBenign when
// msg.Cmd matches one of that code's documented cmds (see benignErrorCodes' doc comment) --
// mirroring how the status rule is itself scoped to building.production.collect -- so a genuine
// failure on an unrelated cmd that happens to share one of these numeric errorCode values still
// falls through to outcomeFailure. This is the single place both logCommandResult and SendAndWait
// derive their behavior from, so the two can never drift out of sync with each other.
func classifyResponse(msg *ExtensionMessage) (commandOutcome, string) {
	ec, has := msg.Params.Get("errorCode")
	if !has {
		if msg.Cmd == "building.production.collect" && collectStatusIsNoOp(msg.Params) {
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
			slog.Warn(label+" no-op (status missing or -1, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.StringRedacted())
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
// (e.g. mail.go's push.chat.get.system.mails). A late reply to an earlier request is skipped
// (WaitForReply), so a timeout cannot shift every later reply of the same cmd by one.
func SendAndWait(conn *GameConn, label, sendCmd string, params *sfs.SFSObject, waitCmds ...string) (*ExtensionMessage, error) {
	id, err := conn.SendExtensionID(sendCmd, params)
	if err != nil {
		wrapped := SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", sendCmd, "error", wrapped)
		return nil, wrapped
	}
	if len(waitCmds) == 0 {
		waitCmds = []string{sendCmd}
	}
	msg, err := WaitForReply(conn, DefaultCmdTimeout, id, waitCmds...)
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
// response. The 1.0.370 client never sends one: its own transport handles
// only system ids 1 and 29 (BaseUtils.decompiled.cs:32344-32357) and goes
// straight to Login, as every login of this client does by default. It is
// kept as the off-by-default -handshake experiment; deleting it is the
// owner's choice. api="1.7.8" cl="Unity" are the defaults of the vanilla
// SmartFox2X 1.7.8 SDK, not values the game sends.
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
// 4000ms, {p: {clientTime: ms}}), required to avoid the ~12s server-perceived
// timeout while we wait on slower steps (e.g. the user fetching an email
// verification code).
//
// The first ping goes out synchronously, before StartHeartbeat returns, so it precedes Login on
// the wire as the real client's does: NetRawProxy.OnConnection enables its 4000 ms keepalive timer
// and calls CheckKeepAlive at once (BaseUtils.decompiled.cs:36011-36035), and Login waits for that
// first pong. The real client's first probe is a CustomPingPong carrying a decoy login template;
// this one is the plain keepalive ping (sendHeartbeat), and Login is not held back for the pong. If the first ping cannot be sent the connection is closed and no loop starts, so
// the caller's Login send fails.
//
// start is also the basis the pongs' echoed clientTime is read against (serverClock).
func (c *GameConn) StartHeartbeat(interval time.Duration, start time.Time) {
	c.clock.setStart(start)
	c.stopHeartbeat = make(chan struct{})
	stopCh := c.stopHeartbeat // snapshot: avoids racing Close()'s concurrent access to the field
	if !c.sendHeartbeat(start) {
		return
	}
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				if !c.sendHeartbeat(start) {
					return
				}
			}
		}
	}()
}

// sendHeartbeat sends one PingPongRequest and reports whether the heartbeat should keep going. A
// send failure closes the connection.
//
// The payload is the client's: clientTime nested one level down, {p: {clientTime}}
// (PingPongRequest.Execute, BaseUtils.decompiled.cs:36497-36500; seen so in the 2026-10-04
// capture). The server answers a flat {clientTime} too, but its pong then carries no clientTime to
// sync the server clock from (serverClock).
func (c *GameConn) sendHeartbeat(start time.Time) bool {
	inner := sfs.NewSFSObject()
	inner.PutLong("clientTime", time.Since(start).Milliseconds())
	pp := sfs.NewSFSObject()
	pp.PutSFSObject("p", inner)
	err := c.SendEnvelope(ControllerSystem, ActionPingPong, pp)
	if err == nil {
		return true
	}
	if errors.Is(err, ErrSessionEnded) {
		return false // already logged by ReadEnvelope; the caller exits and closes the conn
	}
	// SendStageError: consistency with SendAndWait/DoHandshake/the login-path send
	// sites -- this error is never returned across a function boundary or inspected
	// for Timeout() today (it's logged and the connection is closed unconditionally
	// either way), so wrapping it has no behavioral effect now, but keeps the
	// invariant "every direct send-stage error in this package is SendStageError-
	// wrapped" true package-wide for any future caller that does inspect it.
	slog.Error("heartbeat send failed -- closing connection", "error", SendStageError{Err: err})
	_ = c.Close()
	return false
}
