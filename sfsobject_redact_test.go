package main

import (
	"bytes"
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

// TestStringRedactedMasksAllPrimitiveArrayTypes extends
// TestStringRedactedMasksSensitivePrimitiveArray to cover all 8 primitive-array wire types
// (sfsBoolArray..sfsUtfStringArray) under a sensitive key, not just []string/[]int32 -- proving
// primitiveArrayLen's type switch (and therefore redactSFSValue's masking) has no gap for any of
// the 8 shapes readValuePayload's array-tag cases can actually decode into.
func TestStringRedactedMasksAllPrimitiveArrayTypes(t *testing.T) {
	cases := []struct {
		name     string
		sfsType  byte
		val      interface{}
		wantSubs []string // substrings that must not appear in the redacted output
	}{
		{"BoolArray", sfsBoolArray, []bool{true, false, true}, nil},
		{"ByteArray", sfsByteArray, []byte{0xDE, 0xAD, 0xBE, 0xEF}, nil},
		{"ShortArray", sfsShortArray, []int16{-12345, 6789}, []string{"-12345", "6789"}},
		{"IntArray", sfsIntArray, []int32{918273645, 192837465}, []string{"918273645", "192837465"}},
		{"LongArray", sfsLongArray, []int64{1234567890123, 9876543210987}, []string{"1234567890123", "9876543210987"}},
		{"FloatArray", sfsFloatArray, []float32{3.14159, 2.71828}, []string{"3.14159", "2.71828"}},
		{"DoubleArray", sfsDoubleArray, []float64{1.6180339887, 1.4142135623}, []string{"1.618033", "1.414213"}},
		{"StringArray", sfsUtfStringArray, []string{"secret-item-alpha", "secret-item-beta"}, []string{"secret-item-alpha", "secret-item-beta"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewSFSObject()
			o.put("loginKey", SFSValue{c.sfsType, c.val})
			o.PutUtfString("un", "player-one")

			got := o.StringRedacted()

			for _, sub := range c.wantSubs {
				if strings.Contains(got, sub) {
					t.Errorf("StringRedacted leaks a %s primitive array under a sensitive key (found %q): %s", c.name, sub, got)
				}
			}
			if !strings.Contains(got, "player-one") {
				t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
			}
			if !strings.Contains(got, "REDACTED") {
				t.Errorf("StringRedacted should mask the %s array via the [REDACTED N items] shape, got: %s", c.name, got)
			}
		})
	}
}

// TestBuildLoginParamsIOSModeDoesNotLeakSecretsInAnalyticsBlob is the round-13 regression test for
// the credential leak the round-13 audit found: BuildLoginParams' IOSMode branch built the "ta"
// analytics blob's LwDeviceID/LwShumeiID/LwAirKey fields directly from the real live
// in.DeviceID/in.ShumeiBoxId/in.AirKey values, JSON-marshaled the result, and stored it as a plain
// string under the "ta" key. Since "ta" wasn't in sensitiveSFSKeys, StringRedacted() masked the
// top-level deviceId/airKey/shumeiBoxId keys correctly but printed the identical secret values in
// full cleartext nested inside "ta"'s JSON value, in the same output string.
func TestBuildLoginParamsIOSModeDoesNotLeakSecretsInAnalyticsBlob(t *testing.T) {
	const secretDeviceID = "secret-device-id-must-not-leak-abcdef123456"
	const secretAirKey = "secret-air-key-must-not-leak-ghijkl789012"
	const secretShumeiBoxId = "secret-shumei-box-id-must-not-leak-mnopqr345678"

	p := BuildLoginParams(LoginParamsInput{
		FutureID:    1,
		DeviceID:    secretDeviceID,
		AirKey:      secretAirKey,
		GameUid:     "g-123456",
		ServerID:    "1234",
		ShumeiBoxId: secretShumeiBoxId,
		IOSMode:     true,
	})

	got := p.StringRedacted()

	for _, secret := range []string{secretDeviceID, secretAirKey, secretShumeiBoxId} {
		if strings.Contains(got, secret) {
			t.Errorf("StringRedacted leaks a secret identity value (possibly nested inside the ta analytics blob) in cleartext (%q): %s", secret, got)
		}
	}
}

// TestStringRedactedMatchesStringForNonSensitiveData proves StringRedacted is a pure superset of
// String()'s behavior for data with no sensitive keys -- it must not, say, accidentally drop or
// reorder ordinary fields.
//
// Since round 14's Fix 1, String() itself delegates straight to StringRedacted() (see
// sfsobject.go), so this equality now holds unconditionally, not just for non-sensitive data --
// this test still keeps its original non-sensitive-only fixture as basic coverage, while
// TestFmtVerbAutoInvokesStringerSafely below is the test that specifically proves String() also
// redacts sensitive data reached via implicit fmt.Stringer auto-invocation.
func TestStringRedactedMatchesStringForNonSensitiveData(t *testing.T) {
	o := NewSFSObject()
	o.PutUtfString("uid", "1113165390000783")
	o.PutInt("level", 5)
	o.PutBool("collected", true)

	if got, want := o.StringRedacted(), o.String(); got != want {
		t.Errorf("StringRedacted() = %q, want it to match String() = %q when no sensitive keys are present", got, want)
	}
}

// TestFmtVerbAutoInvokesStringerSafely is the round-14 regression test for Fix 1, the structural
// fix to the credential-leak bug class this repo has hunted for four rounds: *SFSObject's String()
// method used to be the raw, unredacted dump (now renamed unsafeRawString(), see sfsobject.go), so
// ANY code path that handed a *SFSObject to fmt's %v/%s verbs, a Print-family function, or slog's
// Any-kind attribute formatting would automatically invoke it via fmt.Stringer -- with zero literal
// ".String()" text in the source, a pattern credential_leak_lint_test.go's text-scanning approach
// structurally cannot see. This test exercises exactly that implicit-invocation path (never calling
// .String()/.StringRedacted() explicitly) and confirms it never leaks a secret, proving Fix 1
// actually closes the gap rather than merely relying on every call site remembering to opt in.
func TestFmtVerbAutoInvokesStringerSafely(t *testing.T) {
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-via-stringer-1234567890"

	o := NewSFSObject()
	o.PutUtfString("loginKey", secretLoginKey)
	o.PutUtfString("un", "player-one")

	// %v is the classic implicit-Stringer verb -- no ".String()" substring appears anywhere in
	// this call.
	gotSprintfV := fmt.Sprintf("resp: %v", o)
	if strings.Contains(gotSprintfV, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%v\", o) leaks a secret via implicit Stringer auto-invocation: %s", gotSprintfV)
	}
	if !strings.Contains(gotSprintfV, "player-one") {
		t.Errorf("fmt.Sprintf(\"%%v\", o) must not mask ordinary non-sensitive fields, got: %s", gotSprintfV)
	}

	// %s also auto-invokes Stringer.
	gotSprintfS := fmt.Sprintf("resp: %s", o)
	if strings.Contains(gotSprintfS, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%s\", o) leaks a secret via implicit Stringer auto-invocation: %s", gotSprintfS)
	}

	// fmt.Errorf("...: %v", someSFSObject) is the exact ordinary, idiomatic pattern called out in
	// the round-14 assignment as the one a future contributor might write without realizing
	// SFSObject.String() used to be unredacted.
	gotErrorf := fmt.Errorf("request failed: %v", o)
	if strings.Contains(gotErrorf.Error(), secretLoginKey) {
		t.Errorf("fmt.Errorf(\"...: %%v\", o) leaks a secret via implicit Stringer auto-invocation: %s", gotErrorf.Error())
	}

	// A Print-family sink (fmt.Fprintln, writing to a buffer instead of stdout so the test stays
	// hermetic) also auto-invokes Stringer for a non-string argument.
	var buf bytes.Buffer
	fmt.Fprintln(&buf, o)
	if strings.Contains(buf.String(), secretLoginKey) {
		t.Errorf("fmt.Fprintln(w, o) leaks a secret via implicit Stringer auto-invocation: %s", buf.String())
	}
}

// TestStringRedactedMasksSensitiveRawSFSArray is the round-14 regression test for Fix 2: a
// sensitive key whose value is a raw *SFSArray (the wrapper type sfsArrayType decodes into, built
// here the same way PutSFSArray's callers do) of scalar items used to fall through redactSFSValue
// into formatSFSValueRedacted's *SFSArray case, which recurses via formatSFSValueRedacted (not
// redactSFSValue) on each item -- losing the "sensitive" context one level down, so each raw scalar
// item printed via the naive fmt.Sprintf("%v", val) default with no redaction at all. No current
// PutSFSArray call site does this for a sensitive key, but a future decoded server response could.
func TestStringRedactedMasksSensitiveRawSFSArray(t *testing.T) {
	const secretItem1 = "secret-raw-array-item-must-not-leak-1"
	const secretItem2 = "secret-raw-array-item-must-not-leak-2"

	arr := NewSFSArray()
	arr.add(SFSValue{sfsUtfString, secretItem1})
	arr.add(SFSValue{sfsUtfString, secretItem2})

	o := NewSFSObject()
	o.PutSFSArray("loginKey", arr)
	o.PutUtfString("un", "player-one")

	got := o.StringRedacted()

	for _, secret := range []string{secretItem1, secretItem2} {
		if strings.Contains(got, secret) {
			t.Errorf("StringRedacted leaks a raw *SFSArray-of-scalars value under a sensitive key: %s", got)
		}
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("StringRedacted should mask the *SFSArray via the [REDACTED N items] shape, got: %s", got)
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestStringRedactedMasksMailAndDeviceIdentifierPII is the round-15 regression test for Fixes 1-2:
// sensitiveSFSKeys was missing "mail" (the real user email address login.go's email-verification
// flow puts under this literal SFS key, PutUtfString("mail", opts.Email)) and the device/
// advertising-identifier PII cluster docs/live-validation.mdx documents as real fields a genuine
// (non-Go-client) Login request carries (IMEI, AndroidID, androidDid, idfa, idfv, gaid, afuid,
// firebaseId, distinct_id). Neither leaks from this Go client's own outgoing traffic today (which
// only ever sends these as empty-string placeholders), but decode.go's -decode-stream tool would
// have printed real values for these fields in cleartext when decoding a genuinely captured
// non-Go-client login, since StringRedacted() had no way to mask a key that isn't in the map.
func TestStringRedactedMasksMailAndDeviceIdentifierPII(t *testing.T) {
	secrets := map[string]string{
		"mail":        "secret-real-user-email-must-not-leak@example.com",
		"IMEI":        "secret-imei-must-not-leak-123456789012345",
		"AndroidID":   "secret-androidid-must-not-leak-abcdef0123456789",
		"androidDid":  "secret-androiddid-must-not-leak-fedcba9876543210",
		"idfa":        "secret-idfa-must-not-leak-00000000-1111-2222-3333-444444444444",
		"idfv":        "secret-idfv-must-not-leak-55555555-6666-7777-8888-999999999999",
		"gaid":        "secret-gaid-must-not-leak-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"afuid":       "secret-afuid-must-not-leak-ffffffff000011112222333344445555",
		"firebaseId":  "secret-firebaseid-must-not-leak-abc123def456ghi789",
		"distinct_id": "secret-distinctid-must-not-leak-xyz987uvw654rst321",
	}

	o := NewSFSObject()
	for key, val := range secrets {
		o.PutUtfString(key, val)
	}
	o.PutUtfString("un", "player-one")

	got := o.StringRedacted()

	for key, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Errorf("StringRedacted leaks %q's PII value in cleartext: %s", key, got)
		}
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestGoStringVerbNeverLeaksSecret is the round-15 regression test for Fix 3: *SFSObject satisfied
// fmt.Stringer safely (round 14) but not fmt.GoStringer, so fmt.Sprintf("%#v", obj) fell through to
// Go's default reflection-based struct formatter, dumping every internal field -- including the
// unexported values map holding a live loginKey/accessToken/etc -- completely raw. Adding GoString()
// (delegating to StringRedacted(), mirroring String()) closes that gap.
func TestGoStringVerbNeverLeaksSecret(t *testing.T) {
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-via-goformat-1234567890"

	o := NewSFSObject()
	o.PutUtfString("loginKey", secretLoginKey)
	o.PutUtfString("un", "player-one")

	got := fmt.Sprintf("%#v", o)
	if strings.Contains(got, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%#v\", o) leaks a secret via GoStringer/reflection fallback: %s", got)
	}
	// A reflection-based struct dump would print the unexported field name "values" verbatim (e.g.
	// "&main.SFSObject{values:map[...". If GoString() is missing or not being invoked, this
	// substring shows up; if GoString() is correctly wired, the output is StringRedacted()'s
	// {key=value, ...} shape instead, which never contains this literal field name.
	if strings.Contains(got, "values:map[") {
		t.Errorf("fmt.Sprintf(\"%%#v\", o) fell through to Go's reflection-based struct formatter instead of GoString(): %s", got)
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("fmt.Sprintf(\"%%#v\", o) must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestBareSFSArrayNeverLeaksViaFmtVerbs is the round-15 regression test for Fix 4: *SFSArray (the
// wrapper type sfsArrayType decodes into) had no String()/StringRedacted()/GoString() methods at
// all, so handing a bare *SFSArray (not wrapped in a parent SFSObject) directly to %v/%s/%#v/
// Println fell through to Go's default reflection-based formatter, printing its raw items
// (including any embedded strings) unredacted. Every current call site ranges over .items directly
// instead of logging the array value itself, so this was latent rather than actively exploited --
// but it's a real gap now that SFSObject itself is safe.
func TestBareSFSArrayNeverLeaksViaFmtVerbs(t *testing.T) {
	const secretItem = "secret-bare-array-item-must-not-leak-abcdef123456"

	arr := NewSFSArray()
	arr.add(SFSValue{sfsUtfString, secretItem})
	arr.add(SFSValue{sfsUtfString, "ordinary-item"})

	cases := map[string]string{
		"%v":  fmt.Sprintf("%v", arr),
		"%s":  fmt.Sprintf("%s", arr),
		"%#v": fmt.Sprintf("%#v", arr),
	}
	for verb, got := range cases {
		if strings.Contains(got, secretItem) {
			t.Errorf("fmt.Sprintf(%q, arr) on a bare *SFSArray leaks a raw scalar item: %s", verb, got)
		}
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, arr)
	if strings.Contains(buf.String(), secretItem) {
		t.Errorf("fmt.Fprintln(w, arr) on a bare *SFSArray leaks a raw scalar item: %s", buf.String())
	}
}

// TestNilNestedValueDoesNotPanic is the round-15 regression test for Fix 5: formatSFSValueRedacted
// (and StringRedacted() itself, on both *SFSObject and *SFSArray) used to have no nil check, so a
// nil *SFSObject/*SFSArray -- whether reached as a nested value inside a parent object/array, or
// called on directly -- would panic with a nil pointer dereference instead of returning a safe
// string. Not reachable from any current call site or from decoded server data (DecodeObject always
// constructs real objects), but a latent crash-on-future-mistake vector, e.g. a hypothetical future
// PutSFSObject(key, nil) call.
func TestNilNestedValueDoesNotPanic(t *testing.T) {
	t.Run("nil *SFSObject nested inside a parent SFSObject", func(t *testing.T) {
		var nilObj *SFSObject
		o := NewSFSObject()
		o.PutSFSObject("child", nilObj)
		o.PutUtfString("un", "player-one")

		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked on a nested nil *SFSObject: %v", r)
				}
			}()
			got = o.StringRedacted()
		}()
		if !strings.Contains(got, "<nil>") {
			t.Errorf("StringRedacted() on a nested nil *SFSObject = %q, want it to contain \"<nil>\"", got)
		}
	})

	t.Run("nil *SFSArray nested inside a parent SFSObject", func(t *testing.T) {
		var nilArr *SFSArray
		o := NewSFSObject()
		o.PutSFSArray("child", nilArr)
		o.PutUtfString("un", "player-one")

		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked on a nested nil *SFSArray: %v", r)
				}
			}()
			got = o.StringRedacted()
		}()
		if !strings.Contains(got, "<nil>") {
			t.Errorf("StringRedacted() on a nested nil *SFSArray = %q, want it to contain \"<nil>\"", got)
		}
	})

	t.Run("StringRedacted directly on a nil *SFSObject", func(t *testing.T) {
		var nilObj *SFSObject
		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked being called directly on a nil *SFSObject: %v", r)
				}
			}()
			got = nilObj.StringRedacted()
		}()
		if got != "<nil>" {
			t.Errorf("nilObj.StringRedacted() = %q, want %q", got, "<nil>")
		}
		if s := nilObj.String(); s != "<nil>" {
			t.Errorf("nilObj.String() = %q, want %q", s, "<nil>")
		}
		if gs := nilObj.GoString(); gs != "<nil>" {
			t.Errorf("nilObj.GoString() = %q, want %q", gs, "<nil>")
		}
	})

	t.Run("StringRedacted directly on a nil *SFSArray", func(t *testing.T) {
		var nilArr *SFSArray
		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked being called directly on a nil *SFSArray: %v", r)
				}
			}()
			got = nilArr.StringRedacted()
		}()
		if got != "<nil>" {
			t.Errorf("nilArr.StringRedacted() = %q, want %q", got, "<nil>")
		}
		if s := nilArr.String(); s != "<nil>" {
			t.Errorf("nilArr.String() = %q, want %q", s, "<nil>")
		}
		if gs := nilArr.GoString(); gs != "<nil>" {
			t.Errorf("nilArr.GoString() = %q, want %q", gs, "<nil>")
		}
	})
}

// TestDecodeLargeByteArrayFieldNotChargedAgainstMaxDecodedNodes is the round-14 regression test for
// Fix 3: sfsByteArray's decode case used to call chargeNodes(int(n)) for the raw byte count,
// treating every decoded byte as a separate "node" toward the flat maxDecodedNodes(300_000) budget
// -- making ~293,000 bytes a hard ceiling on any single legitimate byte-array field, even though a
// Go []byte's memory cost is already a tight ~1:1 ratio with its wire cost (no per-element
// allocation overhead the way e.g. []string has), so maxFrameSize's existing 64MiB wire-size cap
// already bounds it with no amplification risk. This test decodes a single 1MiB sfsByteArray field
// (comfortably over the old ~293,000-byte ceiling, comfortably under maxFrameSize) and confirms it
// no longer spuriously fails.
func TestDecodeLargeByteArrayFieldNotChargedAgainstMaxDecodedNodes(t *testing.T) {
	const size = 1 << 20 // 1 MiB

	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i)
	}

	o := NewSFSObject()
	o.put("payload", SFSValue{sfsByteArray, data})

	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}

	decoded, err := DecodeObject(encoded)
	if err != nil {
		t.Fatalf("DecodeObject of a legitimate %d-byte single sfsByteArray field failed: %v -- this field "+
			"must not be charged per-byte against maxDecodedNodes (see sfsobject.go's sfsByteArray decode "+
			"case)", size, err)
	}

	got, ok := decoded.Get("payload")
	if !ok {
		t.Fatal("decoded object is missing the payload field")
	}
	gotBytes, ok := got.Val.([]byte)
	if !ok {
		t.Fatalf("payload field has the wrong type: %T, want []byte", got.Val)
	}
	if len(gotBytes) != size {
		t.Fatalf("decoded byte array length = %d, want %d", len(gotBytes), size)
	}
	for i := range gotBytes {
		if gotBytes[i] != data[i] {
			t.Fatalf("decoded byte array content mismatch at index %d: got %d, want %d", i, gotBytes[i], data[i])
		}
	}
}
