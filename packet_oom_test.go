package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
)

// midFrameTimeoutReader serves exactly one header byte successfully, then fails every subsequent
// Read with a genuine (non-EOF-shaped) timeout net.Error -- simulating a read deadline that
// expires mid-frame, after the header byte has already been consumed but before the next field
// (e.g. the length field) arrives. This is exactly the shape login.go's waitForInitPush produces
// when it deliberately shortens conn.conn's read deadline to the halfway point to poll for its
// active-pull fallback: a totally ordinary, otherwise-successful read gets interrupted partway
// through a frame by that artificially-shortened deadline.
type midFrameTimeoutReader struct {
	calls int
}

func (r *midFrameTimeoutReader) Read(p []byte) (int, error) {
	r.calls++
	if r.calls == 1 {
		p[0] = 0x00 // header byte: no forward/bigSized/compressed/encrypted flags set
		return 1, nil
	}
	return 0, fakeTimeoutNetError{msg: "simulated mid-frame deadline exceeded"}
}

// TestReadFrameFieldMidFrameTimeoutIsNonTimeoutNetError is the round-42 regression test for the
// MAJOR finding that a read-deadline timeout expiring mid-frame (after the header byte was
// already consumed, but before a later field like the length field arrived) used to be reported
// as an ORDINARY Timeout()==true net.Error, identical to the genuinely-safe-to-retry case of the
// timeout landing on the very first (header) byte, where nothing has been consumed yet. A caller
// that retries on Timeout()==true (e.g. login.go's waitForInitPush, which does exactly this for
// its own deliberately-shortened halfway-poll deadline) would then read whatever partial frame
// content arrives next as if it were a fresh header byte, permanently desyncing frame-boundary
// interpretation on the shared, session-long GameConn.reader for the rest of the session -- with
// no resync mechanism anywhere in this codebase. Proves ReadPacket now returns an error that is a
// net.Error with Timeout()==false for this exact scenario, indistinguishable in severity from a
// genuine dead connection, not a benign retryable timeout.
func TestReadFrameFieldMidFrameTimeoutIsNonTimeoutNetError(t *testing.T) {
	_, err := ReadPacket(&midFrameTimeoutReader{})
	if err == nil {
		t.Fatal("ReadPacket: expected an error for a timeout expiring mid-frame, got nil")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("ReadPacket error = %v (%T), want it to satisfy net.Error", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a timeout expiring mid-frame (after the header byte was already consumed) must be indistinguishable in severity from a genuine dead connection, not treated as a benign, safely-retryable timeout the way a timeout on the still-untouched leading header byte would be")
	}
}

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

// Confirms ReadPacket does not misclassify a mid-frame truncation as a clean
// end-of-stream. Per io.ReadFull's documented contract, a read that consumes
// zero bytes returns bare io.EOF regardless of whether earlier io.ReadFull
// calls in that same ReadPacket invocation already consumed real frame
// bytes -- so a capture cut off exactly on a field-read boundary mid-frame
// (here: right after a would-be second frame's header byte, but before its
// length field) produces the exact same bare io.EOF as a genuine clean
// end-of-stream, even though a real header byte was already consumed. This
// is the truncation shape tools/reassemble_stream.py's documented output
// (a live TCP stream cut off at some arbitrary point mid-capture) actually
// produces. Without the fix in ReadPacket, decode.go's DecodeStreamFile
// would see this bare io.EOF, its `errors.Is(err, io.EOF)` check would
// match, and it would wrongly report "reached end of stream cleanly" for a
// truncated/corrupt capture.
func TestReadPacketMidFrameTruncationIsNotClassifiedAsCleanEOF(t *testing.T) {
	valid, err := EncodePacket([]byte("hello"))
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}

	// Append just a bare header byte for a would-be second frame -- no
	// forward/bigSized/compressed flags set -- so the stream ends exactly
	// on the real protocol boundary between the header field (already
	// fully read) and the 2-byte length field that would come next, rather
	// than at an arbitrary byte count.
	truncated := append(append([]byte{}, valid...), hdrBinary|hdrEncrypted)
	r := bytes.NewReader(truncated)

	// First read recovers the genuine, complete first frame.
	body, err := ReadPacket(r)
	if err != nil {
		t.Fatalf("ReadPacket (first frame): unexpected error: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("ReadPacket (first frame): got body %q, want %q", body, "hello")
	}

	// Second read only has the lone header byte of a truncated frame left
	// -- it must NOT be reported as a clean end-of-stream.
	if _, err := ReadPacket(r); err == nil {
		t.Fatal("ReadPacket (truncated second frame): expected an error, got nil")
	} else if errors.Is(err, io.EOF) {
		t.Fatalf("ReadPacket (truncated second frame): error satisfies errors.Is(err, io.EOF) -- a mid-frame truncation was misclassified as a clean end-of-stream: %v", err)
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadPacket (truncated second frame): expected errors.Is(err, io.ErrUnexpectedEOF), got: %v", err)
	}
}
