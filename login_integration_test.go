package main

import (
	"encoding/json"
	"errors"
	"fmt"
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
		Code:       0,
		ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS0", GameUid: "uid-1"}},
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
		Code:       0,
		ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: ""}},
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
		Code:       0,
		ServerList: []LoginServerInfo{{IP: host, Port: port, Zone: "APS1", GameUid: ""}},
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
