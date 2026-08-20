package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Login() dials via DialGame and fetches its RSA pubkey/server list over real HTTP (CheckVersion,
// GetServerList), so -- like crossserver_test.go's DoCrossServerLogin tests -- it can't be
// exercised over net.Pipe alone. These tests reuse crossserver_test.go's net.Listen-based fake
// SFS2X server helpers (newFakeGameListener/serveFakeGameServer/startFakeGameServer/
// putRedirectServerInfo/splitHostPortInt) for the SFS2X side, and add a small combined fake HTTP
// server for the CheckVersion + GetServerList side, since Login() always resolves both from the
// same gateHost (the single host CheckVersion's checkVersionHosts fallback list happened to
// answer from -- see gsl_http_test.go's TestCheckVersionAgainstFakeServer for the override
// pattern this borrows). Device-identity state (deviceId/gameUid/loginKey persisted under HOME) is
// isolated per test via t.Setenv("HOME", t.TempDir()), the same pattern identity_test.go uses.

// newFakeGSLServer stands up one httptest.Server answering both CheckVersion's
// getlsu3dversion.php (always the same canned response, carrying a throwaway RSA pubkey) and
// GetServerList's getserverlist.php (like TestGetServerListAgainstFakeServer, a plaintext --
// unencrypted -- LoginServerListRespon body, which GetServerList already falls back to when no
// "bin" field is present). gslResponses is consumed one entry per GetServerList POST, in order;
// once exhausted, the last entry repeats, so a test only needs to supply as many entries as it
// cares to distinguish (one per distinct GetServerList call it expects, e.g. the initial
// opt=new/fix/login call and, separately, any mid-redirect opt=fix refresh call).
func newFakeGSLServer(t *testing.T, gslResponses ...LoginServerListRespon) *httptest.Server {
	t.Helper()
	pub := testRSAPubKeyDER(t)

	var mu sync.Mutex
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			_ = json.NewEncoder(w).Encode(CheckVersionResponse{ResMsg: flexString(pub)})
		case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
			mu.Lock()
			idx := call
			if idx >= len(gslResponses) {
				idx = len(gslResponses) - 1
			}
			call++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(gslResponses[idx])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// useFakeGSLServer points checkVersionHosts at server for the duration of the test, restoring the
// real list on cleanup -- same override pattern as gsl_http_test.go's CheckVersion tests.
func useFakeGSLServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	orig := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	t.Cleanup(func() { checkVersionHosts = orig })
}

// fakeInitPushServer replies to a base zone Login (whatever content it receives) with a plain
// success response, then immediately follows up with the bare `init` bootstrap push
// waitForInitPush is waiting for -- so Login()'s step 5 completes almost instantly instead of
// waiting out its real 45s timeout (that timeout is a local const inside Login(), not overridable
// from a test, so the fake server sending `init` promptly is the only way to keep this test fast).
// zoneSeen, if non-nil, receives the zone (`zn`) the client actually logged in with, so a redirect
// test can confirm the redialed Login used the new zone, not the original one.
func fakeInitPushServer(zoneSeen chan<- string) func(*GameConn) {
	return func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if zoneSeen != nil && env.Content != nil {
			zoneSeen <- env.Content.GetString("zn")
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", NewSFSObject())
	}
}

// TestLoginGuestHappyPath covers Login()'s basic dial+login+init-push flow end to end against
// fake servers, with no serverInfo redirect involved: a fresh device identity, a single fake game
// server that accepts the base zone Login and sends the `init` push right away, and a fake GSL
// endpoint returning that server's address. Confirms Login() returns successfully as a guest (no
// -email given) and persists the gameUid the fake server list handed back.
func TestLoginGuestHappyPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, fakeInitPushServer(nil))
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	if result.Ident == nil {
		t.Fatal("result.Ident = nil")
	}
	if result.Ident.GameUid != "uid-1" {
		t.Errorf("Ident.GameUid = %q, want %q (from the GSL server list entry)", result.Ident.GameUid, "uid-1")
	}
}

// fakeInitPushServerWithDuplicateUUIDs mirrors fakeInitPushServer above, but the `init` push it
// sends carries a building_new array with building uuid 111 repeated (plus a distinct uuid 222), and
// a visitor.list array with visitor uid 444 repeated (plus a distinct uid 555) -- reproducing a peer
// that resends the same building/visitor entry within a single init push. See
// TestLoginDedupesInitPushBuildingsAndVisitors, the round 26 regression test this exists for.
func fakeInitPushServerWithDuplicateUUIDs() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}

		b1 := NewSFSObject()
		b1.PutLong("uuid", 111)
		b1.PutInt("bId", BuildingFarmland)
		b1Dup := NewSFSObject()
		b1Dup.PutLong("uuid", 111)
		b1Dup.PutInt("bId", BuildingFarmland)
		b2 := NewSFSObject()
		b2.PutLong("uuid", 222)
		b2.PutInt("bId", BuildingIronMine)
		buildingArr := NewSFSArray()
		buildingArr.AddSFSObject(b1)
		buildingArr.AddSFSObject(b1Dup)
		buildingArr.AddSFSObject(b2)

		v1 := NewSFSObject()
		v1.PutLong("uid", 444)
		v1.PutInt("eventId", 2001)
		v1Dup := NewSFSObject()
		v1Dup.PutLong("uid", 444)
		v1Dup.PutInt("eventId", 2001)
		v2 := NewSFSObject()
		v2.PutLong("uid", 555)
		v2.PutInt("eventId", 2002)
		visitorList := NewSFSArray()
		visitorList.AddSFSObject(v1)
		visitorList.AddSFSObject(v1Dup)
		visitorList.AddSFSObject(v2)
		visitorObj := NewSFSObject()
		visitorObj.PutSFSArray("list", visitorList)

		init := NewSFSObject()
		init.PutSFSArray("building_new", buildingArr)
		init.PutSFSObject("visitor", visitorObj)
		_ = server.SendExtension("init", init)
	}
}

// TestLoginDedupesInitPushBuildingsAndVisitors is the regression test for round 26's fix to
// waitForInitPush -- the PRIMARY init-push path used on every login (Login() calls it directly;
// FetchBuildings in buildings.go is only a fallback reached when this path's result comes back
// empty). Before this fix, waitForInitPush returned ParseInitBuildings/ParseInitVisitors' raw output
// with no per-uuid deduplication at all, unlike FetchBuildings' own
// seenBuildingUUIDs/seenVisitorUUIDs-backed dedup (round 12). A fake server whose `init` push repeats
// one building uuid and one visitor uid must therefore still leave Login()'s returned
// Buildings/Visitors slices with that uuid/uid present exactly once, not once per repetition --
// otherwise CollectAll/GreetVisitors would issue a real, redundant network request for the same uuid
// twice (see dedupeBuildings/dedupeVisitors in login.go).
func TestLoginDedupesInitPushBuildingsAndVisitors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, fakeInitPushServerWithDuplicateUUIDs())
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	if len(result.Buildings) != 2 {
		t.Fatalf("got %d buildings, want 2 (uuid 111 deduped from 2 occurrences to 1, plus uuid 222)", len(result.Buildings))
	}
	seenB := map[int64]int{}
	for _, b := range result.Buildings {
		seenB[b.Uuid()]++
	}
	if seenB[111] != 1 {
		t.Errorf("building uuid 111 appears %d times in result.Buildings, want 1", seenB[111])
	}
	if seenB[222] != 1 {
		t.Errorf("building uuid 222 appears %d times in result.Buildings, want 1", seenB[222])
	}

	if len(result.Visitors) != 2 {
		t.Fatalf("got %d visitors, want 2 (uid 444 deduped from 2 occurrences to 1, plus uid 555)", len(result.Visitors))
	}
	seenV := map[int64]int{}
	for _, v := range result.Visitors {
		seenV[v.Uid()]++
	}
	if seenV[444] != 1 {
		t.Errorf("visitor uid 444 appears %d times in result.Visitors, want 1", seenV[444])
	}
	if seenV[555] != 1 {
		t.Errorf("visitor uid 555 appears %d times in result.Visitors, want 1", seenV[555])
	}
}

// fakeInitPushServerWithWrongTypedBuildingUUID mirrors fakeInitPushServer above, but the `init`
// push it sends carries a building_new array with ONE entry whose `uuid` field has the WRONG
// concrete SFS type (a UtfString, "not-a-long", instead of a Long) alongside a separate, genuinely
// well-typed building whose uuid happens to be the real zero value (0). See
// TestLoginRejectsWrongTypedBuildingUUID, the round 28 regression test this exists for.
func fakeInitPushServerWithWrongTypedBuildingUUID() func(*GameConn) {
	return func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}

		wrongTyped := NewSFSObject()
		wrongTyped.PutUtfString("uuid", "not-a-long") // wrong SFS type: a building uuid must be a Long
		wrongTyped.PutInt("bId", BuildingFarmland)
		genuineZero := NewSFSObject()
		genuineZero.PutLong("uuid", 0) // a real, well-typed uuid that happens to be zero
		genuineZero.PutInt("bId", BuildingIronMine)
		buildingArr := NewSFSArray()
		buildingArr.AddSFSObject(wrongTyped)
		buildingArr.AddSFSObject(genuineZero)

		init := NewSFSObject()
		init.PutSFSArray("building_new", buildingArr)
		_ = server.SendExtension("init", init)
	}
}

// TestLoginRejectsWrongTypedBuildingUUID is the round-28 regression test for requireFieldType
// (buildings.go), exercised end to end through Login()'s PRIMARY init-push path -- waitForInitPush
// (login.go), called directly by Login() on every login, not FetchBuildings' fallback -- proving
// the fix actually protects that path, not just ParseInitBuildings in isolation.
//
// Before this round's fix, requirePresentField only checked that `uuid` was present and non-nil,
// never that its concrete decoded SFS type matched what Building.Uuid() (GetLong) accepts. A
// present-but-wrong-typed uuid silently passed that guard and coerced to int64(0) via GetLong's own
// zero-value fallback -- indistinguishable from THIS test's separate, genuinely-well-typed uuid=0
// building. dedupeBuildings (login.go) would then see two buildings both reading as uuid=0 and
// silently drop one as a spurious "duplicate" -- exactly the scenario TestLoginDedupesInitPushBuildingsAndVisitors
// above proves is otherwise correct behavior for a REAL duplicate, but wrong here since these are
// two distinct buildings, not one resent twice.
//
// Mutation check: reverting ParseInitBuildings' `requireFieldType(bi, "uuid", "building_new",
// sfsFieldKindLong)` back to `requirePresentField(bi, "uuid", "building_new")` makes this test fail
// with len(result.Buildings) == 1 instead of... actually still 1, but the WRONG one survives
// (whichever of the two colliding uuid=0 entries dedupeBuildings happens to keep first) -- so the
// real assertion that catches the regression is BId(), not just the count: it would come back as
// BuildingFarmland (the wrong-typed entry, first in the array) instead of BuildingIronMine (the
// genuine one).
func TestLoginRejectsWrongTypedBuildingUUID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, fakeInitPushServerWithWrongTypedBuildingUUID())
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	if len(result.Buildings) != 1 {
		t.Fatalf("got %d buildings, want exactly 1 (only the genuine, well-typed uuid=0 entry -- the string-typed one must be rejected outright, not silently coerced to uuid=0 and merged with the genuine one)", len(result.Buildings))
	}
	if result.Buildings[0].Uuid() != 0 || result.Buildings[0].BId() != BuildingIronMine {
		t.Errorf("got building uuid=%d bId=%d, want the genuine uuid=0 bId=%d (BuildingIronMine) entry -- got the wrong-typed entry's bId instead if this reads BuildingFarmland", result.Buildings[0].Uuid(), result.Buildings[0].BId(), BuildingIronMine)
	}
}

// TestLoginConnectionFailureWhileWaitingForInit is the integration-level regression test for
// round 17's fix in Login() itself (the "if initErr != nil { ...; conn.Close(); return nil,
// fmt.Errorf(...) }" block in step 5, right after the waitForInitPush call): a genuine connection
// failure while waiting for the `init` bootstrap push must make Login() itself fail fast with a
// wrapping error and a nil *LoginResult, not silently fall through to "giving up... continuing
// anyway" the way a plain silence-until-deadline timeout does.
//
// TestWaitForInitPushConnectionFailure (conn_wait_test.go) already proves this at the
// waitForInitPush helper level directly, over a raw net.Pipe -- but every existing Login() test in
// this file drives its fake server through fakeInitPushServer or an equivalent send-the-init-push
// path, so nothing exercises the initErr!=nil branch through Login()'s own entry point: if that
// block were ever deleted or inverted, no test in the repo would catch it. This test closes that
// gap by having the fake game server accept the base zone Login normally (plain success, no
// serverInfo redirect) and then close the TCP connection outright instead of ever sending `init` --
// the same "peer closes, giving ReadEnvelope a real EOF/reset" failure
// TestWaitForInitPushConnectionFailure forces directly on the helper, but here reached the only way
// Login()'s real callers ever would: through Login() itself.
func TestLoginConnectionFailureWhileWaitingForInit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		// Deliberately never send `init` -- close the connection outright instead, so the client's
		// step-5 wait (waitForInitPush) sees a real read error (EOF/reset), not a silence-until-
		// deadline timeout. A plain timeout would take waitForInitPush's full 45s (Login()'s
		// unexported initPushTimeout, not overridable from a test) and land Login() on the benign
		// "giving up... continuing anyway" path instead of the fail-fast path this test is for.
		server.Close()
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	start := time.Now()
	result, err := Login(LoginOptions{})
	elapsed := time.Since(start)

	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("Login: expected a non-nil error when the connection fails while waiting for the init push, got nil")
	}
	if result != nil {
		t.Errorf("Login: result = %+v, want nil *LoginResult alongside the error", result)
	}
	if !strings.Contains(err.Error(), "connection failed while waiting for init push") {
		t.Errorf("Login err = %q, want it to mention \"connection failed while waiting for init push\"", err.Error())
	}
	// Same rationale as TestWaitForInitPushConnectionFailure's own elapsed-time check: a connection
	// failure should surface promptly, not after waiting out the full 45s initPushTimeout window --
	// proving Login() actually took the initErr!=nil fail-fast branch rather than, say, coincidentally
	// timing out and then erroring for some unrelated reason. A bound of 45s itself would be toothless
	// here: in every way this fix could regress, the err==nil check above already fails the test long
	// before elapsed is ever computed against a 45s bound. 5s is generous for a local fake-server
	// round trip while still being far short of the full window, so it actually catches a "fell
	// through to the slow giving-up path but still happened to error for an unrelated reason" scenario
	// the err==nil check alone wouldn't.
	if elapsed > 5*time.Second {
		t.Errorf("Login took %v, want it to fail promptly on connection failure rather than waiting out the full init-push timeout window", elapsed)
	}
}

// TestLoginRedirectRefreshesGameUid is Login()'s counterpart to crossserver_test.go's
// TestDoCrossServerLoginRedirectRefreshesGameUid, exercising the mirrored fix this round added to
// Login()'s own serverInfo-redirect handling (see login.go, "gameUid changed on GSL refresh"): the
// mid-redirect GSL refresh (opt=fix, triggered because the base zone Login got redirected to a new
// shard) returns a serverList entry with a NEW gameUid, and that gameUid must end up both on the
// returned LoginResult.Ident and persisted to disk -- not left pinned to whatever the initial GSL
// call originally returned.
//
// Flow: a fresh device identity has no loginKey/gameUid, so the initial GSL call uses opt=new and
// points at a first fake game server. That server's Login response carries a serverInfo redirect
// to a second fake game server on a new zone. Following the redirect triggers a second GSL call
// (opt=fix, since the device now has a persisted gameUid from the first GSL response) -- this is
// the one that hands back the updated gameUid. The second fake game server then accepts the
// redialed Login and sends the init push, letting Login() return successfully as a guest.
func TestLoginRedirectRefreshesGameUid(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const oldGameUid = "uid-old"
	const newGameUid = "uid-new"

	gotSecondZone := make(chan string, 1)
	newAddr := startFakeGameServer(t, fakeInitPushServer(gotSecondZone))

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(newAddr, "APS2"))
	})
	oldHost, oldPort := splitHostPortInt(t, oldAddr)

	gsl := newFakeGSLServer(t,
		// Initial GSL call (opt=new): points at the first fake server, on the old gameUid.
		LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(oldHost), Port: flexPort(oldPort), Zone: "APS1", GameUid: oldGameUid}},
			At:         &LoginToken{Token: "tok-1"},
		},
		// Mid-redirect refresh (opt=fix): the account has since moved to a new gameUid -- this is
		// the value that must propagate into the persisted identity, per this round's fix.
		LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{GameUid: newGameUid}},
			At:         &LoginToken{Token: "tok-fresh"},
		},
	)
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	if result.Ident == nil {
		t.Fatal("result.Ident = nil")
	}
	if result.Ident.GameUid != newGameUid {
		t.Errorf("Ident.GameUid = %q, want %q (refreshed via GSL mid-redirect, not the stale %q from the initial call)",
			result.Ident.GameUid, newGameUid, oldGameUid)
	}

	// Confirm the refresh actually landed on disk too, not just on the in-memory Ident the
	// running process happened to hold -- the whole point of persisting gameUid is that the
	// *next* run picks it up (see identity.go's SaveGameUid / gslOptFor's opt=fix case).
	persisted, err := os.ReadFile(gameUidStatePath())
	if err != nil {
		t.Fatalf("read persisted gameUid: %v", err)
	}
	if got := strings.TrimSpace(string(persisted)); got != newGameUid {
		t.Errorf("persisted gameUid = %q, want %q", got, newGameUid)
	}

	select {
	case zn := <-gotSecondZone:
		if zn != "APS2" {
			t.Errorf("second server saw Login zn=%q, want %q (the redialed Login should use the post-redirect zone)", zn, "APS2")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}
}

// TestLoginTooManyRedirects is Login()'s counterpart to crossserver_test.go's
// TestDoCrossServerLoginTooManyRedirects, covering the identical bound on the other side of the
// "same gap" comment in login.go's serverInfo redirect block: maxRedirectHops+1 consecutive fake
// game servers all respond to the base zone Login with a serverInfo redirect to the next one in
// the chain. Login() must give up with the "too many serverInfo redirects" error rather than
// looping past the guard (or dialing the address nothing listens on past the last server in the
// chain -- the guard trips on receipt of the redirect, before any further dial).
func TestLoginTooManyRedirects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const maxRedirectHops = 3 // must match login.go's unexported maxRedirectHops const
	const numServers = maxRedirectHops + 1

	lns := make([]net.Listener, numServers)
	addrs := make([]string, numServers)
	for i := range lns {
		ln, addr := newFakeGameListener(t)
		lns[i] = ln
		addrs[i] = addr
	}
	for i, ln := range lns {
		serveFakeGameServer(ln, func(server *GameConn) {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
			// The last server in the chain "redirects" to an address nothing is listening on --
			// Login()'s redirect-count guard must trip before it ever tries to dial that far, so
			// this address is never actually connected to.
			next := "127.0.0.1:1"
			if i+1 < numServers {
				next = addrs[i+1]
			}
			zone := fmt.Sprintf("APS%d", i+1)
			_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(next, zone))
		})
	}

	host, port := splitHostPortInt(t, addrs[0])
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS0", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error after maxRedirectHops+1 consecutive serverInfo redirects, got nil")
	}
	if !strings.Contains(err.Error(), "too many serverInfo redirects") {
		t.Errorf("err = %q, want it to mention \"too many serverInfo redirects\"", err.Error())
	}
}

// TestLoginExactlyMaxRedirectHopsSucceeds is the round-42 regression test for the MINOR finding
// that TestLoginTooManyRedirects above only proves the guard trips on maxRedirectHops+1 redirects
// -- it never exercises the boundary itself, so a regression tightening login.go's
// `redirectHops > maxRedirectHops` to an off-by-one `redirectHops >= maxRedirectHops` would reject
// a legitimate maxRedirectHops-hop chain with zero test signal. Confirmed via mutation testing:
// that exact `>=` tightening passed TestLoginTooManyRedirects unchanged. Mirrors crossserver_test.go's
// TestDoCrossServerLoginExactlyMaxRedirectsSucceeds for Login()'s own, separate redirect-hop guard.
// Chains exactly maxRedirectHops consecutive redirects, the last of which completes a normal,
// non-redirect login success -- proving the guard's own boundary value is itself still followable.
func TestLoginExactlyMaxRedirectHopsSucceeds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const maxRedirectHops = 3              // must match login.go's unexported maxRedirectHops const
	const numServers = maxRedirectHops + 1 // servers 0..2 redirect; server 3 (the maxRedirectHops-th hop) succeeds

	lns := make([]net.Listener, numServers)
	addrs := make([]string, numServers)
	for i := range lns {
		ln, addr := newFakeGameListener(t)
		lns[i] = ln
		addrs[i] = addr
	}
	for i, ln := range lns {
		i := i
		serveFakeGameServer(ln, func(server *GameConn) {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
			if i+1 < numServers {
				zone := fmt.Sprintf("APS%d", i+1)
				_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(addrs[i+1], zone))
				return
			}
			// The last server in the chain (the maxRedirectHops-th hop) completes a normal
			// login instead of redirecting again -- this is the boundary value itself, which
			// must still be reachable, not rejected.
			resp := NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
			// Immediately follow with `init` so waitForInitPush returns right away instead of
			// silently riding out the full 45s initPushTimeout before Login() gives up and
			// returns anyway -- this test only cares about the redirect-hop boundary, not
			// init-push behavior.
			_ = server.SendExtension("init", NewSFSObject())
		})
	}

	host, port := splitHostPortInt(t, addrs[0])
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS0", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v, want it to succeed after exactly %d redirects (the boundary value)", err, maxRedirectHops)
	}
	defer result.Conn.Close()
}

// buildLoggableServerList returns an n-entry ServerList whose first entry is the real, reachable
// state server (host/port from a fake game listener, used for the actual connection) and whose
// remaining n-1 entries are unreachable placeholders that exist purely to pad ServerList's length
// -- login.go's Login only ever dials ServerList[0], so these are never connected to, only logged.
func buildLoggableServerList(host string, port int) []LoginServerInfo {
	list := make([]LoginServerInfo, maxServerListLogEntries+1)
	list[0] = LoginServerInfo{IP: flexString(host), Port: flexPort(port), Zone: "APS0", GameUid: "uid-1"}
	for i := 1; i < len(list); i++ {
		list[i] = LoginServerInfo{IP: "10.0.0.1", Port: flexPort(9999), Zone: "APS0", GameUid: flexString(fmt.Sprintf("uid-unreachable-%d", i))}
	}
	return list
}

// TestLoginServerListLogExactlyAtCapDoesNotTruncate is the boundary regression test for round 42's
// fix adding maxServerListLogEntries: Login()'s "state server" per-entry logging loop (login.go,
// right after the "GSL getserverlist response" log line) must log ALL entries, with no truncation
// warning, when lsr.ServerList's length is exactly at the cap -- proving the guard is a strict `>`,
// not an off-by-one `>=` that would truncate one entry early. Mirrors this round's sibling boundary
// tests (TestDoCrossServerLoginExactlyMaxRedirectsSucceeds, TestClaimAllMailRewardLoopExactlyAtCapDoesNotTruncate,
// TestCollectAllExactlyAtCapDoesNotTruncate, TestParseInitVisitorsMaxNumExactlyAtUpperBoundDoesNotClamp).
func TestLoginServerListLogExactlyAtCapDoesNotTruncate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, fakeInitPushServer(nil))
	host, port := splitHostPortInt(t, addr)

	serverList := buildLoggableServerList(host, port)[:maxServerListLogEntries]
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: serverList,
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	result, err := Login(LoginOptions{})
	slog.SetDefault(orig)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	logged := strings.Count(buf.String(), `msg="state server"`)
	if logged != maxServerListLogEntries {
		t.Errorf("logged %d \"state server\" lines, want %d (exactly at cap, no truncation)", logged, maxServerListLogEntries)
	}
	if strings.Contains(buf.String(), "truncating per-entry logging") {
		t.Errorf("log output unexpectedly contains a truncation warning at exactly the cap:\n%s", buf.String())
	}
}

// TestLoginServerListLogOverCapTruncatesAndWarns is the over-cap counterpart to
// TestLoginServerListLogExactlyAtCapDoesNotTruncate: when lsr.ServerList's length exceeds
// maxServerListLogEntries by one, Login() must log exactly maxServerListLogEntries "state server"
// lines (not one more), emit a truncation Warn, and -- critically -- still use ServerList[0] for the
// actual connection/GameUid, since the cap only bounds LOGGING, not which server Login() dials.
func TestLoginServerListLogOverCapTruncatesAndWarns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, fakeInitPushServer(nil))
	host, port := splitHostPortInt(t, addr)

	serverList := buildLoggableServerList(host, port)
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: serverList,
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	result, err := Login(LoginOptions{})
	slog.SetDefault(orig)
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	logged := strings.Count(buf.String(), `msg="state server"`)
	if logged != maxServerListLogEntries {
		t.Errorf("logged %d \"state server\" lines, want %d (truncated at cap)", logged, maxServerListLogEntries)
	}
	if !strings.Contains(buf.String(), "truncating per-entry logging") {
		t.Errorf("log output missing truncation warning for an over-cap ServerList:\n%s", buf.String())
	}
	if result.Ident.GameUid != "uid-1" {
		t.Errorf("Ident.GameUid = %q, want %q (login must still use ServerList[0] even though logging truncates)", result.Ident.GameUid, "uid-1")
	}
}

// readNextExtension reads envelopes off server until one decodes as an extension message,
// silently skipping anything else -- in practice the client's own heartbeat pingpong (system
// controller, sent every 4s per conn.go's StartHeartbeat), which TestLoginEmailVerificationPath's
// fake server would otherwise misread as the next expected extension request if the client's real
// FIFO round-trip for the verification code ever took long enough for a heartbeat to land in
// between. Mirrors waitFor's own "skip anything that doesn't match" loop on the client side.
func readNextExtension(server *GameConn) (*ExtensionMessage, error) {
	for {
		env, err := server.ReadEnvelope()
		if err != nil {
			return nil, err
		}
		if msg, ok := env.AsExtension(); ok {
			return msg, nil
		}
	}
}

// mkfifoT creates a FIFO at dir/name and returns its path, failing the test on error -- the real
// mechanism login.go's readCodeFromPipe expects for LoginOptions.CodePipe (it os.Stat's the path
// and requires os.ModeNamedPipe), not a plain file or in-memory reader.
func mkfifoT(t *testing.T, dir, name string) string {
	t.Helper()
	path := dir + "/" + name
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo %s: %v", path, err)
	}
	return path
}

// TestLoginEmailVerificationPath exercises Login()'s email-verification flow (steps 6-8: request
// a code via account.login.send.verify.code, block on LoginOptions.CodePipe for the code, then
// account.login.new followed by the account data arriving separately as a push.account.login.new
// push -- see login.go's step 6-8 comments), which neither TestLoginGuestHappyPath nor
// TestLoginRedirectRefreshesGameUid reaches, since both call Login() with no Email set and so
// always take the early guest-identity return instead.
//
// The verification code is delivered through a real FIFO (not an in-memory reader) because that's
// the only path LoginOptions.CodePipe actually drives (readCodeFromPipe os.Opens the path, which
// blocks until a writer appears) -- a background goroutine here plays that writer, opening the
// FIFO and writing the test code the moment Login() itself opens the read end, so the two
// naturally rendezvous without any test-side sleep/poll.
func TestLoginEmailVerificationPath(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"
	const wantLoginKey = "test-login-key"
	const wantGameUid = "real-uid-1"
	const wantUsername = "RealPlayer"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	gotSendCodeEmail := make(chan string, 1)
	gotFinishEmail := make(chan string, 1)
	gotFinishCode := make(chan string, 1)

	addr := startFakeGameServer(t, func(server *GameConn) {
		// Step 4: base zone Login -- plain success, no serverInfo redirect, immediately
		// followed by the `init` push (same fakeInitPushServer pattern used above) so step 5
		// completes fast and Login() falls through into the email-verification steps this
		// test actually cares about.
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

		// Step 6: account.login.send.verify.code request, then its ack.
		sendCodeMsg, err := readNextExtension(server)
		if err != nil {
			return
		}
		gotSendCodeEmail <- sendCodeMsg.Params.GetString("mail")
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		// Step 8: account.login.new (type=0, mail+code), then its terse ack.
		finishMsg, err := readNextExtension(server)
		if err != nil {
			return
		}
		gotFinishEmail <- finishMsg.Params.GetString("mail")
		gotFinishCode <- finishMsg.Params.GetString("verifyCode")
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		// The real account data arrives separately as a push, per login.go's own comment on
		// why msg2 (not the ack above) is what Login() actually reads gameUid/loginKey from.
		push := NewSFSObject()
		push.PutUtfString("loginKey", wantLoginKey)
		push.PutUtfString("gameUid", wantGameUid)
		push.PutUtfString("gameUserName", wantUsername)
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	// GameUid empty here (unlike the redirect tests) deliberately: a fresh device identity with
	// no loginKey/gameUid drives gslOptFor to opt=new, which is what keeps Login() off the
	// opt=="login" fast-path return and routes it into the email-verification steps below.
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	result, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	select {
	case got := <-gotSendCodeEmail:
		if got != testEmail {
			t.Errorf("account.login.send.verify.code mail = %q, want %q", got, testEmail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received an account.login.send.verify.code request")
	}
	select {
	case got := <-gotFinishEmail:
		if got != testEmail {
			t.Errorf("account.login.new mail = %q, want %q", got, testEmail)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received an account.login.new request")
	}
	select {
	case got := <-gotFinishCode:
		if got != testCode {
			t.Errorf("account.login.new verifyCode = %q, want %q (the code written to CodePipe)", got, testCode)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received an account.login.new request")
	}

	if result.Account == nil {
		t.Fatal("result.Account = nil, want the push.account.login.new params")
	}
	if got := result.Account.GetString("loginKey"); got != wantLoginKey {
		t.Errorf("result.Account loginKey = %q, want %q", got, wantLoginKey)
	}

	if result.Ident == nil {
		t.Fatal("result.Ident = nil")
	}
	if result.Ident.LoginKey != wantLoginKey {
		t.Errorf("Ident.LoginKey = %q, want %q", result.Ident.LoginKey, wantLoginKey)
	}
	if result.Ident.GameUid != wantGameUid {
		t.Errorf("Ident.GameUid = %q, want %q", result.Ident.GameUid, wantGameUid)
	}
	if result.Ident.Username != wantUsername {
		t.Errorf("Ident.Username = %q, want %q", result.Ident.Username, wantUsername)
	}

	// Confirm the account.login.new push's fields actually landed on disk too -- not just on
	// the in-memory Ident this process happens to hold -- same reload-from-disk check
	// TestLoginRedirectRefreshesGameUid uses for gameUid, extended here to loginKey/username
	// since this is the path that populates all three.
	if got, err := os.ReadFile(loginKeyStatePath()); err != nil {
		t.Fatalf("read persisted loginKey: %v", err)
	} else if strings.TrimSpace(string(got)) != wantLoginKey {
		t.Errorf("persisted loginKey = %q, want %q", strings.TrimSpace(string(got)), wantLoginKey)
	}
	if got, err := os.ReadFile(gameUidStatePath()); err != nil {
		t.Fatalf("read persisted gameUid: %v", err)
	} else if strings.TrimSpace(string(got)) != wantGameUid {
		t.Errorf("persisted gameUid = %q, want %q", strings.TrimSpace(string(got)), wantGameUid)
	}
	if got, err := os.ReadFile(usernameStatePath()); err != nil {
		t.Fatalf("read persisted username: %v", err)
	} else if strings.TrimSpace(string(got)) != wantUsername {
		t.Errorf("persisted username = %q, want %q", strings.TrimSpace(string(got)), wantUsername)
	}
}

// TestLoginWarnsOnWrongTypedPersistenceFields is the round-32 regression test for the
// TESTING-RIGOR finding that login.go's four warnIfWrongTypedField diagnostics on its
// persistence-read fields (the base-zone Login response's "un", and push.account.login.new's
// "loginKey"/"gameUid"/"gameUserName") had zero regression-test coverage -- confirmed via mutation
// (commenting out all four calls, the full suite still passed). Sends all four fields with the
// wrong SFS type (PutInt instead of PutUtfString) and asserts all four diagnostics fire, while
// Login() itself still completes successfully (a wrong-typed field degrades to "nothing to
// persist" for that field, not a fatal error) with none of the four fields actually persisted.
func TestLoginWarnsOnWrongTypedPersistenceFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		resp.PutInt("un", 111) // wrong-typed: a real "un" is always a UTF string, never a number
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		if err := server.SendExtension("init", NewSFSObject()); err != nil {
			return
		}

		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		if _, err := readNextExtension(server); err != nil {
			return
		}
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		// All three push fields wrong-typed too.
		push := NewSFSObject()
		push.PutInt("loginKey", 222)
		push.PutInt("gameUid", 333)
		push.PutInt("gameUserName", 444)
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (a wrong-typed persistence field must not be fatal -- it degrades to \"nothing to persist\" for that field)", err)
	}
	defer result.Conn.Close()

	logged := buf.String()
	for _, field := range []string{"un", "loginKey", "gameUid", "gameUserName"} {
		if !strings.Contains(logged, "field="+field) {
			t.Errorf("expected a wrong-typed warning mentioning field=%s, got log:\n%s", field, logged)
		}
	}

	if result.Ident.LoginKey != "" {
		t.Errorf("Ident.LoginKey = %q, want \"\" (wrong-typed loginKey must not be persisted)", result.Ident.LoginKey)
	}
	if result.Ident.GameUid != "" {
		t.Errorf("Ident.GameUid = %q, want \"\" (wrong-typed gameUid must not be persisted)", result.Ident.GameUid)
	}
	if result.Ident.Username != "" {
		t.Errorf("Ident.Username = %q, want \"\" (wrong-typed gameUserName must not be persisted)", result.Ident.Username)
	}
}

// TestLoginEmailVerificationPathWarnsOnPersistFailure is this round's regression test for the
// two SaveGameUid/SaveUsername calls at the very end of Login()'s email-verification success
// path (immediately after the push.account.login.new push arrives), which used to silently
// discard their errors via bare "_ = ident.SaveGameUid(gu)" / "_ = ident.SaveUsername(un)" --
// unlike every other SaveGameUid/SaveUsername/SaveLoginKey call site in login.go (including
// SaveLoginKey three lines above this same block), all of which check the error and slog.Warn on
// failure. A state-file write failure here (disk full, unwritable home dir, permission change
// mid-run) would previously vanish with no trace even though the login itself still succeeded.
//
// The failure is injected by replacing the gameUid/username state files with directories --
// the same "put a directory where the state file is expected" technique
// TestSaveStateFileLeavesTargetUntouchedOnFailedWrite and
// TestLoadOrCreateDeviceIdentityDoesNotClobberOnReadFailure (identity_test.go) use to force a
// real, root-proof rename failure without relying on permission bits -- but timed, from inside
// the fake server's handler, to appear only right before the push.account.login.new push is
// sent: strictly after loadOrCreateDeviceIdentity's own read of these same two paths at the top
// of Login() (which must see them absent/empty, or this would instead exercise the read-failure
// path identity_test.go already covers) and strictly before Login() reaches the
// SaveGameUid/SaveUsername calls the push triggers. mkdirResults reports the Mkdir outcomes back
// to the test goroutine rather than calling T methods from the handler goroutine directly --
// unsafe here per startFakeGameServer's own doc comment, since the handler goroutine can outlive
// the test.
func TestLoginEmailVerificationPathWarnsOnPersistFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"
	const wantGameUid = "real-uid-1"
	const wantUsername = "RealPlayer"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	mkdirResults := make(chan error, 2)

	addr := startFakeGameServer(t, func(server *GameConn) {
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

		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		if _, err := readNextExtension(server); err != nil {
			return
		}
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		// Swap in directories where the gameUid/username state files are expected. This runs
		// strictly after loadOrCreateDeviceIdentity's own read of these same paths (long since
		// completed, at the very top of Login(), before this connection was even dialed) and
		// strictly before the SaveGameUid/SaveUsername calls the push below triggers.
		mkdirResults <- os.Mkdir(gameUidStatePath(), 0700)
		mkdirResults <- os.Mkdir(usernameStatePath(), 0700)

		push := NewSFSObject()
		push.PutUtfString("loginKey", "test-login-key")
		push.PutUtfString("gameUid", wantGameUid)
		push.PutUtfString("gameUserName", wantUsername)
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})

	slog.SetDefault(orig)

	// The persist failure is a robustness warning, not a fatal error -- the SFS zone login and
	// email verification themselves fully succeeded, so Login() must still return success.
	if err != nil {
		t.Fatalf("Login: %v, want success (a SaveGameUid/SaveUsername persist failure must only warn, not fail the login)", err)
	}
	defer result.Conn.Close()

	// Confirm the fake server's own Mkdir calls actually succeeded (test setup), so a failure
	// below is known to come from the SaveGameUid/SaveUsername warnings under test, not from the
	// directories never having been put in place at all.
	for i := 0; i < 2; i++ {
		select {
		case mkErr := <-mkdirResults:
			if mkErr != nil {
				t.Fatalf("test setup: mkdir state path: %v", mkErr)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("fake server never reported its Mkdir results")
		}
	}

	if result.Ident == nil {
		t.Fatal("result.Ident = nil")
	}
	// The in-memory identity is still updated even though the on-disk persist failed --
	// SaveGameUid/SaveUsername set the struct field before attempting the write.
	if result.Ident.GameUid != wantGameUid {
		t.Errorf("Ident.GameUid = %q, want %q", result.Ident.GameUid, wantGameUid)
	}
	if result.Ident.Username != wantUsername {
		t.Errorf("Ident.Username = %q, want %q", result.Ident.Username, wantUsername)
	}

	logged := buf.String()
	if !strings.Contains(logged, "failed to persist gameUid") {
		t.Errorf("Login()'s logged output is missing a \"failed to persist gameUid\" warning for the failed SaveGameUid call:\n%s", logged)
	}
	if !strings.Contains(logged, "failed to persist username") {
		t.Errorf("Login()'s logged output is missing a \"failed to persist username\" warning for the failed SaveUsername call:\n%s", logged)
	}

	// Both warnings must actually be logged at slog.Warn level via the "error" attribute (the
	// same shape as every sibling SaveGameUid/SaveUsername/SaveLoginKey warning in login.go),
	// not just happen to contain the right substring some other way.
	if !strings.Contains(logged, "level=WARN") {
		t.Errorf("Login()'s logged output is missing a WARN-level line for the persist failures:\n%s", logged)
	}

	// Confirm the state paths are still directories, untouched by the failed save attempts --
	// same untouched-on-failure contract TestSaveStateFileLeavesTargetUntouchedOnFailedWrite
	// proves for saveStateFile in isolation.
	if fi, statErr := os.Stat(gameUidStatePath()); statErr != nil || !fi.IsDir() {
		t.Errorf("gameUid state path is no longer a directory after the failed SaveGameUid call (statErr=%v)", statErr)
	}
	if fi, statErr := os.Stat(usernameStatePath()); statErr != nil || !fi.IsDir() {
		t.Errorf("username state path is no longer a directory after the failed SaveUsername call (statErr=%v)", statErr)
	}
}

// TestLoginEmailVerificationPushErrorDoesNotLeakLoginKey proves the push.account.login.new
// errorCode-present branch doesn't leak the response's cleartext loginKey into the returned
// error (and therefore into main.go's slog.Error("login failed", ...) call site) -- a real,
// live credential-leak bug found and fixed this round: the errorCode branch dumped the raw
// response via msg2.Params.String() two lines above the exact comment warning against doing
// that for this specific message type.
func TestLoginEmailVerificationPushErrorDoesNotLeakLoginKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	addr := startFakeGameServer(t, func(server *GameConn) {
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

		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		if _, err := readNextExtension(server); err != nil {
			return
		}
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		// The push carries both a rejection (errorCode) AND the same cleartext loginKey field a
		// successful push would -- proving the errorCode branch must redact it too, not just the
		// success branch.
		push := NewSFSObject()
		push.PutUtfString("errorCode", "999999")
		push.PutUtfString("loginKey", secretLoginKey)
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	_, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})
	if err == nil {
		t.Fatal("Login: expected an error from the rejected push.account.login.new, got nil")
	}
	if !errors.Is(err, ErrAuthRejected) {
		t.Errorf("Login error does not satisfy errors.Is(err, ErrAuthRejected): %v", err)
	}
	if strings.Contains(err.Error(), secretLoginKey) {
		t.Errorf("Login error leaks the raw loginKey in cleartext: %v", err)
	}
}

// TestLoginRedactsCodeDeviceIdAndAirKeyInLogs is the round-12 regression test for two credential-
// leak fixes in Login() itself (not a downstream error path): (1) the "got code" log used to
// include the raw one-time email-verification code in cleartext at default Info level on every
// email login, immediately before that same code is used to complete account.login.new (fixed to
// log codeLen instead); and (2) the "device identity"/"air key" logs at the very top of Login()
// used to include the raw deviceId/airKey in cleartext, unconditionally, on every single Login()
// call regardless of flow -- guest, email, or config reconnect (fixed to log deviceIdLen/
// airKeyLen instead, matching the style main.go's runCrossServerTest already uses for the
// identical two values).
//
// The email-verification path is used here (rather than the guest happy path) because it's the
// only flow that also exercises the verification code, letting one test cover both fixes: deviceId
// and airKey are logged right at the top of Login(), before any network I/O, and the code is
// logged in step 7 then reused on the wire in step 8. Log output is captured at Info level -- the
// default in production -- since both bugs fired unconditionally at that level, with no
// -log-level debug needed for either to reproduce.
func TestLoginRedactsCodeDeviceIdAndAirKeyInLogs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	addr := startFakeGameServer(t, func(server *GameConn) {
		// Step 4: base zone Login -- plain success, immediately followed by the `init` push, same
		// as TestLoginEmailVerificationPath, so Login() falls through into the email-verification
		// steps this test cares about.
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

		// Step 6: account.login.send.verify.code request, then its ack.
		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		// Step 8: account.login.new (type=0, mail+code+deviceId+airKey), then its terse ack.
		if _, err := readNextExtension(server); err != nil {
			return
		}
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		push := NewSFSObject()
		push.PutUtfString("loginKey", "test-login-key")
		push.PutUtfString("gameUid", "real-uid-1")
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	// GameUid empty deliberately (same as TestLoginEmailVerificationPath): drives gslOptFor to
	// opt=new, which keeps Login() off the opt=="login" fast-path return and routes it into the
	// email-verification steps below.
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	if result.Ident == nil {
		t.Fatal("result.Ident = nil")
	}
	// The device identity is freshly generated under this test's isolated HOME (t.Setenv above),
	// so the raw values to check for are only known after Login() returns them on result.Ident --
	// read them back off the live result rather than hardcoding a value the fix wouldn't otherwise
	// be exercised against.
	rawDeviceID := result.Ident.DeviceID
	rawAirKey := result.Ident.AirKey()
	if rawDeviceID == "" || rawAirKey == "" {
		t.Fatalf("test setup: DeviceID/AirKey unexpectedly empty (DeviceID=%q, AirKey=%q)", rawDeviceID, rawAirKey)
	}

	logged := buf.String()

	if strings.Contains(logged, testCode) {
		t.Errorf("Login()'s logged output leaks the raw verification code %q in cleartext:\n%s", testCode, logged)
	}
	if !strings.Contains(logged, "codeLen") {
		t.Errorf("Login()'s logged output is missing the codeLen replacement key -- the \"got code\" log line may not have fired at all:\n%s", logged)
	}

	if strings.Contains(logged, rawDeviceID) {
		t.Errorf("Login()'s logged output leaks the raw deviceId %q in cleartext:\n%s", rawDeviceID, logged)
	}
	if !strings.Contains(logged, "deviceIdLen") {
		t.Errorf("Login()'s logged output is missing the deviceIdLen replacement key -- the \"device identity\" log line may not have fired at all:\n%s", logged)
	}

	if strings.Contains(logged, rawAirKey) {
		t.Errorf("Login()'s logged output leaks the raw airKey %q in cleartext:\n%s", rawAirKey, logged)
	}
	if !strings.Contains(logged, "airKeyLen") {
		t.Errorf("Login()'s logged output is missing the airKeyLen replacement key -- the \"air key\" log line may not have fired at all:\n%s", logged)
	}
}

// TestLoginRedactsEmailInLogs is this round's regression test for a third credential-leak fix in
// Login()'s email-verification flow, sitting alongside the code/deviceId/airKey fixes
// TestLoginRedactsCodeDeviceIdAndAirKeyInLogs already covers: two log lines --
// "sent account.login.send.verify.code" and "verification code should now be arriving" -- used to
// log opts.Email directly via a plain "email" slog attribute, in cleartext, at default Info level
// on every email-verification login. This is a raw Go string field, so it entirely bypassed the
// SFSObject-level redaction system (which only ever sees opts.Email once it's already been put onto
// an SFSObject as "mail", a separate, non-overlapping instance of the same value -- see
// sfsobject.go's sensitiveSFSKeys). Fixed to log emailLen instead, matching this exact function's
// own established pattern for DeviceID/AirKey just above.
//
// Mirrors TestLoginRedactsCodeDeviceIdAndAirKeyInLogs's structure: same fake server flow, same
// Info-level capture via a swapped slog.Default(), same email-verification path (the only flow that
// exercises these two log lines at all).
func TestLoginRedactsEmailInLogs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"

	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	addr := startFakeGameServer(t, func(server *GameConn) {
		// Step 4: base zone Login -- plain success, immediately followed by the `init` push, same
		// as TestLoginEmailVerificationPath, so Login() falls through into the email-verification
		// steps this test cares about.
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

		// Step 6: account.login.send.verify.code request, then its ack -- this is the request whose
		// "sent account.login.send.verify.code" log line is the first of the two under test.
		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		if err := server.SendExtension("account.login.send.verify.code", ack); err != nil {
			return
		}

		// Step 8: account.login.new (type=0, mail+code+deviceId+airKey), then its terse ack.
		if _, err := readNextExtension(server); err != nil {
			return
		}
		finishAck := NewSFSObject()
		finishAck.PutBool("success", true)
		if err := server.SendExtension("account.login.new", finishAck); err != nil {
			return
		}

		push := NewSFSObject()
		push.PutUtfString("loginKey", "test-login-key")
		push.PutUtfString("gameUid", "real-uid-1")
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := splitHostPortInt(t, addr)

	// GameUid empty deliberately (same as TestLoginEmailVerificationPath): drives gslOptFor to
	// opt=new, which keeps Login() off the opt=="login" fast-path return and routes it into the
	// email-verification steps below.
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer result.Conn.Close()

	logged := buf.String()

	if strings.Contains(logged, testEmail) {
		t.Errorf("Login()'s logged output leaks the raw email %q in cleartext:\n%s", testEmail, logged)
	}
	wantEmailLen := fmt.Sprintf("emailLen=%d", len(testEmail))
	if !strings.Contains(logged, wantEmailLen) {
		t.Errorf("Login()'s logged output is missing %q -- the email-verification log lines may not have fired, or logged the wrong length:\n%s", wantEmailLen, logged)
	}
}

// TestLoginRedirectWrongTypedIPIsWarned is the round-30 regression test for the TESTING-RIGOR
// finding that login.go's OWN base-zone Login() call site of redirectIP had no wrong-typed-ip
// regression test -- only crossserver.go's sibling call site did
// (TestDoCrossServerLoginRedirectWrongTypedIPIsWarned, crossserver_test.go). Mirrors that test's
// technique exactly, but end to end through Login() itself: the fake game server's Login response
// carries a serverInfo whose ip field is the WRONG SFS type (PutInt instead of PutUtfString), then
// immediately follows up with the `init` push so Login() completes fast instead of waiting out its
// real 45s initPushTimeout. Proves (1) Login() does not treat the wrong-typed ip as fatal -- it
// still completes successfully, since the response is otherwise a normal (non-redirect) success --
// and (2) a Warn mentioning the wrong-typed ip field is logged, not silence, and (3) no redirect
// was actually followed (no "reconnecting to new address" log line).
func TestLoginRedirectWrongTypedIPIsWarned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutInt("ip", 12345) // wrong-typed: a real ip is always a UTF string, never a number
		si.PutInt("port", 9339)
		si.PutUtfString("zone", "APS2")
		resp := NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		// The wrong-typed ip must not be treated as a redirect, so Login() falls through to
		// step 5's init-push wait -- send it right away so this test doesn't wait out the real
		// 45s initPushTimeout.
		_ = server.SendExtension("init", NewSFSObject())
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (a wrong-typed ip must not be treated as a fatal error -- it degrades to \"no redirect\", same as a genuinely absent ip)", err)
	}
	defer result.Conn.Close()

	logged := buf.String()
	if !strings.Contains(logged, "wrong-typed") || !strings.Contains(logged, "ip") {
		t.Errorf("expected a Warn log mentioning the wrong-typed ip field, got:\n%s", logged)
	}
	if strings.Contains(logged, "reconnecting to new address") {
		t.Errorf("expected no serverInfo redirect to have been followed (the wrong-typed ip can't resolve to one), but the log shows one was:\n%s", logged)
	}
}

// TestLoginRedirectWrongTypedZoneIsWarned is the round-31 regression test for the MINOR
// testing-rigor finding that login.go's own base-zone Login() redirect branch had no wrong-typed-
// zone regression test -- only crossserver.go's sibling branch did
// (TestDoCrossServerLoginRedirectWrongTypedZoneIsWarned, crossserver_test.go), even though both
// call the identical shared redirectZone helper (login.go). Mirrors that test's technique end to
// end through Login(): the first fake server's serverInfo carries a WELL-typed ip/port (so the
// redirect is actually followed) but a WRONG-typed zone (PutInt instead of PutUtfString), and a
// second fake server (fakeInitPushServer) completes the redialed login. Proves (1) the redirect to
// the new address is still followed despite the wrong-typed zone, (2) the zone itself stays at its
// pre-redirect value (the desync risk redirectZone's own doc comment describes: a followed ip/port
// redirect paired with a silently-stale zone), and (3) a Warn mentioning the wrong-typed zone is
// logged, not silence.
func TestLoginRedirectWrongTypedZoneIsWarned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	gotSecondZone := make(chan string, 1)
	newAddr := startFakeGameServer(t, fakeInitPushServer(gotSecondZone))
	newHost, newPort := splitHostPortInt(t, newAddr)

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutUtfString("ip", newHost) // well-typed: the redirect must still be followed
		si.PutInt("port", int32(newPort))
		si.PutInt("zone", 999) // wrong-typed: a real zone is always a UTF string, never a number
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

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (a wrong-typed zone must not be a fatal error -- the redirect itself still follows on the well-typed ip/port)", err)
	}
	defer result.Conn.Close()

	select {
	case zn := <-gotSecondZone:
		if zn != "APS1" {
			t.Errorf("second server saw Login zn=%q, want %q (unchanged/stale -- the wrong-typed zone can't overwrite it, which is exactly the desync risk this test documents: a followed ip/port redirect paired with a stale zone)", zn, "APS1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}

	logged := buf.String()
	if !strings.Contains(logged, "wrong-typed") || !strings.Contains(logged, "zone") {
		t.Errorf("expected a Warn log mentioning the wrong-typed zone field, got:\n%s", logged)
	}
	if !strings.Contains(logged, "reconnecting to new address") {
		t.Errorf("expected the serverInfo redirect to still have been followed (ip/port are well-typed, only zone is wrong-typed), but the log shows none was:\n%s", logged)
	}
}

// TestLoginRejectsOversizedInitialZoneAndAccessTok is the round-47 regression test for the MAJOR
// finding that Login()'s initial zone and accessTok -- read directly off the GSL
// getserverlist.php JSON response's flexString fields, bounded only by the 1MiB whole-response
// cap, not any per-field limit -- were re-encoded via PutUtfString with no length check at all,
// unlike loginKey/gameUid/username which got exactly this guard (maxIdentityFieldLen) in round
// 46. See capOversizedIdentityField's doc comment (login.go) for the full mechanism this closes.
// Proves an oversized zone/accessTok from the GSL response falls back to "" instead of ever
// reaching PutUtfString, Login() still succeeds end to end (dialing only needs stateSrv.IP, not
// zone or the token), and a Warn is logged for each.
func TestLoginRejectsOversizedInitialZoneAndAccessTok(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	gotZn := make(chan string, 1)
	addr := startFakeGameServer(t, fakeInitPushServer(gotZn))
	host, port := splitHostPortInt(t, addr)

	oversizedZone := flexString(strings.Repeat("z", maxIdentityFieldLen+1))
	oversizedTok := flexString(strings.Repeat("t", maxIdentityFieldLen+1))

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: oversizedZone, GameUid: "uid-1"}},
		At:         &LoginToken{Token: oversizedTok},
	})
	useFakeGSLServer(t, gsl)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (an oversized zone/accessTok must fall back to \"\", not abort the login)", err)
	}
	defer result.Conn.Close()

	select {
	case zn := <-gotZn:
		if zn != "" {
			t.Errorf("Login sent zn=%q, want \"\" (the oversized zone must fall back to empty, not reach PutUtfString)", zn)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received a Login request")
	}

	logged := buf.String()
	if !strings.Contains(logged, "zone exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized zone, got:\n%s", logged)
	}
	if !strings.Contains(logged, "accessTok exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized accessTok, got:\n%s", logged)
	}
}

// TestLoginRedirectOversizedZoneIsWarned is TestLoginRedirectWrongTypedZoneIsWarned's sibling for
// an oversized (rather than wrong-typed) serverInfo redirect zone field: redirectZone's own
// capOversizedIdentityField guard (login.go) must reject a well-typed but over-65535-byte zone
// the identical way it rejects a wrong-typed one -- falling back to "" so the caller's existing
// `if newZone != "" { zone = newZone }` guard keeps the stale pre-redirect zone instead of ever
// reaching PutUtfString with an unencodable value. The oversized zone is written directly via
// SFSObject.put with the sfsText wire tag (mirroring mail_orchestration_test.go's technique for
// mail.go's uid/lastUid fields) since PutUtfString itself would fail to encode a >65535-byte
// string.
func TestLoginRedirectOversizedZoneIsWarned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	gotSecondZone := make(chan string, 1)
	newAddr := startFakeGameServer(t, fakeInitPushServer(gotSecondZone))
	newHost, newPort := splitHostPortInt(t, newAddr)

	oversizedZone := strings.Repeat("z", maxIdentityFieldLen+1)
	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutUtfString("ip", newHost) // well-typed: the redirect must still be followed
		si.PutInt("port", int32(newPort))
		si.put("zone", SFSValue{sfsText, oversizedZone}) // well-typed, but one byte over the wire cap
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

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (an oversized zone must not be a fatal error -- the redirect itself still follows on the well-typed ip/port)", err)
	}
	defer result.Conn.Close()

	select {
	case zn := <-gotSecondZone:
		if zn != "APS1" {
			t.Errorf("second server saw Login zn=%q, want %q (unchanged/stale -- the oversized zone can't overwrite it)", zn, "APS1")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}

	logged := buf.String()
	if !strings.Contains(logged, "zone exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized zone, got:\n%s", logged)
	}
	if !strings.Contains(logged, "reconnecting to new address") {
		t.Errorf("expected the serverInfo redirect to still have been followed (ip/port are well-typed, only zone is oversized), but the log shows none was:\n%s", logged)
	}
}

// TestLoginRedirectRefreshKeepsOldAccessTokWhenOversized covers the third of the three accessTok
// call sites the round-47 fix closes (login.go:246 initial assignment, covered by
// TestLoginRejectsOversizedInitialZoneAndAccessTok above; crossserver.go:267, covered by
// TestDoCrossServerLoginRedirectRefreshKeepsOldValuesWhenOversized): the mid-redirect GSL refresh
// (opt=fix) fetched before following a serverInfo redirect. An oversized refreshed token must fall
// back to the PREVIOUS access token (capOversizedIdentityField, login.go) instead of ever reaching
// PutUtfString with a value writeUtfString would hard-reject. newFakeGSLServer's variadic
// responses let the first (initial) and second (mid-redirect refresh) GetServerList calls answer
// differently.
func TestLoginRedirectRefreshKeepsOldAccessTokWhenOversized(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const oldAccessTok = "tok-1"
	oversizedAccessTok := flexString(strings.Repeat("t", maxIdentityFieldLen+1))

	gotParamsAt := make(chan string, 1)
	newAddr := startFakeGameServer(t, func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if pv, ok := env.Content.Get("p"); ok {
			if pObj, ok := pv.Val.(*SFSObject); ok {
				gotParamsAt <- pObj.GetString("at")
			}
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", NewSFSObject())
	})
	newHost, newPort := splitHostPortInt(t, newAddr)

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(newAddr, "APS2"))
	})
	oldHost, oldPort := splitHostPortInt(t, oldAddr)

	gsl := newFakeGSLServer(t,
		LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(oldHost), Port: flexPort(oldPort), Zone: "APS1", GameUid: "uid-1"}},
			At:         &LoginToken{Token: oldAccessTok},
		},
		LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{IP: flexString(newHost), Port: flexPort(newPort), Zone: "APS2", GameUid: "uid-1"}},
			At:         &LoginToken{Token: oversizedAccessTok},
		},
	)
	useFakeGSLServer(t, gsl)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := Login(LoginOptions{})

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("Login: %v (an oversized refreshed accessTok must fall back to the previous token, not fail the login)", err)
	}
	defer result.Conn.Close()

	select {
	case at := <-gotParamsAt:
		if at != oldAccessTok {
			t.Errorf("post-redirect Login params.at = %q, want %q (the stale, still-valid access token)", at, oldAccessTok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}

	logged := buf.String()
	if !strings.Contains(logged, "accessTok exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized refreshed accessTok, got:\n%s", logged)
	}
}

// TestLoginBaseZoneSendFailureIsNonTimeoutNetError is the round-30 regression test for the
// TESTING-RIGOR finding that login.go's base-zone Login() send (the conn.SendEnvelope call
// wrapped in sendStageError, conn.go) had zero test coverage -- a coverage profile showed
// execution count 0 for that block despite the wrapping already being in place since round 29.
// This is the FIRST write Login() ever issues on a freshly dialed connection, so
// withFailingDial(t, 0, ...) (crossserver_test.go) makes it fail deterministically. Mirrors
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError's / TestDoHandshakeSendFailureIsNon
// TimeoutNetError's assertions exactly (conn_wait_test.go/conn_handshake_test.go): the returned
// error must wrap the injected failure (errors.Is) and satisfy net.Error with
// Timeout()==false/Temporary()==false even though the injected failure itself reports
// Timeout()==true -- proving sendStageError's wrapping, not a bare passthrough.
func TestLoginBaseZoneSendFailureIsNonTimeoutNetError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// Idle handler: the fake server never receives anything, since the client's very first send
	// (the base-zone Login itself) fails before the packet ever reaches the network.
	addr := startFakeGameServer(t, func(server *GameConn) {})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	writeErr := fakeWriteFailError{msg: "simulated write-deadline-exceeded failure"}
	withFailingDial(t, 0, writeErr)

	_, err := Login(LoginOptions{})
	if err == nil {
		t.Fatal("expected an error when the base-zone Login send itself fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a send-stage failure must be distinguishable from Login()'s own benign wait-stage timeouts, even though the underlying write error itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write)")
	}
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestLoginSendVerifyCodeSendFailureIsNonTimeoutNetError is TestLoginBaseZoneSendFailure
// IsNonTimeoutNetError's sibling for login.go's account.login.send.verify.code send (step 6):
// unlike the base-zone Login send, this one only runs after the base-zone Login send has already
// succeeded and step 5's init-push wait has completed -- so withFailingDial(t, 1, ...) lets that
// one earlier write through unchanged and fails only the next one. The fake server accepts and
// answers the base-zone Login (sending `init` right away so step 5 doesn't wait out its real 45s
// timeout) but never receives an account.login.send.verify.code request: the client's send for it
// fails before the packet ever reaches the network.
func TestLoginSendVerifyCodeSendFailureIsNonTimeoutNetError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(controllerSystem, actionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", NewSFSObject())
	})
	host, port := splitHostPortInt(t, addr)

	// GameUid empty here, same as TestLoginEmailVerificationPath: a fresh device identity with no
	// loginKey/gameUid drives gslOptFor to opt=new, which keeps Login() off the opt=="login"
	// fast-path return and routes it into the email-verification steps this test needs to reach.
	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	writeErr := fakeWriteFailError{msg: "simulated write-deadline-exceeded failure"}
	withFailingDial(t, 1, writeErr)

	_, err := Login(LoginOptions{Email: "player@example.com"})
	if err == nil {
		t.Fatal("expected an error when the account.login.send.verify.code send itself fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false")
	}
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}

// TestLoginAccountLoginNewSendFailureIsNonTimeoutNetError is TestLoginSendVerifyCodeSendFailure
// IsNonTimeoutNetError's sibling for login.go's account.login.new send (step 8): this one only
// runs after BOTH the base-zone Login send and the account.login.send.verify.code send have
// already succeeded (and the verification code has been read back off CodePipe), so
// withFailingDial(t, 2, ...) lets those two earlier writes through unchanged and fails only the
// third. The fake server drives the flow all the way up to (but not through) account.login.new:
// base-zone Login + immediate `init` push, then a real account.login.send.verify.code
// request/ack round trip -- account.login.new itself is never received, since the client's send
// for it fails before the packet ever reaches the network.
func TestLoginAccountLoginNewSendFailureIsNonTimeoutNetError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	const testEmail = "player@example.com"
	const testCode = "654321"
	pipePath := mkfifoT(t, t.TempDir(), "code.pipe")

	addr := startFakeGameServer(t, func(server *GameConn) {
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

		if _, err := readNextExtension(server); err != nil {
			return
		}
		ack := NewSFSObject()
		ack.PutBool("success", true)
		_ = server.SendExtension("account.login.send.verify.code", ack)
	})
	host, port := splitHostPortInt(t, addr)

	gsl := newFakeGSLServer(t, LoginServerListRespon{
		Code:       "0",
		ServerList: []LoginServerInfo{{IP: flexString(host), Port: flexPort(port), Zone: "APS1", GameUid: ""}},
		At:         &LoginToken{Token: "tok-1"},
	})
	useFakeGSLServer(t, gsl)

	go func() {
		f, err := os.OpenFile(pipePath, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer f.Close()
		_, _ = f.WriteString(testCode + "\n")
	}()

	writeErr := fakeWriteFailError{msg: "simulated write-deadline-exceeded failure"}
	withFailingDial(t, 2, writeErr)

	_, err := Login(LoginOptions{Email: testEmail, CodePipe: pipePath})
	if err == nil {
		t.Fatal("expected an error when the account.login.new send itself fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false")
	}
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}
