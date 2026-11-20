package sfs

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"
	"testing"
)

// loginShapedBody is a small Login-shaped envelope ({c: 0, a: 1, p: {zn, un, p: {appVersion,
// versionCode, platform, _id}}}, the iOS values), the size of everything the production run sends.
func loginShapedBody(t *testing.T) []byte {
	t.Helper()
	inner := NewSFSObject()
	inner.PutUtfString("appVersion", "1.0.370")
	inner.PutUtfString("versionCode", "918")
	inner.PutUtfString("platform", "0")
	inner.PutInt("_id", 1)
	p := NewSFSObject()
	p.PutUtfString("zn", "APS783")
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

// loginShapedFrame is loginShapedBody's frame as Go sent it before B69/B70 (header 0xC0, 2-byte
// length 0x006F, body XORed with its little-endian length). Captured from EncodePacket at 2ceec29.
const loginShapedFrame = "c0006f7d0003006e6302006f0161036f0100011f1200036f027a6e670006413f5337385c000275010800006f" +
	"0170126f04000a0e7070560a727369006e080068312e30413337306f0b76651d73696f01436f640a0800035631380067706c611b" +
	"666f72020800015f00035f066404006f0001"

// TestRequestFramingUnchangedForProductionSizes pins the bytes of the production iOS Login's
// framing (rule 10): the B69/B70 changes leave a small body's frame byte for byte as it was, and
// a ~2 KiB compressible body keeps the compressed, 2-byte-length header. Without forward,
// EncodeRequestPacket frames exactly like EncodePacket.
func TestRequestFramingUnchangedForProductionSizes(t *testing.T) {
	body := loginShapedBody(t)
	for name, encode := range map[string]func([]byte) ([]byte, error){
		"EncodePacket":        EncodePacket,
		"EncodeRequestPacket": func(b []byte) ([]byte, error) { return EncodeRequestPacket(b, false, 0) },
	} {
		got, err := encode(body)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hex.EncodeToString(got) != loginShapedFrame {
			t.Errorf("%s(login-shaped body) = %x, want %s", name, got, loginShapedFrame)
		}
	}

	big := NewSFSObject()
	big.PutUtfString("filler", strings.Repeat("lastwar ", 256)) // 2 KiB, compressible
	bigBody, err := EncodeObject(big)
	if err != nil {
		t.Fatalf("EncodeObject: %v", err)
	}
	frame, err := EncodeRequestPacket(bigBody, false, 0)
	if err != nil {
		t.Fatalf("EncodeRequestPacket: %v", err)
	}
	if want := byte(HdrBinary | HdrEncrypted | HdrCompressed); frame[0] != want {
		t.Errorf("header = %#x, want %#x (compressed, 2-byte length, no forward sid)", frame[0], want)
	}
	if n := int(binary.BigEndian.Uint16(frame[1:3])); n != len(frame)-3 {
		t.Errorf("2-byte length = %d, want the %d payload bytes that follow", n, len(frame)-3)
	}
	got, err := ReadPacket(bytes.NewReader(frame))
	if err != nil || !bytes.Equal(got, bigBody) {
		t.Fatalf("ReadPacket round trip = %d bytes, %v; want the %d-byte body", len(got), err, len(bigBody))
	}
}

// TestEncodeRequestPacketForwardSid: with forward on, the client sets header bit 0x04 and writes
// the zone number big-endian right after the header byte, before the length
// (BaseUtils.decompiled.cs:33557-33567; WriteUShort is big-endian, SmartFox2X.decompiled.cs:
// 20334-20339). The read path already skips it, so the frame round-trips.
func TestEncodeRequestPacketForwardSid(t *testing.T) {
	body := loginShapedBody(t)
	frame, err := EncodeRequestPacket(body, true, 783)
	if err != nil {
		t.Fatalf("EncodeRequestPacket: %v", err)
	}
	if want := byte(HdrBinary | HdrEncrypted | hdrForward); frame[0] != want {
		t.Fatalf("header = %#x, want %#x", frame[0], want)
	}
	if sid := binary.BigEndian.Uint16(frame[1:3]); sid != 783 {
		t.Errorf("sid = %d, want 783", sid)
	}
	plain, err := hex.DecodeString(loginShapedFrame)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame[3:], plain[1:]) {
		t.Errorf("length and body after the sid differ from the plain frame's")
	}
	got, err := ReadPacket(bytes.NewReader(frame))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("ReadPacket round trip = %x, %v; want the body", got, err)
	}
}

// TestEncodeRequestPacketSizeLimit: the client refuses a message body over 1,000,000 bytes before
// compressing it (BaseUtils.decompiled.cs:33548-33551), however small it would compress.
func TestEncodeRequestPacketSizeLimit(t *testing.T) {
	if _, err := EncodeRequestPacket(make([]byte, MaxRequestBodySize), false, 0); err != nil {
		t.Errorf("EncodeRequestPacket(%d bytes) = %v, want the frame", MaxRequestBodySize, err)
	}
	if _, err := EncodeRequestPacket(make([]byte, MaxRequestBodySize+1), false, 0); err == nil {
		t.Errorf("EncodeRequestPacket(%d bytes) = nil error, want the client's size limit", MaxRequestBodySize+1)
	}
	if _, err := EncodePacket(make([]byte, MaxRequestBodySize+1)); err != nil {
		t.Errorf("EncodePacket(%d bytes) = %v; it frames test server frames and has no send limit", MaxRequestBodySize+1, err)
	}
}
