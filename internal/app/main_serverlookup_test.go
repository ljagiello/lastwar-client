package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The game server a lookup returns in these tests: the zone's three gateways on its current port.
const (
	lookupGateways = "gw-a.example|gw-b.example|gw-c.example"
	lookupPort     = 10783
	lookupZone     = "APS783"
	lookupGameUid  = "uid-1"
)

func lookupReply() gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code: "0",
		ServerList: []gsl.LoginServerInfo{
			{ID: "783", IP: lookupGateways, Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: lookupGameUid},
		},
	}
}

// countGetServerList wraps client's transport to count getserverlist.php requests.
func countGetServerList(client *http.Client) func() int {
	var mu sync.Mutex
	n := 0
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getserverlist.php") {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return base.RoundTrip(r)
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// recordingDial serves every address with an in-memory game server: handlers[addr] when present,
// else FakeInitPushServer, and records the addresses dialed. A nil handler refuses the dial the way
// an unreachable gateway does.
func recordingDial(handlers map[string]func(*session.GameConn)) (dial func(string, time.Duration) (*session.GameConn, error), dialed func() []string) {
	var mu sync.Mutex
	var addrs []string
	return func(addr string, timeout time.Duration) (*session.GameConn, error) {
			mu.Lock()
			addrs = append(addrs, addr)
			mu.Unlock()
			handler, ok := handlers[addr]
			if !ok {
				handler = session.FakeInitPushServer(nil)
			} else if handler == nil {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return session.NewInMemoryGameDial(handler)(addr, timeout)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), addrs...)
		}
}

func readSessionConfig(t *testing.T, path string) SessionConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session config was not written to %s: %v", path, err)
	}
	var cfg SessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse persisted session config: %v", err)
	}
	return cfg
}

// TestRunCrossServerTestLooksUpMissingAddress: a session with no ip/port asks GSL getserverlist for
// the role's game server, dials its first gateway, logs in with the zone the lookup named, and
// persists the whole gateway list and port, keeping the configured access token.
func TestRunCrossServerTestLooksUpMissingAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	lookups := countGetServerList(client)
	zoneSeen := make(chan string, 1)
	dial, dialed := recordingDial(map[string]func(*session.GameConn){
		"gw-a.example:10783": session.FakeInitPushServer(zoneSeen),
	})
	cfgPath := t.TempDir() + "/session.json"

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			zone: "APS1", gameUid: lookupGameUid, at: "at-1",
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if n := lookups(); n != 1 {
		t.Errorf("getserverlist requests = %d, want 1", n)
	}
	if got := dialed(); len(got) != 1 || got[0] != "gw-a.example:10783" {
		t.Errorf("dialed %v, want [gw-a.example:10783]", got)
	}
	if z := <-zoneSeen; z != lookupZone {
		t.Errorf("Login zone = %q, want the looked-up %q", z, lookupZone)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.IP != lookupGateways || cfg.Port != lookupPort || cfg.Zone != lookupZone {
		t.Errorf("persisted %s %s:%d, want %s %s:%d", cfg.Zone, cfg.IP, cfg.Port, lookupZone, lookupGateways, lookupPort)
	}
	if cfg.AccessToken != "at-1" || cfg.GameUid != lookupGameUid {
		t.Errorf("persisted accessToken/gameUid changed: tokenLen=%d gameUid=%q", len(cfg.AccessToken), cfg.GameUid)
	}
}

// TestRunCrossServerTestIOSLookupUsesTheIOSCheckVersion: an iOS session with no address fetches
// check-version once, as iOS, and seals its lookup with that reply's RSA key, with platform=iOS. The
// one check-version is what ties the key to the iOS reply; the fake decrypts only a form sealed with
// the key it served. This is how the cron host's iOS session finds its server again after the
// address moves.
func TestRunCrossServerTestIOSLookupUsesTheIOSCheckVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client, forms := testutil.UseDecryptingGSL(t, lookupReply())
	var mu sync.Mutex
	var cvPlatforms []string
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getlsu3dversion.php") {
			mu.Lock()
			cvPlatforms = append(cvPlatforms, r.URL.Query().Get("platform"))
			mu.Unlock()
		}
		return base.RoundTrip(r)
	})
	dial, dialed := recordingDial(nil)

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			zone: lookupZone, gameUid: lookupGameUid, at: "at-1", iosMode: true,
			httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	mu.Lock()
	defer mu.Unlock()
	if len(cvPlatforms) != 1 || cvPlatforms[0] != "iOS" {
		t.Errorf("check-version platforms = %v, want [iOS]", cvPlatforms)
	}
	got := forms()
	if len(got) != 1 {
		t.Fatalf("getserverlist requests = %d, want 1 (the lookup)", len(got))
	}
	if f := got[0]; f.Get("platform") != "iOS" || f.Get("zone") != lookupZone || f.Get("gameuid") != lookupGameUid {
		t.Errorf("lookup form platform=%q zone=%q gameuid=%q, want iOS %s %s (an empty form wasn't sealed with the check-version key)",
			f.Get("platform"), f.Get("zone"), f.Get("gameuid"), lookupZone, lookupGameUid)
	}
	if d := dialed(); len(d) != 1 || d[0] != "gw-a.example:10783" {
		t.Errorf("dialed %v, want [gw-a.example:10783]", d)
	}
}

// TestRunCrossServerTestLooksUpBeforeEveryLogin: the real client clears its cached address and runs
// GetServerList on every cold start (Assembly-CSharp.decompiled.cs:9332, 7210-7225, 1.0.370), so a
// configured address is looked up before the Login too, and the Login goes to the server GSL lists
// (1.0.370 audit B40). A lookup that fails in any way, a 211 included, falls back to the configured
// address and changes nothing.
func TestRunCrossServerTestLooksUpBeforeEveryLogin(t *testing.T) {
	cases := []struct {
		name       string
		reply      gsl.LoginServerListRespon
		wantDial   string
		wantSaveIP string // "" = the session config isn't written
	}{
		{"GSL lists another server", lookupReply(), "gw-a.example:10783", lookupGateways},
		{"GSL refuses (211)", gsl.LoginServerListRespon{Code: "211"}, "10.0.0.1:17783", ""},
		{"GSL lists no server (E120)", gsl.LoginServerListRespon{Code: "0"}, "10.0.0.1:17783", ""},
		// An entry without the role's gameUid is GSL's lastLoggedServer or first entry, not the
		// role's server: the configured address stays.
		{"GSL lists another server without the role's gameUid", gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "901", IP: "other.example", Port: testutil.FlexPort(10901), Zone: "APS901"}},
		}, "10.0.0.1:17783", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, c.reply)
			lookups := countGetServerList(client)
			dial, dialed := recordingDial(nil)
			cfgPath := t.TempDir() + "/session.json"

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if n := lookups(); n != 1 {
				t.Errorf("getserverlist requests = %d, want 1 (the pre-Login lookup)", n)
			}
			if got := dialed(); len(got) != 1 || got[0] != c.wantDial {
				t.Errorf("dialed %v, want [%s]", got, c.wantDial)
			}
			if c.wantSaveIP == "" {
				if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
					t.Errorf("session config written although nothing changed (stat err = %v)", err)
				}
				return
			}
			if cfg := readSessionConfig(t, cfgPath); cfg.IP != c.wantSaveIP || cfg.Port != lookupPort || cfg.AccessToken != "at-1" {
				t.Errorf("persisted %s:%d (tokenLen %d), want %s:%d with the session's token", cfg.IP, cfg.Port, len(cfg.AccessToken), c.wantSaveIP, lookupPort)
			}
		})
	}
}

// lookupTokensReply lists the configured address and hands back a new token pair, which the client's
// SaveGameLoginToken stores the moment the reply arrives (Assembly-CSharp.decompiled.cs:12777,
// 7482-7492, 1.0.370).
func lookupTokensReply() gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "10.0.0.1", Port: testutil.FlexPort(17783), Zone: lookupZone, GameUid: lookupGameUid}},
		At:         &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"},
		Rt:         &gsl.LoginToken{Token: "rt-gsl", Time: "1791900001"},
	}
}

// TestRunCrossServerTestAdoptsLookupTokens: a token the lookup reply carries is the one the Login
// sends, and the session config keeps it, as the client's SaveGameLoginToken stores any non-empty
// at/rt a GSL reply hands it (Assembly-CSharp.decompiled.cs:12777, 7482-7492). A lookup that lists
// no other server keeps the configured address.
func TestRunCrossServerTestAdoptsLookupTokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupTokensReply())
	var seen loginAts
	dial, dialed := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("at-gsl", &seen)})
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-1"})

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if got := seen.get(); fmt.Sprint(got) != "[at-gsl]" {
		t.Errorf("Login access tokens = %v, want [at-gsl]", got)
	}
	if got := dialed(); fmt.Sprint(got) != "[10.0.0.1:17783]" {
		t.Errorf("dialed %v, want [10.0.0.1:17783]", got)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.AccessToken != "at-gsl" || cfg.AccessTokenTime != 1791900000 || cfg.RefreshToken != "rt-gsl" || cfg.RefreshTokenTime != 1791900001 || cfg.IP != "10.0.0.1" {
		t.Errorf("persisted at=%q (%d) rt=%q (%d) ip=%q, want the lookup's tokens and the configured address",
			cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime, cfg.IP)
	}
}

// TestRunCrossServerTestFallsBackToTheConfiguredLogin: the configured address and token stay the
// fallback, and only the part that failed is put back. When every gateway the pre-Login lookup
// listed refuses, the Login goes to the configured address, still with the token the lookup handed
// back; when a server refuses that token (E011), the configured token is tried at the same address.
// The run succeeds, and the session config is left as it was, except for a refresh token the lookup
// rotated (the newest, it stays) and a handed-back token a server took (saved after the Login).
func TestRunCrossServerTestFallsBackToTheConfiguredLogin(t *testing.T) {
	listedWithAt := lookupReply()
	listedWithAt.At = &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}
	listedRefuse := func(configured func(*session.GameConn)) map[string]func(*session.GameConn) {
		return map[string]func(*session.GameConn){
			"gw-a.example:10783": nil, "gw-b.example:10783": nil, "gw-c.example:10783": nil,
			"10.0.0.1:17783": configured,
		}
	}
	cases := []struct {
		name       string
		reply      gsl.LoginServerListRespon
		handlers   func(seen *loginAts) map[string]func(*session.GameConn)
		wantDialed string
		wantLogins string
		// "" for both = the session config is left byte for byte as it was
		wantAt, wantRt string
		wantAtTime     int64
	}{
		{"listed gateways refuse", lookupReply(), func(seen *loginAts) map[string]func(*session.GameConn) {
			return listedRefuse(tokenCheckingServer("at-1", seen))
		}, "[gw-a.example:10783 gw-b.example:10783 gw-c.example:10783 10.0.0.1:17783]", "[at-1]", "", "", 0},
		{"listed gateways refuse, the configured address refuses the handed-back token", listedWithAt, func(seen *loginAts) map[string]func(*session.GameConn) {
			return listedRefuse(tokenCheckingServer("at-1", seen))
		}, "[gw-a.example:10783 gw-b.example:10783 gw-c.example:10783 10.0.0.1:17783 10.0.0.1:17783]", "[at-gsl at-1]", "", "", 0},
		{"listed gateways refuse, the configured address takes the handed-back token", listedWithAt, func(seen *loginAts) map[string]func(*session.GameConn) {
			return listedRefuse(tokenCheckingServer("at-gsl", seen))
		}, "[gw-a.example:10783 gw-b.example:10783 gw-c.example:10783 10.0.0.1:17783]", "[at-gsl]", "at-gsl", "rt-1", 1791900000},
		{"handed-back token refused", gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "10.0.0.1", Port: testutil.FlexPort(17783), Zone: lookupZone, GameUid: lookupGameUid}},
			At:         &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"},
		}, func(seen *loginAts) map[string]func(*session.GameConn) {
			return map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("at-1", seen)}
		}, "[10.0.0.1:17783 10.0.0.1:17783]", "[at-gsl at-1]", "", "", 0},
		{"handed-back token refused, refresh token rotated", lookupTokensReply(), func(seen *loginAts) map[string]func(*session.GameConn) {
			return map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("at-1", seen)}
		}, "[10.0.0.1:17783 10.0.0.1:17783]", "[at-gsl at-1]", "at-1", "rt-gsl", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, c.reply)
			var seen loginAts
			dial, dialed := recordingDial(c.handlers(&seen))
			cfg := SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-1", RefreshToken: "rt-1"}
			cfgPath := writeSessionConfig(t, cfg)
			before, err := os.ReadFile(cfgPath)
			if err != nil {
				t.Fatal(err)
			}

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", refreshTok: "rt-1",
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if got := fmt.Sprint(dialed()); got != c.wantDialed {
				t.Errorf("dialed %s, want %s", got, c.wantDialed)
			}
			if got := fmt.Sprint(seen.get()); got != c.wantLogins {
				t.Errorf("Login access tokens = %s, want %s", got, c.wantLogins)
			}
			if c.wantAt == "" && c.wantRt == "" {
				if after, err := os.ReadFile(cfgPath); err != nil || !bytes.Equal(after, before) {
					t.Errorf("session config changed (err %v):\n%s", err, after)
				}
				return
			}
			saved := readSessionConfig(t, cfgPath)
			if saved.AccessToken != c.wantAt || saved.AccessTokenTime != c.wantAtTime || saved.RefreshToken != c.wantRt || saved.IP != "10.0.0.1" {
				t.Errorf("saved at=%q (%d) rt=%q ip=%q, want %s (%d) and %s at the configured address",
					saved.AccessToken, saved.AccessTokenTime, saved.RefreshToken, saved.IP, c.wantAt, c.wantAtTime, c.wantRt)
			}
		})
	}
}

// TestRunCrossServerTestLookupTokensSavedBeforeLogin: a lookup reply that rotates the refresh token
// is in the session config before the Login, as the client's SaveGameLoginToken stores it the moment
// the reply arrives (Assembly-CSharp.decompiled.cs:12777, 7482-7492, before ToConnectGame), so a
// Login that fails afterwards (a ban here) doesn't lose it and the next run doesn't refresh with a
// dead one. The session config's other fields stay as they were on disk, and no token is logged.
func TestRunCrossServerTestLookupTokensSavedBeforeLogin(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, lookupTokensReply())
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": banningServer})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", refreshTok: "rt-1",
			configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-1", RefreshToken: "rt-1", Features: map[string]bool{"radar": true}})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestLookupTokensSavedBeforeLogin", "LASTWAR_TEST_CFG="+cfgPath)
	if code != exitCodeAccountBlocked {
		t.Errorf("exit code = %d, want %d (the Login was refused); stderr=%s", code, exitCodeAccountBlocked, stderr)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.RefreshToken != "rt-gsl" || cfg.RefreshTokenTime != 1791900001 || cfg.AccessToken != "at-gsl" || cfg.AccessTokenTime != 1791900000 {
		t.Errorf("saved at=%q (%d) rt=%q (%d), want the lookup's at-gsl and rotated rt-gsl",
			cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime)
	}
	if !cfg.Features["radar"] || cfg.IP != "10.0.0.1" || cfg.Port != 17783 {
		t.Errorf("the token save changed other fields: ip=%q port=%d features=%v", cfg.IP, cfg.Port, cfg.Features)
	}
	// The subprocess logs through slog's text handler, which doesn't quote values.
	for _, secret := range []string{"at-gsl", "rt-gsl"} {
		if strings.Contains(stderr, secret) {
			t.Errorf("log contains token %q; stderr=%s", secret, stderr)
		}
	}
}

// TestRunCrossServerTestFallbackRestoresTheConfiguredAccessToken: when the server refuses the access
// token a lookup handed back (E011) and the Login falls back to the configured one, the session
// config gets the configured one back, so the next run doesn't start from a token this one saw
// refused. The refresh token the lookup rotated stays on disk, and the refresh after the second
// E011 sends it, not the one it replaced. That refresh fails here (GSL 500), so no save-back runs.
func TestRunCrossServerTestFallbackRestoresTheConfiguredAccessToken(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client, forms := testutil.UseDecryptingGSL(t, lookupTokensReply(), gsl.LoginServerListRespon{Code: "500"})
		base := client.Transport
		client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			resp, err := base.RoundTrip(r)
			if f := forms(); strings.HasSuffix(r.URL.Path, "getserverlist.php") && len(f) > 0 && f[len(f)-1].Get("opt") == "refresh" {
				fmt.Fprintf(os.Stderr, "refresh sent the rotated refresh token: %v\n", f[len(f)-1].Get("rt") == "rt-gsl")
			}
			return resp, err
		})
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("never", &seen)})
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: "rt-1",
			configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	// An hour old: not due for a refresh, so the run looks the server up first.
	atTime := time.Now().Add(-time.Hour).Unix()
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestFallbackRestoresTheConfiguredAccessToken",
		"LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (the refresh after the second E011 failed); stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "trying the configured token at the same address") || !strings.Contains(stderr, "refresh sent the rotated refresh token: true") {
		t.Errorf("want the fallback Login and a refresh with the rotated refresh token; stderr=%s", stderr)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.AccessToken != "at-1" || cfg.AccessTokenTime != atTime || cfg.RefreshToken != "rt-gsl" || cfg.RefreshTokenTime != 1791900001 {
		t.Errorf("saved at=%q (%d) rt=%q (%d), want the configured at-1 back and the rotated rt-gsl kept",
			cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime)
	}
}

// TestRunCrossServerTestRefusedTokenKeepsTheListedAddress: the pre-Login lookup lists a server that
// has moved (gw-a) and hands back an access token it refuses (E011), and the configured address no
// longer answers. A server that answered E011 has the right address, so the Login stays there and
// tries the configured token, as the client's E011 clears only the at and keeps the server it
// connected to (Assembly-CSharp.decompiled.cs:13637-13641, 12812, 1.0.370). The configured address
// is never dialed. The run succeeds with or without a refresh token (the production config has
// none), and the session config gets the listed server with the configured token and its own time;
// a refresh token the lookup rotated is kept. Re-executes the test binary: a run that fails here
// exits the process.
func TestRunCrossServerTestRefusedTokenKeepsTheListedAddress(t *testing.T) {
	cases := []struct {
		name               string
		configRt           string
		replyRt            *gsl.LoginToken
		wantRt             string
		wantRtTime         int64
		wantRotatedRtSaved bool
	}{
		{"no refresh token", "", nil, "", 0, false},
		{"refresh token", "rt-1", nil, "rt-1", 0, false},
		{"refresh token rotated by the lookup", "rt-1", &gsl.LoginToken{Token: "rt-gsl", Time: "1791900001"}, "rt-gsl", 1791900001, true},
	}
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		i, _ := strconv.Atoi(os.Getenv("LASTWAR_TEST_CASE"))
		c := cases[i]
		reply := lookupReply()
		reply.At = &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}
		reply.Rt = c.replyRt
		client := testutil.UseInMemoryGSL(t, reply)
		var seen loginAts
		dial, dialed := recordingDial(map[string]func(*session.GameConn){
			"gw-a.example:10783": tokenCheckingServer("at-1", &seen),
			"10.0.0.1:17783":     nil,
		})
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: "10.0.0.1", port: 17783, zone: "APS1", gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: c.configRt,
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		fmt.Fprintf(os.Stderr, "dialed=%v logins=%v\n", dialed(), seen.get())
		return
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			atTime := time.Now().Add(-time.Hour).Unix()
			cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: "APS1", GameUid: lookupGameUid,
				AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: c.configRt})
			code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefusedTokenKeepsTheListedAddress",
				"LASTWAR_TEST_CASE="+strconv.Itoa(i), "LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (the listed server takes the configured token); stderr=%s", code, stderr)
			}
			if want := "dialed=[gw-a.example:10783 gw-a.example:10783] logins=[at-gsl at-1]"; !strings.Contains(stderr, want) {
				t.Errorf("want %q (both Logins at the listed server, the configured address never dialed); stderr=%s", want, stderr)
			}
			if got := strings.Contains(stderr, "saved the looked-up tokens"); got != c.wantRotatedRtSaved {
				t.Errorf("lookup tokens saved before the Login = %v, want %v; stderr=%s", got, c.wantRotatedRtSaved, stderr)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.IP != lookupGateways || cfg.Port != lookupPort || cfg.Zone != lookupZone {
				t.Errorf("saved %s %s:%d, want the listed %s %s:%d", cfg.Zone, cfg.IP, cfg.Port, lookupZone, lookupGateways, lookupPort)
			}
			if cfg.AccessToken != "at-1" || cfg.AccessTokenTime != atTime || cfg.RefreshToken != c.wantRt || cfg.RefreshTokenTime != c.wantRtTime {
				t.Errorf("saved at=%q (%d) rt=%q (%d), want at-1 (%d) and %q (%d)",
					cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime, atTime, c.wantRt, c.wantRtTime)
			}
		})
	}
}

// TestRunCrossServerTestRefusedLookupTokenFallsBackAtEverySite: every lookup that hands back an
// access token replaces the session's (adoptLookup, as the client's SaveGameLoginToken stores it:
// Assembly-CSharp.decompiled.cs:12777, 7482-7492, 1.0.370), not only the pre-Login one. When the
// listed server refuses that token (E011), the Login is retried there with the token it replaced, as
// the client's E011 clears only the at and keeps the server it connected to (13637-13641, 12812):
// the session's own token with no address configured, after a typed address or the configured one
// stopped answering, and the token this run's refresh issued. No refresh token is needed (the
// production config has none), and with one, a run that already refreshed doesn't end with exit 2.
// The session config gets the listed server and the token that logged in, with its own time.
// Re-executes the test binary: a run that fails here exits the process.
func TestRunCrossServerTestRefusedLookupTokenFallsBackAtEverySite(t *testing.T) {
	handedBack := lookupReply()
	handedBack.At = &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}
	cases := []struct {
		name           string
		ip             string
		port           int
		explicit       bool
		at, rt         string
		replies        []gsl.LoginServerListRespon
		good           string
		wantDialed     string
		wantLogins     string
		wantLookups    int // getserverlist requests, a refresh included
		wantAt         string
		wantConfigTime bool // the saved at keeps the time the session config gave it
	}{
		{"no address configured", "", 0, false, "at-1", "", []gsl.LoginServerListRespon{handedBack}, "at-1",
			"[gw-a.example:10783 gw-a.example:10783]", "[at-gsl at-1]", 1, "at-1", true},
		{"typed address does not answer", "10.0.0.1", 17783, true, "at-1", "", []gsl.LoginServerListRespon{handedBack}, "at-1",
			"[10.0.0.1:17783 gw-a.example:10783 gw-a.example:10783]", "[at-gsl at-1]", 1, "at-1", true},
		{"pre-Login lookup failed, configured address does not answer", "10.0.0.1", 17783, false, "at-1", "",
			[]gsl.LoginServerListRespon{{Code: "500"}, handedBack}, "at-1",
			"[10.0.0.1:17783 gw-a.example:10783 gw-a.example:10783]", "[at-gsl at-1]", 2, "at-1", true},
		{"refreshed before the Login, configured address does not answer", "10.0.0.1", 17783, false, "", "rt-1",
			[]gsl.LoginServerListRespon{refreshReply(), handedBack}, refreshedAt,
			"[10.0.0.1:17783 gw-a.example:10783 gw-a.example:10783]", "[at-gsl " + refreshedAt + "]", 2, refreshedAt, false},
	}
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		i, _ := strconv.Atoi(os.Getenv("LASTWAR_TEST_CASE"))
		c := cases[i]
		client := testutil.UseInMemoryGSL(t, c.replies...)
		lookups := countGetServerList(client)
		var seen loginAts
		dial, dialed := recordingDial(map[string]func(*session.GameConn){
			"gw-a.example:10783": tokenCheckingServer(c.good, &seen),
			"10.0.0.1:17783":     nil,
		})
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: c.ip, port: c.port, ipExplicit: c.explicit, portExplicit: c.explicit,
				zone: lookupZone, gameUid: lookupGameUid, at: c.at, accessTokTime: atTime, refreshTok: c.rt,
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		fmt.Fprintf(os.Stderr, "dialed=%v logins=%v lookups=%d\n", dialed(), seen.get(), lookups())
		return
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			atTime := time.Now().Add(-time.Hour).Unix()
			cfgPath := writeSessionConfig(t, SessionConfig{IP: c.ip, Port: c.port, Zone: lookupZone, GameUid: lookupGameUid,
				AccessToken: c.at, AccessTokenTime: atTime, RefreshToken: c.rt})
			code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefusedLookupTokenFallsBackAtEverySite",
				"LASTWAR_TEST_CASE="+strconv.Itoa(i), "LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
			if code != 0 {
				t.Fatalf("exit code = %d, want 0 (the listed server takes the token the lookup replaced); stderr=%s", code, stderr)
			}
			if want := fmt.Sprintf("dialed=%s logins=%s lookups=%d", c.wantDialed, c.wantLogins, c.wantLookups); !strings.Contains(stderr, want) {
				t.Errorf("want %q; stderr=%s", want, stderr)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.IP != lookupGateways || cfg.Port != lookupPort || cfg.Zone != lookupZone {
				t.Errorf("saved %s %s:%d, want the listed %s %s:%d", cfg.Zone, cfg.IP, cfg.Port, lookupZone, lookupGateways, lookupPort)
			}
			wantAtTime := int64(refreshedAtTime)
			if c.wantConfigTime {
				wantAtTime = atTime
			}
			if cfg.AccessToken != c.wantAt || cfg.AccessTokenTime != wantAtTime {
				t.Errorf("saved at=%q (%d), want %q (%d)", cfg.AccessToken, cfg.AccessTokenTime, c.wantAt, wantAtTime)
			}
		})
	}
}

// TestRunCrossServerTestRefreshesWhenTheRetryAfterE011CannotConnect: when the listed server refuses
// the handed-back token (E011) and the retry there with the configured token can't reach it, the
// E011 still stands: the run refreshes the access token with the refresh token and logs in at the
// listed server again, as the client does after its E011 (Assembly-CSharp.decompiled.cs:13637-13641,
// 1.0.370). Any other answer to the retry, a ban here, ends the run as it does today, with no GSL
// refresh. Re-executes the test binary: a run that fails here exits the process.
func TestRunCrossServerTestRefreshesWhenTheRetryAfterE011CannotConnect(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		reply := gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "gw-a.example", Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: lookupGameUid}},
			At:         &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"},
		}
		client := testutil.UseInMemoryGSL(t, reply, refreshReply())
		base := client.Transport
		client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "getserverlist.php") {
				fmt.Fprintln(os.Stderr, "getserverlist request")
			}
			return base.RoundTrip(r)
		})
		var seen loginAts
		// The listed server's answers, one per connection; nil refuses the dial.
		answers := []func(*session.GameConn){tokenCheckingServer(refreshedAt, &seen), nil, tokenCheckingServer(refreshedAt, &seen)}
		if os.Getenv("LASTWAR_TEST_CASE") == "ban" {
			answers = []func(*session.GameConn){tokenCheckingServer(refreshedAt, &seen), banningServer}
		}
		var mu sync.Mutex
		var addrs []string
		dial := func(addr string, timeout time.Duration) (*session.GameConn, error) {
			mu.Lock()
			addrs = append(addrs, addr)
			var answer func(*session.GameConn)
			if addr == "gw-a.example:10783" && len(answers) > 0 {
				answer, answers = answers[0], answers[1:]
			}
			mu.Unlock()
			if answer == nil {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return session.NewInMemoryGameDial(answer)(addr, timeout)
		}
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: "rt-1",
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		mu.Lock()
		fmt.Fprintf(os.Stderr, "dialed=%v logins=%v\n", addrs, seen.get())
		mu.Unlock()
		return
	}
	atTime := time.Now().Add(-time.Hour).Unix()
	config := SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"}
	run := func(t *testing.T, name string) (string, int, string) {
		cfgPath := writeSessionConfig(t, config)
		code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshesWhenTheRetryAfterE011CannotConnect",
			"LASTWAR_TEST_CASE="+name, "LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
		return cfgPath, code, stderr
	}

	t.Run("the retry can't connect", func(t *testing.T) {
		cfgPath, code, stderr := run(t, "unreachable")
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (the refreshed token logs in at the listed server); stderr=%s", code, stderr)
		}
		if want := "dialed=[gw-a.example:10783 gw-a.example:10783 gw-a.example:10783] logins=[at-gsl " + refreshedAt + "]"; !strings.Contains(stderr, want) {
			t.Errorf("want %q; stderr=%s", want, stderr)
		}
		if n := strings.Count(stderr, "getserverlist request"); n != 2 {
			t.Errorf("getserverlist requests = %d, want 2 (the lookup, then the refresh); stderr=%s", n, stderr)
		}
		cfg := wantRefreshedTokens(t, cfgPath)
		if cfg.IP != "gw-a.example" || cfg.Port != lookupPort || cfg.Zone != lookupZone {
			t.Errorf("saved %s %s:%d, want the listed %s gw-a.example:%d", cfg.Zone, cfg.IP, cfg.Port, lookupZone, lookupPort)
		}
	})
	t.Run("the retry is banned", func(t *testing.T) {
		cfgPath, code, stderr := run(t, "ban")
		if code != exitCodeAccountBlocked {
			t.Errorf("exit code = %d, want %d; stderr=%s", code, exitCodeAccountBlocked, stderr)
		}
		if n := strings.Count(stderr, "getserverlist request"); n != 1 {
			t.Errorf("getserverlist requests = %d, want only the lookup (no refresh after a ban); stderr=%s", n, stderr)
		}
		if cfg := readSessionConfig(t, cfgPath); cfg.AccessToken != "at-1" || cfg.RefreshToken != "rt-1" || cfg.IP != "10.0.0.1" {
			t.Errorf("saved at=%q rt=%q ip=%q, want the session config as it was", cfg.AccessToken, cfg.RefreshToken, cfg.IP)
		}
	})
}

// TestRunCrossServerTestNoLookupWithExplicitAddress: an address typed on the command line is used
// as given, with no lookup before the Login.
func TestRunCrossServerTestNoLookupWithExplicitAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	lookups := countGetServerList(client)
	dial, dialed := recordingDial(nil)

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", ipExplicit: true,
			httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if n := lookups(); n != 0 {
		t.Errorf("getserverlist requests = %d, want 0", n)
	}
	if got := dialed(); len(got) != 1 || got[0] != "10.0.0.1:17783" {
		t.Errorf("dialed %v, want [10.0.0.1:17783]", got)
	}
}

// TestRunCrossServerTestNoRetryAtAServerWithoutTheRole: when the configured address can't be
// reached and the retry lookup lists only a server that doesn't carry the role's gameUid (GSL's
// lastLoggedServer or first entry), the Login is not retried there, and the run fails as before.
func TestRunCrossServerTestNoRetryAtAServerWithoutTheRole(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		other := gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "901", IP: "other.example", Port: testutil.FlexPort(10901), Zone: "APS901"}},
		}
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "500"}, other)
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			configSavePath: t.TempDir() + "/session.json", httpClient: client,
			dialGame: func(addr string, _ time.Duration) (*session.GameConn, error) {
				fmt.Fprintf(os.Stderr, "dial %s\n", addr)
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoRetryAtAServerWithoutTheRole$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
		t.Fatalf("subprocess err = %v, want exit 1; stderr=%s", runErr, stderr.String())
	}
	log := stderr.String()
	if !strings.Contains(log, "without this role's gameUid; not retrying there") {
		t.Errorf("want the not-retrying warning; stderr=%s", log)
	}
	if strings.Contains(log, "dial other.example") || strings.Count(log, "dial 10.0.0.1:17783") != 1 {
		t.Errorf("want one dial of the configured address and none of other.example; stderr=%s", log)
	}
}

// TestRunCrossServerTestLooksUpAfterUnreachableAddress: when the configured address refuses every
// dial, or accepts the connection but never answers the Login (the August 2026 port move), and the
// pre-Login lookup had failed, one more GSL lookup finds the current server, the Login is retried
// there, and the new address is persisted.
func TestRunCrossServerTestLooksUpAfterUnreachableAddress(t *testing.T) {
	cases := map[string]func(*session.GameConn){
		"dial refused":     nil,
		"login unanswered": func(*session.GameConn) {}, // the drain reads the Login and never replies
	}
	for name, stale := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "500"}, lookupReply())
			lookups := countGetServerList(client)
			dial, dialed := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": stale})
			cfgPath := t.TempDir() + "/session.json"

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if n := lookups(); n != 2 {
				t.Errorf("getserverlist requests = %d, want 2 (the failed pre-Login lookup, then the retry)", n)
			}
			want := []string{"10.0.0.1:17783", "gw-a.example:10783"}
			if got := dialed(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("dialed %v, want %v", got, want)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.IP != lookupGateways || cfg.Port != lookupPort {
				t.Errorf("persisted %s:%d, want %s:%d", cfg.IP, cfg.Port, lookupGateways, lookupPort)
			}
		})
	}
}

// TestRunCrossServerTestNoLookupOnTokenRejection: an E011 rejection comes from a server that
// answered, so another address can't help, and a GSL call is the last thing a token problem should
// trigger. The run exits 2 without looking anything up after the Login (the one lookup is the
// pre-Login one, which lists the configured server). Re-executes the test binary because
// runCrossServerTest exits the process on this path.
func TestRunCrossServerTestNoLookupOnTokenRejection(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "10.0.0.1", Port: testutil.FlexPort(17783), Zone: lookupZone, GameUid: lookupGameUid}},
		})
		reject := func(server *session.GameConn) {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
			resp := sfs.NewSFSObject()
			resp.PutValue("ep", sfs.SFSValue{Type: 16, Val: []string{"E011"}}) // 16 = UTF_STRING_ARRAY
			resp.PutShort("ec", 28)
			_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
		}
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": reject})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			httpClient: client, dialGame: dial,
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoLookupOnTokenRejection$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("subprocess exit code = %d, want 2; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	if n := strings.Count(stderr.String(), "GSL getserverlist: looking up the game server"); n != 1 {
		t.Errorf("GSL lookups = %d, want only the pre-Login one (an E011 must not trigger another); stderr=%s", n, stderr.String())
	}
}

// TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer: when GSL lists the very address that
// just failed, a second dial can't help, so the run reports the original error and exits 1 after
// one dial. The pre-Login lookup fails here, so the retry lookup is the one that lists it.
// Re-executes the test binary because runCrossServerTest exits the process on this path.
func TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "500"}, lookupReply())
		// The configured gateways are the looked-up ones in another order, and all refuse.
		runCrossServerTest(crossServerTestOpts{
			ip: "gw-c.example|gw-b.example|gw-a.example", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			httpClient: client, dialGame: func(addr string, _ time.Duration) (*session.GameConn, error) {
				fmt.Fprintf(os.Stderr, "dial %s\n", addr)
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer$")
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
	if !strings.Contains(log, "GSL lists the game server that just failed; not retrying") {
		t.Errorf("want the not-retrying warning; stderr=%s", log)
	}
	// One pass over the configured gateways, none after the lookup.
	if n := strings.Count(log, "dial gw-"); n != 3 {
		t.Errorf("dialed %d times, want 3 (one pass over the configured gateways); stderr=%s", n, log)
	}
}

func TestConnectFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dial refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"every gateway refused", errors.Join(&net.OpError{Op: "dial"}, &net.OpError{Op: "dial"}), true},
		{"login unanswered", session.DeadlineExceededError{}, true},
		{"connection dropped", session.SendStageError{Err: errors.New("broken pipe")}, true},
		{"auth rejected", fmt.Errorf("CROSS-SERVER LOGIN FAILED: %w", session.ErrAuthRejected), false},
		{"token rejected", fmt.Errorf("CROSS-SERVER LOGIN FAILED: %w", session.ErrTokenRejected), false},
		{"kicked", &session.SessionEndedError{Err: session.ErrKicked}, false},
		{"plain error", errors.New("cross-server login: no access token given"), false},
	}
	for _, c := range cases {
		if got := connectFailed(c.err); got != c.want {
			t.Errorf("%s: connectFailed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSameGameServer(t *testing.T) {
	srv := auth.GameServer{IP: "a|b", Port: 10783, Zone: "APS783"}
	cases := []struct {
		name string
		ip   string
		port int
		zone string
		want bool
	}{
		{"same", "a|b", 10783, "APS783", true},
		{"same hosts, other order", "b|a", 10783, "APS783", true},
		{"other port", "a|b", 17783, "APS783", false},
		{"other zone", "a|b", 10783, "APS8092", false},
		{"captured IPs vs hostnames", "1.2.3.4|5.6.7.8", 10783, "APS783", false},
	}
	for _, c := range cases {
		if got := sameGameServer(srv, c.ip, c.port, c.zone); got != c.want {
			t.Errorf("%s: sameGameServer = %v, want %v", c.name, got, c.want)
		}
	}
}
