package auth

import (
	"encoding/json"
	"testing"

	"lastwar-client/internal/gsl"
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
	if got := p.GetString("packageName"); got != ta["#bundle_id"] {
		t.Errorf("ta #bundle_id %v does not match packageName %q", ta["#bundle_id"], got)
	}
}
