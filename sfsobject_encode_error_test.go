package main

import (
	"strings"
	"testing"
)

// TestEncodeObjectOversizedStringReturnsError proves the writeUtfString/int16Count panic-on-
// oversized-input bug is fixed: EncodeObject must return an error, not crash the process, when a
// value string exceeds the wire format's 2-byte length-prefix limit (65535 bytes). This chain
// (EncodeObject -> writeTaggedValue -> writeValuePayload -> writeUtfString) is reachable from
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
// up through writeValuePayload's sfsObjectType recursion case, not just the top-level string case.
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
