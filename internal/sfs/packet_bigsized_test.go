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

// TestEncodePacketBigSizedThresholdExactBoundary pins the 4-byte length cut on the uncompressed
// body, where the client decides it (NetPacket.onBeforeSend, BaseUtils.decompiled.cs:33546-33556):
// a 65535-byte body keeps the 2-byte length and one byte more takes the 4-byte one, however small
// zlib makes the payload. Go used to decide on the compressed length, so a compressible body over
// 65535 bytes went out with a 2-byte length the client would not have sent (B70).
func TestEncodePacketBigSizedThresholdExactBoundary(t *testing.T) {
	tests := []struct {
		name        string
		bodyLen     int
		wantBigSize bool
	}{
		// Literals (byteArray.Length > 65535, BaseUtils.decompiled.cs:33552-33555), so a wrong
		// shortLengthMax fails here.
		{"exactly at cap", 65535, false},
		{"one byte over cap", 65536, true},
		{"compresses far below the cap", 70000, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := make([]byte, tt.bodyLen) // all zeros: zlib shrinks it to a few hundred bytes

			packet, err := EncodePacket(body)
			if err != nil {
				t.Fatalf("EncodePacket: %v", err)
			}
			gotBigSize := packet[0]&HdrBigSized != 0
			if gotBigSize != tt.wantBigSize {
				t.Fatalf("HdrBigSized set = %v, want %v for a %d-byte body compressed to %d bytes (header=%08b)",
					gotBigSize, tt.wantBigSize, tt.bodyLen, packetPayloadLen(packet), packet[0])
			}
			if n := packetPayloadLen(packet); n != len(packet)-packetHeaderLen(packet) {
				t.Fatalf("length field = %d, want the compressed payload's %d bytes", n, len(packet)-packetHeaderLen(packet))
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

// TestEncodePacketRefusesShortLengthOverflow: an incompressible 65535-byte body keeps the 2-byte
// length, but zlib's stored blocks make its payload a few bytes longer than 65535. The client's
// Convert.ToUInt16 throws there (BaseUtils.decompiled.cs:33574); Go must error rather than wrap
// the length and desync the stream.
func TestEncodePacketRefusesShortLengthOverflow(t *testing.T) {
	body := make([]byte, shortLengthMax)
	if _, err := mathrand.New(mathrand.NewSource(42)).Read(body); err != nil {
		t.Fatalf("rng.Read: %v", err)
	}
	if packet, err := EncodePacket(body); err == nil {
		t.Fatalf("EncodePacket = %d-byte frame with a %d-byte length field, want an error", len(packet), packetPayloadLen(packet))
	}
}

// packetPayloadLen reads the payload length field back out of an EncodePacket frame: a header
// byte, then the payload length as a big-endian uint16, or a uint32 when HdrBigSized is set.
// (xorCrypt is length-preserving, so this is the compressed payload's length.)
func packetPayloadLen(packet []byte) int {
	if packet[0]&HdrBigSized != 0 {
		return int(binary.BigEndian.Uint32(packet[1:5]))
	}
	return int(binary.BigEndian.Uint16(packet[1:3]))
}

// packetHeaderLen is the header byte plus the length field of an EncodePacket frame.
func packetHeaderLen(packet []byte) int {
	if packet[0]&HdrBigSized != 0 {
		return 5
	}
	return 3
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
