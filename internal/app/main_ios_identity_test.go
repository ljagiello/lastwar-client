package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// capturedTa is a made-up ta of the real iOS app (no real device's values), captured under
// 1.0.364/900 for device dev-1_n3d with the shumeiId the client had cached then.
const capturedTa = `{"#os":"iOS","#device_model":"iPhone99,9","#app_version":"1.0.364","#fps":120,"lw_res_version":"1.0.364.1","lw_buildcode":"900",` +
	`"lw_game_session_id":"99999999-8888-7777-6666-555555555555","lw_device_id":"dev-1_n3d","lw_main_level":25,"lw_shumei_id":"cached-1",` +
	`"lw_airKey":"lwDid_ZGV2LTFfbjNk","lw_version":"1.0.364","lw_zone":"APS783"}`

// loginTaServer accepts any Login, as FakeInitPushServer does, and records the ta it carried.
func loginTaServer(seen *[]string, mu *sync.Mutex) func(*session.GameConn) {
	return func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		ta := ""
		if pv, ok := env.Content.Get("p"); ok {
			if p, ok := pv.Val.(*sfs.SFSObject); ok {
				ta = p.GetString("ta")
			}
		}
		mu.Lock()
		*seen = append(*seen, ta)
		mu.Unlock()
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
}

// TestRunCrossServerTestReplaysAndKeepsTheCapturedIOSIdentity: an iOS session config recorded
// from the real app (a captured device with its ta, and the pushed shumeiId) logs in with the
// captured ta, its build keys and shumeiId taken from this run (1.0.370 audit B15), and the
// save-back after a looked-up address keeps the ta and the shumeiId. check-version gives no
// resVersion here, so the captured lw_res_version stays.
func TestRunCrossServerTestReplaysAndKeepsTheCapturedIOSIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	var mu sync.Mutex
	var tas []string
	dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": loginTaServer(&tas, &mu)})
	path := filepath.Join(t.TempDir(), "session.json")
	cfg := SessionConfig{
		Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev-1_n3d", AccessToken: "at-1", IOSMode: true,
		AppVersion: "1.0.370", VersionCode: "918",
		Device:   auth.CapturedDevice{PhoneModel: "iPhone99,9", OSVersion: "iOS 99.1", Ta: capturedTa},
		ShumeiID: "pushed-1",
		Features: map[string]bool{"daily-quests": true},
	}
	if err := SaveSessionConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			zone: cfg.Zone, gameUid: cfg.GameUid, deviceID: cfg.DeviceID, at: cfg.AccessToken, iosMode: true,
			appVersion: cfg.AppVersion, versionCode: cfg.VersionCode, device: cfg.Device, shumeiID: configShumeiID(&cfg, cfg.DeviceID),
			features: cfg.Features, configSavePath: path, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	const wantTa = `{"#os":"iOS","#device_model":"iPhone99,9","#app_version":"1.0.370","#fps":120,"lw_res_version":"1.0.364.1","lw_buildcode":"918",` +
		`"lw_game_session_id":"99999999-8888-7777-6666-555555555555","lw_device_id":"dev-1_n3d","lw_main_level":25,"lw_shumei_id":"pushed-1",` +
		`"lw_airKey":"lwDid_ZGV2LTFfbjNk","lw_version":"1.0.370","lw_zone":"APS783"}`
	mu.Lock()
	defer mu.Unlock()
	if len(tas) != 1 || tas[0] != wantTa {
		t.Errorf("Login ta = %q,\nwant [%s]", tas, wantTa)
	}
	got := readSessionConfig(t, path)
	if got.IP != lookupGateways || got.Port != lookupPort {
		t.Fatalf("persisted %s:%d, want the looked-up %s:%d (the save-back didn't run)", got.IP, got.Port, lookupGateways, lookupPort)
	}
	want := cfg
	want.IP, want.Port = lookupGateways, lookupPort
	if diff := sessionConfigDiff(got, want); len(diff) > 0 {
		t.Errorf("the save-back changed %v; want only ip and port changed", diff)
	}
}

// TestMainSendsTheConfigShumeiID: Run hands the session config's shumeiId to the iOS Login's ta
// lw_shumei_id for the config's own device, and none under a -cs-deviceid naming another.
func TestMainSendsTheConfigShumeiID(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		var mu sync.Mutex
		var tas []string
		record := loginTaServer(&tas, &mu)
		addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
			record(server)
			mu.Lock()
			defer mu.Unlock()
			var blob struct {
				ShumeiID string `json:"lw_shumei_id"`
				DeviceID string `json:"lw_device_id"`
			}
			_ = json.Unmarshal([]byte(tas[len(tas)-1]), &blob)
			fmt.Fprintf(os.Stderr, "ta lw_shumei_id=%q lw_device_id=%q\n", blob.ShumeiID, blob.DeviceID)
		})
		host, port := testutil.SplitHostPortInt(t, addr)
		reply := lookupReply()
		reply.ServerList[0].IP, reply.ServerList[0].Port = gsl.FlexString(host), testutil.FlexPort(port)
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, reply))
		os.Args = append([]string{"lastwar-client", "-config", os.Getenv("LASTWAR_TEST_CFG")}, strings.Fields(os.Getenv("LASTWAR_TEST_ARGS"))...)
		Run()
		return
	}
	cfg := SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev-1_n3d", AccessToken: "at-1", IOSMode: true,
		AppVersion: "1.0.370", VersionCode: "918", ShumeiID: "pushed-1"}
	for _, tc := range []struct{ name, args, want string }{
		{"the config's device", "", `ta lw_shumei_id="pushed-1" lw_device_id="dev-1_n3d"`},
		{"another device", "-cs-deviceid other_n3d", `ta lw_shumei_id="" lw_device_id="other_n3d"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSessionConfig(t, cfg)
			code, stderr := runRefreshHelper(t, "TestMainSendsTheConfigShumeiID", "LASTWAR_TEST_CFG="+path, "LASTWAR_TEST_ARGS="+tc.args)
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("exit %d; want %s in stderr:\n%s", code, tc.want, stderr)
			}
		})
	}
}

// TestRunCrossServerTestWarnsAboutAnIncompleteCapture: an iOS run warns to recapture when the
// session config has no captured device, and when its captured device has no ta (recorded before
// -session-out kept it), since the Login then pairs the real phone with this client's placeholder
// ta. A complete capture, and Android, warn about neither.
func TestRunCrossServerTestWarnsAboutAnIncompleteCapture(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name    string
		ios     bool
		device  auth.CapturedDevice
		wantLog string
	}{
		{"no captured device", true, auth.CapturedDevice{}, "has no captured device"},
		{"a captured device without ta", true, auth.CapturedDevice{PhoneModel: "iPhone99,9"}, "captured device has no ta"},
		{"a complete capture", true, auth.CapturedDevice{PhoneModel: "iPhone99,9", Ta: `{"#os":"iOS"}`}, ""},
		{"Android", false, auth.CapturedDevice{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testutil.UseInMemoryGSL(t, lookupReply())
			dial, _ := recordingDial(nil)
			var logs bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(orig)
			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					zone: lookupZone, gameUid: lookupGameUid, deviceID: "dev-1_n3d", at: "at-1", iosMode: tc.ios, device: tc.device,
					appVersion: "1.0.370", versionCode: "918", httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})
			for _, w := range []string{"has no captured device", "captured device has no ta"} {
				if got := strings.Contains(logs.String(), w); got != (w == tc.wantLog) {
					t.Errorf("warned %q = %v, want %v", w, got, w == tc.wantLog)
				}
			}
		})
	}
}

// TestConfigShumeiID: the pushed shumeiId belongs to the config's device; a run under another
// deviceId, or a config that names none, sends none.
func TestConfigShumeiID(t *testing.T) {
	cfg := &SessionConfig{DeviceID: "dev-1", ShumeiID: "pushed-1"}
	for _, tc := range []struct {
		name string
		cfg  *SessionConfig
		run  string
		want string
	}{
		{"the config's device", cfg, "dev-1", "pushed-1"},
		{"another device", cfg, "dev-2", ""},
		{"no deviceId in the config", &SessionConfig{ShumeiID: "pushed-1"}, "", ""},
		{"no config", nil, "dev-1", ""},
	} {
		if got := configShumeiID(tc.cfg, tc.run); got != tc.want {
			t.Errorf("%s: configShumeiID = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSaveShumeiID: a pushed shumeiId is saved into its own device's config with every other field
// as it was; another device's config, an empty id or an unchanged one writes nothing. A saved id
// and an unchanged one each log their own line, the two the config-copy test waits for (README,
// docs/client-1-0-370.mdx), so a push that repeats the saved id is not read as no push.
func TestSaveShumeiID(t *testing.T) {
	base := SessionConfig{IP: "h", Port: 1, Zone: "APS1", GameUid: "uid-1", DeviceID: "dev-1", AccessToken: "at-1",
		IOSMode: true, ShumeiID: "pushed-1", Device: auth.CapturedDevice{PhoneModel: "iPhone99,9", Ta: `{"#os":"iOS"}`},
		Features: map[string]bool{"radar": true}}
	const savedLine, alreadyLine = "saved the pushed shumeiId", "pushed shumeiId already saved"
	for _, tc := range []struct {
		name, deviceID, id, wantID string
		wantWrite, wantAlready     bool
	}{
		{"a new id for the config's device", "dev-1", "pushed-2", "pushed-2", true, false},
		{"the same id", "dev-1", "pushed-1", "pushed-1", false, true},
		{"an empty id", "dev-1", "", "pushed-1", false, false},
		{"another device", "dev-2", "pushed-2", "pushed-1", false, false},
		{"another device, the config's id", "dev-2", "pushed-1", "pushed-1", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeSessionConfig(t, base)
			before := readSessionConfig(t, path)
			var logs bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			saveShumeiID(path, tc.deviceID, tc.id)
			slog.SetDefault(orig)
			got := readSessionConfig(t, path)
			if got.ShumeiID != tc.wantID {
				t.Errorf("shumeiId = %q, want %q", got.ShumeiID, tc.wantID)
			}
			got.ShumeiID, before.ShumeiID = "", ""
			if diff := sessionConfigDiff(got, before); len(diff) > 0 {
				t.Errorf("saveShumeiID changed %v", diff)
			}
			if wrote := strings.Contains(logs.String(), savedLine); wrote != tc.wantWrite {
				t.Errorf("wrote = %v, want %v; log:\n%s", wrote, tc.wantWrite, logs.String())
			}
			if already := strings.Contains(logs.String(), alreadyLine); already != tc.wantAlready {
				t.Errorf("logged %q = %v, want %v; log:\n%s", alreadyLine, already, tc.wantAlready, logs.String())
			}
			if (tc.wantWrite || tc.wantAlready) && !strings.Contains(logs.String(), fmt.Sprintf("shumeiIdLen=%d", len(tc.id))) {
				t.Errorf("no shumeiIdLen=%d; log:\n%s", len(tc.id), logs.String())
			}
			for _, id := range []string{"pushed-1", "pushed-2"} {
				if strings.Contains(logs.String(), id) {
					t.Errorf("the log shows the shumeiId %q:\n%s", id, logs.String())
				}
			}
		})
	}
}
