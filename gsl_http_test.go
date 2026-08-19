package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
