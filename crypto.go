package main

import (
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
)

const saltAlphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*()_+-/=<>?{}[]"

// randomSalt reproduces AESHelper.GenerateRandomSalt(20) closely enough —
// the server only cares about the resulting bytes, not the PRNG.
func randomSalt(n int) (string, error) {
	buf := make([]byte, 1)
	out := make([]byte, n)
	// Reject bytes past the largest multiple of len(saltAlphabet) that fits in a byte, so every
	// alphabet character has exactly equal probability -- a plain %-mod on a uniform byte would
	// otherwise be a few percent biased toward the first (256 mod len(saltAlphabet)) characters.
	limit := 256 - (256 % len(saltAlphabet))
	for i := 0; i < n; i++ {
		for {
			if _, err := rand.Read(buf); err != nil {
				return "", err
			}
			if int(buf[0]) < limit {
				break
			}
		}
		out[i] = saltAlphabet[int(buf[0])%len(saltAlphabet)]
	}
	return string(out), nil
}

// urlSafeB64Encode matches AESHelper.ToUrlSafeBase64: standard base64,
// then + -> -, / -> _, strip trailing '=' -- which is exactly what Go's
// URLEncoding-with-no-padding alphabet produces.
func urlSafeB64Encode(b []byte) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(b)
}

func urlSafeB64Decode(s string) ([]byte, error) {
	return base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(s)
}

// parseRSAPubKeyFromDER parses the base64 DER SubjectPublicKeyInfo delivered
// in the check-version response's `resMsg` field (no PEM armor from the
// server — the client adds it before feeding BouncyCastle; Go's
// x509.ParsePKIXPublicKey takes DER directly).
func parseRSAPubKeyFromDER(b64Der string) (*rsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(b64Der)
	if err != nil {
		return nil, fmt.Errorf("resMsg base64 decode: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("parse SubjectPublicKeyInfo: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("resMsg key is not RSA (got %T)", pub)
	}
	return rsaPub, nil
}

// md5HexKey reproduces AESHelper.GetMd5Hash: MD5 digest, formatted as a
// lowercase hex STRING, whose ASCII bytes (32 of them) are used directly as
// the AES-256 key -- NOT the raw 16-byte digest.
func md5HexKey(s string) []byte {
	sum := md5.Sum([]byte(s))
	return []byte(hex.EncodeToString(sum[:]))
}

// aesECBEncryptPKCS7 / aesECBDecryptPKCS7 implement AES-256-ECB-PKCS7 by
// hand: Go's stdlib deliberately omits cipher.NewECBEncrypter since ECB is
// unsafe for general use, but this is exactly what GSL uses (confirmed
// empirically against the decompiled RijndaelManaged call, see dossier §03
// -- CipherMode 2 is ECB, not CBC).
func aesECBEncryptPKCS7(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	padded := pkcs7Pad(plaintext, bs)
	out := make([]byte, len(padded))
	for i := 0; i < len(padded); i += bs {
		block.Encrypt(out[i:i+bs], padded[i:i+bs])
	}
	return out, nil
}

func aesECBDecryptPKCS7(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(ciphertext) == 0 || len(ciphertext)%bs != 0 {
		return nil, fmt.Errorf("aesECBDecrypt: ciphertext length %d not a multiple of block size %d", len(ciphertext), bs)
	}
	out := make([]byte, len(ciphertext))
	for i := 0; i < len(ciphertext); i += bs {
		block.Decrypt(out[i:i+bs], ciphertext[i:i+bs])
	}
	return pkcs7Unpad(out, bs)
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+padLen)
	copy(padded, data)
	for i := len(data); i < len(padded); i++ {
		padded[i] = byte(padLen)
	}
	return padded
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("pkcs7Unpad: empty input")
	}
	padLen := int(data[len(data)-1])
	if padLen <= 0 || padLen > len(data) {
		return nil, fmt.Errorf("pkcs7Unpad: invalid padding byte %d", padLen)
	}
	if padLen > blockSize {
		return nil, fmt.Errorf("pkcs7Unpad: padding byte %d exceeds block size %d", padLen, blockSize)
	}
	for _, b := range data[len(data)-padLen:] {
		if int(b) != padLen {
			return nil, fmt.Errorf("pkcs7Unpad: invalid padding bytes")
		}
	}
	return data[:len(data)-padLen], nil
}

// GSLCrypto holds the per-session state needed to encrypt a GSL request and
// later decrypt its response -- the AES key is derived from the salt, and
// the salt must be kept in scope from request to response (dossier §03).
type GSLCrypto struct {
	pub  *rsa.PublicKey
	salt string
}

func NewGSLCrypto(pub *rsa.PublicKey) *GSLCrypto {
	return &GSLCrypto{pub: pub}
}

// EncryptRequest builds the {uuid, data} form fields for POST
// getserverlist.php from a plaintext "k1=v1&k2=v2&..." body.
func (g *GSLCrypto) EncryptRequest(plainForm string) (uuid string, data string, err error) {
	salt, err := randomSalt(20)
	if err != nil {
		return "", "", err
	}
	g.salt = salt

	// uuid = urlsafe_b64( RSA_PKCS1v15( salt ) )
	// PKCS1v15 is what the server actually speaks (confirmed from the
	// decompiled client: RSACryptoServiceProvider.Encrypt(bytes, fOAEP:
	// false)) -- OAEP would not interoperate here.
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, g.pub, []byte(salt)) //nolint:staticcheck // must match server's PKCS1v15 scheme
	if err != nil {
		return "", "", fmt.Errorf("rsa encrypt salt: %w", err)
	}
	uuid = urlSafeB64Encode(ct)

	// data = urlsafe_b64( AES256_ECB_PKCS7( plainForm, key=md5hex(salt) ) )
	key := md5HexKey(salt)
	enc, err := aesECBEncryptPKCS7([]byte(plainForm), key)
	if err != nil {
		return "", "", fmt.Errorf("aes encrypt form: %w", err)
	}
	data = urlSafeB64Encode(enc)
	return uuid, data, nil
}

// DecryptResponse decodes+decrypts the `bin` field of a GSL response using
// the salt from the most recent EncryptRequest call.
func (g *GSLCrypto) DecryptResponse(binField string) (string, error) {
	if g.salt == "" {
		return "", fmt.Errorf("gslcrypto: no salt in scope (call EncryptRequest first)")
	}
	raw, err := urlSafeB64Decode(binField)
	if err != nil {
		return "", fmt.Errorf("bin base64 decode: %w", err)
	}
	key := md5HexKey(g.salt)
	pt, err := aesECBDecryptPKCS7(raw, key)
	if err != nil {
		return "", fmt.Errorf("aes decrypt bin: %w", err)
	}
	return string(pt), nil
}

// rsaModulusBitLen is a small helper used only for sanity logging.
func rsaModulusBitLen(pub *rsa.PublicKey) int {
	return new(big.Int).Set(pub.N).BitLen()
}
