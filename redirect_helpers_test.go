package main

import (
	"bytes"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestFindServerInfo(t *testing.T) {
	t.Run("nested under p", func(t *testing.T) {
		si := NewSFSObject()
		si.PutUtfString("ip", "1.2.3.4")
		p := NewSFSObject()
		p.PutSFSObject("serverInfo", si)
		content := NewSFSObject()
		content.PutSFSObject("p", p)
		got := findServerInfo(content)
		if got == nil || got.GetString("ip") != "1.2.3.4" {
			t.Fatalf("expected nested serverInfo to be found, got %v", got)
		}
	})
	t.Run("top-level fallback", func(t *testing.T) {
		si := NewSFSObject()
		si.PutUtfString("ip", "5.6.7.8")
		content := NewSFSObject()
		content.PutSFSObject("serverInfo", si)
		got := findServerInfo(content)
		if got == nil || got.GetString("ip") != "5.6.7.8" {
			t.Fatalf("expected top-level serverInfo to be found, got %v", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		content := NewSFSObject()
		if got := findServerInfo(content); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
	t.Run("nil content", func(t *testing.T) {
		if got := findServerInfo(nil); got != nil {
			t.Fatalf("expected nil, got %v", got)
		}
	})
}

func TestGetIntFlexible(t *testing.T) {
	t.Run("numeric field", func(t *testing.T) {
		o := NewSFSObject()
		o.PutInt("port", 25092)
		if got := getIntFlexible(o, "port"); got != 25092 {
			t.Fatalf("got %d, want 25092", got)
		}
	})
	t.Run("string-numeric field", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("port", "17783")
		if got := getIntFlexible(o, "port"); got != 17783 {
			t.Fatalf("got %d, want 17783", got)
		}
	})
	t.Run("absent", func(t *testing.T) {
		o := NewSFSObject()
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
	t.Run("empty string", func(t *testing.T) {
		o := NewSFSObject()
		o.PutUtfString("port", "")
		if got := getIntFlexible(o, "port"); got != 0 {
			t.Fatalf("got %d, want 0", got)
		}
	})
}

// TestGetIntFlexibleRejectsOutOfInt32RangeString is the round-30 regression test for the MAJOR
// finding: getIntFlexible's string-fallback path used to do a bare, unchecked int32(n) conversion
// on strconv.Atoi's result, reintroducing the exact int64-to-int32 unchecked-narrowing bug round 29
// fixed in sfsobject.go's GetInt. On a 64-bit platform Go's int is 64-bit, so Atoi parses a numeric
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
			o := NewSFSObject()
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
		o := NewSFSObject()
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
	run := func(t *testing.T, setup func(o *SFSObject)) string {
		t.Helper()
		o := NewSFSObject()
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
		logged := run(t, func(o *SFSObject) { o.PutUtfString("port", "not-a-number") })
		if !strings.Contains(logged, "non-numeric") {
			t.Errorf("expected a Warn mentioning the non-numeric string, got:\n%s", logged)
		}
	})
	t.Run("wrong Go type warns", func(t *testing.T) {
		logged := run(t, func(o *SFSObject) { o.PutBool("port", true) })
		if !strings.Contains(logged, "wrong-typed") {
			t.Errorf("expected a Warn mentioning the wrong-typed field, got:\n%s", logged)
		}
	})
	t.Run("absent stays silent", func(t *testing.T) {
		logged := run(t, func(o *SFSObject) {})
		if logged != "" {
			t.Errorf("expected no log output for a genuinely-absent field, got:\n%s", logged)
		}
	})
	t.Run("legitimate zero stays silent", func(t *testing.T) {
		logged := run(t, func(o *SFSObject) { o.PutInt("port", 0) })
		if logged != "" {
			t.Errorf("expected no log output for a legitimately-zero, correctly-typed field, got:\n%s", logged)
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
