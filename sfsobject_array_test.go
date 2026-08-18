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
