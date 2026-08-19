package main

import (
	"bytes"
	"encoding/binary"
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
		})
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
	const wantMsg = "invalid -cs-port value: -1 (must be positive)"
	if !strings.Contains(log, wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the new wording for an explicitly-typed but invalid port, distinct from the genuinely-never-given case)", log, wantMsg)
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
			ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: "uid-1"}},
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
// allocate) a length-sized body -- so no body bytes are actually consumed here, and the
// underlying byte stream stays synchronized for whatever request/response traffic follows this
// call. The resulting client-side error is a plain fmt.Errorf ("frame body too large: ..."), not
// wrapped in packet.go's net.Error-satisfying deadConnError (that type is reserved for genuine
// connection death, io.EOF/io.ErrUnexpectedEOF) -- exactly the "still-healthy connection hitting
// one bad frame" shape round 26's FetchBuildings-call-site fix targets.
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

// mainFetchBuildingsFailureFakeGameServer answers the base zone Login normally, then sends an
// `init` push with an empty building_new (0 buildings, satisfying Login()'s own waitForInitPush
// fast -- gotInit=true, buildings=nil -- instead of waiting out the full 45s initPushTimeout for a
// genuine silence timeout), which is what actually reaches main()'s zero-buildings FetchBuildings
// fallback call site. It then writes a single malformed oversized frame directly on the
// connection (writeMalformedOversizedFrame above): FetchBuildings' fallback call reads this as
// its very first envelope and returns a plain, non-net.Error decode failure immediately, instead
// of burning the fallback's own 12s timeout.
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
		writeMalformedOversizedFrame(server)
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
func TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		addr := startFakeGameServer(t, mainFetchBuildingsFailureFakeGameServer())
		host, port := splitHostPortInt(t, addr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: "uid-1"}},
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
	if !strings.Contains(log, "fetch buildings failed") {
		t.Errorf("subprocess stderr = %s\nwant it to log FetchBuildings' fallback failure", log)
	}
	if !strings.Contains(log, "frame body too large") {
		t.Errorf("subprocess stderr = %s\nwant the logged error to be the plain decode failure (not a net.Error timeout) -- otherwise this test isn't actually exercising the non-net-error path shouldAbortBeforeInteractive exists for", log)
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
// It then writes a single malformed oversized frame (writeMalformedOversizedFrame above): main()'s
// zero-buildings FetchBuildings fallback call reads this as its very first envelope and returns
// immediately with 0 buildings AND 0 visitors of its own -- the exact shape round 26's Fix 4
// targets, where an unconditional visitors overwrite would silently discard the real, already-known
// visitor list obtained above. Finally it answers CollectAll's fixed request sequence generically
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

		writeMalformedOversizedFrame(server)

		// CollectAll's 8 fixed sub-actions issue 9 requests total when buildings/visitors are both
		// empty (see mainCollectInteractiveFakeGameServer's own doc comment for the per-action
		// breakdown: idle x2, mail-list x1, help-all x1, gifts x2, tech-refresh x1, vip x2) -- plus
		// one more here for GreetVisitors' single visitor.operate call, since this test's whole
		// point is that the ORIGINAL non-empty visitors slice from Login() survives into CollectAll
		// despite the fallback FetchBuildings call above returning 0 visitors of its own.
		const wantRequests = 10
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
			ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: "uid-1"}},
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
