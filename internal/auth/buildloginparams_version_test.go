package auth

import (
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
)

// TestBuildLoginParamsBuildIdentity covers the appVersion/versionCode a Login claims. A token is
// bound to the build it was issued under, so a session config captured from an updated real app
// must be able to override the built-in defaults, in the top-level fields and in ta alike.
func TestBuildLoginParamsBuildIdentity(t *testing.T) {
	cases := []struct {
		name              string
		in                LoginParamsInput
		wantApp, wantCode string
	}{
		{"android default", LoginParamsInput{}, gsl.AppVersion, gsl.VersionCode},
		{"ios default", LoginParamsInput{IOSMode: true}, "1.0.344", "786"},
		{"ios captured build", LoginParamsInput{IOSMode: true, AppVersion: "1.0.350", VersionCode: "801"}, "1.0.350", "801"},
		{"android captured build", LoginParamsInput{AppVersion: "1.0.364", VersionCode: "1892"}, "1.0.364", "1892"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.ServerID = "783"
			p := BuildLoginParams(c.in)
			if got := p.GetString("appVersion"); got != c.wantApp {
				t.Errorf("appVersion = %q, want %q", got, c.wantApp)
			}
			if got := p.GetString("versionCode"); got != c.wantCode {
				t.Errorf("versionCode = %q, want %q", got, c.wantCode)
			}
			if c.in.IOSMode {
				ta := p.GetString("ta")
				for _, want := range []string{`"#app_version":"` + c.wantApp + `"`, `"lw_buildcode":"` + c.wantCode + `"`} {
					if !strings.Contains(ta, want) {
						t.Errorf("ta missing %s: %s", want, ta)
					}
				}
			}
		})
	}
}
