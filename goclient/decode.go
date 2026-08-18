package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
)

// DecodeStreamFile reads a reassembled, single-direction raw TCP byte
// stream (see docsite: "Capturing and decoding traffic") containing
// back-to-back framed SFS2X packets, and prints each one in human-readable
// form to stdout. This is the exact same ReadPacket/DecodeObject this
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
		body, err := ReadPacket(r)
		if err != nil {
			remaining := r.Len()
			fmt.Printf("[%s] #%d @offset %d: ReadPacket error: %v (remaining %d bytes)\n", label, n, start, err, remaining)
			break
		}
		n++
		obj, err := DecodeObject(body)
		if err != nil {
			head := body
			if len(head) > 32 {
				head = head[:32]
			}
			fmt.Printf("[%s] #%d @offset %d: DecodeObject error: %v (body %d bytes, hex head: %x)\n", label, n, start, err, len(body), head)
			continue
		}
		fmt.Printf("[%s] #%d @offset %d: %s\n", label, n, start, obj.String())
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
