package main

import (
	"encoding/binary"
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

func TestNestedCountRejectsNegative(t *testing.T) {
	t.Run("SFSArray", func(t *testing.T) {
		r := &sfsReader{data: []byte{0xFF, 0xFF}} // count = -1
		if _, err := r.readValuePayload(sfsArrayType); err == nil {
			t.Fatal("expected error for negative nested array count, got nil")
		}
	})
	t.Run("SFSObject", func(t *testing.T) {
		r := &sfsReader{data: []byte{0xFF, 0xFF}} // count = -1
		if _, err := r.readValuePayload(sfsObjectType); err == nil {
			t.Fatal("expected error for negative nested object count, got nil")
		}
	})
}

func TestNestingDepthRejected(t *testing.T) {
	// Comfortably over maxNestDepth. Each level is a 1-byte tag + 2-byte count=1, so this stays
	// small and the depth check aborts well before any real recursion risk -- this test should
	// run instantly, not build a giant structure.
	const levels = 200
	var buf []byte
	buf = append(buf, 0, 1) // outermost array's count = 1 (no leading tag -- readValuePayload takes tag as a parameter, not read from the stream)
	for i := 0; i < levels-1; i++ {
		buf = append(buf, sfsArrayType, 0, 1) // one more nested array: tag byte + count=1
	}
	buf = append(buf, sfsBool, 1) // innermost leaf value: tag=bool, value=true

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(sfsArrayType)
	if err == nil {
		t.Fatal("expected an error for excessive nesting depth, got nil (this would have risked a stack overflow before the depth-limit fix)")
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

// TestDecodedNodeCountRejected proves the maxDecodedNodes ceiling catches breadth-driven
// amplification that maxNestDepth alone does not: this payload is only 2 levels deep (far under
// maxNestDepth=64) but its outer*inner fan-out crosses maxDecodedNodes in total leaf count -- the
// same shape as the audit's ~60MB/59M-node reproduction, scaled down so this test builds and runs
// in well under a second instead of allocating gigabytes.
func TestDecodedNodeCountRejected(t *testing.T) {
	const outerCount = 10
	const innerCount = 30001 // outerCount * innerCount > maxDecodedNodes (300_000), each count well under the int16-per-level cap

	var buf []byte
	buf = binary.BigEndian.AppendUint16(buf, outerCount)
	for i := 0; i < outerCount; i++ {
		buf = append(buf, sfsArrayType)
		buf = binary.BigEndian.AppendUint16(buf, innerCount)
		for j := 0; j < innerCount; j++ {
			buf = append(buf, sfsNull)
		}
	}

	r := &sfsReader{data: buf}
	_, err := r.readValuePayload(sfsArrayType)
	if err == nil {
		t.Fatal("expected an error once decoded node count exceeds maxDecodedNodes, got nil (this would allow unbounded heap amplification via wide, shallow nesting)")
	}
}
