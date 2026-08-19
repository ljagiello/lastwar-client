package main

import (
	"encoding/json"
	"os"
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
		Code: 0,
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
