package main

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
			Code: "0",
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

// TestGetServerListFallsBackToLoginServerWhenServerListEmpty covers the opt=new fallback added to
// applyLoginServerFallback (gsl.go): a fake GSL server returns an empty ServerList but a populated
// LoginServer -- the field AccountServerInfo's own doc comment says exists specifically for a
// brand-new device with no account/state yet (opt=new). Before this fallback, login.go's caller
// unconditionally treated an empty ServerList as "no servers returned" for every opt, including
// this exact opt=new case the field documents itself as covering. This does not assert anything
// about real server behavior (no live capture of this scenario exists in this repo yet) -- it only
// confirms GetServerList's own, conservative, additive fallback logic behaves as coded.
func TestGetServerListFallsBackToLoginServerWhenServerListEmpty(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Same plaintext-fallback shape as TestGetServerListAgainstFakeServer, but with an empty
		// ServerList and a populated LoginServer instead.
		resp := LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{},
			LoginServer: &AccountServerInfo{
				IP:     "9.9.9.9",
				Port:   12345,
				WsIP:   "9.9.9.9",
				WsPort: 12346,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList: %v", err)
	}
	if len(lsr.ServerList) != 1 {
		t.Fatalf("ServerList = %+v, want a single synthesized entry from LoginServer", lsr.ServerList)
	}
	got := lsr.ServerList[0]
	if got.IP != "9.9.9.9" || got.Port != 12345 || got.WsIP != "9.9.9.9" {
		t.Errorf("synthesized ServerList[0] = %+v, want IP/Port/WsIP from LoginServer", got)
	}

	// Same scenario but for a non-"new" opt: the fallback must NOT fire, so callers like
	// crossserver.go's opt=fix redirect-refresh and main.go's opt=refresh see zero behavior
	// change -- an empty ServerList must still surface as empty, letting the existing "no
	// servers returned" handling in those callers behave exactly as it did before this fix.
	server2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := LoginServerListRespon{
			Code:       "0",
			ServerList: []LoginServerInfo{},
			LoginServer: &AccountServerInfo{
				IP:   "9.9.9.9",
				Port: 12345,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server2.Close()

	lsrFix, err := GetServerList(defaultHTTPClient(), server2.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "fix"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList: %v", err)
	}
	if len(lsrFix.ServerList) != 0 {
		t.Errorf("ServerList = %+v, want it to stay empty for opt=fix (fallback is scoped to opt=new only)", lsrFix.ServerList)
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

// TestGetServerListDecodeFailuresDoNotLeakRawResponse is the round-11/round-12 regression test for
// gsl.go's response-error branches: a real getserverlist.php response legitimately carries a live
// at/rt session token on success (LoginServerListRespon.At/Rt), so none of these branches may embed
// the raw body/decrypted plaintext in the returned error. Three subtests force a different one of
// the three json.Unmarshal calls in GetServerList to fail while a fake token is present in the
// response; a fourth (round 12) covers the sibling HTTP-status-error branch, which round 11 missed.
// Each asserts the fake token never appears in the resulting error text.
func TestGetServerListDecodeFailuresDoNotLeakRawResponse(t *testing.T) {
	const fakeToken = "FAKE-LIVE-SESSION-TOKEN-must-not-leak-abc123"

	t.Run("top-level JSON invalid", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Not valid JSON at all -- fails the map[string]json.RawMessage unmarshal (gsl.go's
			// "decode top-level GSL response" branch) before any decryption is attempted.
			w.Write([]byte("not json at all, but carries " + fakeToken))
		}))
		defer server.Close()

		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
		if err == nil {
			t.Fatal("GetServerList: expected a decode error, got nil")
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("GetServerList error leaks the raw response body: %v", err)
		}
	})

	t.Run("plaintext fallback type-mismatched", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Valid JSON with no "bin" field (so GetServerList falls to the plaintext-fallback
			// branch), but "serverList" is a string where LoginServerListRespon.ServerList expects
			// a JSON array -- a genuine type mismatch that still fails json.Unmarshal. (This used
			// to type-mismatch "code" instead, but Code is now flexString -- see its doc comment --
			// and flexString.UnmarshalJSON never returns an error, so a string-typed "code" no
			// longer forces a decode failure here.)
			fmt.Fprintf(w, `{"code":"0","serverList":"bad-type carries %s"}`, fakeToken)
		}))
		defer server.Close()

		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
		if err == nil {
			t.Fatal("GetServerList: expected a decode error, got nil")
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("GetServerList error leaks the raw response body: %v", err)
		}
	})

	t.Run("HTTP status error", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A non-200 status whose body still carries what looks like a live session token --
			// gsl.go's HTTP-status-error branch (a sibling of the three decode-failure branches
			// above) must not echo it back either.
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":"server error carries %s"}`, fakeToken)
		}))
		defer server.Close()

		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}
		_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
		if err == nil {
			t.Fatal("GetServerList: expected an HTTP status error, got nil")
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("GetServerList error leaks the raw response body: %v", err)
		}
	})

	t.Run("bin field fails PKCS7 padding after decrypt", func(t *testing.T) {
		// Recover the AES key the same way the "decrypted plaintext type-mismatched" subtest below
		// does, but instead of encrypting a well-formed (if type-mismatched) reply, encrypt one
		// block directly with mismatched padding bytes -- reproducing a tampered/corrupted "bin"
		// field that passes base64+block-alignment but fails PKCS7 unpadding. This is the one
		// decode/decrypt-failure path (gsl.go's "decrypt GSL response" branch) none of this test
		// function's other subtests reach: they all either fail before decryption or fail the
		// subsequent JSON decode of otherwise-valid decrypted plaintext.
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
				return
			}
			saltCT, err := urlSafeB64Decode(r.FormValue("uuid"))
			if err != nil {
				t.Errorf("decode uuid field: %v", err)
				return
			}
			salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
			if err != nil {
				t.Errorf("rsa decrypt salt: %v", err)
				return
			}
			key := md5HexKey(string(salt))
			block, err := aes.NewCipher(key)
			if err != nil {
				t.Errorf("aes.NewCipher: %v", err)
				return
			}
			bs := block.BlockSize()

			// One block of plaintext embedding fakeToken, followed by bytes that are NOT valid
			// PKCS7 padding for this key (last byte looks like a plausible pad length, but the
			// preceding "padding" bytes don't match it -- see
			// TestPkcs7UnpadRejectsMismatchedPaddingBytes in selftest_test.go for the same shape
			// applied directly to pkcs7Unpad).
			plain := make([]byte, bs)
			copy(plain, fakeToken)
			plain[bs-4] = 5
			plain[bs-3] = 4
			plain[bs-2] = 4
			plain[bs-1] = 4

			ct := make([]byte, bs)
			block.Encrypt(ct, plain)
			fmt.Fprintf(w, `{"bin":%q}`, urlSafeB64Encode(ct))
		}))
		defer server.Close()

		_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
		if err == nil {
			t.Fatal("GetServerList: expected a decrypt error, got nil")
		}
		if !strings.Contains(err.Error(), "decrypt GSL response") {
			t.Errorf("GetServerList error = %q, want it to mention the decrypt-GSL-response branch", err)
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("GetServerList error leaks the raw ciphertext/decrypted response: %v", err)
		}
	})

	t.Run("decrypted plaintext type-mismatched", func(t *testing.T) {
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Play the server side of the real GSL crypto envelope (same recipe as
			// TestGSLCryptoRoundTrip in crypto_gsl_test.go): recover the AES key from the
			// client's RSA-encrypted uuid field, then encrypt a type-mismatched reply with it so
			// GetServerList reaches its decrypted-plaintext decode-failure branch instead of the
			// plaintext-fallback one above.
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
				return
			}
			saltCT, err := urlSafeB64Decode(r.FormValue("uuid"))
			if err != nil {
				t.Errorf("decode uuid field: %v", err)
				return
			}
			salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
			if err != nil {
				t.Errorf("rsa decrypt salt: %v", err)
				return
			}
			key := md5HexKey(string(salt))
			// Same "serverList" type mismatch as the plaintext-fallback subtest above (see its
			// comment for why "code" no longer works for this now that Code is flexString), just
			// routed through the real AES envelope so this exercises the decrypted-plaintext
			// decode-failure branch instead.
			reply := fmt.Sprintf(`{"code":"0","serverList":"bad-type carries %s"}`, fakeToken)
			encReply, err := aesECBEncryptPKCS7([]byte(reply), key)
			if err != nil {
				t.Errorf("aes encrypt reply: %v", err)
				return
			}
			fmt.Fprintf(w, `{"bin":%q}`, urlSafeB64Encode(encReply))
		}))
		defer server.Close()

		_, err = GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
		if err == nil {
			t.Fatal("GetServerList: expected a decode error, got nil")
		}
		if strings.Contains(err.Error(), fakeToken) {
			t.Errorf("GetServerList error leaks the raw decrypted response: %v", err)
		}
	})
}

// TestGetServerListBinFieldPresentButEmpty is the round-24 regression test for a fallthrough gap
// in GetServerList's "bin" handling: when the top-level "bin" field is PRESENT but decodes to an
// empty string, the old code's "if binStr != \"\" { ...decrypt and return... }" had no else
// branch, so execution fell through to json.Unmarshal(body, &lsr) against the ORIGINAL top-level
// envelope (shaped like {"bin":"",...}), which has none of LoginServerListRespon's required
// fields -- unknown/extra keys are silently ignored by encoding/json, so lsr ended up completely
// zero-valued with a NIL error. Traced live: login.go's serverInfo-redirect access-token refresh
// call site treats a nil error as success, so this specific case was a fully silent no-op with
// zero diagnostic signal anywhere -- neither the "fresh access token acquired" success log nor
// the "GSL refresh failed; following redirect with stale token anyway" failure log ever fired.
// GetServerList must fail loud instead of returning a zero-valued success.
func TestGetServerListBinFieldPresentButEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"bin":"","code":"0"}`)
	}))
	defer server.Close()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err == nil {
		t.Fatalf("GetServerList: expected an error for a present-but-empty bin field, got nil (lsr=%+v)", lsr)
	}
	if !strings.Contains(err.Error(), "bin field present but empty") {
		t.Errorf("GetServerList error = %q, want it to mention the empty-bin decode-failure branch", err)
	}
}

// TestGetServerListCodeAcceptsStringOrNumber proves LoginServerListRespon.Code decodes both a
// string-typed and a bare-number `code` field without error, mirroring
// TestFlexStringUnmarshalJSON's coverage of flexString's string/number tolerance but exercised
// through LoginServerListRespon (and the full GetServerList round-trip) specifically. This is a
// regression guard for the Code int -> flexString change (see LoginServerListRespon's doc
// comment): getserverlist.php's `code` field hasn't itself been observed flipping type live yet,
// but its sibling endpoint (CheckVersionResponse.Code) has, and a bare int here would make a live
// string-typed code fail json.Unmarshal with an opaque type-mismatch error instead of decoding.
func TestGetServerListCodeAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name     string
		codeJSON string // raw JSON for the "code" field, embedded verbatim into the fake response
		want     string
	}{
		{"string code", `"0"`, "0"},
		{"numeric code", `301`, "301"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":%s,"serverList":[]}`, tt.codeJSON)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if lsr.Code.String() != tt.want {
				t.Errorf("Code = %q, want %q", lsr.Code.String(), tt.want)
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
