package main

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Confirms ReadPacket's Zstandard-decompression branch (header bits hdrCompressed|hdrUseLZ4)
// round-trips correctly -- this exact branch was the root cause of the "init push never arrives"
// investigation documented in docs/live-validation.mdx, and had zero test coverage before this.
func TestReadPacketZstdBranch(t *testing.T) {
	original := []byte(`{"c":"test.cmd","p":{"hello":"world","n":12345}}`)

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := enc.EncodeAll(original, nil)
	enc.Close()

	encrypted := xorCrypt(compressed)

	var buf bytes.Buffer
	buf.WriteByte(hdrBinary | hdrEncrypted | hdrCompressed | hdrUseLZ4)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], uint16(len(encrypted)))
	buf.Write(lb[:])
	var ub [4]byte
	binary.BigEndian.PutUint32(ub[:], uint32(len(original)))
	buf.Write(ub[:])
	buf.Write(encrypted)

	got, err := ReadPacket(&buf)
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("got %q, want %q", got, original)
	}
}
