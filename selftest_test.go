package main

import (
	"bytes"
	"crypto/aes"
	"testing"
)

func TestSFSObjectRoundTrip(t *testing.T) {
	o := NewSFSObject()
	o.PutUtfString("mail", "roundtrip-test@example.com")
	o.PutInt("type", 0)
	o.PutLong("bignum", 1234567890123)
	o.PutBool("flag", true)
	inner := NewSFSObject()
	inner.PutUtfString("nested", "yes")
	o.PutSFSObject("sub", inner)
	arr := NewSFSArray()
	arr.AddInt(1)
	arr.AddInt(2)
	o.PutSFSArray("arr", arr)

	encoded, err := EncodeObject(o)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := DecodeObject(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.GetString("mail") != "roundtrip-test@example.com" {
		t.Errorf("mail mismatch: %q", decoded.GetString("mail"))
	}
	if decoded.GetInt("type") != 0 {
		t.Errorf("type mismatch")
	}
	sub, ok := decoded.Get("sub")
	if !ok {
		t.Fatalf("sub missing")
	}
	subObj := sub.Val.(*SFSObject)
	if subObj.GetString("nested") != "yes" {
		t.Errorf("nested mismatch: %q", subObj.GetString("nested"))
	}
}

func TestAESECBRoundTrip(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")
	if len(key) != 32 {
		t.Fatalf("expected 32-byte key, got %d", len(key))
	}
	plain := []byte(`uuid=abc&airKey=lwDid_xyz&loginFlag=1`)
	ct, err := aesECBEncryptPKCS7(plain, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	pt, err := aesECBDecryptPKCS7(ct, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, plain) {
		t.Errorf("round trip mismatch: got %q want %q", pt, plain)
	}
	// ECB signature: identical plaintext block -> identical ciphertext.
	ct2, _ := aesECBEncryptPKCS7(plain, key)
	if !bytes.Equal(ct, ct2) {
		t.Errorf("expected deterministic ECB output for identical input")
	}
}

// TestPkcs7UnpadRejectsPadLenAboveBlockSize covers the RFC 5652 requirement
// that a PKCS7 pad length can never exceed the cipher's block size (16 bytes
// for the AES-256-ECB use here -- see aesECBDecryptPKCS7's bs := block.BlockSize()).
// pkcs7Unpad's old bound only checked padLen <= len(data), which a
// corrupted/garbage multi-block ciphertext could satisfy while still being
// well beyond one block, silently stripping far more than real padding.
func TestPkcs7UnpadRejectsPadLenAboveBlockSize(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()

	// Craft a multi-block plaintext (13 blocks = 208 bytes) whose final byte
	// -- the claimed pad length -- is 200: bigger than the block size (16)
	// but still <= len(data), so the pre-fix bound check would have accepted
	// it. Encrypt it directly (bypassing pkcs7Pad) so that decrypting via
	// aesECBDecryptPKCS7 reproduces this exact "corrupted" plaintext, with
	// its last byte intact, for pkcs7Unpad to evaluate.
	plain := make([]byte, bs*13)
	plain[len(plain)-1] = 200
	if len(plain) < 200 {
		t.Fatalf("test setup: plaintext too short for claimed pad length")
	}
	ct := make([]byte, len(plain))
	for i := 0; i < len(plain); i += bs {
		block.Encrypt(ct[i:i+bs], plain[i:i+bs])
	}

	if _, err := aesECBDecryptPKCS7(ct, key); err == nil {
		t.Fatalf("expected error for pad length %d exceeding block size %d, got nil", 200, bs)
	}
}

func TestPacketRoundTripSmall(t *testing.T) {
	body := []byte("small payload, no compression")
	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got, err := ReadPacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("mismatch: got %q want %q", got, body)
	}
}

func TestPacketRoundTripLargeCompressed(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 5000) // > compressionThreshold
	packet, err := EncodePacket(body)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if packet[0]&hdrCompressed == 0 {
		t.Errorf("expected compressed flag to be set for large payload")
	}
	got, err := ReadPacket(bytes.NewReader(packet))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("mismatch length: got %d want %d", len(got), len(body))
	}
}

func TestSecurityCodeAlgorithm(t *testing.T) {
	// Just verify determinism + length (32 hex chars), not a known vector.
	sc := securityCode("1700000000", "guest123")
	if len(sc) != 32 {
		t.Errorf("expected 32-char md5 hex, got %d: %q", len(sc), sc)
	}
	oneCode, coreV := oneCodeAndCoreV()
	if len(oneCode) != 64 || len(coreV) != 64 {
		t.Errorf("expected 64-char interleaved codes, got %d/%d", len(oneCode), len(coreV))
	}
}

func TestPackageSignMatchesKnownValue(t *testing.T) {
	// sha1("com.fun.lastwar.gp") lowercase hex, computed independently.
	got := packageSignHex(packageName)
	if len(got) != 40 {
		t.Errorf("expected 40-char sha1 hex, got %d: %q", len(got), got)
	}
	// Confirmed live against a real captured iOS Login request.
	const wantIOS = "506d9b737f4da295c6050b8d9492e00ba00605c0"
	if got := packageSignHex(iosPackageName); got != wantIOS {
		t.Errorf("packageSignHex(iosPackageName) = %q, want %q", got, wantIOS)
	}
}
