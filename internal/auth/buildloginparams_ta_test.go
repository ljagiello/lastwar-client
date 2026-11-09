package auth

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
)

// TestBuildLoginParamsTaIOSUnchanged pins the iOS ta blob byte for byte: the cron's captured iOS
// session has logged in with exactly this shape since August 2026, so any change to it has to be
// deliberate.
func TestBuildLoginParamsTaIOSUnchanged(t *testing.T) {
	p := BuildLoginParams(LoginParamsInput{IOSMode: true, ServerID: "783", AppVersion: "1.0.364", VersionCode: "1892", ResVersion: "1.0.364.42"})
	const want = `{"#os":"iOS","#disk":"50.0/100.0","#device_id":"00000000-0000-0000-0000-000000000000","#os_version":"0.0","#system_language":"en","#bundle_id":"com.lastwar.ios","#carrier":"","#app_version":"1.0.364","#fps":0,"#ram":"0.0/0.0","#simulator":false,"#zone_offset":0,"#manufacturer":"Apple","#screen_height":0,"#network_type":"WIFI","#device_model":"unknown","#screen_width":0,"#install_time":"2000-01-01 00:00:00.000","lw_net":"wifi","lw_res_version":"1.0.364.42","lw_buildcode":"1892","lw_line":"","pd_dl":0,"lw_ab":"0","lw_platform":"AppStore","lw_game_session_id":"00000000-0000-0000-0000-000000000000","lw_first_launch":false,"lw_alliance_id":"00000000000000000000000000000000","lw_device_id":"","lw_device_level":0,"lw_main_level":0,"lw_shumei_id":"","lw_airKey":"","lw_version":"0.0.0","lw_power":0,"lw_zone":"APS783"}`
	if got := p.GetString("ta"); got != want {
		t.Errorf("iOS ta changed:\n got %s\nwant %s", got, want)
	}
}

// TestBuildLoginParamsTaAndroidMarkers covers discussion #22: an Android Login with ta="{}" gets
// ec=28/E005, so the Android blob must carry the Android platform markers and build.
func TestBuildLoginParamsTaAndroidMarkers(t *testing.T) {
	p := BuildLoginParams(LoginParamsInput{ServerID: "783", AppVersion: "1.0.364", VersionCode: "1892"})
	var ta map[string]any
	if err := json.Unmarshal([]byte(p.GetString("ta")), &ta); err != nil {
		t.Fatalf("ta is not a JSON object: %v", err)
	}
	for k, want := range map[string]any{
		"#os": "Android", "#bundle_id": gsl.PackageName, "lw_platform": "market_global",
		"#app_version": "1.0.364", "lw_version": "1.0.364", "lw_buildcode": "1892", "lw_zone": "APS783",
		"lw_device_id": "", "lw_shumei_id": "", "lw_airKey": "",
	} {
		if ta[k] != want {
			t.Errorf("ta[%q] = %v, want %q", k, ta[k], want)
		}
	}
	// The Android device model and manufacturer now describe the hardware the
	// (empty) deviceId selects -- matching the ShuMei fingerprint -- not the
	// old "unknown"/"Android" placeholders.
	hw := shumei.HardwareFor("")
	if ta["#manufacturer"] != hw.Build["manufacturer"] || ta["#device_model"] != hw.Model() {
		t.Errorf("ta #manufacturer/#device_model = %v/%v, want %q/%q",
			ta["#manufacturer"], ta["#device_model"], hw.Build["manufacturer"], hw.Model())
	}
	if got := p.GetString("packageName"); got != ta["#bundle_id"] {
		t.Errorf("ta #bundle_id %v does not match packageName %q", ta["#bundle_id"], got)
	}
}

// TestBuildLoginParamsAndroidDeviceFields checks the Android Login's device
// fields describe the same phone the ShuMei fingerprint does (shumei.HardwareFor),
// not the old "GoClient" placeholders: phone_model is the model, osVersion is
// Unity's "Android OS .." string, device_string/mt/AndroidID/IMEI are the
// DeviceManager/Device dumps, and androidDid is a per-device UUID.
func TestBuildLoginParamsAndroidDeviceFields(t *testing.T) {
	const deviceID = "0123456789abcdef0123456789abcdef_n3d"
	p := BuildLoginParams(LoginParamsInput{DeviceID: deviceID, AirKey: "lwDid_x", ServerID: "783", AppVersion: "1.0.364", VersionCode: "1892"})
	hw := shumei.HardwareFor(deviceID)

	for key, want := range map[string]string{
		"phone_model":   hw.Model(),
		"osVersion":     hw.OSVersionString(),
		"device_string": hw.DeviceString(),
		"mt":            hw.HandSetInfo(),
		"AndroidID":     hw.Serial,
		"IMEI":          hw.DeviceInfo(),
		"androidDid":    shumei.DeviceUUID(deviceID),
	} {
		if got := p.GetString(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// The ta blob's model and manufacturer agree with the top-level fields.
	var ta map[string]any
	if err := json.Unmarshal([]byte(p.GetString("ta")), &ta); err != nil {
		t.Fatalf("ta is not JSON: %v", err)
	}
	if ta["#device_model"] != hw.Model() || ta["#manufacturer"] != hw.Build["manufacturer"] {
		t.Errorf("ta #device_model/#manufacturer = %v/%v, want %q/%q",
			ta["#device_model"], ta["#manufacturer"], hw.Model(), hw.Build["manufacturer"])
	}
}

// TestBuildLoginParamsTaFallsBackWhenOverCap: lw_zone repeats the zone, so a near-cap zone would
// push ta past the SFS string cap. The blob is sent up to the cap, and past it "{}" goes out, with a
// warning (an established account rejects that Login with E005), so the Login still encodes.
func TestBuildLoginParamsTaFallsBackWhenOverCap(t *testing.T) {
	for _, iosMode := range []bool{false, true} {
		overhead := len(BuildLoginParams(LoginParamsInput{IOSMode: iosMode}).GetString("ta"))
		atCap := strings.Repeat("9", sfs.MaxUtfStringBytes-overhead)
		if got := len(BuildLoginParams(LoginParamsInput{IOSMode: iosMode, ServerID: atCap}).GetString("ta")); got != sfs.MaxUtfStringBytes {
			t.Errorf("IOSMode=%v at cap: len(ta) = %d, want the full blob of %d bytes", iosMode, got, sfs.MaxUtfStringBytes)
		}

		var buf bytes.Buffer
		over := func() *sfs.SFSObject {
			orig := slog.Default()
			defer slog.SetDefault(orig)
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			return BuildLoginParams(LoginParamsInput{IOSMode: iosMode, ServerID: atCap + "9"})
		}()
		if got := over.GetString("ta"); got != "{}" {
			t.Errorf("IOSMode=%v over cap: ta = %.40q..., want \"{}\"", iosMode, got)
		}
		if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "too long to encode") {
			t.Errorf("IOSMode=%v over cap: want a warning, got:\n%s", iosMode, buf.String())
		}
		if _, err := sfs.EncodeObject(over); err != nil {
			t.Errorf("IOSMode=%v over cap: Login params no longer encode: %v", iosMode, err)
		}
	}
}
