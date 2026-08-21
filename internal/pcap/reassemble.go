package pcap

import (
	"fmt"
	"net/netip"
	"sort"
)

// Endpoint is one side of a TCP conversation.
type Endpoint struct {
	Addr netip.Addr
	Port uint16
}

func (e Endpoint) String() string { return netip.AddrPortFrom(e.Addr, e.Port).String() }

// Conversation is all the data segments exchanged between two endpoints on a
// single TCP 4-tuple.
type Conversation struct {
	A, B    Endpoint
	Packets []Packet
	// Bytes is total application payload (both directions).
	Bytes int
	// TLS is true if the stream looks like TLS (its first application bytes are
	// a TLS record header) -- i.e. encrypted and not SFS2X-decodable.
	TLS bool
}

// Conversations groups packets into TCP conversations by their unordered
// 4-tuple. Plain (non-TLS) streams sort first, then by total payload bytes
// descending -- so the SFS2X game socket, the busiest decodable stream in this
// project's captures, lands at index 0 while encrypted TLS side-channels sink
// to the bottom.
func Conversations(pkts []Packet) []Conversation {
	byKey := map[string]*Conversation{}
	for _, p := range pkts {
		a := Endpoint{p.SrcIP, p.SrcPort}
		b := Endpoint{p.DstIP, p.DstPort}
		lo, hi := a, b
		if !endpointLess(lo, hi) {
			lo, hi = hi, lo
		}
		key := lo.String() + "|" + hi.String()
		c := byKey[key]
		if c == nil {
			c = &Conversation{A: lo, B: hi}
			byKey[key] = c
		}
		c.Packets = append(c.Packets, p)
		c.Bytes += len(p.Payload)
	}
	out := make([]Conversation, 0, len(byKey))
	for _, c := range byKey {
		c.TLS = looksLikeTLS(c.Packets)
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TLS != out[j].TLS {
			return !out[i].TLS // plain (decodable) streams first
		}
		if out[i].Bytes != out[j].Bytes {
			return out[i].Bytes > out[j].Bytes
		}
		return out[i].A.String() < out[j].A.String() // stable tiebreak
	})
	return out
}

// looksLikeTLS reports whether the conversation's first application bytes are a
// TLS record header (0x16 = handshake, followed by a 0x03xx version) -- enough
// to tell an encrypted side-channel apart from the plain SFS2X game socket.
func looksLikeTLS(pkts []Packet) bool {
	for _, p := range pkts {
		if len(p.Payload) >= 3 {
			return p.Payload[0] == 0x16 && p.Payload[1] == 0x03
		}
	}
	return false
}

// Client returns the client-side address of the conversation. If explicit is
// valid it must be one of the two endpoints and is used as-is. Otherwise the
// client is inferred: the sender of the initial SYN (SYN set, ACK clear) if the
// handshake was captured, else the endpoint with a private/link-local address.
func (c Conversation) Client(explicit netip.Addr) (netip.Addr, error) {
	if explicit.IsValid() {
		if explicit == c.A.Addr || explicit == c.B.Addr {
			return explicit, nil
		}
		return netip.Addr{}, fmt.Errorf("pcap: -client %s is not an endpoint of this conversation (%s <-> %s)", explicit, c.A.Addr, c.B.Addr)
	}
	for _, p := range c.Packets {
		if p.SYN && !p.ACK {
			return p.SrcIP, nil // the side that opened the connection
		}
	}
	aPriv, bPriv := isLocal(c.A.Addr), isLocal(c.B.Addr)
	switch {
	case aPriv && !bPriv:
		return c.A.Addr, nil
	case bPriv && !aPriv:
		return c.B.Addr, nil
	}
	return netip.Addr{}, fmt.Errorf("pcap: could not infer the client side (no captured SYN, and both/neither endpoint is a private address); pass -client explicitly")
}

// Reassemble rebuilds the two directional byte streams of the conversation:
// client->server (c2s) and server->client (s2c). Each direction is reassembled
// by TCP sequence number, so out-of-order segments land at the right offset and
// retransmissions overwrite (rather than duplicate) the bytes they resend. It
// assumes the sequence space does not wrap within the capture, which holds for
// any stream under 4 GiB.
func (c Conversation) Reassemble(client netip.Addr) (c2s, s2c []byte) {
	return reassembleDir(c.Packets, client, true), reassembleDir(c.Packets, client, false)
}

func reassembleDir(pkts []Packet, client netip.Addr, fromClient bool) []byte {
	var segs []Packet
	for _, p := range pkts {
		if len(p.Payload) == 0 {
			continue // SYN/control segments carry no application bytes
		}
		if (p.SrcIP == client) == fromClient {
			segs = append(segs, p)
		}
	}
	if len(segs) == 0 {
		return nil
	}
	base := segs[0].Seq
	for _, s := range segs {
		if s.Seq < base {
			base = s.Seq
		}
	}
	var buf []byte
	for _, s := range segs {
		off := int(s.Seq - base)
		end := off + len(s.Payload)
		if end > len(buf) {
			buf = append(buf, make([]byte, end-len(buf))...)
		}
		copy(buf[off:end], s.Payload)
	}
	return buf
}

func endpointLess(a, b Endpoint) bool {
	if a.Addr != b.Addr {
		return a.Addr.Less(b.Addr)
	}
	return a.Port < b.Port
}

func isLocal(a netip.Addr) bool {
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()
}
