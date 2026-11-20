package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// TestReplayTaKeepsTheCapturedBlob: a replayed ta keeps the captured key order and the raw bytes
// of every value it doesn't refresh (escapes, number forms, nested values), replaces the refreshed
// ones in place, and keeps the captured value where this Login has none.
func TestReplayTaKeepsTheCapturedBlob(t *testing.T) {
	const captured = `{"#os":"iOS","#carrier":"中国","#fps":120,"#ram":"1.9/5.6","pd_dl":2.50,"lw_ab":["x",{"y":1}],` +
		`"lw_version":"1.0.364","lw_device_id":"old-device","lw_shumei_id":"cached-id","lw_res_version":"1.0.364.1","lw_zone":"APS783"}`
	got, err := replayTa(captured, []taKey{
		{"lw_version", "1.0.370"},
		{"lw_device_id", "new-device_n3d"},
		{"lw_shumei_id", ""}, // nothing pushed yet: the cached one stays
		{"lw_res_version", ""},
	})
	if err != nil {
		t.Fatalf("replayTa: %v", err)
	}
	const want = `{"#os":"iOS","#carrier":"中国","#fps":120,"#ram":"1.9/5.6","pd_dl":2.50,"lw_ab":["x",{"y":1}],` +
		`"lw_version":"1.0.370","lw_device_id":"new-device_n3d","lw_shumei_id":"cached-id","lw_res_version":"1.0.364.1","lw_zone":"APS783"}`
	if got != want {
		t.Errorf("replayTa:\n got %s\nwant %s", got, want)
	}
}

// TestReplayTaAppendsMissingKeys: a refreshed key the capture lacks (a super property the client
// sets after the captured Login) is added after the captured keys, in taRefresh's order. An
// airKey's base64 '+', '/' and '=' go out as they are.
func TestReplayTaAppendsMissingKeys(t *testing.T) {
	got, err := replayTa(`{"#os":"iOS"}`, []taKey{{"lw_airKey", "lwDid_a+b/c=="}, {"lw_shumei_id", ""}, {"lw_buildcode", "918"}})
	if err != nil {
		t.Fatalf("replayTa: %v", err)
	}
	if want := `{"#os":"iOS","lw_airKey":"lwDid_a+b/c==","lw_buildcode":"918"}`; got != want {
		t.Errorf("replayTa = %s, want %s", got, want)
	}
}

// TestReplayTaRefusesWhatIsNotOneObject: only one JSON object can be replayed.
func TestReplayTaRefusesWhatIsNotOneObject(t *testing.T) {
	for _, captured := range []string{"", "{}x", `{"a":1}{"b":2}`, `["a"]`, `"ta"`, `{"a":}`, `{"a":1`, "null"} {
		if got, err := replayTa(captured, nil); err == nil {
			t.Errorf("replayTa(%q) = %q, want an error", captured, got)
		}
	}
	if got, err := replayTa(" {} \n", nil); err != nil || got != "{}" {
		t.Errorf("replayTa of an empty object = %q, %v; want {}", got, err)
	}
}

// TestBuildLoginParamsTaIOSFillsTheDeviceKeys: without a captured ta, the iOS blob carries the
// Login's deviceId and airKey and the pushed shumeiId, as SetDeviceIdSuperProperty sets them
// (Assembly-CSharp.decompiled.cs:59117-59125, 1.0.370), never the shumeiBoxId. Android keeps the
// three blank.
func TestBuildLoginParamsTaIOSFillsTheDeviceKeys(t *testing.T) {
	in := LoginParamsInput{
		DeviceID: "dev-1_n3d", AirKey: "lwDid_ZGV2LTFfbjNk", ShumeiBoxId: "box-1", ShumeiID: "pushed-1", ServerID: "783",
	}
	for _, tc := range []struct {
		ios                       bool
		device, airKey, shumeiID  string
		capturedButAndroidIgnored bool
	}{
		{ios: true, device: "dev-1_n3d", airKey: "lwDid_ZGV2LTFfbjNk", shumeiID: "pushed-1"},
		{ios: false},
		{ios: false, capturedButAndroidIgnored: true},
	} {
		in.IOSMode = tc.ios
		in.Device = CapturedDevice{}
		if tc.capturedButAndroidIgnored {
			in.Device.Ta = `{"#os":"iOS","lw_device_id":"captured"}`
		}
		var ta map[string]any
		if err := json.Unmarshal([]byte(BuildLoginParams(in).GetString("ta")), &ta); err != nil {
			t.Fatalf("ios=%v: ta is not JSON: %v", tc.ios, err)
		}
		for k, want := range map[string]string{"lw_device_id": tc.device, "lw_airKey": tc.airKey, "lw_shumei_id": tc.shumeiID} {
			if ta[k] != want {
				t.Errorf("ios=%v captured=%v: ta[%s] = %v, want %q", tc.ios, tc.capturedButAndroidIgnored, k, ta[k], want)
			}
		}
		if tc.capturedButAndroidIgnored && ta["#os"] != "Android" {
			t.Errorf("an Android Login replayed a captured ta: #os = %v", ta["#os"])
		}
	}
}

// TestBuildLoginParamsReplayedTaTakesTheRunsIDsAndZone: a replayed iOS ta carries this Login's
// deviceId and airKey (SetDeviceIdSuperProperty, Assembly-CSharp.decompiled.cs:59117-59125,
// 1.0.370) and its zone (LoginTracking's lw_zone = AccountCredentialManager.ServerInfo.zone,
// :59158, 59189), in place, with the key order kept. The capture was made under another deviceId
// (as a -cs-deviceid run has) on server 781 (before a move); its Login's deviceId and airKey
// would otherwise differ from the ta's. A Login without a serverId keeps the captured zone.
func TestBuildLoginParamsReplayedTaTakesTheRunsIDsAndZone(t *testing.T) {
	const captured = `{"#os":"iOS","#app_version":"1.0.370","lw_res_version":"1.0.370.1","lw_buildcode":"918",` +
		`"lw_device_id":"made-up-captured_n3d","lw_main_level":25,"lw_shumei_id":"cached-1",` +
		`"lw_airKey":"lwDid_bWFkZS11cC1jYXB0dXJlZF9uM2Q=","lw_version":"1.0.370","lw_zone":"APS781"}`
	for _, tc := range []struct{ serverID, want string }{
		{"783", `{"#os":"iOS","#app_version":"1.0.370","lw_res_version":"1.0.370.1","lw_buildcode":"918",` +
			`"lw_device_id":"dev-1_n3d","lw_main_level":25,"lw_shumei_id":"cached-1",` +
			`"lw_airKey":"lwDid_ZGV2LTFfbjNk","lw_version":"1.0.370","lw_zone":"APS783"}`},
		{"", `{"#os":"iOS","#app_version":"1.0.370","lw_res_version":"1.0.370.1","lw_buildcode":"918",` +
			`"lw_device_id":"dev-1_n3d","lw_main_level":25,"lw_shumei_id":"cached-1",` +
			`"lw_airKey":"lwDid_ZGV2LTFfbjNk","lw_version":"1.0.370","lw_zone":"APS781"}`},
	} {
		in := LoginParamsInput{
			IOSMode: true, DeviceID: "dev-1_n3d", AirKey: "lwDid_ZGV2LTFfbjNk", ServerID: tc.serverID,
			AppVersion: "1.0.370", VersionCode: "918",
		}
		in.Device.Ta = captured
		if got := BuildLoginParams(in).GetString("ta"); got != tc.want {
			t.Errorf("serverId %q: ta =\n %s\nwant\n %s", tc.serverID, got, tc.want)
		}
	}
}

// TestBuildLoginParamsUnusableCapturedTaSendsTheBuiltBlob: a captured ta that isn't one JSON
// object, or that would no longer fit an SFS string once refreshed, leaves the built blob, with a
// warning that names no value.
func TestBuildLoginParamsUnusableCapturedTaSendsTheBuiltBlob(t *testing.T) {
	base := LoginParamsInput{IOSMode: true, DeviceID: "dev-1_n3d", AirKey: "lwDid_ZGV2LTFfbjNk", ServerID: "783"}
	built := BuildLoginParams(base).GetString("ta")
	// Exactly at the cap as captured; the refreshed build and device keys make it longer.
	const shape = `{"lw_alliance_id":"","lw_buildcode":"1"}`
	atCap := `{"lw_alliance_id":"` + strings.Repeat("x", sfs.MaxUtfStringBytes-len(shape)) + `","lw_buildcode":"1"}`
	if len(atCap) != sfs.MaxUtfStringBytes {
		t.Fatalf("fixture is %d bytes, want %d", len(atCap), sfs.MaxUtfStringBytes)
	}
	for name, captured := range map[string]string{
		"not an object":    `["secret-in-a-list"]`,
		"truncated":        `{"lw_power":1,"secret`,
		"over the cap now": atCap,
	} {
		in := base
		in.Device.Ta = captured
		var buf bytes.Buffer
		got := func() string {
			orig := slog.Default()
			defer slog.SetDefault(orig)
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			return BuildLoginParams(in).GetString("ta")
		}()
		if got != built {
			t.Errorf("%s: ta = %.60q..., want the built blob", name, got)
		}
		if !strings.Contains(buf.String(), "level=WARN") || strings.Contains(buf.String(), "secret") {
			t.Errorf("%s: want a warning that shows no ta content, got:\n%s", name, buf.String())
		}
	}
}
