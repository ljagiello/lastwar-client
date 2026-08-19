package main

import (
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
