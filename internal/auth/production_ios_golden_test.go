package auth

import (
	"fmt"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// productionIOSLoginInput is the shape of the cron's Login: the reconnect (non-empty gameUid) of a
// captured iOS session that carries its own build (1.0.370/918) and access token, with a resVersion
// derived from check-version. The values are stand-ins; only the shape matters.
func productionIOSLoginInput() LoginParamsInput {
	return LoginParamsInput{
		FutureID:   1,
		DeviceID:   "0123456789ABCDEF0123456789ABCDEF_n3d",
		AirKey:     "lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk",
		GameUid:    "1234567890123",
		AccessTok:  "golden-access-token",
		ServerID:   "783",
		IOSMode:    true,
		AppVersion: "1.0.370", VersionCode: "918",
		ResVersion: "2172.1514_836F_40460.23686",
	}
}

// dumpLoginParams lists every key of a Login parameter object in wire order with its SFS type and
// value. Values that change on every call (the time and the random codes derived from it) are
// replaced by a marker so the dump is stable.
func dumpLoginParams(p *sfs.SFSObject) string {
	volatile := map[string]bool{"cmdBaseTime": true, "SecurityCode": true, "OneCode": true, "CoreV": true, "psh": true}
	var b strings.Builder
	for _, k := range p.Keys() {
		v, _ := p.Get(k)
		val := fmt.Sprint(v.Val)
		if o, ok := v.Val.(*sfs.SFSObject); ok {
			val = fmt.Sprintf("object(%d keys)", len(o.Keys()))
		}
		if volatile[k] {
			val = "<volatile>"
		}
		fmt.Fprintf(&b, "%s %d [%s]\n", k, v.Type, val)
	}
	return b.String()
}

// TestProductionIOSLoginParamsGolden pins every byte of the production iOS Login that a code change
// could move (rule 10 of the 1.0.370 audit brief): the key set, the order, each SFS type and each
// value. A change here must be deliberate and backed by the 1.0.370 iOS LoginMessage.CSSetData
// (Assembly-CSharp.decompiled.cs:83053-83185).
//
// Changed on purpose by the 1.0.370 follow-up (B15, the user's decision of 2026-11-19): ta
// lw_device_id and lw_airKey are the Login's own deviceId and airKey, as SetDeviceIdSuperProperty
// sets them before every Login (:9334, 59121-59122; deviceId/airKey :83092-83093). lw_shumei_id
// stays "" here: this config has no pushed shumeiId yet.
func TestProductionIOSLoginParamsGolden(t *testing.T) {
	got := dumpLoginParams(BuildLoginParams(productionIOSLoginInput()))
	const want = `_id 4 [1]
netType 4 [1]
ta 8 [{"#os":"iOS","#disk":"50.0/100.0","#device_id":"00000000-0000-0000-0000-000000000000","#os_version":"0.0","#system_language":"en","#bundle_id":"com.lastwar.ios","#carrier":"","#app_version":"1.0.370","#fps":0,"#ram":"0.0/0.0","#simulator":false,"#zone_offset":0,"#manufacturer":"Apple","#screen_height":0,"#network_type":"WIFI","#device_model":"unknown","#screen_width":0,"#install_time":"2000-01-01 00:00:00.000","lw_net":"wifi","lw_res_version":"2172.1514_836F_40460.23686","lw_buildcode":"918","lw_line":"","pd_dl":0,"lw_ab":"0","lw_platform":"AppStore","lw_game_session_id":"00000000-0000-0000-0000-000000000000","lw_first_launch":false,"lw_alliance_id":"00000000000000000000000000000000","lw_device_id":"0123456789ABCDEF0123456789ABCDEF_n3d","lw_device_level":0,"lw_main_level":0,"lw_shumei_id":"","lw_airKey":"lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk","lw_version":"1.0.370","lw_power":0,"lw_zone":"APS783"}]
distinct_id 8 []
phone_screen 8 [1920*1080]
configVersion 18 [object(0 keys)]
psh 8 [<volatile>]
mt 8 []
deviceId 8 [0123456789ABCDEF0123456789ABCDEF_n3d]
airKey 8 [lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk]
cmdBaseTime 8 [<volatile>]
SecurityCode 8 [<volatile>]
OneCode 8 [<volatile>]
CoreV 8 [<volatile>]
googlePlay 8 []
androidDid 8 []
googleName 8 []
deeplinkParams 8 []
pfId 8 []
fromCountry 8 [US]
dataConfigMd5 8 []
idfa 8 []
idfv 8 []
phone_native_screen 8 [720*1280]
packageName 8 [com.lastwar.ios]
packageSign 8 [506d9b737f4da295c6050b8d9492e00ba00605c0]
platform 8 [0]
lat 4 [1]
pf 8 [AppStore]
firebaseId 8 []
afuid 8 []
phone_model 8 [GoClient]
configNumber 4 [0]
gaid 8 []
osVersion 8 [GoClient/1.0]
parseRegisterId 8 []
gameUid 8 [1234567890123]
appVersion 8 [1.0.370]
resVersion 8 [2172.1514_836F_40460.23686]
versionCode 8 [918]
lang 8 [en]
serverId 8 [783]
gmLogin 4 [0]
KCPMode 8 [0]
forbidden_froce_merge 4 [1]
shumeiBoxId 8 []
simOp 8 []
simOpName 8 []
delete_account_status 4 [0]
isUseLz4 1 [true]
at 8 [golden-access-token]
`
	if got != want {
		t.Errorf("production iOS Login params changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// goldenCapturedDevice is a made-up iOS device in the shape cmd/pcap -session-out records from a
// capture of the real app (none of these values comes from a real device).
func goldenCapturedDevice() CapturedDevice {
	return CapturedDevice{
		PhoneModel: "iPhone99,9", OSVersion: "iOS 99.1", HandSetInfo: "made-up-mt", Idfv: "00000000-1111-2222-3333-444444444444",
		DistinctID: "made-up-distinct-id", FromCountry: "PL", Lang: "pl", PhoneScreen: "1170*2532",
		Afuid: "1700000000000-1111111111111111111", FirebaseID: "made-up-firebase-id",
	}
}

// TestProductionIOSLoginParamsGoldenCapturedDevice is TestProductionIOSLoginParamsGolden for a
// session config that carries a captured device recorded before -session-out kept the ta (cmd/pcap
// -session-out on an iOS capture). Its ta changed with TestProductionIOSLoginParamsGolden's, for
// the same lines.
func TestProductionIOSLoginParamsGoldenCapturedDevice(t *testing.T) {
	in := productionIOSLoginInput()
	in.Device = goldenCapturedDevice()
	got := dumpLoginParams(BuildLoginParams(in))
	const want = `_id 4 [1]
netType 4 [1]
ta 8 [{"#os":"iOS","#disk":"50.0/100.0","#device_id":"00000000-0000-0000-0000-000000000000","#os_version":"0.0","#system_language":"en","#bundle_id":"com.lastwar.ios","#carrier":"","#app_version":"1.0.370","#fps":0,"#ram":"0.0/0.0","#simulator":false,"#zone_offset":0,"#manufacturer":"Apple","#screen_height":0,"#network_type":"WIFI","#device_model":"unknown","#screen_width":0,"#install_time":"2000-01-01 00:00:00.000","lw_net":"wifi","lw_res_version":"2172.1514_836F_40460.23686","lw_buildcode":"918","lw_line":"","pd_dl":0,"lw_ab":"0","lw_platform":"AppStore","lw_game_session_id":"00000000-0000-0000-0000-000000000000","lw_first_launch":false,"lw_alliance_id":"00000000000000000000000000000000","lw_device_id":"0123456789ABCDEF0123456789ABCDEF_n3d","lw_device_level":0,"lw_main_level":0,"lw_shumei_id":"","lw_airKey":"lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk","lw_version":"1.0.370","lw_power":0,"lw_zone":"APS783"}]
distinct_id 8 [made-up-distinct-id]
phone_screen 8 [1170*2532]
configVersion 18 [object(0 keys)]
psh 8 [<volatile>]
mt 8 [made-up-mt]
deviceId 8 [0123456789ABCDEF0123456789ABCDEF_n3d]
airKey 8 [lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk]
cmdBaseTime 8 [<volatile>]
SecurityCode 8 [<volatile>]
OneCode 8 [<volatile>]
CoreV 8 [<volatile>]
googlePlay 8 []
androidDid 8 []
googleName 8 []
deeplinkParams 8 []
pfId 8 []
fromCountry 8 [PL]
dataConfigMd5 8 []
idfa 8 []
idfv 8 [00000000-1111-2222-3333-444444444444]
phone_native_screen 8 [720*1280]
packageName 8 [com.lastwar.ios]
packageSign 8 [506d9b737f4da295c6050b8d9492e00ba00605c0]
platform 8 [0]
lat 4 [1]
pf 8 [AppStore]
firebaseId 8 [made-up-firebase-id]
afuid 8 [1700000000000-1111111111111111111]
phone_model 8 [iPhone99,9]
configNumber 4 [0]
gaid 8 []
osVersion 8 [iOS 99.1]
parseRegisterId 8 []
gameUid 8 [1234567890123]
appVersion 8 [1.0.370]
resVersion 8 [2172.1514_836F_40460.23686]
versionCode 8 [918]
lang 8 [pl]
serverId 8 [783]
gmLogin 4 [0]
KCPMode 8 [0]
forbidden_froce_merge 4 [1]
shumeiBoxId 8 []
simOp 8 []
simOpName 8 []
delete_account_status 4 [0]
isUseLz4 1 [true]
at 8 [golden-access-token]
`
	if got != want {
		t.Errorf("production iOS Login params (captured device) changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}

// goldenCapturedTa is a made-up ta blob in the shape of the real iOS app's (preset properties, then
// super properties; none of these values comes from a real device), captured under an older build
// (1.0.364/900) with the client's cached shumeiId of that time.
const goldenCapturedTa = `{"#os":"iOS","#disk":"41.2/119.1","#device_id":"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE","#os_version":"99.1","#system_language":"pl","#bundle_id":"com.lastwar.ios","#carrier":"","#app_version":"1.0.364","#fps":120,"#ram":"1.9/5.6","#simulator":false,"#zone_offset":1,"#manufacturer":"Apple","#screen_height":2532,"#network_type":"WIFI","#device_model":"iPhone99,9","#screen_width":1170,"#install_time":"2026-01-02 03:04:05.678","lw_net":"wifi","lw_res_version":"1.0.364.1","lw_buildcode":"900","lw_line":"1","pd_dl":2,"lw_ab":"7","lw_platform":"AppStore","lw_game_session_id":"99999999-8888-7777-6666-555555555555","lw_first_launch":false,"lw_alliance_id":"made-up-alliance-id","lw_device_id":"0123456789ABCDEF0123456789ABCDEF_n3d","lw_device_level":3,"lw_main_level":25,"lw_shumei_id":"made-up-cached-shumei-id","lw_airKey":"lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk","lw_version":"1.0.364","lw_power":12345678,"lw_zone":"APS783"}`

// TestProductionIOSLoginParamsGoldenCapturedTa is TestProductionIOSLoginParamsGolden for a session
// config recorded by the 1.0.370 follow-up's cmd/pcap -session-out: a captured device with its ta,
// and the shumeiId the server pushed. Only ta differs from
// TestProductionIOSLoginParamsGoldenCapturedDevice. It is the captured blob, key order and values
// kept, with these keys taken from this Login:
//   - lw_version: 1.0.364 -> 1.0.370, the claimed appVersion (GameEntry.Sdk.Version,
//     Assembly-CSharp.decompiled.cs:59190-59193);
//   - #app_version: 1.0.364 -> 1.0.370, ThinkingData's preset app version, the same build (the
//     iOS SDK is not in the decompiled set; the built blob has always sent the claimed appVersion);
//   - lw_buildcode: 900 -> 918, the claimed versionCode (Sdk.VersionCode, :59198-59201);
//   - lw_res_version: 1.0.364.1 -> this Login's resVersion (Resource.GetResVersion(), :59194-59197);
//   - lw_shumei_id: the cached id -> the pushed one (PlayerPrefs LW_CacheShumeiDeviceId,
//     :59119-59123);
//   - lw_device_id, lw_airKey: the Login's deviceId and airKey (:59121-59122), equal to the
//     captured ones here, as the capture's deviceId is the session's.
func TestProductionIOSLoginParamsGoldenCapturedTa(t *testing.T) {
	in := productionIOSLoginInput()
	in.Device = goldenCapturedDevice()
	in.Device.Ta = goldenCapturedTa
	in.ShumeiID = "made-up-pushed-shumei-id"
	got := dumpLoginParams(BuildLoginParams(in))
	const want = `_id 4 [1]
netType 4 [1]
ta 8 [{"#os":"iOS","#disk":"41.2/119.1","#device_id":"AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE","#os_version":"99.1","#system_language":"pl","#bundle_id":"com.lastwar.ios","#carrier":"","#app_version":"1.0.370","#fps":120,"#ram":"1.9/5.6","#simulator":false,"#zone_offset":1,"#manufacturer":"Apple","#screen_height":2532,"#network_type":"WIFI","#device_model":"iPhone99,9","#screen_width":1170,"#install_time":"2026-01-02 03:04:05.678","lw_net":"wifi","lw_res_version":"2172.1514_836F_40460.23686","lw_buildcode":"918","lw_line":"1","pd_dl":2,"lw_ab":"7","lw_platform":"AppStore","lw_game_session_id":"99999999-8888-7777-6666-555555555555","lw_first_launch":false,"lw_alliance_id":"made-up-alliance-id","lw_device_id":"0123456789ABCDEF0123456789ABCDEF_n3d","lw_device_level":3,"lw_main_level":25,"lw_shumei_id":"made-up-pushed-shumei-id","lw_airKey":"lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk","lw_version":"1.0.370","lw_power":12345678,"lw_zone":"APS783"}]
distinct_id 8 [made-up-distinct-id]
phone_screen 8 [1170*2532]
configVersion 18 [object(0 keys)]
psh 8 [<volatile>]
mt 8 [made-up-mt]
deviceId 8 [0123456789ABCDEF0123456789ABCDEF_n3d]
airKey 8 [lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk]
cmdBaseTime 8 [<volatile>]
SecurityCode 8 [<volatile>]
OneCode 8 [<volatile>]
CoreV 8 [<volatile>]
googlePlay 8 []
androidDid 8 []
googleName 8 []
deeplinkParams 8 []
pfId 8 []
fromCountry 8 [PL]
dataConfigMd5 8 []
idfa 8 []
idfv 8 [00000000-1111-2222-3333-444444444444]
phone_native_screen 8 [720*1280]
packageName 8 [com.lastwar.ios]
packageSign 8 [506d9b737f4da295c6050b8d9492e00ba00605c0]
platform 8 [0]
lat 4 [1]
pf 8 [AppStore]
firebaseId 8 [made-up-firebase-id]
afuid 8 [1700000000000-1111111111111111111]
phone_model 8 [iPhone99,9]
configNumber 4 [0]
gaid 8 []
osVersion 8 [iOS 99.1]
parseRegisterId 8 []
gameUid 8 [1234567890123]
appVersion 8 [1.0.370]
resVersion 8 [2172.1514_836F_40460.23686]
versionCode 8 [918]
lang 8 [pl]
serverId 8 [783]
gmLogin 4 [0]
KCPMode 8 [0]
forbidden_froce_merge 4 [1]
shumeiBoxId 8 []
simOp 8 []
simOpName 8 []
delete_account_status 4 [0]
isUseLz4 1 [true]
at 8 [golden-access-token]
`
	if got != want {
		t.Errorf("production iOS Login params (captured device and ta) changed:\n got:\n%s\nwant:\n%s", got, want)
	}
}
