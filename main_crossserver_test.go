package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
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
// It also covers round 26's fix: origHost is normalized through firstHost before the comparison
// (see crossServerSaveBackNeeded's own doc comment), so a pipe-delimited multi-host origHost
// (e.g. "host-a|host-b", the shape -cs-ip/session-config's own help text documents as supported)
// connecting cleanly to the FIRST host is correctly recognized as "nothing changed" instead of
// spuriously reporting a save is needed.
//
// Mutation-testing note: reverting crossServerSaveBackNeeded to its pre-round-12 form (dropping
// the `newAccessTok != origAccessTok` term, i.e. `return newHost != origHost || newPort !=
// origPort || newZone != origZone`) makes the "access token alone changed" case below return
// false instead of true, failing this test. Likewise, dropping the `newGameUid != origGameUid`
// term makes the "only GameUid changed" case below return false instead of true. Dropping the
// `origHost = firstHost(origHost)` normalization makes the pipe-delimited-origHost case below
// return true instead of false. All three prove the test actually exercises the corresponding
// fix rather than passing vacuously.
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
		{
			// This is the round-26 regression case: origHost is a pipe-delimited multi-host
			// fallback list (exactly what -cs-ip/session-config's own help text documents as
			// supported, e.g. "host-a|host-b"), and newHost is firstHost(origHost) -- i.e. a clean
			// connection to the FIRST host, with no redirect and no other change at all. Before
			// this round's fix, crossServerSaveBackNeeded compared newHost against the RAW,
			// un-normalized origHost, so a single resolved host could never string-equal a
			// pipe-delimited list and this case spuriously returned true, permanently collapsing
			// the operator's configured multi-host resilience list down to one host in the
			// persisted session config on the very first run.
			name:    "pipe-delimited origHost, newHost is firstHost(origHost) -- no save needed",
			newHost: "host-a", newPort: 100, newZone: "APS1", newAccessTok: "tok", newGameUid: "uid1",
			origHost: "host-a|host-b", origPort: 100, origZone: "APS1", orig: "tok", origGameUid: "uid1",
			want: false,
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

// TestRunCrossServerTestExitsWhenIPExplicitlyEmpty is the regression test for this round's Fix 1:
// the firstHost(ip) == "" check above used to log the exact same "no ip given" message regardless
// of whether -cs-ip was actually typed on the command line with an empty value (e.g. -cs-ip "")
// versus never given at all -- unlike the sibling -cs-port check just below it in main.go, which
// already branched on o.portExplicit to produce a distinct, more accurate message for "explicitly
// passed but invalid" vs "never given at all". This drives runCrossServerTest with ipExplicit:
// true and ip left empty (as -cs-ip "" would leave it) and asserts the resulting message names
// -cs-ip and says it was given-but-empty, distinct from TestRunCrossServerTestExitsWhenIPEmpty
// above's "never given" wording.
//
// Mutation-testing note: reverting the ip check back to always logging "no ip given" regardless of
// o.ipExplicit makes this test's wantMsg assertion fail (the message would still contain "no ip
// given" instead of the explicit-specific wording), proving this test actually exercises the fix
// rather than passing vacuously.
//
// Uses the same re-exec-the-test-binary-as-a-subprocess idiom as TestRunCrossServerTestExitsWhenIPEmpty
// above, for the same reason: runCrossServerTest calls os.Exit(1) directly on this path.
func TestRunCrossServerTestExitsWhenIPExplicitlyEmpty(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		// ip is left empty, but ipExplicit is true -- as if -cs-ip "" were actually typed on the
		// command line. No -cs-rt is set, so this never reaches the GSL-refresh block; the fake GSL
		// server above only exists to satisfy the unconditional CheckVersion call before the ip/port
		// checks are reached.
		runCrossServerTest(crossServerTestOpts{
			ip:         "",
			ipExplicit: true,
			port:       18888,
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenIPExplicitlyEmpty$")
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
	log := stderr.String()
	const wantMsg = "-cs-ip was given but empty"
	if !strings.Contains(log, wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (distinguishing an explicitly-passed-but-empty -cs-ip from one never given at all)", log, wantMsg)
	}
	if strings.Contains(log, "no ip given") {
		t.Errorf("subprocess stderr = %s\nwant the explicit-but-empty wording, not the \"never given at all\" wording TestRunCrossServerTestExitsWhenIPEmpty already covers", log)
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

// TestRunCrossServerTestExitsWhenPortNotGiven is the regression test for this round's Fix 3:
// runCrossServerTest's "port <= 0" pre-flight guard (main.go, the sibling right below the
// firstHost(ip) == "" guard TestRunCrossServerTestExitsWhenIPEmpty above already covers) had zero
// test coverage of its own, despite that guard's own doc comment describing exactly why it exists
// (DoCrossServerLogin has no equivalent Port check; an unset/zero port would otherwise only be
// caught much later by the OS dial call, producing a cryptic "dial tcp 127.0.0.1:0: connect: can't
// assign requested address" instead of a message that says what's actually missing).
//
// Mirrors TestRunCrossServerTestExitsWhenIPEmpty's re-exec-subprocess pattern (runCrossServerTest
// calls os.Exit(1) directly on this path too, so it can't be driven to completion in-process
// without also killing this test binary), but with a valid ip and port left at/below zero instead.
// A fake GSL server is still needed even though -cs-rt is never set here: runCrossServerTest's
// CheckVersion call happens unconditionally, before the ip/port checks, regardless of -cs-rt (see
// its own doc comment in main.go) -- without overriding checkVersionHosts, that call would instead
// try to reach the real, live GSL hosts.
//
// Covers both boundary values the finding calls out ("port left at/below zero"): the flag's own
// zero-value default (0, e.g. -cs-port simply never passed) and a negative value (-1, e.g. a
// malformed -cs-port=-1).
func TestRunCrossServerTestExitsWhenPortNotGiven(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		port := 0
		if raw := os.Getenv("LASTWAR_TEST_CS_PORT"); raw != "" {
			p, err := strconv.Atoi(raw)
			if err != nil {
				t.Fatalf("parse LASTWAR_TEST_CS_PORT=%q: %v", raw, err)
			}
			port = p
		}

		// ip is a valid, non-empty value -- this test targets the port check specifically, not the
		// ip check TestRunCrossServerTestExitsWhenIPEmpty above already covers. No -cs-rt is set, so
		// this never reaches the GSL-refresh block at all; the fake GSL server above only exists to
		// satisfy the unconditional CheckVersion call before the ip/port checks are reached.
		runCrossServerTest(crossServerTestOpts{
			ip:   "1.2.3.4",
			port: port,
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will then
		// see a clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	for _, port := range []int{0, -1} {
		t.Run(fmt.Sprintf("port=%d", port), func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenPortNotGiven$")
			cmd.Env = append(os.Environ(),
				"LASTWAR_TEST_HELPER_PROCESS=1",
				fmt.Sprintf("LASTWAR_TEST_CS_PORT=%d", port),
			)
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
			const wantMsg = "no port given"
			if !strings.Contains(stderr.String(), wantMsg) {
				t.Errorf("subprocess stderr = %s\nwant it to contain %q (the pre-fix behavior instead falls through to a cryptic \"can't assign requested address\" dial failure)", stderr.String(), wantMsg)
			}
		})
	}
}

// TestRunCrossServerTestExitsWhenGameUidEmpty is the regression test for this round's fix: the
// gameUid == "" pre-flight check added to runCrossServerTest (main.go), the sibling right below
// the existing firstHost(ip) == "" and port <= 0 checks TestRunCrossServerTestExitsWhenIPEmpty and
// TestRunCrossServerTestExitsWhenPortNotGiven above already cover.
//
// Without that check, an empty gameUid reached DoCrossServerLogin unguarded (DoCrossServerLogin
// validates AccessTok itself but has no equivalent check for GameUid) and burned a full dial+login
// network round-trip only to fail downstream. Worse than a merely wasted round-trip: unlike
// base-zone login (login.go), which sends an empty "un" field as the normal case, cross-server
// login (crossserver.go) sends the gameUid value directly as the "un" field on the wire, so the
// resulting failure is wrapped in ErrAuthRejected -- the same ec=28/E011 signature README.md
// documents as meaning an expired/stale session, actively misdirecting an operator debugging a
// simple missing-gameUid configuration gap toward the wrong root cause.
//
// Mirrors TestRunCrossServerTestExitsWhenIPEmpty's exact scenario, substituting gameUid for ip: a
// -cs-rt refresh whose response carries a fresh access token (non-empty lsr.At) but an EMPTY
// server list -- refreshHasUsableData only requires EITHER to be usable, not both, so this passes
// that check and falls through with gameUid left exactly as empty as it started (no -cs-gameuid
// flag, no session config, and no server-list entry from the refresh to supply one). ip and port
// are both given valid values so this test isolates the gameUid check specifically, rather than
// tripping the sibling ip/port checks first.
//
// Uses the same re-exec-the-test-binary-as-a-subprocess idiom as
// TestRunCrossServerTestExitsWhenIPEmpty above, for the same reason: runCrossServerTest calls
// os.Exit(1) directly on this path, so it can't be driven to completion in-process without also
// killing this test binary.
func TestRunCrossServerTestExitsWhenGameUidEmpty(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{
			Code:       "0",
			ServerList: nil, // deliberately empty -- the exact case this test targets
			At:         &LoginToken{Token: "fresh-token-but-no-server-list"},
		})
		useFakeGSLServer(t, gsl)

		// gameUid is deliberately left unset, as if -cs-gameuid were never passed and no session
		// config supplied one either; ip/port are valid so this isolates the gameUid check.
		runCrossServerTest(crossServerTestOpts{
			ip:   "1.2.3.4",
			port: 18888,
			rt:   "some-refresh-token",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenGameUidEmpty$")
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
	const wantMsg = "no gameUid given"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (the pre-fix behavior instead proceeds into DoCrossServerLogin and burns a network round-trip before failing downstream)", stderr.String(), wantMsg)
	}
}

// TestRunCrossServerTestExitsWhenGameUidExplicitlyEmpty is the regression test for this round's
// Fix 1 (the gameUid half): the gameUid == "" check above used to log the exact same "no gameUid
// given" message regardless of whether -cs-gameuid was actually typed on the command line with an
// empty value (e.g. -cs-gameuid "") versus never given at all -- unlike the sibling -cs-port check
// in main.go, which already branched on o.portExplicit to produce a distinct, more accurate message
// for "explicitly passed but invalid" vs "never given at all". This drives runCrossServerTest with
// gameUidExplicit: true and gameUid left empty (as -cs-gameuid "" would leave it) and asserts the
// resulting message names -cs-gameuid and says it was given-but-empty, distinct from
// TestRunCrossServerTestExitsWhenGameUidEmpty above's "never given" wording.
//
// Mutation-testing note: reverting the gameUid check back to always logging "no gameUid given"
// regardless of o.gameUidExplicit makes this test's wantMsg assertion fail (the message would still
// contain "no gameUid given" instead of the explicit-specific wording), proving this test actually
// exercises the fix rather than passing vacuously.
//
// Uses the same re-exec-the-test-binary-as-a-subprocess idiom as
// TestRunCrossServerTestExitsWhenGameUidEmpty above, for the same reason: runCrossServerTest calls
// os.Exit(1) directly on this path. ip/port are both given valid values so this test isolates the
// gameUid check specifically, rather than tripping the sibling ip/port checks first.
func TestRunCrossServerTestExitsWhenGameUidExplicitlyEmpty(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		// gameUid is left empty, but gameUidExplicit is true -- as if -cs-gameuid "" were actually
		// typed on the command line. No -cs-rt is set, so this never reaches the GSL-refresh block;
		// the fake GSL server above only exists to satisfy the unconditional CheckVersion call
		// before the ip/port/gameUid checks are reached. ip/port are valid so this isolates the
		// gameUid check.
		runCrossServerTest(crossServerTestOpts{
			ip:              "1.2.3.4",
			port:            18888,
			gameUid:         "",
			gameUidExplicit: true,
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenGameUidExplicitlyEmpty$")
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
	log := stderr.String()
	const wantMsg = "-cs-gameuid was given but empty"
	if !strings.Contains(log, wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q (distinguishing an explicitly-passed-but-empty -cs-gameuid from one never given at all)", log, wantMsg)
	}
	if strings.Contains(log, "no gameUid given") {
		t.Errorf("subprocess stderr = %s\nwant the explicit-but-empty wording, not the \"never given at all\" wording TestRunCrossServerTestExitsWhenGameUidEmpty already covers", log)
	}
}

// TestRunCrossServerTestExitsWhenGSLRefreshCallFails is the regression test for this round's Fix
// 4: the -cs-rt GSL opt=refresh call itself failing (GetServerList returning a transport/HTTP
// error, as distinct from a successful-but-unusable response -- already covered by
// TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData above) had zero test coverage before
// this round.
//
// It also pins down which of the two exit paths after that error is actually reachable, settling
// the question this round's fix answered: GetServerList's own error returns (gsl.go) never wrap
// ErrAuthRejected -- only the SFS2X handshake/login/cross-server-login paths (conn.go, login.go,
// crossserver.go) do, since those are the ones that decode an explicit server-side rejection error
// code. So the errors.Is(err, ErrAuthRejected)-gated os.Exit(2) branch that used to exist here was
// dead code with an inaccurate comment claiming it matched those sibling sites; this round removed
// it, leaving only the generic os.Exit(1) fallback. This test proves that fallback fires correctly:
// a fake GSL server that answers getlsu3dversion.php (CheckVersion) normally but returns a plain
// HTTP 500 for getserverlist.php (the opt=refresh call) must exit 1 with the "GSL refresh failed"
// message.
//
// Uses a hand-built fake HTTP server (rather than newFakeGSLServer, which always answers
// getserverlist.php successfully) so getlsu3dversion.php can keep succeeding -- CheckVersion must
// succeed first, or runCrossServerTest exits fatally there instead, before ever reaching the
// opt=refresh call this test targets. Mirrors gsl_http_test.go's own
// TestGetServerListDecodeFailuresDoNotLeakRawResponse "HTTP status error" subtest for the
// getserverlist.php handler shape, combined with newFakeGSLServer's own getlsu3dversion.php
// handling.
func TestRunCrossServerTestExitsWhenGSLRefreshCallFails(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		pub := testRSAPubKeyDER(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
				_ = json.NewEncoder(w).Encode(CheckVersionResponse{ResMsg: pub})
			case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("simulated GSL server error"))
			default:
				http.NotFound(w, r)
			}
		}))
		defer server.Close()
		useFakeGSLServer(t, server)

		runCrossServerTest(crossServerTestOpts{
			ip:   "1.2.3.4",
			port: 18888,
			rt:   "some-refresh-token",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will then
		// see a clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsWhenGSLRefreshCallFails$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (GetServerList's HTTP-status error never wraps ErrAuthRejected, so only the generic os.Exit(1) fallback is reachable here); stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	const wantMsg = "GSL refresh failed"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q", stderr.String(), wantMsg)
	}
}

// fakeInitPushThenFailAllServer behaves exactly like login_integration_test.go's
// fakeInitPushServer for the login/init handshake (plain success Login response, then the bare
// `init` push), but -- unlike fakeInitPushServer, whose handler goroutine returns right after
// sending `init` and stops reading -- keeps the connection open afterward and answers EVERY
// subsequent request generically with a plain decoded, non-benign errorCode failure: it reads one
// envelope, echoes back whatever cmd it carried (same trick as conn_wait_test.go's
// readAndReply(server, "", ...)) with an errorCode no cmd's benignErrorCodes entry recognizes, and
// loops until the connection closes.
//
// Built for TestRunCrossServerTestCollectBenignFailuresDoNotBlockInteractive below: CollectAll
// (buildings.go) issues one independent sendAndWait per fixed action, and a fake server that never
// replies at all would make each one burn a real 8s defaultCmdTimeout (conn.go) before failing --
// exactly the cost TestSendAndWaitTimeoutNoResponse's own comment (conn_wait_test.go) documents
// this codebase deliberately avoids paying in a test. Replying immediately with a decoded errorCode
// failure instead produces the same "err != nil, but not a net.Error at all" shape as sendAndWait's
// ordinary per-item timeout does for containsNonTimeoutNetError's purposes (buildings.go: both are
// "ordinary business-logic/benign-timeout failures", not evidence of a dead connection) -- fast and
// deterministic instead of slow.
func fakeInitPushThenFailAllServer() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		if err := server.SendExtension("init", NewSFSObject()); err != nil {
			return
		}
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				continue
			}
			fail := NewSFSObject()
			fail.PutUtfString("errorCode", "999999") // not in benignErrorCodes for any cmd
			// Reply under BOTH the exact request cmd (sendAndWait's default waitCmds, used by 7
			// of CollectAll's 8 fixed sub-actions) AND its "push."-prefixed variant (the ONE
			// exception: mail.go's ListMail explicitly waits only on "push."+reqCmd, per the
			// real server's own documented response shape for that one command -- see ListMail's
			// sendAndWait call). Sending both, rather than trying to special-case which shape a
			// given cmd expects, guarantees a match regardless of which convention the specific
			// sub-action uses; whichever one a given sendAndWait call doesn't consume as its
			// match is just a harmless non-matching envelope for the NEXT sendAndWait call to
			// skip over, the same tolerated case TestWaitForDeadlineElapsedAfterNonMatchingEnvelope
			// (conn_wait_test.go) already covers.
			if err := server.SendExtension(msg.Cmd, fail); err != nil {
				return
			}
			if err := server.SendExtension("push."+msg.Cmd, fail); err != nil {
				return
			}
		}
	}
}

// TestRunCrossServerTestCollectBenignFailuresDoNotBlockInteractive is the end-to-end regression
// test for this round's Fix 1 (main.go): before this fix, runCrossServerTest treated ANY non-nil
// CollectAll error -- including one made up entirely of ordinary per-item business-logic/benign-
// timeout failures, which CollectAll's own doc comment (buildings.go) is explicit is "a normal,
// expected... not evidence the connection is dead" -- identically to a genuinely dead connection:
// os.Exit(1) before ever reaching the "if o.interactive != \"\" { RunInteractive(...) }" check a
// few lines later. An operator who explicitly passed -interactive alongside -collect had that
// explicit request silently discarded whenever even one of CollectAll's several independent
// requests came back with an ordinary failure -- not a rare edge case, given how many independent
// requests one collect run issues.
//
// Setup: fakeInitPushThenFailAllServer (above) logs the fake connection in and sends the `init`
// push normally, so FetchBuildings succeeds fast (same as this file's other tests), then answers
// every one of CollectAll's requests -- starting with CollectIdleReward's unconditional, always-
// sent "lw.pve.idle.reward" peek, which guarantees CollectAll's aggregated error is non-nil -- with
// a plain decoded errorCode failure: not a net.Error at all, let alone a non-timeout one, so
// containsNonTimeoutNetError(err) is false and shouldAbortBeforeInteractive (main.go) must NOT
// abort given -interactive was requested. No -cs-rt is used, so the fake GSL server below only
// needs to answer CheckVersion's unconditional getlsu3dversion.php call (see runCrossServerTest's
// own comment on why that call is made unconditionally) with something local, instead of possibly
// reaching out to a real host.
//
// -interactive itself points at a path that does not exist, so RunInteractive (interactive.go)
// logs its "interactive mode: reading commands" banner immediately (before ever touching that
// path) and then exits 1 shortly after via its own pre-existing, already-covered "stat control pipe
// failed" branch -- deliberately reused here instead of standing up a real FIFO, purely so this
// test can observe "control actually reached RunInteractive" via a distinctive log line without
// risking a hang.
//
// Before this round's fix, the subprocess below would instead exit 1 via CollectAll's own
// os.Exit(1), with "collect run had failures" logged and NOTHING further -- "interactive mode:
// reading commands" would never appear in stderr at all. That's the actual regression this test
// targets: the exit code alone is 1 either way (just via a different path), so the assertion below
// is on the log content, not the exit code.
func TestRunCrossServerTestCollectBenignFailuresDoNotBlockInteractive(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gameAddr := startFakeGameServer(t, fakeInitPushThenFailAllServer())
		gameHost, gamePort := splitHostPortInt(t, gameAddr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		runCrossServerTest(crossServerTestOpts{
			ip: gameHost, port: gamePort, zone: "APS1", gameUid: "uid-1", at: "tok-1",
			collect:     true,
			interactive: t.TempDir() + "/does-not-exist-fifo",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will then
		// see a clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestCollectBenignFailuresDoNotBlockInteractive$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1 (from RunInteractive's own \"stat control pipe failed\" exit, only reachable via this round's fix); stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "collect run had failures") {
		t.Errorf("subprocess stderr = %s\nwant it to still contain \"collect run had failures\" -- the collect failures must stay logged, not be silently swallowed, even though they're no longer fatal here", stderr.String())
	}
	const wantReachedInteractive = "interactive mode: reading commands"
	if !strings.Contains(stderr.String(), wantReachedInteractive) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q -- this is the actual regression this test targets: before this round's fix, CollectAll's error triggered os.Exit(1) before RunInteractive was ever reached, so this line would never appear", stderr.String(), wantReachedInteractive)
	}
}

// crossServerFetchBuildingsFailureServer answers the Login request normally, then writes a single
// malformed oversized frame directly on the raw connection (writeMalformedOversizedFrame,
// main_test.go): a declared body length over maxFrameSize (packet.go's own "frame body too large"
// guard). DoCrossServerLogin only ever reads the Login response itself before returning, so
// runCrossServerTest's own FetchBuildings call is the very first read this malformed frame can
// reach -- it errors immediately with a plain, non-net.Error decode failure instead of burning
// FetchBuildings' own 15s timeout.
func crossServerFetchBuildingsFailureServer() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		writeMalformedOversizedFrame(server)
	}
}

// TestRunCrossServerTestFetchBuildingsFailureWithInteractiveReachesRunInteractive is the round-26
// regression test for Fix 1's first call site: FetchBuildings' fallback call in runCrossServerTest
// used to unconditionally os.Exit(1) on ANY FetchBuildings error, with zero reference to whether
// -interactive was requested -- the exact same bug class round 25's shouldAbortBeforeInteractive
// fix closed for CollectAll's two call sites, just at this sibling call site one function up that
// fix never touched. See TestMainFetchBuildingsFallbackFailureWithInteractiveReachesRunInteractive
// (main_test.go) for the twin regression test at main()'s own equivalent call site.
//
// Mirrors TestRunCrossServerTestCollectBenignFailuresDoNotBlockInteractive's own end-to-end shape
// just above (drive runCrossServerTest directly via the re-exec-subprocess idiom, -interactive
// pointed at a path that can never become a real FIFO so RunInteractive's own startup log proves
// this call site was reached before it fails fast on its os.Stat check), but targets the
// FetchBuildings call site instead of CollectAll's.
func TestRunCrossServerTestFetchBuildingsFailureWithInteractiveReachesRunInteractive(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())

		gameAddr := startFakeGameServer(t, crossServerFetchBuildingsFailureServer())
		gameHost, gamePort := splitHostPortInt(t, gameAddr)

		gsl := newFakeGSLServer(t, LoginServerListRespon{Code: "0"})
		useFakeGSLServer(t, gsl)

		runCrossServerTest(crossServerTestOpts{
			ip: gameHost, port: gamePort, zone: "APS1", gameUid: "uid-1", at: "tok-1",
			interactive: t.TempDir() + "/does-not-exist-fifo",
		})
		// Only reached if runCrossServerTest fails to exit -- the outer assertions below will then
		// see a clean (non-error) subprocess exit and fail with a clear message instead of this
		// silently passing.
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestFetchBuildingsFailureWithInteractiveReachesRunInteractive$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
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
		t.Errorf("subprocess stderr = %s\nwant it to log FetchBuildings' failure", log)
	}
	if !strings.Contains(log, "frame body too large") {
		t.Errorf("subprocess stderr = %s\nwant the logged error to be the plain decode failure (not a net.Error timeout) -- otherwise this test isn't actually exercising the non-net-error path shouldAbortBeforeInteractive exists for", log)
	}
	if !strings.Contains(log, "interactive mode: reading commands") {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q -- this is the actual regression this test targets: before this round's fix, FetchBuildings' error triggered os.Exit(1) before RunInteractive was ever reached, so this line would never appear", log, "interactive mode: reading commands")
	}
	if !strings.Contains(log, "stat control pipe failed") {
		t.Errorf("subprocess stderr = %s\nwant RunInteractive's own bogus-control-pipe failure -- confirms the exit code 1 came from there, not from some earlier, different failure", log)
	}
}
