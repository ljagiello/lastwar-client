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

// TestPkcs7UnpadPadLenEqualsBlockSizeBoundary is the round-46 regression test for the MINOR
// finding that pkcs7Unpad's `padLen > blockSize` guard (crypto.go) had no exact-boundary test:
// TestPkcs7UnpadRejectsPadLenAboveBlockSize above only proves rejection at padLen=200, far past
// the 16-byte block size, leaving the guard's own strict `>` (not `>=`) unverified at padLen=16
// (must be ACCEPTED -- it's the pad length pkcs7Pad itself produces whenever the plaintext is
// already block-aligned, i.e. a full block of pure padding) versus padLen=17 (must be REJECTED,
// one byte past the block size).
func TestPkcs7UnpadPadLenEqualsBlockSizeBoundary(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()

	t.Run("padLen exactly blockSize: accepted, a full block of padding is stripped", func(t *testing.T) {
		// A plaintext whose length is already a multiple of bs forces pkcs7Pad to append an
		// entire extra block of padLen=bs bytes (crypto.go: padLen := blockSize -
		// len(data)%blockSize, and len(data)%blockSize == 0 here), the natural way padLen==bs
		// arises from this codebase's own encoder -- so this goes through the real
		// aesECBEncryptPKCS7/pkcs7Pad path rather than being hand-crafted.
		plain := []byte("exactly-one-block")[:bs]
		ct, err := aesECBEncryptPKCS7(plain, key)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		pt, err := aesECBDecryptPKCS7(ct, key)
		if err != nil {
			t.Fatalf("expected padLen==blockSize(%d) to be accepted, got error: %v", bs, err)
		}
		if !bytes.Equal(pt, plain) {
			t.Errorf("round trip mismatch: got %q want %q", pt, plain)
		}
	})

	t.Run("padLen one past blockSize: rejected", func(t *testing.T) {
		// Two full blocks so len(data) >= padLen(17) and the earlier `padLen > len(data)` guard
		// can't also explain a rejection -- isolates the `padLen > blockSize` check specifically.
		plain := make([]byte, bs*2)
		plain[len(plain)-1] = byte(bs + 1)

		ct := make([]byte, len(plain))
		for i := 0; i < len(plain); i += bs {
			block.Encrypt(ct[i:i+bs], plain[i:i+bs])
		}

		if _, err := aesECBDecryptPKCS7(ct, key); err == nil {
			t.Fatalf("expected error for pad length %d (blockSize+1), got nil", bs+1)
		}
	})
}

// TestPkcs7UnpadRejectsZeroPadLen is the round-41 regression test for the MINOR finding that
// pkcs7Unpad's `padLen <= 0` half of its guard (`if padLen <= 0 || padLen > len(data)`) had zero
// test coverage -- confirmed via mutation testing (weakening the guard to only check
// `padLen > len(data)` still passed the entire suite). A decrypted plaintext whose last byte
// happens to be 0x00 (padLen=0) would then slice `data[len(data)-0:]` -- the empty tail -- so the
// byte-by-byte padding-match loop iterates zero times and vacuously "passes", returning the data
// completely unstripped instead of erroring on what is, per RFC 5652, invalid padding (a valid
// PKCS7 pad length is always >= 1). Encrypts a crafted plaintext directly (bypassing pkcs7Pad, the
// same technique TestPkcs7UnpadRejectsPadLenAboveBlockSize/TestPkcs7UnpadRejectsMismatchedPaddingBytes
// use) whose last byte is 0x00, and proves aesECBDecryptPKCS7 rejects it instead of silently
// returning the unstripped plaintext.
func TestPkcs7UnpadRejectsZeroPadLen(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()

	plain := make([]byte, bs)
	copy(plain, []byte("zero pad len byt"))
	plain[bs-1] = 0 // claimed pad length 0 -- invalid per RFC 5652, but not > len(data) either

	ct := make([]byte, bs)
	block.Encrypt(ct, plain)

	if _, err := aesECBDecryptPKCS7(ct, key); err == nil {
		t.Fatalf("expected error for a zero pad length, got nil (the padding-match loop over an empty tail vacuously passes, silently returning the unstripped plaintext)")
	}
}

// TestPkcs7UnpadRejectsMismatchedPaddingBytes covers pkcs7Unpad's byte-by-byte padding-match
// validation loop (the `for _, b := range data[len(data)-padLen:]` check in crypto.go), which
// TestPkcs7UnpadRejectsPadLenAboveBlockSize above does not exercise: that test only forces a
// rejection via the padLen>blockSize bound, never reaching the loop at all. Here the last byte is
// a perfectly plausible pad length (4, well within the 16-byte block size) so both earlier bound
// checks pass, but an earlier "padding" byte doesn't match it (...\x05\x04\x04\x04 instead of the
// correct ...\x04\x04\x04\x04) -- exactly the shape a corrupted/tampered ciphertext block would
// produce after AES decryption. Encrypting this crafted plaintext directly (bypassing pkcs7Pad, as
// the sibling test above does) and decrypting via aesECBDecryptPKCS7 reproduces it verbatim for
// pkcs7Unpad to reject.
func TestPkcs7UnpadRejectsMismatchedPaddingBytes(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()

	plain := make([]byte, bs)
	copy(plain, []byte("corrupted block!"))
	// Last 4 bytes: 0x05, 0x04, 0x04, 0x04 -- the final byte claims padLen=4 (valid: >0, <=
	// len(data), <=blockSize), but the byte just before the last 3 doesn't match 4, so the
	// padding-match loop must reject it.
	plain[bs-4] = 5
	plain[bs-3] = 4
	plain[bs-2] = 4
	plain[bs-1] = 4

	ct := make([]byte, bs)
	block.Encrypt(ct, plain)

	if _, err := aesECBDecryptPKCS7(ct, key); err == nil {
		t.Fatalf("expected error for mismatched padding bytes, got nil")
	}
}

// TestAESECBDecryptPKCS7RejectsBadCiphertextLength covers aesECBDecryptPKCS7's ciphertext-length
// guard (crypto.go: `len(ciphertext) == 0 || len(ciphertext)%bs != 0`), which has zero existing
// test coverage (grep confirms): a corrupted/truncated "bin" field from the server, or one that's
// simply not block-aligned, must be rejected with a clear error rather than being handed to
// block.Decrypt at a length it doesn't support (which would panic).
func TestAESECBDecryptPKCS7RejectsBadCiphertextLength(t *testing.T) {
	key := md5HexKey("test-salt-value-1234")

	t.Run("empty ciphertext", func(t *testing.T) {
		if _, err := aesECBDecryptPKCS7(nil, key); err == nil {
			t.Fatalf("expected error for empty ciphertext, got nil")
		}
	})

	t.Run("length not a multiple of block size", func(t *testing.T) {
		// 17 bytes: one full AES block (16) plus one stray byte.
		ct := make([]byte, 17)
		if _, err := aesECBDecryptPKCS7(ct, key); err == nil {
			t.Fatalf("expected error for non-block-aligned ciphertext length, got nil")
		}
	})
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
