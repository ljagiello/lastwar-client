package pcap

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

// ethIPv4TCP builds an Ethernet + IPv4 + TCP frame carrying payload.
func ethIPv4TCP(t *testing.T, src, dst string, sp, dp uint16, seq uint32, flags byte, payload []byte) []byte {
	t.Helper()
	sa, da := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()

	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], sp)
	binary.BigEndian.PutUint16(tcp[2:4], dp)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = 5 << 4 // data offset = 5 words (20 bytes)
	tcp[13] = flags
	copy(tcp[20:], payload)

	ip := make([]byte, 20)
	ip[0] = 0x45 // v4, IHL 5
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(tcp)))
	ip[9] = protoTCP
	copy(ip[12:16], sa[:])
	copy(ip[16:20], da[:])

	eth := make([]byte, 14)
	binary.BigEndian.PutUint16(eth[12:14], ethIPv4)

	return append(append(eth, ip...), tcp...)
}

// classicPcap wraps frames in a little-endian classic pcap file.
func classicPcap(linktype uint32, frames ...[]byte) []byte {
	var b bytes.Buffer
	hdr := make([]byte, 24)
	binary.LittleEndian.PutUint32(hdr[0:4], 0xa1b2c3d4) // stored byte-reversed -> d4c3b2a1 on disk
	binary.LittleEndian.PutUint16(hdr[4:6], 2)
	binary.LittleEndian.PutUint32(hdr[20:24], linktype)
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

// pcapngFile wraps frames in a minimal little-endian pcapng: SHB, one IDB, then
// an Enhanced Packet Block per frame.
func pcapngFile(linktype uint32, frames ...[]byte) []byte {
	le := binary.LittleEndian
	block := func(btype uint32, body []byte) []byte {
		for len(body)%4 != 0 {
			body = append(body, 0)
		}
		total := uint32(12 + len(body))
		b := make([]byte, 0, total)
		b = le.AppendUint32(b, btype)
		b = le.AppendUint32(b, total)
		b = append(b, body...)
		b = le.AppendUint32(b, total)
		return b
	}
	var out []byte
	shbBody := make([]byte, 16)
	le.PutUint32(shbBody[0:4], 0x1a2b3c4d) // byte-order magic
	le.PutUint16(shbBody[4:6], 1)          // version major
	for i := 8; i < 16; i++ {
		shbBody[i] = 0xff // section length = -1
	}
	out = append(out, block(0x0a0d0d0a, shbBody)...)
	idb := make([]byte, 8)
	le.PutUint16(idb[0:2], uint16(linktype))
	out = append(out, block(0x00000001, idb)...)
	for _, f := range frames {
		epb := make([]byte, 20+len(f))
		le.PutUint32(epb[12:16], uint32(len(f))) // captured len
		le.PutUint32(epb[16:20], uint32(len(f))) // original len
		copy(epb[20:], f)
		out = append(out, block(0x00000006, epb)...)
	}
	return out
}

const (
	tcpSYN = 0x02
	tcpACK = 0x10
)

func TestParseClassicAndReassemble(t *testing.T) {
	client, server := "192.168.1.5", "203.0.113.9"
	frames := [][]byte{
		// client opens the connection (SYN, no payload) at ISN 1000
		ethIPv4TCP(t, client, server, 5555, 17783, 1000, tcpSYN, nil),
		// server->client: "hello" then "world", but sent OUT OF ORDER
		ethIPv4TCP(t, server, client, 17783, 5555, 2005, tcpACK, []byte("world")), // second, arrives first
		ethIPv4TCP(t, server, client, 17783, 5555, 2000, tcpACK, []byte("hello")), // first, arrives second
		// a full RETRANSMIT of "hello" (same seq) -- must not duplicate
		ethIPv4TCP(t, server, client, 17783, 5555, 2000, tcpACK, []byte("hello")),
		// client->server payload
		ethIPv4TCP(t, client, server, 5555, 17783, 1001, tcpACK, []byte("ping")),
	}
	pkts, err := Parse(classicPcap(linkEthernet, frames...))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	convs := Conversations(pkts)
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convs))
	}
	c := convs[0]
	if c.TLS {
		t.Errorf("plain SFS2X-style stream wrongly flagged TLS")
	}
	cl, err := c.Client(netip.Addr{})
	if err != nil {
		t.Fatalf("Client infer: %v", err)
	}
	if cl.String() != client {
		t.Errorf("inferred client = %s, want %s (the SYN sender)", cl, client)
	}
	c2s, s2c := c.Reassemble(cl)
	if string(c2s) != "ping" {
		t.Errorf("c2s = %q, want %q", c2s, "ping")
	}
	if string(s2c) != "helloworld" {
		t.Errorf("s2c = %q, want %q (out-of-order + retransmit must reassemble in seq order without duplication)", s2c, "helloworld")
	}
}

func TestParsePCAPNG(t *testing.T) {
	client, server := "10.0.0.2", "198.51.100.7"
	frames := [][]byte{
		ethIPv4TCP(t, server, client, 17783, 4444, 500, tcpACK, []byte("AB")),
		ethIPv4TCP(t, server, client, 17783, 4444, 502, tcpACK, []byte("CD")),
	}
	pkts, err := Parse(pcapngFile(linkEthernet, frames...))
	if err != nil {
		t.Fatalf("Parse pcapng: %v", err)
	}
	convs := Conversations(pkts)
	if len(convs) != 1 {
		t.Fatalf("got %d conversations, want 1", len(convs))
	}
	_, s2c := convs[0].Reassemble(netip.MustParseAddr(client))
	if string(s2c) != "ABCD" {
		t.Errorf("pcapng s2c = %q, want %q", s2c, "ABCD")
	}
}

func TestTLSDetectionAndOrdering(t *testing.T) {
	// One TLS stream (starts 0x16 0x03) and one plain stream; plain must sort first.
	tlsHello := []byte{0x16, 0x03, 0x01, 0x00, 0x10}
	frames := [][]byte{
		ethIPv4TCP(t, "192.168.1.5", "203.0.113.1", 5000, 443, 10, tcpACK, tlsHello),
		ethIPv4TCP(t, "192.168.1.5", "203.0.113.2", 5001, 17783, 20, tcpACK, []byte{0x80, 0x00, 0x01}),
	}
	pkts, _ := Parse(classicPcap(linkEthernet, frames...))
	convs := Conversations(pkts)
	if len(convs) != 2 {
		t.Fatalf("got %d conversations, want 2", len(convs))
	}
	if convs[0].TLS {
		t.Errorf("idx 0 should be the plain stream, got TLS")
	}
	if !convs[1].TLS {
		t.Errorf("idx 1 should be the TLS stream")
	}
}

func TestParseRejectsUnknownMagic(t *testing.T) {
	if _, err := Parse([]byte{0xde, 0xad, 0xbe, 0xef, 0, 0, 0, 0}); err == nil {
		t.Error("expected an error for an unrecognized file magic")
	}
}
