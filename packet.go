package main

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
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
	d, err := zstd.NewReader(nil)
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

// ReadPacket reads one framed packet from r and returns the decoded,
// decompressed SFSObject body bytes.
func ReadPacket(r io.Reader) ([]byte, error) {
	var hb [1]byte
	if _, err := io.ReadFull(r, hb[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	header := hb[0]

	if header&hdrForward != 0 {
		var sid [2]byte
		if _, err := io.ReadFull(r, sid[:]); err != nil {
			return nil, fmt.Errorf("read forward sid: %w", err)
		}
	}

	var length uint32
	if header&hdrBigSized != 0 {
		var lb [4]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read big length: %w", err)
		}
		length = binary.BigEndian.Uint32(lb[:])
	} else {
		var lb [2]byte
		if _, err := io.ReadFull(r, lb[:]); err != nil {
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
		if _, err := io.ReadFull(r, lb[:]); err != nil {
			return nil, fmt.Errorf("read uncompressed length: %w", err)
		}
		uncompressedLen = binary.BigEndian.Uint32(lb[:])
		if uncompressedLen > maxFrameSize {
			return nil, fmt.Errorf("uncompressed length too large: %d bytes (max %d)", uncompressedLen, maxFrameSize)
		}
	}

	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
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
		out, err := io.ReadAll(zr)
		if err != nil {
			return nil, fmt.Errorf("zlib inflate: %w", err)
		}
		return out, nil
	}

	return body, nil
}
