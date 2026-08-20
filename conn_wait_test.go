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

	// cmd must be "building.production.collect" here, not an arbitrary synthetic name: round 19
	// scoped benignErrorCodes by cmd (conn.go), and 602026 is documented as scoped exclusively to
	// this one cmd -- readAndReply echoes back whatever cmd the client sent, so this must match for
	// classifyResponse to actually take the benign path this test means to exercise.
	msg, err := sendAndWait(client, "test benign", "building.production.collect", NewSFSObject())
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

// fakeTimeoutNetError is a minimal net.Error whose Timeout() reports true, standing in for the
// *net.OpError a real deadline-exceeded net.Conn.Write returns -- per net.Conn's documented
// contract, "a Write ... that exceeds the [write] deadline ... returns an error that wraps
// os.ErrDeadlineExceeded" and satisfies net.Error with Timeout() == true. Used by
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError below to prove sendAndWait's write-stage
// wrapper (sendStageError, conn.go) forces Timeout()==false even when the underlying cause is
// itself exactly this Timeout()==true shape.
type fakeTimeoutNetError struct{ msg string }

func (e fakeTimeoutNetError) Error() string { return e.msg }
func (fakeTimeoutNetError) Timeout() bool   { return true }
func (fakeTimeoutNetError) Temporary() bool { return true }

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
	client, _ := newPipeGameConnPair(t) // server intentionally left idle: the write must fail before any read is attempted
	writeErr := fakeTimeoutNetError{msg: "simulated write-deadline-exceeded failure"}
	client.conn = &writeFailConn{Conn: client.conn, err: writeErr}

	// Sanity check on the test's own setup: the injected failure really does report
	// Timeout()==true, mirroring what a genuine deadline-exceeded net.Conn.Write returns -- if
	// this ever went false the test below would trivially pass for the wrong reason.
	if !writeErr.Timeout() {
		t.Fatal("test setup bug: writeErr must itself report Timeout()==true")
	}

	_, err := sendAndWait(client, "test write failure", "test.cmd", NewSFSObject())
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
	if netErr.Temporary() {
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
	err := sendStageError{err: underlying}
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
		c1.Close()
		c2.Close()
	})

	const timeout = 30 * time.Millisecond
	const readDelay = 10 * timeout // comfortably longer than the whole waitFor timeout

	wrapped := &delayedFirstReadConn{Conn: c1, delay: readDelay}
	client := &GameConn{conn: wrapped, reader: bufio.NewReaderSize(wrapped, 4096)}
	server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}

	go func() {
		unrelated := NewSFSObject()
		unrelated.PutUtfString("noise", "an unrelated push, not what pred is waiting for")
		_ = server.SendExtension("some.other.push", unrelated)
	}()

	start := time.Now()
	env, err := waitFor(client, timeout, func(e *Envelope) bool {
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

// TestBuildBaseZoneLoginAddrZeroPort is the round-19 regression test for
// buildBaseZoneLoginAddr's port guard: a zero (or negative) port must produce a clear error
// rather than silently building a "host:0"-shaped address. Mirrors
// TestBuildBaseZoneLoginAddrEmptyIP's structure for the port half of the same guard function.
func TestBuildBaseZoneLoginAddrZeroPort(t *testing.T) {
	_, err := buildBaseZoneLoginAddr("203.0.113.5", 0)
	if err == nil {
		t.Fatal("buildBaseZoneLoginAddr(\"203.0.113.5\", 0): expected an error for a zero port, got nil")
	}
	if strings.Contains(err.Error(), "203.0.113.5:0") {
		t.Errorf("err = %q, must not contain a \"host:0\"-shaped address (that's the footgun this guard exists to prevent)", err.Error())
	}
}

// TestLoginRedirectRejectsEmptyRedirectIP is the round-18 regression test for the same
// firstHost-without-emptiness-check gap crossserver_test.go's
// TestDoCrossServerLoginRedirectRejectsEmptyRedirectIP covers on the DoCrossServerLogin side:
// Login()'s own serverInfo redirect branch only checked siObj.GetString("ip") != "", not
// firstHost's resolved result -- so a pipe-malformed ip like "|1.2.3.4" (raw non-empty, but
// firstHost resolves it down to "") built a ":<port>"-shaped dial address via a raw fmt.Sprintf,
// which Go's "host:port" dial syntax silently treats as the loopback interface, instead of
// failing clearly. Reuses login_integration_test.go's fake-GSL/fake-game-server infrastructure
// (newFakeGSLServer/useFakeGSLServer) and crossserver_test.go's fake game listener helpers
// (startFakeGameServer/splitHostPortInt) -- all in this same package -- to drive a real Login()
// call through a successful initial dial and into the redirect branch. Proves Login() now returns
// a clear error (routed through buildBaseZoneLoginAddr, same as the initial dial) instead of
// attempting the loopback dial.
func TestLoginRedirectRejectsEmptyRedirectIP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutUtfString("ip", "|1.2.3.4") // firstHost("|1.2.3.4") == "" -- the malformed case
		si.PutInt("port", 9339)
		si.PutUtfString("zone", "APS2")
		resp := NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	oldHost, oldPort := splitHostPortInt(t, oldAddr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(oldHost), Port: flexPort(oldPort), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error for a pipe-malformed redirect ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
	if !strings.Contains(err.Error(), "serverInfo redirect") {
		t.Errorf("err = %q, want it to mention the serverInfo redirect context", err.Error())
	}
}

// TestLoginRedirectRejectsMissingRedirectPort is the round-19 counterpart to
// TestLoginRedirectRejectsEmptyRedirectIP, covering the port half of buildBaseZoneLoginAddr's
// guard instead of the host half: a serverInfo redirect payload that omits `port` entirely (the
// same shape gsl.go's getIntFlexible silently resolves to 0 for, whether the field is absent or
// present-but-unparseable) must make Login() return a clear error, not silently build and dial a
// "host:0"-shaped address. Mirrors TestLoginRedirectRejectsEmptyRedirectIP's fake-GSL/fake-game-
// server setup, just omitting the `port` field on the serverInfo payload instead of malforming
// `ip`.
func TestLoginRedirectRejectsMissingRedirectPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutUtfString("ip", "203.0.113.9")
		// No "port" field at all -- getIntFlexible(si, "port") resolves this to 0, same as an
		// unparseable port value would.
		si.PutUtfString("zone", "APS2")
		resp := NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	oldHost, oldPort := splitHostPortInt(t, oldAddr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(oldHost), Port: flexPort(oldPort), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error for a redirect payload with a missing port, got nil")
	}
	if strings.Contains(err.Error(), "203.0.113.9:0") {
		t.Errorf("err = %q, must not contain a \"host:0\"-shaped address (that's the footgun this guard exists to prevent)", err.Error())
	}
	if !strings.Contains(err.Error(), "serverInfo redirect") {
		t.Errorf("err = %q, want it to mention the serverInfo redirect context", err.Error())
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

// TestWaitForInitPushSendExtensionFailure is the round-19 regression test for waitForInitPush's
// handling of a failed login.init active-pull send: previously, a SendExtension error here was
// only logged, and execution fell through unconditionally into the next blocking ReadEnvelope --
// so a local write failure (a plausible half-open-connection symptom, since a write error can
// surface fast while a peer that never actually closes the connection leaves the read blocking
// until the deadline) got silently downgraded into an ordinary silence-until-deadline timeout
// instead of the definite initErr!=nil connection-failure result Login() is built to fail-fast
// on. Forces this deterministically with writeFailConn rather than relying on a race: the read
// side is left as a genuinely-blocking, non-EOF net.Pipe (no peer close at all), so the *only*
// way this test's error can surface is via the send failure itself, and only a fix that returns
// immediately on that failure -- not one that just logs and keeps waiting -- can make this test
// pass promptly instead of timing out the full window.
//
// Round 31 fix: this test used to inject a plain errors.New(...) (not a net.Error) as writeErr,
// and its only net.Error-related assertion was "if errors.As(err, &netErr) && netErr.Timeout()"
// with no t.Fatalf requiring errors.As to succeed first -- so with sendStageError's wrap REMOVED,
// the raw errors.New(...) doesn't satisfy net.Error at all, errors.As fails, the && short-circuits
// false, and the whole Timeout() check is silently skipped rather than failing the test: the test
// passed identically whether or not the round-30 sendStageError wrap actually existed. Now injects
// fakeTimeoutNetError{} (a genuine net.Error with Timeout()==true, mirroring
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError's own technique above) and requires
// errors.As to succeed via t.Fatalf before asserting Timeout()==false -- a pattern that WOULD catch
// the wrap's removal, since removing it would leave the raw fakeTimeoutNetError's Timeout()==true
// value visible straight through errors.As.
func TestWaitForInitPushSendExtensionFailure(t *testing.T) {
	client, _ := newPipeGameConnPair(t) // server intentionally left idle: no reply, no close
	writeErr := fakeTimeoutNetError{msg: "simulated write failure (e.g. half-open connection)"}
	client.conn = &writeFailConn{Conn: client.conn, err: writeErr}

	const window = 200 * time.Millisecond
	start := time.Now()
	buildings, visitors, gotInit, err := waitForInitPush(client, window)
	elapsed := time.Since(start)

	if gotInit {
		t.Fatalf("expected gotInit=false, got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err == nil {
		t.Fatal("expected a non-nil error when the login.init active-pull send fails, got nil (indistinguishable from a plain timeout)")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap/equal the SendExtension failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error (via sendStageError's wrap)", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a send-stage failure must be distinguishable from a benign wait-stage timeout, even though the underlying write failure itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write)")
	}
	// The active pull only fires at the halfway point (window/2), so the earliest this can
	// possibly return is ~window/2, not immediately from start -- but it must return well before
	// the full window elapses, proving it didn't fall through into the blocking read-and-wait
	// path after logging the send failure.
	if elapsed > window*3/4 {
		t.Errorf("waitForInitPush took %v, want it to return promptly after the failed send rather than waiting out the full %v window", elapsed, window)
	}
}

// TestReadPacketGracefulCloseIsNonTimeoutNetError is the packet.go-level regression test for round
// 24's MAJOR finding: a peer's graceful close (a clean FIN, or the far end process simply exiting,
// with nothing sent) surfaces from ReadPacket's leading io.ReadFull(r, hb[:]) header read as bare
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
	_, err := ReadPacket(bytes.NewReader(nil))
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
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestReadPacketMidFrameCloseIsNonTimeoutNetError is the readFrameField-path sibling of
// TestReadPacketGracefulCloseIsNonTimeoutNetError above: a close partway through a frame (here,
// right after the header byte but before the length field it promised) rather than cleanly between
// packets. packet_oom_test.go's TestReadPacketMidFrameTruncationIsNotClassifiedAsCleanEOF already
// proves this shape correctly satisfies errors.Is(err, io.ErrUnexpectedEOF) (not io.EOF); this test
// proves it ALSO now satisfies net.Error with Timeout()==false, confirming the fix was applied at
// readFrameField itself and not just the one header-read call the audit specifically called out.
func TestReadPacketMidFrameCloseIsNonTimeoutNetError(t *testing.T) {
	// A lone header byte declaring hdrBigSized (so ReadPacket expects a 4-byte length field to
	// follow) with nothing after it.
	_, err := ReadPacket(bytes.NewReader([]byte{hdrBinary | hdrEncrypted | hdrBigSized}))
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
// loop's sendAndWait/waitForCmd/waitFor chain sits on top of, not just ReadPacket in isolation.
func TestReadEnvelopeGracefulCloseIsNonTimeoutNetError(t *testing.T) {
	conn := eofConn{}
	client := &GameConn{conn: conn, reader: bufio.NewReaderSize(conn, 4096)}

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
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
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
	err := deadlineExceededError{}

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
