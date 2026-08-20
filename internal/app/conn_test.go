package app

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAsExtension(t *testing.T) {
	t.Run("non-extension controller", func(t *testing.T) {
		env := &Envelope{Controller: controllerSystem, Content: sfs.NewSFSObject()}
		if _, ok := env.AsExtension(); ok {
			t.Fatal("expected ok=false for a non-extension controller")
		}
	})
	t.Run("nil content", func(t *testing.T) {
		env := &Envelope{Controller: controllerExtension, Content: nil}
		if _, ok := env.AsExtension(); ok {
			t.Fatal("expected ok=false for nil Content")
		}
	})
	t.Run("well-formed extension message", func(t *testing.T) {
		content := sfs.NewSFSObject()
		content.PutUtfString("c", "test.cmd")
		p := sfs.NewSFSObject()
		p.PutInt("foo", 42)
		content.PutSFSObject("p", p)
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok {
			t.Fatal("expected ok=true")
		}
		if msg.Cmd != "test.cmd" {
			t.Errorf("Cmd = %q, want test.cmd", msg.Cmd)
		}
		if msg.Params.GetInt("foo") != 42 {
			t.Errorf("Params.foo = %d, want 42", msg.Params.GetInt("foo"))
		}
	})
	t.Run("missing p defaults to empty params, not nil", func(t *testing.T) {
		content := sfs.NewSFSObject()
		content.PutUtfString("c", "test.cmd")
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok || msg.Params == nil {
			t.Fatalf("expected ok=true and non-nil Params, got ok=%v params=%v", ok, msg.Params)
		}
	})
	// TestAsExtension/wrong-typed p warns is the round-40 regression subtest for the MINOR
	// finding that AsExtension's inner "p" field-read had no wrong-typed diagnostic, unlike every
	// other field-read guard this codebase has added elsewhere.
	t.Run("wrong-typed p warns", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		content := sfs.NewSFSObject()
		content.PutUtfString("c", "test.cmd")
		content.PutUtfString("p", "not-an-object")
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok || msg.Params == nil {
			t.Fatalf("expected ok=true and non-nil (defaulted) Params, got ok=%v params=%v", ok, msg.Params)
		}
		if logged := buf.String(); !strings.Contains(logged, "p field is present but not an object") {
			t.Errorf("expected a warning about the wrong-typed p field, got log:\n%s", logged)
		}
	})
	// TestAsExtension/wrong-typed c warns is the round-43 regression subtest for the MAJOR finding
	// that AsExtension's "c" (cmd) field-read had no wrong-typed diagnostic at all -- unlike its own
	// sibling "p" field just above (round-40 fix) and unlike ReadEnvelope's identical c/a/p triple
	// one level up (also round-40). A wrong-typed "c" silently coerced to cmd="", matching none of
	// the real dispatch keys callers check msg.Cmd against, so a genuinely-arrived response was
	// misclassified as an unrecognized push with zero signal at Warn level.
	t.Run("wrong-typed c warns", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		content := sfs.NewSFSObject()
		content.PutInt("c", 5)
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok {
			t.Fatal("expected ok=true")
		}
		if msg.Cmd != "" {
			t.Errorf("Cmd = %q, want \"\" (the zero-value fallback for a wrong-typed field)", msg.Cmd)
		}
		if logged := buf.String(); !strings.Contains(logged, "c field is present but not a string") {
			t.Errorf("expected a warning about the wrong-typed c field, got log:\n%s", logged)
		}
	})
}

// TestReadEnvelopeWrongTypedFieldsWarn is the round-40 regression test for the MAJOR finding that
// ReadEnvelope silently coerced a present-but-wrong-typed "c"/"a"/"p" field to its Go zero value
// with zero diagnostic, unlike every other field-read site in this codebase (gsl.go's
// gsl.FindServerInfo, buildings.go's requireFieldType, etc.). This mattered in a real way: a
// wrong-typed "c"/"a" used to be silently indistinguishable from a genuine
// controllerSystem/actionHandshake envelope, since both zero-value to exactly those constants.
func TestReadEnvelopeWrongTypedFieldsWarn(t *testing.T) {
	tests := []struct {
		name       string
		build      func(o *sfs.SFSObject)
		wantMsg    string
		wantCtrl   byte
		wantAction int16
	}{
		{
			name: "c wrong-typed",
			build: func(o *sfs.SFSObject) {
				o.PutInt("c", 5)
				o.PutShort("a", actionHandshake)
				o.PutSFSObject("p", sfs.NewSFSObject())
			},
			wantMsg:    "c field is present but not a byte",
			wantCtrl:   0, // zero value -- collides with controllerSystem, exactly the risk this fix documents
			wantAction: actionHandshake,
		},
		{
			name: "a wrong-typed",
			build: func(o *sfs.SFSObject) {
				o.PutByte("c", controllerSystem)
				o.PutUtfString("a", "bad")
				o.PutSFSObject("p", sfs.NewSFSObject())
			},
			wantMsg:    "a field is present but not an int16",
			wantCtrl:   controllerSystem,
			wantAction: 0, // zero value -- collides with actionHandshake
		},
		{
			name: "p wrong-typed",
			build: func(o *sfs.SFSObject) {
				o.PutByte("c", controllerSystem)
				o.PutShort("a", actionHandshake)
				o.PutUtfString("p", "bad")
			},
			wantMsg:    "p field is present but not an object",
			wantCtrl:   controllerSystem,
			wantAction: actionHandshake,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c1, c2 := net.Pipe()
			defer func() { _ = c1.Close() }()
			defer func() { _ = c2.Close() }()

			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(orig)

			outer := sfs.NewSFSObject()
			tt.build(outer)
			body, err := sfs.EncodeObject(outer)
			if err != nil {
				t.Fatalf("sfs.EncodeObject: %v", err)
			}
			packet, err := sfs.EncodePacket(body)
			if err != nil {
				t.Fatalf("sfs.EncodePacket: %v", err)
			}

			writeDone := make(chan error, 1)
			go func() {
				_, werr := c1.Write(packet)
				writeDone <- werr
			}()

			server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}
			_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
			env, err := server.ReadEnvelope()
			if werr := <-writeDone; werr != nil {
				t.Fatalf("write: %v", werr)
			}
			if err != nil {
				t.Fatalf("ReadEnvelope: %v", err)
			}
			if env.Controller != tt.wantCtrl {
				t.Errorf("Controller = %d, want %d", env.Controller, tt.wantCtrl)
			}
			if env.Action != tt.wantAction {
				t.Errorf("Action = %d, want %d", env.Action, tt.wantAction)
			}
			if logged := buf.String(); !strings.Contains(logged, tt.wantMsg) {
				t.Errorf("expected a warning containing %q, got log:\n%s", tt.wantMsg, logged)
			}
		})
	}
}

func newTestExtMsg(cmd string, errorCode any) *ExtensionMessage {
	params := sfs.NewSFSObject()
	if errorCode != nil {
		switch v := errorCode.(type) {
		case string:
			params.PutUtfString("errorCode", v)
		case int:
			params.PutInt("errorCode", int32(v))
		}
	}
	return &ExtensionMessage{Cmd: cmd, Params: params}
}

// newTestExtMsgWithStatus is newTestExtMsg plus an optional "status" int field, for exercising
// classifyResponse's status=0 benign heuristic (which, unlike errorCode, only fires for a
// specific cmd -- see TestClassifyResponse). hasStatus distinguishes "no status field at all"
// from "status=0" -- both need to be tested separately, and a bare int can't represent "absent".
func newTestExtMsgWithStatus(cmd string, errorCode any, status int32, hasStatus bool) *ExtensionMessage {
	msg := newTestExtMsg(cmd, errorCode)
	if hasStatus {
		msg.Params.PutInt("status", status)
	}
	return msg
}

func TestLogCommandResultClassification(t *testing.T) {
	// logCommandResult only logs; this smoke-tests all four branches its switch/if-else can
	// actually take (round 26: the previous "all three" wording only covered outcomeSuccess,
	// outcomeBenign-with-a-code, and outcomeFailure -- missing logCommandResult's inner else,
	// the outcomeBenign-with-an-EMPTY-errorCode case, i.e. the building.production.collect
	// status=0-with-no-errorCode log line -- which was untested by this test, or any other test
	// in the suite):
	//   1. outcomeSuccess
	//   2. outcomeBenign with a non-empty errorCode
	//   3. outcomeBenign with no errorCode (the status=0 heuristic, via newTestExtMsgWithStatus)
	//   4. outcomeFailure
	// Classification logic itself is asserted precisely by TestClassifyResponse above; this just
	// runs each branch through the real logger without panicking. Both benign cases must use their
	// actual documented cmd (building.production.collect) -- since both the errorCode scoping and
	// the status=0 heuristic are cmd-scoped, pairing either with an arbitrary "test.cmd" would
	// silently exercise a different branch than intended.
	logCommandResult("test success", newTestExtMsg("test.cmd", nil))
	logCommandResult("test benign", newTestExtMsg("building.production.collect", "602026"))
	logCommandResult("test benign no code", newTestExtMsgWithStatus("building.production.collect", nil, 0, true))
	logCommandResult("test real failure", newTestExtMsg("test.cmd", "999999"))
}

// TestClassifyResponse asserts classifyResponse's actual (outcome, code) return value directly,
// including that the status=0-with-no-errorCode benign heuristic is scoped to
// building.production.collect only -- for every other command a status=0 response with no
// errorCode is a real success, not a no-op (see classifyResponse's doc comment in conn.go) -- and
// that every benignErrorCodes entry is likewise scoped to its own documented cmd(s): the same
// numeric/string errorCode value on an unrelated cmd must fall through to outcomeFailure rather
// than being silently reclassified as outcomeBenign.
func TestClassifyResponse(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		errorCode   any
		status      int32
		hasStatus   bool
		wantOutcome commandOutcome
		wantCode    string
	}{
		{
			name:        "no errorCode, no status field, any cmd -> success",
			cmd:         "some.other.cmd",
			hasStatus:   false,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=1, any cmd -> success",
			cmd:         "some.other.cmd",
			status:      1,
			hasStatus:   true,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=0, cmd=building.production.collect -> benign",
			cmd:         "building.production.collect",
			status:      0,
			hasStatus:   true,
			wantOutcome: outcomeBenign,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=0, other cmd -> success (heuristic must not apply globally)",
			cmd:         "mail.reward.batch",
			status:      0,
			hasStatus:   true,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "602026 on its documented cmd (building.production.collect) -> benign",
			cmd:         "building.production.collect",
			errorCode:   "602026",
			wantOutcome: outcomeBenign,
			wantCode:    "602026",
		},
		{
			name:        "602026 on an unrelated cmd -> failure (errorCode scoping must not leak across commands)",
			cmd:         "vip.reward.get",
			errorCode:   "602026",
			wantOutcome: outcomeFailure,
			wantCode:    "602026",
		},
		{
			name:        "120289 on vip.add.login.score (one of its two documented cmds) -> benign",
			cmd:         "vip.add.login.score",
			errorCode:   "120289",
			wantOutcome: outcomeBenign,
			wantCode:    "120289",
		},
		{
			name:        "120289 on vip.get.every.day.reward (the other documented cmd) -> benign",
			cmd:         "vip.get.every.day.reward",
			errorCode:   "120289",
			wantOutcome: outcomeBenign,
			wantCode:    "120289",
		},
		{
			name:        "visitor_err_coming on its documented cmd (visitor.operate) -> benign",
			cmd:         "visitor.operate",
			errorCode:   "visitor_err_coming",
			wantOutcome: outcomeBenign,
			wantCode:    "visitor_err_coming",
		},
		{
			name:        "120471 on its documented cmd (al.science.donate) -> benign",
			cmd:         "al.science.donate",
			errorCode:   "120471",
			wantOutcome: outcomeBenign,
			wantCode:    "120471",
		},
		{
			name:        "errorCode is not in benignErrorCodes at all -> failure",
			cmd:         "vip.reward.get",
			errorCode:   "999999",
			wantOutcome: outcomeFailure,
			wantCode:    "999999",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := newTestExtMsgWithStatus(tt.cmd, tt.errorCode, tt.status, tt.hasStatus)
			gotOutcome, gotCode := classifyResponse(msg)
			if gotOutcome != tt.wantOutcome || gotCode != tt.wantCode {
				t.Errorf("classifyResponse() = (%v, %q), want (%v, %q)", gotOutcome, gotCode, tt.wantOutcome, tt.wantCode)
			}
		})
	}
}

// TestClassifyResponseWrongTypedStatusIsNotBenign is the round-29 regression test for the MAJOR
// finding that classifyResponse's building.production.collect status check used
// Has("status")+GetInt("status")==0 -- presence-only plus the silently-zero-coercing GetInt --
// instead of the requireFieldType/sfsFieldKindAccepts machinery (buildings.go) round 28 built
// specifically to catch this bug class elsewhere. GetInt coerces ANY non-int-shaped value to
// int32(0), so a present-but-wrong-typed status field (e.g. the server sending it as a string or a
// double) used to satisfy the old check exactly like a genuine status=0 would, folding a malformed
// response into the same "benign no-op" bucket a real cooldown response gets -- indistinguishable
// from classifyResponse's point of view. Since classifyResponse is the single dedup point both
// logCommandResult and sendAndWait's returned error derive from, this matters beyond just log
// severity. Covers both a wrong-typed non-zero-looking raw value (double 0.0, which GetInt would
// also coerce to 0) and a wrong-typed string "0", proving the fix rejects the field's presence
// entirely rather than just checking its coerced value.
func TestClassifyResponseWrongTypedStatusIsNotBenign(t *testing.T) {
	tests := []struct {
		name string
		put  func(p *sfs.SFSObject)
	}{
		{
			name: "status is a double, not an int",
			put:  func(p *sfs.SFSObject) { p.PutDouble("status", 0) },
		},
		{
			name: "status is a string, not an int",
			put:  func(p *sfs.SFSObject) { p.PutUtfString("status", "0") },
		},
		{
			name: "status is a bool, not an int",
			put:  func(p *sfs.SFSObject) { p.PutBool("status", false) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			params := sfs.NewSFSObject()
			tt.put(params)
			msg := &ExtensionMessage{Cmd: "building.production.collect", Params: params}

			gotOutcome, gotCode := classifyResponse(msg)
			if gotOutcome == outcomeBenign {
				t.Errorf("classifyResponse() = (%v, %q), want NOT outcomeBenign -- a wrong-typed status field must not be misclassified as the benign status==0 case (GetInt's zero-value coercion must not be trusted without a type check first)", gotOutcome, gotCode)
			}
			if gotOutcome != outcomeSuccess {
				t.Errorf("classifyResponse() = (%v, %q), want outcomeSuccess (no errorCode present, and a wrong-typed status must be treated as if status were absent)", gotOutcome, gotCode)
			}
		})
	}
}

func TestGameConnSendReceiveRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close() }()
	defer func() { _ = c2.Close() }()

	client := &GameConn{conn: c1, reader: bufio.NewReaderSize(c1, 4096)}
	server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}

	params := sfs.NewSFSObject()
	params.PutUtfString("hello", "world")

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- client.SendExtension("test.roundtrip", params)
	}()

	_ = c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	env, readErr := server.ReadEnvelope()
	if sendErr := <-sendDone; sendErr != nil {
		t.Fatalf("SendExtension: %v", sendErr)
	}
	if readErr != nil {
		t.Fatalf("ReadEnvelope: %v", readErr)
	}

	msg, ok := env.AsExtension()
	if !ok {
		t.Fatal("expected a well-formed extension message")
	}
	if msg.Cmd != "test.roundtrip" {
		t.Errorf("Cmd = %q, want test.roundtrip", msg.Cmd)
	}
	if got := msg.Params.GetString("hello"); got != "world" {
		t.Errorf("Params.hello = %q, want world", got)
	}
}

// splitWriteConn wraps a net.Conn, splitting every Write over some minimum size into two separate
// underlying writes with a small delay between them -- widening the window during which a
// concurrent, unsynchronized second Write on the same conn could interleave its own bytes into the
// stream. Used by TestSendEnvelopeIsSafeForConcurrentUse below to give a hypothetical regression
// (wmu no longer covering the actual conn.Write call) an actual chance to be observed: a bare
// net.Pipe's own internal buffering can otherwise mask interleaving that would still corrupt a
// real TCP connection under the same unsynchronized-concurrent-Write conditions.
type splitWriteConn struct {
	net.Conn
}

func (c *splitWriteConn) Write(b []byte) (int, error) {
	if len(b) < 8 {
		return c.Conn.Write(b)
	}
	mid := len(b) / 2
	n1, err := c.Conn.Write(b[:mid])
	if err != nil {
		return n1, err
	}
	time.Sleep(2 * time.Millisecond)
	n2, err := c.Conn.Write(b[mid:])
	return n1 + n2, err
}

// TestSendEnvelopeIsSafeForConcurrentUse is the round-45 regression test for the MINOR finding
// that SendEnvelope's documented "safe for concurrent use (heartbeat + main flow)" guarantee
// (conn.go's wmu, which serializes only the write itself -- SetWriteDeadline/conn.Write/
// SetWriteDeadline(zero) -- not envelope construction/encoding) had no test driving two truly
// concurrent SendEnvelope/SendExtension callers against one GameConn. TestGameConnSendReceiveRoundTrip
// above has exactly one sender; the StartHeartbeat tests (conn_handshake_test.go) have the
// heartbeat goroutine as the only sender. Spawns n goroutines each sending a distinct extension
// command concurrently over a splitWriteConn (see its own doc comment for why a bare net.Pipe
// isn't a strong enough vehicle for this specific mutation) and confirms the server side reads
// back exactly n well-formed, non-corrupted envelopes with the right cmd/params pairing -- if
// wmu's scope were ever narrowed to no longer cover the actual conn.Write call, two callers'
// packets could interleave mid-write and desync the frame stream, which would surface here as a
// ReadEnvelope decode failure/timeout or a cmd matched to the wrong params.
func TestSendEnvelopeIsSafeForConcurrentUse(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() { _ = c1.Close(); _ = c2.Close() })
	client := &GameConn{conn: &splitWriteConn{Conn: c1}, reader: bufio.NewReaderSize(c1, 4096)}
	server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	sendErrs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			params := sfs.NewSFSObject()
			params.PutInt("i", int32(i))
			if err := client.SendExtension(fmt.Sprintf("test.cmd.%d", i), params); err != nil {
				sendErrs <- err
			}
		}(i)
	}

	gotCmds := make(map[string]int32, n)
	var mu sync.Mutex
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for i := 0; i < n; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("ReadEnvelope %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				t.Errorf("envelope %d did not decode as an extension message", i)
				return
			}
			mu.Lock()
			gotCmds[msg.Cmd] = msg.Params.GetInt("i")
			mu.Unlock()
		}
	}()

	wg.Wait()
	close(sendErrs)
	for err := range sendErrs {
		t.Errorf("SendExtension: %v", err)
	}

	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server never read all n envelopes -- a corrupted/interleaved concurrent write likely desynced the frame stream")
	}

	if len(gotCmds) != n {
		t.Fatalf("server decoded %d distinct, well-formed envelopes, want %d", len(gotCmds), n)
	}
	for i := 0; i < n; i++ {
		cmd := fmt.Sprintf("test.cmd.%d", i)
		got, ok := gotCmds[cmd]
		if !ok {
			t.Errorf("missing envelope for %s", cmd)
			continue
		}
		if got != int32(i) {
			t.Errorf("envelope %s carried i=%d, want %d -- possible cross-writer corruption", cmd, got, i)
		}
	}
}

// countingCloseConn is a minimal net.Conn whose Close() counts invocations and, from the second
// call onward, returns a distinct "spurious" error -- standing in for the real
// "use of closed network connection" error a genuine net.Conn returns when Close() is called on
// it more than once. Used by the TestCloseIsIdempotent* tests below to prove GameConn.Close()
// invokes the underlying net.Conn's Close() at most once no matter how many times, or how
// concurrently, GameConn.Close() itself is called.
type countingCloseConn struct {
	net.Conn
	mu    sync.Mutex
	calls int
}

func (c *countingCloseConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > 1 {
		return errors.New("use of closed network connection")
	}
	return nil
}

func (c *countingCloseConn) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCloseIsIdempotentSequential is the round-28 regression test for the MINOR finding that
// GameConn.Close()'s sync.Once only deduped the stopHeartbeat channel close, leaving the
// underlying net.Conn.Close() call outside the Once to run unconditionally on every call -- so a
// second Close() invoked the real socket's Close() a second time and could surface a spurious
// "use of closed network connection" error even though nothing new actually failed. Asserts both
// halves of genuine idempotency: the underlying Close() runs exactly once, and every call to
// GameConn.Close() -- not just the first -- returns that same result.
func TestCloseIsIdempotentSequential(t *testing.T) {
	conn := &countingCloseConn{}
	c := &GameConn{conn: conn, reader: bufio.NewReaderSize(conn, 4096)}

	err1 := c.Close()
	err2 := c.Close()
	err3 := c.Close()

	if got := conn.callCount(); got != 1 {
		t.Errorf("underlying net.Conn.Close() called %d times across 3 GameConn.Close() calls, want exactly 1", got)
	}
	if err1 != nil {
		t.Errorf("first Close() = %v, want nil", err1)
	}
	if err2 != err1 {
		t.Errorf("second Close() = %v, want it to equal the first Close()'s result (%v) -- a genuinely idempotent Close must not surface a spurious second error from re-invoking the underlying socket's Close()", err2, err1)
	}
	if err3 != err1 {
		t.Errorf("third Close() = %v, want it to equal the first Close()'s result (%v)", err3, err1)
	}
}

// TestCloseIsIdempotentConcurrent is TestCloseIsIdempotentSequential's concurrent sibling,
// covering the realistic scenario named in the finding: StartHeartbeat's own error branch calling
// c.Close() concurrently with the main goroutine's error-path c.Close() after a failed blocked
// read. Every concurrent caller must observe exactly one real underlying Close() and the same
// returned result -- sync.Once.Do's happens-before guarantee (every Do call blocks until the
// function has finished running) makes this safe to assert without a data race.
func TestCloseIsIdempotentConcurrent(t *testing.T) {
	conn := &countingCloseConn{}
	c := &GameConn{conn: conn, reader: bufio.NewReaderSize(conn, 4096)}

	const n = 20
	errs := make([]error, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			errs[i] = c.Close()
		}(i)
	}
	wg.Wait()

	if got := conn.callCount(); got != 1 {
		t.Errorf("underlying net.Conn.Close() called %d times across %d concurrent GameConn.Close() calls, want exactly 1", got, n)
	}
	for i, err := range errs {
		if err != errs[0] {
			t.Errorf("concurrent Close() call %d = %v, want it to equal call 0's result (%v)", i, err, errs[0])
		}
	}
}

// erroringCloseConn is countingCloseConn's sibling for the round-29 MINOR finding that the
// existing TestCloseIsIdempotent* tests never exercise a genuinely non-nil FIRST-call error:
// countingCloseConn's first call always returns nil by construction, so a regression that
// discarded Close()'s captured error entirely (e.g. GameConn.Close() calling c.conn.Close()
// without storing/returning the result) would still pass every existing test in this file. Every
// call here returns the same fixed, caller-supplied error -- standing in for a real net.Conn whose
// underlying Close() genuinely fails (e.g. a TCP RST already pending, or an already-broken fd).
type erroringCloseConn struct {
	net.Conn
	mu    sync.Mutex
	calls int
	err   error
}

func (c *erroringCloseConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.err
}

func (c *erroringCloseConn) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// TestCloseCapturesNonNilFirstCallError is the round-29 regression test proving GameConn.Close()
// actually stores and returns a genuine non-nil error from the underlying net.Conn.Close(), not
// just that it dedups repeated calls (TestCloseIsIdempotent{Sequential,Concurrent} above already
// cover dedup, but only ever against a nil first-call result). Asserts the same error is returned
// from every call -- not only the first -- exactly like the existing idempotency tests do, but this
// time with a first-call result that would expose a regression discarding closeErr entirely.
func TestCloseCapturesNonNilFirstCallError(t *testing.T) {
	wantErr := errors.New("simulated genuine close failure")
	conn := &erroringCloseConn{err: wantErr}
	c := &GameConn{conn: conn, reader: bufio.NewReaderSize(conn, 4096)}

	err1 := c.Close()
	err2 := c.Close()
	err3 := c.Close()

	if got := conn.callCount(); got != 1 {
		t.Errorf("underlying net.Conn.Close() called %d times across 3 GameConn.Close() calls, want exactly 1", got)
	}
	if err1 != wantErr {
		t.Errorf("first Close() = %v, want it to be the underlying net.Conn.Close()'s genuine error (%v) -- a regression that discards the captured error (e.g. calling c.conn.Close() without storing its result) would return nil here instead", err1, wantErr)
	}
	if err2 != err1 {
		t.Errorf("second Close() = %v, want it to equal the first Close()'s captured error (%v)", err2, err1)
	}
	if err3 != err1 {
		t.Errorf("third Close() = %v, want it to equal the first Close()'s captured error (%v)", err3, err1)
	}
}
