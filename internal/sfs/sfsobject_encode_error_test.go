package sfs

import (
	"bytes"
	"math"
	"strings"
	"testing"
)

// TestEncodeObjectOversizedStringReturnsError proves the WriteUtfString/int16Count panic-on-
// oversized-input bug is fixed: EncodeObject must return an error, not crash the process, when a
// value string exceeds MaxUtfStringBytes (the SFS2X WriteUTF cap, 32767 bytes). This chain
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
// finding that WriteUtfString's own length cap (sfsobject.go: `if len(b) > MaxUtfStringBytes`)
// had no exact-boundary test -- distinct from int16Count's separate item-COUNT cap
// (round 44's TestEncodeObjectExactlyMaxArrayLengthSucceeds covers that one, not this one).
// TestEncodeObjectOversizedStringReturnsError/TestEncodeObjectOversizedNestedStringReturnsError
// above only prove a 70000-byte string (comfortably over the cap) is rejected, never that exactly
// MaxUtfStringBytes -- the boundary value itself -- still encodes and round-trips through
// DecodeObject successfully.
func TestEncodeObjectStringExactlyMaxLenSucceeds(t *testing.T) {
	want := strings.Repeat("z", MaxUtfStringBytes)

	o := NewSFSObject()
	o.PutUtfString("s", want)

	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject() error = %v, want nil for exactly %d bytes (the boundary value, not over the cap)", err, MaxUtfStringBytes)
	}

	decoded, err := DecodeObject(encoded)
	if err != nil {
		t.Fatalf("DecodeObject() error = %v, want nil", err)
	}
	if got := decoded.GetString("s"); got != want {
		t.Errorf("decoded string length = %d, want %d", len(got), len(want))
	}
}

// TestWriteUtfStringCapMatchesSFS2XWriteUTF pins the 32767-byte cap to the game's own SFS2X
// codec: ByteArray.WriteUTF throws above short.MaxValue bytes (SmartFox2X.decompiled.cs:
// 20369-20380), even though the u16 length prefix could carry up to 65535. One byte over the cap
// must fail -- as a value, as an object key, and inside a UTF string array -- since all three are
// written through WriteUtfString, as they are through WriteUTF in the real client. A rejected
// string must not leave a partial length prefix in the buffer.
func TestWriteUtfStringCapMatchesSFS2XWriteUTF(t *testing.T) {
	if MaxUtfStringBytes != 32767 {
		t.Fatalf("MaxUtfStringBytes = %d, want 32767 (short.MaxValue, the SFS2X WriteUTF cap)", MaxUtfStringBytes)
	}
	over := strings.Repeat("o", MaxUtfStringBytes+1)

	var buf bytes.Buffer
	if err := WriteUtfString(&buf, over); err == nil {
		t.Fatal("WriteUtfString accepted a 32768-byte string, want an error")
	}
	if buf.Len() != 0 {
		t.Errorf("WriteUtfString wrote %d bytes for a rejected string, want 0", buf.Len())
	}

	// The cap counts UTF-8 bytes, not runes: 10923 three-byte runes are 32769 bytes.
	if err := WriteUtfString(&buf, strings.Repeat("€", 10923)); err == nil {
		t.Error("WriteUtfString accepted 32769 bytes of multi-byte runes, want an error")
	}

	value := NewSFSObject()
	value.PutUtfString("s", over)
	if _, err := EncodeObject(value); err == nil {
		t.Error("EncodeObject accepted a 32768-byte UTF string value, want an error")
	}

	key := NewSFSObject()
	key.PutInt(over, 1)
	if _, err := EncodeObject(key); err == nil {
		t.Error("EncodeObject accepted a 32768-byte object key, want an error")
	}

	arr := NewSFSObject()
	arr.PutValue("a", SFSValue{sfsUtfStringArray, []string{"ok", over}})
	if _, err := EncodeObject(arr); err == nil {
		t.Error("EncodeObject accepted a 32768-byte UTF string array item, want an error")
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
	for i := range 32768 {
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
	for i := range 32767 {
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
