package main

import (
	"bytes"
	"fmt"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestFindServerInfo(t *testing.T) {
	t.Run("nested under p", func(t *testing.T) {
		si := sfs.NewSFSObject()
		si.PutUtfString("ip", "1.2.3.4")
		p := sfs.NewSFSObject()
		p.PutSFSObject("serverInfo", si)
		content := sfs.NewSFSObject()
		content.PutSFSObject("p", p)
		got := gsl.FindServerInfo(content)
		if got == nil || got.GetString("ip") != "1.2.3.4" {
			t.Fatalf("expected nested serverInfo to be found, got %v", got)
		}
	})
	t.Run("top-level fallback", func(t *testing.T) {
		si := sfs.NewSFSObject()
		si.PutUtfString("ip", "5.6.7.8")
		content := sfs.NewSFSObject()
		content.PutSFSObject("serverInfo", si)
		got := gsl.FindServerInfo(content)
		if got == nil || got.GetString("ip") != "5.6.7.8" {
			t.Fatalf("expected top-level serverInfo to be found, got %v", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		content := sfs.NewSFSObject()
		if got := gsl.FindServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	t.Run("nil content", func(t *testing.T) {
		if got := gsl.FindServerInfo(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	// The four subtests below are the round-39 regression tests for gsl.FindServerInfo's
	// present-but-wrong-typed-vs-genuinely-absent diagnostic gap: three anomaly shapes must now
	// warn, and the fourth (p.serverInfo genuinely absent) must stay silent, exactly mirroring
	// login.go's redirectIP/redirectZone convention on the same object.
	t.Run("top-level serverInfo wrong-typed warns", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		content := sfs.NewSFSObject()
		content.PutUtfString("serverInfo", "not-an-object")
		if got := gsl.FindServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
		if logged := buf.String(); !strings.Contains(logged, "top-level serverInfo field is present but not an object") {
			t.Errorf("expected a warning about the wrong-typed top-level serverInfo field, got log:\n%s", logged)
		}
	})
	t.Run("p wrong-typed warns", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		content := sfs.NewSFSObject()
		content.PutUtfString("p", "not-an-object")
		if got := gsl.FindServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
		if logged := buf.String(); !strings.Contains(logged, "p field is present but not an object") {
			t.Errorf("expected a warning about the wrong-typed p field, got log:\n%s", logged)
		}
	})
	t.Run("p.serverInfo wrong-typed warns", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		p := sfs.NewSFSObject()
		p.PutUtfString("serverInfo", "not-an-object")
		content := sfs.NewSFSObject()
		content.PutSFSObject("p", p)
		if got := gsl.FindServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
		if logged := buf.String(); !strings.Contains(logged, "p.serverInfo field is present but not an object") {
			t.Errorf("expected a warning about the wrong-typed p.serverInfo field, got log:\n%s", logged)
		}
	})
	t.Run("p.serverInfo absent stays silent", func(t *testing.T) {
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		defer slog.SetDefault(orig)

		p := sfs.NewSFSObject()
		content := sfs.NewSFSObject()
		content.PutSFSObject("p", p)
		if got := gsl.FindServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
		if logged := buf.String(); logged != "" {
			t.Errorf("expected no warning for a genuinely absent p.serverInfo, got log:\n%s", logged)
		}
	})
}

func TestGetIntFlexible(t *testing.T) {
	t.Run("numeric field", func(t *testing.T) {
		o := sfs.NewSFSObject()
		o.PutInt("port", 25092)
		if got := getIntFlexible(o, "port"); got != 25092 {
			t.Fatalf("got %d, want 25092", got)
		}
	})
	t.Run("string-numeric field", func(t *testing.T) {
		o := sfs.NewSFSObject()
		o.PutUtfString("port", "17783")
		if got := getIntFlexible(o, "port"); got != 17783 {
			t.Fatalf("got %d, want 17783", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		o := sfs.NewSFSObject()
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
	t.Run("empty string", func(t *testing.T) {
		o := sfs.NewSFSObject()
		o.PutUtfString("port", "")
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
}

// TestGetIntFlexibleRejectsOutOfInt32RangeString is the round-30 regression test for the MAJOR
// finding: getIntFlexible's string-fallback path used to do a bare, unchecked int32(n) conversion
// on strconv.Atoi's result, reintroducing the exact int64-to-int32 unchecked-narrowing bug round 29
// fixed in sfsobject.go's GetInt. On a 64-bit Platform Go's int is 64-bit, so Atoi parses a numeric
// string outside int32's range without error, and the bare conversion used to silently wrap it
// (e.g. "4294967301" -> 5) instead of rejecting it -- a corrupted/hostile numeric-string port would
// then have sailed straight past buildBaseZoneLoginAddr's only guard (rejecting non-positive
// values) and silently redialed the wrong port. This proves that no longer happens: a
// string-encoded value comfortably outside int32's range must now come back as the documented
// zero-value fallback (the same fallback getIntFlexible already uses for an absent/empty field),
// not as a wrapped, corrupted int32.
func TestGetIntFlexibleRejectsOutOfInt32RangeString(t *testing.T) {
	tests := []struct {
		name string
		val  int64
	}{
		// 1<<32 + 5 wraps to 5 under naive int32(n) truncation -- picking a value whose wrapped
		// result would itself look like a plausible small int32 is the whole point: a test value
		// that wrapped to something already-implausible (e.g. still enormous) wouldn't actually
		// prove the old bug is fixed. Mirrors TestGetIntRejectsOutOfInt32RangeLong's own case
		// selection (sfsobject_array_test.go).
		{"just above MaxInt32, wraps to a small negative value under naive truncation", math.MaxInt32 + 1},
		{"far above MaxInt32 (1<<32 + 5 wraps to 5)", int64(1)<<32 + 5},
		{"just below MinInt32", math.MinInt32 - 1},
		{"far below MinInt32", -(int64(1) << 40) - 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := sfs.NewSFSObject()
			o.PutUtfString("port", strconv.FormatInt(tt.val, 10))

			got := getIntFlexible(o, "port")

			// The naive int32(n) conversion Go performs is the exact bug this test guards against --
			// computing it here (rather than hardcoding an expected wrapped value) keeps the test
			// resilient to exactly which wrapped value a given input produces, while still proving
			// getIntFlexible's real output is NOT that wrapped value.
			wrapped := int32(tt.val)
			if got == wrapped && wrapped != 0 {
				t.Errorf("getIntFlexible(%q) = %d, which is the silently-wrapped (int32(n)) value -- want the zero-value fallback (0) for an out-of-int32-range numeric string, not a wrapped/corrupted value", strconv.FormatInt(tt.val, 10), got)
			}
			if got != 0 {
				t.Errorf("getIntFlexible(%q) = %d, want 0 (the documented zero-value fallback for an out-of-int32-range numeric string)", strconv.FormatInt(tt.val, 10), got)
			}
		})
	}

	// Sanity/boundary check: string-encoded values that DO fit in int32's range must still
	// round-trip normally, proving this fix didn't accidentally over-tighten getIntFlexible for
	// legitimate in-range ports (including the exact MinInt32/MaxInt32 boundary values themselves).
	inRange := []int64{0, 1, 25092, -1, math.MaxInt32, math.MinInt32}
	for _, v := range inRange {
		o := sfs.NewSFSObject()
		o.PutUtfString("port", strconv.FormatInt(v, 10))
		if got := getIntFlexible(o, "port"); got != int32(v) {
			t.Errorf("getIntFlexible(%q) = %d, want %d (an in-range numeric string must still round-trip normally)", strconv.FormatInt(v, 10), got, int32(v))
		}
	}
}

// TestGetIntFlexibleWarnsOnWrongTypedField is the round-31 regression test for the MINOR finding
// that getIntFlexible had no diagnostic at all for a present-but-genuinely-anomalous field --
// either a non-empty string that isn't a valid integer literal, or a value of some other Go type
// entirely (bool/float/nested object) that neither the int-shaped nor string-shaped success path
// recognizes -- silently falling through to the same 0 fallback used for a merely-absent field.
// Proves both new anomaly cases now warn, while a genuinely-absent field and legitimately-zero
// in-range/string values (already covered by TestGetIntFlexible) stay silent.
func TestGetIntFlexibleWarnsOnWrongTypedField(t *testing.T) {
	run := func(t *testing.T, setup func(o *sfs.SFSObject)) string {
		t.Helper()
		o := sfs.NewSFSObject()
		setup(o)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := getIntFlexible(o, "port")
		slog.SetDefault(orig)

		if got != 0 {
			t.Errorf("getIntFlexible = %d, want 0", got)
		}
		return buf.String()
	}

	t.Run("non-numeric string warns", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) { o.PutUtfString("port", "not-a-number") })
		if !strings.Contains(logged, "non-numeric") {
			t.Errorf("expected a Warn mentioning the non-numeric string, got:\n%s", logged)
		}
	})
	// Round-32 regression: round 30 added the out-of-int32-range numeric-string guard itself
	// (TestGetIntFlexibleRejectsOutOfInt32RangeString above), but that guard silently returned 0
	// with zero diagnostic until this round -- round 31's own doc comment enumerating the newly-
	// diagnosed anomaly cases for this function omitted this one, even though it's exactly as
	// anomalous as the non-numeric-string case immediately above.
	t.Run("out-of-range numeric string warns", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) { o.PutUtfString("port", "4294967301") }) // 1<<32 + 5
		if !strings.Contains(logged, "out-of-int32-range") {
			t.Errorf("expected a Warn mentioning the out-of-range numeric string, got:\n%s", logged)
		}
	})
	// Round-33 regression: the fourth anomaly shape found after a final exhaustive re-check --
	// a present, CORRECTLY-typed int64 Long whose value simply doesn't fit in int32's range. This
	// is distinct from the string case above (which round 30 already guarded the VALUE against,
	// round 31/32 added the Warn for) -- here the field is a native Long on the wire, not a
	// string, so GetString never even sees it, and sfsFieldKindAccepts(sfsFieldKindInt, ...)
	// accepts any int64 by design (a pure type check, not a value-range check).
	// Round-41 regression: getIntFlexible's own out-of-range-Long check (added round 33) became
	// redundant once GetInt itself (sfsobject.go) gained the identical diagnostic in round 39 --
	// getIntFlexible's first line already calls o.GetInt(key), so from round 39 onward this exact
	// anomaly produced TWO separate Warn log lines for one input until the redundant check was
	// removed. Asserts exactly one occurrence, not just "contains", so a reintroduced duplicate
	// would fail this test instead of passing it unnoticed the way a bare Contains check would.
	t.Run("out-of-range native Long warns exactly once, not twice", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) { o.PutLong("port", int64(math.MaxInt32)+12345) })
		if got := strings.Count(logged, "out-of-int32-range"); got != 1 {
			t.Errorf("got %d Warn line(s) mentioning out-of-int32-range, want exactly 1 (GetInt's own diagnostic, not a redundant second one from getIntFlexible itself):\n%s", got, logged)
		}
	})
	t.Run("wrong Go type warns", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) { o.PutBool("port", true) })
		if !strings.Contains(logged, "wrong-typed") {
			t.Errorf("expected a Warn mentioning the wrong-typed field, got:\n%s", logged)
		}
	})
	t.Run("absent stays silent", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) {})
		if logged != "" {
			t.Errorf("expected no log output for a genuinely-absent field, got:\n%s", logged)
		}
	})
	t.Run("legitimate zero stays silent", func(t *testing.T) {
		logged := run(t, func(o *sfs.SFSObject) { o.PutInt("port", 0) })
		if logged != "" {
			t.Errorf("expected no log output for a legitimately-zero, correctly-typed field, got:\n%s", logged)
		}
	})
}

// TestGetIntFlexibleRedactsSensitiveKeyValue is the round-35 regression test for the MINOR finding
// that getIntFlexible logged a decoded field's raw scalar value directly in three of its four
// anomaly Warn branches, with no sfs.IsSensitiveSFSKey gate -- unlike every sibling wrong-typed-field
// guard in this codebase (requireFieldType/warnIfWrongTypedField/redirectIP/redirectZone all log
// only StringRedacted()/goType, never a field's own raw scalar), and unlike getIntFlexible's own
// fourth branch (the wrong-Go-type case), which already used the safe pattern. getIntFlexible is a
// generic, key-parameterized helper -- today's only real call sites hardcode key="port" (never
// sensitive), but this proves the guard itself works correctly for a key that IS sensitive,
// independent of what today's callers happen to pass.
func TestGetIntFlexibleRedactsSensitiveKeyValue(t *testing.T) {
	run := func(t *testing.T, key string, setup func(o *sfs.SFSObject)) string {
		t.Helper()
		o := sfs.NewSFSObject()
		setup(o)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		getIntFlexible(o, key)
		slog.SetDefault(orig)

		return buf.String()
	}

	t.Run("out-of-range native Long under a sensitive key is redacted", func(t *testing.T) {
		const secret = int64(math.MaxInt32) + 987654321
		logged := run(t, "loginKey", func(o *sfs.SFSObject) { o.PutLong("loginKey", secret) })
		if strings.Contains(logged, fmt.Sprintf("%d", secret)) {
			t.Errorf("expected the real out-of-range value to be redacted, got:\n%s", logged)
		}
		if !strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected a [REDACTED] placeholder in place of the real value, got:\n%s", logged)
		}
	})
	t.Run("non-numeric string under a sensitive key is redacted", func(t *testing.T) {
		logged := run(t, "loginKey", func(o *sfs.SFSObject) { o.PutUtfString("loginKey", "sk-live-not-a-number-secret") })
		if strings.Contains(logged, "sk-live-not-a-number-secret") {
			t.Errorf("expected the real non-numeric string value to be redacted, got:\n%s", logged)
		}
		if !strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected a [REDACTED] placeholder in place of the real value, got:\n%s", logged)
		}
	})
	t.Run("out-of-range numeric string under a sensitive key is redacted", func(t *testing.T) {
		logged := run(t, "loginKey", func(o *sfs.SFSObject) { o.PutUtfString("loginKey", "4294967301") })
		if strings.Contains(logged, "4294967301") {
			t.Errorf("expected the real out-of-range numeric string value to be redacted, got:\n%s", logged)
		}
		if !strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected a [REDACTED] placeholder in place of the real value, got:\n%s", logged)
		}
	})
	// Sanity check: a non-sensitive key's value must still appear in the log unredacted, proving
	// redactedValue doesn't over-sfs.Redact indiscriminately.
	t.Run("non-sensitive key value stays visible", func(t *testing.T) {
		logged := run(t, "port", func(o *sfs.SFSObject) { o.PutUtfString("port", "not-a-number") })
		if !strings.Contains(logged, "not-a-number") {
			t.Errorf("expected the non-sensitive field's real value to remain visible, got:\n%s", logged)
		}
		if strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected no [REDACTED] placeholder for a non-sensitive key, got:\n%s", logged)
		}
	})
}

func TestServerIDFromZone(t *testing.T) {
	cases := []struct{ zone, want string }{
		{"APS1234", "1234"},
		{"1234", "1234"},
		{"AP", "AP"},
		{"", ""},
	}
	for _, c := range cases {
		if got := serverIDFromZone(c.zone); got != c.want {
			t.Errorf("serverIDFromZone(%q) = %q, want %q", c.zone, got, c.want)
		}
	}
}
