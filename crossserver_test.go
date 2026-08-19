package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// DoCrossServerLogin dials its own connection (via DialGame, a plain net.DialTimeout -- see
// conn.go) rather than accepting a pre-built GameConn, so it can't be exercised over net.Pipe the
// way conn_wait_test.go's newPipeGameConnPair-based tests are. Instead these tests spin up a real
// net.Listen-based fake SFS2X server on 127.0.0.1 and let DoCrossServerLogin dial it for real.
// The fake servers below only implement the sliver of the protocol DoCrossServerLogin's own
// read/write path (conn.go's SendEnvelope/ReadEnvelope) actually needs: a single {c,a,p} system
// Login response, optionally carrying a `serverInfo` redirect.

// newFakeGameListener opens a TCP listener on 127.0.0.1 for a fake game server and registers
// t.Cleanup to close it. Split out from serveFakeGameServer (rather than one combined
// "listen and serve" call) so a test that needs to know an address before it can build the
// handler for a DIFFERENT listener -- e.g. a serverInfo redirect chain, where each server's
// response embeds the NEXT server's address -- can open every listener (addresses are known the
// instant Listen returns, no Accept required) before wiring up any handlers.
func newFakeGameListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, ln.Addr().String()
}

// serveFakeGameServer runs handler, each in its own goroutine, once per connection accepted on
// ln, until ln.Close() (via newFakeGameListener's t.Cleanup) breaks the Accept loop at test end.
// Takes no *testing.T deliberately: like conn_wait_test.go's readAndReply, handler may still be
// running in the background after the test function itself has returned, and calling T methods
// from such a goroutine is unsafe.
func serveFakeGameServer(ln net.Listener, handler func(*GameConn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			gc := &GameConn{conn: c, reader: bufio.NewReaderSize(c, 4096)}
			go handler(gc)
		}
	}()
}

// startFakeGameServer covers the common single-listener case: listen and serve immediately.
func startFakeGameServer(t *testing.T, handler func(*GameConn)) string {
	t.Helper()
	ln, addr := newFakeGameListener(t)
	serveFakeGameServer(ln, handler)
	return addr
}

// splitHostPortInt parses a "host:port" address (as returned by net.Listener.Addr().String())
// into DoCrossServerLogin's IP/Port shape.
func splitHostPortInt(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

// putRedirectServerInfo builds a system Login response carrying a `serverInfo` redirect to addr
// (best-effort host/port parsing -- called from background handler goroutines, so it can't use
// *testing.T; a malformed addr just yields a redirect a real client would itself fail to follow,
// which is not a case these tests construct).
func putRedirectServerInfo(addr, zone string) *SFSObject {
	host, portStr, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portStr)
	si := NewSFSObject()
	si.PutUtfString("ip", host)
	si.PutInt("port", int32(port))
	si.PutUtfString("zone", zone)
	resp := NewSFSObject()
	resp.PutSFSObject("serverInfo", si)
	return resp
}

// TestDoCrossServerLoginNoRedirect covers the plain path: the fake server's Login response
// carries no serverInfo, so DoCrossServerLogin should return success on the very first
// connection with none of the redirect-related fields (Addr/Zone/AccessTok) altered from what
// was dialed/sent.
func TestDoCrossServerLoginNoRedirect(t *testing.T) {
	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	host, port := splitHostPortInt(t, addr)

	p := CrossServerLoginParams{
		IP:        host,
		Port:      port,
		Zone:      "APS1",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
	}
	result, err := DoCrossServerLogin(p)
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	if result.AccessTok != "tok-1" {
		t.Errorf("AccessTok = %q, want %q (unchanged -- no redirect happened)", result.AccessTok, "tok-1")
	}
	if result.Addr != addr {
		t.Errorf("Addr = %q, want %q (the dialed address)", result.Addr, addr)
	}
	if result.Zone != "APS1" {
		t.Errorf("Zone = %q, want %q (the input zone, unchanged)", result.Zone, "APS1")
	}
	if result.Content == nil {
		t.Error("Content = nil, want the server's Login response payload")
	}
}

// TestDoCrossServerLoginDebugDumpRedactsCredentials is the round-11 regression test for the
// LWDEBUG_DUMP_LOGIN debug dump (crossserver.go, "full login content"): it used to log the full
// outgoing login SFSObject raw via loginContent.String(), which carries the live access token
// (p.at) and shumeiBoxId in cleartext -- inconsistent with this same function's sibling
// LWDEBUG_DUMP_LOGIN_BODY dump (0600-permissioned, explicitly treated as sensitive) and its own
// later Info log a few lines down (which already redacts the identical two fields). Proves the
// debug dump's captured output never contains the raw access token or shumeiBoxId.
func TestDoCrossServerLoginDebugDumpRedactsCredentials(t *testing.T) {
	t.Setenv("LWDEBUG_DUMP_LOGIN", "1")

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	host, port := splitHostPortInt(t, addr)

	const secretAccessTok = "sensitive-secret-accesstok-must-not-leak-1234567890"
	const secretShumeiBoxId = "sensitive-secret-shumeiboxid-must-not-leak-0987654321"

	p := CrossServerLoginParams{
		IP:          host,
		Port:        port,
		Zone:        "APS1",
		GameUid:     "uid-1",
		DeviceID:    "dev-1",
		AirKey:      "airkey-1",
		AccessTok:   secretAccessTok,
		ShumeiBoxId: secretShumeiBoxId,
	}

	var buf bytes.Buffer
	orig := slog.Default()
	// Debug level explicitly enabled -- LWDEBUG_DUMP_LOGIN's dump is a slog.Debug call, which a
	// default-level (Info) handler would never emit, making this test pass vacuously either way.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	logged := buf.String()
	if !strings.Contains(logged, "full login content") {
		t.Fatal("expected the LWDEBUG_DUMP_LOGIN debug dump to have fired, but no \"full login content\" log line was captured")
	}
	if strings.Contains(logged, secretAccessTok) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump leaks the raw access token in cleartext:\n%s", logged)
	}
	if strings.Contains(logged, secretShumeiBoxId) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump leaks the raw shumeiBoxId in cleartext:\n%s", logged)
	}
}

// TestDoCrossServerLoginDebugDumpBodyFileIsChmodded0600 is the round-13 regression test for
// crossserver.go's LWDEBUG_DUMP_LOGIN_BODY dump: it wrote the dump file via os.WriteFile(f,
// encoded, 0600) with no follow-up os.Chmod call. os.WriteFile's mode argument only applies when
// the file is newly CREATED -- on a pre-existing file at that path, the file's previous mode
// wins and is left untouched -- so a pre-existing target file with looser permissions silently
// kept them even though a live access token (p.at) gets written into it on every run. Mirrors
// config.go's SaveSessionConfig and identity.go's saveStateFile, both of which already
// WriteFile-then-Chmod for exactly this reason. Proves the dump file ends up 0600 even when it
// started out 0644.
func TestDoCrossServerLoginDebugDumpBodyFileIsChmodded0600(t *testing.T) {
	dumpPath := filepath.Join(t.TempDir(), "login-body-dump.bin")
	// Pre-create the target file with loose (0644) permissions, simulating a dump left behind
	// by some other, less-careful process -- os.WriteFile alone would NOT tighten this back to
	// 0600 on an existing file, which is exactly the bug this test guards against.
	if err := os.WriteFile(dumpPath, []byte("stale"), 0644); err != nil {
		t.Fatalf("pre-create dump file: %v", err)
	}
	t.Setenv("LWDEBUG_DUMP_LOGIN_BODY", dumpPath)

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	host, port := splitHostPortInt(t, addr)

	p := CrossServerLoginParams{
		IP:        host,
		Port:      port,
		Zone:      "APS1",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
	}
	result, err := DoCrossServerLogin(p)
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	fi, err := os.Stat(dumpPath)
	if err != nil {
		t.Fatalf("stat dump file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0600 {
		t.Errorf("dump file mode = %o, want 0600 (pre-existing looser permissions must be tightened, not just left alone -- see os.WriteFile's mode-applies-on-creation-only gotcha)", got)
	}
}

// TestDoCrossServerLoginDebugDumpRedactsCredentialsIOSMode is the round-13 regression test for
// the LWDEBUG_DUMP_LOGIN debug dump's IOSMode path specifically: identity.go's BuildLoginParams
// builds an iOS-only "ta" analytics blob (JSON-marshaled into a single opaque string field) that
// used to embed the live DeviceID/AirKey/ShumeiBoxId values directly -- a leak StringRedacted()
// couldn't see, since it only masks known-sensitive *keys*, not secrets embedded inside another
// field's string value, and "ta" itself wasn't even in that key list. Mirrors
// TestDoCrossServerLoginDebugDumpRedactsCredentials but with IOSMode:true and its own
// distinguishable secret values, so this specifically exercises the ta-blob path that test
// (IOSMode left false, so BuildLoginParams never builds a "ta" blob at all) does not.
func TestDoCrossServerLoginDebugDumpRedactsCredentialsIOSMode(t *testing.T) {
	t.Setenv("LWDEBUG_DUMP_LOGIN", "1")

	addr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	host, port := splitHostPortInt(t, addr)

	const secretAccessTok = "sensitive-secret-ios-accesstok-must-not-leak-1234567890"
	const secretDeviceID = "sensitive-secret-ios-deviceid-must-not-leak-aaaaaaaaaa"
	const secretAirKey = "sensitive-secret-ios-airkey-must-not-leak-bbbbbbbbbb"
	const secretShumeiBoxId = "sensitive-secret-ios-shumeiboxid-must-not-leak-cccccccccc"

	p := CrossServerLoginParams{
		IP:          host,
		Port:        port,
		Zone:        "APS1",
		GameUid:     "uid-1",
		DeviceID:    secretDeviceID,
		AirKey:      secretAirKey,
		AccessTok:   secretAccessTok,
		ShumeiBoxId: secretShumeiBoxId,
		IOSMode:     true,
	}

	var buf bytes.Buffer
	orig := slog.Default()
	// Debug level explicitly enabled -- LWDEBUG_DUMP_LOGIN's dump is a slog.Debug call, which a
	// default-level (Info) handler would never emit, making this test pass vacuously either way.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	logged := buf.String()
	if !strings.Contains(logged, "full login content") {
		t.Fatal("expected the LWDEBUG_DUMP_LOGIN debug dump to have fired, but no \"full login content\" log line was captured")
	}
	if strings.Contains(logged, secretAccessTok) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump (IOSMode) leaks the raw access token in cleartext:\n%s", logged)
	}
	if strings.Contains(logged, secretDeviceID) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump (IOSMode) leaks the raw device ID in cleartext (likely via the ta analytics blob):\n%s", logged)
	}
	if strings.Contains(logged, secretAirKey) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump (IOSMode) leaks the raw air key in cleartext (likely via the ta analytics blob):\n%s", logged)
	}
	if strings.Contains(logged, secretShumeiBoxId) {
		t.Errorf("LWDEBUG_DUMP_LOGIN debug dump (IOSMode) leaks the raw shumeiBoxId in cleartext (likely via the ta analytics blob):\n%s", logged)
	}
}

// TestDoCrossServerLoginHandshakeLogRedactsSessionToken is the round-12 regression test for
// crossserver.go's "handshake OK" log (the experimental -handshake path): it used to log the raw
// Handshake response via hsResp.String(), but per docs/wire-protocol.mdx the live production
// server's Handshake response carries a real session token in a `tk` field
// (`{ct=3072, ms=1000000, tk=<32-hex>}`) -- unredacted logging of that response leaks a live
// credential the same way the round-11 LWDEBUG_DUMP_LOGIN bug did (see
// TestDoCrossServerLoginDebugDumpRedactsCredentials above). Proves the "handshake OK" log line's
// captured output never contains the fake tk value, while also confirming the log line itself
// fired (so the test isn't vacuously passing because the handshake path never ran).
func TestDoCrossServerLoginHandshakeLogRedactsSessionToken(t *testing.T) {
	const fakeTk = "sensitive-secret-handshake-tk-must-not-leak-abcdef0123456789"

	addr := startFakeGameServer(t, func(server *GameConn) {
		// First envelope: the Handshake request (controllerSystem/actionHandshake, sent by
		// conn.go's DoHandshake before Login).
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		hsResp := NewSFSObject()
		hsResp.PutInt("ct", 3072)
		hsResp.PutInt("ms", 1000000)
		hsResp.PutUtfString("tk", fakeTk)
		if err := server.SendEnvelope(controllerSystem, actionHandshake, hsResp); err != nil {
			return
		}

		// Second envelope: the subsequent Login request, answered normally so
		// DoCrossServerLogin completes.
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})
	host, port := splitHostPortInt(t, addr)

	p := CrossServerLoginParams{
		IP:        host,
		Port:      port,
		Zone:      "APS1",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
		Handshake: true,
	}

	var buf bytes.Buffer
	orig := slog.Default()
	// Debug level explicitly enabled, matching TestDoCrossServerLoginDebugDumpRedactsCredentials's
	// pattern -- "handshake OK" itself logs at Info, but capturing at Debug keeps this test
	// consistent with this file's other log-capture tests and still catches it either way.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	logged := buf.String()
	if !strings.Contains(logged, "handshake OK") {
		t.Fatal("expected the \"handshake OK\" log line to have fired, but it was not captured")
	}
	if strings.Contains(logged, fakeTk) {
		t.Errorf("\"handshake OK\" log leaks the raw handshake session token (tk) in cleartext:\n%s", logged)
	}
}

// TestDoCrossServerLoginSingleRedirect covers following exactly one serverInfo redirect: the
// first fake server's Login response points at a second fake server, which then responds
// normally (no further redirect). The result's Addr/Zone must reflect the POST-redirect server,
// not the one originally dialed.
func TestDoCrossServerLoginSingleRedirect(t *testing.T) {
	var gotZoneOnSecondServer string
	newAddr := startFakeGameServer(t, func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		gotZoneOnSecondServer = env.Content.GetString("zn")
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(newAddr, "APS2"))
	})
	host, port := splitHostPortInt(t, oldAddr)

	p := CrossServerLoginParams{
		IP:        host,
		Port:      port,
		Zone:      "APS1",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
	}
	result, err := DoCrossServerLogin(p)
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	if result.Addr != newAddr {
		t.Errorf("Addr = %q, want %q (the post-redirect address)", result.Addr, newAddr)
	}
	if result.Zone != "APS2" {
		t.Errorf("Zone = %q, want %q (the post-redirect zone)", result.Zone, "APS2")
	}
	// No HTTPClient/RSAPub/GateHost was given, so the GSL-refresh step can't run and
	// AccessTok should fall back to being reused unrefreshed (logged, not an error).
	if result.AccessTok != "tok-1" {
		t.Errorf("AccessTok = %q, want %q (no GSL client given, so reused unrefreshed)", result.AccessTok, "tok-1")
	}
	if gotZoneOnSecondServer != "APS2" {
		t.Errorf("second server saw Login zn=%q, want %q (the redialed Login should use the new zone)", gotZoneOnSecondServer, "APS2")
	}
}

// TestDoCrossServerLoginTooManyRedirects covers the bounded-loop guard: maxRedirects+1 (see
// crossserver.go's maxRedirects const) consecutive fake servers all respond with a serverInfo
// redirect to the next one in the chain. DoCrossServerLogin must give up with the "too many
// serverInfo redirects" error rather than looping forever (or dialing past the guard).
func TestDoCrossServerLoginTooManyRedirects(t *testing.T) {
	const maxRedirects = 3 // must match crossserver.go's unexported maxRedirects const
	const numServers = maxRedirects + 1

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
			// The last server in the chain "redirects" to an address nothing is listening
			// on -- DoCrossServerLogin's redirect-count guard must trip before it ever
			// tries to dial that far, so this address is never actually connected to.
			next := "127.0.0.1:1"
			if i+1 < numServers {
				next = addrs[i+1]
			}
			zone := fmt.Sprintf("APS%d", i+1)
			_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(next, zone))
		})
	}

	host, port := splitHostPortInt(t, addrs[0])
	p := CrossServerLoginParams{
		IP:        host,
		Port:      port,
		Zone:      "APS0",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
	}
	result, err := DoCrossServerLogin(p)
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error after maxRedirects+1 consecutive serverInfo redirects, got nil")
	}
	if !strings.Contains(err.Error(), "too many serverInfo redirects") {
		t.Errorf("err = %q, want it to mention \"too many serverInfo redirects\"", err.Error())
	}
}

// TestDoCrossServerLoginRedirectRefreshesGameUid exercises the major fix in crossserver.go's
// serverInfo redirect block: the mid-redirect GSL refresh (opt=fix) returns a fresh serverList
// entry alongside the fresh access token, and its gameUid must be propagated into p.GameUid --
// not just the access token -- so the redialed Login carries the account's current gameUid
// instead of a stale one. Both places gameUid appears on the wire (the top-level Login `un`
// field and the nested login-params `gameUid` field, per identity.go's BuildLoginParams) are
// checked on the post-redirect connection.
func TestDoCrossServerLoginRedirectRefreshesGameUid(t *testing.T) {
	const oldGameUid = "uid-old"
	const newGameUid = "uid-new"
	const newAccessTok = "tok-fresh"

	// Fake GSL HTTP server: same plaintext-response fallback GetServerList already supports
	// (see gsl_http_test.go's TestGetServerListAgainstFakeServer) -- returns a fresh access
	// token plus a serverList entry carrying an updated gameUid, simulating an account that
	// moved to a new gameUid as part of the same migration that triggered the redirect below.
	gslServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := LoginServerListRespon{
			Code:       0,
			ServerList: []LoginServerInfo{{GameUid: newGameUid}},
			At:         &LoginToken{Token: newAccessTok},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(gslServer.Close)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	gotUn := make(chan string, 1)
	gotParamsGameUid := make(chan string, 1)
	newAddr := startFakeGameServer(t, func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		gotUn <- env.Content.GetString("un")
		if pv, ok := env.Content.Get("p"); ok {
			if pObj, ok := pv.Val.(*SFSObject); ok {
				gotParamsGameUid <- pObj.GetString("gameUid")
			}
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
	})

	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendEnvelope(controllerSystem, actionLogin, putRedirectServerInfo(newAddr, "APS2"))
	})
	host, port := splitHostPortInt(t, oldAddr)

	p := CrossServerLoginParams{
		IP:         host,
		Port:       port,
		Zone:       "APS1",
		GameUid:    oldGameUid,
		DeviceID:   "dev-1",
		AirKey:     "airkey-1",
		AccessTok:  "tok-1",
		HTTPClient: defaultHTTPClient(),
		RSAPub:     &priv.PublicKey,
		GateHost:   gslServer.URL,
	}
	result, err := DoCrossServerLogin(p)
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer result.Conn.Close()

	if result.Zone != "APS2" {
		t.Errorf("Zone = %q, want %q (the post-redirect zone)", result.Zone, "APS2")
	}
	if result.AccessTok != newAccessTok {
		t.Errorf("AccessTok = %q, want %q (refreshed via GSL)", result.AccessTok, newAccessTok)
	}

	select {
	case un := <-gotUn:
		if un != newGameUid {
			t.Errorf("post-redirect Login `un` = %q, want %q (refreshed gameUid, not the stale input one)", un, newGameUid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}
	select {
	case gu := <-gotParamsGameUid:
		if gu != newGameUid {
			t.Errorf("post-redirect Login params.gameUid = %q, want %q (refreshed gameUid, not the stale input one)", gu, newGameUid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}
}
