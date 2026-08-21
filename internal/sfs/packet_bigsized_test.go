package sfs

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"math"
	mathrand "math/rand"
	"testing"
)

// Confirms the >65535-byte payload framing branch (4-byte length prefix, HdrBigSized) round-trips
// correctly on both encode and decode -- this exact constant was gotten wrong once before (see
// the comment in EncodePacket) and had zero test coverage.
func TestPacketRoundTripBigSized(t *testing.T) {
	// Random bytes are incompressible, so this reliably stays over 65535 bytes even after
	// zlib "compression" (which will slightly expand incompressible input), forcing the
	// bigSized path on both sides.
	body := make([]byte, 70000)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand.Read: %v", err)
	}

	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	if packet[0]&HdrBigSized == 0 {
		t.Fatalf("expected HdrBigSized to be set for a %d-byte body, header=%08b", len(body), packet[0])
	}

	got, err := ReadPacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(body))
	}
}

// TestEncodePacketBigSizedThresholdExactBoundary is the round-45 regression test for the MINOR
// finding that EncodePacket's bigSized framing threshold (packet.go: `bigSized := len(payload) >
// 65535`) had no exact-boundary test -- TestPacketRoundTripBigSized above only proves a
// comfortably-over-cap payload (70000 bytes) selects the 4-byte-length framing, never that a
// payload of exactly 65535 (post-compression) bytes still selects the 2-byte framing, nor that
// exactly one byte more flips it. This constant has a documented history of being gotten wrong
// once already (see the comment above bigSized's declaration in packet.go), underscoring the
// value of pinning down its exact boundary.
//
// EncodePacket unconditionally zlib-compresses any body over CompressionThreshold (1024 bytes),
// and compressed output size is data-dependent, so a body length can't directly control the
// post-compression payload length that bigSized actually checks. Random (incompressible) input
// data makes zlib's BestCompression output track input length almost exactly linearly (DEFLATE
// falls back to near-stored blocks for data it can't compress), so bodyForCompressedPayload below
// derives, at run time, the input length whose compressed payload lands on an exact target. That
// derivation is deliberately dynamic rather than a hardcoded constant: compress/flate's exact
// output size drifts across Go toolchains (go1.27's flate is ~13 bytes tighter on this input than
// the build this test was first written against), so a pinned input length silently stops testing
// the boundary the moment flate changes. Deriving it keeps the test anchored to the *compressed*
// size the bigSized branch actually switches on.
func TestEncodePacketBigSizedThresholdExactBoundary(t *testing.T) {
	const payloadCap = 65535 // SHORT_BYTE_SIZE cutoff; see the bigSized comment in EncodePacket.
	tests := []struct {
		name        string
		wantPayload int
		wantBigSize bool
	}{
		{"exactly at cap", payloadCap, false},
		{"one byte over cap", payloadCap + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bodyForCompressedPayload(t, tt.wantPayload)

			packet, err := EncodePacket(body)
			if err != nil {
				t.Fatalf("EncodePacket: %v", err)
			}

			if gotPayload := packetPayloadLen(packet); gotPayload != tt.wantPayload {
				t.Fatalf("compressed payload = %d, want %d (derivation is stale)", gotPayload, tt.wantPayload)
			}
			gotBigSize := packet[0]&HdrBigSized != 0
			if gotBigSize != tt.wantBigSize {
				t.Fatalf("HdrBigSized set = %v, want %v (header=%08b)", gotBigSize, tt.wantBigSize, packet[0])
			}

			got, err := ReadPacket(bytes.NewReader(packet))
			if err != nil {
				t.Fatalf("ReadPacket: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(body))
			}
		})
	}
}

// packetPayloadLen reads the post-compression payload length back out of an EncodePacket frame:
// a header byte, then the payload length as a big-endian uint16, or a uint32 when HdrBigSized is
// set. (xorCrypt is length-preserving, so the encrypted-payload length this reads equals the
// compressed-payload length bigSized switches on.)
func packetPayloadLen(packet []byte) int {
	if packet[0]&HdrBigSized != 0 {
		return int(binary.BigEndian.Uint32(packet[1:5]))
	}
	return int(binary.BigEndian.Uint16(packet[1:3]))
}

// bodyForCompressedPayload returns an incompressible (fixed-seed random) body whose EncodePacket
// compressed payload length is exactly want, by scanning input lengths just below want. Because
// incompressible data can't be shrunk below its own size, the compressed payload always exceeds
// the input by a small constant framing overhead, so the matching input is a little under want; a
// short downward scan reliably finds it regardless of the exact overhead a given flate build adds.
// Re-seeding per candidate keeps each body deterministic (a fixed seed's byte stream is a prefix
// chain, so length n is a prefix of length n+1).
func bodyForCompressedPayload(t *testing.T, want int) []byte {
	t.Helper()
	for n := want; n >= want-256 && n >= 0; n-- {
		rng := mathrand.New(mathrand.NewSource(42))
		body := make([]byte, n)
		if _, err := rng.Read(body); err != nil {
			t.Fatalf("rng.Read: %v", err)
		}
		packet, err := EncodePacket(body)
		if err != nil {
			t.Fatalf("EncodePacket: %v", err)
		}
		if packetPayloadLen(packet) == want {
			return body
		}
	}
	t.Fatalf("no input length in [%d,%d] produced a compressed payload of exactly %d bytes", want-256, want, want)
	return nil
}

// TestEncodePacketCompressionThresholdExactBoundary is the round-46 regression test for the MINOR
// finding that EncodePacket's compression threshold (packet.go: `if len(body) > CompressionThreshold`,
// CompressionThreshold == 1024) had no exact-boundary test. Unlike bigSized above (which checks the
// POST-compression payload length, requiring empirical derivation of an input that lands on a
// specific compressed size), this guard checks the body's PRE-compression length directly, so the
// boundary is pinned by construction: a 1024-byte body must stay uncompressed (HdrCompressed clear,
// payload passed through verbatim) and a 1025-byte body must be compressed (HdrCompressed set).
func TestEncodePacketCompressionThresholdExactBoundary(t *testing.T) {
	tests := []struct {
		name           string
		bodyLen        int
		wantCompressed bool
	}{
		{"exactly at threshold: uncompressed", CompressionThreshold, false},
		{"one byte over threshold: compressed", CompressionThreshold + 1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := make([]byte, tt.bodyLen)
			if _, err := rand.Read(body); err != nil {
				t.Fatalf("rand.Read: %v", err)
			}

			packet, err := EncodePacket(body)
			if err != nil {
				t.Fatalf("EncodePacket: %v", err)
			}

			gotCompressed := packet[0]&HdrCompressed != 0
			if gotCompressed != tt.wantCompressed {
				t.Fatalf("HdrCompressed set = %v, want %v (header=%08b) for a %d-byte body", gotCompressed, tt.wantCompressed, packet[0], tt.bodyLen)
			}

			got, err := ReadPacket(bytes.NewReader(packet))
			if err != nil {
				t.Fatalf("ReadPacket: %v", err)
			}
			if !bytes.Equal(got, body) {
				t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(body))
			}
		})
	}
}

// TestUint32CountExactBoundary is the round-51 regression test for the MINOR finding that
// EncodePacket's big-sized length field used an unchecked uint32(len(encrypted)) conversion, which
// would silently wrap modulo 2^32 for a payload of exactly 4GiB or more instead of erroring --
// mirrors sfsobject_encode_error_test.go's TestInt32CountExactBoundary technique exactly: a true
// end-to-end EncodePacket test would need a >4GiB payload to actually drive the wire count past
// math.MaxUint32, impractically expensive to construct and run, so uint32Count (packet.go, this
// check's own extracted helper, mirroring sfsobject.go's int16Count/int32Count) is called directly
// here instead.
func TestUint32CountExactBoundary(t *testing.T) {
	t.Run("exactly math.MaxUint32: accepted", func(t *testing.T) {
		got, err := uint32Count(math.MaxUint32, "x")
		if err != nil {
			t.Fatalf("uint32Count(math.MaxUint32, ...) error = %v, want nil", err)
		}
		if got != math.MaxUint32 {
			t.Errorf("got %d, want %d", got, uint32(math.MaxUint32))
		}
	})

	t.Run("math.MaxUint32 plus one: rejected", func(t *testing.T) {
		_, err := uint32Count(math.MaxUint32+1, "x")
		if err == nil {
			t.Fatal("uint32Count(math.MaxUint32+1, ...) error = nil, want an overflow error")
		}
	})
}
