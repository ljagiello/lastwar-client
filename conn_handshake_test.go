package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestDoHandshakeSuccess exercises DoHandshake's normal path against a fake server (same
// newPipeGameConnPair/net.Pipe pattern as the sendAndWait/waitFor tests in conn_wait_test.go):
// the server reads the outgoing HandshakeRequest and replies with a well-formed (no "ec") system
// response, and DoHandshake should return that response's content with no error.
func TestDoHandshakeSuccess(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if env.Controller != controllerSystem || env.Action != actionHandshake {
			return
		}
		resp := NewSFSObject()
		resp.PutUtfString("sess", "abc123")
		_ = server.SendEnvelope(controllerSystem, actionHandshake, resp)
	}()

	got, err := client.DoHandshake(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("DoHandshake: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil response")
	}
	if sess := got.GetString("sess"); sess != "abc123" {
		t.Errorf("response sess = %q, want abc123 (the fake server's reply)", sess)
	}
}

// TestDoHandshakeFailureWrapsErrAuthRejected exercises DoHandshake's failure path: a system
// response carrying an "ec" field is a server-side handshake rejection, and DoHandshake wraps
// that in ErrAuthRejected (conn.go:295-302) so callers can distinguish it from a bare
// dial/timeout/I/O failure, matching the same ec-present convention used by login.go's LOGIN
// FAILED and crossserver.go's CROSS-SERVER LOGIN FAILED errors.
func TestDoHandshakeFailureWrapsErrAuthRejected(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if env.Controller != controllerSystem || env.Action != actionHandshake {
			return
		}
		resp := NewSFSObject()
		resp.PutShort("ec", 4)
		_ = server.SendEnvelope(controllerSystem, actionHandshake, resp)
	}()

	_, err := client.DoHandshake(500 * time.Millisecond)
	if err == nil {
		t.Fatal("expected a non-nil error for an ec-bearing handshake response")
	}
	if !errors.Is(err, ErrAuthRejected) {
		t.Errorf("err = %v, want errors.Is(err, ErrAuthRejected)", err)
	}
}

// TestDoHandshakeSkipRedactsCredentialFields is the round-12 regression test for the
// "skipped envelope while waiting for handshake" fallback branch (conn.go, DoHandshake's read
// loop): if some other envelope -- e.g. an out-of-order push shaped like push.account.login.new,
// carrying a live loginKey -- arrives before the real system/actionHandshake response, DoHandshake
// falls into this skip-and-log branch instead of erroring out. Round 11 added
// SFSObject.StringRedacted() for exactly this kind of "log a full decoded object" call site, but
// this particular site kept calling the raw, unredacted String() instead -- and evaded round-11's
// credential_leak_lint_test.go regex specifically because the .String() call was stashed in a
// local variable (contentStr) two lines above the slog.Info call rather than inline in it. This
// proves the fallback's logged output never contains the raw loginKey, and (since the fake server
// keeps reading after the skip) that DoHandshake still recovers and returns the real response.
func TestDoHandshakeSkipRedactsCredentialFields(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"

	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if env.Controller != controllerSystem || env.Action != actionHandshake {
			return
		}
		// Simulate an out-of-order push arriving before the real handshake response: same
		// {c,a,p} envelope shape as push.account.login.new, sent under an action id (actionLogin)
		// DoHandshake isn't waiting for -- this drives it into the skip-and-log fallback branch
		// rather than the normal-response or ec-rejection paths.
		skipped := NewSFSObject()
		skipped.PutUtfString("loginKey", secretLoginKey)
		skipped.PutUtfString("gameUid", "g1")
		if err := server.SendEnvelope(controllerExtension, actionLogin, skipped); err != nil {
			return
		}

		resp := NewSFSObject()
		resp.PutUtfString("sess", "abc123")
		_ = server.SendEnvelope(controllerSystem, actionHandshake, resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	// Debug/Info level explicitly enabled: the skip-logger fires at Info, but set the handler to
	// Debug so this test would also catch the leak if the log level were ever lowered.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	got, err := client.DoHandshake(500 * time.Millisecond)
	if err != nil {
		t.Fatalf("DoHandshake: %v", err)
	}
	if got == nil || got.GetString("sess") != "abc123" {
		t.Fatalf("expected DoHandshake to recover past the skipped envelope and return the real handshake response, got %+v", got)
	}

	if logged := buf.String(); strings.Contains(logged, secretLoginKey) {
		t.Errorf("DoHandshake's skip-and-log fallback leaks the raw loginKey in cleartext:\n%s", logged)
	}
}

// TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope is the regression test for DoHandshake's
// wall-clock-deadline-elapsed exit (conn.go: the `if remaining <= 0` check at the top of its read
// loop). Before this round's fix, that branch returned a bare fmt.Errorf -- not a net.Error --
// reproducing, in this independent read loop, the exact bug class round 23's
// TestWaitForDeadlineElapsedAfterNonMatchingEnvelope (conn_wait_test.go) fixed for waitFor. Both of
// DoHandshake's current callers (login.go, crossserver.go) happen to treat any non-nil error as an
// unconditional hard abort today, so this doesn't yet change observable behavior -- but the
// invariant (every benign timeout outcome from a read loop in this package must satisfy net.Error
// with Timeout()==true, exactly like sendAndWait's/waitFor's ordinary per-read timeout does) must
// hold here too, before some future caller starts relying on it.
//
// This is only reachable when the loop has already iterated at least once -- i.e. a non-matching
// envelope was successfully read and skipped -- and the deadline elapses on a LATER iteration.
// Constructed deterministically via delayedFirstReadConn (conn_wait_test.go, same package, reused
// verbatim) rather than racing a tight timing window: the first real read deliberately takes far
// longer than the full DoHandshake timeout before returning a valid non-matching envelope, while
// SetReadDeadline is a no-op the whole time -- so the read always "succeeds" (no genuine per-read
// network timeout is even possible here), and by the time DoHandshake's loop goes back to the top,
// the wall-clock deadline has already, unambiguously elapsed.
func TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope(t *testing.T) {
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})

	const timeout = 30 * time.Millisecond
	const readDelay = 10 * timeout // comfortably longer than the whole DoHandshake timeout

	wrapped := &delayedFirstReadConn{Conn: c1, delay: readDelay}
	client := &GameConn{conn: wrapped, reader: bufio.NewReaderSize(wrapped, 4096)}
	server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}

	go func() {
		// Read (and discard) the outgoing HandshakeRequest, then send something that does NOT
		// match controllerSystem/actionHandshake -- this drives DoHandshake's loop into its
		// skip-and-log fallback branch on its first iteration, exactly like a real out-of-order
		// push would, before the deadline elapses on the (deliberately slow) second read.
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		unrelated := NewSFSObject()
		unrelated.PutUtfString("noise", "an unrelated push, not the handshake response")
		_ = server.SendEnvelope(controllerExtension, actionLogin, unrelated)
	}()

	start := time.Now()
	got, err := client.DoHandshake(timeout)
	elapsed := time.Since(start)

	if got != nil {
		t.Fatalf("expected no handshake response (only an unrelated envelope was ever sent), got %+v", got)
	}
	if err == nil {
		t.Fatal("expected an error once the deadline elapsed after the non-matching envelope was skipped, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("err = %v (%T), want a net.Error with Timeout()=true -- DoHandshake's deadline-elapsed branch must satisfy net.Error just like waitFor's identical branch does, so callers that later start doing errors.As(err, &netErr) checks treat it as benign rather than fatal", err, err)
	}
	// delayedFirstReadConn's SetReadDeadline is a deliberate no-op, so the real per-read network
	// timeout mechanism can never fire in this test -- confirmed by construction, not just by the
	// net.Error assertion above. elapsed must be at least readDelay: DoHandshake can only have
	// returned after actually reading (and skipping) the non-matching envelope, which required
	// blocking through the full artificial read delay first.
	if elapsed < readDelay {
		t.Errorf("DoHandshake returned after %v, want at least readDelay (%v): it must have blocked through the slow first read before hitting the deadline-elapsed check", elapsed, readDelay)
	}
}

// TestDoHandshakeReadEnvelopeFailure is the round-26 regression test for DoHandshake's genuine
// ReadEnvelope-failure branch (conn.go: the "read handshake response" error-wrapping return inside
// DoHandshake's read loop) -- go tool cover -func showed DoHandshake at 87.5% statement coverage,
// vs. 100% for waitFor's (login.go) equivalent branch, because none of the other DoHandshake tests
// exercise it: they either succeed cleanly, wrap ErrAuthRejected, or (
// TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope above) deliberately use a fake conn whose
// SetReadDeadline is a no-op specifically so that test's error comes from the wall-clock check, never
// from ReadEnvelope itself returning an error.
//
// Mirrors TestWaitForInitPushConnectionFailure's approach (conn_wait_test.go) rather than
// eofConn/delayedFirstReadConn: the fake server reads (and discards) the real outgoing
// HandshakeRequest -- proving SendEnvelope itself succeeded first -- then closes its end of the pipe
// without ever replying. Per net.Pipe's own semantics (net/pipe.go), closing one end delivers io.EOF,
// not io.ErrClosedPipe, to the other end's pending/future Reads, so the client's subsequent
// ReadEnvelope call fails genuinely (not via any deadline/timeout path). packet.go's
// wrapIfClosed/deadConnError then turns that into a net.Error with Timeout()==false, so asserting
// errors.Is(err, io.EOF) and errors.As(err, &netErr) here proves DoHandshake's
// "read handshake response: %w" wrap survives both all the way out to the caller -- exactly what a
// future caller doing the same net.Error/Timeout() distinction sendAndWait's callers already rely on
// (containsNonTimeoutNetError, buildings.go) would depend on.
func TestDoHandshakeReadEnvelopeFailure(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		server.conn.Close() // close without replying: forces a genuine ReadEnvelope failure, not a timeout
	}()

	_, err := client.DoHandshake(500 * time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when ReadEnvelope genuinely fails mid-handshake, got nil")
	}
	if !strings.Contains(err.Error(), "read handshake response") {
		t.Errorf("err = %v, want it to include DoHandshake's \"read handshake response\" wrapping prefix", err)
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("err = %v, want errors.Is(err, io.EOF) to still hold through DoHandshake's wrap", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error -- proving DoHandshake's wrap preserves this for a future errors.As(err, &netErr) caller", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a genuinely closed connection must be distinguishable from DoHandshake's own benign deadline-elapsed timeout (see TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope)")
	}
}

// TestDoHandshakeSendFailureIsNonTimeoutNetError is the round-29 regression test for the MAJOR
// finding that DoHandshake's own send-stage branch (its c.SendEnvelope call) used a bare
// fmt.Errorf("send handshake: %w", err) instead of sendStageError -- the exact write-vs-read-
// timeout conflation sendStageError (conn.go) was built to prevent in sendAndWait's identical
// send-stage branch, reproduced here in an independent send path. Mirrors
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError's technique (conn_wait_test.go) exactly:
// injects a write failure that itself reports Timeout()==true (fakeTimeoutNetError), and asserts
// the error DoHandshake actually returns reports Timeout()==false once run through
// errors.As(&netErr) -- proving it's forced through sendStageError rather than passed through
// unwrapped, which would otherwise make a connection too broken to even send a request
// indistinguishable from DoHandshake's own benign wall-clock-deadline-elapsed timeout (see
// TestDoHandshakeDeadlineElapsedAfterNonMatchingEnvelope above).
func TestDoHandshakeSendFailureIsNonTimeoutNetError(t *testing.T) {
	client, _ := newPipeGameConnPair(t) // server intentionally left idle: the write must fail before any read is attempted
	writeErr := fakeTimeoutNetError{msg: "simulated write-deadline-exceeded failure"}
	client.conn = &writeFailConn{Conn: client.conn, err: writeErr}

	// Sanity check on the test's own setup: the injected failure really does report
	// Timeout()==true, mirroring what a genuine deadline-exceeded net.Conn.Write returns -- if
	// this ever went false the test below would trivially pass for the wrong reason.
	if !writeErr.Timeout() {
		t.Fatal("test setup bug: writeErr must itself report Timeout()==true")
	}

	_, err := client.DoHandshake(500 * time.Millisecond)
	if err == nil {
		t.Fatal("expected an error when the handshake send itself fails")
	}
	if !strings.Contains(err.Error(), "send handshake") {
		t.Errorf("err = %v, want it to include DoHandshake's \"send handshake\" wrapping prefix", err)
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a send-stage failure must be distinguishable from DoHandshake's own benign deadline-elapsed timeout, even though the underlying write error itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write)")
	}
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestStartHeartbeatSendsPeriodicPingsAndStopsOnClose covers StartHeartbeat's normal loop: pings
// (controllerSystem/actionPingPong) go out roughly every `interval`, and closing the GameConn
// stops the goroutine -- no further pings arrive afterward.
func TestStartHeartbeatSendsPeriodicPingsAndStopsOnClose(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const interval = 20 * time.Millisecond
	client.StartHeartbeat(interval, time.Now())

	pings := make(chan time.Time, 16)
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			if env.Controller == controllerSystem && env.Action == actionPingPong {
				pings <- time.Now()
			}
		}
	}()

	const wantPings = 4
	var times []time.Time
	for i := 0; i < wantPings; i++ {
		select {
		case ts := <-pings:
			times = append(times, ts)
		case <-time.After(2 * time.Second):
			t.Fatalf("only received %d/%d pings before timeout", len(times), wantPings)
		}
	}

	for i := 1; i < len(times); i++ {
		gap := times[i].Sub(times[i-1])
		// Generous bounds to absorb goroutine-scheduling/net.Pipe jitter -- this just confirms
		// pings land roughly at `interval`, not back-to-back and not stalled for multiple ticks.
		if gap < interval/2 || gap > interval*4 {
			t.Errorf("ping %d arrived %v after the previous one, want roughly %v", i, gap, interval)
		}
	}

	client.Close()

	select {
	case <-pings:
		t.Error("received a ping after Close(); heartbeat goroutine should have stopped")
	case <-time.After(interval * 5):
		// expected: heartbeat goroutine exited via stopHeartbeat, no further sends
	}
}

// TestStartHeartbeatSendFailureClosesConn covers StartHeartbeat's error path (conn.go:332-336):
// when a tick's SendEnvelope fails, the goroutine closes the GameConn instead of looping forever
// against a dead connection. The underlying pipe is closed out from under the heartbeat -- rather
// than via GameConn.Close() -- to force the next tick's write to fail without the test itself
// triggering the close-on-failure path it's trying to observe. Success is observed directly on
// the GameConn's own stopHeartbeat channel (same package, so this is legitimate white-box
// access): only GameConn.Close() ever closes it, so seeing it closed is proof StartHeartbeat's
// error branch actually ran c.Close(), not just that the raw pipe died.
func TestStartHeartbeatSendFailureClosesConn(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	const interval = 20 * time.Millisecond
	client.StartHeartbeat(interval, time.Now())

	// Kill the transport out from under the heartbeat goroutine -- its next tick's
	// SendEnvelope/Write will fail, which should drive it into c.Close().
	client.conn.Close()

	select {
	case <-client.stopHeartbeat:
		// closed: StartHeartbeat's error branch called c.Close(), which closed this channel.
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a heartbeat send failure to trigger GameConn.Close()")
	}
}
