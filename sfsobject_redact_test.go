package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
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
	o.PutUtfString("nickname", "player-one")
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
	o.PutUtfString("nickname", "player-one")

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
	strObj.PutUtfString("nickname", "player-one")

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
			o.PutUtfString("nickname", "player-one")

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

// TestStringRedactedFormatsAllPrimitiveArrayTypesForNonSensitiveFields is the round-34 regression
// test for the MINOR finding: primitiveArrayPrefix (sfsobject.go), which formats a primitive
// array's actual elements for a NON-sensitive field (formatSFSValueRedacted's default case calls it
// directly, unlike redactSFSValue's sensitive-key masking path, which only ever calls the separate
// primitiveArrayLen for a bare count), had 7 of its 8 type-switch cases with zero test coverage --
// confirmed via mutation testing (deleting the []byte case outright still left the full suite
// passing). TestStringRedactedMasksAllPrimitiveArrayTypes above exercises all 8 wire types too, but
// only ever under a sensitive "loginKey" field, which never reaches primitiveArrayPrefix at all.
// This mirrors that table but puts each array under an ordinary, non-sensitive field name instead,
// and asserts the real formatted elements actually appear in the output -- proving each type-switch
// case does its job instead of silently falling through to the function's empty-string default.
func TestStringRedactedFormatsAllPrimitiveArrayTypesForNonSensitiveFields(t *testing.T) {
	cases := []struct {
		name     string
		sfsType  byte
		val      interface{}
		wantSubs []string // substrings that MUST appear in the (non-redacted) output
	}{
		{"BoolArray", sfsBoolArray, []bool{true, false, true}, []string{"true", "false"}},
		{"ByteArray", sfsByteArray, []byte{0xDE, 0xAD, 0xBE, 0xEF}, []string{"222", "173", "190", "239"}},
		{"ShortArray", sfsShortArray, []int16{-12345, 6789}, []string{"-12345", "6789"}},
		{"IntArray", sfsIntArray, []int32{918273645, 192837465}, []string{"918273645", "192837465"}},
		{"LongArray", sfsLongArray, []int64{1234567890123, 9876543210987}, []string{"1234567890123", "9876543210987"}},
		{"FloatArray", sfsFloatArray, []float32{3.14159, 2.71828}, []string{"3.14159", "2.71828"}},
		{"DoubleArray", sfsDoubleArray, []float64{1.6180339887, 1.4142135623}, []string{"1.618033", "1.414213"}},
		{"StringArray", sfsUtfStringArray, []string{"plain-item-alpha", "plain-item-beta"}, []string{"plain-item-alpha", "plain-item-beta"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewSFSObject()
			o.put("scoreHistory", SFSValue{c.sfsType, c.val}) // an ordinary, non-sensitive field name

			got := o.StringRedacted()

			for _, sub := range c.wantSubs {
				if !strings.Contains(got, sub) {
					t.Errorf("StringRedacted() on a non-sensitive %s field is missing expected formatted element %q -- want primitiveArrayPrefix's %s case to actually format it, got: %s", c.name, sub, c.name, got)
				}
			}
			if strings.Contains(got, "REDACTED") {
				t.Errorf("StringRedacted() should not mask a non-sensitive %s array, got: %s", c.name, got)
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
// method used to be the raw, unredacted dump (renamed unsafeRawString() in round 14, then deleted
// entirely as dead code in round 15 -- see sfsobject.go), so ANY code path that handed a
// *SFSObject to fmt's %v/%s verbs, a Print-family function, or slog's
// Any-kind attribute formatting would automatically invoke it via fmt.Stringer -- with zero literal
// ".String()" text in the source, a pattern credential_leak_lint_test.go's text-scanning approach
// structurally cannot see. This test exercises exactly that implicit-invocation path (never calling
// .String()/.StringRedacted() explicitly) and confirms it never leaks a secret, proving Fix 1
// actually closes the gap rather than merely relying on every call site remembering to opt in.
func TestFmtVerbAutoInvokesStringerSafely(t *testing.T) {
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-via-stringer-1234567890"

	o := NewSFSObject()
	o.PutUtfString("loginKey", secretLoginKey)
	o.PutUtfString("nickname", "player-one")

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
	o.PutUtfString("nickname", "player-one")

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
	o.PutUtfString("nickname", "player-one")

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
	o.PutUtfString("nickname", "player-one")

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
		o.PutUtfString("nickname", "player-one")

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
		o.PutUtfString("nickname", "player-one")

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

// TestRedactSFSValueMasksScalarTypesUnderSensitiveKey is the round-16 regression test for Fix 1a:
// redactSFSValue's fallback for any non-string, non-array value under a sensitive key used to be
// formatSFSValueRedacted(v) -- the ordinary, NON-redacting recursive formatter, whose own default
// case is the naive fmt.Sprintf("%v", val). This meant a sensitive key's value reached via
// PutInt/PutLong/PutBool/PutDouble/PutByte/PutShort (any scalar Go type other than string) printed
// in full cleartext. Proven live-reachable via interactive.go's putJSONValue, which routes any bare
// JSON number param to PutLong with no key-name restriction -- an operator typing
// `account.login.new {"verifyCode": 837291}` used to leak 837291 in cleartext through
// handleInteractiveLine's params.StringRedacted() logging. redactSFSValue's fallback is now the
// fixed "[REDACTED]" placeholder, so every scalar type is masked regardless of shape.
func TestRedactSFSValueMasksScalarTypesUnderSensitiveKey(t *testing.T) {
	cases := []struct {
		name string
		put  func(o *SFSObject, key string)
		want string // substring that must NOT appear raw in the output
	}{
		{"PutInt", func(o *SFSObject, key string) { o.PutInt(key, 725310) }, "725310"},
		{"PutLong", func(o *SFSObject, key string) { o.PutLong(key, 88613579246) }, "88613579246"},
		{"PutDouble", func(o *SFSObject, key string) { o.PutDouble(key, 90210.5) }, "90210.5"},
		{"PutByte", func(o *SFSObject, key string) { o.PutByte(key, 0xAB) }, "171"},
		{"PutShort", func(o *SFSObject, key string) { o.PutShort(key, -12321) }, "-12321"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewSFSObject()
			c.put(o, "verifyCode")
			o.PutUtfString("nickname", "player-one")

			got := o.StringRedacted()

			if strings.Contains(got, c.want) {
				t.Errorf("StringRedacted leaks a %s scalar value under a sensitive key in cleartext (found %q): %s", c.name, c.want, got)
			}
			if !strings.Contains(got, "verifyCode=[REDACTED]") {
				t.Errorf("StringRedacted should mask a %s scalar value under a sensitive key via the fail-closed [REDACTED] fallback, got: %s", c.name, got)
			}
			if !strings.Contains(got, "player-one") {
				t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
			}
		})
	}

	// PutBool is checked separately: "true"/"false" are too generic to safely substring-search for
	// accidental leakage, so this instead asserts the exact masked shape directly.
	t.Run("PutBool", func(t *testing.T) {
		o := NewSFSObject()
		o.PutBool("verifyCode", true)
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()
		if !strings.Contains(got, "verifyCode=[REDACTED]") {
			t.Errorf("StringRedacted should mask a PutBool scalar value under a sensitive key via the fail-closed [REDACTED] fallback, got: %s", got)
		}
	})

	// sfsFloat (bare float32) and sfsNull (nil) are checked separately from the table-driven cases
	// above: neither has a PutFloat/PutNull helper anywhere in this codebase, so unlike
	// PutInt/PutLong/PutDouble/PutByte/PutShort above, they're only reachable by constructing an
	// SFSValue directly -- e.g. a value decoded off the wire via readValuePayload's sfsFloat/sfsNull
	// cases -- which same-package tests can already do via the unexported `put` method. The code
	// already handles both correctly (redactSFSValue's fail-closed fallback covers any shape it
	// doesn't explicitly recognize as safe), but neither shape had a regression test proving it.
	t.Run("sfsFloat (bare float32, only reachable via decode)", func(t *testing.T) {
		o := NewSFSObject()
		o.put("verifyCode", SFSValue{sfsFloat, float32(90210.5)})
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()
		if strings.Contains(got, "90210.5") {
			t.Errorf("StringRedacted leaks a bare float32 value under a sensitive key in cleartext: %s", got)
		}
		if !strings.Contains(got, "verifyCode=[REDACTED]") {
			t.Errorf("StringRedacted should mask a bare float32 value under a sensitive key via the fail-closed [REDACTED] fallback, got: %s", got)
		}
		if !strings.Contains(got, "player-one") {
			t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
		}
	})

	t.Run("sfsNull (nil, only reachable via decode)", func(t *testing.T) {
		o := NewSFSObject()
		o.put("verifyCode", SFSValue{sfsNull, nil})
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()
		if !strings.Contains(got, "verifyCode=[REDACTED]") {
			t.Errorf("StringRedacted should mask a nil value under a sensitive key via the fail-closed [REDACTED] fallback, got: %s", got)
		}
		if !strings.Contains(got, "player-one") {
			t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
		}
	})
}

// TestStringRedactedMasksCaseVariantSensitiveKeys is the round-17 regression test for Fix 1:
// StringRedacted's sensitiveSFSKeys lookup used to be an exact-case Go map lookup with no
// case-folding. interactive.go's putJSONValue takes a JSON object key from the operator's
// control-FIFO line verbatim, with no case normalization, so a casing variant of a known-sensitive
// key (e.g. an operator typing "LoginKey" instead of the registered "loginKey") bypassed
// redactSFSValue entirely and fell through to formatSFSValueRedacted's plain
// fmt.Sprintf("%v", val) -- printing a secret typed under a mis-cased key in full cleartext in
// local logs. isSensitiveSFSKey (sfsobject.go) now compares case-insensitively instead.
func TestStringRedactedMasksCaseVariantSensitiveKeys(t *testing.T) {
	const secretLoginKey = "secret-case-variant-loginkey-must-not-leak-abcdef123456"

	cases := []string{"LoginKey", "LOGINKEY", "loginkey", "lOgInKeY"}
	for _, key := range cases {
		t.Run(key, func(t *testing.T) {
			o := NewSFSObject()
			o.PutUtfString(key, secretLoginKey)
			o.PutUtfString("nickname", "player-one")

			got := o.StringRedacted()
			if strings.Contains(got, secretLoginKey) {
				t.Errorf("StringRedacted leaks a secret under the case-variant key %q (registered as \"loginKey\") in cleartext: %s", key, got)
			}
			if !strings.Contains(got, "player-one") {
				t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
			}
		})
	}
}

// TestRedactSFSValueMasksNestedSFSObjectUnderSensitiveKey is the round-16 regression test for Fix
// 1b: redactSFSValue's fallback for a nested *SFSObject value under a sensitive key used to
// delegate to formatSFSValueRedacted(v), whose own *SFSObject case calls the NESTED object's OWN
// StringRedacted() -- which only re-checks the NESTED object's OWN key names against
// sensitiveSFSKeys, completely losing the fact that the OUTER key was already known-sensitive. A
// secret sitting under an ordinary-looking sub-key name inside that nested object (e.g.
// {loginKey: {value: "the-real-secret"}}) used to print in full. redactSFSValue now has an
// explicit *SFSObject case that blanket-masks by field count instead, mirroring the *SFSArray
// case's style.
func TestRedactSFSValueMasksNestedSFSObjectUnderSensitiveKey(t *testing.T) {
	const secretValue = "the-real-secret-nested-under-an-ordinary-subkey-must-not-leak"

	inner := NewSFSObject()
	inner.PutUtfString("value", secretValue) // "value" is not itself a sensitive key name

	o := NewSFSObject()
	o.PutSFSObject("loginKey", inner)
	o.PutUtfString("nickname", "player-one")

	got := o.StringRedacted()

	if strings.Contains(got, secretValue) {
		t.Errorf("StringRedacted leaks a secret nested inside a *SFSObject under a sensitive key, via an ordinary-looking sub-key name: %s", got)
	}
	if !strings.Contains(got, "loginKey=[REDACTED 1 fields]") {
		t.Errorf("StringRedacted should blanket-mask a nested *SFSObject under a sensitive key by field count, got: %s", got)
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestRedactSFSValueNilPointerUnderSensitiveKeyDoesNotPanic is the round-16 regression test for
// Fix 2 (and the *SFSObject analog added alongside Fix 1a): redactSFSValue's existing *SFSArray
// case had no nil check -- PutSFSArray(sensitiveKey, (*SFSArray)(nil)) followed by
// StringRedacted() panicked with a nil pointer dereference, since the type assertion succeeds
// (ok=true) for a nil pointer of the right dynamic type, and then `arr.items` dereferences it.
// The new *SFSObject case added by Fix 1a is checked for the same class of bug too.
func TestRedactSFSValueNilPointerUnderSensitiveKeyDoesNotPanic(t *testing.T) {
	t.Run("nil *SFSArray under a sensitive key", func(t *testing.T) {
		var nilArr *SFSArray
		o := NewSFSObject()
		o.PutSFSArray("loginKey", nilArr)
		o.PutUtfString("nickname", "player-one")

		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked on a nil *SFSArray under a sensitive key: %v", r)
				}
			}()
			got = o.StringRedacted()
		}()
		if !strings.Contains(got, "loginKey=<nil>") {
			t.Errorf("StringRedacted() on a nil *SFSArray under a sensitive key = %q, want it to contain \"loginKey=<nil>\"", got)
		}
	})

	t.Run("nil *SFSObject under a sensitive key", func(t *testing.T) {
		var nilObj *SFSObject
		o := NewSFSObject()
		o.PutSFSObject("loginKey", nilObj)
		o.PutUtfString("nickname", "player-one")

		var got string
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("StringRedacted panicked on a nil *SFSObject under a sensitive key: %v", r)
				}
			}()
			got = o.StringRedacted()
		}()
		if !strings.Contains(got, "loginKey=<nil>") {
			t.Errorf("StringRedacted() on a nil *SFSObject under a sensitive key = %q, want it to contain \"loginKey=<nil>\"", got)
		}
	})
}

// TestStringRedactedMasksFix4SensitiveKeys is the round-16 regression test for Fix 4:
// sensitiveSFSKeys was missing gcmRegisterId/parseRegisterId (push-notification device tokens,
// same actionable-device-targeting risk class as firebaseId), googleName (a real person's Google
// account display name -- more directly PII than a device token), mt (undocumented meaning, same
// field cluster per docs/live-validation.mdx), and simOp/simOpName/phone_model/osVersion/
// phone_screen/phone_native_screen (the rest of the device/carrier-identifier cluster) -- all
// confirmed real fields identity.go's BuildLoginParams constructs. None of these leak from this Go
// client's own placeholder traffic today, but decode.go's -decode-stream tool would print real
// values for these fields in cleartext when decoding a genuinely captured non-Go-client login.
func TestStringRedactedMasksFix4SensitiveKeys(t *testing.T) {
	secrets := map[string]string{
		"gcmRegisterId":       "secret-gcmregisterid-must-not-leak-abc123",
		"parseRegisterId":     "secret-parseregisterid-must-not-leak-def456",
		"googleName":          "Secret Real Person Name Must Not Leak",
		"mt":                  "secret-mt-must-not-leak-ghi789",
		"simOp":               "secret-simop-must-not-leak-jkl012",
		"simOpName":           "secret-simopname-must-not-leak-mno345",
		"phone_model":         "secret-phonemodel-must-not-leak-pqr678",
		"osVersion":           "secret-osversion-must-not-leak-stu901",
		"phone_screen":        "secret-phonescreen-must-not-leak-vwx234",
		"phone_native_screen": "secret-phonenativescreen-must-not-leak-yz567",
	}

	o := NewSFSObject()
	for key, val := range secrets {
		o.PutUtfString(key, val)
	}
	o.PutUtfString("nickname", "player-one")

	got := o.StringRedacted()

	for key, secret := range secrets {
		if strings.Contains(got, secret) {
			t.Errorf("StringRedacted leaks %q's value in cleartext: %s", key, got)
		}
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestEncodeObjectNilNestedValueReturnsErrorNotPanic is the round-16 regression test for Fix 3,
// mirroring round 15's TestNilNestedValueDoesNotPanic (the decode/format-side fix) but for the
// encode path: writeValuePayload's sfsObjectType case (`inner := v.Val.(*SFSObject)` then
// `len(inner.keys)`) and sfsArrayType case (`inner := v.Val.(*SFSArray)` then `len(inner.items)`)
// used to panic with a nil pointer dereference on PutSFSObject(key, nil)/PutSFSArray(key, nil) --
// the type assertion succeeds (ok=true) for a nil pointer of the right dynamic type, so
// `inner.keys`/`inner.items` dereferenced it. No current call site in this repo passes nil here
// (login.go/identity.go/crossserver.go/conn.go all confirmed via grep), so this was latent, not
// active -- but a real crash-on-future-mistake vector on the write path, mirroring what round 15
// already fixed on the read path. EncodeObject now returns a clean error instead of panicking.
func TestEncodeObjectNilNestedValueReturnsErrorNotPanic(t *testing.T) {
	t.Run("nil *SFSObject value", func(t *testing.T) {
		var nilObj *SFSObject
		o := NewSFSObject()
		o.PutSFSObject("child", nilObj)
		o.PutUtfString("nickname", "player-one")

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EncodeObject panicked on a nil *SFSObject value: %v", r)
				}
			}()
			_, err = EncodeObject(o)
		}()
		if err == nil {
			t.Fatal("EncodeObject with a nil *SFSObject value should return an error, got nil")
		}
	})

	t.Run("nil *SFSArray value", func(t *testing.T) {
		var nilArr *SFSArray
		o := NewSFSObject()
		o.PutSFSArray("child", nilArr)
		o.PutUtfString("nickname", "player-one")

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EncodeObject panicked on a nil *SFSArray value: %v", r)
				}
			}()
			_, err = EncodeObject(o)
		}()
		if err == nil {
			t.Fatal("EncodeObject with a nil *SFSArray value should return an error, got nil")
		}
	})

	t.Run("nil *SFSObject value nested two levels deep", func(t *testing.T) {
		var nilObj *SFSObject
		inner := NewSFSObject()
		inner.PutSFSObject("grandchild", nilObj)

		o := NewSFSObject()
		o.PutSFSObject("child", inner)

		var err error
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("EncodeObject panicked on a nil *SFSObject value nested two levels deep: %v", r)
				}
			}()
			_, err = EncodeObject(o)
		}()
		if err == nil {
			t.Fatal("EncodeObject with a nil *SFSObject value nested two levels deep should return an error, got nil")
		}
	})
}

// TestStringRedactedSanitizesTerminalEscapeSequences is the round-19 regression test for Fix 1
// (MAJOR): decoded server strings used to reach StringRedacted()'s output completely raw --
// formatSFSValueRedacted's default case was a bare fmt.Sprintf("%v", val) and redactSFSValue's
// string case returned redact(s) unmodified, neither stripping nor escaping control characters. A
// malicious server or crafted capture file could embed a raw ESC (0x1b)/BEL (0x07)-based ANSI
// terminal-title-injection sequence in any decoded string field, and it would reach the operator's
// screen unescaped the moment that value flowed into decode.go's -decode-stream tool or
// buildings.go's PrintBuildings (both plain fmt.Printf sinks with no escaping of their own).
// sanitizeForTerminal (sfsobject.go) now escapes every C0 control byte other than newline/tab
// (plus DEL) as a visible "\xHH" sequence at every leaf point a raw string reaches
// StringRedacted()'s output: an ordinary field's value (formatSFSValueRedacted's default case), a
// sensitive field's redact()-shortened value (redactSFSValue), and a decoded field's KEY name
// itself (StringRedacted()'s own loop).
func TestStringRedactedSanitizesTerminalEscapeSequences(t *testing.T) {
	// A classic xterm OSC-0 "set window title" injection, terminated by BEL -- the same
	// ESC(0x1b)]0;...BEL(0x07) shape a real terminal-spoofing attack would use to overwrite the
	// operator's window/tab title with fake text.
	const titleInjection = "\x1b]0;PWNED-BY-SERVER\x07"
	// A CSI erase/cursor/color sequence -- ESC(0x1b) followed by '[' -- used for e.g. faking error
	// text at an arbitrary screen position or wiping what's already on screen.
	const csiInjection = "\x1b[2J\x1b[H\x1b[31mFAKE ERROR: connection lost\x1b[0m"

	t.Run("ordinary non-sensitive field value", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("motd", "hello"+titleInjection+csiInjection+"world")
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()

		if strings.Contains(got, "\x1b") || strings.Contains(got, "\x07") {
			t.Errorf("StringRedacted lets a raw ESC/BEL byte through in an ordinary field's value: %q", got)
		}
		if !strings.Contains(got, "hello") || !strings.Contains(got, "world") {
			t.Errorf("sanitization must not eat the surrounding non-control text, got: %q", got)
		}
		if !strings.Contains(got, "player-one") {
			t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %q", got)
		}
	})

	t.Run("sensitive field value surviving redact()'s first4...last4 shortening", func(t *testing.T) {
		// redact() keeps the first 4 and last 4 bytes of a long string -- place the injection at
		// both ends so it survives into the shortened output regardless of exactly how redact()
		// slices it.
		secret := titleInjection + "middle-of-a-very-long-secret-value-padding-out-the-string" + titleInjection

		o := NewSFSObject()
		o.PutUtfString("loginKey", secret)
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()

		if strings.Contains(got, "\x1b") || strings.Contains(got, "\x07") {
			t.Errorf("StringRedacted lets a raw ESC/BEL byte through in a sensitive field's redacted value: %q", got)
		}
	})

	t.Run("field key name itself", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString(titleInjection+"evilkey", "some-value")
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()

		if strings.Contains(got, "\x1b") || strings.Contains(got, "\x07") {
			t.Errorf("StringRedacted lets a raw ESC/BEL byte through in a decoded field's KEY name: %q", got)
		}
	})

	t.Run("newline and tab stay readable", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("motd", "line one\nline two\tindented")
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()

		if !strings.Contains(got, "line one\nline two\tindented") {
			t.Errorf("sanitizeForTerminal should leave plain newline/tab bytes alone for readability, got: %q", got)
		}
	})

	// Round-33 regression: DEL (0x7f) is escaped by a separate clause in sanitizeForTerminal
	// (sfsobject.go) from the general C0-control-byte range (< 0x20) the other subtests above
	// exercise -- it sits outside that range entirely, so a regression dropping only the DEL
	// clause would not be caught by any of them.
	t.Run("DEL byte is escaped", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("motd", "before\x7fafter")
		o.PutUtfString("nickname", "player-one")

		got := o.StringRedacted()

		if strings.Contains(got, "\x7f") {
			t.Errorf("StringRedacted lets a raw DEL (0x7f) byte through: %q", got)
		}
		if !strings.Contains(got, `\x7f`) {
			t.Errorf("expected DEL to be escaped as the literal text \\x7f, got: %q", got)
		}
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Errorf("sanitization must not eat the surrounding non-control text, got: %q", got)
		}
	})
}

// TestEncodeObjectNilTopLevelReturnsErrorNotPanic is the round-19 regression test for Fix 2:
// EncodeObject(nil) used to panic with a nil pointer dereference on the `len(o.keys)` access
// inside its int16Count call, instead of returning a clean error -- inconsistent with this file's
// own nil-guard hardening on writeValuePayload's sfsObjectType/sfsArrayType cases (round 16, see
// TestEncodeObjectNilNestedValueReturnsErrorNotPanic above, which covers a nil value NESTED inside
// a valid parent object -- a different code path from the top-level `o` itself being nil) and on
// StringRedacted/formatSFSValueRedacted/redactSFSValue (rounds 15-16), all of which already handle
// a nil *SFSObject/*SFSArray gracefully. This test covers the top-level EncodeObject(nil) call,
// which none of the existing nil-related tests exercised.
func TestEncodeObjectNilTopLevelReturnsErrorNotPanic(t *testing.T) {
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("EncodeObject(nil) panicked instead of returning a clean error: %v", r)
			}
		}()
		_, err = EncodeObject(nil)
	}()
	if err == nil {
		t.Fatal("EncodeObject(nil) should return an error, got nil")
	}
}

// TestSFSValueStringAndGoStringRedactSecret is the round-19 regression test for Fix 3: SFSValue
// (the type Get() returns) had no String()/GoString() redaction guard, unlike *SFSObject/*SFSArray
// which got exactly this treatment in rounds 14-15. fmt.Sprintf("%v", someValue) on a raw SFSValue
// extracted via o.Get("loginKey") used to fall through to Go's default reflection-based struct
// formatter and print the real secret sitting in its exported Val field in full cleartext --
// currently latent (no call site in the repo formats a bare SFSValue this way today; every
// .Get() call type-asserts .Val or recurses into a nested object/array instead), matching the same
// "no live call site today, but an idiomatic future call would leak" shape that justified the
// SFSObject/SFSArray fixes in rounds 14-15. SFSValue.String()/GoString() now blanket-mask,
// mirroring bare *SFSArray.StringRedacted()'s own "no key context to lean on" reasoning.
func TestSFSValueStringAndGoStringRedactSecret(t *testing.T) {
	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-via-bare-sfsvalue-1234567890"

	o := NewSFSObject()
	o.PutUtfString("loginKey", secretLoginKey)

	v, ok := o.Get("loginKey")
	if !ok {
		t.Fatal(`Get("loginKey") returned ok=false`)
	}

	if got := fmt.Sprintf("%v", v); strings.Contains(got, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%v\", sfsValue) leaks a real secret via the default reflection-based struct formatter: %s", got)
	}
	if got := fmt.Sprintf("%s", v); strings.Contains(got, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%s\", sfsValue) leaks a real secret via implicit Stringer auto-invocation: %s", got)
	}
	if got := fmt.Sprintf("%#v", v); strings.Contains(got, secretLoginKey) {
		t.Errorf("fmt.Sprintf(\"%%#v\", sfsValue) leaks a real secret via the default reflection-based struct formatter: %s", got)
	}

	var buf bytes.Buffer
	fmt.Fprintln(&buf, v)
	if strings.Contains(buf.String(), secretLoginKey) {
		t.Errorf("fmt.Fprintln(w, sfsValue) leaks a real secret via implicit Stringer auto-invocation: %s", buf.String())
	}

	if got := v.String(); strings.Contains(got, secretLoginKey) {
		t.Errorf("SFSValue.String() leaks a real secret: %s", got)
	}
	if got := v.GoString(); strings.Contains(got, secretLoginKey) {
		t.Errorf("SFSValue.GoString() leaks a real secret: %s", got)
	}
}

// TestStringRedactedMasksRound28SensitiveKeys is the round-28 regression test for reclassifying
// "un" (the classic SFS2X username field -- the server's real returned account username, which
// login.go used to log in cleartext at Info level on every successful login) and "googlePlay"
// (part of the Google-identity field cluster identity.go's BuildLoginParams constructs alongside
// the already-recognized googleName/androidDid) as sensitive. Both used to sit in
// sfsobject_sensitive_keys_sync_test.go's knownNonSensitiveSFSKeys allowlist instead; see
// sensitiveSFSKeys' own doc comments on these two entries (sfsobject.go) for the full reasoning.
func TestStringRedactedMasksRound28SensitiveKeys(t *testing.T) {
	const secretUsername = "secret-real-account-username-must-not-leak"
	const secretGooglePlay = "secret-googleplay-value-must-not-leak"

	o := NewSFSObject()
	o.PutUtfString("un", secretUsername)
	o.PutUtfString("googlePlay", secretGooglePlay)
	o.PutUtfString("nickname", "ordinary-field-still-visible")

	got := o.StringRedacted()

	if strings.Contains(got, secretUsername) {
		t.Errorf("StringRedacted leaks the un (username) field in cleartext: %s", got)
	}
	if strings.Contains(got, secretGooglePlay) {
		t.Errorf("StringRedacted leaks the googlePlay field in cleartext: %s", got)
	}
	if !strings.Contains(got, "ordinary-field-still-visible") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestStringRedactedMasksGameUserName is the round-31 regression test for reclassifying
// "gameUserName" as sensitive: it's "un"'s exact sibling on the OTHER login path (login.go's
// push.account.login.new response, vs. "un" on the base-zone Login response) -- both carry the
// identical real-account-username value and are persisted via the same ident.SaveUsername() call,
// but only "un" was added to sensitiveSFSKeys in round 28. See sensitiveSFSKeys' own doc comment on
// "gameUserName" (sfsobject.go) for the full reasoning.
func TestStringRedactedMasksGameUserName(t *testing.T) {
	const secretUsername = "secret-real-game-username-must-not-leak"

	o := NewSFSObject()
	o.PutUtfString("gameUserName", secretUsername)
	o.PutUtfString("nickname", "ordinary-field-still-visible")

	got := o.StringRedacted()

	if strings.Contains(got, secretUsername) {
		t.Errorf("StringRedacted leaks the gameUserName field in cleartext: %s", got)
	}
	if !strings.Contains(got, "ordinary-field-still-visible") {
		t.Errorf("StringRedacted must not mask ordinary non-sensitive fields, got: %s", got)
	}
}

// TestStringRedactedFormatBudgetBoundsLargeArray is the round-28 regression test for the MAJOR
// format-time-budget finding: before this fix, StringRedacted()/formatSFSValueRedacted() had ZERO
// format-time cost bound of their own -- maxDecodedNodes only bounds DECODE-time cost for one wire
// payload, not a later format/log walk of an object that's already sitting in memory. That gap is
// most starkly real for an object built PROGRAMMATICALLY via Put*/Add*, as this test does (not via
// DecodeObject): maxDecodedNodes/chargeNodes only ever run inside DecodeObject's read path, so such
// an object had no size cap anywhere before this fix, regardless of how it's later formatted. This
// builds an *SFSArray with far more items than maxFormattedNodes and proves a single
// StringRedacted() call on it is now bounded in both output size and the number of items it
// actually walks, rather than scaling with the array's real size.
func TestStringRedactedFormatBudgetBoundsLargeArray(t *testing.T) {
	const itemCount = 200_000 // comfortably more than maxFormattedNodes (50_000)

	arr := NewSFSArray()
	for i := 0; i < itemCount; i++ {
		arr.AddInt(int32(i))
	}

	o := NewSFSObject()
	o.PutSFSArray("buildingList", arr) // an ordinary, non-sensitive field name

	got := o.StringRedacted()

	if !strings.Contains(got, formatTruncatedMarker) {
		t.Fatalf("StringRedacted() on a %d-item array did not truncate -- expected the visible %q marker in the output (%d bytes)", itemCount, formatTruncatedMarker, len(got))
	}
	// A generous ceiling on output size: each formatted int item is at most a handful of bytes
	// (e.g. ", 49998"), so maxFormattedNodes items plus object/array framing and the marker should
	// stay comfortably under this regardless of exactly how many digits the largest formatted
	// value takes -- the point is proving the output does NOT scale with itemCount, not pinning an
	// exact byte count.
	const maxReasonableOutputBytes = 10 * maxFormattedNodes
	if len(got) > maxReasonableOutputBytes {
		t.Errorf("StringRedacted() output is %d bytes, want at most %d -- a single call must not scale with the real item count (%d)", len(got), maxReasonableOutputBytes, itemCount)
	}
	// An item near the end of the array must NOT appear -- proving the walk itself stopped early,
	// not merely that the already-fully-formatted text got chopped off afterward.
	lateItem := fmt.Sprintf("%d", itemCount-1)
	if strings.Contains(got, lateItem) {
		t.Errorf("StringRedacted() output contains item %q from near the end of a %d-item array -- the format walk did not actually stop at the budget", lateItem, itemCount)
	}
	// The first item must still be present, confirming this isn't simply an empty/broken output.
	if !strings.Contains(got, "buildingList=[0,") {
		t.Errorf("StringRedacted() output is missing the first array item, want it present before truncation kicks in: %.200s", got)
	}
}

// TestStringRedactedFormatBudgetBoundsManyTopLevelKeys is
// TestStringRedactedFormatBudgetBoundsLargeArray's sibling for the OTHER loop the same fix bounds:
// stringRedactedBudgeted's own top-level key loop (SFSObject.StringRedacted()), not just
// formatSFSValueRedacted's array-item loop. A hand-built object with far more distinct keys than
// maxFormattedNodes -- again built via ordinary Put* calls, not decoded off the wire -- must
// truncate the same way.
func TestStringRedactedFormatBudgetBoundsManyTopLevelKeys(t *testing.T) {
	const keyCount = 200_000

	o := NewSFSObject()
	for i := 0; i < keyCount; i++ {
		o.PutInt(fmt.Sprintf("k%06d", i), int32(i))
	}

	got := o.StringRedacted()

	if !strings.Contains(got, formatTruncatedMarker) {
		t.Fatalf("StringRedacted() on a %d-key object did not truncate -- expected the visible %q marker in the output (%d bytes)", keyCount, formatTruncatedMarker, len(got))
	}
	const maxReasonableOutputBytes = 15 * maxFormattedNodes
	if len(got) > maxReasonableOutputBytes {
		t.Errorf("StringRedacted() output is %d bytes, want at most %d -- a single call must not scale with the real key count (%d)", len(got), maxReasonableOutputBytes, keyCount)
	}
	lateKey := fmt.Sprintf("k%06d", keyCount-1)
	if strings.Contains(got, lateKey) {
		t.Errorf("StringRedacted() output contains key %q from near the end of a %d-key object -- the format walk did not actually stop at the budget", lateKey, keyCount)
	}
	if !strings.Contains(got, "k000000=0") {
		t.Errorf("StringRedacted() output is missing the first key, want it present before truncation kicks in: %.200s", got)
	}
}

// TestStringRedactedFormatBudgetBoundsLargePrimitiveArrayField is the round-29 regression test for
// the MINOR finding: formatSFSValueRedacted's default case (handling a bare string/sfsText value and
// all 8 primitive-array types) used to charge only ONE formatBudget unit for the ENTIRE value
// regardless of its actual size (e.g. a 40,000-element string array was charged as 1 unit) -- so a
// single huge primitive-array-valued field was effectively EXEMPT from maxFormattedNodes, unlike a
// semantically-equivalent large *SFSArray (already proven bounded by
// TestStringRedactedFormatBudgetBoundsLargeArray above). This builds a single ordinary, non-sensitive
// field whose value is a primitive []int32 array (readValuePayload's sfsIntArray shape -- a plain
// unwrapped Go slice, NOT the *SFSArray wrapper type those other two tests exercise) far larger than
// maxFormattedNodes, and proves a single StringRedacted() call on it is now bounded the same way.
func TestStringRedactedFormatBudgetBoundsLargePrimitiveArrayField(t *testing.T) {
	const itemCount = 200_000 // comfortably more than maxFormattedNodes (50_000)

	arr := make([]int32, itemCount)
	for i := range arr {
		arr[i] = int32(i)
	}

	o := NewSFSObject()
	// "nickname" is written FIRST so it's guaranteed to be charged/formatted before the huge
	// primitive-array field below can exhaust the shared budget.
	o.PutUtfString("nickname", "player-one")
	o.put("scoreHistory", SFSValue{sfsIntArray, arr}) // an ordinary, non-sensitive field name

	got := o.StringRedacted()

	if !strings.Contains(got, formatTruncatedMarker) {
		t.Fatalf("StringRedacted() on a %d-element primitive-array field did not truncate -- expected the visible %q marker in the output (%d bytes); a large primitive array must not be exempt from maxFormattedNodes", itemCount, formatTruncatedMarker, len(got))
	}
	const maxReasonableOutputBytes = 10 * maxFormattedNodes
	if len(got) > maxReasonableOutputBytes {
		t.Errorf("StringRedacted() output is %d bytes, want at most %d -- a single call must not scale with the real element count (%d)", len(got), maxReasonableOutputBytes, itemCount)
	}
	lateItem := fmt.Sprintf("%d", itemCount-1)
	if strings.Contains(got, lateItem) {
		t.Errorf("StringRedacted() output contains element %q from near the end of a %d-element primitive array -- the format walk did not actually stop at the budget", lateItem, itemCount)
	}
	if !strings.Contains(got, "player-one") {
		t.Errorf("StringRedacted() output is missing the sibling \"nickname\" field, which was written before the huge array and should still be present: %.200s", got)
	}
	if !strings.Contains(got, "scoreHistory=[0") {
		t.Errorf("StringRedacted() output is missing the first array element, want it present before truncation kicks in: %.200s", got)
	}
}

// TestStringRedactedTruncationMarkerAppearsOnlyOnce is the round-29 regression test for the NIT
// finding: once the shared formatBudget is exhausted mid-recursion, EVERY still-in-progress
// enclosing nesting level used to independently re-check fb.charge() (still false) and append its
// own formatTruncatedMarker, so a single StringRedacted() output could contain multiple redundant
// truncation markers instead of just one. This builds a 3-level-deep fixture specifically so THREE
// separate loops each notice the exhausted budget in the same call:
//  1. an innermost *SFSArray with far more items than maxFormattedNodes (its own item loop notices
//     exhaustion first, and is the one that should actually append the marker);
//  2. an outer *SFSArray holding that huge array as its first item, plus a second item after it
//     (this outer array's own item loop notices exhaustion too, once it tries to charge for that
//     second item); and
//  3. the top-level SFSObject holding the outer array under one key, plus a second key after it
//     (the top-level key loop notices exhaustion too, once it tries to charge for that second key).
//
// Before the fix, all three would append formatTruncatedMarker independently. After the fix, only
// the first (innermost) one should.
func TestStringRedactedTruncationMarkerAppearsOnlyOnce(t *testing.T) {
	const innerCount = maxFormattedNodes + 10 // ample to exhaust the shared budget deep inside a nested array

	innerArr := NewSFSArray()
	for i := 0; i < innerCount; i++ {
		innerArr.AddInt(int32(i))
	}

	outerArr := NewSFSArray()
	outerArr.add(SFSValue{sfsArrayType, innerArr}) // item[0]: the huge nested array that exhausts the budget
	outerArr.AddInt(1)                             // item[1]: outerArr's own loop must notice exhaustion here

	o := NewSFSObject()
	o.PutSFSArray("a", outerArr) // key "a": consumes 1 unit, then recurses into outerArr's own loop
	o.PutInt("b", 2)             // key "b": the top-level key loop must notice exhaustion here too

	got := o.StringRedacted()

	markerCount := strings.Count(got, formatTruncatedMarker)
	if markerCount != 1 {
		t.Fatalf("StringRedacted() output contains %d occurrences of formatTruncatedMarker, want exactly 1 -- "+
			"every still-in-progress enclosing nesting level (the inner array's own item loop, outerArr's own "+
			"item loop, and the top-level key loop) independently notices the exhausted shared budget, but only "+
			"the FIRST one to notice should append the marker; got (truncated to 500 bytes): %.500s",
			markerCount, got)
	}
}

// TestStringRedactedFormatBudgetSharedAcrossNestingLevels is the round-29 regression test for the
// MINOR testing-rigor finding: the existing maxFormattedNodes tests
// (TestStringRedactedFormatBudgetBoundsLargeArray, TestStringRedactedFormatBudgetBoundsManyTopLevelKeys)
// only prove a single FLAT level truncates -- neither proves the budget is actually SHARED (not
// reset) across nested recursion, despite formatBudget's own doc comment explicitly stating it must
// not reset per nesting level (sfsobject.go). This builds a top-level object with a NESTED object
// holding maxFormattedNodes keys of its own, plus a SIBLING key placed after the nested object in
// insertion order, and proves:
//
//  1. the nested object's own key loop truncates PARTWAY THROUGH (with the marker), even though its
//     own key count alone is exactly maxFormattedNodes -- it only runs out because the outer
//     object's "nested" key itself already spent one unit of the SAME shared budget before
//     recursing in; and
//  2. the sibling key placed AFTER the nested object never appears in the output at all, because the
//     shared budget is already fully spent by the time the outer loop reaches it.
//
// If the budget were (incorrectly) reset to a fresh maxFormattedNodes at the start of the nested
// object's own stringRedactedBudgeted call -- the exact bug this test guards against -- neither of
// these would be true: the nested object would format all of its own keys with no truncation, and
// the sibling key after it would print normally too.
func TestStringRedactedFormatBudgetSharedAcrossNestingLevels(t *testing.T) {
	nested := NewSFSObject()
	for i := 0; i < maxFormattedNodes; i++ {
		nested.PutInt(fmt.Sprintf("n%06d", i), int32(i))
	}

	o := NewSFSObject()
	o.PutSFSObject("nested", nested) // consumes 1 unit of the shared budget before recursing in
	o.PutUtfString("afterNested", "should-not-appear-if-budget-is-shared-correctly")

	got := o.StringRedacted()

	if !strings.Contains(got, formatTruncatedMarker) {
		t.Fatalf("StringRedacted() with a %d-key nested object (consuming 1 shared-budget unit via its own "+
			"outer key first) did not truncate at all -- expected the shared budget to run out partway "+
			"through the nested object's own keys: %.300s", maxFormattedNodes, got)
	}
	// The nested object's very last key must NOT appear -- proving its own internal loop was cut
	// short by the shared (not freshly-reset) budget.
	lastNestedKey := fmt.Sprintf("n%06d", maxFormattedNodes-1)
	if strings.Contains(got, lastNestedKey) {
		t.Errorf("StringRedacted() output contains the nested object's last key %q -- the nested object's "+
			"own key loop was not actually bounded by the shared budget (it behaved as if it had its own "+
			"fresh %d-unit budget instead)", lastNestedKey, maxFormattedNodes)
	}
	// The sibling key placed AFTER the nested object in the OUTER object's own key list must not
	// appear either -- the shared budget is already exhausted formatting the nested object, so the
	// outer loop must never even reach this key.
	if strings.Contains(got, "afterNested") {
		t.Errorf("StringRedacted() output contains the sibling key \"afterNested\", which sits after the "+
			"nested object in insertion order -- the shared format budget must already be exhausted by the "+
			"time the outer loop reaches it if it is truly shared (not reset) across nesting levels, got: %.300s", got)
	}
	// The nested object's first key must still be present, confirming this isn't simply an
	// empty/broken output.
	if !strings.Contains(got, "n000000=0") {
		t.Errorf("StringRedacted() output is missing the nested object's first key, want it present before truncation kicks in: %.300s", got)
	}
}

// TestStringRedactedFormatBudgetBoundary is the round-29 boundary-condition regression test for
// maxFormattedNodes: every prior maxFormattedNodes test
// (TestStringRedactedFormatBudgetBoundsLargeArray, TestStringRedactedFormatBudgetBoundsManyTopLevelKeys)
// overshoots the cap by a wide margin (~4x), so neither would catch an off-by-one regression in
// formatBudget.charge()'s own boundary condition (`remaining <= 0`) -- the exact anti-pattern round
// 28 itself fixed for every OTHER raw-item cap in this codebase (see
// TestParseInitBuildingsRawItemCapBoundary, buildings_visitors_test.go, for the pattern this test
// mirrors). This drives both sides of the exactly-maxFormattedNodes boundary directly with
// well-formed top-level keys, so the PRESENCE/ABSENCE of specific keys in the output -- not just a
// truncation-marker count -- proves whether the cap fired at exactly the right point.
func TestStringRedactedFormatBudgetBoundary(t *testing.T) {
	buildObject := func(n int) *SFSObject {
		o := NewSFSObject()
		for i := 0; i < n; i++ {
			o.PutInt(fmt.Sprintf("k%06d", i), int32(i))
		}
		return o
	}

	t.Run("exactly cap keys: all formatted, no truncation marker", func(t *testing.T) {
		got := buildObject(maxFormattedNodes).StringRedacted()

		if strings.Contains(got, formatTruncatedMarker) {
			t.Errorf("unexpected truncation marker at exactly-cap boundary (%d keys): %.300s", maxFormattedNodes, got)
		}
		lastKey := fmt.Sprintf("k%06d=%d", maxFormattedNodes-1, maxFormattedNodes-1)
		if !strings.Contains(got, lastKey) {
			t.Errorf("StringRedacted() output is missing the last key %q at exactly-cap boundary (%d keys), want every key present: %.300s", lastKey, maxFormattedNodes, got)
		}
	})

	t.Run("cap+1 keys: truncation marker fires, only cap keys formatted", func(t *testing.T) {
		got := buildObject(maxFormattedNodes + 1).StringRedacted()

		if !strings.Contains(got, formatTruncatedMarker) {
			t.Fatalf("expected a truncation marker at cap+1 boundary (%d keys), got: %.300s", maxFormattedNodes+1, got)
		}
		lastFittingKey := fmt.Sprintf("k%06d=%d", maxFormattedNodes-1, maxFormattedNodes-1)
		if !strings.Contains(got, lastFittingKey) {
			t.Errorf("StringRedacted() output is missing key %q, which should still fit exactly at the boundary (cap+1 input must still format the first %d keys in full): %.300s", lastFittingKey, maxFormattedNodes, got)
		}
		droppedKey := fmt.Sprintf("k%06d", maxFormattedNodes)
		if strings.Contains(got, droppedKey) {
			t.Errorf("StringRedacted() output contains key %q, the one key past the cap -- it must be the first (and only) key dropped by truncation at the cap+1 boundary: %.300s", droppedKey, got)
		}
	})
}

// TestChargeUpToAlreadyExhaustedBudget is the round-49 regression test for the MINOR finding that
// chargeUpTo's already-exhausted-budget branch (`if fb.remaining <= 0 { return 0, true }`) had zero
// test coverage of any kind, confirmed via go tool cover (execution count 0). This branch is only
// reached when a PRIOR key/field in the same object has already fully exhausted the shared
// formatBudget via charge() before a later bare-string/primitive-array field reaches chargeUpTo --
// every existing StringRedacted()-level budget test either never calls chargeUpTo after exhaustion
// (int-only fields exhaust via charge(), which breaks the enclosing loop before reaching a later
// field) or calls chargeUpTo while remaining is still positive. Calls chargeUpTo directly on a
// formatBudget already driven to remaining==0, rather than relying on a specific StringRedacted()
// field ordering to reach it indirectly.
func TestChargeUpToAlreadyExhaustedBudget(t *testing.T) {
	fb := newFormatBudget()
	fb.remaining = 0

	allowed, ranOut := fb.chargeUpTo(5)

	if allowed != 0 {
		t.Errorf("allowed = %d, want 0 (an already-exhausted budget must not charge anything)", allowed)
	}
	if !ranOut {
		t.Error("ranOut = false, want true (an already-exhausted budget must report ranOut)")
	}
}

// TestStringRedactedBudgetedNilReceiver is the round-49 regression test for the MINOR finding that
// stringRedactedBudgeted's own `if o == nil` guard is currently unreachable dead code and has zero
// test coverage, confirmed via go tool cover (execution count 0). Both of its call sites --
// SFSObject.StringRedacted() and formatSFSValueRedacted's *SFSObject case -- already nil-check and
// return "<nil>" before ever calling stringRedactedBudgeted, so nesting a nil object inside an
// SFSValue/SFSArray (the existing TestNilNestedValueDoesNotPanic's technique) never reaches this
// guard either -- formatSFSValueRedacted's own nil check intercepts first. Calls the unexported
// method directly on a nil receiver to actually exercise it.
func TestStringRedactedBudgetedNilReceiver(t *testing.T) {
	var o *SFSObject
	got := o.stringRedactedBudgeted(newFormatBudget())
	if got != "<nil>" {
		t.Errorf("stringRedactedBudgeted on a nil receiver = %q, want %q", got, "<nil>")
	}
}

// TestStringRedactedTruncatesLargeStringAtRuneBoundary is the round-32 regression test for the
// MINOR finding that formatSFSValueRedacted's bare-string truncation path (round 29's proportional-
// budget-charging fix) sliced at a raw byte offset with no UTF-8 rune-boundary awareness, so a
// format-budget cutoff landing mid-rune of a multi-byte UTF-8 string emitted an invalid, truncated
// byte sequence into StringRedacted()'s output -- reachable via decode.go's -decode-stream tool,
// which prints StringRedacted()'s output through a raw, non-escaping fmt.Printf. login.go's
// redact() was already hardened for the identical byte-vs-rune bug shape; this sibling truncation
// path never was, until truncateAtRuneBoundary (sfsobject.go).
//
// Uses a 3-byte-per-rune CJK character (making a byte-boundary/rune-boundary mismatch highly
// likely for most cutoff points) and a field long enough to guarantee the shared format budget
// runs out mid-string, then asserts the output is valid UTF-8 throughout.
func TestStringRedactedTruncatesLargeStringAtRuneBoundary(t *testing.T) {
	const runeCount = maxFormattedNodes // 3 bytes/rune -- guarantees exceeding the byte budget
	longValue := strings.Repeat("中", runeCount)

	o := NewSFSObject()
	o.PutUtfString("nickname", longValue) // an ordinary, non-sensitive field name

	got := o.StringRedacted()

	if !strings.Contains(got, formatTruncatedMarker) {
		t.Fatalf("StringRedacted() on a %d-rune string did not truncate -- expected the visible %q marker in the output (%d bytes)", runeCount, formatTruncatedMarker, len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("StringRedacted() output is not valid UTF-8 -- the format-budget cutoff landed mid-rune of the multi-byte value:\n%q", got)
	}
	if strings.Contains(got, "�") {
		t.Errorf("StringRedacted() output contains the UTF-8 replacement character, suggesting a corrupted rune sequence:\n%q", got)
	}
}
