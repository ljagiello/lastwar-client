package sfs

import (
	"bytes"
	"slices"
	"testing"
)

// PutIntArray must write the C# IntArray layout byte for byte: tag 12, an int16 item count, then
// big-endian int32 items (the 2-byte count, unlike ByteArray's 4-byte one).
func TestPutIntArrayWireBytes(t *testing.T) {
	o := NewSFSObject()
	o.PutIntArray("index", []int32{3451, -2})
	got, err := EncodeObject(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		SFSObjectType, 0, 1, // object, 1 key
		0, 5, 'i', 'n', 'd', 'e', 'x', // key
		sfsIntArray, 0, 2, // IntArray, 2 items
		0, 0, 0x0d, 0x7b, // 3451
		0xff, 0xff, 0xff, 0xfe, // -2
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("encoded % x\nwant    % x", got, want)
	}
	back, err := DecodeObject(got)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := back.Get("index")
	if !ok || v.Type != sfsIntArray {
		t.Fatalf("decoded index = %#v", v)
	}
	if arr, _ := v.Val.([]int32); !slices.Equal(arr, []int32{3451, -2}) {
		t.Errorf("decoded items = %v", v.Val)
	}
}

func TestAddByteArrayRoundTrips(t *testing.T) {
	arr := NewSFSArray()
	arr.AddByteArray([]byte{0x08, 0x96, 0x01})
	arr.AddByteArray(nil)
	o := NewSFSObject()
	o.PutSFSArray("points", arr)
	enc, err := EncodeObject(o)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeObject(enc)
	if err != nil {
		t.Fatal(err)
	}
	v, _ := back.Get("points")
	items := v.Val.(*SFSArray).Items()
	if len(items) != 2 || items[0].Type != sfsByteArray || !bytes.Equal(items[0].Val.([]byte), []byte{0x08, 0x96, 0x01}) ||
		len(items[1].Val.([]byte)) != 0 {
		t.Errorf("decoded points = %#v", items)
	}
}
