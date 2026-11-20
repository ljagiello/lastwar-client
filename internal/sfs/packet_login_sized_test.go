package sfs

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"testing"
)

// loginSizedBody is a Login-sized envelope ({c: 0, a: 1, p: {zn, un, p: {...}}}) of about 1.8 KB,
// the size class of the production iOS Login (~2.2 KB: compressed, 2-byte length): the iOS build
// values, made-up identity values, and a ThinkingData-style ta string. None of it comes from a
// capture.
func loginSizedBody(t *testing.T) []byte {
	t.Helper()
	inner := NewSFSObject()
	inner.PutUtfString("appVersion", "1.0.370")
	inner.PutUtfString("versionCode", "918")
	inner.PutUtfString("platform", "0")
	inner.PutUtfString("deviceId", "00000000-0000-4000-8000-000000000000")
	inner.PutUtfString("phone_model", "test-model")
	inner.PutUtfString("osVersion", "test-os")
	inner.PutUtfString("lang", "en")
	inner.PutUtfString("fromCountry", "US")
	inner.PutInt("netType", 1)
	for i := range 30 {
		inner.PutUtfString(fmt.Sprintf("field%02d", i), strings.Repeat("v", 12))
	}
	var ta strings.Builder
	ta.WriteString(`{"#lib":"test","#os":"test-os","lw_version":"1.0.370"`)
	for i := range 40 {
		fmt.Fprintf(&ta, `,"prop%02d":"value-%02d"`, i, i)
	}
	ta.WriteString("}")
	inner.PutUtfString("ta", ta.String())
	inner.PutInt("_id", 1)
	p := NewSFSObject()
	p.PutUtfString("zn", "APS000")
	p.PutUtfString("un", "")
	p.PutSFSObject("p", inner)
	outer := NewSFSObject()
	outer.PutByte("c", 0)
	outer.PutShort("a", 1)
	outer.PutSFSObject("p", p)
	body, err := EncodeObject(outer)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}
	return body
}

// clientDecodeRequest reads a request frame the way NetPacket.onBeforeSend builds it
// (BaseUtils.decompiled.cs:33544-33584), independently of ReadPacket: the header byte, a 2-byte
// big-endian length (no forward sid, not big-sized), the payload XORed with the 4 little-endian
// bytes of its own length (Encrypt, :33586-33595), and zlib (ByteArray.Compress uses ZOutputStream
// level 9, SmartFox2X.decompiled.cs:20230-20242) when the header's 0x20 bit is set.
func clientDecodeRequest(t *testing.T, frame []byte) (header byte, length int, body []byte) {
	t.Helper()
	if len(frame) < 3 {
		t.Fatalf("frame of %d bytes", len(frame))
	}
	header, length = frame[0], int(binary.BigEndian.Uint16(frame[1:3]))
	payload := append([]byte(nil), frame[3:]...)
	var key [4]byte
	binary.LittleEndian.PutUint32(key[:], uint32(len(payload)))
	for i := range payload {
		payload[i] ^= key[i%4]
	}
	if header&0x20 == 0 {
		return header, length, payload
	}
	r, err := zlib.NewReader(bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("zlib header: %v", err)
	}
	body, err = io.ReadAll(r)
	if err != nil {
		t.Fatalf("zlib: %v", err)
	}
	return header, length, body
}

// TestLoginSizedRequestFrame pins the framing of a Login-sized request (final-r3 auth-transport
// M2): the client compresses a body over 1024 bytes and keeps the 2-byte length up to 65535
// (onBeforeSend, BaseUtils.decompiled.cs:33546-33557), so the header is 0x80 binary | 0x40
// encrypted | 0x20 compressed = 0xE0 (package_header.Encode, :33407-33435), the length field holds
// the compressed payload's size, and the payload decodes back to the body. The compressed bytes
// themselves are not pinned: they follow the zlib implementation.
func TestLoginSizedRequestFrame(t *testing.T) {
	body := loginSizedBody(t)
	if len(body) <= 1024 || len(body) > 65535 {
		t.Fatalf("login-sized body is %d bytes, want 1025..65535 (compressed, 2-byte length)", len(body))
	}
	frame, err := EncodeRequestPacket(body, false, 0)
	if err != nil {
		t.Fatalf("EncodeRequestPacket: %v", err)
	}
	header, length, got := clientDecodeRequest(t, frame)
	if header != 0xE0 {
		t.Errorf("header = %#x, want 0xe0 (binary, encrypted, compressed; no big-size, no forward)", header)
	}
	if length != len(frame)-3 {
		t.Errorf("2-byte length = %d, want the %d payload bytes that follow", length, len(frame)-3)
	}
	if length >= len(body) {
		t.Errorf("payload %d bytes for a %d-byte body: not compressed", length, len(body))
	}
	if !bytes.Equal(got, body) {
		t.Errorf("decoded payload (%d bytes) differs from the %d-byte body", len(got), len(body))
	}
}

// TestRequestCompressionThresholdLiteral pins onBeforeSend's `byteArray.Length > 1024`
// (BaseUtils.decompiled.cs:33547) with the client's literal: a 1024-byte body goes out with header
// 0xC0 and as is, a 1025-byte body with 0xE0, compressed.
func TestRequestCompressionThresholdLiteral(t *testing.T) {
	for _, c := range []struct {
		size   int
		header byte
	}{{1024, 0xC0}, {1025, 0xE0}} {
		body := bytes.Repeat([]byte("a"), c.size)
		frame, err := EncodeRequestPacket(body, false, 0)
		if err != nil {
			t.Fatalf("%d bytes: %v", c.size, err)
		}
		header, length, got := clientDecodeRequest(t, frame)
		if header != c.header || length != len(frame)-3 || !bytes.Equal(got, body) {
			t.Errorf("%d-byte body: header %#x, length %d of %d, round trip %v; want header %#x",
				c.size, header, length, len(frame)-3, bytes.Equal(got, body), c.header)
		}
	}
}
