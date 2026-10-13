package auth

import (
	"strings"
	"testing"
)

// TestBuildLoginParamsDoesNotLeakSecretsInAnalyticsBlob is the round-13 regression test for
// the credential leak the round-13 audit found: BuildLoginParams' IOSMode branch built the "ta"
// analytics blob's LwDeviceID/LwShumeiID/LwAirKey fields directly from the real live
// in.DeviceID/in.ShumeiBoxId/in.AirKey values, JSON-marshaled the result, and stored it as a plain
// string under the "ta" key. Since "ta" wasn't in sfs.SensitiveSFSKeys, StringRedacted() masked the
// top-level deviceId/airKey/shumeiBoxId keys correctly but printed the identical secret values in
// full cleartext nested inside "ta"'s JSON value, in the same output string.
func TestBuildLoginParamsDoesNotLeakSecretsInAnalyticsBlob(t *testing.T) {
	const secretDeviceID = "secret-device-id-must-not-leak-abcdef123456"
	const secretAirKey = "secret-air-key-must-not-leak-ghijkl789012"
	const secretShumeiBoxId = "secret-shumei-box-id-must-not-leak-mnopqr345678"

	// Both platforms build the blob (discussion #22), so both are checked. StringRedacted() masks
	// "ta" as a whole key, so only the raw ta check below can see a secret inside the blob.
	for _, iosMode := range []bool{true, false} {
		p := BuildLoginParams(LoginParamsInput{
			FutureID:    1,
			DeviceID:    secretDeviceID,
			AirKey:      secretAirKey,
			GameUid:     "g-123456",
			ServerID:    "1234",
			ShumeiBoxId: secretShumeiBoxId,
			IOSMode:     iosMode,
		})

		for _, out := range []string{p.StringRedacted(), p.GetString("ta")} {
			for _, secret := range []string{secretDeviceID, secretAirKey, secretShumeiBoxId} {
				if strings.Contains(out, secret) {
					t.Errorf("IOSMode=%v: a secret identity value leaks in cleartext (possibly nested inside the ta analytics blob) (%q): %s", iosMode, secret, out)
				}
			}
		}
	}
}
