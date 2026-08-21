// Package pcap is a dependency-free reader for the TCP payloads in a packet
// capture. It parses both classic pcap and pcapng files (the two formats
// tcpdump and Wireshark produce), across the link-layer types a laptop/phone
// capture actually uses -- Ethernet, Linux "cooked" SLL/SLL2, raw IP, and BSD
// loopback -- and yields the per-packet TCP segments (4-tuple + sequence
// number + payload). It exists so the capture->decode pipeline for this client
// is pure Go, with no tshark or Python in the loop; see cmd/pcap.
//
// It is deliberately not a general packet dissector: it decodes only what the
// reassembler needs (IPv4/IPv6 + TCP) and skips everything else. It reads the
// whole file into memory, which is fine for the modest captures this workflow
// produces (tens of MB at most).
package pcap

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// Packet is one TCP segment carrying application data (segments with no payload
// are dropped, except that the SYN flag is preserved so the connection
// initiator can be identified).
type Packet struct {
	SrcIP   netip.Addr
	DstIP   netip.Addr
	SrcPort uint16
	DstPort uint16
	Seq     uint32
	SYN     bool
	ACK     bool
	Payload []byte
}

// Link-layer types (from https://www.tcpdump.org/linktypes.html) this reader
// understands. Anything else yields a clear error rather than silent garbage.
const (
	linkNull     = 0   // BSD loopback: 4-byte host-endian address family
	linkEthernet = 1   // Ethernet II
	linkRawIP    = 101 // raw IP (first nibble selects v4/v6)
	linkSLL      = 113 // Linux "cooked" v1 (tcpdump -i any, older)
	linkSLL2     = 276 // Linux "cooked" v2 (tcpdump -i any, newer)
)

const (
	ethIPv4  = 0x0800
	ethIPv6  = 0x86dd
	ethVLAN  = 0x8100
	protoTCP = 6
)

// Parse reads every TCP-payload segment out of a classic-pcap or pcapng file.
func Parse(data []byte) ([]Packet, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("pcap: file too short (%d bytes)", len(data))
	}
	switch {
	case binary.BigEndian.Uint32(data) == 0x0a0d0d0a:
		return parsePCAPNG(data)
	default:
		// classic pcap magic in either byte order / timestamp resolution
		m := binary.LittleEndian.Uint32(data)
		switch m {
		case 0xa1b2c3d4, 0xd4c3b2a1, 0xa1b23c4d, 0x4d3cb2a1:
			return parseClassic(data)
		}
		return nil, fmt.Errorf("pcap: unrecognized file magic 0x%08x (not classic pcap or pcapng)", binary.BigEndian.Uint32(data))
	}
}

// parseClassic handles the classic pcap format: a 24-byte global header
// followed by 16-byte record headers, each prefixing one captured frame.
func parseClassic(data []byte) ([]Packet, error) {
	// The magic as it sits on disk (raw big-endian read) tells us the file's
	// byte order: a little-endian file stores the 0xa1b2c3d4/0xa1b23c4d magic
	// byte-reversed, so it reads back as 0xd4c3b2a1/0x4d3cb2a1.
	raw := binary.BigEndian.Uint32(data)
	le := raw == 0xd4c3b2a1 || raw == 0x4d3cb2a1
	ord := order(le)
	if len(data) < 24 {
		return nil, fmt.Errorf("pcap: truncated global header")
	}
	linktype := ord.Uint32(data[20:24])
	var out []Packet
	off := 24
	for off+16 <= len(data) {
		inclLen := int(ord.Uint32(data[off+8 : off+12]))
		off += 16
		if inclLen < 0 || off+inclLen > len(data) {
			break // truncated final record; return what we have
		}
		if p, ok := decodeLink(linktype, data[off:off+inclLen]); ok {
			out = append(out, p)
		}
		off += inclLen
	}
	return out, nil
}

// parsePCAPNG handles the block-structured pcapng format. Only the blocks that
// matter are interpreted -- Section Header (byte order + section boundary),
// Interface Description (per-interface link type), Enhanced Packet and Simple
// Packet (the frames) -- everything else is skipped by its declared length.
func parsePCAPNG(data []byte) ([]Packet, error) {
	var out []Packet
	var ord binary.ByteOrder = binary.LittleEndian
	var ifaceLink []uint32 // interface index -> link type, in IDB appearance order
	off := 0
	for off+8 <= len(data) {
		btype := binary.LittleEndian.Uint32(data[off : off+4]) // type is endian-agnostic enough for SHB detection
		// For non-SHB blocks the length uses the current section's byte order.
		var total uint32
		if btype == 0x0a0d0d0a {
			// Section Header Block: read the byte-order magic to (re)set endianness.
			if off+12 > len(data) {
				break
			}
			if binary.LittleEndian.Uint32(data[off+8:off+12]) == 0x1a2b3c4d {
				ord = binary.LittleEndian
			} else {
				ord = binary.BigEndian
			}
			total = ord.Uint32(data[off+4 : off+8])
			ifaceLink = ifaceLink[:0] // new section resets interface numbering
		} else {
			total = ord.Uint32(data[off+4 : off+8])
		}
		if total < 12 || off+int(total) > len(data) {
			break // corrupt or truncated block
		}
		body := data[off+8 : off+int(total)-4] // between header (8) and trailing length (4)
		switch btype {
		case 0x00000001: // Interface Description Block
			if len(body) >= 4 {
				ifaceLink = append(ifaceLink, uint32(ord.Uint16(body[0:2])))
			}
		case 0x00000006: // Enhanced Packet Block
			if len(body) >= 20 {
				ifID := ord.Uint32(body[0:4])
				capLen := int(ord.Uint32(body[12:16]))
				if 20+capLen <= len(body) && int(ifID) < len(ifaceLink) {
					if p, ok := decodeLink(ifaceLink[ifID], body[20:20+capLen]); ok {
						out = append(out, p)
					}
				}
			}
		case 0x00000003: // Simple Packet Block (uses interface 0)
			if len(body) >= 4 && len(ifaceLink) > 0 {
				capLen := int(ord.Uint32(body[0:4]))
				if 4+capLen <= len(body) {
					if p, ok := decodeLink(ifaceLink[0], body[4:4+capLen]); ok {
						out = append(out, p)
					}
				}
			}
		}
		off += int(total)
	}
	return out, nil
}

// decodeLink strips the link layer to reach the IP packet, then decodes TCP.
func decodeLink(linktype uint32, frame []byte) (Packet, bool) {
	switch linktype {
	case linkEthernet:
		if len(frame) < 14 {
			return Packet{}, false
		}
		et := binary.BigEndian.Uint16(frame[12:14])
		payload := frame[14:]
		if et == ethVLAN { // one 802.1Q tag
			if len(frame) < 18 {
				return Packet{}, false
			}
			et = binary.BigEndian.Uint16(frame[16:18])
			payload = frame[18:]
		}
		return decodeIPByEtherType(et, payload)
	case linkSLL:
		if len(frame) < 16 {
			return Packet{}, false
		}
		return decodeIPByEtherType(binary.BigEndian.Uint16(frame[14:16]), frame[16:])
	case linkSLL2:
		if len(frame) < 20 {
			return Packet{}, false
		}
		return decodeIPByEtherType(binary.BigEndian.Uint16(frame[0:2]), frame[20:])
	case linkRawIP:
		return decodeIPByVersion(frame)
	case linkNull:
		if len(frame) < 4 {
			return Packet{}, false
		}
		// 4-byte host-endian address family; content selects v4/v6 by first nibble.
		return decodeIPByVersion(frame[4:])
	}
	return Packet{}, false
}

func decodeIPByEtherType(et uint16, p []byte) (Packet, bool) {
	switch et {
	case ethIPv4:
		return decodeIPv4(p)
	case ethIPv6:
		return decodeIPv6(p)
	}
	return Packet{}, false
}

func decodeIPByVersion(p []byte) (Packet, bool) {
	if len(p) < 1 {
		return Packet{}, false
	}
	switch p[0] >> 4 {
	case 4:
		return decodeIPv4(p)
	case 6:
		return decodeIPv6(p)
	}
	return Packet{}, false
}

func decodeIPv4(p []byte) (Packet, bool) {
	if len(p) < 20 {
		return Packet{}, false
	}
	ihl := int(p[0]&0x0f) * 4
	if ihl < 20 || ihl > len(p) || p[9] != protoTCP {
		return Packet{}, false
	}
	src, _ := netip.AddrFromSlice(p[12:16])
	dst, _ := netip.AddrFromSlice(p[16:20])
	return decodeTCP(src, dst, p[ihl:])
}

func decodeIPv6(p []byte) (Packet, bool) {
	if len(p) < 40 {
		return Packet{}, false
	}
	src, _ := netip.AddrFromSlice(p[8:24])
	dst, _ := netip.AddrFromSlice(p[24:40])
	next := p[6]
	rest := p[40:]
	// Walk the common extension headers until we reach TCP or give up.
	for i := 0; i < 8; i++ {
		switch next {
		case protoTCP:
			return decodeTCP(src, dst, rest)
		case 0, 43, 60: // hop-by-hop, routing, destination options
			if len(rest) < 2 {
				return Packet{}, false
			}
			hlen := (int(rest[1]) + 1) * 8
			if hlen > len(rest) {
				return Packet{}, false
			}
			next, rest = rest[0], rest[hlen:]
		case 44: // fragment header (fixed 8 bytes)
			if len(rest) < 8 {
				return Packet{}, false
			}
			next, rest = rest[0], rest[8:]
		default:
			return Packet{}, false
		}
	}
	return Packet{}, false
}

func decodeTCP(src, dst netip.Addr, p []byte) (Packet, bool) {
	if len(p) < 20 {
		return Packet{}, false
	}
	dataOff := int(p[12]>>4) * 4
	if dataOff < 20 || dataOff > len(p) {
		return Packet{}, false
	}
	syn := p[13]&0x02 != 0
	payload := p[dataOff:]
	if len(payload) == 0 && !syn {
		return Packet{}, false // pure ACK / control segment with no data
	}
	return Packet{
		SrcIP:   src,
		DstIP:   dst,
		SrcPort: binary.BigEndian.Uint16(p[0:2]),
		DstPort: binary.BigEndian.Uint16(p[2:4]),
		Seq:     binary.BigEndian.Uint32(p[4:8]),
		SYN:     syn,
		ACK:     p[13]&0x10 != 0,
		Payload: payload,
	}, true
}

func order(le bool) binary.ByteOrder {
	if le {
		return binary.LittleEndian
	}
	return binary.BigEndian
}
