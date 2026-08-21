package sfs

import (
	"bytes"
	"encoding/binary"
	"log/slog"
	"math"
	"strings"
	"testing"
)

// Confirms the decode-only array-tag branches round-trip correctly, and specifically that
// ByteArray uses a 4-byte element count while every other array tag uses a 2-byte count -- the
// exact asymmetry that silently misaligned a real 313KB production payload before it was caught
// (see sfsobject.go's comment on the sfsByteArray case, and docs/live-validation.mdx).
func TestArrayDecodeRoundTrips(t *testing.T) {
	t.Run("BoolArray (2-byte count)", func(t *testing.T) {
		buf := []byte{0, 3, 1, 0, 1} // count=3 (int16 BE), then true,false,true
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsBoolArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.([]bool)
		if !ok {
			t.Fatalf("wrong type: %T", v.Val)
		}
		want := []bool{true, false, true}
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("ShortArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 2)
		buf = binary.BigEndian.AppendUint16(buf, 0xFFFE) // -2
		buf = binary.BigEndian.AppendUint16(buf, 7)
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsShortArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]int16)
		want := []int16{-2, 7}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("IntArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 2)
		negHundred := int32(-100)
		buf = binary.BigEndian.AppendUint32(buf, uint32(negHundred))
		buf = binary.BigEndian.AppendUint32(buf, 200)
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsIntArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]int32)
		want := []int32{-100, 200}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("LongArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 1)
		buf = binary.BigEndian.AppendUint64(buf, uint64(int64(1234567890123)))
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsLongArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]int64)
		if len(got) != 1 || got[0] != 1234567890123 {
			t.Fatalf("got %v, want [1234567890123]", got)
		}
	})

	t.Run("FloatArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 1)
		buf = binary.BigEndian.AppendUint32(buf, 0x3F800000) // 1.0f
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsFloatArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]float32)
		if len(got) != 1 || got[0] != 1.0 {
			t.Fatalf("got %v, want [1.0]", got)
		}
	})

	t.Run("DoubleArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 1)
		buf = binary.BigEndian.AppendUint64(buf, 0x3FF0000000000000) // 1.0
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsDoubleArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]float64)
		if len(got) != 1 || got[0] != 1.0 {
			t.Fatalf("got %v, want [1.0]", got)
		}
	})

	t.Run("UtfStringArray (2-byte count)", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint16(buf, 2)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len("hello")))
		buf = append(buf, "hello"...)
		buf = binary.BigEndian.AppendUint16(buf, uint16(len("world")))
		buf = append(buf, "world"...)
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsUtfStringArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]string)
		want := []string{"hello", "world"}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("ByteArray uses a 4-byte count, unlike every other array tag", func(t *testing.T) {
		var buf []byte
		buf = binary.BigEndian.AppendUint32(buf, 3) // 4-byte count, not 2
		buf = append(buf, 0x01, 0x02, 0x03)
		r := &sfsReader{data: buf}
		v, err := r.readValuePayload(sfsByteArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got := v.Val.([]byte)
		want := []byte{1, 2, 3}
		if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

// TestArrayEncodeDecodeRoundTrips proves writeValuePayload's array/text cases -- added to close
// the audit finding that the encode path had no cases for any primitive-array wire type or
// SFSText, though decode fully supported them -- produce bytes that readValuePayload decodes back
// to the original value. Unlike TestArrayDecodeRoundTrips above (which only proves decode parses
// hand-built bytes), this drives both directions through the real encode and decode code paths.
func TestArrayEncodeDecodeRoundTrips(t *testing.T) {
	t.Run("BoolArray", func(t *testing.T) {
		want := []bool{true, false, true}
		var buf bytes.Buffer
		_ = writeValuePayload(&buf, SFSValue{sfsBoolArray, want})
		r := &sfsReader{data: buf.Bytes()}
		v, err := r.readValuePayload(sfsBoolArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.([]bool)
		if !ok || len(got) != len(want) {
			t.Fatalf("got %v, want %v", v.Val, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("IntArray", func(t *testing.T) {
		want := []int32{-100, 0, 200}
		var buf bytes.Buffer
		_ = writeValuePayload(&buf, SFSValue{sfsIntArray, want})
		r := &sfsReader{data: buf.Bytes()}
		v, err := r.readValuePayload(sfsIntArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.([]int32)
		if !ok || len(got) != len(want) {
			t.Fatalf("got %v, want %v", v.Val, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("ByteArray (4-byte count)", func(t *testing.T) {
		want := []byte{1, 2, 3, 4}
		var buf bytes.Buffer
		_ = writeValuePayload(&buf, SFSValue{sfsByteArray, want})
		r := &sfsReader{data: buf.Bytes()}
		v, err := r.readValuePayload(sfsByteArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.([]byte)
		if !ok || !bytes.Equal(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("UtfStringArray", func(t *testing.T) {
		want := []string{"hello", "world"}
		var buf bytes.Buffer
		_ = writeValuePayload(&buf, SFSValue{sfsUtfStringArray, want})
		r := &sfsReader{data: buf.Bytes()}
		v, err := r.readValuePayload(sfsUtfStringArray)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.([]string)
		if !ok || len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("index %d: got %v, want %v", i, got[i], want[i])
			}
		}
	})

	t.Run("Text", func(t *testing.T) {
		want := "a long-form text field"
		var buf bytes.Buffer
		_ = writeValuePayload(&buf, SFSValue{SFSText, want})
		r := &sfsReader{data: buf.Bytes()}
		v, err := r.readValuePayload(SFSText)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		got, ok := v.Val.(string)
		if !ok || got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})
}

func TestNestedCountRejectsNegative(t *testing.T) {
	t.Run("SFSArray", func(t *testing.T) {
		r := &sfsReader{data: []byte{0xFF, 0xFF}} // count = -1
		if _, err := r.readValuePayload(sfsArrayType); err == nil {
			t.Fatal("expected error for negative nested array count, got nil")
		}
	})
	t.Run("SFSObject", func(t *testing.T) {
		r := &sfsReader{data: []byte{0xFF, 0xFF}} // count = -1
		if _, err := r.readValuePayload(SFSObjectType); err == nil {
			t.Fatal("expected error for negative nested object count, got nil")
		}
	})
}

func TestNestingDepthRejected(t *testing.T) {
	// Comfortably over MaxNestDepth. Each level is a 1-byte tag + 2-byte count=1, so this stays
	// small and the depth check aborts well before any real recursion risk -- this test should
	// run instantly, not build a giant structure.
	const levels = 200
	var buf []byte
	buf = append(buf, 0, 1) // outermost array's count = 1 (no leading tag -- readValuePayload takes tag as a parameter, not read from the stream)
	for range levels - 1 {
		buf = append(buf, sfsArrayType, 0, 1) // one more nested array: tag byte + count=1
	}
	buf = append(buf, SFSBool, 1) // innermost leaf value: tag=bool, value=true

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(sfsArrayType)
	if err == nil {
		t.Fatal("expected an error for excessive nesting depth, got nil (this would have risked a stack overflow before the depth-limit fix)")
	}
}

// TestNestingDepthExactlyAtCapSucceeds is the round-43 regression test for the MINOR finding that
// MaxNestDepth's strict greater-than boundary (r.depth > MaxNestDepth, sfsobject.go) had no
// exact-boundary test -- TestNestingDepthRejected above only proves 200 levels (comfortably OVER
// the 64-level cap) is rejected, never that exactly 64 levels -- the boundary value itself -- is
// still accepted, which would catch an off-by-one `>=` mutation that rejected one level early.
// Mirrors this round's sibling boundary tests (TestCollectAllExactlyAtCapDoesNotTruncate,
// TestParseInitVisitorsMaxNumExactlyAtUpperBoundDoesNotClamp, etc.) applied to this decode-time cap.
func TestNestingDepthExactlyAtCapSucceeds(t *testing.T) {
	const levels = MaxNestDepth
	var buf []byte
	buf = append(buf, 0, 1) // outermost array's count = 1 (tag passed as a parameter, not read from the stream)
	for range levels - 1 {
		buf = append(buf, sfsArrayType, 0, 1) // one more nested array: tag byte + count=1
	}
	buf = append(buf, SFSBool, 1) // innermost leaf value: tag=bool, value=true

	r := &sfsReader{data: buf}
	if _, err := r.readValuePayload(sfsArrayType); err != nil {
		t.Errorf("readValuePayload() error = %v, want nil for exactly %d levels of nesting (the boundary value itself, not over the cap)", err, levels)
	}
}

// TestNestingDepthRejectedSFSObject is the round-50 regression test for the SFSObjectType sibling
// of TestNestingDepthRejected above: the depth check (r.depth > MaxNestDepth) is applied
// identically in the SFSObjectType case (sfsobject.go), but until now was only ever exercised via
// nested SFSArrays, never nested SFSObjects -- a plausible independent regression site, since the
// two cases increment/check/defer-decrement r.depth as separate, textually-duplicated blocks
// rather than shared code.
func TestNestingDepthRejectedSFSObject(t *testing.T) {
	const levels = 200
	var buf []byte
	buf = append(buf, 0, 1) // outermost object's count = 1 (tag passed as a parameter, not read from the stream)
	for range levels - 1 {
		buf = append(buf, 0, 0)          // key: empty string (2-byte length = 0)
		buf = append(buf, SFSObjectType) // one more nested object: tag byte
		buf = append(buf, 0, 1)          // nested object's own count = 1
	}
	buf = append(buf, 0, 0)       // innermost leaf key: empty string
	buf = append(buf, SFSBool, 1) // innermost leaf value: tag=bool, value=true

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(SFSObjectType)
	if err == nil {
		t.Fatal("expected an error for excessive nesting depth via nested SFSObjects, got nil (this would have risked a stack overflow before the depth-limit fix)")
	}
}

// TestNestingDepthExactlyAtCapSucceedsSFSObject is the SFSObjectType sibling of
// TestNestingDepthExactlyAtCapSucceeds above, proving the same off-by-one boundary
// (r.depth > MaxNestDepth) is correct for nested SFSObjects specifically, not just nested
// SFSArrays.
func TestNestingDepthExactlyAtCapSucceedsSFSObject(t *testing.T) {
	const levels = MaxNestDepth
	var buf []byte
	buf = append(buf, 0, 1) // outermost object's count = 1
	for range levels - 1 {
		buf = append(buf, 0, 0)
		buf = append(buf, SFSObjectType)
		buf = append(buf, 0, 1)
	}
	buf = append(buf, 0, 0)
	buf = append(buf, SFSBool, 1)

	r := &sfsReader{data: buf}
	if _, err := r.readValuePayload(SFSObjectType); err != nil {
		t.Errorf("readValuePayload() error = %v, want nil for exactly %d levels of nesting via nested SFSObjects (the boundary value itself, not over the cap)", err, levels)
	}
}

func TestByteArrayRejectsNegativeCount(t *testing.T) {
	negOne := int32(-1)
	var buf []byte
	buf = binary.BigEndian.AppendUint32(buf, uint32(negOne))
	r := &sfsReader{data: buf}
	if _, err := r.readValuePayload(sfsByteArray); err == nil {
		t.Fatal("expected an error for a negative byte-array count, got nil")
	}
}

// TestTextRejectsNegativeCount mirrors TestByteArrayRejectsNegativeCount: SFSText shares
// sfsByteArray's bare 4-byte length prefix, and now shares its explicit negative-length guard
// too, so a corrupt/hostile length produces the same specific, tag-appropriate error instead of
// falling through to readBytes' generic negative-length check.
func TestTextRejectsNegativeCount(t *testing.T) {
	negOne := int32(-1)
	var buf []byte
	buf = binary.BigEndian.AppendUint32(buf, uint32(negOne))
	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(SFSText)
	if err == nil {
		t.Fatal("expected an error for a negative text count, got nil")
	}
	wantMsg := "sfsobject: text negative size: -1"
	if err.Error() != wantMsg {
		t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
	}
}

// TestReadBytesRejectsNegativeCount is the round-50 regression test for sfsReader.readBytes'
// leading `n < 0` guard (sfsobject.go), which had zero direct test coverage: every existing
// negative-count test above (TestNestedCountRejectsNegative, TestByteArrayRejectsNegativeCount,
// TestTextRejectsNegativeCount, ...) only reaches this guard indirectly, through a caller that has
// already independently validated non-negativity itself (readArrayCount's own `n < 0` check, or
// sfsByteArray/SFSText's explicit int32-negative-length guards) before ever calling readBytes --
// so readBytes' own guard, defense-in-depth for any call site that does NOT pre-validate, was never
// actually exercised. Calls readBytes directly with n=-1, the one shape that can only ever reach
// this specific guard.
func TestReadBytesRejectsNegativeCount(t *testing.T) {
	r := &sfsReader{data: []byte{1, 2, 3}}
	if _, err := r.readBytes(-1); err == nil {
		t.Fatal("expected an error for a negative byte count, got nil")
	}
}

// TestReadTaggedValuePropagatesReadByteError is the round-51 regression test for
// sfsReader.readTaggedValue's error-propagation branch (sfsobject.go) -- the only decode path used
// for reading each element inside an SFSArray container -- which had zero test coverage. Calls it
// directly on an exhausted reader (no bytes remaining for the next item's tag byte), the one shape
// that can only ever reach this specific guard.
func TestReadTaggedValuePropagatesReadByteError(t *testing.T) {
	r := &sfsReader{data: []byte{}}
	if _, err := r.readTaggedValue(); err == nil {
		t.Fatal("expected an error reading a tagged value with no bytes remaining, got nil")
	}
}

// TestArrayTruncatedMidElementPropagatesError is the integration-level companion to
// TestReadTaggedValuePropagatesReadByteError above: proves the guard is reachable through a real
// array decode, not just directly. Builds a well-formed SFSArray header declaring 2 items, followed
// by exactly one well-formed item and then nothing -- the second item's tag-byte read inside
// readTaggedValue is what fails, reproducing a connection reset/truncated capture landing exactly
// at an array element boundary (this project's own explicitly in-scope hostile -cs-ip threat model).
func TestArrayTruncatedMidElementPropagatesError(t *testing.T) {
	var buf []byte
	buf = append(buf, 0, 2)       // declared item count = 2
	buf = append(buf, SFSBool, 1) // item 0: a well-formed bool, tag+value
	// item 1 is never written -- the array header promised 2 items but only 1 arrived.

	r := &sfsReader{data: buf}
	if _, err := r.readValuePayload(sfsArrayType); err == nil {
		t.Fatal("expected an error decoding an array truncated mid-element, got nil")
	}
}

func TestGetIntGetLongCoercion(t *testing.T) {
	o := NewSFSObject()
	o.PutByte("b", 7)
	o.PutShort("s", 8)
	o.PutInt("i", 9)
	o.PutLong("l", 10)

	if got := o.GetInt("b"); got != 7 {
		t.Errorf("GetInt(byte field) = %d, want 7", got)
	}
	if got := o.GetInt("s"); got != 8 {
		t.Errorf("GetInt(short field) = %d, want 8", got)
	}
	if got := o.GetInt("l"); got != 10 {
		t.Errorf("GetInt(long field) = %d, want 10", got)
	}
	if got := o.GetLong("b"); got != 7 {
		t.Errorf("GetLong(byte field) = %d, want 7", got)
	}
	if got := o.GetLong("s"); got != 8 {
		t.Errorf("GetLong(short field) = %d, want 8", got)
	}
	if got := o.GetLong("i"); got != 9 {
		t.Errorf("GetLong(int field) = %d, want 9", got)
	}
}

// TestGetIntRejectsOutOfInt32RangeLong is the round-29 regression test for the MAJOR finding: GetInt
// used to do a bare, unchecked int32(n) conversion in its int64 case, which Go silently
// truncates/wraps modulo 2^32 rather than erroring on -- an out-of-int32-range Long used to come out
// as a small, unrelated, possibly-negative int32 instead of being treated as invalid. This proves
// that no longer happens: a value comfortably outside int32's range must now come back as the
// documented zero-value fallback (the same fallback GetInt already uses for a wrong-Go-typed field),
// not as a wrapped, corrupted int32.
func TestGetIntRejectsOutOfInt32RangeLong(t *testing.T) {
	tests := []struct {
		name string
		val  int64
	}{
		// 1<<32 + 5 wraps to 5 under naive int32(n) truncation -- picking a value whose wrapped
		// result would itself look like a plausible small int32 is the whole point: a test value
		// that wrapped to something already-implausible (e.g. still enormous) wouldn't actually
		// prove the old bug is fixed.
		{"just above MaxInt32, wraps to a small negative value under naive truncation", math.MaxInt32 + 1},
		{"far above MaxInt32 (1<<32 + 5 wraps to 5)", int64(1)<<32 + 5},
		{"just below MinInt32", math.MinInt32 - 1},
		// -(1<<40) alone would coincidentally wrap to exactly 0 (it's a multiple of 1<<32), which
		// wouldn't distinguish this from the already-correct zero-value fallback -- the -7 offset
		// keeps it comfortably out of int32's range while still wrapping to a recognizably nonzero
		// value (-7) under the naive conversion this test guards against.
		{"far below MinInt32", -(int64(1) << 40) - 7},
		{"math.MaxInt64", math.MaxInt64},
		// math.MinInt64 alone would also coincidentally wrap to exactly 0 (same "multiple of 1<<32"
		// reason as above) -- +1 keeps it at the extreme boundary while wrapping to a recognizably
		// nonzero value (1) instead.
		{"math.MinInt64 + 1", math.MinInt64 + 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := NewSFSObject()
			o.PutLong("v", tt.val)

			got := o.GetInt("v")

			// The naive int32(n) conversion Go performs is the exact bug this test guards against --
			// computing it here (rather than hardcoding an expected wrapped value) keeps the test
			// resilient to exactly which wrapped value a given input produces, while still proving
			// GetInt's real output is NOT that wrapped value.
			wrapped := int32(tt.val)
			if got == wrapped && wrapped != 0 {
				t.Errorf("GetInt(%d) = %d, which is the silently-wrapped (int32(n)) value -- want the zero-value fallback (0) for an out-of-int32-range Long, not a wrapped/corrupted value", tt.val, got)
			}
			if got != 0 {
				t.Errorf("GetInt(%d) = %d, want 0 (the documented zero-value fallback for an out-of-int32-range Long)", tt.val, got)
			}
		})
	}

	// Sanity/boundary check: values that DO fit in int32's range must still round-trip normally,
	// proving this fix didn't accidentally over-tighten GetInt for legitimate in-range Longs
	// (including the exact MinInt32/MaxInt32 boundary values themselves).
	inRange := []int64{0, 1, -1, math.MaxInt32, math.MinInt32}
	for _, v := range inRange {
		o := NewSFSObject()
		o.PutLong("v", v)
		if got := o.GetInt("v"); got != int32(v) {
			t.Errorf("GetInt(%d) = %d, want %d (an in-range Long must still round-trip normally)", v, got, int32(v))
		}
	}
}

// TestGetIntWarnsOnOutOfRangeLong is the round-39 regression test for the MAJOR finding that
// GetInt's out-of-int32-range Long fallback (added round 29) degraded silently, with zero
// diagnostic -- unlike its two siblings specifically hardened against this exact anomaly shape
// (gsl.go's getIntFlexible, round 33; visitors.go's ParseInitVisitors maxNum handling, round 35).
// A real call site (alliance.go's findRecommendedTech -> DonateRecommendedAllianceTech) reads a
// corrupted scienceId this way straight into a live al.science.donate network request with no
// other signal the value was wrong rather than genuinely 0. Also proves the logged value is
// redacted for a sensitive key, matching getIntFlexible's own IsSensitiveSFSKey gating.
func TestGetIntWarnsOnOutOfRangeLong(t *testing.T) {
	run := func(t *testing.T, key string, val int64) string {
		t.Helper()
		o := NewSFSObject()
		o.PutLong(key, val)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := o.GetInt(key)
		slog.SetDefault(orig)

		if got != 0 {
			t.Errorf("GetInt(%d) = %d, want 0", val, got)
		}
		return buf.String()
	}

	t.Run("out-of-range Long under a non-sensitive key warns with the real value visible", func(t *testing.T) {
		logged := run(t, "scienceId", math.MaxInt32+12345)
		if !strings.Contains(logged, "out-of-int32-range") {
			t.Errorf("expected a Warn mentioning the out-of-range Long, got:\n%s", logged)
		}
		if !strings.Contains(logged, "2147495992") { // math.MaxInt32 + 12345
			t.Errorf("expected the real out-of-range value visible for a non-sensitive key, got:\n%s", logged)
		}
	})
	t.Run("out-of-range Long under a sensitive key is redacted", func(t *testing.T) {
		logged := run(t, "loginKey", math.MaxInt32+12345)
		if !strings.Contains(logged, "out-of-int32-range") {
			t.Errorf("expected a Warn mentioning the out-of-range Long, got:\n%s", logged)
		}
		if strings.Contains(logged, "2147495992") {
			t.Errorf("expected the real out-of-range value to be redacted for a sensitive key, got:\n%s", logged)
		}
		if !strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected a [REDACTED] placeholder in place of the real value, got:\n%s", logged)
		}
	})
	t.Run("in-range Long stays silent", func(t *testing.T) {
		o := NewSFSObject()
		o.PutLong("scienceId", 42)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := o.GetInt("scienceId")
		slog.SetDefault(orig)

		if got != 42 {
			t.Errorf("GetInt(42) = %d, want 42", got)
		}
		if buf.String() != "" {
			t.Errorf("expected no log output for a legitimate in-range Long, got:\n%s", buf.String())
		}
	})
}

// TestDecodeObjectRejectsTrailingBytes proves DecodeObject errors instead of silently ignoring
// leftover bytes after a well-formed top-level object -- every real caller (conn.go, decode.go)
// hands DecodeObject an exact-length frame body, so a trailing remainder means the encode/decode
// walk desynced somewhere, the same class of silent misalignment the sfsByteArray count-width bug
// caused before it was caught (see the comment on that case in sfsobject.go).
func TestDecodeObjectRejectsTrailingBytes(t *testing.T) {
	o := NewSFSObject()
	o.PutUtfString("key", "value")
	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// A well-formed encoding decodes cleanly on its own.
	if _, err := DecodeObject(encoded); err != nil {
		t.Fatalf("unexpected error decoding well-formed object: %v", err)
	}

	// Appending arbitrary trailing bytes must now be rejected instead of silently truncated-but-
	// successful.
	withGarbage := append(append([]byte(nil), encoded...), 0xDE, 0xAD, 0xBE)
	_, err = DecodeObject(withGarbage)
	if err == nil {
		t.Fatal("expected an error for trailing bytes after a well-formed object, got nil")
	}
	wantMsg := "sfsobject: 3 trailing bytes after decoded object"
	if err.Error() != wantMsg {
		t.Fatalf("error = %q, want %q", err.Error(), wantMsg)
	}
}

// TestDecodedNodeCountRejected proves the MaxDecodedNodes ceiling catches breadth-driven
// amplification that MaxNestDepth alone does not: this payload is only 2 levels deep (far under
// MaxNestDepth=64) but its outer*inner fan-out crosses MaxDecodedNodes in total leaf count -- the
// same shape as the audit's ~60MB/59M-node reproduction, scaled down so this test builds and runs
// in well under a second instead of allocating gigabytes.
func TestDecodedNodeCountRejected(t *testing.T) {
	const outerCount = 10
	const innerCount = 30001 // outerCount * innerCount > MaxDecodedNodes (300_000), each count well under the int16-per-level cap

	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, outerCount)
	for range outerCount {
		buf = append(buf, sfsArrayType)
		buf = binary.BigEndian.AppendUint16(buf, innerCount)
		for range innerCount {
			buf = append(buf, SFSNull)
		}
	}

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(sfsArrayType)
	if err == nil {
		t.Fatal("expected an error once decoded node count exceeds MaxDecodedNodes, got nil (this would allow unbounded heap amplification via wide, shallow nesting)")
	}
}

// TestDecodedNodeCountExactlyAtCapSucceeds is the round-44 regression test for the MINOR finding
// that MaxDecodedNodes' strict greater-than boundary (r.nodes > MaxDecodedNodes, checked in both
// readValuePayload and chargeNodes) had no exact-boundary test -- TestDecodedNodeCountRejected
// above only proves a payload comfortably OVER the 300,000-node cap is rejected, never that a
// payload landing on EXACTLY 300,000 total decoded nodes -- the boundary value itself -- is still
// accepted, which would catch an off-by-one `>=` mutation that rejected the last legitimate node.
// Mirrors TestDecodedNodeCountRejected's outer-array-of-inner-arrays-of-SFSNull shape, sized to
// land on exactly MaxDecodedNodes: 1 (the outer array's own node, charged by the direct
// readValuePayload(sfsArrayType) call below) + outerCount (each inner array's own node) + the sum
// of each inner array's leaf count. Group sizes are uneven (9 groups of 30,000 plus one of 29,989)
// purely to hit the exact total without needing an evenly-divisible count -- each individual count
// still stays comfortably under the int16 per-level array-count limit (32767).
func TestDecodedNodeCountExactlyAtCapSucceeds(t *testing.T) {
	const outerCount = 10
	innerCounts := [outerCount]int{30000, 30000, 30000, 30000, 30000, 30000, 30000, 30000, 30000, 29989}

	total := 1 + outerCount
	for _, c := range innerCounts {
		total += c
	}
	if total != MaxDecodedNodes {
		t.Fatalf("test construction bug: total nodes = %d, want exactly %d", total, MaxDecodedNodes)
	}

	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, outerCount)
	for _, innerCount := range innerCounts {
		buf = append(buf, sfsArrayType)
		buf = binary.BigEndian.AppendUint16(buf, uint16(innerCount))
		for range innerCount {
			buf = append(buf, SFSNull)
		}
	}

	r := &sfsReader{data: buf}
	if _, err := r.readValuePayload(sfsArrayType); err != nil {
		t.Errorf("readValuePayload() error = %v, want nil for a payload decoding to exactly %d total nodes (the boundary value, not over the cap)", err, MaxDecodedNodes)
	}
	if r.nodes != MaxDecodedNodes {
		t.Errorf("r.nodes = %d, want exactly %d (test construction should land precisely on the boundary)", r.nodes, MaxDecodedNodes)
	}
}

// TestDecodedNodeCountRejectedForPrimitiveArrays is the round-13 regression test for the
// MaxDecodedNodes undercount the round-13 audit found: the 8 primitive-array decode cases
// (sfsBoolArray..sfsUtfStringArray) each only charged 1 node regardless of how many elements they
// actually decoded (up to 32767 per array, read directly via readByte/readInt16/etc. rather than
// recursively through readValuePayload per element like the container types), so a wire payload
// with many primitive-array fields could stay comfortably under the old (undercounted) budget
// while decoding into far more Go heap than the budget was meant to bound. This payload is only
// one level deep (an SFSObject with outerCount sibling fields, no nesting at all) but its
// outerCount*arrayLen total element count crosses MaxDecodedNodes(300_000) -- under the old,
// per-field-flat-1 counting this would have cost only outerCount=10 nodes and sailed through.
func TestDecodedNodeCountRejectedForPrimitiveArrays(t *testing.T) {
	const outerCount = 10
	const arrayLen = 32767 // max representable int16 array count; outerCount*arrayLen (327,670) > MaxDecodedNodes (300_000)

	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, outerCount) // SFSObject key count
	for i := range outerCount {
		key := []byte{'k', byte('0' + i)}
		buf = binary.BigEndian.AppendUint16(buf, uint16(len(key)))
		buf = append(buf, key...)
		buf = append(buf, sfsBoolArray) // field tag
		buf = binary.BigEndian.AppendUint16(buf, uint16(arrayLen))
		buf = append(buf, make([]byte, arrayLen)...) // arrayLen bool elements, all false
	}

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(SFSObjectType)
	if err == nil {
		t.Fatal("expected an error once a primitive array's decoded element count pushes the total over MaxDecodedNodes, got nil (this would allow ~8x Go-heap amplification within the existing wire-frame cap via many cheap-on-the-wire primitive-array fields)")
	}
}

// TestSFSObjectAccessorsOnNilReceiverDoNotPanic is the round-32 regression test for the NIT finding
// that Has/Get/GetString/GetInt/GetLong lacked the nil-receiver guard StringRedacted/EncodeObject/
// writeValuePayload already apply elsewhere in this file -- a bare `var o *SFSObject; o.GetString(
// "x")` used to panic with a nil-pointer dereference. Currently unreachable in practice (the sole
// helper that can hand back a nil *SFSObject, gsl.go's findServerInfo, is nil-checked at both its
// current call sites), but the guard costs nothing and closes the inconsistency against a future
// call site that isn't as careful.
func TestSFSObjectAccessorsOnNilReceiverDoNotPanic(t *testing.T) {
	var o *SFSObject

	if got := o.Has("x"); got != false {
		t.Errorf("nil.Has(...) = %v, want false", got)
	}
	if v, ok := o.Get("x"); ok || v != (SFSValue{}) {
		t.Errorf("nil.Get(...) = (%v, %v), want (zero value, false)", v, ok)
	}
	if got := o.GetString("x"); got != "" {
		t.Errorf("nil.GetString(...) = %q, want \"\"", got)
	}
	if got := o.GetInt("x"); got != 0 {
		t.Errorf("nil.GetInt(...) = %d, want 0", got)
	}
	if got := o.GetLong("x"); got != 0 {
		t.Errorf("nil.GetLong(...) = %d, want 0", got)
	}
}
