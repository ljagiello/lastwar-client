package main

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"bogus", slog.LevelInfo}, // unrecognized -- falls back to info (with a stderr warning)
		{"", slog.LevelInfo},      // the flag's own default -- falls back to info, no warning
	}
	for _, c := range cases {
		if got := parseLogLevel(c.in); got != c.want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", c.in, got, c.want)
		}
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
// main() itself. Every case below exits either inside fs.Parse's own error branch, or (the stray
// positional argument case, round 19's Fix 1) in the fs.NArg() check immediately after it succeeds
// -- either way, before any network/login/config-loading code runs, so this is safe to run with no
// fake servers or HOME override.
func TestMainFlagParseExitCodes(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		argv := []string{"lastwar-client"}
		if raw := os.Getenv("LASTWAR_TEST_MAIN_ARGS"); raw != "" {
			argv = append(argv, strings.Split(raw, "\x00")...)
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
	}{
		{"long help flag exits 0", []string{"-help"}, 0, ""},
		{"short help flag exits 0", []string{"-h"}, 0, ""},
		{"unrecognized flag exits 1", []string{"-this-flag-does-not-exist"}, 1, ""},
		{"malformed flag value exits 1", []string{"-cs-port=not-a-number"}, 1, ""},
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
			"stray positional argument exits 1 with a clear error instead of silently launching a real run",
			[]string{"collect"}, 1, "unexpected argument(s): collect",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainFlagParseExitCodes$")
			cmd.Env = append(os.Environ(),
				"LASTWAR_TEST_HELPER_PROCESS=1",
				"LASTWAR_TEST_MAIN_ARGS="+strings.Join(c.args, "\x00"),
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
		})
	}
}
