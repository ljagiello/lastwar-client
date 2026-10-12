package shumei

import (
	"bytes"
	"compress/flate"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// TestBoxIDErrors covers the replies BoxID must refuse: a non-200 status, the
// server's 1902 with no deviceId (ErrUnassigned), and a body that isn't JSON.
func TestBoxIDErrors(t *testing.T) {
	for _, c := range []struct {
		name       string
		status     int
		body       string
		unassigned bool
	}{
		// A success-shaped body, so only the status check can reject it.
		{"non-200", http.StatusInternalServerError, `{"code":1100,"detail":{"deviceId":"smid-1"}}`, false},
		{"1902 without deviceId", http.StatusOK, `{"code":1902,"detail":{},"requestId":"r-1"}`, true},
		{"malformed JSON", http.StatusOK, "not json", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: c.status,
					Body:       io.NopCloser(strings.NewReader(c.body)),
					Header:     http.Header{},
				}, nil
			})}
			boxID, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d")
			if err == nil {
				t.Fatalf("BoxID = %q, nil; want an error", boxID)
			}
			if got := errors.Is(err, ErrUnassigned); got != c.unassigned {
				t.Errorf("errors.Is(%v, ErrUnassigned) = %v, want %v", err, got, c.unassigned)
			}
		})
	}
}

// roundTripperFunc adapts a func to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestProtectRoundTrip inverts the whole native `w1` pipeline. protect takes
// the RSA key as a parameter, so a test-generated key pair can decrypt ep back
// to r. The AES key is derived here from the spec (hex MD5 of r), not via
// aesKeyFromNonce, so a broken helper can't cancel itself out.
func TestProtectRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey = %v", err)
	}
	in := deviceData("0123456789abcdef0123456789abcdef_n3d")
	data, ep, tn, err := protect(&priv.PublicKey, in)
	if err != nil {
		t.Fatalf("protect = %v", err)
	}
	oaepDecrypt := func(name, s string) []byte {
		ct, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("%s is not base64: %v", name, err)
		}
		pt, err := rsa.DecryptOAEP(sha256.New(), nil, priv, ct, nil)
		if err != nil {
			t.Fatalf("%s: RSA-OAEP(SHA-256) decrypt = %v", name, err)
		}
		return pt
	}

	// ep wraps the 16-char alphanumeric nonce r; tn wraps "0".
	r := oaepDecrypt("ep", ep)
	if len(r) != 16 {
		t.Fatalf("ep plaintext = %q, want the 16-char nonce", r)
	}
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	for _, c := range string(r) {
		if !strings.ContainsRune(alnum, c) {
			t.Fatalf("ep plaintext = %q, want alphanumeric", r)
		}
	}
	if got := string(oaepDecrypt("tn", tn)); got != "0" {
		t.Errorf("tn plaintext = %q, want \"0\"", got)
	}

	// data = base64(AES-256-CBC(rawDeflate(in), hex(MD5(r)), IV "0102030405060708")).
	sum := md5.Sum(r)
	block, err := aes.NewCipher([]byte(hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatalf("aes.NewCipher = %v", err)
	}
	ct, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		t.Fatalf("data is not base64: %v", err)
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		t.Fatalf("data length = %d, want a non-zero multiple of %d", len(ct), aes.BlockSize)
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, []byte("0102030405060708")).CryptBlocks(pt, ct)
	pad := int(pt[len(pt)-1])
	if pad < 1 || pad > aes.BlockSize || !bytes.Equal(pt[len(pt)-pad:], bytes.Repeat([]byte{byte(pad)}, pad)) {
		t.Fatalf("decrypted data ends in % x, want PKCS#7 padding", pt[len(pt)-aes.BlockSize:])
	}
	got, err := io.ReadAll(flate.NewReader(bytes.NewReader(pt[:len(pt)-pad])))
	if err != nil {
		t.Fatalf("raw inflate = %v", err)
	}
	if string(got) != in {
		t.Errorf("inflate(decrypt(data)) = %q, want the device-data JSON", got)
	}
}

// TestProtectWithEmbeddedKey checks the embedded iShumei key parses as 2048-bit
// RSA: its OAEP ciphertexts are 256 bytes, i.e. 344 base64 chars.
func TestProtectWithEmbeddedKey(t *testing.T) {
	pub, err := publicKey()
	if err != nil {
		t.Fatalf("publicKey = %v", err)
	}
	_, ep, tn, err := protect(pub, `{"a1":"all"}`)
	if err != nil {
		t.Fatalf("protect = %v", err)
	}
	for name, v := range map[string]string{"ep": ep, "tn": tn} {
		if len(v) != 344 {
			t.Errorf("%s length = %d, want 344 (2048-bit RSA-OAEP base64)", name, len(v))
		}
	}
}

// TestAESKeyFromNonce pins the key derivation to a known answer: the
// lowercase-hex MD5 of r.
func TestAESKeyFromNonce(t *testing.T) {
	const want = "19fc8eff82037f1fc0d8ea1d32b5e339" // md5("ABCDEFGHIJKLMNOP")
	if got := string(aesKeyFromNonce("ABCDEFGHIJKLMNOP")); got != want {
		t.Errorf("aesKeyFromNonce = %q, want %q", got, want)
	}
}

// TestDeviceData checks the template is valid JSON once filled in and that the
// device ID lands in all three of its slots (a2, a80, a97). A broken template
// only shows up live as a 1902, so this is the guard against hand edits.
func TestDeviceData(t *testing.T) {
	if n := strings.Count(innerTemplate, "__DEVICE_ID__"); n != 3 {
		t.Errorf("innerTemplate has %d __DEVICE_ID__ placeholders, want 3", n)
	}
	const id = "0123456789abcdef0123456789abcdef_n3d"
	var m map[string]any
	if err := json.Unmarshal([]byte(deviceData(id)), &m); err != nil {
		t.Fatalf("deviceData is not valid JSON: %v", err)
	}
	for _, k := range []string{"a2", "a80", "a97"} {
		if m[k] != id {
			t.Errorf("%s = %v, want the device ID %q", k, m[k], id)
		}
	}
}
