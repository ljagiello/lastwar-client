package main

import (
	"os"
	"regexp"
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
