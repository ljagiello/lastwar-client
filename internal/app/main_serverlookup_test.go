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
// fallback. When every gateway the pre-Login lookup listed refuses, the Login goes to the configured
// address; when the server refuses the token the lookup handed back (E011), the configured token is
// tried. Either way the run succeeds and the session config is left as it was, except for a refresh
// token the lookup rotated: that one is the newest and stays.
func TestRunCrossServerTestFallsBackToTheConfiguredLogin(t *testing.T) {
	cases := []struct {
		name       string
		reply      gsl.LoginServerListRespon
		handlers   func(seen *loginAts) map[string]func(*session.GameConn)
		wantDialed string
		wantLogins string
		wantRt     string // "" = the session config is left byte for byte as it was
	}{
		{"listed gateways refuse", lookupReply(), func(seen *loginAts) map[string]func(*session.GameConn) {
			return map[string]func(*session.GameConn){
				"gw-a.example:10783": nil, "gw-b.example:10783": nil, "gw-c.example:10783": nil,
				"10.0.0.1:17783": tokenCheckingServer("at-1", seen),
			}
		}, "[gw-a.example:10783 gw-b.example:10783 gw-c.example:10783 10.0.0.1:17783]", "[at-1]", ""},
		{"handed-back token refused", gsl.LoginServerListRespon{
			Code:       "0",
			ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "10.0.0.1", Port: testutil.FlexPort(17783), Zone: lookupZone, GameUid: lookupGameUid}},
			At:         &gsl.LoginToken{Token: "at-gsl", Time: "1791900000"},
		}, func(seen *loginAts) map[string]func(*session.GameConn) {
			return map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("at-1", seen)}
		}, "[10.0.0.1:17783 10.0.0.1:17783]", "[at-gsl at-1]", ""},
		{"handed-back token refused, refresh token rotated", lookupTokensReply(), func(seen *loginAts) map[string]func(*session.GameConn) {
			return map[string]func(*session.GameConn){"10.0.0.1:17783": tokenCheckingServer("at-1", seen)}
		}, "[10.0.0.1:17783 10.0.0.1:17783]", "[at-gsl at-1]", "rt-gsl"},
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
			if c.wantRt == "" {
				if after, err := os.ReadFile(cfgPath); err != nil || !bytes.Equal(after, before) {
					t.Errorf("session config changed (err %v):\n%s", err, after)
				}
				return
			}
			saved := readSessionConfig(t, cfgPath)
			if saved.AccessToken != "at-1" || saved.AccessTokenTime != 0 || saved.RefreshToken != c.wantRt || saved.IP != "10.0.0.1" {
				t.Errorf("saved at=%q (%d) rt=%q ip=%q, want the configured at-1 and address with the rotated %s",
					saved.AccessToken, saved.AccessTokenTime, saved.RefreshToken, saved.IP, c.wantRt)
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
	if !strings.Contains(stderr, "trying the configured address and token") || !strings.Contains(stderr, "refresh sent the rotated refresh token: true") {
		t.Errorf("want the fallback Login and a refresh with the rotated refresh token; stderr=%s", stderr)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.AccessToken != "at-1" || cfg.AccessTokenTime != atTime || cfg.RefreshToken != "rt-gsl" || cfg.RefreshTokenTime != 1791900001 {
		t.Errorf("saved at=%q (%d) rt=%q (%d), want the configured at-1 back and the rotated rt-gsl kept",
			cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime)
	}
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
