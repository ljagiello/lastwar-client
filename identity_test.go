package main

import "testing"

// Confirms BuildLoginParams' Android/iOS and empty-vs-set-GameUid conditional field logic --
// exactly the static-vs-dynamic field set whose mismatch caused the documented "reconnect wall"
// identity-mismatch production bug (see docs/live-validation.mdx).
func TestBuildLoginParamsConditionalFields(t *testing.T) {
	cases := []struct {
		name    string
		iosMode bool
		gameUid string
	}{
		{"android, empty gameUid", false, ""},
		{"ios, empty gameUid", true, ""},
		{"android, set gameUid", false, "12345"},
		{"ios, set gameUid", true, "12345"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := BuildLoginParams(LoginParamsInput{
				FutureID: 1,
				DeviceID: "dev-1",
				AirKey:   "air-1",
				GameUid:  c.gameUid,
				ServerID: "1234",
				IOSMode:  c.iosMode,
			})

			for _, key := range []string{"AndroidID", "IMEI"} {
				if got := p.Has(key); got != !c.iosMode {
					t.Errorf("Has(%q) = %v, want %v (IOSMode=%v)", key, got, !c.iosMode, c.iosMode)
				}
			}
			if got := p.Has("google_available"); got != !c.iosMode {
				t.Errorf("Has(google_available) = %v, want %v (IOSMode=%v)", got, !c.iosMode, c.iosMode)
			}

			for _, key := range []string{"idfa", "idfv", "phone_native_screen"} {
				if got := p.Has(key); got != c.iosMode {
					t.Errorf("Has(%q) = %v, want %v (IOSMode=%v)", key, got, c.iosMode, c.iosMode)
				}
			}

			wantEmptyUidFields := c.gameUid == ""
			for _, key := range []string{"country", "suggestCountry", "timeoffset", "gcmRegisterId", "referrer"} {
				if got := p.Has(key); got != wantEmptyUidFields {
					t.Errorf("Has(%q) = %v, want %v (GameUid=%q)", key, got, wantEmptyUidFields, c.gameUid)
				}
			}

			wantPackageName := packageName
			wantPlatform := "1"
			wantPf := "market_global"
			wantAppVersion := appVersion
			wantVersionCode := versionCode
			if c.iosMode {
				wantPackageName = iosPackageName
				wantPlatform = "0"
				wantPf = "AppStore"
				wantAppVersion = "1.0.344"
				wantVersionCode = "786"
			}
			if got := p.GetString("packageName"); got != wantPackageName {
				t.Errorf("packageName = %q, want %q", got, wantPackageName)
			}
			if got := p.GetString("platform"); got != wantPlatform {
				t.Errorf("platform = %q, want %q", got, wantPlatform)
			}
			if got := p.GetString("pf"); got != wantPf {
				t.Errorf("pf = %q, want %q", got, wantPf)
			}
			if got := p.GetString("appVersion"); got != wantAppVersion {
				t.Errorf("appVersion = %q, want %q", got, wantAppVersion)
			}
			if got := p.GetString("versionCode"); got != wantVersionCode {
				t.Errorf("versionCode = %q, want %q", got, wantVersionCode)
			}
		})
	}
}
