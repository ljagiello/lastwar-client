package auth

import (
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// CheckVersionFor fetches check-version as the build a Login will claim: the iOS build under
// -cs-ios, otherwise the Android build. zone is the session's zone ("APS783"), or "" when it is
// not known yet. The reply's host is the gate for the GSL calls that follow, and its resMsg is
// their RSA key.
func CheckVersionFor(httpClient *http.Client, iosMode bool, appVersion, versionCode, zone string) (*gsl.CheckVersionResponse, string, error) {
	return gsl.CheckVersionAs(httpClient, loginBuild(iosMode, appVersion, versionCode), zone)
}

// ResVersionFrom returns the Login resVersion for a check-version reply already fetched as the
// build that Login will claim (see gsl.CheckVersionResponse.ResVersion), or "" when it cannot be
// derived, in which case BuildLoginParams sends "0". It never fails a login: a missing reply or a
// reply without the manifest fields is one Warn.
func ResVersionFrom(cv *gsl.CheckVersionResponse, iosMode bool, appVersion, versionCode string) string {
	build := loginBuild(iosMode, appVersion, versionCode)
	if cv == nil {
		slog.Warn(`resVersion: no check-version reply this run; sending resVersion "0"`, "packageName", build.PackageName)
		return ""
	}
	rv := cv.ResVersion()
	if rv == "" {
		slog.Warn(`resVersion: check-version lacks GameRes, DllRes, lwfile2, table_version or locale; sending resVersion "0"`,
			"packageName", build.PackageName)
		return ""
	}
	slog.Info("resVersion derived from check-version", "resVersion", rv, "packageName", build.PackageName)
	return rv
}
