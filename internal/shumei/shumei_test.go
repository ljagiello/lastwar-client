package shumei

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

	smid, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d")
	if err != nil {
		t.Fatalf("BoxID = %v, want nil", err)
	}
	if smid != "smid-test-0123456789" {
		t.Errorf("BoxID smid = %q, want %q", smid, "smid-test-0123456789")
	}
	if ft.content != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ft.content)
	}

	// The POSTed body must carry the exact SDK field names: organization, os,
	// sdkver, appId, encode, data (the protected fingerprint). The real first
	// request carries no `retry` field (only the background resend adds it).
	var profile map[string]any
	if err := json.Unmarshal(ft.body, &profile); err != nil {
		t.Fatalf("POSTed body is not JSON: %v", err)
	}
	for key, want := range map[string]any{
		"organization": Organization,
		"os":           OSName,
		"sdkver":       SDKVersion,
		"appId":        AppID,
		"encode":       2.0,
	} {
		if got, ok := profile[key]; !ok || got != want {
			t.Errorf("profile[%q] = %v (present=%v), want %v", key, got, ok, want)
		}
	}
	if _, hasRetry := profile["retry"]; hasRetry {
		t.Errorf("profile carries an unexpected \"retry\" field (the first request has none)")
	}
	if data, ok := profile["data"].(string); !ok || data == "" {
		t.Errorf("profile[\"data\"] = %v, want a non-empty protected fingerprint", profile["data"])
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

func TestDeviceFingerprintIsRSAProtectedBase64(t *testing.T) {
	enc, err := DeviceFingerprint("0123456789abcdef0123456789abcdef_n3d")
	if err != nil {
		t.Fatalf("DeviceFingerprint = %v", err)
	}
	if len(enc) == 0 {
		t.Fatal("DeviceFingerprint returned empty string")
	}
	// RSA-OAEP on a 2048-bit key yields a 256-byte ciphertext -> 344 base64
	// chars, so a real protected blob is far longer than any plaintext marker.
	if len(enc) < 300 {
		t.Errorf("DeviceFingerprint length = %d, want >= 344 (2048-bit RSA-OAEP base64)", len(enc))
	}
}
