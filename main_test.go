package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// TestOsExitAfterDeferredConnCloseCallsCloseExplicitlyFirst is the round-40 regression test for
// the MINOR finding that main() and runCrossServerTest() each register `defer conn.Close()` right
// after a successful login, then later call os.Exit(1) on four separate error paths reached AFTER
// that registration -- os.Exit does not run deferred functions, so the defer never ran on any of
// those four paths, a textbook os.Exit-skips-defers gap. This is currently harmless in effect only
// because GameConn.Close() does nothing a process exit doesn't already accomplish (no flush/
// notify/graceful-FIN logic), which is exactly why no black-box/subprocess test can observe a
// behavioral difference here today -- from a network peer's perspective, killing the process also
// closes the socket either way. Source-scanning is the honest way to pin down the fix (mirroring
// this codebase's own established convention, e.g. TestFetchBuildingsAggregateCeilingUsesDedicatedConstant/
// TestCrossServerFlagNamesMatchesDeclarations): every os.Exit(1) call site reached after `defer
// conn.Close()` must now be immediately preceded by an explicit conn.Close() call, so the fix
// stays correct even once/if Close() ever gains real cleanup logic of its own.
func TestOsExitAfterDeferredConnCloseCallsCloseExplicitlyFirst(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}

	re := regexp.MustCompile(`conn\.Close\(\)\s*\n\s*os\.Exit\(1\)`)
	matches := re.FindAll(src, -1)
	const want = 4 // main()'s two post-defer os.Exit(1) sites + runCrossServerTest()'s two
	if len(matches) != want {
		t.Errorf("found %d conn.Close()-immediately-before-os.Exit(1) sites in main.go, want %d -- every os.Exit(1) reached after `defer conn.Close()` registers must call conn.Close() explicitly first, since os.Exit skips deferred functions", len(matches), want)
	}
}

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"bogus", slog.LevelInfo}, // unrecognized -- falls back to info (with an slog.Warn)
		{"", slog.LevelInfo},      // the flag's own default -- falls back to info, no warning
	}
	for _, c := range cases {
		if got := parseLogLevel(c.in); got != c.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseLogLevelUnrecognizedValueWarnsViaSlogNotStderr is the round-31 regression test for the
// MAJOR finding that an unrecognized -log-level value used to bypass slog entirely and print a raw
// plain-text line directly to stderr via fmt.Fprintf -- breaking the all-JSON log stream invariant
// main()'s very first statement (installing a placeholder JSON handler before any flag is even
// declared) exists specifically to guarantee. Captures slog's output through a JSON handler (the
// same shape main() itself installs) and asserts the diagnostic reaches THAT sink, structured, with
// no direct stderr write racing it.
func TestParseLogLevelUnrecognizedValueWarnsViaSlogNotStderr(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	if got := parseLogLevel("bogus"); got != slog.LevelInfo {
		t.Errorf("parseLogLevel(%q) = %v, want %v", "bogus", got, slog.LevelInfo)
	}

	logged := buf.String()
	if !strings.Contains(logged, `"level":"WARN"`) {
		t.Errorf("expected a JSON-formatted WARN log line, got:\n%s", logged)
	}
	if !strings.Contains(logged, "bogus") {
		t.Errorf("expected the log line to name the offending value %q, got:\n%s", "bogus", logged)
	}
}

// TestMainFlagParseExitCodes is the regression test for the flag-parsing exit-code contract
// documented by the comment above fs.Parse near the top of main() (main.go): -h/-help exits 0,
// any other flag-parse error (unknown flag, bad value) exits 1, and both are kept distinct from
// exit code 2 (reserved for a confirmed server-side auth rejection -- see the ErrAuthRejected
// handling further down in main()). This contract is operationally load-bearing -- README's cron
// examples check $? to decide whether to alert -- but until this test, it had zero coverage:
// main_flags_test.go only exercises pure helper functions, never fs.Parse/os.Exit inside main()
// itself.
//
// main() reads os.Args and calls os.Exit directly, so (like TestRunCrossServerTestExitsWhenIPEmpty
// in main_crossserver_test.go) it can't be driven to completion in-process without also killing
// this test binary. Unlike that test, there's no already-extracted helper function taking a plain
// options struct here -- the flag-parsing logic lives inline in main() itself -- so instead of
// calling a helper, this reuses the same re-exec-the-test-binary-as-a-subprocess idiom but has the
// child overwrite os.Args to the exact argv main() would see for a real invocation before calling
// main() itself. Every case below exits either inside fs.Parse's own error branch, in the
// fs.NArg() check immediately after it succeeds (the stray positional argument case, round 19's
// Fix 1), or (the swallowed-flag-value case, round 25's Fix 1) inside the fs.Visit callback a few
// lines further down that populates visitedFlags -- either way, before any network/login/
// config-loading code runs, so this is safe to run with no fake servers or HOME override.
func TestMainFlagParseExitCodes(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		argv := []string{"lastwar-client"}
		if raw := os.Getenv("LASTWAR_TEST_MAIN_ARGS"); raw != "" {
			// "\x1f" (ASCII unit separator), not NUL: os/exec rejects any environment variable
			// whose value contains an embedded NUL byte (a security fix -- see Go issue #56284),
			// so a NUL-joined multi-arg value would make cmd.Run() below fail immediately with
			// "environment variable contains NUL" before the child process (and this branch) ever
			// runs at all. None of this table's flag values legitimately contain "\x1f" either,
			// so it's an equally safe delimiter that doesn't hit that restriction.
			argv = append(argv, strings.Split(raw, "\x1f")...)
		}
		os.Args = argv
		main()
		// main() always os.Exits before returning for every argv this test drives it with; only
		// reached if that stops being true, and the outer assertions will then see a clean
		// (non-error) subprocess exit and fail with a clear message instead of this silently
		// passing.
		return
	}

	cases := []struct {
		name             string
		args             []string
		wantCode         int
		wantStderrSubstr string // empty = no stderr content assertion, just the exit code
		wantJSON         bool   // true = also assert stderr is a single well-formed JSON log line (not raw plain text)
	}{
		{"long help flag exits 0", []string{"-help"}, 0, "", false},
		{"short help flag exits 0", []string{"-h"}, 0, "", false},
		// wantJSON: true is the round-33 regression assertion for both of these -- flag.FlagSet's
		// own built-in failf()-then-usage output used to print a raw plain-text line straight to
		// stderr from INSIDE fs.Parse, before main() ever saw the returned error, so no
		// error-handling branch in main() could have intercepted it even after round 32's fix.
		// fs.SetOutput(io.Discard) (main.go) now suppresses that, and the error's own message goes
		// through slog.Error instead.
		{"unrecognized flag exits 1", []string{"-this-flag-does-not-exist"}, 1, "", true},
		{"malformed flag value exits 1", []string{"-cs-port=not-a-number"}, 1, "", true},
		{
			// This is the regression case for this round's Fix 1: Go's flag package stops parsing
			// at the first non-'-'-prefixed token and silently stashes everything after it as
			// fs.Args(), with fs.Parse itself returning a nil error -- so before the fs.NArg()
			// check added in main() (right after fs.Parse succeeds), this exact invocation used to
			// exit 0 and proceed straight into a real guest-login run instead of catching what's
			// almost certainly a mistyped flag missing its leading dash (e.g. "collect" instead of
			// "-collect"). It's safe to drive through the real main() here, with no fake servers or
			// HOME override, because the new check exits before any network/login/config-loading
			// code runs -- same as every other case in this table.
			//
			// wantJSON: true is the round-32 regression assertion -- this diagnostic used to bypass
			// slog entirely via a bare fmt.Fprintf, and this same substring check would have passed
			// identically against either the old plain-text output or the new JSON one, so it alone
			// never actually caught the bug. Asserting the stderr line is well-formed JSON is what
			// would have caught it.
			"stray positional argument exits 1 with a clear error instead of silently launching a real run",
			[]string{"collect"}, 1, "unexpected argument(s)", true,
		},
		{
			// Regression case for this round's Fix 1 (the MAJOR finding): flag.FlagSet.Parse's
			// internal parseOne unconditionally consumes the very next token as a non-bool flag's
			// value, with no check for whether that token itself looks like a registered flag
			// name -- so "-email -collect" parses to email="-collect", collect=false (never
			// visited at all: its token was consumed as -email's value, not parsed as a flag),
			// with fs.Parse itself returning a nil error and fs.NArg()==0 (nothing left over for
			// the stray-positional-argument case above to catch). This is a realistic operator
			// mistake -- e.g. `-email "$EMAIL" -collect` with an unset/empty, unquoted $EMAIL
			// shell variable -- see detectSwallowedFlagValue's doc comment in main.go for the full
			// mechanism and why it matters (the swallowed value would otherwise flow straight into
			// login.go's outgoing verification-code request). Safe to drive through the real
			// main() here, with no fake servers or HOME override, since the new check exits
			// before any network/login/config-loading code runs -- same as every other case in
			// this table.
			"an unquoted/empty -email value swallowing the next flag's name exits 1 with a clear error naming both flags, instead of silently launching a run with a corrupted -email value",
			[]string{"-email", "-collect"}, 1,
			// Deliberately quote-free: slog's JSON handler backslash-escapes literal '"' characters
			// in the log line, so a substring straight out of the %q-formatted portion of the
			// message wouldn't match the raw escaped bytes actually written to stderr.
			"-email never got a real value of its own and instead swallowed -collect off the command line",
			true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainFlagParseExitCodes$")
			cmd.Env = append(os.Environ(),
				"LASTWAR_TEST_HELPER_PROCESS=1",
				"LASTWAR_TEST_MAIN_ARGS="+strings.Join(c.args, "\x1f"),
			)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			gotCode := 0
			if runErr != nil {
				exitErr, ok := runErr.(*exec.ExitError)
				if !ok {
					t.Fatalf("subprocess did not run/exit as expected: err=%v, stderr=%s", runErr, stderr.String())
				}
				gotCode = exitErr.ExitCode()
			}
			if gotCode != c.wantCode {
				t.Errorf("subprocess exit code = %d, want %d; stderr=%s", gotCode, c.wantCode, stderr.String())
			}
			if c.wantStderrSubstr != "" && !strings.Contains(stderr.String(), c.wantStderrSubstr) {
				t.Errorf("subprocess stderr = %s\nwant it to contain %q", stderr.String(), c.wantStderrSubstr)
			}
			// wantJSON: proves the diagnostic actually went through slog's JSON handler, not a bare
			// fmt.Fprintf to stderr -- see the stray-positional-argument case's own comment above for
			// why a plain substring check alone (matched by either plain-text or JSON output) would
			// not have caught the round-32 regression this specifically guards against.
			if c.wantJSON {
				line := strings.TrimSpace(stderr.String())
				var parsed map[string]any
				if err := json.Unmarshal([]byte(line), &parsed); err != nil {
					t.Errorf("subprocess stderr is not well-formed JSON (want a single slog JSON log line, not raw plain text): %v\nstderr=%s", err, stderr.String())
				} else if parsed["level"] != "ERROR" {
					t.Errorf(`subprocess stderr JSON "level" = %v, want "ERROR"; stderr=%s`, parsed["level"], stderr.String())
				}
			}
		})
	}
}

// TestMainVersionFlag is the round-50 regression test for printVersion (main.go) and its -version
// call site, which had zero test coverage: unlike every other main() exit path exercised by
// TestMainFlagParseExitCodes above, -version deliberately returns from main() normally (see its
// own doc comment on why it bypasses the ignored-flags warning machinery) rather than os.Exit-ing
// non-zero, and prints to stdout via a bare fmt.Printf/fmt.Println rather than through slog -- so
// it needs its own stdout-capturing subprocess harness instead of reusing
// TestMainFlagParseExitCodes' stderr-only one. Reuses the identical re-exec-the-test-binary-as-a-
// subprocess idiom (safe here for the same reason: -version's own doc comment guarantees it exits
// before any network/login/config-loading code runs).
func TestMainVersionFlag(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS_VERSION") == "1" {
		os.Args = []string{"lastwar-client", "-version"}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainVersionFlag$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS_VERSION=1")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("subprocess did not exit cleanly: err=%v, stdout=%s, stderr=%s", err, stdout.String(), stderr.String())
	}
	got := strings.TrimSpace(stdout.String())
	if !strings.HasPrefix(got, "lastwar-client") {
		t.Errorf("stdout = %q, want it to start with %q", got, "lastwar-client")
	}
	if stderr.String() != "" {
		t.Errorf("stderr = %q, want empty (-version must print only to stdout)", stderr.String())
	}
}

// TestMainDecodeStreamFlag is the round-50 regression test for runDecode (decode.go) and its
// -decode-stream call site, which had zero test coverage: decode_test.go exercises
// DecodeStreamFile directly and extensively, but nothing drives runDecode itself, so neither its
// empty-label-defaults-to-"stream" fallback nor its os.Exit(1)-on-failure branch was ever actually
// covered. Mirrors TestMainVersionFlag's stdout-capturing subprocess harness (runDecode, like
// printVersion, prints via bare fmt.Printf/Println rather than slog for its per-packet output,
// though its failure path does go through slog.Error).
func TestMainDecodeStreamFlag(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS_DECODE") == "1" {
		os.Args = []string{"lastwar-client", "-decode-stream", os.Getenv("LASTWAR_TEST_DECODE_PATH")}
		main()
		return
	}

	t.Run("empty label defaults to stream and exits 0", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "capture.bin")
		if err := os.WriteFile(path, mustEncodePacket(t, "field", "value"), 0600); err != nil {
			t.Fatal(err)
		}

		cmd := exec.Command(os.Args[0], "-test.run=^TestMainDecodeStreamFlag$")
		cmd.Env = append(os.Environ(),
			"LASTWAR_TEST_HELPER_PROCESS_DECODE=1",
			"LASTWAR_TEST_DECODE_PATH="+path,
		)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("subprocess did not exit cleanly: err=%v, stdout=%s, stderr=%s", err, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "[stream]") {
			t.Errorf("stdout = %q, want it to contain the default %q label prefix (no -decode-label was set)", stdout.String(), "[stream]")
		}
	})

	t.Run("invalid path exits 1", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMainDecodeStreamFlag$")
		cmd.Env = append(os.Environ(),
			"LASTWAR_TEST_HELPER_PROCESS_DECODE=1",
			"LASTWAR_TEST_DECODE_PATH="+filepath.Join(t.TempDir(), "does-not-exist.bin"),
		)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("subprocess did not run/exit as expected: err=%v, stderr=%s", runErr, stderr.String())
		}
		if exitErr.ExitCode() != 1 {
			t.Errorf("exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "decode failed") {
			t.Errorf("stderr = %q, want it to mention the decode failure", stderr.String())
		}
	})
}

// TestMainStrayPositionalArgumentDoesNotLeakContent is the round-37 regression test for the MAJOR
// finding that the stray-positional-argument diagnostic (main.go, right after fs.Parse succeeds)
// used to log the raw joined argument content via "args", strings.Join(fs.Args(), " ") --
// completely unconstrained content, unlike detectSwallowedFlagValue's own "value" field (which by
// construction can only ever equal a registered flag's name). A cron-wrapper script that drops a
// -cs-at/-cs-shumei/-cs-deviceid/-email flag NAME while still passing its VALUE would land that
// secret value directly in this log line, in cleartext. Drives a real main() invocation (like
// TestMainFlagParseExitCodes) with a secret-looking trailing positional argument and asserts it
// never appears in stderr, only a count/length.
func TestMainStrayPositionalArgumentDoesNotLeakContent(t *testing.T) {
	const secretLookingArg = "sk-live-totally-secret-token-abc123xyz789"

	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS_STRAY_ARG") == "1" {
		os.Args = []string{"lastwar-client", secretLookingArg}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainStrayPositionalArgumentDoesNotLeakContent$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS_STRAY_ARG=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	gotCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("subprocess did not run/exit as expected: err=%v, stderr=%s", runErr, stderr.String())
		}
		gotCode = exitErr.ExitCode()
	}
	if gotCode != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", gotCode, stderr.String())
	}

	out := stderr.String()
	if strings.Contains(out, secretLookingArg) {
		t.Errorf("subprocess stderr leaks the raw positional argument content, want only a count/length; stderr=%s", out)
	}
	if !strings.Contains(out, "unexpected argument(s)") {
		t.Errorf("subprocess stderr missing the expected diagnostic message; stderr=%s", out)
	}
	if !strings.Contains(out, `"count":1`) {
		t.Errorf("subprocess stderr missing a count field for the stray argument(s); stderr=%s", out)
	}
}

// TestMainConfigMergeExplicitlyEmptyFlagsWarnAndSkipConfigFallback is the round-34 regression test
// for the MAJOR finding that round 33's explicit-vs-config merge fix (mergeExplicitOrConfigString/
// mergeExplicitOrConfigPort, config.go) was applied only to -cs-ip/-cs-port/-cs-gameuid inside
// main()'s config-merge block -- -cs-zone/-cs-deviceid/-cs-shumei/-cs-at were left on the old bare
// applyOverride pattern, which cannot distinguish "flag never mentioned" from "flag explicitly
// passed as empty" and silently backfills from the session config either way, with zero
// diagnostic. For -cs-at specifically this also defeated DoCrossServerLogin's own fatal
// "no access token given" guard (crossserver.go) on the common plain-reconnect path.
//
// This doubles as the regression test for the sibling MAJOR finding that main()'s actual
// config-merge call site (as opposed to the extracted pure helper functions, already unit-tested
// in isolation by TestMergeExplicitOrConfigString/TestMergeExplicitOrConfigPort in config_test.go)
// had zero test coverage of its own: every existing main_crossserver_test.go test constructs a
// crossServerTestOpts struct directly, bypassing main()'s own config-loading and merge code
// entirely.
//
// Drives a real main() invocation (like TestMainFlagParseExitCodes) with a temp session config
// file on disk holding non-empty Zone/DeviceID/ShumeiBoxId/AccessToken, and -cs-zone/-cs-deviceid/
// -cs-shumei/-cs-at each explicitly passed as empty on the command line. -cs-port is also
// explicitly passed as 0 (already covered on its own by round 33, reused here purely as a
// deterministic, pre-dial exit point: runCrossServerTest's port<=0 check fires and os.Exit(1)s
// right after the unconditional CheckVersion call, before ever attempting a real TCP dial -- see
// TestRunCrossServerTestPortExplicitButInvalidWording for the same technique). A fake GSL server
// satisfies that CheckVersion call. Asserts every new explicitly-empty warning fired, and that
// -cs-gameuid (left non-empty here, not part of this round's fix) did NOT warn, proving the new
// checks are scoped correctly rather than firing indiscriminately.
func TestMainConfigMergeExplicitlyEmptyFlagsWarnAndSkipConfigFallback(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS_CONFIG_MERGE") == "1" {
		home := t.TempDir()
		t.Setenv("HOME", home)

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		cfgPath := filepath.Join(home, "session.json")
		cfgJSON, err := json.Marshal(SessionConfig{
			IP: "9.9.9.9", Port: 9999, Zone: "APS9999", GameUid: "cfg-gameuid",
			DeviceID: "cfg-deviceid", ShumeiBoxId: "cfg-shumei", AccessToken: "cfg-at",
			// IOSMode: true, with -cs-ios never passed below, proves the round-35 fix to the
			// -cs-ios merge (mergeExplicitOrConfigBool) actually takes effect end to end -- the
			// flag's own zero-value default is false, so without this merge working, iosMode
			// would stay false instead of picking up the config's true.
			IOSMode: true,
		})
		if err != nil {
			t.Fatalf("marshal session config: %v", err)
		}
		if err := os.WriteFile(cfgPath, cfgJSON, 0600); err != nil {
			t.Fatalf("write session config: %v", err)
		}

		os.Args = []string{
			"lastwar-client",
			"-config", cfgPath,
			"-cs-ip", "1.2.3.4",
			"-cs-port", "0",
			"-cs-zone", "",
			"-cs-deviceid", "",
			"-cs-shumei", "",
			"-cs-at", "",
			"-cs-gameuid", "explicit-gameuid",
		}
		main()
		// Only reached if main() fails to exit -- the outer assertions below will then see a clean
		// (non-error) subprocess exit and fail with a clear message instead of this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainConfigMergeExplicitlyEmptyFlagsWarnAndSkipConfigFallback$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS_CONFIG_MERGE=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	gotCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("subprocess did not run/exit as expected: err=%v, stderr=%s", runErr, stderr.String())
		}
		gotCode = exitErr.ExitCode()
	}
	if gotCode != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (the port<=0 pre-dial guard); stderr=%s", gotCode, stderr.String())
	}

	out := stderr.String()
	for _, want := range []string{
		"-cs-zone was explicitly given as empty",
		"-cs-deviceid was explicitly given as empty",
		"-cs-shumei was explicitly given as empty",
		"-cs-at was explicitly given as empty",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("subprocess stderr missing expected warning %q; stderr=%s", want, out)
		}
	}
	if !strings.Contains(out, "-cs-port was explicitly given as 0") {
		t.Errorf("subprocess stderr missing the expected \"-cs-port was explicitly given as 0\" warning (this run passes -cs-port 0 explicitly); stderr=%s", out)
	}
	if strings.Contains(out, "-cs-gameuid was explicitly given as empty") {
		t.Errorf("subprocess stderr unexpectedly warned about -cs-gameuid, which was passed non-empty in this test; stderr=%s", out)
	}
	if !strings.Contains(out, `"iosMode":true`) {
		t.Errorf("subprocess stderr missing iosMode=true -- the config's IOSMode:true should have taken effect since -cs-ios was never passed; stderr=%s", out)
	}
}

// TestMainConfigMergeCsIPExplicitlyEmptyWarns is the round-38 regression test for the MINOR
// finding that -cs-ip's own explicit-vs-config merge (main.go, mergeExplicitOrConfigString) had no
// end-to-end coverage at all -- TestMainConfigMergeExplicitlyEmptyFlagsWarnAndSkipConfigFallback
// above always passes -cs-ip as a real, non-empty value, so it can never observe -cs-ip's own
// explicit-empty branch (a non-empty flag value behaves identically whether routed through the
// old applyOverride pattern or the new mergeExplicitOrConfigString one -- confirmed via mutation
// testing: reverting -cs-ip's merge call site to the pre-round-33 pattern left every existing test
// passing unchanged). -cs-ip gates whether main() takes the cross-server branch at all
// (`if *csIP != "" || *csRt != ""`), so this also sets -cs-rt to stay on that branch -- the fake
// GSL server's empty-ServerList, no-access-token response then makes runCrossServerTest's own
// "refresh returned nothing usable" guard exit 2 quickly and deterministically, with no real
// network dial ever attempted.
func TestMainConfigMergeCsIPExplicitlyEmptyWarns(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS_CS_IP_EMPTY") == "1" {
		home := t.TempDir()
		t.Setenv("HOME", home)

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		cfgPath := filepath.Join(home, "session.json")
		cfgJSON, err := json.Marshal(SessionConfig{IP: "9.9.9.9", Port: 9999, Zone: "APS9999", GameUid: "cfg-gameuid"})
		if err != nil {
			t.Fatalf("marshal session config: %v", err)
		}
		if err := os.WriteFile(cfgPath, cfgJSON, 0600); err != nil {
			t.Fatalf("write session config: %v", err)
		}

		os.Args = []string{
			"lastwar-client",
			"-config", cfgPath,
			"-cs-ip", "",
			"-cs-rt", "some-refresh-token",
		}
		main()
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainConfigMergeCsIPExplicitlyEmptyWarns$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS_CS_IP_EMPTY=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	gotCode := 0
	if runErr != nil {
		exitErr, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("subprocess did not run/exit as expected: err=%v, stderr=%s", runErr, stderr.String())
		}
		gotCode = exitErr.ExitCode()
	}
	if gotCode != 2 {
		t.Errorf("subprocess exit code = %d, want 2 (the -cs-rt refresh-returned-nothing-usable guard); stderr=%s", gotCode, stderr.String())
	}

	out := stderr.String()
	if !strings.Contains(out, "-cs-ip was explicitly given as empty") {
		t.Errorf("subprocess stderr missing the expected \"-cs-ip was explicitly given as empty\" warning; stderr=%s", out)
	}
}

// TestDetectSwallowedFlagValue is the fast, deterministic unit test of the pure decision extracted
// from this round's Fix 1 (the MAJOR finding): detectSwallowedFlagValue in main.go. See its own
// doc comment there for the full mechanism this guards against -- Go's flag package parseOne
// unconditionally consuming the very next token as a non-bool flag's value, with no check for
// whether that token itself looks like a registered flag name -- and stringFlagSwallowGuardNames'
// doc comment for why the guard is scoped to specific string flags rather than applied blanket.
//
// TestMainFlagParseExitCodes above proves this is actually wired into main()'s own fs.Visit call
// site end to end; this test instead pins down the pure decision's edge cases directly, without
// paying for a subprocess re-exec per case.
func TestDetectSwallowedFlagValue(t *testing.T) {
	registered := map[string]bool{"collect": true, "no-config": true, "cs-ip": true, "version": true}

	cases := []struct {
		name          string
		flagName      string
		value         string
		wantSwallowed string
		wantOK        bool
	}{
		{
			"guarded flag (-email) whose value is exactly another registered flag's name -- the finding's demonstrated case",
			"email", "-collect", "collect", true,
		},
		{
			"double-dash value still matches after stripping both leading dashes",
			"email", "--collect", "collect", true,
		},
		{
			"guarded flag with a normal, non-dash-prefixed value -- never flagged",
			"email", "user@example.com", "", false,
		},
		{
			"guarded flag with a dash-prefixed value that doesn't match ANY registered flag -- not flagged (only an exact registered-flag-name match counts, per the finding's narrow scoping instruction)",
			"email", "-bogus", "", false,
		},
		{
			"guarded flag whose value is a bare single dash -- not flagged (stripping leaves nothing to match against a flag name)",
			"email", "-", "", false,
		},
		{
			"guarded flag whose value is all dashes and nothing else -- same as the single-dash case, not flagged",
			"email", "--", "", false,
		},
		{
			"a flag NOT in the guarded set, even with a value that exactly matches a registered flag name, is left alone -- e.g. an fs.Int flag like -cs-port, whose value could legitimately be a negative number, and which doesn't need this guard anyway (a swallowed flag name there already fails fs.Parse's own int conversion)",
			"cs-port", "-collect", "", false,
		},
		{
			"another guarded flag (-cs-ip) whose value swallowed a different registered flag's name",
			"cs-ip", "-no-config", "no-config", true,
		},
		{
			"a value that happens to equal the flag's OWN name is still detected (e.g. a doubled '-cs-ip -cs-ip' typo) -- there's nothing special-cased about self-matches",
			"cs-ip", "-cs-ip", "cs-ip", true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotSwallowed, gotOK := detectSwallowedFlagValue(c.flagName, c.value, registered)
			if gotOK != c.wantOK || gotSwallowed != c.wantSwallowed {
				t.Errorf("detectSwallowedFlagValue(%q, %q, %v) = (%q, %v), want (%q, %v)",
					c.flagName, c.value, registered, gotSwallowed, gotOK, c.wantSwallowed, c.wantOK)
			}
		})
	}
}

// TestRunCrossServerTestPortExplicitButInvalidWording is the regression test for this round's
// Fix 2: runCrossServerTest's "port <= 0" pre-flight guard (main.go, right below the
// firstHost(ip) == "" guard) used to log the exact same "no port given" message whether -cs-port
// was never passed at all OR was actually typed with an invalid (<=0) value (e.g. a typo'd
// negative number) -- indistinguishable wording for two very different operator mistakes, despite
// o.portExplicit (crossServerTestOpts' own visitedFlags-derived field, populated in main() via the
// same fs.Visit mechanism already used elsewhere in this file -- e.g. the neighboring
// "ignoring -cs-at" / serverListOverrideFlags call sites -- to make exactly this kind of
// distinction) already being in scope.
//
// Mirrors main_crossserver_test.go's TestRunCrossServerTestExitsWhenPortNotGiven re-exec-subprocess
// pattern (runCrossServerTest calls os.Exit(1) directly on this path, so it can't be driven to
// completion in-process without also killing this test binary), but sets portExplicit: true
// alongside the invalid port value -- something that sibling test deliberately never does (it
// leaves portExplicit at its zero value, false, for both its port=0 and port=-1 cases, so it
// keeps asserting the pre-existing "no port given" wording for the genuinely-never-given case).
// This test is what actually exercises the new, distinct wording this round adds for the
// "typed but invalid" case, and confirms the old wording does NOT also appear.
func TestRunCrossServerTestPortExplicitButInvalidWording(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		// ip is a valid, non-empty value -- this test targets the port-invalid wording
		// specifically, not the ip check (TestRunCrossServerTestExitsWhenIPEmpty already covers
		// that one). No -cs-rt is set, so this never reaches the GSL-refresh block at all; the
		// fake GSL server above only exists to satisfy the unconditional CheckVersion call that
		// happens before the ip/port checks are reached.
		runCrossServerTest(crossServerTestOpts{
			ip:           "1.2.3.4",
			port:         -1,
			portExplicit: true,
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestPortExplicitButInvalidWording$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
	}

	log := stderr.String()
	const wantMsg = "invalid -cs-port value (must be positive)"
	if !strings.Contains(log, wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the new wording for an explicitly-typed but invalid port, distinct from the genuinely-never-given case)", log, wantMsg)
	}
	const wantPortField = "port=-1"
	if !strings.Contains(log, wantPortField) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q -- the invalid port value is now logged as a structured field, not baked into the message text", log, wantPortField)
	}
	const dontWantMsg = "no port given"
	if strings.Contains(log, dontWantMsg) {
		t.Errorf("subprocess stderr = %s\nmust NOT contain %q -- a port that was explicitly typed but invalid is a different mistake than one that was never given at all, and must be worded differently", log, dontWantMsg)
	}
}

// mainCollectInteractiveFakeGameServer is the fake game-server handler for
// TestMainCollectInteractiveCallSiteReachesRunInteractiveDespiteBusinessLogicError below: it
// answers the base zone Login (mirroring login_integration_test.go's fakeInitPushServer, but with
// one non-collectible building in the init push's building_new field -- bId 0 never matches
// collectCmdFor's switch in buildings.go -- so main() sees a non-empty buildings list and skips
// the extra push.init.build/FetchBuildings round trip entirely), then answers CollectAll's fixed
// 8-sub-action request sequence (9 requests total: idle x2, mail-list x1, help-all x1, gifts x2,
// tech-refresh x1, vip x2 -- visitors is empty so GreetVisitors sends nothing, and mail/alliance
// tech tree responses are left empty so ClaimAllMail/DonateRecommendedAllianceTech each stop after
// their own first request). Mirrors buildings_orchestration_test.go's
// TestCollectAllAggregatesErrorsWithoutShortCircuiting response table exactly, including its one
// injected business-logic (non-net.Error) failure on al.help.all (errorCode 999999) -- so
// CollectAll returns a non-nil error without the underlying connection ever being anything but
// healthy, which is exactly the case shouldAbortBeforeInteractive (round 24) and this test (round
// 25) care about. An earlier version of this handler injected the failure on
// vip.add.login.score's real, live-documented "already claimed today" errorCode (120289) instead
// -- that turned out to be the wrong choice here: conn.go's benignErrorCodes list means
// sendAndWait treats 120289 as an expected no-op and returns a nil error for it, so CollectAll
// ended up entirely error-free and never exercised the non-nil-error path at all. 999999 is
// deliberately not a real, documented error code, matching the sibling test's own choice,
// specifically so it can't accidentally collide with a benign one.
func mainCollectInteractiveFakeGameServer() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		loginResp := NewSFSObject()
		loginResp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, loginResp); err != nil {
			return
		}
		initParams := NewSFSObject()
		buildingArr := NewSFSArray()
		buildingArr.AddSFSObject(newTestBuildingSFS(1, 0, 1)) // bId=0: present, but not collectible
		initParams.PutSFSArray("building_new", buildingArr)
		if err := server.SendExtension("init", initParams); err != nil {
			return
		}

		const wantRequests = 9
		for i := 0; i < wantRequests; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			resp := NewSFSObject()
			replyCmd := msg.Cmd
			switch msg.Cmd {
			case "chat.get.system.mails":
				// ListMail waits under a distinct push cmd, not an echo of the request -- left
				// with no "msg"/"more" fields, it reads as zero mail found, one page, done.
				replyCmd = "push.chat.get.system.mails"
			case "al.help.all":
				// The one injected business-logic failure -- see this function's doc comment.
				// 999999 is not a real, documented errorCode (deliberately, so it can't collide
				// with an entry in conn.go's benignErrorCodes and get silently swallowed the way
				// vip.add.login.score's real 120289 code would).
				resp.PutUtfString("errorCode", "999999")
			case "science.data.refresh":
				// No "allianceScience" field: DonateRecommendedAllianceTech reads that as "no
				// tech tree data" and returns nil without a second al.science.donate call.
			default: // lw.pve.idle.reward, alliance.reward.allreceive, vip.add.login.score, vip.get.every.day.reward
				resp.PutBool("success", true)
			}
			_ = server.SendExtension(replyCmd, resp)
		}
	}
}

// TestMainCollectInteractiveCallSiteReachesRunInteractiveDespiteBusinessLogicError is the
// best-effort, round-25 end-to-end regression test for shouldAbortBeforeInteractive's coverage
// gap identified this round: round 24 added thorough unit-test coverage for
// shouldAbortBeforeInteractive itself, plus end-to-end coverage through runCrossServerTest's own
// call site (main_crossserver_test.go), but main()'s OWN -collect+-interactive call site --
// reached only after a REAL Login() call, with no dependency-injection seam comparable to
// runCrossServerTest's crossServerTestOpts -- had no end-to-end test of its own. This drives a
// full guest login through a fake GSL server and a fake game server (mirroring
// login_integration_test.go's own pattern for exercising Login() end to end), with -collect
// hitting one injected business-logic (non-net.Error) failure, and confirms main() does NOT
// os.Exit(1) on that -- the pre-round-24 behavior at this exact call site -- but instead proceeds
// into RunInteractive, proven by RunInteractive's own "interactive mode: reading commands"
// startup log.
//
// -interactive is deliberately pointed at a path that can never become a real FIFO:
// RunInteractive logs its startup lines unconditionally, before ever validating/opening the
// control pipe (see interactive.go), so a bogus path still proves this call site was reached, and
// then fails fast and deterministically on its own os.Stat check moments later -- letting this
// test use a plain cmd.Run()-and-check-exit-code shape instead of needing to background the
// subprocess, poll its stderr for a "now blocked" signal, and kill it (RunInteractive otherwise
// blocks forever, same as every other caller of it in this codebase).
func TestMainCollectInteractiveCallSiteReachesRunInteractiveDespiteBusinessLogicError(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		addr := startFakeGameServer(t, mainCollectInteractiveFakeGameServer())
		host, port := splitHostPortInt(t, addr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
			At:         &LoginToken{Token: "tok-1"},
		})
		useFakeGSLServer(t, gsl)

		os.Args = []string{"lastwar-client", "-collect", "-interactive", "/nonexistent/lastwar-test-control-pipe"}
		main()
		// Only reached if main() fails to exit -- the outer assertions below will then see a
		// clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainCollectInteractiveCallSiteReachesRunInteractiveDespiteBusinessLogicError$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (RunInteractive's own os.Stat failure on the bogus control pipe path); stderr=%s", exitErr.ExitCode(), stderr.String())
	}

	log := stderr.String()
	if !strings.Contains(log, "interactive mode: reading commands") {
		t.Errorf("subprocess stderr = %s\nwant it to contain RunInteractive's startup log -- proof main()'s own -collect+-interactive call site actually reached RunInteractive instead of aborting on CollectAll's business-logic error", log)
	}
	if !strings.Contains(log, "collect run had failures") {
		t.Errorf("subprocess stderr = %s\nwant it to log CollectAll's aggregated failure (from the injected vip.add.login.score errorCode) -- otherwise this test isn't actually exercising the non-nil-error path shouldAbortBeforeInteractive exists for", log)
	}
	if !strings.Contains(log, "stat control pipe failed") {
		t.Errorf("subprocess stderr = %s\nwant RunInteractive's own bogus-control-pipe failure -- confirms the exit code 1 came from there, not from some earlier, different failure", log)
	}
}

// writeMalformedOversizedFrame writes a single malformed packet header directly on server's raw
// connection: a declared body length over maxFrameSize (packet.go's own "frame body too large"
// guard, mirroring packet_oom_test.go's TestReadPacketRejectsOversizedDeclaredLength). ReadPacket
// rejects this using only the header's length field, before ever attempting to read (let alone
// allocate) a length-sized body.
//
// Round-43 fix: the resulting client-side error is now wrapped in packet.go's net.Error-satisfying
// deadConnError, NOT a plain fmt.Errorf -- this guard fires after the length field is already
// consumed and returns without draining the declared body, so a peer that actually follows an
// oversized header with real trailing bytes (unlike this helper, which sends only the 5-byte
// header and nothing else) would leave the reader desynced. Since this helper's own trailing
// silence is what makes ReadPacket's guard trip on JUST the header, it remains useful for testing
// that the guard rejects using only the header fields (see the packet_oom_test.go tests above), but
// is NO LONGER suitable for a test that needs a genuinely non-fatal, non-net.Error decode failure
// -- use writeMalformedZlibBombFrame below for that instead.
//
// Takes no *testing.T deliberately, matching serveFakeGameServer's own established pattern
// (crossserver_test.go): the fake server handlers that call this run in a background goroutine
// that may still be executing after the test function itself has returned, and calling T methods
// from such a goroutine is unsafe.
func writeMalformedOversizedFrame(server *GameConn) {
	var hdr bytes.Buffer
	hdr.WriteByte(hdrBinary | hdrEncrypted | hdrBigSized)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], maxFrameSize+1)
	hdr.Write(lb[:])
	_, _ = server.conn.Write(hdr.Bytes())
}

// malformedZlibBombPacket lazily builds and caches writeMalformedZlibBombFrame's packet bytes --
// round-49 fix: encoding a 64MB+4KB all-zero plaintext (even though it compresses down to a tiny
// wire size) is genuinely CPU-heavy under -race (confirmed several seconds by
// mainZeroBuildingsFallbackFakeGameServer's own pre-existing doc comment below), and computing it
// freshly INSIDE a fake server's connection-handling goroutine -- as this file previously did --
// burns a chunk of the client's own read deadline before the frame is even sent, tightening race
// windows the caller's test timing wasn't written to tolerate. sync.Once computes it exactly once,
// on first use, so every caller after the first pays no compression cost at all -- eliminating the
// CPU-bound delay from ever landing inside a timing-critical connection window.
var (
	malformedZlibBombPacketOnce  sync.Once
	malformedZlibBombPacketBytes []byte
)

func malformedZlibBombPacket() []byte {
	malformedZlibBombPacketOnce.Do(func() {
		plain := bytes.Repeat([]byte{0}, maxFrameSize+4096)
		packet, err := EncodePacket(plain)
		if err != nil {
			return
		}
		malformedZlibBombPacketBytes = packet
	})
	return malformedZlibBombPacketBytes
}

// writeMalformedZlibBombFrame writes a single legitimately-framed, zlib-compressed packet whose
// declared (compressed) length is small but whose DECOMPRESSED output exceeds maxFrameSize --
// packet.go's "zlib inflated output exceeds" guard. Unlike writeMalformedOversizedFrame's guard,
// this one fires only AFTER readFrameField has already consumed the full declared (compressed)
// body from the reader, so the underlying byte stream stays synchronized afterward regardless of
// this error -- a genuine "plain decode error over an otherwise still-healthy connection". The
// resulting client-side error remains a plain, unwrapped fmt.Errorf, not packet.go's
// net.Error-satisfying deadConnError, so this is the round-43 replacement for
// writeMalformedOversizedFrame at any call site that specifically needs shouldAbortBeforeInteractive
// to take its non-fatal branch.
func writeMalformedZlibBombFrame(server *GameConn) {
	packet := malformedZlibBombPacket()
	if packet == nil {
		return
	}
	_, _ = server.conn.Write(packet)
}

// mainFetchBuildingsFailureFakeGameServer answers the base zone Login normally, then sends an
// `init` push with an empty building_new (0 buildings, satisfying Login()'s own waitForInitPush
// fast -- gotInit=true, buildings=nil -- instead of waiting out the full 45s initPushTimeout for a
// genuine silence timeout), which is what actually reaches main()'s zero-buildings FetchBuildings
// fallback call site. It then writes a single malformed zlib-bomb frame directly on the connection
// (writeMalformedZlibBombFrame -- round-43 swap from writeMalformedOversizedFrame, whose error is
// now fatal per packet.go's round-43 fix): FetchBuildings' fallback call reads this as its very
// first envelope and returns a plain, non-net.Error decode failure immediately, instead of burning
// the fallback's own 12s timeout.
func mainFetchBuildingsFailureFakeGameServer() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		loginResp := NewSFSObject()
		loginResp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, loginResp); err != nil {
			return
		}
		if err := server.SendExtension("init", NewSFSObject()); err != nil {
			return
		}
		writeMalformedZlibBombFrame(server)
		// Round-49 fix: buildings.go's FetchBuildings now survives a single malformed/
		// undecodable push instead of returning it as a fatal error (mirroring login.go's
		// waitForInitPush's own round-48 fix), so the malformed frame above is no longer
		// enough to make FetchBuildings' fallback call return at all -- and its own read
		// loop uses a fresh 12s deadline (this fallback isn't preceded by a real `init`
		// push here, that's the whole reason this fallback fires: Login()'s own
		// waitForInitPush already collected 0 buildings from the `init` push sent above).
		// Keep reading (and discarding) instead of blocking on just ONE more read: an
		// abandoned/unreferenced connection has been observed to produce a spurious EOF
		// instead of the clean per-read timeout FetchBuildings' benign "waited long enough"
		// completion path expects, and a SINGLE blocking read isn't enough to prevent that
		// either -- Login()'s own background heartbeat (conn.go's StartHeartbeat, started
		// once login succeeds) sends a PingPong roughly every 4s, which completes a single
		// blocked ReadEnvelope call here and lets this goroutine return anyway (the scenario
		// that made this flaky specifically under -race, whose slower scheduling pushes the
		// client's own completion past the 4s heartbeat mark often enough to matter). Looping
		// keeps discarding every heartbeat indefinitely so the connection stays genuinely
		// open until the 12s deadline elapses and FetchBuildings returns normally.
		for {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
		}
	}
}

// TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive is the round-26
// regression test for Fix 1's second call site: FetchBuildings' fallback call in main() itself
// (as opposed to runCrossServerTest's twin call site, covered separately by
// TestRunCrossServerTestFetchBuildingsFailureWithInteractiveReachesRunInteractive in
// main_crossserver_test.go) used to unconditionally os.Exit(1) on ANY FetchBuildings error, with
// zero reference to whether -interactive was requested -- the exact same bug class round 25's
// shouldAbortBeforeInteractive fix closed for CollectAll's two call sites, just at this sibling
// call site one function up that fix never touched.
//
// Mirrors TestMainCollectInteractiveCallSiteReachesRunInteractiveDespiteBusinessLogicError's own
// end-to-end shape (a full guest login through a fake GSL server and a fake game server, -interactive
// pointed at a path that can never become a real FIFO so RunInteractive's own startup log proves
// this call site was reached before it fails fast on its os.Stat check), but targets the sibling
// FetchBuildings call site instead of CollectAll's.
//
// Round-43 note: mainFetchBuildingsFailureFakeGameServer's fake server now triggers this via
// writeMalformedZlibBombFrame, not writeMalformedOversizedFrame -- packet.go's round-43 fix wraps
// the oversized-declared-length error in deadConnError (a genuine net.Error), which would now make
// shouldAbortBeforeInteractive abort unconditionally, defeating this test's whole premise. The
// zlib-bomb decode failure remains a plain, non-net.Error error over an otherwise-synchronized
// stream, so it's still the right trigger for exercising the non-fatal branch this test targets.
//
// Round-49 note: buildings.go's FetchBuildings now survives a single malformed/undecodable push
// instead of returning it as a fatal error (mirroring login.go's waitForInitPush's own round-48
// fix), so writeMalformedZlibBombFrame's error no longer propagates out of FetchBuildings' fallback
// call at all -- this test's assertions were updated to match: it now proves the malformed push is
// survived (a Warn logged, no fatal error) and interactive is reached because nothing failed at
// all, rather than proving a non-fatal error was tolerated. Runs the fallback's full 12s deadline
// (no further push arrives for FetchBuildings' own read loop to shortcut on), so this test is
// necessarily slow.
func TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		addr := startFakeGameServer(t, mainFetchBuildingsFailureFakeGameServer())
		host, port := splitHostPortInt(t, addr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
			At:         &LoginToken{Token: "tok-1"},
		})
		useFakeGSLServer(t, gsl)

		os.Args = []string{"lastwar-client", "-interactive", "/nonexistent/lastwar-test-control-pipe"}
		main()
		// Only reached if main() fails to exit -- the outer assertions below will then see a
		// clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (RunInteractive's own os.Stat failure on the bogus control pipe path); stderr=%s", exitErr.ExitCode(), stderr.String())
	}

	log := stderr.String()
	if strings.Contains(log, "fetch buildings failed") {
		t.Errorf("subprocess stderr = %s\nwant NO FetchBuildings fallback failure logged -- round 49's fix means a single malformed push is survived, not fatal, so the fallback should complete successfully once its 12s deadline elapses", log)
	}
	if !strings.Contains(log, "zlib inflated output exceeds") {
		t.Errorf("subprocess stderr = %s\nwant the malformed push's decode failure to still be logged (as a Warn, not a fatal error) -- otherwise this test isn't actually exercising the malformed-push-survival path", log)
	}
	if !strings.Contains(log, "interactive mode: reading commands") {
		t.Errorf("subprocess stderr = %s\nwant it to contain RunInteractive's startup log -- proof main()'s FetchBuildings fallback call site actually reached RunInteractive instead of unconditionally aborting on the fetch failure", log)
	}
	if !strings.Contains(log, "stat control pipe failed") {
		t.Errorf("subprocess stderr = %s\nwant RunInteractive's own bogus-control-pipe failure -- confirms the exit code 1 came from there, not from some earlier, different failure", log)
	}
}

// mainZeroBuildingsFallbackFakeGameServer answers Login normally, then sends an `init` push
// carrying a malformed/empty building_new (0 buildings) alongside a normal, non-empty
// visitor.list (one visitor, uid 777) -- both parsed from the very same init push (see
// ParseInitBuildings/ParseInitVisitors), so this is a real reachable state, not a contrived one.
// It then writes a single malformed zlib-bomb frame (writeMalformedZlibBombFrame -- round-43 swap
// from writeMalformedOversizedFrame, whose error is now fatal per packet.go's round-43 fix, which
// would abort before CollectAll ever ran): main()'s zero-buildings FetchBuildings fallback call
// reads this as its very first envelope and returns immediately with 0 buildings AND 0 visitors of
// its own -- the exact shape round 26's Fix 4 targets, where an unconditional visitors overwrite
// would silently discard the real, already-known visitor list obtained above. Finally it answers
// CollectAll's fixed request sequence generically
// (mirroring mainCollectInteractiveFakeGameServer's own pattern), recording the uid any
// "visitor.operate" request carries into gotVisitorUID so the test can confirm GreetVisitors
// actually ran against the ORIGINAL visitor list, not the fallback's empty one.
func mainZeroBuildingsFallbackFakeGameServer(gotVisitorUID *int64) func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		loginResp := NewSFSObject()
		loginResp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, loginResp); err != nil {
			return
		}

		v := NewSFSObject()
		v.PutLong("uid", 777)
		v.PutInt("eventId", 1)
		list := NewSFSArray()
		list.AddSFSObject(v)
		visitorObj := NewSFSObject()
		visitorObj.PutSFSArray("list", list)
		initParams := NewSFSObject()
		initParams.PutSFSObject("visitor", visitorObj) // building_new deliberately omitted -- 0 buildings
		if err := server.SendExtension("init", initParams); err != nil {
			return
		}

		writeMalformedZlibBombFrame(server)

		// CollectAll's 8 fixed sub-actions issue 9 requests total when buildings/visitors are both
		// empty (see mainCollectInteractiveFakeGameServer's own doc comment for the per-action
		// breakdown: idle x2, mail-list x1, help-all x1, gifts x2, tech-refresh x1, vip x2) -- plus
		// one more here for GreetVisitors' single visitor.operate call, since this test's whole
		// point is that the ORIGINAL non-empty visitors slice from Login() survives into CollectAll
		// despite the fallback FetchBuildings call above returning 0 visitors of its own.
		// Round-43 note: reads via readNextExtension (login_integration_test.go), not a bare
		// ReadEnvelope+AsExtension pair that treats any non-extension envelope as reason to give
		// up -- writeMalformedZlibBombFrame's compression step (the round-43 replacement for
		// writeMalformedOversizedFrame) is genuinely CPU-heavy (several seconds under -race),
		// long enough to span the client's own 4s heartbeat interval (Login() already started it
		// via StartHeartbeat). Without skipping non-extension envelopes here, a heartbeat
		// PingPong that lands on the wire during that stall gets misread as this loop's first
		// expected request, AsExtension() returns ok=false for it, and the handler gives up --
		// leaving the connection to eventually read as a genuine EOF/dead-connection failure to
		// the client's next real request instead of the benign push this actually was.
		const wantRequests = 10
		for i := 0; i < wantRequests; i++ {
			msg, err := readNextExtension(server)
			if err != nil {
				return
			}
			resp := NewSFSObject()
			replyCmd := msg.Cmd
			switch msg.Cmd {
			case "visitor.operate":
				*gotVisitorUID = msg.Params.GetLong("uid")
				resp.PutBool("success", true)
			case "chat.get.system.mails":
				// ListMail waits under a distinct push cmd, not an echo of the request.
				replyCmd = "push.chat.get.system.mails"
			case "science.data.refresh":
				// No "allianceScience" field: DonateRecommendedAllianceTech reads that as "no tech
				// tree data" and returns nil without a second al.science.donate call.
			default: // lw.pve.idle.reward, al.help.all, alliance.reward.allreceive, vip.*
				resp.PutBool("success", true)
			}
			_ = server.SendExtension(replyCmd, resp)
		}
	}
}

// TestMainZeroBuildingsFallbackPreservesNonEmptyVisitors is the round-26 regression test for Fix
// 4: the zero-buildings fallback re-fetch (main.go) used to unconditionally overwrite the already-
// obtained visitors slice too, even though the trigger condition only tested buildings' length.
// Both buildings and visitors come from the same Login()/waitForInitPush call, so an init push
// with a malformed/partial building_new (0 buildings) alongside a normal, non-empty visitor.list
// is a real reachable state -- and since the bootstrap init push fires once per session,
// FetchBuildings' own fallback call has no second init push to observe and (in this test) fails
// fast with a decode error, returning visitors=nil -- which, before this fix, silently clobbered
// the real, already-known visitors before CollectAll ever ran.
//
// This drives a full guest login (fake GSL + fake game server) with -collect and -interactive both
// set (so the fallback's own non-net.Error decode failure doesn't itself abort the run before
// CollectAll runs -- see TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive
// above, Fix 1's sibling regression test), and confirms the fake server actually received a
// "visitor.operate" request for uid 777 -- the ORIGINAL visitor from Login()'s own init-push
// parse, not the fallback's empty result -- proving CollectAll ran against the preserved slice.
func TestMainZeroBuildingsFallbackPreservesNonEmptyVisitors(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		var gotVisitorUID int64
		addr := startFakeGameServer(t, mainZeroBuildingsFallbackFakeGameServer(&gotVisitorUID))
		host, port := splitHostPortInt(t, addr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
			At:         &LoginToken{Token: "tok-1"},
		})
		useFakeGSLServer(t, gsl)

		os.Args = []string{"lastwar-client", "-collect", "-interactive", "/nonexistent/lastwar-test-control-pipe"}
		main()
		// main() always os.Exits before returning on this path (RunInteractive's own os.Stat
		// failure on the bogus control pipe) -- if gotVisitorUID were wrong, the assertion below
		// (running in the PARENT process, after this child exits) is what catches it; nothing
		// further needs to happen in this branch itself.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestMainZeroBuildingsFallbackPreservesNonEmptyVisitors$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (RunInteractive's own os.Stat failure on the bogus control pipe path); stderr=%s", exitErr.ExitCode(), stderr.String())
	}

	log := stderr.String()
	if !strings.Contains(log, "attempting visitor greet") || !strings.Contains(log, "\"uid\":777") {
		t.Errorf("subprocess stderr = %s\nwant GreetVisitors' own \"attempting visitor greet\" log for uid 777 -- proof CollectAll ran against the ORIGINAL non-empty visitors slice from Login(), not the fallback FetchBuildings call's empty one", log)
	}
}
