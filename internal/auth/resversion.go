package auth

import (
	"lastwar-client/internal/gsl"
	"log/slog"
	"net/http"
)

// ResVersionFor returns the Login resVersion for the build a Login claims (see
// gsl.CheckVersionResponse.ResVersion), or "" when it cannot be derived, in which case
// BuildLoginParams keeps sending "0". It never fails a login: every failure is one Warn.
//
// androidCV is the Android check-version reply the caller already fetched for the gate host and the
// GSL RSA key, and it only serves an Android-mode Login. iOS mode makes a second, anonymous
// check-version GET as the iOS build (gsl.CheckVersionAs: no uid, deviceId or token), used for
// nothing but this value. Two calls are needed because the access token is bound to the iOS build
// it was issued under, so an iOS-mode Login claims that build, and the hot-update versions differ by
// platform (Android 2343.1502_826F vs iOS 2159.1505_821F in October 2026). Reusing the Android
// reply would pair an iOS 1.0.344 Login with Android's manifest versions, a combination no real
// client sends. The gate host, RSA key and every GSL call still come from the Android reply, so no
// token path changes. With no Android reply (androidCV nil: every host failed) the iOS GET is
// skipped, since it would try the same hosts.
func ResVersionFor(httpClient *http.Client, androidCV *gsl.CheckVersionResponse, iosMode bool, appVersion, versionCode, zone string) string {
	cv, build := androidCV, gsl.AndroidBuild
	if iosMode {
		build = loginBuild(true, appVersion, versionCode)
		if androidCV == nil {
			// The Android check-version already failed on every host; trying the same hosts
			// again would only delay the login by up to one timeout per host.
			slog.Warn(`resVersion: no check-version reply this run; skipping the iOS check-version and sending resVersion "0"`)
			return ""
		}
		var err error
		if cv, _, err = gsl.CheckVersionAs(httpClient, build, zone); err != nil {
			slog.Warn(`resVersion: iOS check-version failed; sending resVersion "0"`,
				"appVersion", build.AppVersion, "versionCode", build.VersionCode, "error", err)
			return ""
		}
	}
	rv := cv.ResVersion()
	if rv == "" {
		slog.Warn(`resVersion: check-version lacks GameRes, DllRes, lwfile2, table_version or locale; sending resVersion "0"`,
			"packageName", build.PackageName, "haveReply", cv != nil)
		return ""
	}
	slog.Info("resVersion derived from check-version", "resVersion", rv, "packageName", build.PackageName)
	return rv
}
