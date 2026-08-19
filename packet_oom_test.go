package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// failReader serves a fixed byte sequence and fails the test the instant
// anything asks for more than that. It exists to prove ReadPacket's size
// guards reject a hostile frame using only the header fields -- before ever
// attempting to read (let alone allocate) the declared body -- rather than
// merely detecting the oversize condition after the fact.
type failReader struct {
	t    *testing.T
	data []byte
	pos  int
}

func (r *failReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		r.t.Fatalf("ReadPacket read past the fabricated header: guard did not reject before touching the body")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}

// Confirms the declared (compressed) length guard in ReadPacket rejects an
// oversized frame using only the header's length field -- before any body
// bytes are read or a length-sized buffer is allocated. Without this guard,
// a hostile or corrupted peer could declare an arbitrary multi-GB length and
// force an unbounded allocation (see the maxFrameSize doc comment).
func TestReadPacketRejectsOversizedDeclaredLength(t *testing.T) {
	var hdr bytes.Buffer
	hdr.WriteByte(hdrBinary | hdrEncrypted | hdrBigSized)
	var lb [4]byte
	binary.BigEndian.PutUint32(lb[:], maxFrameSize+1)
	hdr.Write(lb[:])

	r := &failReader{t: t, data: hdr.Bytes()}
	if _, err := ReadPacket(r); err == nil {
		t.Fatal("expected ReadPacket to reject a declared length over maxFrameSize, got nil error")
	}
}

// Confirms the zstd-flagged uncompressed-length guard in ReadPacket rejects
// an oversized frame using only the header's uncompressed-length field --
// before decompression (or even the compressed-body read) is attempted.
// zstd.DecodeAll is handed this value as a preallocation hint, so an
// unchecked value here is the same multi-GB OOM vector as the declared
// length guard, just one level deeper.
func TestReadPacketRejectsOversizedZstdUncompressedLength(t *testing.T) {
	var hdr bytes.Buffer
	hdr.WriteByte(hdrBinary | hdrEncrypted | hdrCompressed | hdrUseLZ4)
	var lb [2]byte
	binary.BigEndian.PutUint16(lb[:], 16) // small, otherwise-valid declared compressed length
	hdr.Write(lb[:])
	var ub [4]byte
	binary.BigEndian.PutUint32(ub[:], maxFrameSize+1)
	hdr.Write(ub[:])

	r := &failReader{t: t, data: hdr.Bytes()}
	if _, err := ReadPacket(r); err == nil {
		t.Fatal("expected ReadPacket to reject a declared zstd uncompressed length over maxFrameSize, got nil error")
	}
}

// Confirms ReadPacket's zlib branch bounds the ACTUAL post-inflate size, not
// just the on-wire (compressed) length -- a classic zip-bomb shape where a
// small compressed frame expands into something far past maxFrameSize.
// EncodePacket's zlib compressor collapses a long run of a single repeated
// byte down to a tiny frame, so this proves the guard operates on real
// inflate output rather than trusting any declared/precomputed size.
func TestReadPacketRejectsZlibBombOutput(t *testing.T) {
	// Comfortably over maxFrameSize once inflated; the repeated byte is what
	// keeps the compressed frame small and the test fast.
	plain := bytes.Repeat([]byte{0}, maxFrameSize+4096)

	packet, err := EncodePacket(plain)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	t.Logf("compressed %d plaintext bytes down to %d wire bytes", len(plain), len(packet))

	if _, err := ReadPacket(bytes.NewReader(packet)); err == nil {
		t.Fatal("expected ReadPacket to reject a zlib payload whose inflated size exceeds maxFrameSize, got nil error")
	}
}
