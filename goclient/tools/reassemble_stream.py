#!/usr/bin/env python3
"""Reassemble one TCP stream from a pcap into two raw byte-stream files,
one per direction, ready for `lastwar-client -decode-stream`.

Usage:
    python3 reassemble_stream.py <pcap_file> <tcp_stream_index> <client_ip> <output_prefix>

Example:
    python3 reassemble_stream.py capture.pcap 5 2600:1700:7278:2310:b196:704a:ef39:e353 stream5
    # writes stream5_c2s.bin and stream5_s2c.bin

See the docsite page "Capturing and decoding traffic" for how to find the
right <tcp_stream_index> and <client_ip> for a given capture (they're
different every time) before running this script.

Why not `tshark -z follow,tcp`: that reassembles by arrival order, not TCP
sequence number, so out-of-order and retransmitted segments in a real
capture can corrupt the reassembled stream. This reassembles by sequence
number directly and de-overlaps retransmissions, which `follow,tcp` does
not reliably do.
"""

import subprocess
import sys


def reassemble(pcap_file, tcp_stream, client_ip, output_prefix):
    result = subprocess.run(
        [
            "tshark", "-r", pcap_file,
            "-Y", f"tcp.stream=={tcp_stream} && tcp.len>0",
            "-T", "fields",
            "-e", "ip.src", "-e", "ipv6.src", "-e", "tcp.seq", "-e", "tcp.len", "-e", "data",
        ],
        capture_output=True, text=True, check=True,
    )

    streams = {}  # direction -> {seq: bytes}
    for line in result.stdout.splitlines():
        parts = line.split("\t")
        if len(parts) != 5:
            continue
        ip4, ip6, seq, _length, data = parts
        src = ip4 if ip4 else ip6
        direction = "c2s" if src == client_ip else "s2c"
        streams.setdefault(direction, {})[int(seq)] = bytes.fromhex(data)

    if not streams:
        print(f"no packets found for tcp.stream=={tcp_stream} in {pcap_file}", file=sys.stderr)
        sys.exit(1)

    for direction, segments in streams.items():
        out = bytearray()
        next_seq = None
        for seq in sorted(segments):
            data = segments[seq]
            if next_seq is None or seq >= next_seq:
                out.extend(data)
                next_seq = seq + len(data)
            elif seq + len(data) > next_seq:
                # Partial overlap with what's already written (a retransmit
                # that extends past the previous segment) -- keep only the
                # new tail.
                overlap = next_seq - seq
                out.extend(data[overlap:])
                next_seq = seq + len(data)
            # else: fully-contained retransmit of data already written, skip it.
        out_path = f"{output_prefix}_{direction}.bin"
        with open(out_path, "wb") as f:
            f.write(out)
        print(f"{direction}: {len(out)} bytes -> {out_path}")


if __name__ == "__main__":
    if len(sys.argv) != 5:
        print(__doc__, file=sys.stderr)
        sys.exit(1)
    reassemble(sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4])
