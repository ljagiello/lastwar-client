package main

import (
	"bufio"
	"errors"
	"net"
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

	buildings, visitors, gotInit := waitForInitPush(client, window)

	if gotInit {
		t.Fatalf("expected gotInit=false (server never sends the init push), got true (buildings=%v visitors=%v)", buildings, visitors)
	}

	select {
	case elapsed := <-activePullAt:
		// login.init should fire roughly at the halfway point (window/2 = 100ms here). Generous
		// quarter/three-quarter bounds absorb goroutine-scheduling jitter without letting the
		// test pass if the fallback fired essentially immediately or essentially at the deadline.
		if elapsed < window/4 || elapsed > window*3/4 {
			t.Errorf("login.init sent at %v, want roughly the halfway point (~%v of a %v window)", elapsed, window/2, window)
		}
	default:
		t.Fatal("expected waitForInitPush to send login.init as an active-pull fallback partway through the window, but it never arrived")
	}
}
