package sfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/klauspost/compress/zstd"
)

const CompressionThreshold = 1024

// MaxFrameSize bounds any length-prefixed allocation in ReadPacket. The
// connection is plain TCP with no TLS, and the on-wire "encryption" is only
// a reversible length-keyed XOR (obfuscation, not confidentiality -- see
// xorCrypt), so length/uncompressedLen are effectively server/on-path
// controlled: an unbounded make([]byte, length) is a trivial multi-GB OOM
// vector. 64MiB is comfortably above the ~313KB real init payload this
// protocol has ever been observed sending.
const MaxFrameSize = 64 << 20 // 64 MiB

// zstd.NewReader is expensive; the docs recommend a single shared decoder
// reused across calls. DecodeAll is safe for concurrent use.
var zstdDecoder = func() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxFrameSize))
	if err != nil {
		panic(err) // only fails on invalid options, never at runtime
	}
	return d
}()

const (
	HdrBinary     = 0x80
	HdrEncrypted  = 0x40
	HdrCompressed = 0x20
	hdrUseLZ4     = 0x10 // receive-side: actually Zstandard, see dossier §04
	HdrBigSized   = 0x08
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
	if len(body) > CompressionThreshold {
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
	header := byte(HdrBinary | HdrEncrypted)
	if compressed {
		header |= HdrCompressed
	}
	if bigSized {
		header |= HdrBigSized
	}

	encrypted := xorCrypt(payload)

	var out bytes.Buffer
	out.WriteByte(header)
	if bigSized {
		// Round-51 fix: len(encrypted) is an int (64-bit on every platform this codebase
		// actually runs on), but the big-sized length field is a wire uint32 -- an unchecked
		// conversion would silently wrap modulo 2^32 for a payload of exactly 4GiB or more,
		// writing a length header that no longer matches the real body size written just below
		// and permanently desyncing the receiving side's frame-boundary interpretation for the
		// rest of the connection, the same failure class sfsobject.go's sibling int16Count/
		// int32Count/WriteUtfString helpers already guard against for their own wire counts.
		// Not reachable via any current call site (SendEnvelope only ever builds small
		// hand-built command envelopes), but defense-in-depth matching this codebase's existing
		// convention of erroring rather than silently wrapping an oversized wire count.
		n, err := uint32Count(len(encrypted), "encrypted payload bytes")
		if err != nil {
			return nil, err
		}
		var lb [4]byte
		binary.BigEndian.PutUint32(lb[:], n)
		out.Write(lb[:])
	} else {
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(encrypted)))
		out.Write(lb[:])
	}
	out.Write(encrypted)
	return out.Bytes(), nil
}

// uint32Count converts a length to uint32 for a wire count field, returning an error instead of
// silently wrapping into a wrong count if the value is ever too large to represent -- this file's
// own sibling of sfsobject.go's int16Count/int32Count, for EncodePacket's big-sized length field.
func uint32Count(n int, what string) (uint32, error) {
	if n > math.MaxUint32 {
		return 0, fmt.Errorf("packet: too many %s to encode (%d, max %d)", what, n, uint32(math.MaxUint32))
	}
	return uint32(n), nil
}

// DeadConnError wraps an error that is (or unwraps to) io.EOF or
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
// ReadFrameField's own mid-frame-vs-clean-EOF classification below) keep
// working unchanged straight through this wrapper.
type DeadConnError struct {
	err error
}

func (e DeadConnError) Error() string { return e.err.Error() }
func (e DeadConnError) Unwrap() error { return e.err }
func (DeadConnError) Timeout() bool   { return false }
func (DeadConnError) Temporary() bool { return false }

// NewDeadConnError wraps err as a DeadConnError so callers in other packages can construct one
// without access to the unexported err field. Used wherever the codebase's read loops decide a
// connection is genuinely dead (e.g. a run of undecodable frames) and want that classified as a
// non-timeout net.Error, the same way ReadPacket's own I/O-failure paths already are.
func NewDeadConnError(err error) DeadConnError { return DeadConnError{err: err} }

// WrapIfClosed wraps err in DeadConnError when it is, or unwraps to, io.EOF,
// io.ErrUnexpectedEOF, or io.ErrClosedPipe. Any other error (including an
// already-net.Error network failure such as a connection reset, which
// arrives as a genuine *net.OpError rather than one of those three bare
// sentinels) passes through unchanged. A nil err passes through unchanged
// too.
//
// io.ErrClosedPipe -- round-52 addition -- is what a net.Pipe (this
// codebase's own standard concurrency-test fixture, see conn_wait_test.go's
// newPipeGameConnPair) returns from a blocked Read when the SAME end that's
// blocked has Close() called on it from another goroutine, e.g.
// interactive.go's signal-handling goroutine or StartHeartbeat's own
// send-failure branch (conn.go) racing the main goroutine's blocked read. A
// real *net.TCPConn's identical scenario instead returns a *net.OpError
// wrapping net.ErrClosed, which already satisfies net.Error natively with no
// wrapping needed here -- so this addition only ever changes net.Pipe-backed
// TEST behavior, never a real production connection's (a real GameConn is
// always backed by a *net.TCPConn -- see DialGame -- so io.ErrClosedPipe can
// never actually occur there). Without it, a self-close-while-blocked-read
// test written the idiomatic way for this codebase (newPipeGameConnPair,
// close the client's own end from a background goroutine) would silently
// exercise a much slower, different code path (login.go's
// maxConsecutiveDecodeFailures give-up, ~20 failed reads) than what real TCP
// actually does (abort on the very first failed read), instead of the fast,
// single-read net.Error classification every other genuine dead-connection
// shape in this file already gets.
//
// Only used at ReadPacket's leading header-byte read below -- ReadFrameField
// (round-42 fix) now unconditionally wraps every one of its own errors in
// DeadConnError directly, since unlike the header-byte read, ANY failure
// there is fatal to frame-boundary sync, not just an EOF-shaped one.
func WrapIfClosed(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) {
		return DeadConnError{err: err}
	}
	return err
}

// ReadFrameField performs io.ReadFull for a frame field read that happens
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
// Round-42 fix: EVERY error from this function -- not just an EOF-shaped one
// -- is now unconditionally wrapped in DeadConnError, forcing
// Timeout()==false, instead of only the EOF-shaped subset going through
// WrapIfClosed while any other error (most concretely, a read DEADLINE
// expiring mid-field, e.g. login.go's waitForInitPush deliberately shortening
// its read deadline to poll for the halfway active-pull check) passed through
// unchanged as an ordinary net.Error with Timeout()==true. That used to be
// genuinely dangerous: ReadFrameField is, by this doc comment's own first
// sentence, ONLY ever called after the packet's header byte has already been
// consumed from the shared, session-long GameConn.reader -- so ANY failure
// here, for ANY reason, means real wire bytes were already irrevocably
// consumed and discarded this call, leaving the reader positioned mid-frame.
// A caller that treats a Timeout()==true result as safely retryable (the
// standard assumption throughout this codebase for the FIRST, header-byte
// read, which genuinely IS safe since it consumes nothing on failure) would
// instead read whatever partial frame content arrives next as if it were a
// fresh header byte -- permanently desyncing frame-boundary interpretation
// for every subsequent read on that connection, for the rest of the session,
// with no resync mechanism anywhere in this codebase to recover from it. Only
// the leading header-byte read in ReadPacket below (which does NOT go through
// this helper) can safely report Timeout()==true; every field read past it
// must be unconditionally fatal.
func ReadFrameField(r io.Reader, buf []byte) error {
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return DeadConnError{err: err}
	}
	return nil
}

// ReadPacket reads one framed packet from r and returns the decoded,
// decompressed SFSObject body bytes.
func ReadPacket(r io.Reader) ([]byte, error) {
	var hb [1]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		// WrapIfClosed: a bare io.EOF here (the only shape io.ReadFull produces for a
		// zero-byte read, per its documented contract) is a genuine clean end-of-stream --
		// e.g. a peer's graceful TCP close between packets -- so it must satisfy net.Error
		// with Timeout()==false once it reaches a live network caller like
		// GameConn.ReadEnvelope, while still satisfying errors.Is(err, io.EOF) for
		// between-packets callers like decode.go's DecodeStreamFile. See DeadConnError's
		// doc comment above ReadFrameField for the full rationale.
		return nil, fmt.Errorf("read header: %w", WrapIfClosed(err))
	}
	header := hb[0]

	if header&hdrForward != 0 {
		var sid [2]byte
		if err := ReadFrameField(r, sid[:]); err != nil {
			return nil, fmt.Errorf("read forward sid: %w", err)
		}
	}

	var length uint32
	if header&HdrBigSized != 0 {
		var lb [4]byte
		if err := ReadFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read big length: %w", err)
		}
		length = binary.BigEndian.Uint32(lb[:])
	} else {
		var lb [2]byte
		if err := ReadFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read length: %w", err)
		}
		length = uint32(binary.BigEndian.Uint16(lb[:]))
	}
	if length > MaxFrameSize {
		// DeadConnError (not a bare fmt.Errorf): the length field has already been
		// consumed from the shared, session-long reader, and -- unlike the body-size
		// guard below at line ~304, which fires only after ReadFrameField has already
		// consumed the full body -- this guard returns WITHOUT ever reading/draining
		// those `length` declared body bytes. If the peer actually follows an oversized
		// length header with real trailing body bytes (the natural way to send an
		// oversized frame, and exactly the threat MaxFrameSize exists to catch), those
		// bytes are left unconsumed on the wire and the next ReadPacket call
		// misinterprets the first leftover byte as a fresh header, permanently
		// desyncing frame-boundary interpretation -- the same class of bug ReadFrameField
		// was hardened against above. A bare, unwrapped error here would also fail
		// containsNonTimeoutNetError's type switch (no Unwrap), so callers like
		// buildings.go's CollectAll would not abort on it and keep issuing requests over
		// the now-desynced reader.
		return nil, DeadConnError{err: fmt.Errorf("frame body too large: %d bytes (max %d)", length, MaxFrameSize)}
	}

	var uncompressedLen uint32
	hasZstdLen := header&HdrCompressed != 0 && header&hdrUseLZ4 != 0
	if hasZstdLen {
		var lb [4]byte
		if err := ReadFrameField(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read uncompressed length: %w", err)
		}
		uncompressedLen = binary.BigEndian.Uint32(lb[:])
		if uncompressedLen > MaxFrameSize {
			// DeadConnError -- same rationale as the `length` guard above: the
			// uncompressed-length field has already been consumed, and this guard
			// returns before the `length` declared body bytes (still pending on the
			// wire) are ever read, leaving the reader mid-frame if the peer sends them.
			return nil, DeadConnError{err: fmt.Errorf("uncompressed length too large: %d bytes (max %d)", uncompressedLen, MaxFrameSize)}
		}
	}

	body := make([]byte, length)
	if err := ReadFrameField(r, body); err != nil {
		return nil, fmt.Errorf("read body (%d bytes): %w", length, err)
	}

	if header&HdrEncrypted != 0 {
		body = xorCrypt(body)
	}

	if header&HdrCompressed != 0 {
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
		limited := io.LimitReader(zr, MaxFrameSize+1)
		out, err := io.ReadAll(limited)
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %w", err)
		}
		if len(out) > MaxFrameSize {
			return nil, fmt.Errorf("zlib inflated output exceeds %d bytes", MaxFrameSize)
		}
		return out, nil
	}

	return body, nil
}
