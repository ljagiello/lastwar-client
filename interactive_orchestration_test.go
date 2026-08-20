package main

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// handleInteractiveLine's pure JSON-decoding piece (putJSONValue) is covered directly in
// interactive_test.go. This file covers handleInteractiveLine itself over a net.Pipe-backed
// GameConn (newPipeGameConnPair, conn_wait_test.go), the same pattern conn_wait_test.go and
// visitors_orchestration_test.go use for their sendAndWait/GreetVisitors coverage.

// TestHandleInteractiveLineAbortsOnUnsupportedValue checks the "abort on unsupported JSON value"
// path: when putJSONValue rejects a key's value (here, a JSON array -- decoded by
// encoding/json into []any, which putJSONValue has no case for), handleInteractiveLine must log
// and return without ever calling conn.SendExtension. Proving that requires more than "the call
// returned" -- net.Pipe's Read/Write rendezvous synchronously with no buffering, so if
// handleInteractiveLine sent anything here it would block forever with nobody on the other end
// reading, and this test would hang instead of failing cleanly. Running it in a goroutine with a
// bounded wait turns that hang into a clean timeout failure.
func TestHandleInteractiveLineAbortsOnUnsupportedValue(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `test.cmd {"bad":[1,2,3]}`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly -- it likely tried to send despite the unsupported param value")
	}
}

// TestHandleInteractiveLineSendsParsedCommand checks the normal path: a well-formed
// "cmd.name {json}" line is parsed into an SFSObject and sent as an Extension call with that exact
// cmd, and a matching reply within the window is read back without handleInteractiveLine erroring
// out (it just logs -- there's no return value to assert on, so we assert on what the fake server
// on the other end of the pipe actually received).
func TestHandleInteractiveLineSendsParsedCommand(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type got struct {
		cmd    string
		params *SFSObject
	}
	gotCh := make(chan got, 1)
	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		gotCh <- got{msg.Cmd, msg.Params}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `some.command {"key":"value","num":42}`)
	}()

	var g got
	select {
	case g = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received the Extension call")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return after receiving its reply")
	}

	if g.cmd != "some.command" {
		t.Errorf("Cmd = %q, want some.command", g.cmd)
	}
	if got := g.params.GetString("key"); got != "value" {
		t.Errorf(`params["key"] = %q, want "value"`, got)
	}
	if got := g.params.GetLong("num"); got != 42 {
		t.Errorf(`params["num"] = %d, want 42`, got)
	}
}

// TestHandleInteractiveLineRedactsCredentialFields is interactive.go's sibling of
// TestLoginEmailVerificationPushErrorDoesNotLeakLoginKey (login_integration_test.go):
// RunInteractive's whole purpose is trying arbitrary commands -- including, plausibly,
// account/login-family ones -- against a real authenticated session, so its "sending
// command"/"received response" log lines must never dump a live credential in cleartext, on
// either the outgoing params or the incoming response.
func TestHandleInteractiveLineRedactsCredentialFields(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"
	const secretPw = "sensitive-secret-outgoing-pw-must-not-leak-abcdef"

	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		resp := NewSFSObject()
		resp.PutUtfString("loginKey", secretLoginKey)
		resp.PutUtfString("gameUid", "g1")
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `account.login.new {"pw":"`+secretPw+`"}`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly")
	}

	logged := buf.String()
	if strings.Contains(logged, secretLoginKey) {
		t.Errorf("interactive log output leaks the response loginKey in cleartext:\n%s", logged)
	}
	if strings.Contains(logged, secretPw) {
		t.Errorf("interactive log output leaks the outgoing pw in cleartext:\n%s", logged)
	}
}

// TestHandleInteractiveLineDoesNotLeakRawParamsOnJSONParseError covers the "bad JSON params"
// error path (handleInteractiveLine's json.Decoder.Decode failure branch), a case
// TestHandleInteractiveLineRedactsCredentialFields above doesn't reach: that test's line is
// well-formed JSON, so it exercises only the StringRedacted() calls on the successfully-parsed
// params/response. An operator testing with a real captured credential who makes a JSON typo
// (missing closing brace/quote, as below) never gets that far -- the raw, un-parsed command text
// itself is what's at risk of being echoed into the log verbatim, and unlike the redacted-field
// case, no .String()/.StringRedacted() call is involved for credential_leak_lint_test.go to catch,
// so this has to be checked directly against the captured log output.
func TestHandleInteractiveLineDoesNotLeakRawParamsOnJSONParseError(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	const secretPw = "secret-value-must-not-leak"

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Deliberately malformed: missing the closing quote and brace.
		handleInteractiveLine(client, `account.login.new {"pw":"`+secretPw)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly on malformed JSON params")
	}

	logged := buf.String()
	if strings.Contains(logged, secretPw) {
		t.Errorf("interactive log output leaks the raw unparsed params (containing the secret) on a JSON parse error:\n%s", logged)
	}
	if !strings.Contains(logged, "bad JSON params") {
		t.Errorf("expected a \"bad JSON params\" log entry, got:\n%s", logged)
	}
}

// TestHandleInteractiveLineAbortsOnTrailingGarbageAfterJSON covers the "well-formed JSON value
// followed by leftover bytes" case: json.Decoder.Decode only consumes the first JSON value on the
// line and returns no error on its own just because text remains afterward, so
// handleInteractiveLine must check dec.More() itself and abort -- the same as the adjacent
// malformed-JSON branch covered by TestHandleInteractiveLineDoesNotLeakRawParamsOnJSONParseError
// above -- instead of silently discarding the trailing text and sending a command with only the
// first value's params. As in TestHandleInteractiveLineAbortsOnUnsupportedValue, running this
// over a net.Pipe-backed GameConn with nobody reading on the other end turns "did it still send
// despite the trailing garbage" into a clean timeout failure instead of a hang.
func TestHandleInteractiveLineAbortsOnTrailingGarbageAfterJSON(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Well-formed JSON object immediately followed by trailing garbage on the same line.
		handleInteractiveLine(client, `cmd.name {"uuid":123} some trailing garbage here`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly -- it likely tried to send despite trailing garbage after the JSON value")
	}

	logged := buf.String()
	if !strings.Contains(logged, "bad JSON params") {
		t.Errorf("expected a \"bad JSON params\" log entry for trailing garbage after a well-formed JSON value, got:\n%s", logged)
	}
}

// TestHandleInteractiveLineRejectsBareNullParams is the regression test for this round's fix: a
// bare top-level JSON null params body (e.g. "cmd.name null") decodes successfully into a nil
// map[string]any, with json.Decoder.Decode returning no error at all -- diverging from every OTHER
// malformed-body shape (a bare number/string/bool/array), which all correctly fail that same
// Decode call and hit the existing "bad JSON params" error branch
// (TestHandleInteractiveLineDoesNotLeakRawParamsOnJSONParseError above covers that error branch;
// TestPutJSONValue's own "unsupported nil is rejected" case, interactive_test.go, is a different
// thing entirely -- that's putJSONValue rejecting a nil VALUE for one key inside an otherwise valid
// object, not the top-level params body itself being bare null). Before this fix,
// handleInteractiveLine let this one shape slip through as if it were a legitimate empty params
// object, silently sending the command with no params and no diagnostic. As in
// TestHandleInteractiveLineAbortsOnUnsupportedValue, running this over a net.Pipe-backed GameConn
// with nobody reading on the other end turns "did it still send despite the bare null" into a
// clean timeout failure instead of a hang.
func TestHandleInteractiveLineRejectsBareNullParams(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `cmd.name null`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly -- it likely sent the command despite a bare JSON null params body")
	}

	logged := buf.String()
	if !strings.Contains(logged, "bad JSON params") {
		t.Errorf("expected a \"bad JSON params\" log entry for a bare JSON null params body, got:\n%s", logged)
	}
}

// TestHandleInteractiveLineAbortsOnMissingSpaceBeforeJSON covers a case that falls through both
// of the above malformed-input checks entirely: strings.Cut(line, " ") requires a literal space
// between the command name and its JSON params, so a line where that space was dropped (a JSON
// params blob glued directly onto the command name, e.g. "cmd.name{\"uuid\":123}") makes Cut
// report "not found" and hand back the *entire* line as cmd, with rest == "". Since rest is
// empty, the "if rest != \"\"" JSON-decode block never runs at all -- not even the dec.Decode
// error path TestHandleInteractiveLineDoesNotLeakRawParamsOnJSONParseError exercises -- so
// without the fix, handleInteractiveLine would silently send the whole mangled line as a literal
// SFS2X command name with empty params. As in TestHandleInteractiveLineAbortsOnUnsupportedValue,
// running this over a net.Pipe-backed GameConn with nobody reading on the other end turns "did it
// still send" into a clean timeout failure instead of a hang.
func TestHandleInteractiveLineAbortsOnMissingSpaceBeforeJSON(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// No space at all between the command name and the JSON params.
		handleInteractiveLine(client, `cmd.name{"uuid":123}`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly -- it likely sent the mangled line as a literal command despite the missing space")
	}

	logged := buf.String()
	if !strings.Contains(logged, "bad JSON params") {
		t.Errorf("expected a \"bad JSON params\" log entry for a missing space before JSON params, got:\n%s", logged)
	}
	if strings.Contains(logged, `cmd.name{"uuid":123}`) {
		t.Errorf("interactive log output leaks the raw glued command+JSON line verbatim:\n%s", logged)
	}
}

// TestHandleInteractiveLineSendsBareCommandWithNoSpace is the flip side of
// TestHandleInteractiveLineAbortsOnMissingSpaceBeforeJSON: a line that's just a command name with
// no JSON params and, consequently, no space at all (e.g. "some.command") must still be sent
// as-is, unaffected by the missing-space check above -- that check is scoped to lines containing
// '{', per this tool's own documented flat-scalar-only command format where bare commands with
// no params are legitimate and don't need a trailing space to be valid.
func TestHandleInteractiveLineSendsBareCommandWithNoSpace(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type got struct {
		cmd    string
		params *SFSObject
	}
	gotCh := make(chan got, 1)
	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		gotCh <- got{msg.Cmd, msg.Params}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `some.command`)
	}()

	var g got
	select {
	case g = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received the Extension call for a bare, space-less command")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return after receiving its reply")
	}

	if g.cmd != "some.command" {
		t.Errorf("Cmd = %q, want some.command", g.cmd)
	}
	if n := len(g.params.keys); n != 0 {
		t.Errorf("params has %d entries, want 0 for a bare command with no JSON", n)
	}
}

// TestHandleInteractiveLineWaitForCmdTimeoutDoesNotExit is this round's regression test for
// handleInteractiveLine's waitForCmd error handling: the exact same net.Error/Timeout()
// distinction round 21 applied at 6+ other call sites (buildings.go, mail.go, visitors.go,
// alliance.go) -- and that's already honored two lines up by SendExtension's own
// unconditional-fatal treatment on send failure -- was simply absent from this waitForCmd site.
// A Timeout()==true net.Error is waitForCmd's ordinary "no matching response within 8s" outcome
// (conn_wait_test.go's TestWaitForTimeout confirms sendAndWait/waitForCmd's plain timeout is
// itself a net.Error with Timeout()==true) -- routine and expected, not evidence the connection
// died -- so handleInteractiveLine must keep its original log-and-return behavior here, NOT
// os.Exit.
//
// It reuses fakeNetErrConn/fakeNetError (buildings_orchestration_test.go, same package) with
// timeout: true instead of interactive.go's real 8-second wait or a live net.Pipe peer: every
// Read on the fake connection fails immediately with a Timeout()==true net.Error, so this test
// drives the exact same error shape waitForCmd's real deadline-expiry produces, but fast and
// deterministically. Writes still succeed (fakeNetErrConn's default behavior), so
// conn.SendExtension above this in handleInteractiveLine still succeeds and this test actually
// reaches the waitForCmd failure path rather than short-circuiting on the send.
//
// Mutation check: a fix that treats every waitForCmd error identically as fatal (dropping the
// Timeout() check entirely) would make this test hang until its own 2s watchdog fires, since
// handleInteractiveLine would call os.Exit(1) and kill the whole test binary before `done` could
// ever close.
func TestHandleInteractiveLineWaitForCmdTimeoutDoesNotExit(t *testing.T) {
	fake := &fakeNetErrConn{timeout: true}
	conn := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(conn, `some.command`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly on a routine (Timeout()==true) waitForCmd failure -- did it call os.Exit instead of returning?")
	}

	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (the SendExtension call, which must still have succeeded before the waitForCmd timeout)", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "no matching response within 8s") {
		t.Errorf("expected the routine \"no matching response within 8s\" log line for a Timeout()==true waitForCmd error, got:\n%s", logged)
	}
	if strings.Contains(logged, "connection appears dead") {
		t.Errorf("a Timeout()==true waitForCmd error must not be logged as a dead connection:\n%s", logged)
	}
}

// TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits is the fatal-path counterpart to
// TestHandleInteractiveLineWaitForCmdTimeoutDoesNotExit above: a net.Error from waitForCmd whose
// Timeout() is false means the underlying game connection is actually gone (connection reset,
// broken pipe, etc.), not an ordinary per-call timeout -- so handleInteractiveLine must treat it
// exactly like the adjacent SendExtension failure two lines up and os.Exit(1), since
// -interactive's whole reason for existing is interacting with a live connection.
//
// handleInteractiveLine calls os.Exit(1) directly on this path, so (like
// TestRunCrossServerTestExitsWhenIPEmpty in main_crossserver_test.go and
// TestLoadEffectiveConfigExitsOnDefaultPathReadFailure in config_test.go) it can't be driven to
// completion in-process without also killing this test binary. This uses the same
// re-exec-the-test-binary-as-a-subprocess idiom: LASTWAR_TEST_HELPER_PROCESS=1 gates a branch
// that actually calls handleInteractiveLine and lets it exit, while the outer test spawns that as
// a child process and asserts on its exit code AND stderr message.
//
// It reuses fakeNetErrConn/fakeNetError with the default timeout: false (a stand-in for
// connection reset/broken pipe/DNS failure/TLS error, not an ordinary per-call timeout), so
// waitForCmd's read fails immediately with a genuine, non-timeout net.Error.
func TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		fake := &fakeNetErrConn{}
		conn := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}
		handleInteractiveLine(conn, `some.command`)
		// Only reached if handleInteractiveLine fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	const wantMsg = "connection appears dead"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the same phrasing used by the adjacent SendExtension-failure fatal exit)", stderr.String(), wantMsg)
	}
}

// TestHandleInteractiveLineWaitForCmdRealGracefulCloseExits is the round-25 regression-safety-gap
// closer for TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits above -- see that test's
// sibling, TestCollectAllAbortsRemainingActionsOnRealGracefulClose
// (buildings_orchestration_test.go), for the full rationale: fakeNetErrConn injects an
// already-a-net.Error fake, never exercising the real bare-io.EOF-through-ReadPacket-through-
// deadConnError conversion path (packet.go's wrapIfClosed/deadConnError, round 24).
//
// realEOFConn (buildings_orchestration_test.go, same package) drives a real io.EOF through an actual
// GameConn instead, mirroring conn_wait_test.go's TestReadEnvelopeGracefulCloseIsNonTimeoutNetError,
// so waitForCmd's read fails via the real deadConnError conversion -- what a real peer's graceful TCP
// close actually produces -- not a synthetic net.Error stand-in. Same subprocess re-exec idiom as
// TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits above: handleInteractiveLine calls
// os.Exit(1) directly on this path, so it can't be driven to completion in-process without also
// killing this test binary.
func TestHandleInteractiveLineWaitForCmdRealGracefulCloseExits(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		fake := &realEOFConn{}
		conn := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}
		handleInteractiveLine(conn, `some.command`)
		// Only reached if handleInteractiveLine fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestHandleInteractiveLineWaitForCmdRealGracefulCloseExits$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	const wantMsg = "connection appears dead"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the same phrasing used by the adjacent SendExtension-failure fatal exit)", stderr.String(), wantMsg)
	}
}

// TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorDoesNotExitDuringShutdown is the round-40
// regression test for the MINOR finding that RunInteractive's signal-handling goroutine (Ctrl-C/
// SIGTERM) raced handleInteractiveLine's own os.Exit(1) call: conn.Close() unblocks a pending
// waitForCmd with the identical non-timeout net.Error shape
// TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorExits above proves is normally fatal --
// which used to make handleInteractiveLine call os.Exit(1) with a misleading "connection appears
// dead" log, racing (with no happens-before ordering) the signal goroutine's own os.Exit(0). Now
// that path checks interactiveShuttingDown first: with it set (simulating "the signal handler
// already began shutdown"), the exact same fakeNetErrConn failure that exits with code 1 above
// must instead return quietly -- proven here by running IN-PROCESS (unlike the subprocess re-exec
// idiom the exit-triggering tests above need) and simply completing without os.Exit ever firing.
func TestHandleInteractiveLineWaitForCmdNonTimeoutNetErrorDoesNotExitDuringShutdown(t *testing.T) {
	interactiveShuttingDown.Store(true)
	t.Cleanup(func() { interactiveShuttingDown.Store(false) })

	fake := &fakeNetErrConn{}
	conn := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(conn, `some.command`)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handleInteractiveLine never returned -- it should return quietly during a shutdown instead of exiting or hanging")
	}
}

// TestStatControlPipeWithRetryRecoversFromTransientMissingFile is this round's regression test for
// the MAJOR finding that RunInteractive's per-iteration os.Stat call on the control FIFO used to
// treat ANY failure as immediately fatal (os.Exit(1)), even though the failure at the instant of
// one particular stat call can be purely transient and self-correcting (e.g. the pipe being
// momentarily replaced/recreated by another process) rather than evidence the FIFO is permanently
// gone. Like TestHandleInteractiveLineWaitForCmdTimeoutDoesNotExit above, this drives the retry
// helper directly (rather than the os.Exit(1)-terminated RunInteractive loop itself) so a failed
// fix reports as a clean test failure instead of killing the test binary.
//
// The FIFO at path deliberately does not exist yet when statControlPipeWithRetry is first called --
// a background goroutine creates it only after controlPipeRetryDelay, well inside the function's
// controlPipeRetries budget -- so the very first stat attempt is guaranteed to see ENOENT. A bare
// os.Stat call (RunInteractive's pre-fix behavior) would have given up right there; this asserts
// the retry helper instead keeps trying and succeeds once the FIFO actually appears.
func TestStatControlPipeWithRetryRecoversFromTransientMissingFile(t *testing.T) {
	path := t.TempDir() + "/control.pipe"

	go func() {
		time.Sleep(controlPipeRetryDelay)
		_ = syscall.Mkfifo(path, 0o600)
	}()

	fi, err := statControlPipeWithRetry(path)
	if err != nil {
		t.Fatalf("statControlPipeWithRetry() error = %v, want nil once the FIFO appears within the retry budget", err)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		t.Errorf("statControlPipeWithRetry() returned FileInfo for %q that isn't a FIFO, want the real FIFO's info", path)
	}
}

// TestOpenControlPipeWithRetryRecoversFromTransientMissingFile is openControlPipeWithRetry's
// sibling of TestStatControlPipeWithRetryRecoversFromTransientMissingFile above, for the identical
// reason (see that test's doc comment) applied to RunInteractive's os.Open call instead of its
// os.Stat call.
//
// This creates a plain regular file rather than a real FIFO: openControlPipeWithRetry itself only
// wraps os.Open and retries on failure -- it has no FIFO-specific behavior of its own (the
// FIFO-mode-bit check happens separately, in RunInteractive, via statControlPipeWithRetry, before
// openControlPipeWithRetry is ever called) -- and a plain file sidesteps a real FIFO's read-open-
// blocks-until-a-writer-connects semantics, which would otherwise need a second synchronized
// goroutine and risk flakiness unrelated to what this test is actually checking: that a failed
// open attempt is retried instead of immediately giving up.
func TestOpenControlPipeWithRetryRecoversFromTransientMissingFile(t *testing.T) {
	path := t.TempDir() + "/control-file"

	go func() {
		time.Sleep(controlPipeRetryDelay)
		f, err := os.Create(path)
		if err == nil {
			f.Close()
		}
	}()

	f, err := openControlPipeWithRetry(path)
	if err != nil {
		t.Fatalf("openControlPipeWithRetry() error = %v, want nil once the file appears within the retry budget", err)
	}
	f.Close()
}

// TestStatControlPipeWithRetryGivesUpOnPersistentFailure is the boundedness counterpart to
// TestStatControlPipeWithRetryRecoversFromTransientMissingFile above: a path that never appears at
// all must still eventually report failure (not retry forever) so RunInteractive's existing fatal
// os.Exit(1) behavior for a genuinely-broken control pipe is preserved -- this fix is about not
// giving up on the very FIRST failure, not about softening the case where the control pipe really
// is gone for good. The elapsed-time bound (generous relative to controlPipeRetries *
// controlPipeRetryDelay) is what would catch a mutation that dropped the retry loop's upper bound
// entirely and made it retry forever instead of just being slow.
func TestStatControlPipeWithRetryGivesUpOnPersistentFailure(t *testing.T) {
	path := t.TempDir() + "/never-created"

	start := time.Now()
	_, err := statControlPipeWithRetry(path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("statControlPipeWithRetry() error = nil, want a non-nil error for a control pipe path that never appears")
	}
	if want := 3 * time.Second; elapsed > want {
		t.Errorf("statControlPipeWithRetry() took %v to give up, want well under %v (bounded, not retrying forever)", elapsed, want)
	}
}

// TestOpenControlPipeWithRetryGivesUpOnPersistentFailure is openControlPipeWithRetry's sibling of
// TestStatControlPipeWithRetryGivesUpOnPersistentFailure above, for the identical reason (see that
// test's doc comment): a path that never appears at all must still eventually report failure (not
// retry forever), and within a bounded amount of time -- this closes the coverage gap noted this
// round, where openControlPipeWithRetry (structurally identical to statControlPipeWithRetry) had a
// "recovers from a transient failure" test (TestOpenControlPipeWithRetryRecoversFromTransientMissingFile
// above) but no matching "gives up on a persistent one" boundary test.
func TestOpenControlPipeWithRetryGivesUpOnPersistentFailure(t *testing.T) {
	path := t.TempDir() + "/never-created"

	start := time.Now()
	_, err := openControlPipeWithRetry(path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("openControlPipeWithRetry() error = nil, want a non-nil error for a control pipe path that never appears")
	}
	if want := 3 * time.Second; elapsed > want {
		t.Errorf("openControlPipeWithRetry() took %v to give up, want well under %v (bounded, not retrying forever)", elapsed, want)
	}
}

// TestRunInteractivePersistentScanErrorGivesUpBounded is the regression test for this round's fix
// to RunInteractive's bufio.Scanner loop: before this fix, RunInteractive never called
// scanner.Buffer() at all, silently defaulting to bufio.MaxScanTokenSize (64KB) -- so any single
// control-pipe line over that size failed with bufio.ErrTooLong. And when scanner.Scan() ended via
// a genuine scanner.Err() != nil (of which ErrTooLong is one example, but not the only one), the
// pre-fix code just logged "control pipe scan error" and immediately looped back to the top
// (reopening the FIFO) with NO delay and NO attempt cap -- unlike the bounded
// statControlPipeWithRetry/openControlPipeWithRetry helpers this file already covers above. If the
// writer producing the bad input stayed connected and kept reproducing it, this could spin
// open-error-close indefinitely.
//
// This drives a real FIFO with a background writer goroutine that repeatedly opens it and writes a
// line larger than maxControlPipeLineSize with no '\n' at all -- guaranteeing a genuine, persistent
// bufio.ErrTooLong scan error on every single iteration, never a clean EOF (which would just be the
// FIFO's ordinary "writer closed" case, not a scan error at all). RunInteractive calls os.Exit(1)
// once its new consecutive-scan-error budget is exhausted, so (like this file's other
// os.Exit-reaching tests above) this uses the same re-exec-the-test-binary-as-a-subprocess idiom --
// with the elapsed wall-clock time as the actual proof this doesn't spin unboundedly: the exit
// code/log content alone wouldn't distinguish "gave up after a handful of bounded attempts" from
// "eventually gave up after spinning for an hour", only the bounded elapsed time does.
//
// Mutation-testing note: reverting the fix (dropping the consecutiveScanErrors cap and its
// controlPipeRetryDelay pause, back to "log and immediately loop with no bound") would make this
// test hang until cmd.Run() is killed by the test framework's own overall timeout, rather than
// exiting within this test's own elapsed-time bound -- a clear, non-silent failure either way.
func TestRunInteractivePersistentScanErrorGivesUpBounded(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		path := os.Getenv("LASTWAR_TEST_CONTROL_PIPE")
		client, _ := newPipeGameConnPair(t)

		go func() {
			// Comfortably over maxControlPipeLineSize, and never containing a '\n' -- the scanner
			// can never find a token boundary within its buffer budget, guaranteeing ErrTooLong
			// every time instead of ever producing a clean line or a clean EOF.
			oversized := bytes.Repeat([]byte("a"), maxControlPipeLineSize+4096)
			for {
				f, err := os.OpenFile(path, os.O_WRONLY, 0)
				if err != nil {
					return
				}
				_, _ = f.Write(oversized)
				f.Close()
			}
		}()

		RunInteractive(client, path)
		// RunInteractive only ever returns via os.Exit -- reached only if this fix regresses back
		// to spinning forever, in which case the outer cmd.Run() below never completes and this
		// test fails via the surrounding test framework's own timeout instead of a clean assertion.
		return
	}

	dir := t.TempDir()
	path := dir + "/control.pipe"
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("Mkfifo: %v", err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunInteractivePersistentScanErrorGivesUpBounded$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1", "LASTWAR_TEST_CONTROL_PIPE="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	if want := 10 * time.Second; elapsed > want {
		t.Errorf("subprocess took %v to give up on a persistent control pipe scan error, want well under %v (bounded, not spinning open-error-close forever)", elapsed, want)
	}

	log := stderr.String()
	if !strings.Contains(log, "control pipe scan error") {
		t.Errorf("subprocess stderr = %s\nwant repeated \"control pipe scan error\" log lines, proving this actually went through the scan-error path", log)
	}
	if !strings.Contains(log, "giving up") {
		t.Errorf("subprocess stderr = %s\nwant a \"giving up\" log line once the consecutive-scan-error budget is exhausted", log)
	}
}
