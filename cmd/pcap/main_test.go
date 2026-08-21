package main

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"lastwar-client/internal/sfs"
)

// --- minimal classic-pcap / Ethernet-IPv4-TCP builders (package main can't see
// internal/pcap's test helpers) ---

func ethV4TCP(t *testing.T, src, dst string, sp, dp uint16, seq uint32, flags byte, payload []byte) []byte {
	t.Helper()
	sa := netip.MustParseAddr(src).As4()
	da := netip.MustParseAddr(dst).As4()
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], sp)
	binary.BigEndian.PutUint16(tcp[2:4], dp)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = 5 << 4
	tcp[13] = flags
	copy(tcp[20:], payload)
	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(tcp)))
	ip[9] = 6 // TCP
	copy(ip[12:16], sa[:])
	copy(ip[16:20], da[:])
	eth := make([]byte, 14)
	binary.BigEndian.PutUint16(eth[12:14], 0x0800)
	return append(append(eth, ip...), tcp...)
}

func classicPcap(t *testing.T, frames ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:4], 0xa1b2c3d4)
	binary.LittleEndian.PutUint16(hdr[4:6], 2)
	binary.LittleEndian.PutUint32(hdr[20:24], 1) // Ethernet
	b.Write(hdr)
	for _, f := range frames {
		rec := make([]byte, 16)
		binary.LittleEndian.PutUint32(rec[8:12], uint32(len(f)))
		binary.LittleEndian.PutUint32(rec[12:16], uint32(len(f)))
		b.Write(rec)
		b.Write(f)
	}
	return b.Bytes()
}

// synthCapture writes a temp pcap with a client SYN then one client->server SFS
// packet, and returns its path.
func synthCapture(t *testing.T) string {
	t.Helper()
	obj := sfs.NewSFSObject()
	obj.PutByte("c", 1)
	obj.PutInt("a", 13)
	body, err := sfs.EncodeObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	framed, err := sfs.EncodePacket(body)
	if err != nil {
		t.Fatal(err)
	}
	frames := [][]byte{
		ethV4TCP(t, "192.168.1.9", "203.0.113.5", 5000, 17783, 1000, 0x02, nil),    // SYN
		ethV4TCP(t, "192.168.1.9", "203.0.113.5", 5000, 17783, 1001, 0x10, framed), // data
	}
	path := filepath.Join(t.TempDir(), "cap.pcap")
	if err := os.WriteFile(path, classicPcap(t, frames...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// quiet runs fn with os.Stdout redirected to a temp file so the tool's output
// doesn't clutter the test log.
func quiet(t *testing.T, fn func()) {
	t.Helper()
	orig := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	defer func() { os.Stdout = orig; _ = f.Close() }()
	fn()
}

func TestRunListReassembleDecode(t *testing.T) {
	in := synthCapture(t)

	// -list
	quiet(t, func() {
		if err := run(in, true, -1, "", "", false); err != nil {
			t.Errorf("-list: %v", err)
		}
	})

	// reassemble to .bin files (stream 0 is the only conversation)
	prefix := filepath.Join(t.TempDir(), "s")
	quiet(t, func() {
		if err := run(in, false, 0, "", prefix, false); err != nil {
			t.Errorf("-out: %v", err)
		}
	})
	for _, suffix := range []string{"_c2s.bin", "_s2c.bin"} {
		if _, err := os.Stat(prefix + suffix); err != nil {
			t.Errorf("expected %s to be written: %v", prefix+suffix, err)
		}
	}
	// the c2s side must contain the SFS packet we planted (non-empty)
	if b, _ := os.ReadFile(prefix + "_c2s.bin"); len(b) == 0 {
		t.Error("c2s .bin is empty; expected the planted packet")
	}

	// inline decode (with explicit client)
	quiet(t, func() {
		if err := run(in, false, 0, "192.168.1.9", "", true); err != nil {
			t.Errorf("-decode: %v", err)
		}
	})
}

func TestRunErrors(t *testing.T) {
	in := synthCapture(t)
	cases := []struct {
		name               string
		inArg, client, out string
		list, decode       bool
		stream             int
	}{
		{name: "missing-in", stream: 0},
		{name: "bad-stream-index", inArg: in, stream: 99},
		{name: "no-out-no-decode", inArg: in, stream: 0},
		{name: "bad-client", inArg: in, stream: 0, client: "not-an-ip", decode: true},
		{name: "unreadable-file", inArg: filepath.Join(t.TempDir(), "nope.pcap"), stream: 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			quiet(t, func() {
				if err := run(c.inArg, c.list, c.stream, c.client, c.out, c.decode); err == nil {
					t.Errorf("%s: expected an error, got nil", c.name)
				}
			})
		})
	}
}
