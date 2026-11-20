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
//
// "ta" is a redacted key now, so the redacted dump must show none of them on either platform. The
// iOS blob carries the deviceId and airKey on purpose, as the client's does (lw_device_id/lw_airKey,
// Assembly-CSharp.decompiled.cs:59117-59125, 1.0.370; audit B15): only the raw iOS ta may hold them.
// The shumeiBoxId never goes into ta: lw_shumei_id is the pushed shumeiId, a different value.
func TestBuildLoginParamsDoesNotLeakSecretsInAnalyticsBlob(t *testing.T) {
	const secretDeviceID = "secret-device-id-must-not-leak-abcdef123456"
	const secretAirKey = "secret-air-key-must-not-leak-ghijkl789012"
	const secretShumeiBoxId = "secret-shumei-box-id-must-not-leak-mnopqr345678"

	// Both platforms build the blob (discussion #22), so both are checked.
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

		for _, secret := range []string{secretDeviceID, secretAirKey, secretShumeiBoxId} {
			if out := p.StringRedacted(); strings.Contains(out, secret) {
				t.Errorf("IOSMode=%v: the redacted Login shows a secret identity value (%q): %s", iosMode, secret, out)
			}
		}
		rawTaSecrets := []string{secretShumeiBoxId}
		if !iosMode {
			rawTaSecrets = append(rawTaSecrets, secretDeviceID, secretAirKey)
		}
		for _, secret := range rawTaSecrets {
			if ta := p.GetString("ta"); strings.Contains(ta, secret) {
				t.Errorf("IOSMode=%v: the raw ta blob holds %q: %s", iosMode, secret, ta)
			}
		}
	}
}
