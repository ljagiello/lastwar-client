package app

import (
	"fmt"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// ownDeviceGSLReply is GSL's opt=login reply for the bound device: its own token pair and the
// servers of the account's two roles.
func ownDeviceGSLReply() gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code: "0", LastLoggedServer: "1",
		ServerList: []gsl.LoginServerInfo{
			{ID: "1", IP: "gw-1.example", Port: testutil.FlexPort(10001), Zone: "APS1", GameUid: "uid-1"},
			{ID: "783", IP: lookupGateways, Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: "uid-783"},
		},
		At: &gsl.LoginToken{Token: "at-own", Time: "1791900000"},
		Rt: &gsl.LoginToken{Token: "rt-own", Time: "1791900000"},
	}
}

// emailBindingGameServer walks Login's email path and pushes an account with two roles: uid-1 on
// server 1 (the top-level pick) and uid-783 on 783, whose entry carries its own loginKey. Its
// init carries no buildings, so a plain -email run going on into the game sits out
// FetchBuildings' 12 s wait before exiting 0. mode "error" pushes an errorCode instead,
// as for a refused code; "kick" pushes push.user.off, as when the account logs in elsewhere.
func emailBindingGameServer(mode string) func(*session.GameConn) {
	return func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		ok := sfs.NewSFSObject()
		ok.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, ok); err != nil {
			return
		}
		if err := server.SendExtension("init", sfs.NewSFSObject()); err != nil {
			return
		}
		for _, cmd := range []string{"account.login.send.verify.code", "account.login.new"} {
			if _, err := session.ReadNextExtension(server); err != nil {
				return
			}
			if err := server.SendExtension(cmd, ok); err != nil {
				return
			}
		}
		push := sfs.NewSFSObject()
		switch mode {
		case "error":
			push.PutInt("errorCode", 208193)
			_ = server.SendExtension("push.account.login.new", push)
			return
		case "kick":
			_ = server.SendExtension("push.user.off", sfs.NewSFSObject())
			return
		}
		arr := sfs.NewSFSArray()
		for _, r := range []struct {
			id                  int32
			zone, uid, loginKey string
		}{{1, "APS1", "uid-1", ""}, {783, "APS783", "uid-783", "lk-role-783"}} {
			role := sfs.NewSFSObject()
			role.PutInt("id", r.id)
			role.PutUtfString("zone", r.zone)
			role.PutUtfString("gameUid", r.uid)
			if r.loginKey != "" {
				role.PutUtfString("loginKey", r.loginKey)
			}
			arr.AddSFSObject(role)
		}
		push.PutUtfString("loginKey", "lk-account")
		push.PutUtfString("gameUid", "uid-1")
		push.PutSFSArray("accountArr", arr)
		_ = server.SendExtension("push.account.login.new", push)
	}
}

// TestMainOwnDeviceSessionWithEmail runs Run() end to end in a subprocess, against a fake GSL and a
// fake game server: -email binding a device (with or without -server), -own-device-session trading
// its loginKey and writing the session config, and the cases around them.
func TestMainOwnDeviceSessionWithEmail(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		home := os.Getenv("LASTWAR_TEST_HOME")
		t.Setenv("HOME", home)
		var replies []gsl.LoginServerListRespon
		if os.Getenv("LASTWAR_TEST_BOUND") == "" {
			addr := session.StartFakeGameServer(t, emailBindingGameServer(os.Getenv("LASTWAR_TEST_PUSH_MODE")))
			host, port := testutil.SplitHostPortInt(t, addr)
			// Login's opt=new/opt=fix: a guest token and the fake game server.
			replies = append(replies, gsl.LoginServerListRespon{Code: "0",
				ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS1"}},
				At:         &gsl.LoginToken{Token: "at-guest"}})
		}
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, append(replies, ownDeviceGSLReply())...))
		pipe := filepath.Join(home, "code.pipe")
		if err := syscall.Mkfifo(pipe, 0o600); err != nil {
			t.Fatal(err)
		}
		go func() {
			f, err := os.OpenFile(pipe, os.O_WRONLY, 0)
			if err != nil {
				return
			}
			defer func() { _ = f.Close() }()
			_, _ = f.WriteString("123456\n")
		}()
		args := []string{"lastwar-client", "-no-config", "-code-pipe", pipe}
		if os.Getenv("LASTWAR_TEST_PLAIN") == "" {
			args = append(args, "-own-device-session", filepath.Join(home, "own.json"))
		}
		os.Args = append(args, strings.Fields(os.Getenv("LASTWAR_TEST_ARGS"))...)
		Run()
		fmt.Fprintln(os.Stderr, "run returned")
		return
	}

	bound := map[string]string{".lastwar_goclient_device_id": "dev_n3d", ".lastwar_goclient_loginkey": "lk-account", ".lastwar_goclient_gameuid": "uid-783"}
	type tc struct {
		name, args   string
		seed         map[string]string
		plain        bool   // no -own-device-session: the plain -email login
		pushMode     string // emailBindingGameServer's mode
		wantCode     int
		wantLog      []string
		noLog        []string
		wantGameUid  string // the persisted gameUid, and the config's when one is written
		wantLoginKey string // the persisted loginKey ("" = none)
		wantZone     string // the config's zone; "" = no config written
		wantDeviceID string // the config's deviceId when seeded
		wantIOS      bool   // the config's iosMode, and the iOS build
	}
	for _, c := range []tc{
		{name: "fresh device, -server 783", args: "-email player@example.com -server 783",
			wantLog: []string{"account role", "binding the role on the requested server", "device bound by email verification", "wrote own-device session config"},
			noLog:   []string{"ignoring -email", "this device already logged in as a guest"},
			// The 783 entry carries its own loginKey, which wins over the account's.
			wantGameUid: "uid-783", wantLoginKey: "lk-role-783", wantZone: lookupZone},
		{name: "fresh device, -cs-ios", args: "-email player@example.com -cs-ios",
			wantLog:     []string{`"iosMode":true`, `"platform":"iOS"`, "device bound by email verification", "wrote own-device session config"},
			noLog:       []string{"ignoring -cs-ios", "ignoring -cs-* flags"},
			wantGameUid: "uid-1", wantLoginKey: "lk-account", wantZone: "APS1", wantIOS: true},
		{name: "fresh device, no -server", args: "-email player@example.com",
			wantLog:     []string{"account role", "device bound by email verification", "wrote own-device session config"},
			noLog:       []string{"binding the role on the requested server"},
			wantGameUid: "uid-1", wantLoginKey: "lk-account", wantZone: "APS1"},
		{name: "already bound", args: "-email player@example.com -server 783", seed: bound,
			wantLog:     []string{"ignoring -email and -server: this device is already bound", `"serverGiven":true`, "wrote own-device session config"},
			noLog:       []string{"step 6: request email verify code", "check-version: fetch RSA pubkey and pick gate host"},
			wantGameUid: "uid-783", wantLoginKey: "lk-account", wantZone: lookupZone, wantDeviceID: "dev_n3d"},
		{name: "-server without -email", args: "-server 783", seed: bound,
			wantLog:     []string{"ignoring -server because -email is not set", "wrote own-device session config"},
			wantGameUid: "uid-783", wantLoginKey: "lk-account", wantZone: lookupZone, wantDeviceID: "dev_n3d"},
		{name: "reused guest (username)", args: "-email player@example.com -server 783",
			seed:        map[string]string{".lastwar_goclient_device_id": "dev_n3d", ".lastwar_goclient_username": "guest-1"},
			wantLog:     []string{"this device already logged in as a guest", "device bound by email verification"},
			wantGameUid: "uid-783", wantLoginKey: "lk-role-783", wantZone: lookupZone, wantDeviceID: "dev_n3d"},
		{name: "reused guest (gameUid)", args: "-email player@example.com -server 783",
			seed:        map[string]string{".lastwar_goclient_device_id": "dev_n3d", ".lastwar_goclient_gameuid": "uid-guest"},
			wantLog:     []string{"this device already logged in as a guest", "device bound by email verification"},
			wantGameUid: "uid-783", wantLoginKey: "lk-role-783", wantZone: lookupZone, wantDeviceID: "dev_n3d"},
		{name: "no role on -server", args: "-email player@example.com -server 999", wantCode: 1,
			wantLog: []string{"no role on server 999", "fresh LASTWAR_STATE_DIR"},
			noLog:   []string{"own-device bootstrap failed", "wrote own-device session config"}},
		{name: "code refused", args: "-email player@example.com -server 783", pushMode: "error", wantCode: 2,
			wantLog: []string{"LOGIN-WITH-CODE FAILED"},
			noLog:   []string{"own-device bootstrap failed", "wrote own-device session config"}},
		{name: "kicked while binding", args: "-email player@example.com -server 783", pushMode: "kick", wantCode: exitCodeSessionEnded,
			wantLog: []string{"the server ended this session"},
			noLog:   []string{"own-device bootstrap failed", "wrote own-device session config"}},
		{name: "device identity unreadable", args: "-email player@example.com",
			seed: map[string]string{".lastwar_goclient_device_id": "<dir>"}, wantCode: 1,
			wantLog: []string{"load device identity failed"},
			noLog:   []string{"step 6: request email verify code", "wrote own-device session config"}},
		{name: "plain -email -server", args: "-email player@example.com -server 783", plain: true,
			wantLog:     []string{"binding the role on the requested server"},
			wantGameUid: "uid-783", wantLoginKey: "lk-role-783"},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			env := []string{"LASTWAR_TEST_HOME=" + home, "LASTWAR_TEST_ARGS=" + c.args}
			for name, v := range c.seed {
				if v == "<dir>" {
					// A directory where the state file should be: the read fails.
					if err := os.Mkdir(filepath.Join(home, name), 0o700); err != nil {
						t.Fatal(err)
					}
					continue
				}
				if err := os.WriteFile(filepath.Join(home, name), []byte(v), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if c.seed[".lastwar_goclient_loginkey"] != "" {
				env = append(env, "LASTWAR_TEST_BOUND=1")
			}
			if c.plain {
				env = append(env, "LASTWAR_TEST_PLAIN=1")
			}
			if c.pushMode != "" {
				env = append(env, "LASTWAR_TEST_PUSH_MODE="+c.pushMode)
			}
			code, stderr := runRefreshHelper(t, "TestMainOwnDeviceSessionWithEmail", env...)
			if code != c.wantCode {
				t.Fatalf("exit code = %d, want %d; stderr=%s", code, c.wantCode, stderr)
			}
			if c.wantCode == 0 && !strings.Contains(stderr, "run returned") {
				t.Fatalf("Run() didn't return cleanly; stderr=%s", stderr)
			}
			for _, want := range c.wantLog {
				if !strings.Contains(stderr, want) {
					t.Errorf("log lacks %q; stderr=%s", want, stderr)
				}
			}
			for _, not := range c.noLog {
				if strings.Contains(stderr, not) {
					t.Errorf("log has %q; stderr=%s", not, stderr)
				}
			}
			state := func(name string) string {
				b, err := os.ReadFile(filepath.Join(home, name))
				if err != nil {
					return ""
				}
				return strings.TrimSpace(string(b))
			}
			if got := state(".lastwar_goclient_loginkey"); got != c.wantLoginKey {
				t.Errorf("persisted loginKey = %q, want %q", got, c.wantLoginKey)
			}
			if c.wantGameUid != "" {
				if got := state(".lastwar_goclient_gameuid"); got != c.wantGameUid {
					t.Errorf("persisted gameUid = %q, want %q", got, c.wantGameUid)
				}
			}
			cfgPath := filepath.Join(home, "own.json")
			if c.wantZone == "" {
				if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
					t.Errorf("a session config was written (stat err %v)", err)
				}
			} else {
				cfg := readSessionConfig(t, cfgPath)
				if cfg.AccessToken != "at-own" || cfg.RefreshToken != "rt-own" || cfg.GameUid != c.wantGameUid || cfg.Zone != c.wantZone {
					t.Errorf("session config: at=%q rt=%q gameUid=%q zone=%q, want at-own rt-own %s %s", cfg.AccessToken, cfg.RefreshToken, cfg.GameUid, cfg.Zone, c.wantGameUid, c.wantZone)
				}
				if cfg.DeviceID == "" || (c.wantDeviceID != "" && cfg.DeviceID != c.wantDeviceID) || cfg.IOSMode != c.wantIOS {
					t.Errorf("session config: deviceId=%q iosMode=%v, want this device (%q), iosMode=%v", cfg.DeviceID, cfg.IOSMode, c.wantDeviceID, c.wantIOS)
				}
				if c.wantIOS && (cfg.AppVersion != "1.0.344" || cfg.VersionCode != "786") {
					t.Errorf("session config build = %s/%s, want the iOS 1.0.344/786", cfg.AppVersion, cfg.VersionCode)
				}
			}
			for _, secret := range []string{"lk-account", "lk-role-783", "at-own", "rt-own"} {
				if strings.Contains(stderr, secret) {
					t.Errorf("log contains %q; stderr=%s", secret, stderr)
				}
			}
			// The code is six digits, which a timestamp's fraction can contain, so only a logged
			// value counts.
			if strings.Contains(stderr, `"123456"`) || strings.Contains(stderr, ":123456") {
				t.Errorf("log contains the verification code; stderr=%s", stderr)
			}
		})
	}
}

// TestMainCrossServerIgnoresEmailAndServer: an access token selects the cross-server reconnect,
// which doesn't run the email flow, so -email and -server are both named as ignored.
func TestMainCrossServerIgnoresEmailAndServer(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		gsl.CheckVersionHosts = []string{"http://gsl.invalid"} // never the real GSL
		os.Args = []string{"lastwar-client", "-no-config", "-cs-at", "at-x", "-email", "player@example.com", "-server", "783"}
		Run()
		return
	}
	_, stderr := runRefreshHelper(t, "TestMainCrossServerIgnoresEmailAndServer")
	for _, want := range []string{"ignoring -email because -cs-ip/-cs-rt/-cs-at is set", "ignoring -server because -cs-ip/-cs-rt/-cs-at is set"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("log lacks %q; stderr=%s", want, stderr)
		}
	}
}
