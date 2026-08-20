package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"lastwar-client/internal/sfs"
	"log/slog"
	"os"
)

// DecodeStreamFile reads a reassembled, single-direction raw TCP byte
// stream (see docs/capturing-and-decoding-traffic.mdx) containing
// back-to-back framed SFS2X packets, and prints each one in human-readable
// form to stdout. This is the exact same sfs.ReadPacket/sfs.DecodeObject this
// client uses on its own live connection (packet.go, sfsobject.go) --
// deliberately not a separate copy, so a decoded capture can never drift
// out of sync with what this client actually implements. `label` is just
// a prefix for each printed line (e.g. "c2s"/"s2c") to tell two directions
// apart when comparing output side by side.
func DecodeStreamFile(label, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	r := bytes.NewReader(data)
	n := 0
	for {
		start := len(data) - r.Len()
		body, err := sfs.ReadPacket(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Printf("[%s] reached end of stream cleanly after %d packets (%d bytes consumed of %d)\n", label, n, start, len(data))
				break
			}
			return fmt.Errorf("stream truncated or corrupt at offset %d (remaining %d bytes): %w", start, r.Len(), err)
		}
		n++
		obj, err := sfs.DecodeObject(body)
		if err != nil {
			// Deliberately no hex/content dump of body here: a decode failure can still have an
			// intact sensitive field (e.g. "tk"/"loginKey") sitting near the front of an otherwise-
			// undecoded frame (a truncated capture, for instance), and body is the raw pre-decode
			// bytes -- there's no sfs.SFSObject to run through sfs.SensitiveSFSKeys/StringRedacted yet, so
			// any raw slice of it bypasses that redaction entirely. Print only the error itself and
			// the body's byte length (diagnostic, reveals nothing about content) -- never body's
			// bytes. See decode_test.go's TestDecodeStreamFileDoesNotLeakSensitiveFieldOnDecodeFailure.
			fmt.Printf("[%s] #%d @offset %d: DecodeObject error: %v (body %d bytes)\n", label, n, start, err, len(body))
			continue
		}
		fmt.Printf("[%s] #%d @offset %d: %s\n", label, n, start, obj.StringRedacted())
	}
	fmt.Printf("[%s] total decoded: %d packets, %d bytes consumed of %d\n", label, n, len(data)-r.Len(), len(data))
	return nil
}

func runDecode(label, path string) {
	if label == "" {
		label = "stream"
	}
	if err := DecodeStreamFile(label, path); err != nil {
		slog.Error("decode failed", "error", err)
		os.Exit(1)
	}
}
