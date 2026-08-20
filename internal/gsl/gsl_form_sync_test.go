package gsl

import (
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestEncodeFormSortedOrderMatchesGetServerListFields is a regression test against the drift
// encodeFormSorted's `order` whitelist is exposed to: GetServerList's form.Set(...) calls and
// encodeFormSorted's `order` slice are two hand-maintained lists of field names with nothing
// enforcing they stay in sync. If GetServerList ever grows a new form.Set("newField", ...) call
// without a matching entry added to `order`, encodeFormSorted silently omits that field from the
// outgoing request body -- no error, no log (see encodeFormSorted's doc comment in gsl.go, which
// documents exactly this class of drift-caused production bug).
//
// A test that only inspects the request GetServerList actually sends over the wire cannot catch
// this: a field encodeFormSorted drops is, by definition, a field that never makes it onto the
// wire, so comparing wire content against `order` would trivially "pass" even after a real drift
// -- the wire content is filtered *through* `order`, so the two always agree by construction.
//
// Instead, this reads gsl.go's own source -- the same technique
// TestCrossServerFlagNamesMatchesDeclarations (main_flags_test.go) uses against main.go -- rather
// than hand-duplicating a third list here, since a hand-duplicated list would reproduce the exact
// drift risk this test exists to catch. It extracts every form.Set("key", ...) call from
// GetServerList's body (real ground truth for what fields the function actually sets) and every
// field name in encodeFormSorted's `order` slice literal, then asserts the two sets match
// exactly in both directions.
func TestEncodeFormSortedOrderMatchesGetServerListFields(t *testing.T) {
	src, err := os.ReadFile("gsl.go")
	if err != nil {
		t.Fatalf("read gsl.go: %v", err)
	}

	// Isolate GetServerList's body. It's the only top-level func in gsl.go whose text contains a
	// line that is exactly "}" (an unindented closer) before the next top-level "func ", so a
	// non-greedy match up to the first "\n}\n" lands on its real closing brace.
	funcRe := regexp.MustCompile(`(?s)func GetServerList\(.*?\n\}\n`)
	body := funcRe.FindString(string(src))
	if body == "" {
		t.Fatal("could not find GetServerList's body in gsl.go -- the regexp is likely out of sync with how the function is written there")
	}

	// Only match calls on the "form" receiver -- GetServerList also builds a separate postBody
	// url.Values for the outer {uuid, data} POST fields, which is unrelated to encodeFormSorted's
	// whitelist and must not be conflated with it.
	setRe := regexp.MustCompile(`form\.Set\("([a-zA-Z0-9]+)"`)
	setMatches := setRe.FindAllStringSubmatch(body, -1)
	if len(setMatches) == 0 {
		t.Fatal("found zero form.Set(...) calls in GetServerList -- the regexp is likely out of sync with how the form is built there")
	}
	setFields := make(map[string]bool, len(setMatches))
	for _, m := range setMatches {
		setFields[m[1]] = true
	}

	orderRe := regexp.MustCompile(`(?s)order\s*:=\s*\[\]string\{(.*?)\}`)
	orderMatch := orderRe.FindStringSubmatch(string(src))
	if orderMatch == nil {
		t.Fatal("could not find encodeFormSorted's `order` slice literal in gsl.go -- the regexp is likely out of sync with how it's declared there")
	}
	nameRe := regexp.MustCompile(`"([a-zA-Z0-9]+)"`)
	orderNames := nameRe.FindAllStringSubmatch(orderMatch[1], -1)
	if len(orderNames) == 0 {
		t.Fatal("found zero field names inside encodeFormSorted's `order` slice literal -- the regexp is likely out of sync with how it's declared there")
	}
	orderFields := make(map[string]bool, len(orderNames))
	for _, m := range orderNames {
		orderFields[m[1]] = true
	}

	for name := range setFields {
		if !orderFields[name] {
			t.Errorf("GetServerList calls form.Set(%q, ...) but encodeFormSorted's `order` whitelist does not include %q -- add it there, or the field is silently dropped from the outgoing GSL request with no error and no log", name, name)
		}
	}
	for name := range orderFields {
		if !setFields[name] {
			t.Errorf("encodeFormSorted's `order` whitelist includes %q but GetServerList never calls form.Set(%q, ...) -- remove it, or check whether the field was renamed in GetServerList", name, name)
		}
	}
}

// TestEncodeFormSortedRejectsUnknownFormKey is the runtime counterpart to
// TestEncodeFormSortedOrderMatchesGetServerListFields above. That test only catches drift between
// `order` and GetServerList's form.Set(...) calls as they exist in gsl.go's source right now --
// it can't protect against a stray key reaching encodeFormSorted some other way (a future caller
// that isn't GetServerList, a typo'd field name added without a matching source-regexp update,
// etc). This test proves encodeFormSorted itself refuses to silently drop a field it doesn't
// recognize: any key in the form url.Values that `order` doesn't cover must produce an error, not
// a silently-shorter request body (see encodeFormSorted's doc comment in gsl.go for the
// silent-field-drop precedent this guards against).
func TestEncodeFormSortedRejectsUnknownFormKey(t *testing.T) {
	form := url.Values{}
	form.Set("uuid", "test-uuid")
	form.Set("totallyUnknownField", "some-value")

	got, err := encodeFormSorted(form)
	if err == nil {
		t.Fatalf("encodeFormSorted: expected an error for a form key absent from `order`, got nil (encoded = %q)", got)
	}
}

// TestEncodeFormSortedRejectsAmpersandInValue is the round-33 regression test for encodeFormSorted's
// `&`-injection guard, which had zero prior test coverage -- confirmed by round 33's own audit via
// mutation testing (temporarily disabling the `strings.Contains(v[0], "&")` check in gsl.go left the
// full `go test ./...` suite passing with zero failures, proving nothing exercised this branch).
//
// encodeFormSorted hand-builds the request body with a bare "&"-joined string.Builder, not
// url.Values.Encode() (which would percent-escape "&" in a value automatically) -- so an
// unescaped "&" inside a field's own value would inject a bogus extra key=value pair into the
// outgoing GSL request, corrupting it in a way the receiving server would parse as attacker-
// controlled additional fields. This proves the guard actually fires instead of silently writing
// the corrupting value through.
func TestEncodeFormSortedRejectsAmpersandInValue(t *testing.T) {
	form := url.Values{}
	form.Set("uuid", "malicious&injected=value")

	got, err := encodeFormSorted(form)
	if err == nil {
		t.Fatalf("encodeFormSorted: expected an error for a value containing '&', got nil (encoded = %q)", got)
	}
	if !strings.Contains(err.Error(), "uuid") {
		t.Errorf("encodeFormSorted error = %q, want it to name the offending field (%q)", err.Error(), "uuid")
	}
	if !strings.Contains(err.Error(), "&") {
		t.Errorf("encodeFormSorted error = %q, want it to mention the offending character ('&')", err.Error())
	}
}

// TestLoginServerListResponShadowStructFieldsMatch is the round-52 regression test for the MINOR
// finding that LoginServerListRespon.UnmarshalJSON's shadow struct (gsl.go, the anonymous "raw"
// struct used to pre-inspect serverList/loginServer/at/rt before shape-tolerant decoding) had zero
// automated protection against drifting out of sync with the real LoginServerListRespon field
// list -- UnmarshalJSON's own doc comment already warns a future field added to one and not the
// other "will compile but silently stop round-tripping through this custom decoder", the identical
// risk class TestEncodeFormSortedOrderMatchesGetServerListFields above already protects for this
// same file's encodeFormSorted/GetServerList pair, just never extended to this second
// hand-maintained-list-pair.
//
// Mirrors that test's technique exactly: reads gsl.go's own source and extracts every json tag
// name from both struct literals, rather than hand-duplicating a third list here (which would
// reproduce the exact drift risk this test exists to catch).
func TestLoginServerListResponShadowStructFieldsMatch(t *testing.T) {
	src, err := os.ReadFile("gsl.go")
	if err != nil {
		t.Fatalf("read gsl.go: %v", err)
	}
	source := string(src)

	tagRe := regexp.MustCompile(`json:"([a-zA-Z]+)"`)

	realRe := regexp.MustCompile(`(?s)type LoginServerListRespon struct \{(.*?)\n\}\n`)
	realMatch := realRe.FindStringSubmatch(source)
	if realMatch == nil {
		t.Fatal("could not find LoginServerListRespon's struct literal in gsl.go -- the regexp is likely out of sync with how it's declared there")
	}
	realFields := make(map[string]bool)
	for _, m := range tagRe.FindAllStringSubmatch(realMatch[1], -1) {
		realFields[m[1]] = true
	}
	if len(realFields) == 0 {
		t.Fatal("found zero json-tagged fields in LoginServerListRespon -- the regexp is likely out of sync with how it's declared there")
	}

	shadowRe := regexp.MustCompile(`(?s)var raw struct \{(.*?)\n\t\}\n`)
	shadowMatch := shadowRe.FindStringSubmatch(source)
	if shadowMatch == nil {
		t.Fatal("could not find UnmarshalJSON's shadow \"raw\" struct literal in gsl.go -- the regexp is likely out of sync with how it's declared there")
	}
	shadowFields := make(map[string]bool)
	for _, m := range tagRe.FindAllStringSubmatch(shadowMatch[1], -1) {
		shadowFields[m[1]] = true
	}
	if len(shadowFields) == 0 {
		t.Fatal("found zero json-tagged fields in UnmarshalJSON's shadow \"raw\" struct -- the regexp is likely out of sync with how it's declared there")
	}

	for name := range realFields {
		if !shadowFields[name] {
			t.Errorf("LoginServerListRespon has json tag %q but UnmarshalJSON's shadow \"raw\" struct does not -- add it there, or that field will silently stop round-tripping through the custom decoder", name)
		}
	}
	for name := range shadowFields {
		if !realFields[name] {
			t.Errorf("UnmarshalJSON's shadow \"raw\" struct has json tag %q but LoginServerListRespon does not -- remove it, or check whether the field was renamed", name)
		}
	}
}
