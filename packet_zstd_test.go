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

// Confirms ReadPacket's zstd branch bounds the ACTUAL decoded size, not just
// the declared uncompressedLen header field -- the zstd counterpart to
// TestReadPacketRejectsZlibBombOutput's zip-bomb shape. The declared
// uncompressedLen is a lie (well under maxFrameSize), so the early
// header-field guard (see TestReadPacketRejectsOversizedZstdUncompressedLength
// in packet_oom_test.go) never fires; the wire payload is real zstd output
// -- via the same klauspost/compress/zstd encoder used above -- of a run of
// a single repeated byte far past maxFrameSize once decoded. Only the shared
// zstdDecoder's WithDecoderMaxMemory(maxFrameSize) limit stands between this
// and an unbounded allocation, so this proves that limit actually holds on
// real decoder output rather than trusting the (attacker-controlled) header
// hint.
func TestReadPacketRejectsZstdBombOutput(t *testing.T) {
	// Comfortably over maxFrameSize once decoded; the repeated byte is what
	// keeps the compressed frame small and the test fast.
	plain := bytes.Repeat([]byte{0}, maxFrameSize+4096)

	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd writer: %v", err)
	}
	compressed := enc.EncodeAll(plain, nil)
	enc.Close()
	t.Logf("compressed %d plaintext bytes down to %d wire bytes", len(plain), len(compressed))

	encrypted := xorCrypt(compressed)

	// Mirror EncodePacket's own bigSized-vs-short framing choice rather than
	// assuming the compressed size fits in a uint16 -- it comfortably does
	// for this all-zero input, but the test shouldn't silently depend on it.
	bigSized := len(encrypted) > 65535
	header := byte(hdrBinary | hdrEncrypted | hdrCompressed | hdrUseLZ4)
	if bigSized {
		header |= hdrBigSized
	}

	var buf bytes.Buffer
	buf.WriteByte(header)
	if bigSized {
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(encrypted)))
		buf.Write(lb[:])
	} else {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(encrypted)))
		buf.Write(lb[:])
	}
	var ub [4]byte
	// The lie: a small, otherwise-plausible declared uncompressed length so
	// ReadPacket's cheap header-field check passes and actual decompression
	// is attempted.
	binary.BigEndian.PutUint32(ub[:], 16)
	buf.Write(ub[:])
	buf.Write(encrypted)

	if _, err := ReadPacket(&buf); err == nil {
		t.Fatal("expected ReadPacket to reject a zstd payload whose decoded size exceeds maxFrameSize, got nil error")
	}
}

// TestReadPacketAcceptsZstdUncompressedLengthExactlyAtCap is the round-44 regression test for the
// MINOR finding that packet_oom_test.go's TestReadPacketRejectsOversizedZstdUncompressedLength only
// proves packet.go's `uncompressedLen > maxFrameSize` guard rejects at maxFrameSize+1 -- it never
// exercises the boundary itself, so an off-by-one `>=` tightening would silently reject a
// legitimate frame declaring exactly maxFrameSize with zero test signal (the identical gap round 43
// closed for maxGSLResponseSize/maxNestDepth elsewhere in this codebase). The guard only inspects
// the DECLARED uncompressedLen header field, not the real decompressed size, so this cheaply proves
// the boundary accepts using a small actual zstd payload (mirroring TestReadPacketZstdBranch above)
// with the header's uncompressedLen field set to exactly maxFrameSize instead of the payload's real
// length -- no 64MiB of real data needs to change hands to exercise this specific guard.
func TestReadPacketAcceptsZstdUncompressedLengthExactlyAtCap(t *testing.T) {
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
	// Exactly at the cap, not the real decompressed length -- the boundary value itself.
	binary.BigEndian.PutUint32(ub[:], maxFrameSize)
	buf.Write(ub[:])
	buf.Write(encrypted)

	got, err := ReadPacket(&buf)
	if err != nil {
		t.Fatalf("ReadPacket() error = %v, want nil for uncompressedLen declared as exactly maxFrameSize (the boundary value, not over the cap)", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("ReadPacket() = %q, want %q", got, original)
	}
}
