package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
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
			_ = json.NewEncoder(w).Encode(CheckVersionResponse{ResMsg: pub})
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
		Code:       0,
		ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: "uid-1"}},
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
			Code:       0,
			ServerList: []LoginServerInfo{{IP: oldHost, Port: oldPort, Zone: "APS1", GameUid: oldGameUid}},
			At:         &LoginToken{Token: "tok-1"},
		},
		// Mid-redirect refresh (opt=fix): the account has since moved to a new gameUid -- this is
		// the value that must propagate into the persisted identity, per this round's fix.
		LoginServerListRespon{
			Code:       0,
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
