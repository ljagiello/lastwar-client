package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"lastwar-client/internal/testutil"
)

// TestConfigRefreshToken: the config's refresh token belongs to the config's device. Run used to
// drop it whenever -cs-deviceid was typed, even naming the config's own device, and the save-back
// then wrote refreshToken "" over it (MASTER B11).
func TestConfigRefreshToken(t *testing.T) {
	cfg := &SessionConfig{DeviceID: "dev-1", RefreshToken: "rt-1", RefreshTokenTime: 42}
	if tok, at := configRefreshToken(cfg, "dev-1"); tok != "rt-1" || at != 42 {
		t.Errorf("same device: got (%q, %d), want (rt-1, 42)", tok, at)
	}
	if tok, at := configRefreshToken(cfg, "dev-2"); tok != "" || at != 0 {
		t.Errorf("another device: got (%q, %d), want none", tok, at)
	}
	if tok, at := configRefreshToken(&SessionConfig{RefreshToken: "rt-1", RefreshTokenTime: 42}, "dev-2"); tok != "" || at != 0 {
		t.Errorf("a config without a deviceId, run under -cs-deviceid: got (%q, %d), want none", tok, at)
	}
	if tok, at := configRefreshToken(nil, "dev-1"); tok != "" || at != 0 {
		t.Errorf("no config: got (%q, %d), want none", tok, at)
	}
}

// TestRunCrossServerTestSameDeviceSaveBackKeepsConfig: a run whose -cs-deviceid names the config's
// own device and whose server is looked up saves the new address back with every other field of
// the config unchanged, the refresh token and its time among them. The opts are built the way Run
// builds them from the config after the deviceId merge.
func TestRunCrossServerTestSameDeviceSaveBackKeepsConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	dial, _ := recordingDial(nil)
	path := filepath.Join(t.TempDir(), "session.json")
	var want SessionConfig

	synctest.Test(t, func(t *testing.T) {
		atTime := time.Now().Add(-time.Hour).Unix()
		want = SessionConfig{
			Zone: lookupZone, GameUid: lookupGameUid, DeviceID: "dev-1", ShumeiBoxId: "Bbox-1",
			AccessToken: "at-1", AccessTokenTime: atTime, RefreshToken: "rt-1", RefreshTokenTime: atTime - 1,
			AppVersion: "1.0.364", VersionCode: "900",
			Features:   map[string]bool{"daily-quests": true, "radar-claims": false},
			DuelPolicy: map[string]string{"radar-claims": "always"},
		}
		if err := SaveSessionConfig(&want, path); err != nil {
			t.Fatalf("SaveSessionConfig: %v", err)
		}
		cfg, err := LoadSessionConfig(path)
		if err != nil {
			t.Fatalf("LoadSessionConfig: %v", err)
		}
		const csDeviceID = "dev-1" // -cs-deviceid, the config's own device
		rt, rtTime := configRefreshToken(cfg, csDeviceID)
		runCrossServerTest(crossServerTestOpts{
			zone: cfg.Zone, gameUid: cfg.GameUid, deviceID: csDeviceID, shumeiBoxId: cfg.ShumeiBoxId,
			at: cfg.AccessToken, accessTokTime: cfg.AccessTokenTime, refreshTok: rt, refreshTokTime: rtTime,
			appVersion: cfg.AppVersion, versionCode: cfg.VersionCode, features: cfg.Features, duelPolicy: cfg.DuelPolicy,
			configSavePath: path, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	got := readSessionConfig(t, path)
	if got.IP != lookupGateways || got.Port != lookupPort {
		t.Fatalf("persisted %s:%d, want the looked-up %s:%d (the save-back didn't run)", got.IP, got.Port, lookupGateways, lookupPort)
	}
	want.IP, want.Port = lookupGateways, lookupPort
	if diff := sessionConfigDiff(got, want); len(diff) > 0 {
		t.Errorf("the save-back changed %v; want only ip and port changed", diff)
	}
}

// sessionConfigDiff names the SessionConfig fields that differ (SessionConfig redacts itself in
// %v, so a failure prints names, not values).
func sessionConfigDiff(a, b SessionConfig) []string {
	var out []string
	va, vb := reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range va.NumField() {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			out = append(out, va.Type().Field(i).Name)
		}
	}
	return out
}

// TestLoadSessionConfigToleratesUnknownFields: a session config carrying keys SessionConfig doesn't
// model (a newer build's field, a note) still loads with every modeled field intact.
func TestLoadSessionConfigToleratesUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	raw := `{"zone": "APS1", "gameUid": "uid-1", "deviceId": "dev-1", "refreshToken": "rt-1", "refreshTokenTime": 42,
		"note": "captured 2026-10-04", "futureField": {"a": [1, 2]}, "features": {"quests": true}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSessionConfig(path)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	want := SessionConfig{Zone: "APS1", GameUid: "uid-1", DeviceID: "dev-1", RefreshToken: "rt-1", RefreshTokenTime: 42,
		Features: map[string]bool{"quests": true}}
	if diff := sessionConfigDiff(*got, want); len(diff) > 0 {
		t.Errorf("loaded config differs in %v", diff)
	}
}
