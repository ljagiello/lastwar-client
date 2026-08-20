package session

import (
	"bytes"
	"errors"
	"io"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSendAndWaitSuccess(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		ReadAndReply(server, "", resp)
	}()

	msg, err := SendAndWait(client, "test success", "test.cmd", sfs.NewSFSObject())
	if err != nil {
		t.Fatalf("sendAndWait: %v", err)
	}
	if outcome, code := classifyResponse(msg); outcome != outcomeSuccess {
		t.Errorf("classifyResponse() = (%v, %q), want outcomeSuccess", outcome, code)
	}
}

func TestSendAndWaitBenignErrorCode(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		resp := sfs.NewSFSObject()
		resp.PutUtfString("errorCode", "602026") // benignErrorCodes: "In production, please be patient."
		ReadAndReply(server, "", resp)
	}()

	// cmd must be "building.production.collect" here, not an arbitrary synthetic name: round 19
	// scoped benignErrorCodes by cmd (conn.go), and 602026 is documented as scoped exclusively to
	// this one cmd -- ReadAndReply echoes back whatever cmd the client sent, so this must match for
	// classifyResponse to actually take the benign path this test means to exercise.
	msg, err := SendAndWait(client, "test benign", "building.production.collect", sfs.NewSFSObject())
	if err != nil {
		t.Fatalf("sendAndWait returned an error for a benign errorCode: %v", err)
	}
	if outcome, code := classifyResponse(msg); outcome != outcomeBenign || code != "602026" {
		t.Errorf("classifyResponse() = (%v, %q), want (outcomeBenign, \"602026\")", outcome, code)
	}
}

func TestSendAndWaitRealFailure(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		resp := sfs.NewSFSObject()
		resp.PutUtfString("errorCode", "999999") // not in benignErrorCodes
		ReadAndReply(server, "", resp)
	}()

	msg, err := SendAndWait(client, "test failure", "test.cmd", sfs.NewSFSObject())
	if err == nil {
		t.Fatal("expected sendAndWait to return an error for a genuine (non-benign) errorCode")
	}
	if msg == nil {
		t.Fatal("expected sendAndWait to still return the response message alongside the error")
	}
	if outcome, code := classifyResponse(msg); outcome != outcomeFailure || code != "999999" {
		t.Errorf("classifyResponse() = (%v, %q), want (outcomeFailure, \"999999\")", outcome, code)
	}
}

// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError is the round-28 regression test for the
// MAJOR finding that sendAndWait used to return a SendExtension/write-stage failure completely
// unwrapped, indistinguishable from waitForCmd's benign wait-stage timeout (TestWaitForTimeout
// above) once the underlying write error happened to itself be a Timeout()==true net.Error --
// exactly what SendEnvelope's writeTimeout deadline produces on a genuinely half-open connection.
// Reuses writeFailConn (defined above for TestWaitForInitPushSendExtensionFailure) to force
// conn.SendExtension's underlying Write to fail deterministically, injecting fakeTimeoutNetError
// as the write failure specifically so this test fails loudly if sendAndWait ever stops wrapping
// the write-stage error: without the fix, errors.As below would find the raw, unwrapped
// fakeTimeoutNetError and this test's Timeout()==false assertion would correctly catch the
// regression.
func TestSendAndWaitWriteStageFailureIsNonTimeoutNetError(t *testing.T) {
	client, _ := NewPipeGameConnPair(t) // server intentionally left idle: the write must fail before any read is attempted
	writeErr := testutil.FakeTimeoutNetError{Msg: "simulated write-deadline-exceeded failure"}
	client.conn = &writeFailConn{Conn: client.conn, err: writeErr}

	// Sanity check on the test's own setup: the injected failure really does report
	// Timeout()==true, mirroring what a genuine deadline-exceeded net.Conn.Write returns -- if
	// this ever went false the test below would trivially pass for the wrong reason.
	if !writeErr.Timeout() {
		t.Fatal("test setup bug: writeErr must itself report Timeout()==true")
	}

	_, err := SendAndWait(client, "test write failure", "test.cmd", sfs.NewSFSObject())
	if err == nil {
		t.Fatal("expected sendAndWait to return an error when the send itself fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a write-stage failure must be distinguishable from sendAndWait's ordinary benign wait-stage timeout (TestWaitForTimeout), even though the underlying write error itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write); downstream containsNonTimeoutNetError-style checks (buildings.go, mail.go, alliance.go, visitors.go) must treat this as a genuine connection failure requiring abort, not a benign per-command timeout")
	}
	if netErr.Temporary() { //nolint:staticcheck // SA1019: asserts the returned net.Error contract, including the deprecated Temporary()
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestSendStageErrorMessage is the round-29 regression test for the MINOR finding that
// sendStageError's Error() method was never directly asserted anywhere -- only incidentally
// exercised via slog output in sendAndWait's send-failure branch and TestSendAndWait
// WriteStageFailureIsNonTimeoutNetError above, neither of which checks its exact returned string.
// Asserts the "send: " prefix (conn.go's sendStageError.Error()) against a known underlying error
// directly.
func TestSendStageErrorMessage(t *testing.T) {
	underlying := errors.New("boom")
	err := SendStageError{Err: underlying}
	if got, want := err.Error(), "send: boom"; got != want {
		t.Errorf("sendStageError{}.Error() = %q, want %q", got, want)
	}
}

func TestSendAndWaitTimeoutNoResponse(t *testing.T) {
	// sendAndWait takes no timeout parameter -- it always waits via waitForCmd(conn,
	// defaultCmdTimeout, ...), and defaultCmdTimeout (conn.go) is a plain 8*time.Second const, not
	// a var, so a test can't override it either. Exercising this sub-case for real would mean
	// either blocking this test for a genuine 8s (violates "keep test timeouts short" for the
	// whole file) or changing production code this round doesn't authorize touching: turning
	// defaultCmdTimeout into a package-level var a test could shrink for its duration, or adding
	// an explicit timeout parameter to sendAndWait. TestWaitForTimeout below covers the same
	// underlying deadline mechanism (waitFor's read-deadline handling, which waitForCmd and thus
	// sendAndWait sit directly on top of) with a short, test-scoped timeout instead.
	t.Skip("sendAndWait's 8s timeout is hardcoded (const, no parameter) -- not testable fast without a production change; see comment above")
}

// TestWaitForTimeout directly exercises the deadline-driven error path that sendAndWait's
// (untestable-fast, see TestSendAndWaitTimeoutNoResponse) timeout case sits on top of: waitFor
// takes an explicit timeout, so this can run with a short one instead of a real 8s.
func TestWaitForTimeout(t *testing.T) {
	client, _ := NewPipeGameConnPair(t)

	start := time.Now()
	_, err := WaitFor(client, 50*time.Millisecond, func(*Envelope) bool { return true })
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error when no envelope ever arrives before the deadline")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v (%T), want a net.Error with Timeout()=true", err, err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("waitFor took %v to return, want close to the 50ms timeout", elapsed)
	}
}

// delayedFirstReadConn wraps a net.Conn and makes its first Read call sleep for a fixed delay
// before delegating to the embedded conn, while making SetReadDeadline an unconditional no-op.
// Used by TestWaitForDeadlineElapsedAfterNonMatchingEnvelope below to deterministically construct
// waitFor's untested "loop iterated at least once (a non-matching envelope was read), THEN
// remaining<=0 fired on a LATER iteration" exit, without racing a tight wall-clock timing window:
// since SetReadDeadline here never actually arms a real deadline, the ONLY way a read in that
// test can ever fail with a timeout is via waitFor's own remaining<=0 wall-clock check at the top
// of its loop -- not a genuine per-read SetReadDeadline+ReadEnvelope timeout (the OTHER exit from
// waitFor, already covered by TestWaitForTimeout). That makes the resulting error unambiguously
// attributable to the branch under test.
type delayedFirstReadConn struct {
	net.Conn
	delay     time.Duration
	firstDone bool
}

func (c *delayedFirstReadConn) SetReadDeadline(time.Time) error { return nil }

func (c *delayedFirstReadConn) Read(b []byte) (int, error) {
	if !c.firstDone {
		c.firstDone = true
		time.Sleep(c.delay)
	}
	return c.Conn.Read(b)
}

// TestWaitForDeadlineElapsedAfterNonMatchingEnvelope is the regression test for waitFor's
// wall-clock-deadline-elapsed exit (login.go: the `if remaining <= 0` check at the top of
// waitFor's read loop). Before this round's fix, that branch returned a bare fmt.Errorf --  not a
// net.Error -- which silently defeated every net.Error/Timeout() "benign timeout vs. dead
// connection" distinction built on top of waitFor/waitForCmd across rounds 20-22 (buildings.go,
// mail.go, visitors.go, alliance.go, interactive.go), all of which assume sendAndWait's/
// waitForCmd's ordinary timeout outcome IS ITSELF a net.Error with Timeout()==true.
// TestWaitForTimeout above already covers the OTHER exit from this same function (a genuine
// per-read timeout when no envelope ever arrives at all) but never touches this one: it's only
// reachable when a NON-matching envelope is successfully read close enough to the deadline that
// time.Until(deadline) goes <=0 on a LATER loop iteration -- e.g. an unrelated game push arriving
// just before an operator's -interactive command's response would.
//
// Constructed deterministically via delayedFirstReadConn rather than racing a tight timing
// window: the first (and, since the envelope fits in one bufio.Reader fill, only) real read
// deliberately takes far longer than the full waitFor timeout before returning a valid
// non-matching envelope, while SetReadDeadline is a no-op the whole time -- so the read always
// "succeeds" (no per-read timeout is even possible here), and by the time waitFor's loop goes
// back to the top, the wall-clock deadline has already, unambiguously elapsed.
func TestWaitForDeadlineElapsedAfterNonMatchingEnvelope(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		_ = c1.Close()
		_ = c2.Close()
	})

	const timeout = 30 * time.Millisecond
	const readDelay = 10 * timeout // comfortably longer than the whole waitFor timeout

	wrapped := &delayedFirstReadConn{Conn: c1, delay: readDelay}
	client := NewGameConnForTest(wrapped)
	server := NewGameConnForTest(c2)

	go func() {
		unrelated := sfs.NewSFSObject()
		unrelated.PutUtfString("noise", "an unrelated push, not what pred is waiting for")
		_ = server.SendExtension("some.other.push", unrelated)
	}()

	start := time.Now()
	env, err := WaitFor(client, timeout, func(e *Envelope) bool {
		msg, ok := e.AsExtension()
		return ok && msg.Cmd == "never.arrives"
	})
	elapsed := time.Since(start)

	if env != nil {
		t.Fatalf("expected no matching envelope (only the unrelated push was ever sent), got %+v", env)
	}
	if err == nil {
		t.Fatal("expected an error once the deadline elapsed after the non-matching envelope was read, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v (%T), want a net.Error with Timeout()=true -- the deadline-elapsed branch must satisfy net.Error just like the per-read-timeout branch does, so downstream errors.As(err, &netErr) checks (interactive.go, buildings.go, mail.go, visitors.go, alliance.go) treat it as benign rather than fatal", err, err)
	}
	// delayedFirstReadConn's SetReadDeadline is a deliberate no-op, so the real per-read network
	// timeout mechanism can never fire in this test -- confirmed by construction, not just by the
	// net.Error assertion above. elapsed must be at least readDelay: waitFor can only have
	// returned after actually reading (and skipping) the non-matching envelope, which required
	// blocking through the full artificial read delay first.
	if elapsed < readDelay {
		t.Errorf("waitFor returned after %v, want at least readDelay (%v): it must have blocked through the slow first read before hitting the deadline-elapsed check", elapsed, readDelay)
	}
}

// TestWaitForCmdSkipsUnmatchedPushes checks that waitForCmd (and the waitFor it's built on) skips
// past a push whose cmd doesn't match and keeps reading rather than returning the wrong message.
func TestWaitForCmdSkipsUnmatchedPushes(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		unrelated := sfs.NewSFSObject()
		unrelated.PutUtfString("noise", "ignore me")
		if err := server.SendExtension("some.other.push", unrelated); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension("wanted.cmd", resp)
	}()

	msg, err := WaitForCmd(client, 500*time.Millisecond, "wanted.cmd")
	if err != nil {
		t.Fatalf("waitForCmd: %v", err)
	}
	if msg.Cmd != "wanted.cmd" {
		t.Errorf("Cmd = %q, want wanted.cmd", msg.Cmd)
	}
}

// TestWaitForCmdSurvivesNonExtensionEnvelope is the round-52 regression test for the MAJOR finding
// that two more, previously-overlooked siblings of round 51's AsExtension nil-msg guard fix
// (buildings.go's FetchBuildings, login.go's waitForInitPush) had zero test coverage: waitForCmd's
// own predicate closure (`msg, ok := e.AsExtension(); if !ok { return false }`, login.go) and
// waitFor's "skipped push while waiting" debug logger (`if msg, ok := env.AsExtension(); ok {
// slog.Debug(...) }`, login.go) -- both reached on every single sendAndWait/waitForCmd call across
// the entire codebase (mail.go, alliance.go, visitors.go, vip.go, buildings.go's CollectAll), not
// just the two less-central sites round 51 already covered. A controllerSystem envelope (e.g. the
// client's own background heartbeat PingPong reply, sent every ~4s once connected) makes
// AsExtension() return ok=false, exercising both guards in the same call. Sends one such envelope
// before the actual awaited extension response, proving waitForCmd survives it silently instead of
// panicking on a nil msg dereference.
func TestWaitForCmdSurvivesNonExtensionEnvelope(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		if err := server.SendEnvelope(ControllerSystem, ActionPingPong, sfs.NewSFSObject()); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension("wanted.cmd", resp)
	}()

	msg, err := WaitForCmd(client, 500*time.Millisecond, "wanted.cmd")
	if err != nil {
		t.Fatalf("waitForCmd: %v", err)
	}
	if msg.Cmd != "wanted.cmd" {
		t.Errorf("Cmd = %q, want wanted.cmd", msg.Cmd)
	}
}

// TestWaitForSurvivesCorruptEnvelope is the round-49 regression test for the MAJOR finding that
// waitFor (login.go) -- the shared read-loop primitive underlying every sendAndWait/waitForCmd
// call plus the raw login-response waits in login.go's Login and crossserver.go's
// DoCrossServerLogin -- used to return ANY ReadEnvelope error immediately with zero net.Error
// classification at all: `if err != nil { return nil, err }`. A plain sfs.DecodeObject parse failure
// on one malformed/unrecognized push (never itself a net.Error, since sfs.ReadPacket has already
// fully consumed that frame's bytes before sfs.DecodeObject ever runs, so the stream stays in sync)
// used to abort the caller's single, non-retried wait outright, even though the genuinely awaited
// response/push might arrive on the very next read -- the same class of bug login.go's
// waitForInitPush had before its own round-48 fix. The fake server here writes one
// well-framed-but-undecodable packet (mustEncodeCorruptPacket, decode_test.go) directly to the raw
// connection, then sends a normal matching envelope. waitFor must survive the corrupt packet (a
// Warn logged, not an abort) and still return the following matching envelope.
func TestWaitForSurvivesCorruptEnvelope(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	go func() {
		if _, err := server.conn.Write(testutil.MustEncodeCorruptPacket(t, "field", "value")); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension("wanted.cmd", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	msg, err := WaitForCmd(client, 2*time.Second, "wanted.cmd")

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("waitForCmd: %v (a single corrupt/undecodable envelope must not abort the wait -- the stream stays in sync and a subsequent matching envelope must still be read)", err)
	}
	if msg.Cmd != "wanted.cmd" {
		t.Errorf("Cmd = %q, want wanted.cmd", msg.Cmd)
	}
	logged := buf.String()
	if !strings.Contains(logged, "failed to read/decode an envelope while waiting") {
		t.Errorf("expected a Warn about the corrupt envelope, got:\n%s", logged)
	}
}

// TestWaitForConsecutiveDecodeFailuresBoundary is the round-50 regression test for the MAJOR
// finding that waitFor's round-49 fix (tolerating a corrupt envelope instead of aborting) removed
// the only thing that used to bound how long a hostile peer could keep waitFor spinning: nothing
// capped how many consecutive malformed/undecodable frames it would silently tolerate before the
// caller-supplied wall-clock timeout eventually fired, letting a peer stream them as fast as the
// link allows for the full window of every sendAndWait/waitForCmd call in the codebase. Proves
// maxConsecutiveDecodeFailures (login.go) is a strict `>` bound: exactly that many corrupt frames
// in a row still lets a subsequent matching envelope succeed, one more gives up with a clear error
// instead of continuing to tolerate them.
func TestWaitForConsecutiveDecodeFailuresBoundary(t *testing.T) {
	sendCorruptThenMatch := func(t *testing.T, n int) (err error) {
		client, server := NewPipeGameConnPair(t)
		// Deliberately not gated on the server goroutine finishing: once waitForCmd gives up
		// early (the cap+1 case), the client stops reading mid-stream and net.Pipe's
		// unbuffered/synchronous Write blocks forever waiting for a reader that will never come
		// back -- NewPipeGameConnPair's own t.Cleanup closes both ends of the pipe at test end,
		// which unblocks that pending write with an error and lets the goroutine exit; there's
		// nothing else worth synchronizing on here.
		go func() {
			for i := 0; i < n; i++ {
				if _, werr := server.conn.Write(testutil.MustEncodeCorruptPacket(t, "field", "value")); werr != nil {
					return
				}
			}
			resp := sfs.NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendExtension("wanted.cmd", resp)
		}()

		_, err = WaitForCmd(client, 2*time.Second, "wanted.cmd")
		return err
	}

	t.Run("exactly cap consecutive corrupt frames: still succeeds", func(t *testing.T) {
		if err := sendCorruptThenMatch(t, MaxConsecutiveDecodeFailures); err != nil {
			t.Errorf("waitForCmd() error = %v, want nil (exactly the cap must still be tolerated)", err)
		}
	})

	t.Run("cap+1 consecutive corrupt frames: gives up", func(t *testing.T) {
		err := sendCorruptThenMatch(t, MaxConsecutiveDecodeFailures+1)
		if err == nil {
			t.Fatal("waitForCmd() error = nil, want an error once the consecutive-failure cap is exceeded")
		}
		if !strings.Contains(err.Error(), "consecutive malformed/undecodable envelopes") {
			t.Errorf("err = %v, want it to mention the consecutive-failure cap being exceeded", err)
		}
		// Round-51 regression assertion for the MAJOR finding that this give-up error, by
		// construction, is never itself a net.Error (this branch is only reached after both
		// the Timeout()==true check and containsNonTimeoutNetError(err) above have already
		// ruled that out for the underlying corrupt-frame error) -- so without sfs.DeadConnError's
		// wrap (login.go), every containsNonTimeoutNetError/errors.As(&netErr)-based "abort on
		// dead connection" check across the codebase (CollectAll, ClaimAllMail, GreetVisitors,
		// shouldAbortBeforeInteractive, ...) would silently treat a connection that just proved
		// it cannot decode 20+ consecutive frames as an ordinary, non-fatal failure instead of
		// the fatal one it actually represents.
		var netErr net.Error
		if !errors.As(err, &netErr) {
			t.Fatalf("err = %v (%T), want it to satisfy net.Error (via sfs.DeadConnError's wrap)", err, err)
		}
		if netErr.Timeout() {
			t.Errorf("netErr.Timeout() = true, want false")
		}
		if !ContainsNonTimeoutNetError(err) {
			t.Errorf("containsNonTimeoutNetError(err) = false, want true -- every downstream 'abort on dead connection' check in this codebase uses this helper")
		}
	})
}

// TestWaitForNonMatchingEnvelopeCapBoundary is the round-53 regression test for the MAJOR finding
// that maxConsecutiveDecodeFailures only ever bounded consecutive DECODE FAILURES -- a hostile
// peer streaming well-formed, successfully-decoded, simply-irrelevant extension pushes for the
// duration of a wait window resets that counter to 0 on every single one and was never slowed down
// by it, each one still costing a full sfs.ReadPacket/sfs.DecodeObject/AsExtension cycle for the entire
// caller-supplied timeout. Proves maxNonMatchingEnvelopesPerWait (login.go) is a strict `>` bound:
// exactly that many well-formed-but-irrelevant pushes in a row still lets the following matching
// one succeed, one more makes waitForCmd give up with a benign (Timeout()==true) error instead of
// continuing to tolerate them indefinitely.
func TestWaitForNonMatchingEnvelopeCapBoundary(t *testing.T) {
	sendNoiseThenMatch := func(t *testing.T, n int) (err error) {
		client, server := NewPipeGameConnPair(t)
		// Deliberately not gated on the server goroutine finishing -- see
		// TestWaitForConsecutiveDecodeFailuresBoundary's identical comment above for why.
		go func() {
			noise := sfs.NewSFSObject()
			noise.PutUtfString("irrelevant", "noise")
			for i := 0; i < n; i++ {
				if err := server.SendExtension("irrelevant.cmd", noise); err != nil {
					return
				}
			}
			resp := sfs.NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendExtension("wanted.cmd", resp)
		}()

		_, err = WaitForCmd(client, 5*time.Second, "wanted.cmd")
		return err
	}

	t.Run("exactly cap non-matching envelopes: still succeeds", func(t *testing.T) {
		if err := sendNoiseThenMatch(t, MaxNonMatchingEnvelopesPerWait); err != nil {
			t.Errorf("waitForCmd() error = %v, want nil (exactly the cap must still be tolerated)", err)
		}
	})

	t.Run("cap+1 non-matching envelopes: gives up", func(t *testing.T) {
		err := sendNoiseThenMatch(t, MaxNonMatchingEnvelopesPerWait+1)
		if err == nil {
			t.Fatal("waitForCmd() error = nil, want an error once the non-matching-envelope cap is exceeded")
		}
		if !strings.Contains(err.Error(), "non-matching envelopes processed") {
			t.Errorf("err = %v, want it to mention the non-matching-envelope cap being exceeded", err)
		}
		var netErr net.Error
		if !errors.As(err, &netErr) {
			t.Fatalf("err = %v (%T), want it to satisfy net.Error (via deadlineExceededError)", err, err)
		}
		if !netErr.Timeout() {
			t.Errorf("netErr.Timeout() = true, want false -- this is a benign give-up (like a real timeout), not a genuine dead-connection error (unlike the maxConsecutiveDecodeFailures give-up, which uses sfs.DeadConnError with Timeout()==false)")
		}
	})
}

// TestWaitForCmdSkipRedactsCredentialFields is the round-11 regression test for waitFor's generic
// "skipped push while waiting" Debug logger (login.go:513-515): if push.account.login.new --
// which carries a live loginKey in cleartext -- arrives while a caller is waiting for a different
// cmd (the exact race login.go:372/386's two separate waitForCmd calls leave open), it falls into
// this skip-and-log branch instead of the dedicated, already-redacted push.account.login.new read
// site the round-10 fix hardened. Proves the skip-logger's output never contains the raw loginKey.
func TestWaitForCmdSkipRedactsCredentialFields(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"

	go func() {
		push := sfs.NewSFSObject()
		push.PutUtfString("loginKey", secretLoginKey)
		push.PutUtfString("gameUid", "g1")
		if err := server.SendExtension("push.account.login.new", push); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension("account.login.new", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	// Debug level explicitly enabled -- this skip-logger only fires under -log-level debug in
	// production, so a default-level (Info) handler would never emit it and this test would pass
	// vacuously either way.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	msg, err := WaitForCmd(client, 500*time.Millisecond, "account.login.new")
	if err != nil {
		t.Fatalf("waitForCmd: %v", err)
	}
	if msg.Cmd != "account.login.new" {
		t.Errorf("Cmd = %q, want account.login.new", msg.Cmd)
	}

	if logged := buf.String(); strings.Contains(logged, secretLoginKey) {
		t.Errorf("waitFor's skip-logger leaks the raw loginKey in cleartext:\n%s", logged)
	}
}

// writeFailConn wraps a net.Conn and makes every Write fail with a fixed error while leaving
// Read (and everything else -- SetReadDeadline, SetWriteDeadline, Close, ...) delegated to the
// embedded conn unchanged. Used by TestWaitForInitPushSendExtensionFailure below to simulate a
// half-open connection: a local write that fails immediately, paired with a read that would
// otherwise genuinely block until the caller's deadline (there's no peer-close/EOF involved, so
// it's distinguishable from TestWaitForInitPushConnectionFailure's scenario above).
type writeFailConn struct {
	net.Conn
	err error
}

func (w *writeFailConn) Write([]byte) (int, error) { return 0, w.err }

// TestReadPacketGracefulCloseIsNonTimeoutNetError is the packet.go-level regression test for round
// 24's MAJOR finding: a peer's graceful close (a clean FIN, or the far end process simply exiting,
// with nothing sent) surfaces from sfs.ReadPacket's leading io.ReadFull(r, hb[:]) header read as bare
// io.EOF, which does not itself implement net.Error -- and fmt.Errorf's %w wrapping doesn't change
// that, since errors.As only succeeds if SOME error in the chain implements the target interface.
// Left unfixed, every one of the 5 "abort remaining independent work on a genuine dead connection"
// checks built across rounds 16-23 (buildings.go's CollectAll, mail.go's ClaimAllMail, visitors.go's
// GreetVisitors, alliance.go's ClaimAllianceGifts, interactive.go's handleInteractiveLine, all via
// containsNonTimeoutNetError or a direct net.Error check) silently never fires for this, the single
// most realistic real-world failure mode -- empirically reproduced during the audit: wiring an
// equivalent fake conn into CollectAll produced 9 separate wasted requests, each burning a full
// defaultCmdTimeout, instead of aborting after the first.
//
// bytes.NewReader(nil) is itself exactly the shape under test: an io.Reader whose very first Read
// call returns bare io.EOF and nothing else -- standing in for a socket a peer closed before
// sending anything at all (the between-packets case; see
// TestReadPacketMidFrameCloseIsNonTimeoutNetError below for the mid-frame variant).
func TestReadPacketGracefulCloseIsNonTimeoutNetError(t *testing.T) {
	_, err := sfs.ReadPacket(bytes.NewReader(nil))
	if err == nil {
		t.Fatal("expected an error reading a packet off a closed/empty stream, got nil")
	}
	// The fix must wrap, not replace: decode.go's DecodeStreamFile still needs
	// errors.Is(err, io.EOF) to hold for the header-read site specifically, to keep correctly
	// reporting a genuine between-packets stream end as clean.
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want errors.Is(err, io.EOF) to still hold through the net.Error wrapper", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error -- a graceful close must be recognizable as a genuine dead connection, not silently indistinguishable from an ordinary decode error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a graceful close is a genuine dead connection, not the benign kind of net.Error sendAndWait's ordinary per-command timeout produces (see TestWaitForTimeout), which downstream early-abort checks must NOT trip on")
	}
	if netErr.Temporary() { //nolint:staticcheck // SA1019: asserts the returned net.Error contract, including the deprecated Temporary()
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestReadPacketMidFrameCloseIsNonTimeoutNetError is the sfs.ReadFrameField-path sibling of
// TestReadPacketGracefulCloseIsNonTimeoutNetError above: a close partway through a frame (here,
// right after the header byte but before the length field it promised) rather than cleanly between
// packets. packet_oom_test.go's TestReadPacketMidFrameTruncationIsNotClassifiedAsCleanEOF already
// proves this shape correctly satisfies errors.Is(err, io.ErrUnexpectedEOF) (not io.EOF); this test
// proves it ALSO now satisfies net.Error with Timeout()==false, confirming the fix was applied at
// sfs.ReadFrameField itself and not just the one header-read call the audit specifically called out.
func TestReadPacketMidFrameCloseIsNonTimeoutNetError(t *testing.T) {
	// A lone header byte declaring sfs.HdrBigSized (so sfs.ReadPacket expects a 4-byte length field to
	// follow) with nothing after it.
	_, err := sfs.ReadPacket(bytes.NewReader([]byte{sfs.HdrBinary | sfs.HdrEncrypted | sfs.HdrBigSized}))
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("err = %v, satisfies errors.Is(err, io.EOF) -- a mid-frame close must not be misclassified as a clean end-of-stream", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want errors.Is(err, io.ErrUnexpectedEOF)", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false")
	}
}

// eofConn is a minimal net.Conn whose every Read returns bare io.EOF, simulating a peer's graceful
// close at the live-connection level -- as opposed to fakeNetError-style fakes elsewhere in this
// package (buildings_orchestration_test.go et al.), which simulate an already-a-net.Error
// connection failure such as a connection reset. This is the shape a real net.Conn produces for
// THAT specific close, which round 24 found silently defeated every containsNonTimeoutNetError-style
// early-abort check because bare io.EOF does not itself implement net.Error. Every other net.Conn
// method is left as a nil-embed panic if called -- ReadEnvelope never calls them, so a test that did
// would be exercising the wrong thing anyway.
type eofConn struct {
	net.Conn
}

func (eofConn) Read([]byte) (int, error) { return 0, io.EOF }

// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError is the conn.go-level regression test: proves
// the fix reaches GameConn.ReadEnvelope, the actual live-connection call site every orchestration
// loop's sendAndWait/waitForCmd/waitFor chain sits on top of, not just sfs.ReadPacket in isolation.
func TestReadEnvelopeGracefulCloseIsNonTimeoutNetError(t *testing.T) {
	conn := eofConn{}
	client := NewGameConnForTest(conn)

	_, err := client.ReadEnvelope()
	if err == nil {
		t.Fatal("expected an error reading an envelope off a gracefully-closed connection, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- ReadEnvelope's caller chain (waitFor/waitForCmd/sendAndWait) must be able to distinguish this from its own ordinary per-command timeout")
	}
	if netErr.Temporary() { //nolint:staticcheck // SA1019: asserts the returned net.Error contract, including the deprecated Temporary()
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestWaitForCmdSelfCloseWhileBlockedRespondsPromptly is the round-52 regression test for the
// MINOR finding that the "goroutine B calls GameConn.Close() while goroutine A is blocked inside
// ReadEnvelope/waitFor/waitForCmd on that same connection" scenario had no test anywhere in the
// suite -- and that TestReadEnvelopeGracefulCloseIsNonTimeoutNetError above, despite being cited by
// interactive.go's own doc comment as proof this is handled, doesn't actually exercise it: eofConn
// is a synchronous fake with no live blocking read and no concurrent Close() call at all.
//
// On a real *net.TCPConn (the only kind a production GameConn is ever backed by -- see DialGame),
// Close()'ing a connection while another goroutine is blocked in Read on it makes that Read return
// promptly with a *net.OpError wrapping net.ErrClosed, which already satisfies net.Error natively.
// This codebase's own standard concurrency-test fixture, NewPipeGameConnPair (net.Pipe-backed),
// used to diverge: closing the SAME end that's blocked in Read produced bare io.ErrClosedPipe,
// which satisfied neither net.Error nor io.EOF/io.ErrUnexpectedEOF, so waitForCmd would silently
// fall through to login.go's much slower maxConsecutiveDecodeFailures give-up path (~20 failed
// reads) instead of aborting promptly on the first one -- masking any future regression in the
// fast single-read classification real TCP actually depends on. Fixed by widening packet.go's
// sfs.WrapIfClosed to also treat io.ErrClosedPipe as a dead connection, matching io.EOF/
// io.ErrUnexpectedEOF's existing treatment (see its own doc comment for the full rationale). This
// test proves both that a self-close now resolves fast (not via the ~20-iteration give-up path)
// and that the resulting error is correctly classified as a genuine dead connection.
func TestWaitForCmdSelfCloseWhileBlockedRespondsPromptly(t *testing.T) {
	client, _ := NewPipeGameConnPair(t)

	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		_, err := WaitForCmd(client, 5*time.Second, "wanted.cmd")
		errCh <- err
	}()

	// Give the background goroutine a moment to actually block inside ReadEnvelope before closing
	// out from under it -- otherwise Close() could race ahead of the read even starting.
	time.Sleep(50 * time.Millisecond)
	_ = client.Close()

	var err error
	select {
	case err = <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForCmd did not return within 2s of a self-close while blocked -- want a prompt single-read failure, not the ~20-iteration give-up path (or worse, waiting out the full 5s timeout)")
	}
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForCmd() error = nil, want an error for a connection closed out from under a blocked read")
	}
	if elapsed > 1*time.Second {
		t.Errorf("waitForCmd took %v to return after a self-close, want well under 1s -- an elapsed time this long suggests it took the slow maxConsecutiveDecodeFailures give-up path instead of failing promptly on the first read", elapsed)
	}
	if strings.Contains(err.Error(), "consecutive malformed/undecodable") {
		t.Errorf("err = %v, want it NOT to be the maxConsecutiveDecodeFailures give-up error -- a self-close must be classified as a dead connection on the first failed read, not tolerated as a string of decode failures", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a self-close is a genuine dead connection, not a benign timeout")
	}
	if !ContainsNonTimeoutNetError(err) {
		t.Errorf("containsNonTimeoutNetError(err) = false, want true -- every 'abort remaining work on a genuinely dead connection' check in this codebase uses this helper")
	}
}

// TestDeadlineExceededErrorDirect is the minor regression-coverage fix for round 23's
// deadlineExceededError (login.go): go tool cover -func showed Error() and Temporary() at 0.0%
// coverage, with only Timeout() exercised (indirectly, via
// TestWaitForDeadlineElapsedAfterNonMatchingEnvelope above) -- a regression emptying Error()'s
// message or flipping Temporary() to true would go undetected. Exercises all three methods
// directly against the zero value, matching how every real caller constructs it
// (deadlineExceededError{}, waitFor's own deadline-elapsed branch).
func TestDeadlineExceededErrorDirect(t *testing.T) {
	err := DeadlineExceededError{}

	msg := err.Error()
	if msg == "" {
		t.Error("Error() returned an empty message, want a non-empty description of what happened")
	}
	if !strings.Contains(msg, "timed out") && !strings.Contains(msg, "timeout") {
		t.Errorf("Error() = %q, want a message that actually describes a timeout", msg)
	}
	if err.Temporary() {
		t.Error("Temporary() = true, want false")
	}
	if !err.Timeout() {
		t.Error("Timeout() = false, want true")
	}
}
