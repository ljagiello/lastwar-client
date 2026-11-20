package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
	"lastwar-client/internal/testutil"
)

// gwAOnly is a lookup reply listing one gateway, gw-a, so a refused dial there can't move on to
// the zone's other gateways.
func gwAOnly(at, rt *gsl.LoginToken) gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{ID: "783", IP: "gw-a.example", Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: lookupGameUid}},
		At:         at,
		Rt:         rt,
	}
}

// sequencedDial answers each dial of addr with the next of answers (nil refuses the dial, as an
// unreachable gateway does), refuses every other address, and records the addresses dialed.
func sequencedDial(addr string, answers ...func(*session.GameConn)) (dial func(string, time.Duration) (*session.GameConn, error), dialed func() []string) {
	var mu sync.Mutex
	var addrs []string
	return func(a string, timeout time.Duration) (*session.GameConn, error) {
			mu.Lock()
			addrs = append(addrs, a)
			var answer func(*session.GameConn)
			if a == addr && len(answers) > 0 {
				answer, answers = answers[0], answers[1:]
			}
			mu.Unlock()
			if answer == nil {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return session.NewInMemoryGameDial(answer)(a, timeout)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), addrs...)
		}
}

// TestRunCrossServerTestRestoresTheConfiguredTokenAtEverySite (final review N1): a lookup that
// hands back an access token and rotates the refresh token saves both before the Login. When the
// listed server refuses that token and the configured one too, the configured access token goes
// back on disk at the site with no configured address and at the re-lookup after the configured
// address stopped answering, as at the pre-Login one
// (TestRunCrossServerTestFallbackRestoresTheConfiguredAccessToken); the rotated refresh token stays.
// The refresh after the second E011 fails (GSL 500), so no save-back runs after it.
func TestRunCrossServerTestRestoresTheConfiguredTokenAtEverySite(t *testing.T) {
	tokens := gwAOnly(&gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}, &gsl.LoginToken{Token: "rt-gsl", Time: "1791900001"})
	failed := gsl.LoginServerListRespon{Code: "500"}
	cases := []struct {
		name       string
		ip         string
		port       int
		replies    []gsl.LoginServerListRespon
		wantDialed string
	}{
		{"no address configured", "", 0, []gsl.LoginServerListRespon{tokens, failed},
			"[gw-a.example:10783 gw-a.example:10783]"},
		{"re-lookup after the configured address stopped answering", "10.0.0.1", 17783, []gsl.LoginServerListRespon{failed, tokens, failed},
			"[10.0.0.1:17783 gw-a.example:10783 gw-a.example:10783]"},
	}
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		i, _ := strconv.Atoi(os.Getenv("LASTWAR_TEST_CASE"))
		c := cases[i]
		client := testutil.UseInMemoryGSL(t, c.replies...)
		var seen loginAts
		dial, dialed := sequencedDial("gw-a.example:10783", tokenCheckingServer("never", &seen), tokenCheckingServer("never", &seen))
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		defer func() { fmt.Fprintf(os.Stderr, "dialed=%v logins=%v\n", dialed(), seen.get()) }()
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: c.ip, port: c.port, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: "rt-1",
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		return
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			atTime := time.Now().Add(-time.Hour).Unix()
			cfgPath := writeSessionConfig(t, SessionConfig{IP: c.ip, Port: c.port, Zone: lookupZone, GameUid: lookupGameUid,
				AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"})
			code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRestoresTheConfiguredTokenAtEverySite",
				"LASTWAR_TEST_CASE="+strconv.Itoa(i), "LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
			if code != 1 {
				t.Errorf("exit code = %d, want 1 (the refresh after the second E011 failed); stderr=%s", code, stderr)
			}
			if !strings.Contains(stderr, "trying the configured token at the same address") {
				t.Errorf("want the retry with the configured token; stderr=%s", stderr)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.AccessToken != "at-1" || cfg.AccessTokenTime != atTime || cfg.RefreshToken != "rt-gsl" || cfg.RefreshTokenTime != 1791900001 {
				t.Errorf("saved at=%q (%d) rt=%q (%d), want the configured at-1 (%d) back and the rotated rt-gsl kept",
					cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, cfg.RefreshTokenTime, atTime)
			}
		})
	}
}

// TestRunCrossServerTestRefreshesAfterTheReLookupsRefusedToken (final review tests-tables-docs 5):
// the configured address doesn't answer, the re-lookup lists gw-a with a handed-back token, gw-a
// refuses it (E011), and the retry there with the configured token can't connect. That E011 still
// stands, so the run refreshes with its refresh token and logs in at gw-a, as the client does after
// an E011 (Assembly-CSharp.decompiled.cs:13637-13641, 1.0.370): retryReplacedToken records the
// E011 for the refresh at the re-lookup site too.
func TestRunCrossServerTestRefreshesAfterTheReLookupsRefusedToken(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		handedBack := gwAOnly(&gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}, nil)
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "500"}, handedBack, refreshReply())
		var seen loginAts
		dial, dialed := sequencedDial("gw-a.example:10783", tokenCheckingServer(refreshedAt, &seen), nil, tokenCheckingServer(refreshedAt, &seen))
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		defer func() { fmt.Fprintf(os.Stderr, "dialed=%v logins=%v\n", dialed(), seen.get()) }()
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: "rt-1",
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		return
	}
	atTime := time.Now().Add(-time.Hour).Unix()
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshesAfterTheReLookupsRefusedToken",
		"LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (the refreshed token logs in at gw-a); stderr=%s", code, stderr)
	}
	if want := "dialed=[10.0.0.1:17783 gw-a.example:10783 gw-a.example:10783 gw-a.example:10783] logins=[at-gsl " + refreshedAt + "]"; !strings.Contains(stderr, want) {
		t.Errorf("want %q; stderr=%s", want, stderr)
	}
	cfg := wantRefreshedTokens(t, cfgPath)
	if cfg.IP != "gw-a.example" || cfg.Port != lookupPort {
		t.Errorf("saved %s:%d, want gw-a.example:%d", cfg.IP, cfg.Port, lookupPort)
	}
}

// seasonWindowServer refuses the Login with ec=28 and the given ep[0], as the server does while a
// season opens (E015) or closes (E016).
func seasonWindowServer(code string) func(*session.GameConn) {
	return func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutValue("ep", sfs.SFSValue{Type: 16, Val: []string{code}}) // 16 = UTF_STRING_ARRAY
		resp.PutShort("ec", 28)
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
	}
}

// TestRunCrossServerTestSeasonWindowExits1 (final review prod m7): a Login refused with E015
// (season opening) or E016 (season closing) is a server state, not a credential problem: the
// client shows reload-or-quit and clears nothing (Assembly-CSharp.decompiled.cs:13474-13491,
// 1.0.370). The run exits 1, the generic failure the next run retries, never 2 (re-auth), and
// makes no refresh, no fallback and no change to the session config.
func TestRunCrossServerTestSeasonWindowExits1(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, configuredServerReply())
		base := client.Transport
		client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "getserverlist.php") {
				fmt.Fprintln(os.Stderr, "getserverlist request")
			}
			return base.RoundTrip(r)
		})
		inner, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": seasonWindowServer(os.Getenv("LASTWAR_TEST_CODE"))})
		dial := func(addr string, timeout time.Duration) (*session.GameConn, error) {
			fmt.Fprintln(os.Stderr, "dial "+addr)
			return inner(addr, timeout)
		}
		atTime, _ := strconv.ParseInt(os.Getenv("LASTWAR_TEST_AT_TIME"), 10, 64)
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", accessTokTime: atTime, refreshTok: "rt-1",
				configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		return
	}
	for _, code := range []string{"E015", "E016"} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			atTime := time.Now().Add(-time.Hour).Unix()
			before := SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid,
				AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"}
			cfgPath := writeSessionConfig(t, before)
			exit, stderr := runRefreshHelper(t, "TestRunCrossServerTestSeasonWindowExits1",
				"LASTWAR_TEST_CODE="+code, "LASTWAR_TEST_CFG="+cfgPath, "LASTWAR_TEST_AT_TIME="+strconv.FormatInt(atTime, 10))
			if exit != 1 {
				t.Errorf("exit code = %d, want 1; stderr=%s", exit, stderr)
			}
			if strings.Count(stderr, "dial ") != 1 || !strings.Contains(stderr, "dial 10.0.0.1:10783") || strings.Count(stderr, "getserverlist request") != 1 {
				t.Errorf("want one Login and only the pre-Login lookup (no refresh, no fallback); stderr=%s", stderr)
			}
			if diff := sessionConfigDiff(readSessionConfig(t, cfgPath), before); len(diff) > 0 {
				t.Errorf("the session config changed in %v", diff)
			}
		})
	}
}

// TestRunCrossServerTestTypedAccessTokenIsNotSaved (final review M6): an access token typed as
// -cs-at is this run's choice, so a save-back keeps the session config's own access token and its
// time: the one it had, after a looked-up address, and the one a GSL lookup saved there, after a
// rotated refresh token. Run passes accessTokTime 0 with a typed -cs-at (main.go), and so does this.
func TestRunCrossServerTestTypedAccessTokenIsNotSaved(t *testing.T) {
	const atTime = int64(1791000000)
	for _, tc := range []struct {
		name           string
		reply          gsl.LoginServerListRespon
		wantAt         string
		wantAtTime     int64
		wantRt         string
		wantSavedAddr  bool
		configuredAddr string
	}{
		{"a looked-up address", lookupReply(), "at-1", atTime, "rt-1", true, "10.0.0.1"},
		{"a rotated refresh token", gwAOnly(&gsl.LoginToken{Token: "at-gsl", Time: "1791900000"}, &gsl.LoginToken{Token: "rt-gsl", Time: "1791900001"}),
			"at-gsl", 1791900000, "rt-gsl", false, "gw-a.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, tc.reply)
			var seen loginAts
			dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": tokenCheckingServer("at-typed", &seen)})
			path := writeSessionConfig(t, SessionConfig{IP: tc.configuredAddr, Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid,
				DeviceID: "dev-1", AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1"})
			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: tc.configuredAddr, port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, deviceID: "dev-1",
					at: "at-typed", atExplicit: true, refreshTok: "rt-1",
					configSavePath: path, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})
			if got := seen.get(); len(got) != 1 || got[0] != "at-typed" {
				t.Fatalf("Logins %v, want one with the typed at-typed", got)
			}
			cfg := readSessionConfig(t, path)
			if cfg.AccessToken != tc.wantAt || cfg.AccessTokenTime != tc.wantAtTime || cfg.RefreshToken != tc.wantRt {
				t.Errorf("saved at=%q (%d) rt=%q, want %q (%d) %q", cfg.AccessToken, cfg.AccessTokenTime, cfg.RefreshToken, tc.wantAt, tc.wantAtTime, tc.wantRt)
			}
			if saved := cfg.IP == lookupGateways; saved != tc.wantSavedAddr {
				t.Errorf("saved ip %q; want the looked-up address saved = %v", cfg.IP, tc.wantSavedAddr)
			}
		})
	}
}

// TestRunCrossServerTestAndroidInstallAnchorsToTheRunsDeviceID (final review M5): a new Android
// install takes the install time of the session's ShuMei install for the Login's deviceId, which
// is the -cs-deviceid one when it is given, not the identity's own.
func TestRunCrossServerTestAndroidInstallAnchorsToTheRunsDeviceID(t *testing.T) {
	fit := time.Date(1999, 6, 1, 12, 0, 0, 0, time.UTC)
	install := shumei.Install{DeviceID: "override_n3d", FirstInstallTime: fit}
	logins := make(chan *sfs.SFSObject, 8)
	path := writeTestSessionConfig(t, &SessionConfig{ShumeiInstall: install})
	runSaveBackWith(t, crossServerTestOpts{deviceID: "override_n3d", shumeiInstall: install, configSavePath: path}, loginParamsServer(logins))
	close(logins)
	p := <-logins
	if p == nil {
		t.Fatal("the game server saw no Login")
	}
	if got := p.GetString("deviceId"); got != "override_n3d" {
		t.Fatalf("Login deviceId = %q, want the -cs-deviceid", got)
	}
	if installed, _ := loginInstallTimes(t, p); !installed.Equal(fit) {
		t.Errorf("ta #install_time = %v, want the -cs-deviceid's ShuMei install time %v", installed, fit)
	}
}

// TestSaveSessionTokensSkipsAnUnchangedWrite (final review M5): tokens equal to the ones on disk
// write nothing; the file keeps its bytes, a key SessionConfig doesn't model included.
func TestSaveSessionTokensSkipsAnUnchangedWrite(t *testing.T) {
	path := writeSessionConfig(t, SessionConfig{})
	const raw = `{"deviceId":"dev-1","accessToken":"at-1","accessTokenTime":5,"refreshToken":"rt-1","refreshTokenTime":6,"note":"kept"}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	saveRefreshedTokens(path, "dev-1", auth.Tokens{AccessTok: "at-1", RefreshTok: "rt-1", AccessTokTime: 5, RefreshTokTime: 6})
	if b, _ := os.ReadFile(path); string(b) != raw {
		t.Errorf("an unchanged token save rewrote the file:\n%s", b)
	}
	saveRefreshedTokens(path, "dev-1", auth.Tokens{AccessTok: "at-2", RefreshTok: "rt-1", AccessTokTime: 7, RefreshTokTime: 6})
	if cfg := readSessionConfig(t, path); cfg.AccessToken != "at-2" || cfg.AccessTokenTime != 7 {
		t.Errorf("a changed token save wrote at=%q (%d), want at-2 (7)", cfg.AccessToken, cfg.AccessTokenTime)
	}
}
