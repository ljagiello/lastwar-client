// Command pcap turns a packet capture into decoded SFS2X traffic, with no
// tshark or Python in the loop. It replaces tools/reassemble_stream.py (and the
// tshark steps around it) with pure Go:
//
//	pcap -in capture.pcap -list                       # list TCP conversations, pick one
//	pcap -in capture.pcap -stream 0 -decode           # reassemble + decode it inline
//	pcap -in capture.pcap -stream 0 -out stream0      # write stream0_c2s.bin / stream0_s2c.bin
//
// The game socket is the busiest conversation, so it sorts to index 0 in -list.
// The client side is auto-detected (the SYN sender, or the private-IP endpoint);
// pass -client to override. The written .bin files feed `lastwar-client
// -decode-stream`, exactly as the old Python tool's output did.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"

	"lastwar-client/internal/pcap"
	"lastwar-client/internal/sfs"
)

func main() {
	in := flag.String("in", "", "input capture file (classic pcap or pcapng)")
	list := flag.Bool("list", false, "list TCP conversations and exit")
	stream := flag.Int("stream", -1, "conversation index to use (from -list)")
	clientStr := flag.String("client", "", "client-side IP (default: auto-detect the connection initiator)")
	out := flag.String("out", "", "write <prefix>_c2s.bin and <prefix>_s2c.bin")
	decode := flag.Bool("decode", false, "decode the reassembled stream inline instead of writing .bin files")
	flag.Parse()

	if err := run(*in, *list, *stream, *clientStr, *out, *decode); err != nil {
		fmt.Fprintln(os.Stderr, "pcap:", err)
		os.Exit(1)
	}
}

func run(in string, list bool, stream int, clientStr, out string, decode bool) error {
	if in == "" {
		return errors.New("-in is required")
	}
	data, err := os.ReadFile(in)
	if err != nil {
		return err
	}
	pkts, err := pcap.Parse(data)
	if err != nil {
		return err
	}
	convs := pcap.Conversations(pkts)
	if len(convs) == 0 {
		return fmt.Errorf("no TCP conversations with payload found in %s", in)
	}

	if list {
		fmt.Printf("%-4s %-5s %-46s %-46s %10s\n", "idx", "kind", "endpoint A", "endpoint B", "bytes")
		for i, c := range convs {
			kind := "plain"
			if c.TLS {
				kind = "tls"
			}
			fmt.Printf("%-4d %-5s %-46s %-46s %10d  (%d segments)\n", i, kind, c.A, c.B, c.Bytes, len(c.Packets))
		}
		fmt.Fprintln(os.Stderr, "\n(plain streams are SFS2X-decodable; tls streams are encrypted side-channels. The game socket is usually plain idx 0.)")
		return nil
	}

	if stream < 0 || stream >= len(convs) {
		return fmt.Errorf("choose a -stream between 0 and %d (run with -list to see them)", len(convs)-1)
	}
	conv := convs[stream]

	var client netip.Addr
	if clientStr != "" {
		if client, err = netip.ParseAddr(clientStr); err != nil {
			return fmt.Errorf("bad -client %q: %w", clientStr, err)
		}
	}
	client, err = conv.Client(client)
	if err != nil {
		return err
	}
	c2s, s2c := conv.Reassemble(client)
	fmt.Fprintf(os.Stderr, "stream %d: %s (client) <-> %s  |  c2s %d bytes, s2c %d bytes\n",
		stream, client, other(conv, client), len(c2s), len(s2c))

	if decode {
		decodeStream("c2s", c2s)
		decodeStream("s2c", s2c)
		return nil
	}
	if out == "" {
		return errors.New("pass -out <prefix> to write .bin files, or -decode to decode inline")
	}
	for _, d := range []struct {
		label string
		buf   []byte
	}{{"c2s", c2s}, {"s2c", s2c}} {
		path := out + "_" + d.label + ".bin"
		if err := os.WriteFile(path, d.buf, 0o644); err != nil {
			return err
		}
		fmt.Printf("%s: %d bytes -> %s\n", d.label, len(d.buf), path)
	}
	return nil
}

// decodeStream frames and decodes one reassembled direction, mirroring
// lastwar-client -decode-stream's redacted, per-packet output.
func decodeStream(label string, data []byte) {
	r := bytes.NewReader(data)
	n := 0
	for {
		start := len(data) - r.Len()
		body, err := sfs.ReadPacket(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				fmt.Printf("[%s] end of stream after %d packets (%d/%d bytes)\n", label, n, start, len(data))
			} else {
				fmt.Printf("[%s] stopped at offset %d: %v\n", label, start, err)
			}
			return
		}
		n++
		obj, err := sfs.DecodeObject(body)
		if err != nil {
			fmt.Printf("[%s] #%d @offset %d: decode error: %v (body %d bytes)\n", label, n, start, err, len(body))
			continue
		}
		fmt.Printf("[%s] #%d @offset %d: %s\n", label, n, start, obj.StringRedacted())
	}
}

func other(c pcap.Conversation, client netip.Addr) pcap.Endpoint {
	if c.A.Addr == client {
		return c.B
	}
	return c.A
}
