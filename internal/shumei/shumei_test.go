package shumei

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport returns a canned device-profile response, recording the
// request body so the test can assert on the exact wire shape.
type fakeTransport struct {
	body    []byte
	content string
	status  int
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.body, _ = io.ReadAll(req.Body)
	f.content = req.Header.Get("Content-Type")
	return &http.Response{
		StatusCode: f.status,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"code":1100,"detail":{"deviceId":"smid-test-0123456789","sid":"s-1","c":1,"t":1700000000},"requestId":"r-1"}`))),
		Header:     http.Header{},
	}, nil
}

func TestBoxID(t *testing.T) {
	ft := &fakeTransport{status: http.StatusOK}
	client := &http.Client{Transport: ft}

	boxID, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d")
	if err != nil {
		t.Fatalf("BoxID = %v, want nil", err)
	}
	// The Login request expects getDeviceId()'s "B" + smid string.
	if boxID != "Bsmid-test-0123456789" {
		t.Errorf("BoxID = %q, want %q", boxID, "Bsmid-test-0123456789")
	}
	if ft.content != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ft.content)
	}

	// The POSTed body must carry the exact native `w1` field names:
	// organization, os, appId, encode (2), compress (3), data, tn, ep. The
	// real first request carries no `retry` field (only the background resend
	// adds it).
	var profile map[string]any
	if err := json.Unmarshal(ft.body, &profile); err != nil {
		t.Fatalf("POSTed body is not JSON: %v", err)
	}
	for key, want := range map[string]any{
		"organization": Organization,
		"os":           OSName,
		"appId":        AppID,
		"encode":       2.0,
		"compress":     3.0,
	} {
		if got, ok := profile[key]; !ok || got != want {
			t.Errorf("profile[%q] = %v (present=%v), want %v", key, got, ok, want)
		}
	}
	if _, hasRetry := profile["retry"]; hasRetry {
		t.Errorf("profile carries an unexpected \"retry\" field (the first request has none)")
	}
	for _, key := range []string{"data", "tn", "ep"} {
		if v, ok := profile[key].(string); !ok || v == "" {
			t.Errorf("profile[%q] = %v, want a non-empty protected value", key, profile[key])
		}
	}
}

func TestBoxIDRejectsUnassigned(t *testing.T) {
	ft := &fakeTransport{status: http.StatusOK}
	client := &http.Client{Transport: ft}
	// override the canned body with a no-deviceId response
	client.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader([]byte(`{"code":1902,"detail":{},"requestId":"r-1"}`))),
			Header:     http.Header{},
		}, nil
	})

	if _, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d"); err == nil {
		t.Fatal("BoxID accepted a response with no deviceId, want ErrUnassigned")
	} else if got := err.Error(); got == "" {
		t.Fatal("BoxID returned an empty error string")
	}
}

// roundTripperFunc adapts a func to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestProtectProducesHybridCiphertext(t *testing.T) {
	pub, err := publicKey()
	if err != nil {
		t.Fatalf("publicKey = %v", err)
	}
	data, ep, tn, err := protect(pub, `{"a1":"all"}`)
	if err != nil {
		t.Fatalf("protect = %v", err)
	}
	// data is AES-256-CBC ciphertext: base64 of a multiple of the 16-byte block.
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("data is not base64: %v", err)
	}
	if len(raw) == 0 || len(raw)%aes.BlockSize != 0 {
		t.Errorf("data length = %d, want a non-zero multiple of %d", len(raw), aes.BlockSize)
	}
	// ep/tn are RSA-OAEP on a 2048-bit key: 256-byte ciphertext -> 344 base64 chars.
	for name, v := range map[string]string{"ep": ep, "tn": tn} {
		if len(v) != 344 {
			t.Errorf("%s length = %d, want 344 (2048-bit RSA-OAEP base64)", name, len(v))
		}
	}
}

// TestAESKeyDerivationAndRoundTrip verifies the AES-256-CBC + PKCS#7 padding
// the native `w1` uses: key = MD5(r) lowercase hex, IV "0102030405060708".
func TestAESKeyDerivationAndRoundTrip(t *testing.T) {
	const r = "ABCDEFGHIJKLMNOP"
	key := aesKeyFromNonce(r)
	if len(key) != 32 {
		t.Fatalf("aesKeyFromNonce length = %d, want 32", len(key))
	}
	// The key must be lowercase hex (MD5(r) hex-encoded).
	for _, c := range string(key) {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("aesKeyFromNonce = %q, want lowercase hex", key)
		}
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher = %v", err)
	}
	iv := []byte("0102030405060708")
	plaintext := []byte("hello shumei")
	padded := pkcs7Pad(plaintext, block.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)

	// decrypt back and strip padding
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	padLen := int(pt[len(pt)-1])
	pt = pt[:len(pt)-padLen]
	if string(pt) != "hello shumei" {
		t.Errorf("round-trip = %q, want %q", pt, "hello shumei")
	}
}
