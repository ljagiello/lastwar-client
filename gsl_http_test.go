package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Confirms CheckVersion and GetServerList are testable against a fake HTTP server without any
// source changes -- the seams (an overridable host list for CheckVersion, an injected gateHost
// parameter for GetServerList) already exist; they just weren't being exercised by any test.

func TestCheckVersionAgainstFakeServer(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := CheckVersionResponse{ResMsg: pub}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	cv, host, err := CheckVersion(defaultHTTPClient())
	if err != nil {
		t.Fatalf("CheckVersion: %v", err)
	}
	if host != server.URL {
		t.Errorf("host = %q, want %q", host, server.URL)
	}
	if cv.ResMsg != pub {
		t.Errorf("ResMsg mismatch: got %q, want %q", cv.ResMsg, pub)
	}
}

// TestCheckVersionRejectsOversizedResponse exercises maxGSLResponseSize's rejection branch: a
// fake server that writes a body over the limit must produce the size-limit error, not silently
// read the whole thing (or worse, an unbounded amount) into memory.
func TestCheckVersionRejectsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), maxGSLResponseSize+1))
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	_, _, err := CheckVersion(defaultHTTPClient())
	if err == nil {
		t.Fatal("CheckVersion: expected an error for an oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("CheckVersion error = %q, want it to mention the size limit", err)
	}
}

func TestGetServerListAgainstFakeServer(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A real server would decrypt the request and encrypt the response; this fake server
		// just returns a plain (unencrypted) response, which GetServerList already supports as
		// a fallback when no "bin" field is present in the top-level response.
		resp := LoginServerListRespon{
			Code: 0,
			ServerList: []LoginServerInfo{
				{ID: 1, Name: "test-server", IP: "1.2.3.4", Port: 17783, Zone: "APS1", GameUid: "g1", Status: "0"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList: %v", err)
	}
	if len(lsr.ServerList) != 1 || lsr.ServerList[0].IP != "1.2.3.4" {
		t.Fatalf("got %+v, want a single server with IP 1.2.3.4", lsr.ServerList)
	}
}

// TestGetServerListRejectsOversizedResponse exercises maxGSLResponseSize's rejection branch on
// the GetServerList side (its own io.ReadAll/LimitReader call site, separate from CheckVersion's).
func TestGetServerListRejectsOversizedResponse(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(bytes.Repeat([]byte("a"), maxGSLResponseSize+1))
	}))
	defer server.Close()

	_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err == nil {
		t.Fatal("GetServerList: expected an error for an oversized response, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") || !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("GetServerList error = %q, want it to mention the size limit", err)
	}
}

// TestFlexStringUnmarshalJSON covers the three shapes observed live for flexString fields: a
// plain JSON string, a JSON string containing an escaped quote (the case naive
// strings.Trim(`"`) got wrong -- it only strips the leading/trailing quote byte, leaving the
// escape sequence in the middle untouched), and a bare JSON number.
func TestFlexStringUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name string
		json string
		want string
	}{
		{"plain string", `"0"`, "0"},
		{"escaped quote", `"a\"b"`, `a"b`},
		{"bare number", `301`, "301"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var f flexString
			if err := json.Unmarshal([]byte(tt.json), &f); err != nil {
				t.Fatalf("Unmarshal(%s): %v", tt.json, err)
			}
			if f.String() != tt.want {
				t.Errorf("Unmarshal(%s) = %q, want %q", tt.json, f.String(), tt.want)
			}
		})
	}
}

// testRSAPubKeyDER generates a throwaway RSA keypair and returns its public key as base64 DER,
// matching the shape CheckVersion's real response field (ResMsg) carries.
func testRSAPubKeyDER(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}
