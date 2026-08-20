package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
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

// flexPort converts a plain int port into gsl.go's flexString shape, for building test
// LoginServerInfo/AccountServerInfo fixtures now that Port/WsPort are flexString (round 35 --
// see flexString.Int's own doc comment in gsl.go for why).
func flexPort(n int) flexString { return flexString(strconv.Itoa(n)) }

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

// TestDoCrossServerLoginExactlyMaxRedirectsSucceeds is the round-42 regression test for the MINOR
// finding that TestDoCrossServerLoginTooManyRedirects above only proves the guard trips on
// maxRedirects+1 redirects -- it never exercises the boundary itself, so a regression tightening
// crossserver.go's `hop > maxRedirects` to an off-by-one `hop >= maxRedirects` would reject a
// legitimate maxRedirects-hop chain (the exact live APS783->APS8092 case this file's own doc
// comment cites) with zero test signal. Confirmed via mutation testing: that exact `>=` tightening
// passed TestDoCrossServerLoginTooManyRedirects unchanged. Chains exactly maxRedirects consecutive
// redirects, the last of which completes a normal, non-redirect login success -- proving the
// guard's own boundary value is itself still followable, not just "somewhere past it errors."
func TestDoCrossServerLoginExactlyMaxRedirectsSucceeds(t *testing.T) {
	const maxRedirects = 3              // must match crossserver.go's unexported maxRedirects const
	const numServers = maxRedirects + 1 // servers 0..2 redirect; server 3 (the maxRedirects-th hop) succeeds

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
			// The last server in the chain (hop == maxRedirects) completes a normal login
			// instead of redirecting again -- this is the boundary value itself, which must
			// still be reachable, not rejected.
			resp := NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
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
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v, want it to succeed after exactly %d redirects (the boundary value)", err, maxRedirects)
	}
	defer result.Conn.Close()

	if result.Addr != addrs[numServers-1] {
		t.Errorf("Addr = %q, want %q (the final, maxRedirects-th server in the chain)", result.Addr, addrs[numServers-1])
	}
}

// TestDoCrossServerLoginRedirectRejectsEmptyRedirectIP is the round-18 regression test for
// DoCrossServerLogin's serverInfo redirect branch: it built the redialed address via a raw
// fmt.Sprintf("%s:%d", firstHost(siObj.GetString("ip")), ...) guarded only by
// siObj.GetString("ip") != "" -- the RAW string, not firstHost's resolved result. A pipe-malformed
// ip like "|1.2.3.4" is non-empty raw but firstHost resolves it down to "", so the old code built a
// ":<port>"-shaped address, which Go's "host:port" dial syntax silently treats as the loopback
// interface instead of failing clearly. Mirrors login.go's own TestLoginRedirectRejectsEmptyRedirectIP
// for the same bug on Login()'s side (conn_wait_test.go); this fix routes DoCrossServerLogin's
// redirect branch through login.go's buildBaseZoneLoginAddr instead of duplicating the guard.
func TestDoCrossServerLoginRedirectRejectsEmptyRedirectIP(t *testing.T) {
	oldAddr := startFakeGameServer(t, func(server *GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := NewSFSObject()
		si.PutUtfString("ip", "|1.2.3.4") // firstHost("|1.2.3.4") == "" -- the malformed case
		si.PutInt("port", 9339)
		si.PutUtfString("zone", "APS2")
		resp := NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		_ = server.SendEnvelope(controllerSystem, actionLogin, resp)
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
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error for a pipe-malformed redirect ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
	if !strings.Contains(err.Error(), "serverInfo redirect") {
		t.Errorf("err = %q, want it to mention the serverInfo redirect context", err.Error())
	}
}

// TestDoCrossServerLoginRedirectWrongTypedIPIsWarned is the round-29 regression test for the
// gate login.go's redirectIP helper closes: the redirect-detection check used to be an entirely
// UNGUARDED siObj.GetString("ip") != "" -- which silently returns "" for ANY wrong-typed ip
// field, indistinguishable from a genuinely absent one, making a real redirect completely
// invisible instead of erroring or warning. This is not theoretical: gsl.go's getIntFlexible
// helper exists specifically because this SAME serverInfo object's neighboring port field is
// documented as "confirmed live... sometimes a UTF string instead" of a number -- only port was
// ever hardened against that, not ip. Here the fake server sends serverInfo.ip as an int
// (PutInt) instead of the expected UTF string, simulating exactly that failure mode. Proves two
// things: (1) DoCrossServerLogin does NOT silently vanish the signal -- it logs a Warn
// mentioning the wrong-typed ip field -- and (2) since the ip genuinely can't be resolved, it
// still falls back to treating the response as a normal (non-redirect) success, matching the
// pre-fix behavior's control flow exactly, only now with a diagnostic instead of total silence.
func TestDoCrossServerLoginRedirectWrongTypedIPIsWarned(t *testing.T) {
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

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v (a wrong-typed ip must not be treated as a fatal error -- it degrades to \"no redirect\", same as a genuinely absent ip)", err)
	}
	defer result.Conn.Close()

	if result.Addr != addr {
		t.Errorf("Addr = %q, want %q (the wrong-typed ip must not resolve to a followed redirect)", result.Addr, addr)
	}
	if result.Zone != "APS1" {
		t.Errorf("Zone = %q, want %q (unchanged -- the redirect must not have been followed)", result.Zone, "APS1")
	}

	logged := buf.String()
	if !strings.Contains(logged, "wrong-typed") || !strings.Contains(logged, "ip") {
		t.Errorf("expected a Warn log mentioning the wrong-typed ip field, got:\n%s", logged)
	}
}

// TestDoCrossServerLoginRejectsEmptyInitialIP is the round-24 regression test for
// DoCrossServerLogin's INITIAL dial address: it used to build addr via a raw
// fmt.Sprintf("%s:%d", firstHost(p.IP), p.Port) with no validation at all -- unlike this same
// function's own redirect branch (TestDoCrossServerLoginRedirectRejectsEmptyRedirectIP above) and
// both of login.go's Login() call sites, all three of which route through buildBaseZoneLoginAddr
// and reject an empty resolved host with a clear error. A pipe-malformed ip like "|1.2.3.4" is
// non-empty raw but firstHost resolves it down to "", so the old code built a ":<port>"-shaped
// address, which Go's "host:port" dial syntax silently treats as the loopback interface instead of
// failing clearly. No fake game server is needed here -- the fix must reject this before ever
// attempting to dial one.
func TestDoCrossServerLoginRejectsEmptyInitialIP(t *testing.T) {
	p := CrossServerLoginParams{
		IP:        "|1.2.3.4", // firstHost("|1.2.3.4") == "" -- the malformed case
		Port:      9339,
		Zone:      "APS1",
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
		t.Fatal("expected an error for an empty/pipe-malformed initial ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
}

// TestDoCrossServerLoginRejectsNonPositiveInitialPort is the port-half counterpart to
// TestDoCrossServerLoginRejectsEmptyInitialIP: a non-positive Port (e.g. the zero value a caller
// forgets to set) must also be rejected clearly by the INITIAL dial's buildBaseZoneLoginAddr call,
// not silently turned into a "host:0"-shaped address that Go's dial syntax would treat as "any
// port" rather than erroring. No fake game server is needed -- the fix must reject this before
// ever attempting to dial one.
func TestDoCrossServerLoginRejectsNonPositiveInitialPort(t *testing.T) {
	p := CrossServerLoginParams{
		IP:        "203.0.113.9",
		Port:      0,
		Zone:      "APS1",
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
		t.Fatal("expected an error for a non-positive initial port, got nil")
	}
	if strings.Contains(err.Error(), "203.0.113.9:0") {
		t.Errorf("err = %q, must not contain a \"host:0\"-shaped address (that's the footgun this guard exists to prevent)", err.Error())
	}
}

// TestDoCrossServerLoginRejectsOversizedZoneGameUidAccessTok is the round-47 regression test for
// the MAJOR finding that p.Zone/p.GameUid/p.AccessTok -- re-encoded verbatim via PutUtfString on
// every hop of DoCrossServerLogin's loop (zn/un/p.at) -- had no length check at all, unlike
// loginKey/gameUid/username which got exactly this guard (maxIdentityFieldLen) in round 46. Every
// current caller sources these from an unguarded gsl.go flexString field or an unguarded SFS2X
// serverInfo redirect (see capOversizedIdentityField's doc comment, login.go), so validating
// synchronously here -- before any connection is even dialed -- closes the gap for every caller in
// one place. Mirrors TestDoCrossServerLoginRejectsEmptyInitialIP/
// TestDoCrossServerLoginRejectsNonPositiveInitialPort's no-fake-server-needed shape: the fix must
// reject before ever attempting to dial.
func TestDoCrossServerLoginRejectsOversizedZoneGameUidAccessTok(t *testing.T) {
	oversized := strings.Repeat("x", maxIdentityFieldLen+1)
	base := CrossServerLoginParams{
		IP:        "203.0.113.9",
		Port:      9339,
		Zone:      "APS1",
		GameUid:   "uid-1",
		DeviceID:  "dev-1",
		AirKey:    "airkey-1",
		AccessTok: "tok-1",
	}

	cases := []struct {
		name  string
		build func(p CrossServerLoginParams) CrossServerLoginParams
	}{
		{"zone", func(p CrossServerLoginParams) CrossServerLoginParams { p.Zone = oversized; return p }},
		{"gameUid", func(p CrossServerLoginParams) CrossServerLoginParams { p.GameUid = oversized; return p }},
		{"accessTok", func(p CrossServerLoginParams) CrossServerLoginParams { p.AccessTok = oversized; return p }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result, err := DoCrossServerLogin(c.build(base))
			if err == nil {
				if result != nil && result.Conn != nil {
					result.Conn.Close()
				}
				t.Fatalf("expected an error for an oversized %s, got nil", c.name)
			}
			// Must be the synchronous length-validation error, not a dial failure against the
			// (non-listening, reserved TEST-NET-3) 203.0.113.9 address -- proving the field is
			// rejected BEFORE any connection is attempted, not merely that some later step
			// happens to fail too.
			if !strings.Contains(err.Error(), "too long") {
				t.Errorf("err = %q, want it to mention the field being too long (i.e. rejected by the synchronous length check, not a dial failure)", err.Error())
			}
		})
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
			Code:       "0",
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

// TestDoCrossServerLoginRedirectRefreshKeepsOldValuesWhenOversized is
// TestDoCrossServerLoginRedirectRefreshesGameUid's sibling for an oversized (rather than
// legitimate) mid-redirect GSL refresh: an oversized accessTok/gameUid returned by the opt=fix
// refresh must fall back to the PREVIOUS value (capOversizedIdentityField, login.go) instead of
// ever reaching PutUtfString with a value writeUtfString would hard-reject, which would otherwise
// fail the post-redirect Login's encode step and get misclassified by sendStageError (conn.go) as
// a genuine dead connection.
func TestDoCrossServerLoginRedirectRefreshKeepsOldValuesWhenOversized(t *testing.T) {
	const oldGameUid = "uid-old"
	const oldAccessTok = "tok-1"
	oversizedGameUid := strings.Repeat("g", maxIdentityFieldLen+1)
	oversizedAccessTok := flexString(strings.Repeat("t", maxIdentityFieldLen+1))

	gslServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{{GameUid: flexString(oversizedGameUid)}},
			At:         &LoginToken{Token: oversizedAccessTok},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(gslServer.Close)

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	gotUn := make(chan string, 1)
	gotParamsAt := make(chan string, 1)
	newAddr := startFakeGameServer(t, func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		gotUn <- env.Content.GetString("un")
		if pv, ok := env.Content.Get("p"); ok {
			if pObj, ok := pv.Val.(*SFSObject); ok {
				gotParamsAt <- pObj.GetString("at")
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

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	p := CrossServerLoginParams{
		IP:         host,
		Port:       port,
		Zone:       "APS1",
		GameUid:    oldGameUid,
		DeviceID:   "dev-1",
		AirKey:     "airkey-1",
		AccessTok:  oldAccessTok,
		HTTPClient: defaultHTTPClient(),
		RSAPub:     &priv.PublicKey,
		GateHost:   gslServer.URL,
	}
	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v (an oversized refreshed gameUid/accessTok must fall back to the previous value, not fail the login)", err)
	}
	defer result.Conn.Close()

	if result.AccessTok != oldAccessTok {
		t.Errorf("AccessTok = %q, want %q (the oversized refreshed token must be rejected, keeping the previous one)", result.AccessTok, oldAccessTok)
	}
	if result.GameUid != oldGameUid {
		t.Errorf("GameUid = %q, want %q (the oversized refreshed gameUid must be rejected, keeping the previous one)", result.GameUid, oldGameUid)
	}

	select {
	case un := <-gotUn:
		if un != oldGameUid {
			t.Errorf("post-redirect Login `un` = %q, want %q (the stale, still-valid gameUid)", un, oldGameUid)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}
	select {
	case at := <-gotParamsAt:
		if at != oldAccessTok {
			t.Errorf("post-redirect Login params.at = %q, want %q (the stale, still-valid access token)", at, oldAccessTok)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("post-redirect fake server never received a Login request")
	}

	logged := buf.String()
	if !strings.Contains(logged, "gameUid exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized refreshed gameUid, got:\n%s", logged)
	}
	if !strings.Contains(logged, "accessTok exceeds identity field length cap") {
		t.Errorf("expected a Warn about the oversized refreshed accessTok, got:\n%s", logged)
	}
}

// TestDoCrossServerLoginRedirectWrongTypedZoneIsWarned is the round-30 regression test for the
// gate login.go's redirectZone helper closes: unlike its siblings redirectIP (hardened round 29
// for exactly this "present but wrong-typed" gap) and port (hardened via getIntFlexible), the
// serverInfo redirect's zone field used to be read via an entirely UNGUARDED
// siObj.GetString("zone") -- which silently returns "" for ANY wrong-typed zone field,
// indistinguishable from a genuinely absent one.
//
// This matters in a DIFFERENT, arguably worse way than a wrong-typed ip does: a wrong-typed ip
// stops the redirect from being followed at all, since there's nowhere to redial to. A wrong-typed
// zone does NOT stop anything -- here the ip/port are well-typed, so the redirect IS followed to
// the new address, but the zone silently falls back to "" and (per the existing `if newZone != ""`
// guard) the STALE pre-redirect zone is kept and resent as `zn` on the redialed Login. That's a
// real, non-theoretical desync risk: the connection ends up talking to the new shard's ip/port
// while both the redialed Login request and DoCrossServerLogin's own returned Zone still claim the
// old one.
//
// Here the first fake server's redirect carries a well-typed ip/port (pointing at a second fake
// server) but a wrong-typed zone (PutInt instead of PutUtfString). Proves: (1) the redirect is
// still followed (Addr == the second server's address, and that server actually receives the
// redialed Login), (2) Zone is NOT updated -- it stays "APS1", the stale pre-redirect value, and
// that stale value is what actually gets resent as `zn` -- and (3) a Warn mentioning the
// wrong-typed zone field is logged, unlike the pre-fix total silence.
func TestDoCrossServerLoginRedirectWrongTypedZoneIsWarned(t *testing.T) {
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

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	result, err := DoCrossServerLogin(p)

	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v (a wrong-typed zone must not be a fatal error -- the redirect itself still follows on the well-typed ip/port)", err)
	}
	defer result.Conn.Close()

	if result.Addr != newAddr {
		t.Errorf("Addr = %q, want %q (the redirect must still be followed -- ip/port are well-typed, only zone is wrong-typed)", result.Addr, newAddr)
	}
	if result.Zone != "APS1" {
		t.Errorf("Zone = %q, want %q (unchanged/stale -- the wrong-typed zone can't overwrite it, which is exactly the desync risk this test documents: a followed ip/port redirect paired with a stale zone)", result.Zone, "APS1")
	}

	if gotZoneOnSecondServer != "APS1" {
		t.Errorf("second server saw Login zn=%q, want %q (the redialed Login resent the stale zone, since the new one could not be read)", gotZoneOnSecondServer, "APS1")
	}

	logged := buf.String()
	if !strings.Contains(logged, "wrong-typed") || !strings.Contains(logged, "zone") {
		t.Errorf("expected a Warn log mentioning the wrong-typed zone field, got:\n%s", logged)
	}
}

// fakeWriteFailError is a minimal net.Error used to inject a deterministic write-stage failure
// into a real (dialed, not net.Pipe) *GameConn -- see writeFailAfterConn and withFailingDial
// below. Deliberately reports Timeout()==true, mirroring conn_wait_test.go's
// fakeTimeoutNetError/TestSendAndWaitWriteStageFailureIsNonTimeoutNetError and
// conn_handshake_test.go's TestDoHandshakeSendFailureIsNonTimeoutNetError: a genuine
// deadline-exceeded net.Conn.Write returns exactly this Timeout()==true shape, and the whole
// point of sendStageError (conn.go) is to force Timeout()==false on the error a caller actually
// sees regardless of what the underlying write failure itself reports. Defined locally here
// (rather than reusing conn_wait_test.go's identically-shaped type) since this file's tests must
// not depend on the exact contents of a file owned by a different concurrently-editing agent.
type fakeWriteFailError struct{ msg string }

func (e fakeWriteFailError) Error() string { return e.msg }
func (fakeWriteFailError) Timeout() bool   { return true }
func (fakeWriteFailError) Temporary() bool { return true }

// writeFailAfterConn wraps a net.Conn, passing the first n Write calls through to the embedded
// conn unchanged and making the (n+1)th and every later Write fail with a fixed error. This is
// writeFailConn's (conn_wait_test.go) counting sibling: writeFailConn fails every write
// unconditionally, which is exactly right for a helper under test that itself performs the very
// first send (e.g. DoHandshake, or DoCrossServerLogin's/Login()'s own base-zone Login send), but
// too blunt for targeting a SPECIFIC send-stage call site deep in Login()'s multi-step flow --
// e.g. account.login.send.verify.code or account.login.new, both of which only run after one or
// more earlier sends on the same connection have already succeeded. n=0 degenerates to "fail
// every write", the same behavior as writeFailConn.
type writeFailAfterConn struct {
	net.Conn
	n   int
	err error
}

func (w *writeFailAfterConn) Write(p []byte) (int, error) {
	if w.n <= 0 {
		return 0, w.err
	}
	w.n--
	return w.Conn.Write(p)
}

// withFailingDial overrides login.go's dialGame package var for the duration of the calling test
// (restored via t.Cleanup), so that the NEXT n writes issued by whatever GameConn Login() or
// DoCrossServerLogin() dials succeed as normal (dialing for real, against whatever fake game
// server the test already stood up), while the (n+1)th and every later write fails with err. This
// is the round-30 regression-test infrastructure for the finding that all 4 of login.go's/
// crossserver.go's direct SendEnvelope/SendExtension call sites on the login hot path had zero
// test coverage for their sendStageError wrapping: Login()/DoCrossServerLogin() dial their own
// connection internally via DialGame, so -- unlike sendAndWait/DoHandshake, which operate on a
// caller-supplied *GameConn a test can build over net.Pipe and swap writeFailConn into directly
// (conn_wait_test.go/conn_handshake_test.go) -- there is no seam to inject a write failure without
// either winning an inherently racy real-TCP "peer closed right after accept" timing game, or
// indirecting the dial itself. dialGame is that indirection: production code always resolves it to
// the real DialGame (conn.go); only tests ever reassign it.
func withFailingDial(t *testing.T, n int, err error) {
	t.Helper()
	orig := dialGame
	dialGame = func(addr string, timeout time.Duration) (*GameConn, error) {
		conn, dialErr := DialGame(addr, timeout)
		if dialErr != nil {
			return nil, dialErr
		}
		conn.conn = &writeFailAfterConn{Conn: conn.conn, n: n, err: err}
		return conn, nil
	}
	t.Cleanup(func() { dialGame = orig })
}

// TestDoCrossServerLoginSendFailureIsNonTimeoutNetError is the round-30 regression test for the
// TESTING-RIGOR finding that crossserver.go's own login-send call site (its conn.SendEnvelope
// call, wrapped in sendStageError since round 29) had zero coverage -- a coverage profile showed
// execution count 0 for that block despite the wrapping already being in place. DoCrossServerLogin
// sends the base-zone Login as the very FIRST write on a freshly dialed connection (no handshake
// by default), so withFailingDial(t, 0, ...) makes that first write fail deterministically.
// Mirrors TestSendAndWaitWriteStageFailureIsNonTimeoutNetError's / TestDoHandshakeSendFailure
// IsNonTimeoutNetError's assertions exactly (conn_wait_test.go/conn_handshake_test.go): the
// returned error must wrap the injected failure (errors.Is) and satisfy net.Error with
// Timeout()==false/Temporary()==false even though the injected failure itself reports
// Timeout()==true -- proving sendStageError's wrapping, not a bare passthrough.
func TestDoCrossServerLoginSendFailureIsNonTimeoutNetError(t *testing.T) {
	// Idle handler: the fake server never receives anything, since the client's send itself fails
	// before the packet ever reaches the network.
	addr := startFakeGameServer(t, func(server *GameConn) {})
	host, port := splitHostPortInt(t, addr)

	writeErr := fakeWriteFailError{msg: "simulated write-deadline-exceeded failure"}
	withFailingDial(t, 0, writeErr)

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
	if err == nil {
		if result != nil && result.Conn != nil {
			result.Conn.Close()
		}
		t.Fatal("expected an error when the login send itself fails")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap the underlying write failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a send-stage failure must be distinguishable from DoCrossServerLogin's own benign wait-stage timeout, even though the underlying write error itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write)")
	}
	if netErr.Temporary() {
		t.Errorf("netErr.Temporary() = true, want false")
	}
}
