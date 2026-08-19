package main

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// Confirms the >65535-byte payload framing branch (4-byte length prefix, hdrBigSized) round-trips
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
	if packet[0]&hdrBigSized == 0 {
		t.Fatalf("expected hdrBigSized to be set for a %d-byte body, header=%08b", len(body), packet[0])
	}

	got, err := ReadPacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d bytes", len(got), len(body))
	}
}
