package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
	"lastwar-client/internal/testutil"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The token pair a refresh in these tests hands out.
const (
	refreshedAt     = "at-new"
	refreshedRt     = "rt-new"
	refreshedAtTime = 1791900000
	refreshedRtTime = 1791900001
)

// refreshReply is a GSL opt=refresh reply: the role's server plus a fresh at and a rotated rt.
func refreshReply() gsl.LoginServerListRespon {
	r := lookupReply()
	r.At = &gsl.LoginToken{Token: refreshedAt, Time: gsl.FlexString(strconv.Itoa(refreshedAtTime))}
	r.Rt = &gsl.LoginToken{Token: refreshedRt, Time: gsl.FlexString(strconv.Itoa(refreshedRtTime))}
	return r
}

// loginAts records the p.at of every Login a tokenCheckingServer reads.
type loginAts struct {
	mu  sync.Mutex
	ats []string
}

func (l *loginAts) add(at string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ats = append(l.ats, at)
}

func (l *loginAts) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ats...)
}

// tokenCheckingServer accepts a Login carrying the good access token (then sends init) and rejects
// any other with ec=28/E011, the way the game server treats a superseded token.
func tokenCheckingServer(good string, seen *loginAts) func(*session.GameConn) {
	return func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		at := ""
		if pv, ok := env.Content.Get("p"); ok {
			if pObj, ok := pv.Val.(*sfs.SFSObject); ok {
				at = pObj.GetString("at")
			}
		}
		seen.add(at)
		resp := sfs.NewSFSObject()
		if at != good {
			resp.PutValue("ep", sfs.SFSValue{Type: 16, Val: []string{"E011"}}) // 16 = UTF_STRING_ARRAY
			resp.PutShort("ec", 28)
			_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
			return
		}
		resp.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
}

// writeSessionConfig writes cfg to a fresh path and returns it.
func writeSessionConfig(t *testing.T, cfg SessionConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveSessionConfig(&cfg, path); err != nil {
		t.Fatalf("write session config: %v", err)
	}
	return path
}

// wantRefreshedTokens checks that the session config at path holds the refreshed pair.
func wantRefreshedTokens(t *testing.T, path string) SessionConfig {
	t.Helper()
	cfg := readSessionConfig(t, path)
	if cfg.AccessToken != refreshedAt || cfg.RefreshToken != refreshedRt {
		t.Errorf("persisted at/rt = %q/%q, want %q/%q", cfg.AccessToken, cfg.RefreshToken, refreshedAt, refreshedRt)
	}
	if cfg.AccessTokenTime != refreshedAtTime || cfg.RefreshTokenTime != refreshedRtTime {
		t.Errorf("persisted at.time/rt.time = %d/%d, want %d/%d", cfg.AccessTokenTime, cfg.RefreshTokenTime, refreshedAtTime, refreshedRtTime)
	}
	return cfg
}

// TestRunCrossServerTestRefreshesAfterE011: a Login rejected with E011 is followed by one GSL
// opt=refresh with the session config's refresh token and one more Login with the fresh access
// token, as in the real client, and the new pair lands in the session config.
func TestRunCrossServerTestRefreshesAfterE011(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, refreshReply())
	gslCalls := countGetServerList(client)
	var seen loginAts
	dial, dialed := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer(refreshedAt, &seen)})
	features := map[string]bool{"radar": true}
	cfgPath := writeSessionConfig(t, SessionConfig{
		IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-old", RefreshToken: "rt-old", Features: features,
	})

	synctest.Test(t, func(t *testing.T) {
		now := time.Now().Unix()
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", accessTokTime: now - 3600, refreshTokTime: now - 3600,
			features: features, configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if got, want := seen.get(), []string{"at-old", refreshedAt}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("Login access tokens = %v, want %v", got, want)
	}
	if n := gslCalls(); n != 1 {
		t.Errorf("getserverlist requests = %d, want 1 (the refresh)", n)
	}
	if got := dialed(); len(got) != 2 {
		t.Errorf("dialed %v, want the configured server twice", got)
	}
	cfg := wantRefreshedTokens(t, cfgPath)
	if cfg.IP != "10.0.0.1" || cfg.Port != lookupPort || !cfg.Features["radar"] {
		t.Errorf("the save lost other fields: ip=%q port=%d features=%v", cfg.IP, cfg.Port, cfg.Features)
	}
}

// TestRunCrossServerTestRefreshesBeforeLogin: an access token whose at.time is 30 days old, or one
// that is missing, is refreshed before the Login (gsl.ChooseOpt), so the Login goes out once, with
// the fresh token.
func TestRunCrossServerTestRefreshesBeforeLogin(t *testing.T) {
	cases := []struct {
		name string
		at   string
		age  time.Duration
	}{
		{"30 days old", "at-old", 30 * 24 * time.Hour},
		{"missing", "", time.Hour},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, refreshReply())
			gslCalls := countGetServerList(client)
			var seen loginAts
			dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer(refreshedAt, &seen)})
			cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: c.at, RefreshToken: "rt-old"})

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
					at: c.at, refreshTok: "rt-old", accessTokTime: time.Now().Add(-c.age).Unix(),
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if got := seen.get(); fmt.Sprint(got) != fmt.Sprint([]string{refreshedAt}) {
				t.Errorf("Login access tokens = %v, want only the refreshed one", got)
			}
			if n := gslCalls(); n != 1 {
				t.Errorf("getserverlist requests = %d, want 1 (the refresh)", n)
			}
			wantRefreshedTokens(t, cfgPath)
		})
	}
}

// TestRunCrossServerTestFreshTokenNeedsNoRefresh: a working access token younger than 30 days is
// used as it is, with no GSL call, and nothing is rewritten.
func TestRunCrossServerTestFreshTokenNeedsNoRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, refreshReply())
	gslCalls := countGetServerList(client)
	var seen loginAts
	dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("at-old", &seen)})
	cfgPath := filepath.Join(t.TempDir(), "session.json")

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", accessTokTime: time.Now().Add(-29 * 24 * time.Hour).Unix(),
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if got := seen.get(); fmt.Sprint(got) != fmt.Sprint([]string{"at-old"}) {
		t.Errorf("Login access tokens = %v, want [at-old]", got)
	}
	if n := gslCalls(); n != 0 {
		t.Errorf("getserverlist requests = %d, want 0", n)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("session config written although nothing changed (stat err = %v)", err)
	}
}

// TestRunCrossServerTestSaveBackKeepsRefreshToken: a server-address save-back rewrites the session
// config from scratch, and used to drop refreshToken and both token times, stranding the device's
// refresh token after a server lookup or a redirect.
func TestRunCrossServerTestSaveBackKeepsRefreshToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	dial, _ := recordingDial(nil)
	cfgPath := filepath.Join(t.TempDir(), "session.json")
	var atTime int64

	synctest.Test(t, func(t *testing.T) {
		atTime = time.Now().Add(-time.Hour).Unix()
		runCrossServerTest(crossServerTestOpts{
			zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			refreshTok: "rt-1", accessTokTime: atTime, refreshTokTime: atTime - 1,
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	cfg := readSessionConfig(t, cfgPath)
	if cfg.IP != lookupGateways {
		t.Fatalf("persisted ip %q, want the looked-up %q (the save-back didn't run)", cfg.IP, lookupGateways)
	}
	if cfg.AccessToken != "at-1" || cfg.RefreshToken != "rt-1" || cfg.AccessTokenTime != atTime || cfg.RefreshTokenTime != atTime-1 {
		t.Errorf("save-back tokens: at=%q rt=%q at.time=%d rt.time=%d, want at-1 rt-1 %d %d",
			cfg.AccessToken, cfg.RefreshToken, cfg.AccessTokenTime, cfg.RefreshTokenTime, atTime, atTime-1)
	}
}

// runRefreshHelper re-executes the test binary to run name's helper branch, for paths where
// runCrossServerTest exits the process, and returns the exit code and stderr.
func runRefreshHelper(t *testing.T, name string, env ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+name+"$")
	cmd.Env = append(append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1"), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not run: %v; stderr=%s", err, stderr.String())
	}
	return exitErr.ExitCode(), stderr.String()
}

// TestRunCrossServerTestRefreshSavedBeforeLogin: the refreshed pair is in the session config even
// when the Login after the refresh fails, so a rotated refresh token is never lost. The session
// config's other fields stay as they were on disk.
func TestRunCrossServerTestRefreshSavedBeforeLogin(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, refreshReply())
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{
		IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid,
		AccessToken: "at-old", RefreshToken: "rt-old", Features: map[string]bool{"radar": true}, DuelPolicy: map[string]string{"radar": "always"},
	})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshSavedBeforeLogin", "LASTWAR_TEST_CFG="+cfgPath)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	cfg := wantRefreshedTokens(t, cfgPath)
	if !cfg.Features["radar"] || cfg.DuelPolicy["radar"] != "always" || cfg.IP != "10.0.0.1" {
		t.Errorf("the token save lost other fields: %+v", struct {
			IP       string
			Features map[string]bool
			Duel     map[string]string
		}{cfg.IP, cfg.Features, cfg.DuelPolicy})
	}
	if !strings.Contains(stderr, "rejected even after a GSL refresh") {
		t.Errorf("want the after-refresh E011 message; stderr=%s", stderr)
	}
	if strings.Count(stderr, "refreshing the access token") != 1 {
		t.Errorf("want exactly one refresh (no loop); stderr=%s", stderr)
	}
	// The subprocess logs through slog's text handler, which doesn't quote values.
	for _, secret := range []string{"at-old", "rt-old", refreshedAt, refreshedRt} {
		if strings.Contains(stderr, secret) {
			t.Errorf("log contains token %q; stderr=%s", secret, stderr)
		}
	}
}

// TestRunCrossServerTestRefreshRejectedExits2: GSL code 211 on the refresh after an E011 means the
// device's credentials are gone, so the run exits 2 with the re-auth message and saves nothing.
func TestRunCrossServerTestRefreshRejectedExits2(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "211"})
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-old", RefreshToken: "rt-old"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshRejectedExits2", "LASTWAR_TEST_CFG="+cfgPath)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "GSL rejected this device's credentials") {
		t.Errorf("want the re-auth message; stderr=%s", stderr)
	}
	if cfg := readSessionConfig(t, cfgPath); cfg.AccessToken != "at-old" || cfg.RefreshToken != "rt-old" {
		t.Errorf("a rejected refresh changed the saved tokens: at=%q rt=%q", cfg.AccessToken, cfg.RefreshToken)
	}
}

// TestRunCrossServerTestNoRefreshTokenKeepsRecaptureHint: without a refresh token an E011 still
// exits 2 with the recapture instructions and no GSL call, as before.
func TestRunCrossServerTestNoRefreshTokenKeepsRecaptureHint(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, refreshReply())
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-old",
			httpClient: client, dialGame: dial,
		})
		return
	}
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestNoRefreshTokenKeepsRecaptureHint")
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "Recapture:") || strings.Contains(stderr, "GSL getserverlist") {
		t.Errorf("want the recapture hint and no GSL call; stderr=%s", stderr)
	}
}

// TestMainUsesSessionConfigRefreshToken runs Run() end to end. The fake GSL lists the fake game
// server, so a config needs no address of its own.
//   - A config holding only a refresh token (no address, no access token) selects the reconnect by
//     itself, refreshes (opt=refresh), logs in with the fresh token, and saves the pair.
//   - A config whose accessTokenTime is 31 days old refreshes before the Login.
//   - An explicit -cs-at isn't judged by the config's accessTokenTime: it is used as given.
//   - With -cs-deviceid, the config's refresh token (bound to the config's device) is left alone.
func TestMainUsesSessionConfigRefreshToken(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		var seen loginAts
		addr := session.StartFakeGameServer(t, tokenCheckingServer(os.Getenv("LASTWAR_TEST_GOOD"), &seen))
		host, port := testutil.SplitHostPortInt(t, addr)
		reply := refreshReply()
		if os.Getenv("LASTWAR_TEST_NO_RT") != "" {
			reply = noRtReply()
		}
		reply.ServerList = []gsl.LoginServerInfo{{ID: "783", IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: lookupZone, GameUid: lookupGameUid}}
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, reply))
		cfgPath := os.Getenv("LASTWAR_TEST_CFG")
		if os.Getenv("LASTWAR_TEST_SET_ADDR") == "1" {
			cfg := readSessionConfig(t, cfgPath)
			cfg.IP, cfg.Port = host, port
			if err := SaveSessionConfig(&cfg, cfgPath); err != nil {
				t.Fatal(err)
			}
		}
		os.Args = append([]string{"lastwar-client", "-config", cfgPath}, strings.Fields(os.Getenv("LASTWAR_TEST_ARGS"))...)
		Run()
		fmt.Fprintf(os.Stderr, "logins=%v\n", seen.get())
		return
	}

	stale := time.Now().Add(-31 * 24 * time.Hour).Unix()
	for _, c := range []struct {
		name       string
		cfg        SessionConfig
		env        []string
		wantCode   int
		wantLogins string
		refresh    bool
	}{
		{"refresh token only, no address",
			SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev_n3d", RefreshToken: "rt-old"},
			[]string{"LASTWAR_TEST_GOOD=" + refreshedAt}, 0, "[" + refreshedAt + "]", true},
		{"30-day-old accessTokenTime",
			SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev_n3d", AccessToken: "at-old", AccessTokenTime: stale, RefreshToken: "rt-old"},
			[]string{"LASTWAR_TEST_GOOD=" + refreshedAt, "LASTWAR_TEST_SET_ADDR=1"}, 0, "[" + refreshedAt + "]", true},
		// The live reply shape (no rt): the config's refresh token and its time come through the
		// run and are saved back unchanged.
		{"30-day-old accessTokenTime, reply without rt",
			SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev_n3d", AccessToken: "at-old", AccessTokenTime: stale, RefreshToken: "rt-old", RefreshTokenTime: 42},
			[]string{"LASTWAR_TEST_GOOD=" + refreshedAt, "LASTWAR_TEST_SET_ADDR=1", "LASTWAR_TEST_NO_RT=1"}, 0, "[" + refreshedAt + "]", true},
		{"explicit -cs-at ignores the config's accessTokenTime",
			SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev_n3d", AccessToken: "at-old", AccessTokenTime: stale, RefreshToken: "rt-old"},
			[]string{"LASTWAR_TEST_GOOD=at-explicit", "LASTWAR_TEST_SET_ADDR=1", "LASTWAR_TEST_ARGS=-cs-at at-explicit"}, 0, "[at-explicit]", false},
		{"-cs-deviceid ignores the config's refresh token",
			SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev_n3d", RefreshToken: "rt-old", AccessToken: "at-old"},
			[]string{"LASTWAR_TEST_GOOD=" + refreshedAt, "LASTWAR_TEST_SET_ADDR=1", "LASTWAR_TEST_ARGS=-cs-deviceid other_n3d"}, 2, "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfgPath := writeSessionConfig(t, c.cfg)
			code, stderr := runRefreshHelper(t, "TestMainUsesSessionConfigRefreshToken", append([]string{"LASTWAR_TEST_CFG=" + cfgPath}, c.env...)...)
			if code != c.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr=%s", code, c.wantCode, stderr)
			}
			if c.wantLogins != "" && !strings.Contains(stderr, "logins="+c.wantLogins) {
				t.Errorf("want logins=%s; stderr=%s", c.wantLogins, stderr)
			}
			if got := strings.Contains(stderr, `"opt":"refresh"`); got != c.refresh {
				t.Errorf("refreshed = %v, want %v; stderr=%s", got, c.refresh, stderr)
			}
			switch {
			case c.refresh && c.cfg.RefreshTokenTime != 0:
				cfg := readSessionConfig(t, cfgPath)
				if cfg.AccessToken != refreshedAt || cfg.RefreshToken != "rt-old" || cfg.RefreshTokenTime != 42 {
					t.Errorf("at=%q rt=%q rt.time=%d, want %s and the kept rt-old/42", cfg.AccessToken, cfg.RefreshToken, cfg.RefreshTokenTime, refreshedAt)
				}
			case c.refresh:
				wantRefreshedTokens(t, cfgPath)
			}
		})
	}
}

// TestSaveRefreshedTokensKeepsOtherFields: only the four token fields change on disk.
func TestSaveRefreshedTokensKeepsOtherFields(t *testing.T) {
	in := SessionConfig{IP: "h", Port: 1, Zone: "APS1", GameUid: "uid-1", DeviceID: "dev", ShumeiBoxId: "box",
		AccessToken: "at-old", RefreshToken: "rt-old", IOSMode: true, AppVersion: "1.0.364", VersionCode: "1892",
		Features: map[string]bool{"radar": true}}
	path := writeSessionConfig(t, in)
	saveRefreshedTokens(path, "dev", auth.Tokens{AccessTok: refreshedAt, RefreshTok: refreshedRt, AccessTokTime: refreshedAtTime, RefreshTokTime: refreshedRtTime})

	got := wantRefreshedTokens(t, path)
	got.AccessToken, got.RefreshToken, got.AccessTokenTime, got.RefreshTokenTime = in.AccessToken, in.RefreshToken, 0, 0
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(in)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("other fields changed:\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestSaveRefreshedTokensRefusesAnotherDevicesConfig: tokens issued to one device aren't saved into
// a session config made for another (a -cs-rt run under -cs-deviceid), which would leave the
// config's device with tokens it can't use. A config without a deviceId takes its owner from
// shumeiInstall, and one with neither accepts them.
func TestSaveRefreshedTokensRefusesAnotherDevicesConfig(t *testing.T) {
	tokens := auth.Tokens{AccessTok: refreshedAt, RefreshTok: refreshedRt, AccessTokTime: refreshedAtTime, RefreshTokTime: refreshedRtTime}
	for _, c := range []struct {
		name      string
		cfg       SessionConfig
		runDevice string
		wantSaved bool
	}{
		{"same device", SessionConfig{DeviceID: "dev-a"}, "dev-a", true},
		{"another device", SessionConfig{DeviceID: "dev-a"}, "dev-b", false},
		{"owner from shumeiInstall", SessionConfig{ShumeiInstall: shumei.Install{DeviceID: "dev-a"}}, "dev-b", false},
		{"no owner", SessionConfig{}, "dev-b", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.cfg.AccessToken, c.cfg.RefreshToken = "at-old", "rt-old"
			path := writeSessionConfig(t, c.cfg)
			saveRefreshedTokens(path, c.runDevice, tokens)
			got := readSessionConfig(t, path)
			if saved := got.AccessToken == refreshedAt; saved != c.wantSaved {
				t.Errorf("saved = %v, want %v (at=%q rt=%q)", saved, c.wantSaved, got.AccessToken, got.RefreshToken)
			}
			if !c.wantSaved && (got.AccessToken != "at-old" || got.RefreshToken != "rt-old") {
				t.Errorf("a refused save changed the tokens: at=%q rt=%q", got.AccessToken, got.RefreshToken)
			}
		})
	}
}

// TestRunCrossServerTestRefreshFailureAfterE011Exits1: a refresh after E011 that fails for any
// reason but 211/212 (here GSL code 500) says nothing about the session, so the run exits 1, not
// the exit-2 "session is stale" code, without the "bind the device again" message, and saves
// nothing.
func TestRunCrossServerTestRefreshFailureAfterE011Exits1(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: "500"})
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-old", RefreshToken: "rt-old"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshFailureAfterE011Exits1", "LASTWAR_TEST_CFG="+cfgPath)
	if code != 1 {
		t.Errorf("exit code = %d, want 1; stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "the next run will try again") || strings.Contains(stderr, "bind the device again") {
		t.Errorf("want the retry message and not the re-bind one; stderr=%s", stderr)
	}
	if cfg := readSessionConfig(t, cfgPath); cfg.AccessToken != "at-old" || cfg.RefreshToken != "rt-old" {
		t.Errorf("a failed refresh changed the saved tokens: at=%q rt=%q", cfg.AccessToken, cfg.RefreshToken)
	}
}

// TestRunCrossServerTestCsRtSavesRefreshState: a -cs-rt refresh records the rt that worked (or the
// one the reply rotated it to) and at.time, so the session config can refresh on its own later.
func TestRunCrossServerTestCsRtSavesRefreshState(t *testing.T) {
	for _, c := range []struct {
		name      string
		reply     gsl.LoginServerListRespon
		wantRt    string
		wantRtTim int64
	}{
		{"rotated", refreshReply(), refreshedRt, refreshedRtTime},
		{"not rotated", func() gsl.LoginServerListRespon { r := refreshReply(); r.Rt = nil; return r }(), "rt-flag", 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, c.reply)
			var seen loginAts
			dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": tokenCheckingServer(refreshedAt, &seen)})
			cfgPath := writeSessionConfig(t, SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-old"})

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					zone: lookupZone, gameUid: lookupGameUid, at: "at-old", rt: "rt-flag",
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			cfg := readSessionConfig(t, cfgPath)
			if cfg.AccessToken != refreshedAt || cfg.AccessTokenTime != refreshedAtTime {
				t.Errorf("at/at.time = %q/%d, want %q/%d", cfg.AccessToken, cfg.AccessTokenTime, refreshedAt, refreshedAtTime)
			}
			if cfg.RefreshToken != c.wantRt || cfg.RefreshTokenTime != c.wantRtTim {
				t.Errorf("rt/rt.time = %q/%d, want %q/%d", cfg.RefreshToken, cfg.RefreshTokenTime, c.wantRt, c.wantRtTim)
			}
		})
	}
}

// TestRunCrossServerTestWarnsWhenRefreshedTokenStillStale: a refresh that hands back an at.time 30
// days old (GSL returned the current at with its original time, live, eight days in) means every
// run will refresh again; the run logs that once at Warn and still logs in.
func TestRunCrossServerTestWarnsWhenRefreshedTokenStillStale(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)
	var seen loginAts
	dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer(refreshedAt, &seen)})

	synctest.Test(t, func(t *testing.T) {
		old := time.Now().Add(-31 * 24 * time.Hour).Unix()
		reply := refreshReply()
		reply.At.Time = gsl.FlexString(strconv.FormatInt(old, 10))
		client := testutil.UseInMemoryGSL(t, reply)
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", accessTokTime: old, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if got := seen.get(); fmt.Sprint(got) != fmt.Sprint([]string{refreshedAt}) {
		t.Errorf("Login access tokens = %v, want [%s]", got, refreshedAt)
	}
	if !strings.Contains(buf.String(), "still 30 days old") {
		t.Errorf("want the still-stale warning:\n%s", buf.String())
	}
}

// noRtReply is the refresh reply shape seen live: a fresh at and no rt.
func noRtReply() gsl.LoginServerListRespon {
	r := refreshReply()
	r.Rt = nil
	return r
}

// TestRunCrossServerTestRefreshWithoutRtKeepsTheRefreshToken: live, opt=refresh returned an at and
// no rt. The config's refresh token and its time must survive that, after an E011 and before a
// Login alike, or the feature works exactly once.
func TestRunCrossServerTestRefreshWithoutRtKeepsTheRefreshToken(t *testing.T) {
	for _, c := range []struct {
		name       string
		at         string
		staleAt    bool
		wantLogins []string
	}{
		{"after E011", "at-old", false, []string{"at-old", refreshedAt}},
		{"before the Login", "at-old", true, []string{refreshedAt}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, noRtReply())
			var seen loginAts
			dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer(refreshedAt, &seen)})
			cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: c.at, RefreshToken: "rt-old", RefreshTokenTime: 42})

			synctest.Test(t, func(t *testing.T) {
				var atTime int64
				if c.staleAt {
					atTime = time.Now().Add(-31 * 24 * time.Hour).Unix()
				}
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
					at: c.at, refreshTok: "rt-old", accessTokTime: atTime, refreshTokTime: 42,
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if got := seen.get(); fmt.Sprint(got) != fmt.Sprint(c.wantLogins) {
				t.Errorf("Login access tokens = %v, want %v", got, c.wantLogins)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.AccessToken != refreshedAt || cfg.AccessTokenTime != refreshedAtTime {
				t.Errorf("at/at.time = %q/%d, want %q/%d", cfg.AccessToken, cfg.AccessTokenTime, refreshedAt, refreshedAtTime)
			}
			if cfg.RefreshToken != "rt-old" || cfg.RefreshTokenTime != 42 {
				t.Errorf("rt/rt.time = %q/%d, want the kept rt-old/42", cfg.RefreshToken, cfg.RefreshTokenTime)
			}
		})
	}
}

// TestRunCrossServerTestRefreshesOncePerRun: a 30-day-old token refreshed before the Login and then
// rejected anyway is not refreshed a second time; the run exits 2 after one refresh.
func TestRunCrossServerTestRefreshesOncePerRun(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, refreshReply())
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: "at-old", refreshTok: "rt-old", accessTokTime: time.Now().Add(-31 * 24 * time.Hour).Unix(),
			configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: lookupPort, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-old", RefreshToken: "rt-old"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshesOncePerRun", "LASTWAR_TEST_CFG="+cfgPath)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	if n := strings.Count(stderr, "GSL getserverlist: refreshing the access token"); n != 1 {
		t.Errorf("refreshes = %d, want 1; stderr=%s", n, stderr)
	}
	if !strings.Contains(stderr, "rejected even after a GSL refresh") {
		t.Errorf("want the after-refresh message; stderr=%s", stderr)
	}
}

// TestRunCrossServerTestRefreshBeforeLoginFails: a refresh before the Login that GSL rejects with
// 211 and no access token to fall back on exits 2 with the re-auth message; one that fails for
// another reason while an old access token exists logs in with the old token.
func TestRunCrossServerTestRefreshBeforeLoginFails(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		code := os.Getenv("LASTWAR_TEST_GSL_CODE")
		client := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{Code: gsl.FlexString(code)})
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer("at-old", &seen)})
		at, atTime := os.Getenv("LASTWAR_TEST_AT"), time.Now().Add(-31*24*time.Hour).Unix()
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid,
			at: at, refreshTok: "rt-old", accessTokTime: atTime, httpClient: client, dialGame: dial,
		})
		fmt.Fprintf(os.Stderr, "logins=%v\n", seen.get())
		return
	}
	t.Run("211, no access token", func(t *testing.T) {
		code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshBeforeLoginFails", "LASTWAR_TEST_GSL_CODE=211", "LASTWAR_TEST_AT=")
		if code != 2 || !strings.Contains(stderr, "GSL rejected this device's credentials") {
			t.Errorf("exit code = %d, want 2 with the re-auth message; stderr=%s", code, stderr)
		}
	})
	t.Run("211, old access token", func(t *testing.T) {
		code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshBeforeLoginFails", "LASTWAR_TEST_GSL_CODE=211", "LASTWAR_TEST_AT=at-old")
		if code != 0 || !strings.Contains(stderr, "logins=[at-old]") || !strings.Contains(stderr, "refresh token is no longer accepted") {
			t.Errorf("exit code = %d, want 0, a Login with the old token, and the bind-again error; stderr=%s", code, stderr)
		}
	})
	t.Run("transient failure, old access token", func(t *testing.T) {
		code, stderr := runRefreshHelper(t, "TestRunCrossServerTestRefreshBeforeLoginFails", "LASTWAR_TEST_GSL_CODE=500", "LASTWAR_TEST_AT=at-old")
		if code != 0 || !strings.Contains(stderr, "logins=[at-old]") || !strings.Contains(stderr, "trying the old access token") {
			t.Errorf("exit code = %d, want 0 and a Login with the old token; stderr=%s", code, stderr)
		}
	})
}

// TestRunCrossServerTestCsRtSavesBeforeAFailedLogin: a -cs-rt refresh saves the rotated rt before
// the Login, so a Login that fails afterwards doesn't lose it.
func TestRunCrossServerTestCsRtSavesBeforeAFailedLogin(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, refreshReply())
		var seen loginAts
		dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": tokenCheckingServer("never", &seen)})
		runCrossServerTest(crossServerTestOpts{
			zone: lookupZone, gameUid: lookupGameUid, at: "at-old", rt: "rt-flag",
			configSavePath: os.Getenv("LASTWAR_TEST_CFG"), httpClient: client, dialGame: dial,
		})
		return
	}
	cfgPath := writeSessionConfig(t, SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-old"})
	code, stderr := runRefreshHelper(t, "TestRunCrossServerTestCsRtSavesBeforeAFailedLogin", "LASTWAR_TEST_CFG="+cfgPath)
	if code != 2 {
		t.Errorf("exit code = %d, want 2; stderr=%s", code, stderr)
	}
	wantRefreshedTokens(t, cfgPath)
}

// TestRunCrossServerTestRefreshRequest decrypts the refresh this run sends: opt=refresh with the
// config's refresh token, the run's device, zone and gameUid, and the session's platform.
func TestRunCrossServerTestRefreshRequest(t *testing.T) {
	for _, c := range []struct {
		iosMode      bool
		wantPlatform string
	}{{false, gsl.Platform}, {true, "iOS"}} {
		t.Run(c.wantPlatform, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client, forms := testutil.UseDecryptingGSL(t, refreshReply())
			var seen loginAts
			dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:10783": tokenCheckingServer(refreshedAt, &seen)})

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, deviceID: "dev-cfg",
					refreshTok: "rt-old", iosMode: c.iosMode, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			got := forms()
			if len(got) != 1 {
				t.Fatalf("getserverlist requests = %d, want 1", len(got))
			}
			want := map[string]string{"opt": "refresh", "rt": "rt-old", "uuid": "dev-cfg", "zone": lookupZone, "gameuid": lookupGameUid, "platform": c.wantPlatform}
			for k, v := range want {
				if g := got[0].Get(k); g != v {
					t.Errorf("%s = %q, want %q", k, g, v)
				}
			}
		})
	}
}

// TestRunCrossServerTestCsRtRefreshesOnce: a -cs-rt refresh whose reply still carries a 30-day-old
// at.time (GSL returned the original time, live) is not followed by a second refresh before the
// Login.
func TestRunCrossServerTestCsRtRefreshesOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var seen loginAts
	dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": tokenCheckingServer(refreshedAt, &seen)})
	var calls func() int

	synctest.Test(t, func(t *testing.T) {
		reply := refreshReply()
		reply.At.Time = gsl.FlexString(strconv.FormatInt(time.Now().Add(-31*24*time.Hour).Unix(), 10))
		client := testutil.UseInMemoryGSL(t, reply)
		calls = countGetServerList(client)
		runCrossServerTest(crossServerTestOpts{
			zone: lookupZone, gameUid: lookupGameUid, at: "at-old", rt: "rt-flag", httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if n := calls(); n != 1 {
		t.Errorf("getserverlist requests = %d, want 1 (the -cs-rt refresh only)", n)
	}
	if got := seen.get(); fmt.Sprint(got) != fmt.Sprint([]string{refreshedAt}) {
		t.Errorf("Login access tokens = %v, want [%s]", got, refreshedAt)
	}
}
