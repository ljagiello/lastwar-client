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
	"lastwar-client/internal/crypto"
	"log/slog"
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
		resp := CheckVersionResponse{ResMsg: flexString(pub)}
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
	if cv.ResMsg.String() != pub {
		t.Errorf("ResMsg mismatch: got %q, want %q", cv.ResMsg, pub)
	}
}

// TestCheckVersionResponseFieldsAcceptStringOrNumber is the round-41 regression test for the MAJOR
// finding that CheckVersionResponse.Msg/DownloadURL/ResMsg/HotUpdateMsg were still bare `string`
// fields while their siblings Code/UpdateType were already flexString -- a wrong-typed value on
// ANY field in this struct fails json.Unmarshal for the WHOLE response (flexString's own doc
// comment documents live evidence of exactly this endpoint sending a bare-string-typed field,
// code, as a JSON number instead). Sends msg/downloadurl/hotUpdateMsg as bare JSON numbers
// alongside a valid string-typed resMsg (the one field genuinely read, by crypto.ParseRSAPubKeyFromDER)
// and proves the whole response still decodes successfully, with ResMsg's real value intact.
func TestCheckVersionResponseFieldsAcceptStringOrNumber(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"msg":12345,"downloadurl":67890,"resMsg":%q,"hotUpdateMsg":11111}`, pub)
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	cv, _, err := CheckVersion(defaultHTTPClient())
	if err != nil {
		t.Fatalf("CheckVersion: %v", err)
	}
	if cv.ResMsg.String() != pub {
		t.Errorf("ResMsg mismatch: got %q, want %q", cv.ResMsg, pub)
	}
	if cv.Msg.String() != "12345" {
		t.Errorf("Msg.String() = %q, want %q", cv.Msg.String(), "12345")
	}
	if cv.DownloadURL.String() != "67890" {
		t.Errorf("DownloadURL.String() = %q, want %q", cv.DownloadURL.String(), "67890")
	}
	if cv.HotUpdateMsg.String() != "11111" {
		t.Errorf("HotUpdateMsg.String() = %q, want %q", cv.HotUpdateMsg.String(), "11111")
	}
}

// TestCheckVersionFallsBackToNextHostOnConnectionFailure is the round-41 regression test for the
// MINOR finding that every existing CheckVersion test overrides checkVersionHosts to a single-URL
// slice, so the multi-host fallback loop's continue branches (http.NewRequest error, httpClient.Do
// network error, io.ReadAll error) were never exercised with more than one host -- confirmed via
// mutation testing: short-circuiting the httpClient.Do-error branch from `continue` to an early
// `return nil, "", err` (abandoning the fallback loop on the very first host's failure) still
// passed the entire suite. The first host here is an address nothing listens on (127.0.0.1:1,
// a well-known privileged port real servers don't bind), so httpClient.Do fails immediately with
// a real connection-refused error -- not a synthetic stand-in -- and CheckVersion must still fall
// through to the second, working host and succeed.
func TestCheckVersionFallsBackToNextHostOnConnectionFailure(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := CheckVersionResponse{ResMsg: flexString(pub)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{"http://127.0.0.1:1", server.URL}
	defer func() { checkVersionHosts = origHosts }()

	cv, host, err := CheckVersion(defaultHTTPClient())
	if err != nil {
		t.Fatalf("CheckVersion: %v, want it to fall back to the second, working host", err)
	}
	if host != server.URL {
		t.Errorf("host = %q, want %q (the second host, since the first must have failed)", host, server.URL)
	}
	if cv.ResMsg.String() != pub {
		t.Errorf("ResMsg mismatch: got %q, want %q", cv.ResMsg, pub)
	}
}

// TestCheckVersionLogsEachHostFailureBeforeFallingBackToNext is the round-42 regression test for
// the MINOR finding that CheckVersion's multi-host fallback loop discarded every host's failure
// reason except the last: none of the loop's seven `continue` branches logged anything, so a
// caller only ever saw the LAST-tried host's error, even when an EARLIER host failed for a
// distinct, actionable reason (e.g. "API shape changed" vs. a later host's plain timeout). This
// specifically proves the earlier host's failure survives in the log even when the overall call
// SUCCEEDS via a later host -- the case where it's easiest to assume nothing worth logging
// happened at all, since the caller never sees an error.
func TestCheckVersionLogsEachHostFailureBeforeFallingBackToNext(t *testing.T) {
	badServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not valid json{{{")
	}))
	defer badServer.Close()

	pub := testRSAPubKeyDER(t)
	goodServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := CheckVersionResponse{ResMsg: flexString(pub)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer goodServer.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{badServer.URL, goodServer.URL}
	defer func() { checkVersionHosts = origHosts }()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	cv, host, err := CheckVersion(defaultHTTPClient())
	slog.SetDefault(orig)

	if err != nil {
		t.Fatalf("CheckVersion: %v, want it to succeed via the second host", err)
	}
	if host != goodServer.URL {
		t.Errorf("host = %q, want %q", host, goodServer.URL)
	}
	if cv.ResMsg.String() != pub {
		t.Errorf("ResMsg mismatch: got %q, want %q", cv.ResMsg, pub)
	}

	logged := buf.String()
	if !strings.Contains(logged, badServer.URL) || !strings.Contains(logged, "decode JSON") {
		t.Errorf("expected the first (failing) host's distinct decode-JSON failure to be logged even though the overall call succeeded via the second host, got log:\n%s", logged)
	}
}

// TestCheckVersionRejectsServerErrorCode is the round-38 regression test for the MAJOR finding
// that CheckVersion's server-rejection check (cv.Code != "") had zero test coverage -- confirmed
// via mutation testing (disabling the check entirely still passed the full suite). This is a
// confirmed-live rejection path, not speculative: CheckVersionResponse.Code's own doc comment
// states the server has been observed returning a non-empty numeric code (e.g. 301) on rejection.
// Proves a non-empty code makes CheckVersion return an error naming the code/msg, not a
// nil-error CheckVersionResponse an unsuspecting caller might otherwise proceed with.
func TestCheckVersionRejectsServerErrorCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := CheckVersionResponse{Code: "301", Msg: "client update required"}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	cv, _, err := CheckVersion(defaultHTTPClient())
	if err == nil {
		t.Fatalf("CheckVersion: expected an error for a non-empty rejection code, got nil (cv=%+v)", cv)
	}
	if !strings.Contains(err.Error(), "301") {
		t.Errorf("CheckVersion error = %q, want it to mention the rejection code %q", err.Error(), "301")
	}
	if !strings.Contains(err.Error(), "client update required") {
		t.Errorf("CheckVersion error = %q, want it to mention the rejection message", err.Error())
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

// TestCheckVersionAcceptsExactlyMaxSizeResponse is the round-43 regression test for the MINOR
// finding that maxGSLResponseSize's strict greater-than boundary (len(body) > maxGSLResponseSize,
// gsl.go) was only tested on the rejection side (TestCheckVersionRejectsOversizedResponse above,
// maxGSLResponseSize+1 bytes) -- no test proved a response of EXACTLY maxGSLResponseSize bytes is
// accepted by the size gate, which would catch an off-by-one `>=` mutation that rejected the
// boundary value itself. Builds a real, minimal, successfully-decodable CheckVersionResponse
// padded via its own resMsg field to land at exactly maxGSLResponseSize bytes, so a passing test
// proves the size gate accepted it AND the response still decoded correctly -- not just that some
// later, unrelated failure happened to also return a non-"byte limit" error.
func TestCheckVersionAcceptsExactlyMaxSizeResponse(t *testing.T) {
	const prefix = `{"code":"","resMsg":"`
	const suffix = `"}`
	padLen := maxGSLResponseSize - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("a", padLen) + suffix
	if len(body) != maxGSLResponseSize {
		t.Fatalf("constructed test body is %d bytes, want exactly %d", len(body), maxGSLResponseSize)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	cv, host, err := CheckVersion(defaultHTTPClient())
	if err != nil {
		t.Fatalf("CheckVersion() error = %v, want nil for a response body of exactly maxGSLResponseSize bytes (the boundary value, not over the cap)", err)
	}
	if host != server.URL {
		t.Errorf("host = %q, want %q", host, server.URL)
	}
	if got := len(cv.ResMsg); got != padLen {
		t.Errorf("len(ResMsg) = %d, want %d -- the response should have decoded correctly, not just avoided the size-limit error", got, padLen)
	}
}

// TestCheckVersionRejectsNon200Status is the round-40 regression test for the MINOR finding that
// CheckVersion's non-200 HTTP status branch (`resp.StatusCode != http.StatusOK`) had zero test
// coverage, unlike its size-limit and server-error-code sibling branches above. Confirms a bare
// HTTP 500 (no server-error-code JSON body at all) makes CheckVersion return an error naming both
// the status code and the response body, not silently proceed to decode a body that was never
// meant to be read as a success response.
func TestCheckVersionRejectsNon200Status(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "internal server error")
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	_, _, err := CheckVersion(defaultHTTPClient())
	if err == nil {
		t.Fatal("CheckVersion: expected an error for a non-200 HTTP status, got nil")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("CheckVersion error = %q, want it to mention the HTTP status code 500", err.Error())
	}
	if !strings.Contains(err.Error(), "internal server error") {
		t.Errorf("CheckVersion error = %q, want it to mention the response body", err.Error())
	}
}

// TestCheckVersionRejectsMalformedJSON is the round-40 regression test for the MINOR finding that
// CheckVersion's json.Unmarshal error branch had zero test coverage, unlike its size-limit,
// server-error-code, and (see TestCheckVersionRejectsNon200Status above) non-200-status sibling
// branches. Confirms an HTTP-200 response whose body isn't valid JSON at all makes CheckVersion
// return a clear decode error instead of a corrupted/zero-valued CheckVersionResponse.
func TestCheckVersionRejectsMalformedJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "not valid json{{{")
	}))
	defer server.Close()

	origHosts := checkVersionHosts
	checkVersionHosts = []string{server.URL}
	defer func() { checkVersionHosts = origHosts }()

	_, _, err := CheckVersion(defaultHTTPClient())
	if err == nil {
		t.Fatal("CheckVersion: expected an error for a malformed JSON body, got nil")
	}
	if !strings.Contains(err.Error(), "decode JSON") {
		t.Errorf("CheckVersion error = %q, want it to mention the JSON decode failure", err.Error())
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
				{ID: flexPort(1), Name: "test-server", IP: "1.2.3.4", Port: flexPort(17783), Zone: "APS1", GameUid: "g1", Status: "0"},
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

// TestGetServerListSuccessfulEncryptedRoundTrip is the round-51 regression test for the MAJOR
// finding that GetServerList's actual production code path -- a "bin" field present, non-empty,
// and successfully AES-decrypted and JSON-decoded -- had zero test coverage (gsl.go's
// "applyLoginServerFallback(&lsr, opt); return &lsr, nil" success return, reached only from inside
// the bin-present-and-decodable branch). TestGetServerListAgainstFakeServer above deliberately
// tests the PLAINTEXT-fallback path (no "bin" field at all); every "bin"-field test in
// TestGetServerListDecodeFailuresDoNotLeakRawResponse below deliberately injects a decode/decrypt
// failure; TestGSLCryptoRoundTrip (crypto_gsl_test.go) exercises the crypto primitives directly,
// never through GetServerList's own wiring. Against a real game server, every single login attempt
// goes through exactly this branch -- plays the server side of the real GSL crypto envelope (same
// recipe as TestGSLCryptoRoundTrip and this file's own decode-failure subtests below: recover the
// AES key from the client's RSA-encrypted uuid field, encrypt a well-formed reply with it), and
// asserts GetServerList actually returns the decrypted ServerList content, not just a nil error.
func TestGetServerListSuccessfulEncryptedRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		saltCT, err := crypto.URLSafeB64Decode(r.FormValue("uuid"))
		if err != nil {
			t.Errorf("decode uuid field: %v", err)
			return
		}
		salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
		if err != nil {
			t.Errorf("rsa decrypt salt: %v", err)
			return
		}
		key := crypto.MD5HexKey(string(salt))
		reply := LoginServerListRespon{
			Code: "0",
			ServerList: []LoginServerInfo{
				{ID: flexPort(1), Name: "test-server", IP: "1.2.3.4", Port: flexPort(17783), Zone: "APS1", GameUid: "g1", Status: "0"},
			},
			At: &LoginToken{Token: "tok-encrypted-round-trip"},
		}
		plain, err := json.Marshal(reply)
		if err != nil {
			t.Errorf("marshal reply: %v", err)
			return
		}
		encReply, err := crypto.AESECBEncryptPKCS7(plain, key)
		if err != nil {
			t.Errorf("aes encrypt reply: %v", err)
			return
		}
		fmt.Fprintf(w, `{"bin":%q}`, crypto.URLSafeB64Encode(encReply))
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList: %v", err)
	}
	if len(lsr.ServerList) != 1 || lsr.ServerList[0].IP.String() != "1.2.3.4" {
		t.Fatalf("got %+v, want a single server with IP 1.2.3.4", lsr.ServerList)
	}
	if lsr.At == nil || lsr.At.Token.String() != "tok-encrypted-round-trip" {
		t.Fatalf("got At = %+v, want Token %q", lsr.At, "tok-encrypted-round-trip")
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
				Port:   flexPort(12345),
				WsIP:   "9.9.9.9",
				WsPort: flexPort(12346),
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
	if got.IP != "9.9.9.9" || got.Port != flexPort(12345) || got.WsIP != "9.9.9.9" {
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
				Port: flexPort(12345),
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

// TestGetServerListLoginServerAtRtAcceptEmptyArrayShape is the round-44 regression test for the
// MINOR finding that LoginServerListRespon.LoginServer/At/Rt (unlike every scalar field in the
// same struct family) had no tolerance for a non-object JSON shape -- e.g. PHP's json_encode's
// common `[]` encoding for an empty associative array -- which used to fail json.Unmarshal for the
// ENTIRE GetServerList response. Sends a response with loginServer/at/rt all as `[]` instead of
// `{}`/absent, and confirms GetServerList still succeeds, with ServerList/Code/LastLoggedServer
// intact and LoginServer/At/Rt all nil -- the same "absent" behavior every consumer already
// expects (applyLoginServerFallback's `lsr.LoginServer == nil` check, login.go's/crossserver.go's/
// main.go's `lsr.At != nil` checks).
func TestGetServerListLoginServerAtRtAcceptEmptyArrayShape(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","serverList":[{"id":"1","ip":"1.2.3.4","port":"9000","zone":"APS1","gameUid":"g1"}],"lastLoggedServer":"1","loginServer":[],"at":[],"rt":[]}`)
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList() error = %v, want the malformed loginServer/at/rt shape to degrade gracefully, not fail the whole response", err)
	}
	if len(lsr.ServerList) != 1 {
		t.Fatalf("ServerList = %+v, want the single, well-formed entry to still decode", lsr.ServerList)
	}
	if lsr.LoginServer != nil {
		t.Errorf("LoginServer = %+v, want nil for a non-object []-shaped value", lsr.LoginServer)
	}
	if lsr.At != nil {
		t.Errorf("At = %+v, want nil for a non-object []-shaped value", lsr.At)
	}
	if lsr.Rt != nil {
		t.Errorf("Rt = %+v, want nil for a non-object []-shaped value", lsr.Rt)
	}
	if got := lsr.LastLoggedServer.String(); got != "1" {
		t.Errorf("LastLoggedServer.String() = %q, want %q", got, "1")
	}
}

// TestGetServerListServerListAcceptsObjectShape is the round-45 regression test for the MAJOR
// finding that LoginServerListRespon.ServerList -- unlike its sibling struct-pointer fields
// LoginServer/At/Rt, hardened against a non-array/non-object shape mismatch in round 44 -- had no
// tolerance at all for arriving as a JSON object instead of an array (e.g. PHP's json_encode's
// common encoding for a non-sequentially-keyed associative array), which used to fail
// json.Unmarshal for the ENTIRE GetServerList response with no recovery path anywhere (Login()
// returns the error immediately). Sends a response with serverList as `{}` instead of `[]`/absent,
// and confirms GetServerList still succeeds, with ServerList degrading to empty (the same "no
// servers returned" case Login()'s own len(lsr.ServerList)==0 check and applyLoginServerFallback's
// opt=new synthesis already handle) and Code/LastLoggedServer still intact.
func TestGetServerListServerListAcceptsObjectShape(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","serverList":{"101":{"id":"101","ip":"1.2.3.4","port":"9000","zone":"APS1","gameUid":"g1"}},"lastLoggedServer":"1"}`)
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList() error = %v, want the malformed object-shaped serverList to degrade gracefully, not fail the whole response", err)
	}
	if len(lsr.ServerList) != 0 {
		t.Errorf("ServerList = %+v, want empty for an object-shaped value", lsr.ServerList)
	}
	if got := lsr.Code.String(); got != "0" {
		t.Errorf("Code.String() = %q, want %q", got, "0")
	}
	if got := lsr.LastLoggedServer.String(); got != "1" {
		t.Errorf("LastLoggedServer.String() = %q, want %q", got, "1")
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

// TestGetServerListAcceptsExactlyMaxSizeResponse is
// TestCheckVersionAcceptsExactlyMaxSizeResponse's sibling for GetServerList's own
// io.ReadAll/LimitReader call site -- round-43 regression test for the MINOR finding that
// maxGSLResponseSize's strict greater-than boundary was only tested on the rejection side here
// too. Builds a real, minimal, plaintext (no "bin" field) LoginServerListRespon padded via its own
// lastLoggedServer field to land at exactly maxGSLResponseSize bytes.
func TestGetServerListAcceptsExactlyMaxSizeResponse(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	const prefix = `{"code":"0","serverList":[],"lastLoggedServer":"`
	const suffix = `"}`
	padLen := maxGSLResponseSize - len(prefix) - len(suffix)
	body := prefix + strings.Repeat("a", padLen) + suffix
	if len(body) != maxGSLResponseSize {
		t.Fatalf("constructed test body is %d bytes, want exactly %d", len(body), maxGSLResponseSize)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	defer server.Close()

	lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
	if err != nil {
		t.Fatalf("GetServerList() error = %v, want nil for a response body of exactly maxGSLResponseSize bytes (the boundary value, not over the cap)", err)
	}
	if got := len(lsr.LastLoggedServer); got != padLen {
		t.Errorf("len(LastLoggedServer) = %d, want %d -- the response should have decoded correctly, not just avoided the size-limit error", got, padLen)
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

// TestLoginServerListResponUnmarshalJSONNestedFailure is the round-50 regression test for
// LoginServerListRespon.UnmarshalJSON's nested-decode error branches (gsl.go: the
// "serverList:"/"loginServer:"/"at:"/"rt:"-prefixed fmt.Errorf wraps), which had zero test
// coverage: every existing LoginServerListRespon test only ever hands it well-formed JSON.
//
// Only the "serverList:" branch is actually reachable today, empirically confirmed (not assumed)
// by probing all four with deliberately malformed input before writing this test: LoginServerInfo,
// AccountServerInfo, and LoginToken all have EVERY field typed flexString specifically so a
// wrong-typed field value can never fail json.Unmarshal (flexString.UnmarshalJSON never itself
// returns an error -- falls back to storing the raw bytes verbatim; see its own doc comment) -- so
// for loginServer/at/rt, once looksLikeJSONObject's leading-'{' shape check passes, decoding into
// their all-flexString target structs cannot fail regardless of what's nested inside. serverList
// decodes into []LoginServerInfo instead, a genuinely-typed slice: a value that passes
// looksLikeJSONArray's leading-'[' shape check but whose elements aren't JSON objects at all (e.g.
// bare numbers) still fails, since unmarshaling a JSON number into the LoginServerInfo struct type
// itself -- not one of its fields -- is what json.Unmarshal rejects.
func TestLoginServerListResponUnmarshalJSONNestedFailure(t *testing.T) {
	const body = `{"code":"0","serverList":[1,2,3]}`
	var l LoginServerListRespon
	err := json.Unmarshal([]byte(body), &l)
	if err == nil {
		t.Fatalf("Unmarshal(%s) error = nil, want an error for a serverList array of non-objects", body)
	}
	if !strings.Contains(err.Error(), "serverList:") {
		t.Errorf("err = %v, want it prefixed with \"serverList:\" so the failing field is identifiable", err)
	}
}

// TestFlexStringInt is the round-36 regression test for the MAJOR finding that flexString.Int()
// (gsl.go) -- the accessor round 35 introduced specifically so a wrong-typed GetServerList field
// could fall back to 0 instead of fatally failing json.Unmarshal for the whole response -- had its
// own two non-happy-path branches (the empty-string fast path and the malformed-value fallback)
// completely uncovered and mutation-blind: every existing .Int() call site in other tests only
// ever exercises a valid numeric string. Every downstream caller's own "port <= 0"-style
// validation depends on the malformed/empty cases specifically falling back to 0, not some other
// value, so this pins that contract down directly.
func TestFlexStringInt(t *testing.T) {
	cases := []struct {
		name string
		f    flexString
		want int
	}{
		{"empty string", flexString(""), 0},
		{"valid numeric string", flexString("17783"), 17783},
		{"negative numeric string", flexString("-1"), -1},
		{"malformed, non-numeric string", flexString("not-a-number"), 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.f.Int("port"); got != c.want {
				t.Errorf("flexString(%q).Int() = %d, want %d", string(c.f), got, c.want)
			}
		})
	}
}

// TestFlexStringIntWarnsOnMalformedValue proves the malformed (non-numeric, non-empty) case logs
// a diagnostic -- distinct from the empty-string case, which is the ordinary "field absent"
// shape and stays silent by design, matching this codebase's established anomaly-diagnostic
// convention (e.g. getIntFlexible's own absent-vs-malformed distinction).
func TestFlexStringIntWarnsOnMalformedValue(t *testing.T) {
	run := func(t *testing.T, key string, f flexString) string {
		t.Helper()
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		f.Int(key)
		slog.SetDefault(orig)
		return buf.String()
	}

	t.Run("malformed value warns", func(t *testing.T) {
		logged := run(t, "port", flexString("not-a-number"))
		if !strings.Contains(logged, "not-a-number") {
			t.Errorf("expected a Warn mentioning the malformed value, got:\n%s", logged)
		}
	})
	t.Run("empty value stays silent", func(t *testing.T) {
		logged := run(t, "port", flexString(""))
		if logged != "" {
			t.Errorf("expected no log output for an empty/absent value, got:\n%s", logged)
		}
	})
}

// TestFlexStringIntRedactsSensitiveKeyValue is the round-42 regression test for the MINOR finding
// that flexString.Int()'s malformed-value Warn logged both the raw value AND (via strconv's own
// error text, which embeds the value a second time) the value again, with no isSensitiveSFSKey
// gate at all -- unlike this function's structural sibling getIntFlexible (same file), which
// received exactly this hardening in round 35. Both real call sites today (login.go/main.go's
// "port") are non-sensitive, so this wasn't exploitable in practice, but a future caller passing
// a sensitive key would otherwise leak its raw malformed value in cleartext, in both places it
// appears in the log line.
func TestFlexStringIntRedactsSensitiveKeyValue(t *testing.T) {
	run := func(t *testing.T, key string) string {
		t.Helper()
		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		flexString("not-a-number-secret-value").Int(key)
		slog.SetDefault(orig)
		return buf.String()
	}

	t.Run("sensitive key redacts", func(t *testing.T) {
		logged := run(t, "loginKey")
		if strings.Contains(logged, "not-a-number-secret-value") {
			t.Errorf("expected the malformed value to be redacted for a sensitive key, got:\n%s", logged)
		}
		if !strings.Contains(logged, "[REDACTED]") {
			t.Errorf("expected [REDACTED] in the log for a sensitive key, got:\n%s", logged)
		}
	})
	t.Run("non-sensitive key stays visible", func(t *testing.T) {
		logged := run(t, "port")
		if !strings.Contains(logged, "not-a-number-secret-value") {
			t.Errorf("expected the malformed value to stay visible for a non-sensitive key, got:\n%s", logged)
		}
	})
}

// TestLoginTokenStringGoStringRedact is the round-47 regression test for the MAJOR finding that
// LoginToken -- unlike the SFSObject/SFSArray/SFSValue family, which got exactly this
// redaction-by-construction treatment in rounds 14-15 -- carried a live bearer access/refresh
// token with nothing structurally stopping a future call site from formatting it directly.
// Proves String()/GoString() redact both the bare value and, critically, a LoginToken NESTED
// inside a containing struct's %+v -- covering the concrete threat scenario the audit described
// (a future fmt.Errorf("...: %+v", lsr)-shaped call site on the whole LoginServerListRespon).
func TestLoginTokenStringGoStringRedact(t *testing.T) {
	const liveToken = "FAKE-LIVE-BEARER-TOKEN-must-not-leak-xyz789"
	tok := LoginToken{Token: flexString(liveToken), Time: "12345"}

	t.Run("String", func(t *testing.T) {
		s := tok.String()
		if strings.Contains(s, liveToken) {
			t.Errorf("String() = %q, must not contain the live token", s)
		}
	})
	t.Run("GoString", func(t *testing.T) {
		s := tok.GoString()
		if strings.Contains(s, liveToken) {
			t.Errorf("GoString() = %q, must not contain the live token", s)
		}
	})
	t.Run("nested inside a containing struct's %+v", func(t *testing.T) {
		lsr := LoginServerListRespon{Code: "0", At: &tok}
		formatted := fmt.Sprintf("%+v", lsr)
		if strings.Contains(formatted, liveToken) {
			t.Errorf("fmt.Sprintf(%%+v, lsr) = %q, must not contain the live token nested in .At", formatted)
		}
	})
	t.Run("via fmt.Errorf %v", func(t *testing.T) {
		err := fmt.Errorf("refresh failed: %v", tok)
		if strings.Contains(err.Error(), liveToken) {
			t.Errorf("err = %q, must not contain the live token", err.Error())
		}
	})
}

// TestGSLOptStringGoStringRedact is the round-48 regression test for the MINOR finding that
// GSLOpt -- which carries LoginKey/Rt, live credentials -- had no String()/GoString() redaction,
// the same class of gap round 47/48 closed for LoginToken/deviceIdentity/SessionConfig.
func TestGSLOptStringGoStringRedact(t *testing.T) {
	const liveLoginKey = "FAKE-LIVE-LOGIN-KEY-must-not-leak-def456"
	o := GSLOpt{Opt: "login", LoginKey: liveLoginKey}

	if s := o.String(); strings.Contains(s, liveLoginKey) {
		t.Errorf("String() = %q, must not contain the live loginKey", s)
	}
	if s := o.GoString(); strings.Contains(s, liveLoginKey) {
		t.Errorf("GoString() = %q, must not contain the live loginKey", s)
	}
	if s := fmt.Sprintf("%+v", struct{ O GSLOpt }{O: o}); strings.Contains(s, liveLoginKey) {
		t.Errorf("fmt.Sprintf(%%+v, wrapper) = %q, must not contain the live loginKey nested in .O", s)
	}
}

// TestGetServerListDecodeFailuresDoNotLeakRawResponse is the round-11/round-12 regression test for
// gsl.go's response-error branches: a real getserverlist.php response legitimately carries a live
// at/rt session token on success (LoginServerListRespon.At/Rt), so none of these branches may embed
// the raw body/decrypted plaintext in the returned error. Three subtests force a different one of
// GetServerList's several json.Unmarshal calls to fail while a fake token is present in the
// response; a fourth (round 12) covers the sibling HTTP-status-error branch, which round 11 missed.
// Each asserts the fake token never appears in the resulting error text.
//
// The "bin field wrong-typed" subtest below used to type-mismatch LoginServerListRespon.ServerList
// directly instead -- round 45 made that field JSON-shape-tolerant (degrading gracefully instead of
// erroring, closing the same gap round 44 closed for its sibling LoginServer/At/Rt fields), so a
// type-mismatched serverList no longer fails GetServerList at all. The whole LoginServerListRespon
// struct family is now, by design, effectively immune to type mismatches after 12 rounds of
// hardening -- this subtest exercises the still-genuinely-fallible "bin" field decode instead, one
// level up from LoginServerListRespon's own UnmarshalJSON.
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

	t.Run("bin field wrong-typed", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Valid top-level JSON, but "bin" is present and not a JSON string -- a genuine type
			// mismatch that still fails json.Unmarshal(binRaw, &binStr) (gsl.go's "decode bin
			// field" branch), unrelated to LoginServerListRespon's own JSON-shape tolerance
			// (round 44/45 widened ServerList/LoginServer/At/Rt to degrade gracefully instead of
			// failing, but "bin" is decoded separately, before LoginServerListRespon's own
			// UnmarshalJSON is ever reached). This subtest used to type-mismatch "serverList"
			// directly instead, but round 45 made that field shape-tolerant too (degrading to an
			// empty ServerList instead of erroring), so it no longer exercises a decode failure at
			// all -- the whole struct family has, by design, become effectively immune to type
			// mismatches after 12 rounds of hardening.
			fmt.Fprintf(w, `{"bin":12345,"note":"carries %s"}`, fakeToken)
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
			saltCT, err := crypto.URLSafeB64Decode(r.FormValue("uuid"))
			if err != nil {
				t.Errorf("decode uuid field: %v", err)
				return
			}
			salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
			if err != nil {
				t.Errorf("rsa decrypt salt: %v", err)
				return
			}
			key := crypto.MD5HexKey(string(salt))
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
			// applied directly to crypto.PKCS7Unpad).
			plain := make([]byte, bs)
			copy(plain, fakeToken)
			plain[bs-4] = 5
			plain[bs-3] = 4
			plain[bs-2] = 4
			plain[bs-1] = 4

			ct := make([]byte, bs)
			block.Encrypt(ct, plain)
			fmt.Fprintf(w, `{"bin":%q}`, crypto.URLSafeB64Encode(ct))
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

	t.Run("decrypted plaintext malformed JSON syntax", func(t *testing.T) {
		priv, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatalf("generate RSA key: %v", err)
		}

		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Play the server side of the real GSL crypto envelope (same recipe as
			// TestGSLCryptoRoundTrip in crypto_gsl_test.go): recover the AES key from the
			// client's RSA-encrypted uuid field, then encrypt a malformed reply with it so
			// GetServerList reaches its decrypted-plaintext decode-failure branch instead of the
			// plaintext-fallback one above.
			//
			// Genuinely malformed JSON SYNTAX (an unterminated array), not just a type mismatch --
			// round 44/45 made every field in LoginServerListRespon (ServerList/LoginServer/At/Rt,
			// on top of every scalar field already widened to flexString in rounds 33-43)
			// JSON-shape-tolerant, degrading gracefully instead of failing json.Unmarshal, so a
			// type-mismatched value (this subtest's original shape) no longer reaches this branch's
			// decode-failure path at all. Only a genuine syntax error still does.
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
				return
			}
			saltCT, err := crypto.URLSafeB64Decode(r.FormValue("uuid"))
			if err != nil {
				t.Errorf("decode uuid field: %v", err)
				return
			}
			salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
			if err != nil {
				t.Errorf("rsa decrypt salt: %v", err)
				return
			}
			key := crypto.MD5HexKey(string(salt))
			reply := fmt.Sprintf(`{"code":"0","serverList":[ carries %s`, fakeToken)
			encReply, err := crypto.AESECBEncryptPKCS7([]byte(reply), key)
			if err != nil {
				t.Errorf("aes encrypt reply: %v", err)
				return
			}
			fmt.Fprintf(w, `{"bin":%q}`, crypto.URLSafeB64Encode(encReply))
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

// TestGetServerListPortIDAcceptsStringOrNumber is the round-35 regression test for the MAJOR
// finding: LoginServerInfo.ID/Port used to be plain int, so a wrong-typed value on either field (a
// JSON string -- the same shape LoginServerInfo.Status and LoginServerListRespon.Code are already
// confirmed-live to sometimes arrive as, on this exact endpoint/struct family) failed
// json.Unmarshal for the ENTIRE GetServerList response -- fatal on the primary login path
// (login.go's Login) and the standalone -cs-rt refresh command (main.go), neither of which has a
// fallback for a GetServerList error. Mirrors TestGetServerListCodeAcceptsStringOrNumber's raw-JSON
// table shape, but for id/port specifically, and additionally proves flexString.Int() recovers the
// correct integer value (not just that decoding didn't error).
func TestGetServerListPortIDAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string // raw JSON for the "id"/"port" fields, embedded verbatim into the fake response
	}{
		{"string id/port", `"17783"`},
		{"numeric id/port", `17783`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[{"id":%s,"port":%s,"zone":"APS1","gameUid":"g1","status":"0"}]}`, tt.json, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single entry", lsr.ServerList)
			}
			got := lsr.ServerList[0]
			if got.Port.Int("port") != 17783 {
				t.Errorf("Port.Int() = %d, want 17783", got.Port.Int("port"))
			}
			if got.ID.Int("id") != 17783 {
				t.Errorf("ID.Int() = %d, want 17783", got.ID.Int("id"))
			}
		})
	}
}

// TestGetServerListUidAcceptsStringOrNumber is the round-37 regression test for the MAJOR finding
// that LoginServerInfo.Uid was still a plain string, not flexString, while every other
// numeric-looking sibling field on the same struct (ID/Port/Status) was already hardened in
// rounds 33-36 -- a bare-numeric "uid" on any serverList entry used to fail json.Unmarshal for
// the ENTIRE GetServerList response. Mirrors TestGetServerListPortIDAcceptsStringOrNumber's
// raw-JSON table shape.
func TestGetServerListUidAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string uid", `"12345"`},
		{"numeric uid", `12345`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[{"id":"1","port":"9000","zone":"APS1","gameUid":"g1","uid":%s,"status":"0"}]}`, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single entry", lsr.ServerList)
			}
			if got := lsr.ServerList[0].Uid.String(); got != "12345" {
				t.Errorf("Uid.String() = %q, want %q", got, "12345")
			}
		})
	}
}

// TestGetServerListGameUidAcceptsStringOrNumber is the round-40 regression test for the MAJOR
// finding that LoginServerInfo.GameUid was still a plain string, not flexString, while its
// siblings ID/Port/Uid/Status on the same struct were already hardened in rounds 33-37 -- a
// bare-numeric "gameUid" on any serverList entry used to fail json.Unmarshal for the ENTIRE
// GetServerList response, fatal on the primary login path (login.go's Login). Unlike Uid,
// GameUid is genuinely read (login.go/crossserver.go/main.go all consume it), so this also
// proves the value survives all the way through GetServerList's caller-facing accessor intact.
func TestGetServerListGameUidAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string gameUid", `"2931530835002297"`},
		{"numeric gameUid", `2931530835002297`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[{"id":"1","port":"9000","zone":"APS1","gameUid":%s,"uid":"1","status":"0"}]}`, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single entry", lsr.ServerList)
			}
			if got := lsr.ServerList[0].GameUid.String(); got != "2931530835002297" {
				t.Errorf("GameUid.String() = %q, want %q", got, "2931530835002297")
			}
		})
	}
}

// TestGetServerListNameIPWsIPZoneAcceptsStringOrNumber is the round-42 regression test for the
// MAJOR finding that LoginServerInfo.Name/IP/WsIP/Zone were still bare string fields while every
// other field on the same struct (ID/Port/GameUid/Uid/Status) was already hardened across rounds
// 33-41 -- a bare-numeric value on ANY of these four fields used to fail json.Unmarshal for the
// ENTIRE GetServerList response, fatal on the primary login path. Zone is genuinely read (Login
// reads it as the redial zone and resends it as the wire "zn" field), so this also proves the
// value survives intact through GetServerList's caller-facing accessor via flexString.String().
func TestGetServerListNameIPWsIPZoneAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string values", `"1001"`},
		{"numeric values", `1001`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[{"id":"1","name":%s,"ip":%s,"ws_ip":%s,"port":"9000","zone":%s,"gameUid":"g1","uid":"1","status":"0"}]}`,
					tt.json, tt.json, tt.json, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single entry", lsr.ServerList)
			}
			got := lsr.ServerList[0]
			if got.Name.String() != "1001" {
				t.Errorf("Name.String() = %q, want %q", got.Name.String(), "1001")
			}
			if got.IP.String() != "1001" {
				t.Errorf("IP.String() = %q, want %q", got.IP.String(), "1001")
			}
			if got.WsIP.String() != "1001" {
				t.Errorf("WsIP.String() = %q, want %q", got.WsIP.String(), "1001")
			}
			if got.Zone.String() != "1001" {
				t.Errorf("Zone.String() = %q, want %q", got.Zone.String(), "1001")
			}
		})
	}
}

// TestGetServerListAccountServerInfoPortWsPortAcceptsStringOrNumber is
// TestGetServerListPortIDAcceptsStringOrNumber's sibling for AccountServerInfo.Port/WsPort,
// exercised through the opt=new applyLoginServerFallback path that actually reads them (see
// TestGetServerListFallsBackToLoginServerWhenServerListEmpty).
func TestGetServerListAccountServerInfoPortWsPortAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string port", `"8443"`},
		{"numeric port", `8443`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[],"loginServer":{"ip":"1.2.3.4","port":%s,"ws_ip":"1.2.3.4","ws_port":%s}}`, tt.json, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single synthesized entry from LoginServer", lsr.ServerList)
			}
			if got := lsr.ServerList[0].Port.Int("port"); got != 8443 {
				t.Errorf("synthesized ServerList[0].Port.Int() = %d, want 8443", got)
			}
			if got := lsr.LoginServer.WsPort.Int("ws_port"); got != 8443 {
				t.Errorf("LoginServer.WsPort.Int() = %d, want 8443", got)
			}
		})
	}
}

// TestGetServerListAccountServerInfoIPWsIPAcceptsStringOrNumber is
// TestGetServerListAccountServerInfoPortWsPortAcceptsStringOrNumber's sibling for
// AccountServerInfo.IP/WsIP -- round-42 regression test for the MAJOR finding that these two
// fields were still bare string while Port/WsPort on the same struct were already flexString.
// applyLoginServerFallback copies AccountServerInfo.IP directly into the synthesized
// LoginServerInfo.IP (also flexString as of this round), so this proves the value round-trips
// intact through both structs with no conversion loss.
func TestGetServerListAccountServerInfoIPWsIPAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string ip", `"1001"`},
		{"numeric ip", `1001`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[],"loginServer":{"ip":%s,"port":"9000","ws_ip":%s,"ws_port":"9000"}}`, tt.json, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if len(lsr.ServerList) != 1 {
				t.Fatalf("ServerList = %+v, want a single synthesized entry from LoginServer", lsr.ServerList)
			}
			if got := lsr.ServerList[0].IP.String(); got != "1001" {
				t.Errorf("synthesized ServerList[0].IP.String() = %q, want %q", got, "1001")
			}
			if got := lsr.LoginServer.WsIP.String(); got != "1001" {
				t.Errorf("LoginServer.WsIP.String() = %q, want %q", got, "1001")
			}
		})
	}
}

// TestGetServerListLoginTokenTimeAcceptsStringOrNumber is the round-36 regression test for the
// MAJOR finding that LoginToken.Time was the one field round 35's GetServerList JSON
// type-safety sweep missed -- a wrong-typed "time" value on either "at" or "rt" used to fail
// json.Unmarshal for the ENTIRE GetServerList response (fatal on the primary login path and the
// standalone -cs-rt refresh command, same failure mode as the already-fixed
// LoginServerInfo.ID/Port and AccountServerInfo.Port/WsPort). Mirrors
// TestGetServerListPortIDAcceptsStringOrNumber's raw-JSON table shape, exercised through "at".
func TestGetServerListLoginTokenTimeAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string time", `"1699999999999"`},
		{"numeric time", `1699999999999`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[],"at":{"token":"abc","time":%s}}`, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if lsr.At == nil {
				t.Fatal("lsr.At is nil, want a decoded LoginToken")
			}
			if got := lsr.At.Token.String(); got != "abc" {
				t.Errorf("lsr.At.Token.String() = %q, want %q", got, "abc")
			}
			if got := lsr.At.Time.String(); got != "1699999999999" {
				t.Errorf("lsr.At.Time.String() = %q, want %q", got, "1699999999999")
			}
		})
	}
}

// TestGetServerListLoginTokenTokenAcceptsStringOrNumber is the round-43 regression test for the
// MAJOR finding that LoginToken.Token -- the one field actually READ (unlike its sibling Time,
// widened round-36 purely so it couldn't take the rest of the struct down) -- was the LAST bare
// string field left in the entire GetServerList/CheckVersion response family after rounds 33-42
// widened every other field. A wrong-typed "token" value used to fail json.Unmarshal for the
// ENTIRE GetServerList response, fatal on the primary Login() path (which has no fallback) and
// main.go's standalone -cs-rt command (which os.Exit(1)s on decode failure). Mirrors
// TestGetServerListLoginTokenTimeAcceptsStringOrNumber's table shape, exercised through "token"
// instead of "time".
func TestGetServerListLoginTokenTokenAcceptsStringOrNumber(t *testing.T) {
	tests := []struct {
		name string
		json string
	}{
		{"string token", `"tok-abc"`},
		{"numeric token", `301`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			priv, err := rsa.GenerateKey(rand.Reader, 2048)
			if err != nil {
				t.Fatalf("generate RSA key: %v", err)
			}

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"code":"0","serverList":[],"at":{"token":%s,"time":"1699999999999"}}`, tt.json)
			}))
			defer server.Close()

			lsr, err := GetServerList(defaultHTTPClient(), server.URL, &priv.PublicKey, "test-device", GSLOpt{Opt: "new"}, "", "")
			if err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if lsr.At == nil {
				t.Fatal("lsr.At is nil, want a decoded LoginToken")
			}
			want := strings.Trim(tt.json, `"`)
			if got := lsr.At.Token.String(); got != want {
				t.Errorf("lsr.At.Token.String() = %q, want %q", got, want)
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
