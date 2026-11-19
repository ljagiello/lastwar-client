package auth

import (
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
)

// TestBuildLoginParamsNetTypeIsWifi: netType 1 is ReachableViaLocalAreaNetwork, the wifi the ta
// blob describes; 2 is carrier data (DeviceManager.GetNetworkStatus,
// Assembly-CSharp.decompiled.cs:73295-73303; 1.0.370 audit B39).
func TestBuildLoginParamsNetTypeIsWifi(t *testing.T) {
	for _, ios := range []bool{true, false} {
		if got := BuildLoginParams(LoginParamsInput{IOSMode: ios, ServerID: "1"}).GetInt("netType"); got != 1 {
			t.Errorf("IOSMode=%v: netType = %d, want 1 (wifi)", ios, got)
		}
	}
}

// TestBuildLoginParamsNoZstdOmitsIsUseLz4: isUseLz4 goes out only while the new packet class and
// Zstd are on (Assembly-CSharp.decompiled.cs:83176-83179; 1.0.370 audit B43).
func TestBuildLoginParamsNoZstdOmitsIsUseLz4(t *testing.T) {
	for _, ios := range []bool{true, false} {
		if p := BuildLoginParams(LoginParamsInput{IOSMode: ios, ServerID: "1"}); !p.Has("isUseLz4") {
			t.Errorf("IOSMode=%v: isUseLz4 missing by default", ios)
		}
		if p := BuildLoginParams(LoginParamsInput{IOSMode: ios, ServerID: "1", NoZstd: true}); p.Has("isUseLz4") {
			t.Errorf("IOSMode=%v, NoZstd: isUseLz4 = %v, want it left out", ios, p.GetString("isUseLz4"))
		}
	}
}

// TestBuildLoginParamsReplaysCapturedIOSDevice: an iOS Login sends the captured device's fields
// (1.0.370 audit B15); an Android one keeps the hardware profile's.
func TestBuildLoginParamsReplaysCapturedIOSDevice(t *testing.T) {
	dev := CapturedDevice{
		PhoneModel: "iPhone15,2", OSVersion: "iOS 18.1", HandSetInfo: "mt-1", Idfv: "IDFV-1", DistinctID: "d-1",
		FromCountry: "PL", Lang: "pl", PhoneScreen: "1179*2556", Afuid: "af-1", FirebaseID: "fb-1",
	}
	p := BuildLoginParams(LoginParamsInput{IOSMode: true, ServerID: "783", GameUid: "g", Device: dev})
	for key, want := range map[string]string{
		"phone_model": "iPhone15,2", "osVersion": "iOS 18.1", "mt": "mt-1", "idfv": "IDFV-1", "distinct_id": "d-1",
		"fromCountry": "PL", "lang": "pl", "phone_screen": "1179*2556", "afuid": "af-1", "firebaseId": "fb-1",
		"idfa": "",
	} {
		if got := p.GetString(key); got != want {
			t.Errorf("iOS %s = %q, want %q", key, got, want)
		}
	}

	const deviceID = "0123456789abcdef_n3d"
	hw := shumei.HardwareFor(deviceID)
	a := BuildLoginParams(LoginParamsInput{DeviceID: deviceID, ServerID: "783", GameUid: "g", Device: dev})
	if a.GetString("phone_model") != hw.Model() || a.GetString("osVersion") != hw.OSVersionString() || a.GetString("fromCountry") != "US" {
		t.Errorf("Android took the captured iOS device: phone_model=%q osVersion=%q fromCountry=%q",
			a.GetString("phone_model"), a.GetString("osVersion"), a.GetString("fromCountry"))
	}
}

// TestCapturedDeviceOversizedFieldDropped: a field too long for an SFS UtfString is left out, so
// the Login still encodes.
func TestCapturedDeviceOversizedFieldDropped(t *testing.T) {
	dev := CapturedDevice{PhoneModel: strings.Repeat("x", sfs.MaxUtfStringBytes+1), OSVersion: "iOS 18.1"}
	p := BuildLoginParams(LoginParamsInput{IOSMode: true, ServerID: "783", Device: dev})
	if got := p.GetString("phone_model"); got != "GoClient" {
		t.Errorf("phone_model = %.20q..., want the placeholder", got)
	}
	if got := p.GetString("osVersion"); got != "iOS 18.1" {
		t.Errorf("osVersion = %q, want the captured one", got)
	}
	if _, err := sfs.EncodeObject(p); err != nil {
		t.Errorf("Login params don't encode: %v", err)
	}
}

func TestCapturedDeviceRedacts(t *testing.T) {
	d := CapturedDevice{Idfv: "IDFV-SECRET"}
	for _, s := range []string{d.String(), d.GoString(), d.LogValue().String()} {
		if strings.Contains(s, "IDFV-SECRET") {
			t.Errorf("%q leaks the idfv", s)
		}
	}
}

// TestGameServerFromCarriesReplyTokens: a lookup reply's at/rt come back for the caller to log in
// with, as the client's SaveGameLoginToken stores them (1.0.370 audit B40).
func TestGameServerFromCarriesReplyTokens(t *testing.T) {
	lsr := &gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: "gw.example", Port: "10783", Zone: "APS783", GameUid: "g"}},
		At:         &gsl.LoginToken{Token: "at-new", Time: "100"},
		Rt:         &gsl.LoginToken{Token: "rt-new", Time: "101"},
	}
	s, err := gameServerFrom(lsr, "g", "APS783", "at-old")
	if err != nil {
		t.Fatal(err)
	}
	if s.AccessTok != "at-new" || s.AccessTokTime != 100 || s.RefreshTok != "rt-new" || s.RefreshTokTime != 101 {
		t.Errorf("tokens = %q/%d %q/%d, want at-new/100 rt-new/101", s.AccessTok, s.AccessTokTime, s.RefreshTok, s.RefreshTokTime)
	}
	lsr.At, lsr.Rt = nil, nil
	if s, err = gameServerFrom(lsr, "g", "APS783", "at-old"); err != nil || s.AccessTok != "" || s.RefreshTok != "" {
		t.Errorf("no reply tokens: got %q/%q (err %v), want none", s.AccessTok, s.RefreshTok, err)
	}
}

// TestLegacyConfigBuild pins the builds a session config without appVersion/versionCode keeps
// claiming (1.0.370 audit B21), and the new defaults.
func TestLegacyConfigBuild(t *testing.T) {
	if a, c := LegacyConfigBuild(true); a != "1.0.344" || c != "786" {
		t.Errorf("iOS legacy = %s/%s, want 1.0.344/786", a, c)
	}
	if a, c := LegacyConfigBuild(false); a != "1.0.364" || c != "1892" {
		t.Errorf("Android legacy = %s/%s, want 1.0.364/1892", a, c)
	}
	if b := loginBuild(true, "", ""); b.AppVersion != "1.0.370" || b.VersionCode != "918" {
		t.Errorf("iOS default = %s/%s, want 1.0.370/918", b.AppVersion, b.VersionCode)
	}
	if b := loginBuild(false, "", ""); b.AppVersion != "1.0.370" || b.VersionCode != "1911" {
		t.Errorf("Android default = %s/%s, want 1.0.370/1911", b.AppVersion, b.VersionCode)
	}
}
