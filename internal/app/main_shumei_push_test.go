package app

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// pushShumeiServer accepts any Login, records its ta, sends init and then, when id is not empty,
// the push.shumei.id the server sends the client (PushShumeiIdMessage.lua:10-18).
func pushShumeiServer(seen *[]string, mu *sync.Mutex, id string) func(*session.GameConn) {
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
		if err := server.SendExtension("init", sfs.NewSFSObject()); err != nil {
			return
		}
		if id != "" {
			p := sfs.NewSFSObject()
			p.PutUtfString("shumeiId", id)
			_ = server.SendExtension("push.shumei.id", p)
		}
	}
}

// TestRunCrossServerTestSavesThePushedShumeiID (FU-C needs 1-2): a push.shumei.id the server sends
// after init is kept in the session config's shumeiId, as the client caches it in PlayerPrefs
// (PushShumeiIdMessage.lua:14-18), and the next iOS Login sends it as ta lw_shumei_id
// (A-CS:59119, 59123). A config made for another device keeps its own.
func TestRunCrossServerTestSavesThePushedShumeiID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	run := func(t *testing.T, cfg SessionConfig, path, runDevice, push string) []string {
		t.Helper()
		client := testutil.UseInMemoryGSL(t, lookupReply())
		var mu sync.Mutex
		var tas []string
		dial, _ := recordingDial(map[string]func(*session.GameConn){"gw-a.example:10783": pushShumeiServer(&tas, &mu, push)})
		synctest.Test(t, func(t *testing.T) {
			runCrossServerTest(crossServerTestOpts{
				ip: cfg.IP, port: cfg.Port, zone: cfg.Zone, gameUid: cfg.GameUid, deviceID: runDevice, at: cfg.AccessToken, iosMode: true,
				appVersion: cfg.AppVersion, versionCode: cfg.VersionCode, shumeiID: configShumeiID(&cfg, runDevice),
				configSavePath: path, httpClient: client, dialGame: dial,
			})
			synctest.Wait()
		})
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), tas...)
	}
	const pushed = "made-up-pushed-2"
	has := func(ta string, subs ...string) bool {
		for _, sub := range subs {
			if !strings.Contains(ta, sub) {
				return false
			}
		}
		return true
	}
	cfg := SessionConfig{Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev-1_n3d", AccessToken: "at-1", IOSMode: true,
		AppVersion: "1.0.370", VersionCode: "918", ShumeiID: "pushed-1"}
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveSessionConfig(&cfg, path); err != nil {
		t.Fatal(err)
	}

	first := run(t, cfg, path, cfg.DeviceID, pushed)
	if len(first) != 1 || !has(first[0], `"lw_shumei_id":"pushed-1"`, `"lw_device_id":"dev-1_n3d"`) {
		t.Fatalf("first Login ta = %q, want the configured lw_shumei_id pushed-1", first)
	}
	saved := readSessionConfig(t, path)
	if saved.ShumeiID != pushed {
		t.Fatalf("saved shumeiId = %q, want the pushed %q", saved.ShumeiID, pushed)
	}

	second := run(t, saved, path, saved.DeviceID, "")
	if len(second) != 1 || !has(second[0], `"lw_shumei_id":"`+pushed+`"`) {
		t.Errorf("next Login ta = %q, want lw_shumei_id %q", second, pushed)
	}
	if got := readSessionConfig(t, path).ShumeiID; got != pushed {
		t.Errorf("a run without a push changed the saved shumeiId to %q", got)
	}

	// A -cs-deviceid run under another device, at the saved address (so no save-back rewrites
	// the config): the push belongs to that device, not to the config's, so the config keeps its
	// shumeiId.
	other := run(t, saved, path, "other_n3d", "made-up-pushed-3")
	if len(other) != 1 || !has(other[0], `"lw_shumei_id":""`, `"lw_device_id":"other_n3d"`) {
		t.Errorf("another device's Login ta = %q, want no lw_shumei_id", other)
	}
	if got := readSessionConfig(t, path).ShumeiID; got != pushed {
		t.Errorf("another device's push was saved into the config: shumeiId %q, want %q", got, pushed)
	}
}
