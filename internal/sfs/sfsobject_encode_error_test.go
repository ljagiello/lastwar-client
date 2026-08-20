package sfs

import (
	"math"
	"strings"
	"testing"
)

// TestEncodeObjectOversizedStringReturnsError proves the WriteUtfString/int16Count panic-on-
// oversized-input bug is fixed: EncodeObject must return an error, not crash the process, when a
// value string exceeds the wire format's 2-byte length-prefix limit (65535 bytes). This chain
// (EncodeObject -> writeTaggedValue -> writeValuePayload -> WriteUtfString) is reachable from
// server-controlled data with zero recover() anywhere in this repo, so a panic here previously
// meant any oversized value could crash the whole process.
func TestEncodeObjectOversizedStringReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("should not panic, got: %v", r)
		}
	}()

	o := NewSFSObject()
	o.PutUtfString("oversized", strings.Repeat("x", 70000))

	_, err := EncodeObject(o)
	if err == nil {
		t.Fatal("expected an error for a too-long string, got nil")
	}
}

// TestEncodeObjectOversizedNestedStringReturnsError proves the error propagates correctly back
// up through writeValuePayload's SFSObjectType recursion case, not just the top-level string case.
func TestEncodeObjectOversizedNestedStringReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("should not panic, got: %v", r)
		}
	}()

	inner := NewSFSObject()
	inner.PutUtfString("oversized", strings.Repeat("y", 70000))
	o := NewSFSObject()
	o.PutSFSObject("sub", inner)

	_, err := EncodeObject(o)
	if err == nil {
		t.Fatal("expected an error for a too-long nested string, got nil")
	}
}

// TestEncodeObjectStringExactlyMaxLenSucceeds is the round-45 regression test for the MINOR
// finding that WriteUtfString's own 65535-byte length-prefix cap (sfsobject.go: `if len(b) >
// 65535`) had no exact-boundary test -- distinct from int16Count's separate item-COUNT cap
// (round 44's TestEncodeObjectExactlyMaxArrayLengthSucceeds covers that one, not this one).
// TestEncodeObjectOversizedStringReturnsError/TestEncodeObjectOversizedNestedStringReturnsError
// above only prove a 70000-byte string (comfortably over the cap) is rejected, never that exactly
// 65535 bytes -- the boundary value itself -- still encodes and round-trips through DecodeObject
// successfully.
func TestEncodeObjectStringExactlyMaxLenSucceeds(t *testing.T) {
	want := strings.Repeat("z", 65535)

	o := NewSFSObject()
	o.PutUtfString("s", want)

	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject() error = %v, want nil for exactly 65535 bytes (the boundary value, not over the cap)", err)
	}

	decoded, err := DecodeObject(encoded)
	if err != nil {
		t.Fatalf("DecodeObject() error = %v, want nil", err)
	}
	if got := decoded.GetString("s"); got != want {
		t.Errorf("decoded string length = %d, want %d", len(got), len(want))
	}
}

// TestEncodeObjectTooManyKeysReturnsError proves int16Count's other call site (a too-large
// collection, not just a too-long string) also returns an error instead of panicking.
func TestEncodeObjectTooManyKeysReturnsError(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("should not panic, got: %v", r)
		}
	}()

	arr := NewSFSArray()
	for i := 0; i < 32768; i++ {
		arr.AddInt(int32(i))
	}
	o := NewSFSObject()
	o.PutSFSArray("bigArr", arr)

	_, err := EncodeObject(o)
	if err == nil {
		t.Fatal("expected an error for an over-32767-item array, got nil")
	}
}

// TestEncodeObjectExactlyMaxArrayLengthSucceeds is the round-44 regression test for the MINOR
// finding that int16Count's strict greater-than boundary (n > 32767, sfsobject.go) had no
// exact-boundary test -- TestEncodeObjectTooManyKeysReturnsError above only proves an array one
// item OVER the cap (32768) is rejected, never that exactly 32767 items -- the boundary value
// itself, shared by all 10 of int16Count's call sites (EncodeObject's key count and the 7
// primitive-array element counts, plus nested object/array key/item counts) -- still encodes (and
// round-trips through DecodeObject) successfully, which would catch an off-by-one `>=` mutation
// that rejected the last legitimate item.
func TestEncodeObjectExactlyMaxArrayLengthSucceeds(t *testing.T) {
	arr := NewSFSArray()
	for i := 0; i < 32767; i++ {
		arr.AddInt(int32(i))
	}
	o := NewSFSObject()
	o.PutSFSArray("bigArr", arr)

	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject() error = %v, want nil for exactly 32767 items (the boundary value, not over the cap)", err)
	}

	decoded, err := DecodeObject(encoded)
	if err != nil {
		t.Fatalf("DecodeObject() error = %v, want nil", err)
	}
	v, ok := decoded.Get("bigArr")
	if !ok {
		t.Fatal("decoded object missing bigArr field")
	}
	gotArr, ok := v.Val.(*SFSArray)
	if !ok {
		t.Fatalf("bigArr decoded as %T, want *SFSArray", v.Val)
	}
	if len(gotArr.items) != 32767 {
		t.Errorf("decoded array length = %d, want 32767", len(gotArr.items))
	}
}

// TestInt32CountExactBoundary is the round-48 regression test for the MINOR finding that
// int32Count (SFSText/sfsByteArray's wire-count overflow guard, the int32-wide sibling of
// int16Count above and WriteUtfString) had zero test coverage of any kind. Both siblings have
// dedicated exact-boundary tests here (TestEncodeObjectExactlyMaxArrayLengthSucceeds/
// TestEncodeObjectTooManyKeysReturnsError for int16Count; TestEncodeObjectStringExactlyMaxLenSucceeds/
// TestEncodeObjectOversizedStringReturnsError for WriteUtfString), each proving both accept-at-
// boundary and reject-one-past-boundary behavior -- int32Count had none. A true end-to-end
// EncodeObject test would need a >2GB string/byte-slice value to actually drive n past
// math.MaxInt32, impractically expensive to construct and run; int32Count is called directly here
// instead, mirroring int16Count's own pure-function-level testability.
func TestInt32CountExactBoundary(t *testing.T) {
	t.Run("exactly math.MaxInt32: accepted", func(t *testing.T) {
		got, err := int32Count(math.MaxInt32, "x")
		if err != nil {
			t.Fatalf("int32Count(math.MaxInt32, ...) error = %v, want nil", err)
		}
		if got != math.MaxInt32 {
			t.Errorf("got %d, want %d", got, int32(math.MaxInt32))
		}
	})

	t.Run("math.MaxInt32 plus one: rejected", func(t *testing.T) {
		_, err := int32Count(math.MaxInt32+1, "x")
		if err == nil {
			t.Fatal("int32Count(math.MaxInt32+1, ...) error = nil, want an overflow error")
		}
	})
}
