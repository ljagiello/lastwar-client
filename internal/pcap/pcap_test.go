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

// --- helpers for the remaining link types / IP versions ---

func makeTCP(sp, dp uint16, seq uint32, flags byte, payload []byte) []byte {
	tcp := make([]byte, 20+len(payload))
	binary.BigEndian.PutUint16(tcp[0:2], sp)
	binary.BigEndian.PutUint16(tcp[2:4], dp)
	binary.BigEndian.PutUint32(tcp[4:8], seq)
	tcp[12] = 5 << 4
	tcp[13] = flags
	copy(tcp[20:], payload)
	return tcp
}

func ipv4TCP(t *testing.T, src, dst string, sp, dp uint16, seq uint32, flags byte, payload []byte) []byte {
	t.Helper()
	sa, da := netip.MustParseAddr(src).As4(), netip.MustParseAddr(dst).As4()
	tcp := makeTCP(sp, dp, seq, flags, payload)
	ip := make([]byte, 20)
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:4], uint16(20+len(tcp)))
	ip[9] = protoTCP
	copy(ip[12:16], sa[:])
	copy(ip[16:20], da[:])
	return append(ip, tcp...)
}

// ipv6TCP builds an IPv6 packet with a TCP segment. If withHopByHop, a
// hop-by-hop extension header is inserted before TCP (exercising the extension
// walk).
func ipv6TCP(t *testing.T, src, dst string, sp, dp uint16, seq uint32, flags byte, withHopByHop bool, payload []byte) []byte {
	t.Helper()
	sa, da := netip.MustParseAddr(src).As16(), netip.MustParseAddr(dst).As16()
	tcp := makeTCP(sp, dp, seq, flags, payload)
	body := tcp
	next := byte(protoTCP)
	if withHopByHop {
		hbh := make([]byte, 8) // one 8-byte hop-by-hop options header
		hbh[0] = protoTCP      // next header
		hbh[1] = 0             // (0+1)*8 = 8 bytes
		body = append(hbh, tcp...)
		next = 0 // hop-by-hop
	}
	ip := make([]byte, 40)
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(body)))
	ip[6] = next
	ip[7] = 64
	copy(ip[8:24], sa[:])
	copy(ip[24:40], da[:])
	return append(ip, body...)
}

func eth(et uint16, ip []byte) []byte {
	f := make([]byte, 14)
	binary.BigEndian.PutUint16(f[12:14], et)
	return append(f, ip...)
}
func vlanEth(et uint16, ip []byte) []byte {
	f := make([]byte, 18)
	binary.BigEndian.PutUint16(f[12:14], ethVLAN)
	binary.BigEndian.PutUint16(f[16:18], et)
	return append(f, ip...)
}
func sll(et uint16, ip []byte) []byte {
	f := make([]byte, 16)
	binary.BigEndian.PutUint16(f[14:16], et)
	return append(f, ip...)
}
func sll2(et uint16, ip []byte) []byte {
	f := make([]byte, 20)
	binary.BigEndian.PutUint16(f[0:2], et)
	return append(f, ip...)
}
func nullFrame(ip []byte) []byte { return append(make([]byte, 4), ip...) }

func classicPcapBE(linktype uint32, frames ...[]byte) []byte {
	var b bytes.Buffer
	hdr := make([]byte, 24)
	binary.BigEndian.PutUint32(hdr[0:4], 0xa1b2c3d4) // stored big-endian
	binary.BigEndian.PutUint16(hdr[4:6], 2)
	binary.BigEndian.PutUint32(hdr[20:24], linktype)
	b.Write(hdr)
	for _, f := range frames {
		rec := make([]byte, 16)
		binary.BigEndian.PutUint32(rec[8:12], uint32(len(f)))
		binary.BigEndian.PutUint32(rec[12:16], uint32(len(f)))
		b.Write(rec)
		b.Write(f)
	}
	return b.Bytes()
}

// onePayload parses a single-frame capture and returns the lone packet's payload.
func onePayload(t *testing.T, data []byte) Packet {
	t.Helper()
	pkts, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(pkts) != 1 {
		t.Fatalf("got %d packets, want 1", len(pkts))
	}
	return pkts[0]
}

func TestDecodeIPv6(t *testing.T) {
	frame := eth(ethIPv6, ipv6TCP(t, "2001:db8::1", "2001:db8::2", 5, 6, 100, tcpACK, false, []byte("v6")))
	p := onePayload(t, classicPcap(linkEthernet, frame))
	if string(p.Payload) != "v6" {
		t.Errorf("payload = %q, want %q", p.Payload, "v6")
	}
	if p.SrcIP.String() != "2001:db8::1" || p.DstIP.String() != "2001:db8::2" {
		t.Errorf("addrs = %s -> %s, want 2001:db8::1 -> 2001:db8::2", p.SrcIP, p.DstIP)
	}
}

func TestDecodeIPv6HopByHopExtension(t *testing.T) {
	frame := eth(ethIPv6, ipv6TCP(t, "fe80::1", "fe80::2", 5, 6, 100, tcpACK, true, []byte("ext")))
	p := onePayload(t, classicPcap(linkEthernet, frame))
	if string(p.Payload) != "ext" {
		t.Errorf("payload through hop-by-hop ext header = %q, want %q", p.Payload, "ext")
	}
}

func TestLinkTypes(t *testing.T) {
	v4 := func() []byte { return ipv4TCP(t, "192.168.1.5", "203.0.113.9", 5, 6, 100, tcpACK, []byte("v4")) }
	v6 := func() []byte { return ipv6TCP(t, "2001:db8::9", "2001:db8::a", 5, 6, 100, tcpACK, false, []byte("v6")) }
	cases := []struct {
		name string
		lt   uint32
		frm  []byte
		want string
	}{
		{"SLL/v4", linkSLL, sll(ethIPv4, v4()), "v4"},
		{"SLL2/v4", linkSLL2, sll2(ethIPv4, v4()), "v4"},
		{"rawIP/v4", linkRawIP, v4(), "v4"},
		{"rawIP/v6", linkRawIP, v6(), "v6"},
		{"null/v4", linkNull, nullFrame(v4()), "v4"},
		{"vlan/v4", linkEthernet, vlanEth(ethIPv4, v4()), "v4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := onePayload(t, classicPcap(c.lt, c.frm))
			if string(p.Payload) != c.want {
				t.Errorf("payload = %q, want %q", p.Payload, c.want)
			}
		})
	}
}

func TestBigEndianClassic(t *testing.T) {
	frame := eth(ethIPv4, ipv4TCP(t, "10.0.0.1", "10.0.0.2", 1, 2, 5, tcpACK, []byte("be")))
	p := onePayload(t, classicPcapBE(linkEthernet, frame))
	if string(p.Payload) != "be" {
		t.Errorf("big-endian pcap payload = %q, want %q", p.Payload, "be")
	}
}

func TestClientInference(t *testing.T) {
	priv, pub := "192.168.1.7", "203.0.113.4"
	// no SYN captured; one endpoint is private -> inferred as client
	frames := [][]byte{
		eth(ethIPv4, ipv4TCP(t, priv, pub, 5, 17783, 100, tcpACK, []byte("q"))),
		eth(ethIPv4, ipv4TCP(t, pub, priv, 17783, 5, 200, tcpACK, []byte("r"))),
	}
	c := Conversations(mustParse(t, classicPcap(linkEthernet, frames...)))[0]

	got, err := c.Client(netip.Addr{})
	if err != nil || got.String() != priv {
		t.Errorf("inferred client = %v (err %v), want %s (private endpoint)", got, err, priv)
	}
	if got, err := c.Client(netip.MustParseAddr(pub)); err != nil || got.String() != pub {
		t.Errorf("explicit client = %v (err %v), want %s", got, err, pub)
	}
	if _, err := c.Client(netip.MustParseAddr("198.51.100.99")); err == nil {
		t.Error("expected error when -client is not an endpoint of the conversation")
	}

	// both endpoints public and no SYN -> cannot infer
	pubOnly := Conversations(mustParse(t, classicPcap(linkEthernet,
		eth(ethIPv4, ipv4TCP(t, "203.0.113.1", "203.0.113.2", 5, 6, 1, tcpACK, []byte("x"))),
	)))[0]
	if _, err := pubOnly.Client(netip.Addr{}); err == nil {
		t.Error("expected error inferring client with two public endpoints and no SYN")
	}
}

func mustParse(t *testing.T, data []byte) []Packet {
	t.Helper()
	p, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// ipv6Custom builds an IPv6 packet with an arbitrary next-header and body.
func ipv6Custom(src, dst string, next byte, body []byte) []byte {
	sa, da := netip.MustParseAddr(src).As16(), netip.MustParseAddr(dst).As16()
	ip := make([]byte, 40)
	ip[0] = 0x60
	binary.BigEndian.PutUint16(ip[4:6], uint16(len(body)))
	ip[6] = next
	ip[7] = 64
	copy(ip[8:24], sa[:])
	copy(ip[24:40], da[:])
	return append(ip, body...)
}

func TestDecodeSkipsNonTCPAndMalformed(t *testing.T) {
	v4 := func() []byte { return ipv4TCP(t, "1.1.1.1", "2.2.2.2", 1, 2, 1, tcpACK, []byte("x")) }
	cases := []struct {
		name string
		lt   uint32
		frm  []byte
	}{
		{"unknown-linktype", 999, eth(ethIPv4, v4())},
		{"truncated-ethernet", linkEthernet, []byte{0, 1, 2}},
		{"truncated-sll", linkSLL, []byte{0, 1, 2}},
		{"truncated-sll2", linkSLL2, []byte{0, 1}},
		{"truncated-null", linkNull, []byte{0, 1}},
		{"non-ip-ethertype", linkEthernet, eth(0x0806 /*ARP*/, []byte{1, 2, 3, 4})},
		{"bad-ip-version", linkRawIP, []byte{0x30, 0, 0, 0}},
		{"truncated-ipv6", linkEthernet, eth(ethIPv6, []byte{0x60, 0, 0})},
		{"ipv6-non-tcp", linkEthernet, eth(ethIPv6, ipv6Custom("2001:db8::1", "2001:db8::2", 58 /*ICMPv6*/, []byte{1, 2, 3}))},
		{"tcp-bad-dataoffset", linkEthernet, eth(ethIPv4, func() []byte { ip := v4(); ip[20+12] = 0; return ip }())},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if p := mustParse(t, classicPcap(c.lt, c.frm)); len(p) != 0 {
				t.Errorf("expected 0 packets, got %d", len(p))
			}
		})
	}

	// IPv4 carrying a non-TCP protocol (UDP) is skipped.
	udp := v4()
	udp[9] = 17
	if p := mustParse(t, classicPcap(linkEthernet, eth(ethIPv4, udp))); len(p) != 0 {
		t.Errorf("non-TCP IPv4 should be skipped, got %d packets", len(p))
	}
}

func TestDecodeIPv6ExtensionVariants(t *testing.T) {
	tcp := makeTCP(5, 6, 100, tcpACK, []byte("ok"))
	// fragment header (44): next(1), reserved(1), offset(2), id(4)
	frag := make([]byte, 8)
	frag[0] = protoTCP
	// routing header (43): next(1), hdrExtLen(1), 6 bytes
	routing := make([]byte, 8)
	routing[0] = protoTCP
	for _, c := range []struct {
		name string
		next byte
		body []byte
	}{
		{"fragment", 44, append(frag, tcp...)},
		{"routing", 43, append(routing, tcp...)},
	} {
		t.Run(c.name, func(t *testing.T) {
			frame := eth(ethIPv6, ipv6Custom("2001:db8::1", "2001:db8::2", c.next, c.body))
			p := onePayload(t, classicPcap(linkEthernet, frame))
			if string(p.Payload) != "ok" {
				t.Errorf("payload through %s ext header = %q, want %q", c.name, p.Payload, "ok")
			}
		})
	}
}
