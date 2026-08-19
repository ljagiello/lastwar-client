package main

import (
	"bufio"
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
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
