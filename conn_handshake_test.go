package main

import (
	"bytes"
	"errors"
	"log/slog"
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
