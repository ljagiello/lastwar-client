package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

const compressionThreshold = 1024

// maxFrameSize bounds any length-prefixed allocation in ReadPacket. The
// connection is plain TCP with no TLS, and the on-wire "encryption" is only
// a reversible length-keyed XOR (obfuscation, not confidentiality -- see
// xorCrypt), so length/uncompressedLen are effectively server/on-path
// controlled: an unbounded make([]byte, length) is a trivial multi-GB OOM
// vector. 64MiB is comfortably above the ~313KB real init payload this
// protocol has ever been observed sending.
const maxFrameSize = 64 << 20 // 64 MiB

// zstd.NewReader is expensive; the docs recommend a single shared decoder
// reused across calls. DecodeAll is safe for concurrent use.
var zstdDecoder = func() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(maxFrameSize))
	if err != nil {
		panic(err) // only fails on invalid options, never at runtime
	}
	return d
}()

const (
	hdrBinary     = 0x80
	hdrEncrypted  = 0x40
	hdrCompressed = 0x20
	hdrUseLZ4     = 0x10 // receive-side: actually Zstandard, see dossier §04
	hdrBigSized   = 0x08
	hdrForward    = 0x04
)

// xorCrypt implements the game's "encryption": the keystream is just the
// 4-byte little-endian length of the buffer itself, sent in cleartext in
// the packet header. It is applied after compression on send, and reversed
// before decompression on receive. This is obfuscation, not confidentiality
// -- see dossier §04.
func xorCrypt(data []byte) []byte {
	n := uint32(len(data))
	var key [4]byte
	binary.LittleEndian.PutUint32(key[:], n)
	out := make([]byte, len(data))
	for i := range data {
		out[i] = data[i] ^ key[i%4]
	}
	return out
}

// EncodePacket wraps a serialized SFSObject body (the outer {c,a,p}
// envelope) into the on-wire framed packet.
func EncodePacket(body []byte) ([]byte, error) {
	compressed := false
	payload := body
	if len(body) > compressionThreshold {
		var buf bytes.Buffer
		w, err := zlib.NewWriterLevel(&buf, zlib.BestCompression)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(body); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		payload = buf.Bytes()
		compressed = true
	}

	// 65535 (BitSwarmManager.WriteBinaryData / SHORT_BYTE_SIZE cutoff,
	// Smartfox2xLw.decompiled.cs:13563), matching the actual Unity SFS2X
	// SDK this game embeds -- NOT the official sfs2x-api JS SDK's 65335,
	// which is an idiosyncratic quirk of that specific (different)
	// client implementation and not what this game's server was built
	// against. Caught and reverted after a dossier audit pass flagged
	// the earlier "fix" as based on the wrong reference implementation.
	bigSized := len(payload) > 65535
	header := byte(hdrBinary | hdrEncrypted)
	if compressed {
		header |= hdrCompressed
	}
	if bigSized {
		header |= hdrBigSized
	}

	encrypted := xorCrypt(payload)

	var out bytes.Buffer
	out.WriteByte(header)
	if bigSized {
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], uint32(len(encrypted)))
		out.Write(lb[:])
	} else {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(encrypted)))
		out.Write(lb[:])
	}
	out.Write(encrypted)
	return out.Bytes(), nil
}

// deadConnError wraps an error that is (or unwraps to) io.EOF or
// io.ErrUnexpectedEOF -- i.e. the underlying reader is definitively
// closed/exhausted, not a timeout or some other transient I/O error -- so it
// additionally satisfies net.Error with Timeout()==false and
// Temporary()==false.
//
// Neither io.EOF nor io.ErrUnexpectedEOF implements net.Error, and
// fmt.Errorf's %w wrapping doesn't change that: errors.As (and the direct
// type assertions containsNonTimeoutNetError-style helpers use) only
// succeed if SOME error in the chain implements the target interface. Left
// unwrapped, a peer's graceful close (a clean FIN, or the far end simply
// exiting -- io.ReadFull surfaces this as bare io.EOF for a between-packets
// close, or io.ErrUnexpectedEOF for a mid-packet close) would silently defeat
// every "abort remaining independent work on a genuine dead connection"
// check built across rounds 16-23 (buildings.go's CollectAll, mail.go's
// ClaimAllMail, visitors.go's GreetVisitors, alliance.go's
// ClaimAllianceGifts, interactive.go's handleInteractiveLine) -- each of
// those wastes a full defaultCmdTimeout re-discovering the same dead
// connection independently instead of aborting after the first failure.
//
// Mirrors login.go's deadlineExceededError (same net.Error shape via a small
// local type) but with the opposite semantic: that one means "benign, keep
// going" (Timeout()==true), this one means "genuine dead connection, abort"
// (Timeout()==false). Wraps (never replaces) the original error via Unwrap
// so errors.Is(err, io.EOF)/errors.Is(err, io.ErrUnexpectedEOF) checks
// elsewhere (e.g. decode.go's DecodeStreamFile stream-termination check, and
// readFrameField's own mid-frame-vs-clean-EOF classification below) keep
// working unchanged straight through this wrapper.
type deadConnError struct {
	err error
}

func (e deadConnError) Error() string { return e.err.Error() }
func (e deadConnError) Unwrap() error { return e.err }
func (deadConnError) Timeout() bool   { return false }
func (deadConnError) Temporary() bool { return false }

// wrapIfClosed wraps err in deadConnError when it is, or unwraps to, io.EOF
// or io.ErrUnexpectedEOF. Any other error (including an already-net.Error
// network failure such as a connection reset, which arrives as a genuine
// *net.OpError rather than a bare io.EOF/io.ErrUnexpectedEOF) passes through
// unchanged. A nil err passes through unchanged too.
func wrapIfClosed(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return deadConnError{err: err}
	}
	return err
}

// readFrameField performs io.ReadFull for a frame field read that happens
// AFTER a packet's leading header byte has already been successfully read.
// Per io.ReadFull's documented contract, a read that consumes zero bytes
// returns bare io.EOF regardless of whether earlier io.ReadFull calls in
// this same ReadPacket invocation already consumed real frame bytes -- so a
// capture truncated exactly on a field-read boundary mid-frame (e.g. right
// after the header but before the length field) would otherwise produce the
// exact same bare io.EOF as a genuine clean end-of-stream. This helper
// converts that bare io.EOF into io.ErrUnexpectedEOF so callers such as
// DecodeStreamFile's `errors.Is(err, io.EOF)` check correctly classify only
// a genuine stream-start EOF (the header read, which does NOT use this
// helper) as clean, while any mid-frame truncation surfaces as a real error.
//
// The (possibly just-converted) error is then run through wrapIfClosed: a
// closed/exhausted reader at this point -- whether a genuine clean end (bare
// io.EOF further up the call chain, before any field of THIS frame was
// touched) or a mid-frame truncation (io.ErrUnexpectedEOF, from either the
// conversion above or a real partial io.ReadFull) -- means the connection is
// definitively dead, so the result must satisfy net.Error with
// Timeout()==false by the time it reaches a live network caller such as
// GameConn.ReadEnvelope.
func readFrameField(r io.Reader, buf []byte) error {
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return wrapIfClosed(err)
	}
	return nil
}

// ReadPacket reads one framed packet from r and returns the decoded,
// decompressed SFSObject body bytes.
func ReadPacket(r io.Reader) ([]byte, error) {
	var hb [1]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		// wrapIfClosed: a bare io.EOF here (the only shape io.ReadFull produces for a
		// zero-byte read, per its documented contract) is a genuine clean end-of-stream --
		// e.g. a peer's graceful TCP close between packets -- so it must satisfy net.Error
		// with Timeout()==false once it reaches a live network caller like
		// GameConn.ReadEnvelope, while still satisfying errors.Is(err, io.EOF) for
		// between-packets callers like decode.go's DecodeStreamFile. See deadConnError's
		// doc comment above readFrameField for the full rationale.
		return nil, fmt.Errorf("read header: %w", wrapIfClosed(err))
	}
	header := hb[0]

	if header&hdrForward != 0 {
		var sid [2]byte
		if err := readFrameField(r, sid[:]); err != nil {
			return nil, fmt.Errorf("read forward sid: %w", err)
		}
	}

	var length uint32
	if header&hdrBigSized != 0 {
		var lb [4]byte
		if err := readFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read big length: %w", err)
		}
		length = binary.BigEndian.Uint32(lb[:])
	} else {
		var lb [2]byte
		if err := readFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read length: %w", err)
		}
		length = uint32(binary.BigEndian.Uint16(lb[:]))
	}
	if length > maxFrameSize {
		return nil, fmt.Errorf("frame body too large: %d bytes (max %d)", length, maxFrameSize)
	}

	var uncompressedLen uint32
	hasZstdLen := header&hdrCompressed != 0 && header&hdrUseLZ4 != 0
	if hasZstdLen {
		var lb [4]byte
		if err := readFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read uncompressed length: %w", err)
		}
		uncompressedLen = binary.BigEndian.Uint32(lb[:])
		if uncompressedLen > maxFrameSize {
			return nil, fmt.Errorf("uncompressed length too large: %d bytes (max %d)", uncompressedLen, maxFrameSize)
		}
	}

	body := make([]byte, length)
	if err := readFrameField(r, body); err != nil {
		return nil, fmt.Errorf("read body (%d bytes): %w", length, err)
	}

	if header&hdrEncrypted != 0 {
		body = xorCrypt(body)
	}

	if header&hdrCompressed != 0 {
		if hasZstdLen {
			// The real client's own `init` bootstrap push (confirmed via
			// live packet capture against production, ~313KB uncompressed)
			// arrives exactly this way. Every "init never arrives" symptom
			// investigated earlier tonight was this: ReadPacket erroring
			// out on this exact branch, which every caller's read loop
			// silently treated as "connection closed / no more data".
			out, err := zstdDecoder.DecodeAll(body, make([]byte, 0, uncompressedLen))
			if err != nil {
				return nil, fmt.Errorf("zstd decode: %w", err)
			}
			return out, nil
		}
		zr, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("zlib reader: %w", err)
		}
		defer zr.Close()
		limited := io.LimitReader(zr, maxFrameSize+1)
		out, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %w", err)
		}
		if len(out) > maxFrameSize {
			return nil, fmt.Errorf("zlib inflated output exceeds %d bytes", maxFrameSize)
		}
		return out, nil
	}

	return body, nil
}
