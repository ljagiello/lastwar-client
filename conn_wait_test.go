package main

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// newPipeGameConnPair returns two GameConns wired together over a net.Pipe, mirroring
// TestGameConnSendReceiveRoundTrip's fake-server pattern (conn_test.go): client is the real
// GameConn under test, server is a fake server a test goroutine drives directly. Both ends are
// closed automatically via t.Cleanup, which also unblocks any goroutine still parked in a
// Read/Write on the pipe once the test returns.
func newPipeGameConnPair(t *testing.T) (client, server *GameConn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		c1.Close()
		c2.Close()
	})
	client = &GameConn{conn: c1, reader: bufio.NewReaderSize(c1, 4096)}
	server = &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}
	return client, server
}

// readAndReply is the fake-server half used by the sendAndWait tests below: read one request off
// server, then reply to it with the given cmd/params. Run in a goroutine since net.Pipe is
// unbuffered/synchronous -- the client's send and this read rendezvous directly. Takes no
// *testing.T: it runs in a background goroutine that may still be alive after the test function
// returns (unblocked by newPipeGameConnPair's t.Cleanup), and calling T methods from a goroutine
// after the test has completed is unsafe.
func readAndReply(server *GameConn, replyCmd string, replyParams *SFSObject) {
	env, err := server.ReadEnvelope()
	if err != nil {
		return
	}
	msg, ok := env.AsExtension()
	if !ok {
		return
	}
	cmd := replyCmd
	if cmd == "" {
		cmd = msg.Cmd
	}
	_ = server.SendExtension(cmd, replyParams)
}

func TestSendAndWaitSuccess(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := NewSFSObject()
		resp.PutBool("success", true)
		readAndReply(server, "", resp)
	}()

	msg, err := sendAndWait(client, "test success", "test.cmd", NewSFSObject())
	if err != nil {
		t.Fatalf("sendAndWait: %v", err)
	}
	if outcome, code := classifyResponse(msg); outcome != outcomeSuccess {
		t.Errorf("classifyResponse() = (%v, %q), want outcomeSuccess", outcome, code)
	}
}

func TestSendAndWaitBenignErrorCode(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := NewSFSObject()
		resp.PutUtfString("errorCode", "602026") // benignErrorCodes: "In production, please be patient."
		readAndReply(server, "", resp)
	}()

	msg, err := sendAndWait(client, "test benign", "test.cmd", NewSFSObject())
	if err != nil {
		t.Fatalf("sendAndWait returned an error for a benign errorCode: %v", err)
	}
	if outcome, code := classifyResponse(msg); outcome != outcomeBenign || code != "602026" {
		t.Errorf("classifyResponse() = (%v, %q), want (outcomeBenign, \"602026\")", outcome, code)
	}
}

func TestSendAndWaitRealFailure(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := NewSFSObject()
		resp.PutUtfString("errorCode", "999999") // not in benignErrorCodes
		readAndReply(server, "", resp)
	}()

	msg, err := sendAndWait(client, "test failure", "test.cmd", NewSFSObject())
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
	client, _ := newPipeGameConnPair(t)

	start := time.Now()
	_, err := waitFor(client, 50*time.Millisecond, func(*Envelope) bool { return true })
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

// TestWaitForCmdSkipsUnmatchedPushes checks that waitForCmd (and the waitFor it's built on) skips
// past a push whose cmd doesn't match and keeps reading rather than returning the wrong message.
func TestWaitForCmdSkipsUnmatchedPushes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		unrelated := NewSFSObject()
		unrelated.PutUtfString("noise", "ignore me")
		if err := server.SendExtension("some.other.push", unrelated); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension("wanted.cmd", resp)
	}()

	msg, err := waitForCmd(client, 500*time.Millisecond, "wanted.cmd")
	if err != nil {
		t.Fatalf("waitForCmd: %v", err)
	}
	if msg.Cmd != "wanted.cmd" {
		t.Errorf("Cmd = %q, want wanted.cmd", msg.Cmd)
	}
}

// TestWaitForCmdSkipRedactsCredentialFields is the round-11 regression test for waitFor's generic
// "skipped push while waiting" Debug logger (login.go:513-515): if push.account.login.new --
// which carries a live loginKey in cleartext -- arrives while a caller is waiting for a different
// cmd (the exact race login.go:372/386's two separate waitForCmd calls leave open), it falls into
// this skip-and-log branch instead of the dedicated, already-redacted push.account.login.new read
// site the round-10 fix hardened. Proves the skip-logger's output never contains the raw loginKey.
func TestWaitForCmdSkipRedactsCredentialFields(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"

	go func() {
		push := NewSFSObject()
		push.PutUtfString("loginKey", secretLoginKey)
		push.PutUtfString("gameUid", "g1")
		if err := server.SendExtension("push.account.login.new", push); err != nil {
			return
		}
		resp := NewSFSObject()
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

	msg, err := waitForCmd(client, 500*time.Millisecond, "account.login.new")
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

// TestBuildBaseZoneLoginAddrEmptyIP is the regression test for buildBaseZoneLoginAddr's empty-ip
// guard (login.go): an empty ip must produce a clear error rather than silently building a
// ":<port>"-shaped address, which Go's "host:port" dial syntax treats as the loopback interface
// (see main.go's equivalent firstHost(ip) == "" guard on the cross-server login path, which this
// mirrors). Exercised directly against the small helper Login() calls -- rather than through a
// full Login() integration test with fake GSL/game servers -- since this is a pure function of its
// two arguments and doesn't need any network fakery to prove the guard fires.
func TestBuildBaseZoneLoginAddrEmptyIP(t *testing.T) {
	_, err := buildBaseZoneLoginAddr("", 9339)
	if err == nil {
		t.Fatal("buildBaseZoneLoginAddr(\"\", 9339): expected an error for an empty ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
}

// TestBuildBaseZoneLoginAddrNonEmptyIP is TestBuildBaseZoneLoginAddrEmptyIP's happy-path
// counterpart: a normal, non-empty ip must still build the expected "host:port" address and return
// no error, confirming the new guard doesn't reject valid input.
func TestBuildBaseZoneLoginAddrNonEmptyIP(t *testing.T) {
	addr, err := buildBaseZoneLoginAddr("203.0.113.5", 9339)
	if err != nil {
		t.Fatalf("buildBaseZoneLoginAddr: unexpected error for a valid ip: %v", err)
	}
	if want := "203.0.113.5:9339"; addr != want {
		t.Errorf("addr = %q, want %q", addr, want)
	}
}

// TestBuildBaseZoneLoginAddrFirstOfFallbackList confirms buildBaseZoneLoginAddr's guard checks
// firstHost's result (the "|"-delimited list entry actually used to dial), not the raw ip string --
// a pipe-delimited list starting with an empty entry must still be caught, not let through just
// because the raw string itself is non-empty.
func TestBuildBaseZoneLoginAddrFirstOfFallbackList(t *testing.T) {
	if _, err := buildBaseZoneLoginAddr("|203.0.113.5", 9339); err == nil {
		t.Error("buildBaseZoneLoginAddr(\"|203.0.113.5\", 9339): expected an error (firstHost of this list is empty), got nil")
	}
	addr, err := buildBaseZoneLoginAddr("203.0.113.5|198.51.100.7", 9339)
	if err != nil {
		t.Fatalf("buildBaseZoneLoginAddr: unexpected error: %v", err)
	}
	if want := "203.0.113.5:9339"; addr != want {
		t.Errorf("addr = %q, want %q (first entry of the fallback list)", addr, want)
	}
}

// TestWaitForInitPushHalfwayActivePull checks waitForInitPush's two-phase deadline scheme: when
// the server stays completely silent (no `init` push ever arrives), the login.init active-pull
// fallback still gets sent roughly at the halfway point of the window, not at the very start and
// not saved for the very end -- which is the entire point of capping the first read's deadline at
// the halfway mark instead of the full window (see waitForInitPush's doc comment in login.go).
func TestWaitForInitPushHalfwayActivePull(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const window = 200 * time.Millisecond
	activePullAt := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				continue
			}
			if msg.Cmd == "login.init" {
				activePullAt <- time.Since(start)
				return // stay silent for the rest of the window -- the `init` push is never sent
			}
		}
	}()

	buildings, visitors, gotInit, err := waitForInitPush(client, window)

	if gotInit {
		t.Fatalf("expected gotInit=false (server never sends the init push), got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err != nil {
		t.Errorf("err = %v, want nil (a genuine silence-until-deadline timeout is not an error)", err)
	}

	select {
	case elapsed := <-activePullAt:
		// login.init should fire roughly at the halfway point (window/2 = 100ms here), not
		// essentially immediately and not saved for the very end. These bounds are deliberately
		// wide (window/8 to window*7/8, i.e. 25ms-175ms of the 200ms window here) rather than a
		// tight quarter/three-quarter band: this is a wall-clock assertion with no injectable
		// clock, so it's inherently susceptible to scheduler/CI jitter, and this test is only
		// trying to prove the active pull fired somewhere in the middle of the window -- not pin
		// down precisely when.
		if elapsed < window/8 || elapsed > window*7/8 {
			t.Errorf("login.init sent at %v, want roughly the halfway point (~%v of a %v window)", elapsed, window/2, window)
		}
	default:
		t.Fatal("expected waitForInitPush to send login.init as an active-pull fallback partway through the window, but it never arrived")
	}
}

// TestWaitForInitPushConnectionFailure is the regression test for waitForInitPush's terminal-error
// return: a genuine connection failure (here: the peer closing, giving ReadEnvelope a real EOF/
// closed-pipe error) must be visible to the caller as a non-nil, non-timeout error -- not silently
// collapsed into the exact same (nil, nil, false, nil) outcome as a plain silence-until-deadline
// timeout, which is what waitForInitPush used to return unconditionally (discarding the real
// ReadEnvelope error entirely; see login.go's call site and doc comment). Mirrors
// TestWaitForInitPushHalfwayActivePull's structure, but forces a real error instead of letting the
// server just stay silent.
func TestWaitForInitPushConnectionFailure(t *testing.T) {
	client, server := newPipeGameConnPair(t)
	server.conn.Close() // simulate a real connection failure (EOF/reset), not silence

	const window = 200 * time.Millisecond
	start := time.Now()
	buildings, visitors, gotInit, err := waitForInitPush(client, window)
	elapsed := time.Since(start)

	if gotInit {
		t.Fatalf("expected gotInit=false, got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err == nil {
		t.Fatal("expected a non-nil error for a genuine connection failure, got nil (indistinguishable from a plain timeout)")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Errorf("err = %v, want a genuine connection-failure error, not a timeout error", err)
	}
	if elapsed > window {
		t.Errorf("waitForInitPush took %v, want it to return promptly on connection failure rather than waiting out the full %v window", elapsed, window)
	}
}
