package app

import (
	"bytes"
	"encoding/json"
	"io"
	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
	"lastwar-client/internal/testutil"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// clientConfig builds a check-version client_config string with the given switches on.
func clientConfig(on ...int) string {
	s := make([]string, 52)
	for i := range s {
		s[i] = "0"
	}
	for _, i := range on {
		s[i] = "1"
	}
	return strings.Join(s, "|")
}

// withCheckVersionFields rewrites every check-version reply client's transport hands back, setting
// fields on it (client_config, updateType, ...), so a test can drive the switches the 1.0.370 client
// reads.
func withCheckVersionFields(t *testing.T, client *http.Client, fields map[string]any) {
	t.Helper()
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if err != nil || !strings.HasSuffix(r.URL.Path, "getlsu3dversion.php") {
			return resp, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var m map[string]any
		if err := json.Unmarshal(body, &m); err != nil {
			t.Errorf("check-version reply is not JSON: %v", err)
			m = map[string]any{}
		}
		for k, v := range fields {
			m[k] = v
		}
		out, _ := json.Marshal(m)
		resp.Body = io.NopCloser(bytes.NewReader(out))
		resp.ContentLength = int64(len(out))
		return resp, nil
	})
}

// loginParamsServer hands the Login's p object to got, accepts the Login and sends init.
func loginParamsServer(got chan<- *sfs.SFSObject) func(*session.GameConn) {
	return func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if pv, ok := env.Content.Get("p"); ok {
			if p, ok := pv.Val.(*sfs.SFSObject); ok {
				got <- p
			}
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
}

// runLogin runs one cross-server reconnect against a configured address with an in-memory GSL
// (check-version fields set) and returns the Login's p object.
func runLogin(t *testing.T, fields map[string]any, o crossServerTestOpts) *sfs.SFSObject {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, configuredServerReply())
	withCheckVersionFields(t, client, fields)
	got := make(chan *sfs.SFSObject, 4)
	o.httpClient = client
	o.dialGame = session.NewInMemoryGameDial(loginParamsServer(got))
	if o.ip == "" {
		o.ip, o.port, o.zone, o.gameUid = "10.0.0.1", lookupPort, lookupZone, lookupGameUid
	}
	if o.at == "" {
		o.at = "at-1"
	}
	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(o)
		synctest.Wait()
	})
	select {
	case p := <-got:
		return p
	default:
		t.Fatal("the fake game server saw no Login")
		return nil
	}
}

// TestLoginShumeiBoxID covers ShumeiSdkManager.GetLoginSmsdkId (Assembly-CSharp.decompiled.cs:59937,
// 60044-60058): a configured shumeiBoxId goes into the Login only while ClientSwitch 25 is on; with
// no check-version reply the switch is unknown and the value is replayed as before (1.0.370 audit B41).
func TestLoginShumeiBoxID(t *testing.T) {
	on := &gsl.CheckVersionResponse{ClientConfig: gsl.FlexString(clientConfig(25))}
	off := &gsl.CheckVersionResponse{ClientConfig: gsl.FlexString(clientConfig())}
	cases := []struct {
		name       string
		cv         *gsl.CheckVersionResponse
		configured string
		want       string
	}{
		{"switch 25 on", on, "sm-1", "sm-1"},
		{"switch 25 off", off, "sm-1", ""},
		{"no check-version reply", nil, "sm-1", "sm-1"},
		{"nothing configured", on, "", ""},
	}
	for _, c := range cases {
		if got := loginShumeiBoxID(c.cv, c.configured); got != c.want {
			t.Errorf("%s: loginShumeiBoxID = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestRunCrossServerTestShumeiBoxIDFollowsSwitch25: the Login a reconnect sends carries the
// configured shumeiBoxId only with ClientSwitch 25 on, iOS included.
func TestRunCrossServerTestShumeiBoxIDFollowsSwitch25(t *testing.T) {
	for _, c := range []struct {
		name   string
		config string
		want   string
	}{
		{"switch 25 on", clientConfig(25), "sm-captured"},
		{"switch 25 off", clientConfig(), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := runLogin(t, map[string]any{"client_config": c.config}, crossServerTestOpts{shumeiBoxId: "sm-captured", iosMode: true})
			if got := p.GetString("shumeiBoxId"); got != c.want {
				t.Errorf("Login shumeiBoxId = %q, want %q", got, c.want)
			}
		})
	}
}

// TestRunCrossServerTestIsUseLz4FollowsSwitches4And5: the Login carries isUseLz4 only while
// ClientSwitch 4 and 5 are both off (Assembly-CSharp.decompiled.cs:9744-9745, 83176-83179; 1.0.370
// audit B43).
func TestRunCrossServerTestIsUseLz4FollowsSwitches4And5(t *testing.T) {
	for _, c := range []struct {
		name   string
		config string
		want   bool
	}{
		{"both off", clientConfig(25), true},
		{"switch 4 on", clientConfig(4), false},
		{"switch 5 on", clientConfig(5), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := runLogin(t, map[string]any{"client_config": c.config}, crossServerTestOpts{iosMode: true})
			if got := p.Has("isUseLz4"); got != c.want {
				t.Errorf("Login has isUseLz4 = %v, want %v", got, c.want)
			}
		})
	}
}

// TestRunCrossServerTestReplaysCapturedDevice: an iOS reconnect sends the session config's
// captured device fields in its Login, and their fromCountry/lang in the GSL lookup's form; the
// session config keeps them through a save-back (1.0.370 audit B15).
func TestRunCrossServerTestReplaysCapturedDevice(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	device := auth.CapturedDevice{
		PhoneModel: "iPhone15,2", OSVersion: "iOS 18.1", HandSetInfo: "mt-captured", Idfv: "IDFV-1",
		DistinctID: "distinct-1", FromCountry: "PL", Lang: "pl", PhoneScreen: "1179*2556", Afuid: "af-1", FirebaseID: "fb-1",
	}
	client, forms := testutil.UseDecryptingGSL(t, lookupReply())
	got := make(chan *sfs.SFSObject, 2)
	cfgPath := writeSessionConfig(t, SessionConfig{IP: "10.0.0.1", Port: 17783, Zone: lookupZone, GameUid: lookupGameUid, AccessToken: "at-1", IOSMode: true, Device: device})

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", iosMode: true, device: device,
			configSavePath: cfgPath, httpClient: client, dialGame: session.NewInMemoryGameDial(loginParamsServer(got)),
		})
		synctest.Wait()
	})

	p := <-got
	for key, want := range map[string]string{
		"phone_model": "iPhone15,2", "osVersion": "iOS 18.1", "mt": "mt-captured", "idfv": "IDFV-1", "distinct_id": "distinct-1",
		"fromCountry": "PL", "lang": "pl", "phone_screen": "1179*2556", "afuid": "af-1", "firebaseId": "fb-1",
	} {
		if v := p.GetString(key); v != want {
			t.Errorf("Login %s = %q, want the captured %q", key, v, want)
		}
	}
	if f := forms(); len(f) != 1 || f[0].Get("country") != "PL" || f[0].Get("lang") != "pl" {
		t.Errorf("lookup forms = %v, want one with country=PL lang=pl", f)
	}
	// The lookup listed another server, so the session config was rewritten; the device stays.
	if cfg := readSessionConfig(t, cfgPath); cfg.Device != device || cfg.IP != lookupGateways {
		t.Errorf("saved config ip=%q device kept=%v, want the looked-up address and the captured device", cfg.IP, cfg.Device == device)
	}
}

// TestRunCrossServerTestWarnsWithoutCapturedDevice: an iOS reconnect whose session config has no
// captured device keeps the placeholders and says to recapture.
func TestRunCrossServerTestWarnsWithoutCapturedDevice(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)
	p := runLogin(t, nil, crossServerTestOpts{iosMode: true})
	if got := p.GetString("phone_model"); got != "GoClient" {
		t.Errorf("phone_model = %q, want the placeholder", got)
	}
	if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "no captured device") {
		t.Errorf("want the recapture warning; log=%s", buf.String())
	}
}

// TestReconnectBuild covers B21's fallback: a session config without a build keeps claiming the
// build such configs always have; one with a build, and a bare-flag reconnect, are left alone.
func TestReconnectBuild(t *testing.T) {
	cases := []struct {
		name              string
		fromConfig, ios   bool
		app, code         string
		wantApp, wantCode string
	}{
		{"config without a build, iOS", true, true, "", "", "1.0.344", "786"},
		{"config without a build, Android", true, false, "", "", "1.0.364", "1892"},
		{"config with a build", true, true, "1.0.370", "918", "1.0.370", "918"},
		{"bare flags", false, true, "", "", "", ""},
	}
	for _, c := range cases {
		app, code := reconnectBuild(c.fromConfig, c.ios, c.app, c.code)
		if app != c.wantApp || code != c.wantCode {
			t.Errorf("%s: reconnectBuild = %s/%s, want %s/%s", c.name, app, code, c.wantApp, c.wantCode)
		}
	}
}

// TestRunCrossServerTestRtRefreshSendsZone: the -cs-rt refresh sends the session zone, as the client
// sends its cached SERVER_ZONE with every opt (Assembly-CSharp.decompiled.cs:77411, 77437; 1.0.370
// audit B81), and the device fields its Login sends: the phone's SIM operator on Android, the
// captured fromCountry/lang on iOS (:77433-77436; Android :77539-77542; audit B91, B15).
func TestRunCrossServerTestRtRefreshSendsZone(t *testing.T) {
	captured := auth.CapturedDevice{PhoneModel: "iPhone15,2", FromCountry: "PL", Lang: "pl"}
	for _, c := range []struct {
		name   string
		ios    bool
		device auth.CapturedDevice
		// An Android form's simOp is its phone's SIM operator, checked below; iOS sends "".
		wantCountry, wantLang, wantPlatform string
	}{
		{"Android", false, auth.CapturedDevice{}, "US", "en", "Android"},
		{"iOS with a captured device", true, captured, "PL", "pl", "iOS"},
		{"iOS without a capture", true, auth.CapturedDevice{}, "US", "en", "iOS"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client, forms := testutil.UseDecryptingGSL(t, refreshReply())
			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					zone: lookupZone, gameUid: lookupGameUid, rt: "rt-flag", iosMode: c.ios, device: c.device,
					httpClient: client, dialGame: session.NewInMemoryGameDial(session.FakeInitPushServer(nil)),
				})
				synctest.Wait()
			})
			f := forms()
			if len(f) == 0 || f[0].Get("opt") != "refresh" || f[0].Get("zone") != lookupZone {
				t.Fatalf("first GSL form = %v, want opt=refresh with zone=%s", f, lookupZone)
			}
			form := f[0]
			wantSimOp := ""
			if !c.ios {
				wantSimOp = shumei.HardwareFor(form.Get("uuid")).SimOperator
				if wantSimOp == "" {
					t.Fatalf("the catalog phone for deviceId %q has no SIM operator; the check below would mean nothing", form.Get("uuid"))
				}
			}
			if form.Get("simOp") != wantSimOp || form.Get("country") != c.wantCountry || form.Get("lang") != c.wantLang || form.Get("platform") != c.wantPlatform {
				t.Errorf("refresh form simOp=%q country=%q lang=%q platform=%q, want %q %q %q %q", form.Get("simOp"), form.Get("country"),
					form.Get("lang"), form.Get("platform"), wantSimOp, c.wantCountry, c.wantLang, c.wantPlatform)
			}
		})
	}
}

// forcedUpdateHelper runs runCrossServerTest in a subprocess against a GSL whose check-version, or
// the game server's Login reply, demands a forced update.
func forcedUpdateHelper(t *testing.T, where string) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, configuredServerReply())
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getserverlist.php") {
			_, _ = os.Stderr.WriteString("getserverlist called\n")
		}
		return base.RoundTrip(r)
	})
	if where == "check-version" {
		withCheckVersionFields(t, client, map[string]any{"updateType": "2", "downloadurl": "https://store.invalid/app"})
	}
	reply := func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_, _ = os.Stderr.WriteString("game server saw a Login\n")
		resp := sfs.NewSFSObject()
		resp.PutInt("updateType", 2)
		resp.PutUtfString("downloadurl", "https://store.invalid/app")
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
	runCrossServerTest(crossServerTestOpts{
		ip: "10.0.0.1", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-1", iosMode: true,
		httpClient: client, dialGame: func(addr string, timeout time.Duration) (*session.GameConn, error) {
			return session.NewInMemoryGameDial(reply)(addr, timeout)
		},
	})
}

// TestRunCrossServerTestForcedUpdateExits5: a forced update from check-version stops the run before
// any GSL call or Login, and one from the Login reply stops it before init; both exit 5 (1.0.370
// audit B16).
func TestRunCrossServerTestForcedUpdateExits5(t *testing.T) {
	if where := os.Getenv("LASTWAR_TEST_FORCED_UPDATE"); where != "" && os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		forcedUpdateHelper(t, where)
		return
	}
	for _, c := range []struct {
		where              string
		wantGSL, wantLogin bool
	}{
		{"check-version", false, false},
		{"login", true, true},
	} {
		t.Run(c.where, func(t *testing.T) {
			code, stderr := runRefreshHelper(t, "TestRunCrossServerTestForcedUpdateExits5", "LASTWAR_TEST_FORCED_UPDATE="+c.where)
			if code != exitCodeForcedUpdate {
				t.Errorf("exit code = %d, want %d; stderr=%s", code, exitCodeForcedUpdate, stderr)
			}
			if got := strings.Contains(stderr, "getserverlist called"); got != c.wantGSL {
				t.Errorf("GSL called = %v, want %v; stderr=%s", got, c.wantGSL, stderr)
			}
			if got := strings.Contains(stderr, "game server saw a Login"); got != c.wantLogin {
				t.Errorf("Login sent = %v, want %v; stderr=%s", got, c.wantLogin, stderr)
			}
			if strings.Contains(stderr, "fetching building list") {
				t.Errorf("the run went on to init after a forced update; stderr=%s", stderr)
			}
		})
	}
}

// TestExitLoginFailedForcedUpdateExits5: the guest/email Login path maps a forced update to 5 too.
func TestExitLoginFailedForcedUpdateExits5(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		exitLoginFailed(gsl.ErrForcedUpdate)
		return
	}
	code, stderr := runRefreshHelper(t, "TestExitLoginFailedForcedUpdateExits5")
	if code != exitCodeForcedUpdate {
		t.Errorf("exit code = %d, want %d; stderr=%s", code, exitCodeForcedUpdate, stderr)
	}
}

// TestAndroidInstallFor: the Login describes the identity's own Android install, whose afuid follows
// its install time, so ta #install_time is never taken from elsewhere (1.0.370 audit B31, B95). A
// ShuMei install of this deviceId that reports another time can't be changed any more, so the
// difference is logged; one that agrees, belongs to another deviceId, or has no time is quiet.
func TestAndroidInstallFor(t *testing.T) {
	own := time.Date(1999, 11, 1, 0, 0, 0, 0, time.UTC)
	fp := time.Date(1999, 3, 1, 0, 0, 0, 0, time.UTC)
	base := auth.AndroidInstall{InstallTime: own, Afuid: "1-2", DistinctID: "d"}
	cases := []struct {
		name    string
		install shumei.Install
		warn    bool
	}{
		{"install bound to this device, another time", shumei.Install{DeviceID: "dev_n3d", FirstInstallTime: fp}, true},
		{"install not bound yet, another time", shumei.Install{FirstInstallTime: fp}, true},
		{"install with the same time", shumei.Install{DeviceID: "dev_n3d", FirstInstallTime: own}, false},
		{"another device's install", shumei.Install{DeviceID: "other_n3d", FirstInstallTime: fp}, false},
		{"no install time", shumei.Install{DeviceID: "dev_n3d"}, false},
	}
	orig := slog.Default()
	defer slog.SetDefault(orig)
	for _, c := range cases {
		var buf bytes.Buffer
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		if got := androidInstallFor(base, c.install, "dev_n3d"); got != base {
			t.Errorf("%s: androidInstallFor = %+v, want the identity's install unchanged", c.name, got)
		}
		if warned := strings.Contains(buf.String(), "level=WARN"); warned != c.warn {
			t.Errorf("%s: warned = %v, want %v; log=%s", c.name, warned, c.warn, buf.String())
		}
	}
}

// loginInstallTimes returns an Android Login's ta #install_time and its afuid's first launch.
func loginInstallTimes(t *testing.T, p *sfs.SFSObject) (installed, firstLaunch time.Time) {
	t.Helper()
	var ta struct {
		InstallTime string `json:"#install_time"`
	}
	if err := json.Unmarshal([]byte(p.GetString("ta")), &ta); err != nil {
		t.Fatalf("Login ta is not JSON: %v", err)
	}
	installed, err := time.Parse("2006-01-02 15:04:05.000", ta.InstallTime)
	if err != nil {
		t.Fatalf("ta #install_time %q: %v", ta.InstallTime, err)
	}
	prefix, _, ok := strings.Cut(p.GetString("afuid"), "-")
	ms, err := strconv.ParseInt(prefix, 10, 64)
	if !ok || err != nil {
		t.Fatalf("afuid %q is not <first-launch ms>-<n>", p.GetString("afuid"))
	}
	return installed, time.UnixMilli(ms).UTC()
}

// TestRunCrossServerTestOneAndroidInstallTime: an Android Login's ta #install_time, its afuid's
// first launch and the ShuMei fingerprint's a125 fit describe one install, whichever came first
// (1.0.370 audit B31, B95). The afuid's first launch is seconds to minutes after the install time
// (AFb1lSDK.java:84-90), and the registered fit is that same install time.
//   - A session config that already keeps a ShuMei install: the new Android install takes its time.
//   - A session without one: the ShuMei install minted for it takes the Android install's time.
func TestRunCrossServerTestOneAndroidInstallTime(t *testing.T) {
	register := registerShumeiDevice
	t.Cleanup(func() { registerShumeiDevice = register })
	fit := time.Date(1999, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name    string
		install shumei.Install
		mint    bool
		want    time.Time // the install time every report must carry; zero = the Android install's own
	}{
		{"ShuMei install in the session, not minting", shumei.Install{FirstInstallTime: fit}, false, fit},
		{"ShuMei install in the session, minting", shumei.Install{FirstInstallTime: fit}, true, fit},
		{"no ShuMei install, minting", shumei.Install{}, true, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var registered []time.Time
			registerShumeiDevice = func(client shumei.Client, deviceID string, dev shumei.Device) (string, error) {
				registered = append(registered, dev.Install.FirstInstallTime)
				return register(client, deviceID, dev)
			}
			logins := make(chan *sfs.SFSObject, 8)
			path := writeTestSessionConfig(t, &SessionConfig{ShumeiInstall: c.install})
			saved := runSaveBackWith(t, crossServerTestOpts{shumeiGen: c.mint, shumeiInstall: c.install, configSavePath: path}, loginParamsServer(logins))
			close(logins)
			p := <-logins
			if p == nil {
				t.Fatal("the game server saw no Login")
			}
			installed, firstLaunch := loginInstallTimes(t, p)
			if !c.want.IsZero() && !installed.Equal(c.want) {
				t.Errorf("ta #install_time = %v, want the session's ShuMei install time %v", installed, c.want)
			}
			if d := firstLaunch.Sub(installed); d < 5*time.Second || d >= 90*time.Second {
				t.Errorf("afuid first launch is %v after ta #install_time (%v), want seconds to minutes after it", d, installed)
			}
			if !c.mint {
				if len(registered) != 0 {
					t.Errorf("registered %d ShuMei devices without -cs-shumei-gen", len(registered))
				}
				return
			}
			if len(registered) != 1 || !registered[0].Equal(installed) {
				t.Errorf("registered a125 fit %v, want ta #install_time %v", registered, installed)
			}
			if !saved.ShumeiInstall.FirstInstallTime.Equal(installed) {
				t.Errorf("saved shumeiInstall firstInstallTime = %v, want %v", saved.ShumeiInstall.FirstInstallTime, installed)
			}
		})
	}
}
