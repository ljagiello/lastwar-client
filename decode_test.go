package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureStdout redirects os.Stdout to a pipe for the duration of fn and
// returns everything written to it. DecodeStreamFile writes directly via
// fmt.Printf rather than accepting an io.Writer, so this is the only way to
// observe its per-packet output (as opposed to just its returned error).
//
// The drain runs in a goroutine concurrently with fn, not after it: an
// os.Pipe has a small fixed kernel buffer, and DecodeStreamFile can print
// more than that fits, so a synchronous "run fn, then read" would deadlock
// with fn blocked on a full pipe and nothing reading it yet.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		io.Copy(&buf, r)
		outCh <- buf.String()
	}()

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-outCh
	r.Close()
	return out
}

// mustEncodePacket builds one on-wire framed packet from a plain key/value
// pair, matching what a real capture of a simple server push looks like.
func mustEncodePacket(t *testing.T, field, value string) []byte {
	t.Helper()
	o := NewSFSObject()
	o.PutUtfString(field, value)
	body, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}
	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	return packet
}

// mustEncodeCorruptPacket builds a packet whose framing is entirely valid
// (correct length prefix, correctly round-trips through ReadPacket) but
// whose SFSObject body is not: the leading tag byte is overwritten so
// DecodeObject's "expected top-level tag 18" check fails. This is the
// packet-framing-succeeded-but-content-is-garbage case, as distinct from a
// truncated/corrupt frame (which ReadPacket itself rejects).
func mustEncodeCorruptPacket(t *testing.T, field, value string) []byte {
	t.Helper()
	o := NewSFSObject()
	o.PutUtfString(field, value)
	body, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}
	body[0] = 0xEE // not sfsObjectType (18) -> DecodeObject must error
	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	return packet
}

// TestDecodeStreamFile exercises DecodeStreamFile's three independently
// observable branches against real EncodePacket(EncodeObject(...)) output
// written to a temp file -- no network or mocking involved, since the
// function itself only ever reads a plain []byte stream.
func TestDecodeStreamFile(t *testing.T) {
	tests := []struct {
		name string
		// build returns the raw byte stream to write to the input file.
		build func(t *testing.T) []byte
		// check inspects the returned error and captured stdout.
		check func(t *testing.T, err error, stdout string)
	}{
		{
			// All packets well-formed and fully consumed: DecodeStreamFile
			// must report the clean io.EOF case (nil error, "reached end
			// of stream cleanly" summary), not an error.
			name: "clean EOF after well-formed packets",
			build: func(t *testing.T) []byte {
				var stream []byte
				stream = append(stream, mustEncodePacket(t, "packet", "first")...)
				stream = append(stream, mustEncodePacket(t, "packet", "second")...)
				return stream
			},
			check: func(t *testing.T, err error, stdout string) {
				if err != nil {
					t.Fatalf("expected nil error for a clean stream, got: %v", err)
				}
				if !strings.Contains(stdout, "reached end of stream cleanly after 2 packets") {
					t.Errorf("stdout missing clean-EOF summary, got:\n%s", stdout)
				}
			},
		},
		{
			// Second frame is chopped mid-body: ReadPacket's io.ReadFull on
			// the body fails with io.ErrUnexpectedEOF (0 < n < requested),
			// not io.EOF, so this must surface as a non-nil error naming
			// the byte offset -- not be swallowed as a clean end of stream.
			name: "truncated frame returns byte-offset error",
			build: func(t *testing.T) []byte {
				first := mustEncodePacket(t, "packet", "first")
				second := mustEncodePacket(t, "packet", "second")
				if len(second) < 8 {
					t.Fatalf("test packet too small to truncate meaningfully: %d bytes", len(second))
				}
				truncated := second[:len(second)-3] // chop only into the body, header/length stay intact
				stream := append([]byte{}, first...)
				stream = append(stream, truncated...)
				return stream
			},
			check: func(t *testing.T, err error, stdout string) {
				if err == nil {
					t.Fatal("expected a non-nil error for a truncated frame, got nil")
				}
				first := mustEncodePacket(t, "packet", "first")
				wantOffset := fmt.Sprintf("offset %d", len(first))
				if !strings.Contains(err.Error(), wantOffset) {
					t.Errorf("error %q missing %q", err.Error(), wantOffset)
				}
				if !strings.Contains(err.Error(), "truncated or corrupt") {
					t.Errorf("error %q missing truncated/corrupt wording", err.Error())
				}
			},
		},
		{
			// Middle packet has valid framing but a corrupt SFSObject body.
			// DecodeStreamFile must print the DecodeObject error inline and
			// move on to the next packet, rather than aborting the stream.
			// A non-nil error or a missing third-packet line would mean it
			// stopped instead of continuing; asserting on captured stdout
			// (not just the returned error) is what actually proves the
			// loop reached and decoded packet #3, rather than just quietly
			// returning nil without processing the rest of the file.
			name: "DecodeObject error on one packet continues to the next",
			build: func(t *testing.T) []byte {
				var stream []byte
				stream = append(stream, mustEncodePacket(t, "packet", "first")...)
				stream = append(stream, mustEncodeCorruptPacket(t, "packet", "second")...)
				stream = append(stream, mustEncodePacket(t, "packet", "third")...)
				return stream
			},
			check: func(t *testing.T, err error, stdout string) {
				if err != nil {
					t.Fatalf("expected nil error (bad packet is logged, not fatal), got: %v", err)
				}
				if !strings.Contains(stdout, "packet=first") {
					t.Errorf("stdout missing decoded packet #1, got:\n%s", stdout)
				}
				if !strings.Contains(stdout, "DecodeObject error") {
					t.Errorf("stdout missing DecodeObject error for packet #2, got:\n%s", stdout)
				}
				if !strings.Contains(stdout, "packet=third") {
					t.Errorf("stdout missing decoded packet #3 -- stream did not continue past the bad packet, got:\n%s", stdout)
				}
				if !strings.Contains(stdout, "reached end of stream cleanly after 3 packets") {
					t.Errorf("stdout missing clean-EOF summary after all 3 packets, got:\n%s", stdout)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "stream.bin")
			if err := os.WriteFile(path, tc.build(t), 0o600); err != nil {
				t.Fatalf("write test stream: %v", err)
			}

			var decodeErr error
			stdout := captureStdout(t, func() {
				decodeErr = DecodeStreamFile("test", path)
			})
			tc.check(t, decodeErr, stdout)
		})
	}
}

// mustEncodePushAccountLoginNewPacket builds one on-wire framed packet shaped like a real
// server->client push.account.login.new (conn.go's SendEnvelope/SendExtension wire format:
// outer {c,a,p}, with the extension content's own {c,r,p} carrying the cmd name and params) whose
// params include a live loginKey, exactly the shape docs/capturing-and-decoding-traffic.mdx's
// capture-a-real-login-and-decode-it workflow would hand to -decode-stream.
func mustEncodePushAccountLoginNewPacket(t *testing.T, loginKey string) []byte {
	t.Helper()
	params := NewSFSObject()
	params.PutUtfString("gameUid", "12345678")
	params.PutUtfString("loginKey", loginKey)

	extContent := NewSFSObject()
	extContent.PutUtfString("c", "push.account.login.new")
	extContent.PutInt("r", -1)
	extContent.PutSFSObject("p", params)

	outer := NewSFSObject()
	outer.PutByte("c", controllerExtension)
	outer.PutShort("a", actionCallExtension)
	outer.PutSFSObject("p", extContent)

	body, err := EncodeObject(outer)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}
	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("EncodePacket: %v", err)
	}
	return packet
}

// TestDecodeStreamFileRedactsCredentialFields is a regression test for the finding that
// DecodeStreamFile printed obj.String() -- the raw, unredacted dump -- for every successfully
// decoded packet, so a real capture spanning login (this tool's whole documented purpose per
// docs/capturing-and-decoding-traffic.mdx) would print a live loginKey from
// push.account.login.new straight to stdout. Reverting the obj.StringRedacted() fix back to
// obj.String() would make this test fail: the raw secret would appear verbatim in stdout.
func TestDecodeStreamFileRedactsCredentialFields(t *testing.T) {
	const secretLoginKey = "SUPERSECRETLOGINKEYDONOTLEAK12345"

	dir := t.TempDir()
	path := filepath.Join(dir, "stream.bin")
	stream := mustEncodePushAccountLoginNewPacket(t, secretLoginKey)
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatalf("write test stream: %v", err)
	}

	var decodeErr error
	stdout := captureStdout(t, func() {
		decodeErr = DecodeStreamFile("test", path)
	})
	if decodeErr != nil {
		t.Fatalf("expected nil error for a well-formed stream, got: %v", decodeErr)
	}
	if strings.Contains(stdout, secretLoginKey) {
		t.Errorf("DecodeStreamFile leaked the raw loginKey in cleartext:\n%s", stdout)
	}
	// Sanity: prove the packet was actually decoded and printed (not silently skipped), so the
	// absence of the secret above reflects redaction rather than the packet never being reached.
	if !strings.Contains(stdout, "loginKey=") {
		t.Errorf("stdout missing the redacted loginKey field entirely -- packet may not have been decoded/printed at all, got:\n%s", stdout)
	}
}
