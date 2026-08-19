package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestStringRedactedMasksSensitiveKeys is the codec-layer regression test for the round-11
// credential-leak sweep (interactive.go, gsl.go, crossserver.go, login.go's waitFor/waitForCmd
// call sites all switched from String() to StringRedacted()): proves StringRedacted masks every
// known-sensitive field while leaving ordinary gameplay fields untouched, both at the top level
// and inside a nested SFSObject/SFSArray -- mirroring String()'s own recursive behavior.
func TestStringRedactedMasksSensitiveKeys(t *testing.T) {
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"
	const secretAccessTok = "sensitive-secret-accesstok-must-not-leak-0987654321"

	inner := NewSFSObject()
	inner.PutUtfString("loginKey", secretLoginKey)
	inner.PutUtfString("gameUid", "g-123456")

	arr := NewSFSArray()
	arr.AddSFSObject(inner)

	o := NewSFSObject()
	o.PutUtfString("at", secretAccessTok)
	o.PutUtfString("un", "player-one")
	o.PutSFSArray("accountArr", arr)

	got := o.StringRedacted()

	if strings.Contains(got, secretLoginKey) {
		t.Errorf("StringRedacted leaks the nested loginKey in cleartext: %s", got)
	}
	if strings.Contains(got, secretAccessTok) {
		t.Errorf("StringRedacted leaks the top-level at (access token) in cleartext: %s", got)
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
	if !strings.Contains(got, "g-123456") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive nested fields, got: %s", got)
	}
	// redact()'s own shape (first4...last4) should still be visible for a long secret, proving
	// this went through actual redaction rather than, say, dropping the field entirely.
	if !strings.Contains(got, "sens...7890") {
		t.Errorf("StringRedacted should mask loginKey via redact()'s first4...last4 shape, got: %s", got)
	}
}

// TestStringRedactedMasksNewSensitiveKeys is the round-12 regression test for the completeness
// gap the round-12 audit found in round 11's sensitiveSFSKeys: verifyCode (the live one-time
// email-verification code, login.go's account.login.new), deviceId (the SFS-layer bearer
// credential paired with airKey, identity.go's BuildLoginParams), chatToken (the separate chat
// WebSocket's bearer credential, docs/auth.mdx's `init` push), and tk (the vanilla SFS2X
// Handshake response's session token, docs/wire-protocol.mdx) were all real credential fields
// missing from the map, so StringRedacted printed them in cleartext.
func TestStringRedactedMasksNewSensitiveKeys(t *testing.T) {
	const secretVerifyCode = "secret-verifycode-must-not-leak-123456"
	const secretDeviceId = "secret-deviceid-must-not-leak-234567"
	const secretChatToken = "secret-chattoken-must-not-leak-345678"
	const secretTk = "secret-tk-sessiontoken-must-not-leak-456789"

	o := NewSFSObject()
	o.PutUtfString("verifyCode", secretVerifyCode)
	o.PutUtfString("deviceId", secretDeviceId)
	o.PutUtfString("chatToken", secretChatToken)
	o.PutUtfString("tk", secretTk)
	o.PutUtfString("un", "player-one")

	got := o.StringRedacted()

	for _, secret := range []string{secretVerifyCode, secretDeviceId, secretChatToken, secretTk} {
		if strings.Contains(got, secret) {
			t.Errorf("StringRedacted leaks a new sensitive key in cleartext (%q): %s", secret, got)
		}
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestStringRedactedMasksSensitivePrimitiveArray is the round-12 regression test for the
// formatSFSValueRedacted completeness gap the round-12 audit found: a sensitive key whose value
// is one of the 8 primitive array types readValuePayload's array-tag cases decode into (plain
// unwrapped Go slices, not *SFSObject/*SFSArray) fell through formatSFSValueRedacted's type
// switch into its naive `default: fmt.Sprintf("%v", val)` case, printing the raw slice contents
// with no masking at all -- defeating redactSFSValue's whole point for that shape.
func TestStringRedactedMasksSensitivePrimitiveArray(t *testing.T) {
	secretStrings := []string{"secret-arr-item-must-not-leak-1", "secret-arr-item-must-not-leak-2"}
	secretInts := []int32{918273645, 192837465}

	strObj := NewSFSObject()
	strObj.put("loginKey", SFSValue{sfsUtfStringArray, secretStrings})
	strObj.PutUtfString("un", "player-one")

	gotStr := strObj.StringRedacted()
	for _, s := range secretStrings {
		if strings.Contains(gotStr, s) {
			t.Errorf("StringRedacted leaks a []string primitive array under a sensitive key: %s", gotStr)
		}
	}
	if !strings.Contains(gotStr, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", gotStr)
	}

	intObj := NewSFSObject()
	intObj.put("loginKey", SFSValue{sfsIntArray, secretInts})

	gotInt := intObj.StringRedacted()
	for _, n := range secretInts {
		if strings.Contains(gotInt, fmt.Sprintf("%d", n)) {
			t.Errorf("StringRedacted leaks a []int32 primitive array under a sensitive key: %s", gotInt)
		}
	}
}

// TestStringRedactedMatchesStringForNonSensitiveData proves StringRedacted is a pure superset of
// String()'s behavior for data with no sensitive keys -- it must not, say, accidentally drop or
// reorder ordinary fields.
func TestStringRedactedMatchesStringForNonSensitiveData(t *testing.T) {
	o := NewSFSObject()
	o.PutUtfString("uid", "1113165390000783")
	o.PutInt("level", 5)
	o.PutBool("collected", true)

	if got, want := o.StringRedacted(), o.String(); got != want {
		t.Errorf("StringRedacted() = %q, want it to match String() = %q when no sensitive keys are present", got, want)
	}
}
