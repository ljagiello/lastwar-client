package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestCrossServerSaveBackNeeded is a fast, deterministic unit test of the pure comparison
// extracted from runCrossServerTest's save-back check (see crossServerSaveBackNeeded's doc
// comment in main.go). It directly covers round 12's Fix 1: a -cs-rt refresh that changes ONLY
// the access token (host/port/zone all unchanged) must still be reported as needing a save --
// before that round, runCrossServerTest's save-back condition didn't compare AccessTok at all,
// so this exact case was silently dropped. It also covers round 13's Fix 3: the same class of
// bug for GameUid, which was never compared at all until this round.
//
// Mutation-testing note: reverting crossServerSaveBackNeeded to its pre-round-12 form (dropping
// the `newAccessTok != origAccessTok` term, i.e. `return newHost != origHost || newPort !=
// origPort || newZone != origZone`) makes the "access token alone changed" case below return
// false instead of true, failing this test. Likewise, dropping the `newGameUid != origGameUid`
// term makes the "only GameUid changed" case below return false instead of true. Both prove the
// test actually exercises the corresponding fix rather than passing vacuously.
func TestCrossServerSaveBackNeeded(t *testing.T) {
	cases := []struct {
		name                                                                              string
		newHost, newZone, newAccessTok, newGameUid, origHost, origZone, orig, origGameUid string
		newPort, origPort                                                                 int
		want                                                                              bool
	}{
		{
			name:    "nothing changed",
			newHost: "1.2.3.4", newPort: 100, newZone: "APS1", newAccessTok: "tok", newGameUid: "uid1",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid1",
			want: false,
		},
		{
			name:    "host changed (serverInfo redirect)",
			newHost: "5.6.7.8", newPort: 100, newZone: "APS1", newAccessTok: "tok", newGameUid: "uid1",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid1",
			want: true,
		},
		{
			name:    "port changed",
			newHost: "1.2.3.4", newPort: 200, newZone: "APS1", newAccessTok: "tok", newGameUid: "uid1",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid1",
			want: true,
		},
		{
			name:    "zone changed",
			newHost: "1.2.3.4", newPort: 100, newZone: "APS2", newAccessTok: "tok", newGameUid: "uid1",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid1",
			want: true,
		},
		{
			// This is the round-12 regression case: address/zone are completely unchanged (no
			// serverInfo redirect happened), but the access token differs -- exactly the shape of
			// a -cs-rt refresh that obtained a fresh token without also hitting a redirect. Before
			// that round's fix, runCrossServerTest's save-back condition never looked at the
			// access token at all, so this case was indistinguishable from "nothing changed" and
			// the fresh token was never persisted.
			name:    "only access token changed (the -cs-rt refresh case)",
			newHost: "1.2.3.4", newPort: 100, newZone: "APS1", newAccessTok: "tok-fresh", newGameUid: "uid1",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok-stale", origGameUid: "uid1",
			want: true,
		},
		{
			// This is the round-13 regression case, the same class of bug as above but for
			// GameUid: host/port/zone/accessTok are all completely unchanged, but GameUid differs
			// -- exactly the shape of a -cs-rt refresh whose server list entry carries a new
			// GameUid with no other field changing. Before this round's fix,
			// crossServerSaveBackNeeded never compared GameUid at all, so this case was
			// indistinguishable from "nothing changed" and the fresh GameUid was never persisted.
			name:    "only GameUid changed (the -cs-rt refresh case)",
			newHost: "1.2.3.4", newPort: 100, newZone: "APS1", newAccessTok: "tok", newGameUid: "uid-fresh",
			origHost: "1.2.3.4", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid-stale",
			want: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := crossServerSaveBackNeeded(c.newHost, c.newPort, c.newZone, c.newAccessTok, c.newGameUid, c.origHost, c.origPort, c.origZone, c.orig, c.origGameUid)
			if got != c.want {
				t.Errorf("crossServerSaveBackNeeded(%+v) = %v, want %v", c, got, c.want)
			}
		})
	}
}

// TestRunCrossServerTestRtRefreshPersistsFreshAccessToken is the end-to-end regression test for
// round 12's Fix 1: it drives the actual runCrossServerTest function (not just the extracted
// pure function above) through a -cs-rt refresh that returns BOTH a fresh access token AND a
// server list -- the "ordinary case" described in the bug report, where DoCrossServerLogin's own
// Login exchange succeeds with no additional serverInfo redirect of its own. Before this round's
// fix, the save-back comparison diffed the post-refresh ip/port/zone/accessTok locals against
// themselves (always false) and never looked at AccessTok at all, so the freshly obtained token
// was silently never written to the session config file. This test proves it now is.
//
// Setup:
//   - a fresh device identity under an isolated HOME (loadOrCreateDeviceIdentity's own state
//     files -- no bearing on this test, just needs to succeed)
//   - a fake GSL HTTP server (newFakeGSLServer, reused from login_integration_test.go) answering
//     both CheckVersion's getlsu3dversion.php and the opt=refresh getserverlist.php call with a
//     server list pointing at a real fake game server, plus a fresh access token
//   - a fake game server (startFakeGameServer/fakeInitPushServer, reused from
//     crossserver_test.go/login_integration_test.go) that accepts the Login with no serverInfo
//     redirect and immediately sends the `init` bootstrap push, so FetchBuildings (which
//     runCrossServerTest calls unconditionally after a successful login) returns quickly instead
//     of waiting out its full 15s timeout
//
// crossServerTestOpts intentionally passes STALE/placeholder ip/port/zone/gameUid/at (as if
// loaded from an old session config) that differ from what the GSL refresh actually returns --
// mirroring the real bug scenario, where the refresh response's server list and access token
// replace what was originally on disk.
func TestRunCrossServerTestRtRefreshPersistsFreshAccessToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const (
		staleAccessTok = "stale-access-token-from-old-config"
		freshAccessTok = "fresh-access-token-from-refresh"
		freshZone      = "APS-REAL"
		freshGameUid   = "uid-real"
	)

	gameAddr := startFakeGameServer(t, fakeInitPushServer(nil))
	gameHost, gamePort := splitHostPortInt(t, gameAddr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code: "0",
		ServerList: []LoginServerInfo{
			{IP: gameHost, Port: gamePort, Zone: freshZone, GameUid: freshGameUid},
		},
		At: &LoginToken{Token: freshAccessTok},
	})
	useFakeGSLServer(t, gsl)

	cfgPath := t.TempDir() + "/session.json"

	runCrossServerTest(crossServerTestOpts{
		// Stale placeholders, standing in for what would have been loaded from an old session
		// config or passed on the command line -- deliberately different from the fake GSL
		// server's refresh response in every field, including the access token.
		ip:      "stale-placeholder-host",
		port:    1,
		zone:    "APS-STALE",
		gameUid: "uid-stale",
		at:      staleAccessTok,
		rt:      "some-refresh-token",

		configSavePath: cfgPath,
	})

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("session config was not written to %s: %v", cfgPath, err)
	}
	var got SessionConfig
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse persisted session config: %v", err)
	}

	if got.AccessToken != freshAccessTok {
		t.Errorf("persisted AccessToken = %q, want %q (the fresh -cs-rt token, not %q)", got.AccessToken, freshAccessTok, staleAccessTok)
	}
	if got.IP != gameHost {
		t.Errorf("persisted IP = %q, want %q (the refreshed server list's host)", got.IP, gameHost)
	}
	if got.Port != gamePort {
		t.Errorf("persisted Port = %d, want %d (the refreshed server list's port)", got.Port, gamePort)
	}
	if got.Zone != freshZone {
		t.Errorf("persisted Zone = %q, want %q (the refreshed server list's zone)", got.Zone, freshZone)
	}
	if got.GameUid != freshGameUid {
		t.Errorf("persisted GameUid = %q, want %q (the refreshed server list's gameUid)", got.GameUid, freshGameUid)
	}
}

// TestRunCrossServerTestExitsWhenIPEmpty is the regression test for this round's fix: the
// firstHost(ip) == "" pre-flight check added to runCrossServerTest alongside its existing
// port <= 0 check (see main.go). Without that check, an empty ip reaches
// crossserver.go's addr := fmt.Sprintf("%s:%d", firstHost(p.IP), p.Port) as bare ":<port>" --
// and Go's "host:port" dial syntax resolves an empty host to the LOOPBACK interface, so this
// never failed with any "no host" indication at all: it silently attempted a real TCP connection
// to 127.0.0.1/::1 and returned a misleading "connection refused" (still exit code 1, but via
// DoCrossServerLogin's generic "cross-server login failed" path, not a message that says what's
// actually missing).
//
// This specifically reproduces the reachable path the fix's comment documents: a bare -cs-rt
// with no -cs-ip (and no session config supplying one) leaves ip empty in scope, and
// refreshHasUsableData only requires EITHER a fresh access token OR a non-empty server list --
// not both -- so a GSL opt=refresh response carrying a fresh access token but an EMPTY server
// list passes that check and falls through with ip left exactly as empty as it started.
//
// runCrossServerTest calls os.Exit(1) directly on this path (matching the sibling port <= 0
// check's own posture), so it can't be driven to completion in-process without also killing this
// test binary. This uses the standard re-exec-the-test-binary-as-a-subprocess idiom (as used by
// e.g. the Go standard library's own os/exec tests) instead: LASTWAR_TEST_HELPER_PROCESS=1 gates
// a branch that actually calls runCrossServerTest and lets it exit, while the outer test spawns
// that as a child process and asserts on its exit code AND stderr message -- the message check is
// what actually distinguishes the fixed "no ip given" exit from the pre-fix "connection refused"
// exit, since both are exit code 1.
func TestRunCrossServerTestExitsWhenIPEmpty(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: nil, // deliberately empty -- the exact case this test targets
			At:         &LoginToken{Token: "fresh-token-but-no-server-list"},
		})
		useFakeGSLServer(t, gsl)

		// ip is deliberately left unset, as if -cs-rt were passed alone with no -cs-ip and no
		// session config supplying one.
		runCrossServerTest(crossServerTestOpts{
			port: 18888,
			rt:   "some-refresh-token",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenIPEmpty$")
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
	const wantMsg = "no ip given"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the pre-fix behavior instead falls through to a misleading \"connection refused\" dial failure)", stderr.String(), wantMsg)
	}
}

// TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData is the regression test for this
// round's Fix 1 (exit-code contract gap): a GSL opt=refresh response with neither a fresh access
// token nor a non-empty server list (refreshHasUsableData(lsr) == false) used to exit 1, the
// generic "look at the log" code -- but README.md makes an explicit, unqualified operator-facing
// promise ("Exit code 2 means the session itself is stale ... Login/auth failures (both the
// plain-login and cross-server-reconnect paths) exit 2 specifically ... a cron wrapper can check $?
// directly ... without needing to grep the log"). A GSL refresh rejection IS semantically a
// stale/rejected session, so this path now exits 2 instead.
//
// Uses the same re-exec-the-test-binary-as-a-subprocess idiom as
// TestRunCrossServerTestExitsWhenIPEmpty above, for the same reason: runCrossServerTest calls
// os.Exit directly on this path, so it can't be driven to completion in-process without also
// killing this test binary.
func TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: nil, // deliberately empty...
			At:         nil, // ...and no fresh access token: refreshHasUsableData(lsr) == false
		})
		useFakeGSLServer(t, gsl)

		runCrossServerTest(crossServerTestOpts{
			ip:   "1.2.3.4", // present so this fails on the refresh-data check, not the ip check
			port: 18888,
			rt:   "some-refresh-token",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("subprocess exit code = %d, want 2 (README's documented stale-session exit-code contract, not the generic 1); stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	const wantMsg = "no usable data"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q", stderr.String(), wantMsg)
	}
}

// TestRunCrossServerTestServerListOverrideLogging is the end-to-end regression test for this
// round's Fix 2 (silent flag override, log-level asymmetry): a GSL opt=refresh response with a
// non-empty ServerList silently reassigns ip/port/zone/gameUid, even when the operator explicitly
// passed -cs-ip/-cs-port/-cs-zone/-cs-gameuid -- but that used to only ever log at INFO
// ("server selected"), unlike the symmetric -cs-rt-overriding-cs-at case right above it in
// runCrossServerTest, which already logs an explicit WARN. Given README's own guidance that
// -log-level trims a cron log down to warnings/errors only, an operator running with a trimmed
// log level would see the -cs-at override warning but silently miss this one. This drives
// runCrossServerTest through both shapes -- explicit flags actually being overridden, and no
// explicit flags at all (e.g. a fresh cron run with only a session config, or nothing set yet) --
// and asserts the log level/wording differs accordingly.
func TestRunCrossServerTestServerListOverrideLogging(t *testing.T) {
	const freshZone, freshGameUid = "APS-REAL", "uid-real"

	run := func(t *testing.T, explicit bool) string {
		t.Setenv("HOME", t.TempDir())

		gameAddr := startFakeGameServer(t, fakeInitPushServer(nil))
		gameHost, gamePort := splitHostPortInt(t, gameAddr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code: "0",
			ServerList: []LoginServerInfo{
				{IP: gameHost, Port: gamePort, Zone: freshZone, GameUid: freshGameUid},
			},
			At: &LoginToken{Token: "fresh-token"},
		})
		useFakeGSLServer(t, gsl)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(orig)

		// Stale placeholders, deliberately different from the fake GSL server's refresh response
		// in every field, mirroring an old session config or explicit -cs-* flags being overridden.
		runCrossServerTest(crossServerTestOpts{
			ip: "stale-placeholder-host", port: 1, zone: "APS-STALE", gameUid: "uid-stale",
			rt:         "some-refresh-token",
			ipExplicit: explicit, portExplicit: explicit, zoneExplicit: explicit, gameUidExplicit: explicit,
		})
		return buf.String()
	}

	t.Run("explicit flags overridden -> WARN naming them", func(t *testing.T) {
		log := run(t, true)
		if !strings.Contains(log, "level=WARN") {
			t.Errorf("log = %s\nwant a WARN-level record", log)
		}
		if !strings.Contains(log, "overriding explicitly-passed flag") {
			t.Errorf("log = %s\nwant it to mention the override explicitly", log)
		}
		for _, name := range []string{"cs-ip", "cs-port", "cs-zone", "cs-gameuid"} {
			if !strings.Contains(log, name) {
				t.Errorf("log = %s\nwant it to name %q among the overridden flags", log, name)
			}
		}
	})

	t.Run("no explicit flags -> plain INFO, no override WARN", func(t *testing.T) {
		log := run(t, false)
		if strings.Contains(log, "overriding explicitly-passed flag") {
			t.Errorf("log = %s\nwant no override WARN when nothing was explicitly set (e.g. a fresh cron run)", log)
		}
		if !strings.Contains(log, "server selected") {
			t.Errorf("log = %s\nwant the original plain INFO \"server selected\" log to still fire unescalated", log)
		}
	})
}

// TestRunCrossServerTestAtWarningAttribution is the end-to-end regression test for this round's
// Fix 3 (misattribution to the flag when the value came from config): the "ignoring -cs-at because
// -cs-rt is set" and "continuing with the original -cs-at unrefreshed" warnings used to fire on the
// bare condition o.at != "", with no distinction between "-cs-at was actually typed on the command
// line" and "-cs-at ended up non-empty purely because a loaded session config's accessToken field
// was merged in" (see the config-merge step in main(), "*csAt = applyOverride(cfg.AccessToken,
// *csAt)") -- so a config-sourced value got blamed on a flag the operator never typed. This drives
// runCrossServerTest through all four combinations of {refresh returns a fresh token, refresh
// returns no token} x {-cs-at explicitly typed, -cs-at from config only} and checks the resulting
// log wording names -cs-at only in the explicit cases.
func TestRunCrossServerTestAtWarningAttribution(t *testing.T) {
	const freshZone, freshGameUid = "APS-REAL", "uid-real"

	run := func(t *testing.T, atExplicit, withFreshToken bool) string {
		t.Setenv("HOME", t.TempDir())

		gameAddr := startFakeGameServer(t, fakeInitPushServer(nil))
		gameHost, gamePort := splitHostPortInt(t, gameAddr)

		lsr := LoginServerListRespon{
			Code: "0",
			// A non-empty server list keeps refreshHasUsableData true even in the
			// withFreshToken=false cases below, where At is left nil.
			ServerList: []LoginServerInfo{
				{IP: gameHost, Port: gamePort, Zone: freshZone, GameUid: freshGameUid},
			},
		}
		if withFreshToken {
			lsr.At = &LoginToken{Token: "fresh-token"}
		}
		gsl := newFakeGSLServer(t, lsr)
		useFakeGSLServer(t, gsl)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(orig)

		runCrossServerTest(crossServerTestOpts{
			at: "some-access-token", rt: "some-refresh-token",
			atExplicit: atExplicit,
		})
		return buf.String()
	}

	t.Run("refresh returns fresh token, -cs-at explicitly typed -> warns and names -cs-at", func(t *testing.T) {
		log := run(t, true, true)
		if !strings.Contains(log, "ignoring -cs-at because -cs-rt is set") {
			t.Errorf("log = %s\nwant the \"ignoring -cs-at\" warning, naming the flag the operator actually typed", log)
		}
	})

	t.Run("refresh returns fresh token, -cs-at value came from config only -> no misattributed warning", func(t *testing.T) {
		log := run(t, false, true)
		if strings.Contains(log, "-cs-at") {
			t.Errorf("log = %s\nwant no warning misattributing the config-sourced value to a flag the operator never typed", log)
		}
	})

	t.Run("refresh returns no token, -cs-at explicitly typed -> warns and names -cs-at", func(t *testing.T) {
		log := run(t, true, false)
		if !strings.Contains(log, "continuing with the original -cs-at unrefreshed") {
			t.Errorf("log = %s\nwant the \"-cs-at unrefreshed\" warning, naming the flag the operator actually typed", log)
		}
	})

	t.Run("refresh returns no token, -cs-at value came from config only -> warns without naming -cs-at", func(t *testing.T) {
		log := run(t, false, false)
		if !strings.Contains(log, "continuing with the session config's access token, unrefreshed") {
			t.Errorf("log = %s\nwant the config-attributed \"unrefreshed\" warning -- the operational risk (a stale, unrefreshed token) is worth flagging either way, just not misattributed to -cs-at", log)
		}
		if strings.Contains(log, "-cs-at") {
			t.Errorf("log = %s\nwant it to not misattribute the config-sourced value to -cs-at", log)
		}
	})
}

// TestRunCrossServerTestNoAccessTokenAtAllWarning is the regression test for this round's Fix 3:
// the "o.at != "" but lsr.At is nil" case just above (see TestRunCrossServerTestAtWarningAttribution)
// already logs a WARN when a GSL refresh leaves a possibly-stale token in place unrefreshed -- but
// the symmetric, strictly worse case (o.at was ALREADY empty -- no -cs-at, no session-config access
// token at all -- AND the refresh response's lsr.At is also nil) used to log nothing at all. That
// leaves accessTok as the empty string it already was, which DoCrossServerLogin's own
// `p.AccessTok == ""` check rejects immediately (before ever dialing) -- but until this fix, an
// operator watching the log (even at a trimmed -log-level=warn) would get zero indication of why
// until that later, less specific-sounding failure. This drives runCrossServerTest with o.at left
// empty and a GSL refresh response that carries a non-empty server list (so refreshHasUsableData
// stays true and this reaches the access-token branch at all, per refreshHasUsableData's own
// doc comment/test) but no At token.
//
// Because DoCrossServerLogin's AccessTok=="" check fails fast with a plain error (not
// ErrAuthRejected), runCrossServerTest's own generic error path calls os.Exit(1) -- so, like
// TestRunCrossServerTestExitsWhenIPEmpty and TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData
// above, this can't be driven to completion in-process without also killing this test binary, and
// uses the same re-exec-the-test-binary-as-a-subprocess idiom instead. No fake game server is needed
// (unlike this file's other end-to-end runCrossServerTest tests): DoCrossServerLogin returns its
// error before ever dialing one, so the server list's IP/port here are just placeholders.
func TestRunCrossServerTestNoAccessTokenAtAllWarning(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code: "0",
			// A non-empty server list keeps refreshHasUsableData true even though At is left nil
			// below -- this is the exact shape that reaches the "no access token" branch without
			// failing earlier on refreshHasUsableData's own check. The IP/port are placeholders:
			// DoCrossServerLogin's AccessTok=="" check rejects before ever dialing this address.
			ServerList: []LoginServerInfo{
				{IP: "192.0.2.1", Port: 12345, Zone: "APS-REAL", GameUid: "uid-real"},
			},
			At: nil,
		})
		useFakeGSLServer(t, gsl)

		runCrossServerTest(crossServerTestOpts{
			// o.at deliberately left empty -- no -cs-at flag, no session-config access token either.
			at: "",
			rt: "some-refresh-token",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will then
		// see a clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoAccessTokenAtAllWarning$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (DoCrossServerLogin's plain AccessTok==\"\" error, not a confirmed auth rejection); stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	log := stderr.String()
	const wantMsg = "GSL refresh response carried no access token, and none was already set"
	if !strings.Contains(log, wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the new warning for the o.at==\"\" case)", log, wantMsg)
	}
	if strings.Contains(log, "continuing with the original -cs-at unrefreshed") ||
		strings.Contains(log, "continuing with the session config's access token, unrefreshed") {
		t.Errorf("subprocess stderr = %s\nwant the o.at==\"\" branch's own distinct wording, not the sibling o.at!=\"\" branch's \"unrefreshed\" wording (there's no stale token here to be unrefreshed, there's no token at all)", log)
	}
}
