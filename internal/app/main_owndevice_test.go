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

// ownDeviceGSLReply is GSL's opt=login reply for the bound device: its own token pair and its
// role's server.
func ownDeviceGSLReply() gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code: "0", LastLoggedServer: "783",
		ServerList: []gsl.LoginServerInfo{{ID: "783", IP: lookupGateways, Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: "uid-783"}},
		At:         &gsl.LoginToken{Token: "at-own", Time: "1791900000"},
		Rt:         &gsl.LoginToken{Token: "rt-own", Time: "1791900000"},
	}
}

// emailBindingGameServer walks Login's email path and pushes an account with one role, on 783.
func emailBindingGameServer(server *session.GameConn) {
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
	role := sfs.NewSFSObject()
	role.PutInt("id", 783)
	role.PutUtfString("zone", "APS783")
	role.PutUtfString("gameUid", "uid-783")
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(role)
	push := sfs.NewSFSObject()
	push.PutUtfString("loginKey", "lk-account")
	push.PutUtfString("gameUid", "uid-783")
	push.PutSFSArray("accountArr", arr)
	_ = server.SendExtension("push.account.login.new", push)
}

// TestMainOwnDeviceSessionWithEmail runs Run() end to end: -email binds a fresh device (the role on
// -server 783), then the same run trades the loginKey for the device's own token pair and writes
// the session config. With a loginKey already on file, -email is ignored and no game server is
// dialed. -server without -email is ignored with a warning.
func TestMainOwnDeviceSessionWithEmail(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		home := os.Getenv("LASTWAR_TEST_HOME")
		t.Setenv("HOME", home)
		var replies []gsl.LoginServerListRespon
		if os.Getenv("LASTWAR_TEST_BOUND") == "" {
			addr := session.StartFakeGameServer(t, emailBindingGameServer)
			host, port := testutil.SplitHostPortInt(t, addr)
			// Login's opt=new: a guest token and the fake game server.
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
		os.Args = append([]string{"lastwar-client", "-code-pipe", pipe, "-own-device-session", filepath.Join(home, "own.json")},
			strings.Fields(os.Getenv("LASTWAR_TEST_ARGS"))...)
		Run()
		fmt.Fprintln(os.Stderr, "run returned")
		return
	}

	type tc struct {
		name, args string
		bound      bool
		wantLog    []string
		noLog      []string
	}
	for _, c := range []tc{
		{"fresh device", "-email player@example.com -server 783", false,
			[]string{"binding the role on the requested server", "device bound by email verification", "wrote own-device session config"},
			[]string{"ignoring -email"}},
		{"already bound", "-email player@example.com", true,
			[]string{"ignoring -email: this device is already bound", "wrote own-device session config"},
			[]string{"step 6: request email verify code"}},
		{"-server without -email", "-server 783", true,
			[]string{"ignoring -server because -email is not set", "wrote own-device session config"}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			env := []string{"LASTWAR_TEST_HOME=" + home, "LASTWAR_TEST_ARGS=" + c.args}
			if c.bound {
				for name, v := range map[string]string{".lastwar_goclient_device_id": "dev_n3d", ".lastwar_goclient_loginkey": "lk-account", ".lastwar_goclient_gameuid": "uid-783"} {
					if err := os.WriteFile(filepath.Join(home, name), []byte(v), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				env = append(env, "LASTWAR_TEST_BOUND=1")
			}
			code, stderr := runRefreshHelper(t, "TestMainOwnDeviceSessionWithEmail", env...)
			if code != 0 || !strings.Contains(stderr, "run returned") {
				t.Fatalf("exit code = %d, want a clean return; stderr=%s", code, stderr)
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
			cfg := readSessionConfig(t, filepath.Join(home, "own.json"))
			if cfg.AccessToken != "at-own" || cfg.RefreshToken != "rt-own" || cfg.GameUid != "uid-783" || cfg.Zone != lookupZone || cfg.IP != lookupGateways {
				t.Errorf("session config: at=%q rt=%q gameUid=%q zone=%q ip=%q", cfg.AccessToken, cfg.RefreshToken, cfg.GameUid, cfg.Zone, cfg.IP)
			}
			if cfg.DeviceID == "" || cfg.IOSMode {
				t.Errorf("session config: deviceId=%q iosMode=%v, want this device, Android", cfg.DeviceID, cfg.IOSMode)
			}
			for _, secret := range []string{"lk-account", "at-own", "rt-own", "123456"} {
				if strings.Contains(stderr, `"`+secret+`"`) || strings.Contains(stderr, "="+secret) {
					t.Errorf("log contains %q; stderr=%s", secret, stderr)
				}
			}
		})
	}
}
