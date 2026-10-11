// Package shumei reimplements the ShuMei (数美) device-fingerprinting
// registration flow the SDK performs against /deviceprofile/v4, enough to
// obtain a real `shumeiBoxId` for the Login request — instead of relying on
// the empty placeholder the game server already accepts.
//
// The Login's `shumeiBoxId` is what the SDK's getDeviceId() returns: `"B"` +
// the server-assigned `smid` (89 chars) once one exists, or a `"D"`-prefixed
// local fallback before that. It is never needed for the client to work: an
// empty `shumeiBoxId` is a first-class, accepted path (confirmed live).
//
// Reconstruction sources (all byte-exact where noted):
//
//   - the Obfuscator-LLVM-hardened `libsmsdk.so` (string decryption via
//     emulation of the 22 `.datadiv_decode*` init functions);
//   - `com.ishumei.smantifraud.SmAntiFraud` / `l11l11lI1lll` / `l111l1111lI1l`
//     / `l1l11I1l` / `l11l111llI1l` / `l11l11l1l1Il` / `l1l1l111Il`
//     decompiled from classes6.dex;
//   - `com.sdkmanager.SmSdkManager` (the game's own SDK wiring).
//
// The flow, end to end:
//
//  1. SmSdkManager configures the SDK: organization "9dNCYkjHeFMYZMOjxgX8",
//     appId "default", an X.509 certificate as the public key, area
//     AREA_FJNY, and it overrides the SDK's region-pinned fengkongcloud.com
//     hosts with a proxy at shumei-api.lastwargame.com:19091.
//
//  2. The SDK collects device data (the SubCollector/AbsDetector chain plus
//     the native `w1`), protects it with
//     RSA/ECB/OAEPWITHSHA-256ANDMGF1PADDING — RSA-OAEP with SHA-256 as both
//     the message digest and MGF1 hash, and an empty OAEP label (the Java
//     `OAEPParameterSpec("SHA-256","MGF1",MGF1ParameterSpec.SHA256,
//     PSource.PSpecified.DEFAULT)`), which is Go's
//     rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, msg, nil) — plus
//     AES-256-CBC for the payload (hybrid key wrap).
//
//  3. The profile JSON is POSTed to /deviceprofile/v4 as
//     Content-Type: application/octet-stream. The body is the first JSON
//     object native `w1` returns, sent as-is: Java adds nothing. The
//     `"retry":1` field is added only by the background resend
//     (`l11l11l1l1Il`), not by the first request.
//
//  4. The reply is {"code":<int>,"detail":{"deviceId":...},
//     "requestId":"..."}. code 1100 success, 1902 the server's generic
//     "can't use this" reply (any body it can't decode), 1903 hard error.
//
// Note on status: the full device-data collection that `w1` protects has not
// been reproduced, so BoxID has never returned a real smid against the live
// endpoint (it gets 1902). That is the intended stopping point: minting a
// real `smid` would mean forging a device fingerprint for ShuMei's
// anti-fraud service so it issues an ID for a device that does not exist,
// and the game server does not require it. BoxID degrades to an empty
// shumeiBoxId on failure, which is sufficient.
//
// Field NAMES (organization, os, sdkver, smid, appId, encode, data,
// appname, tn, ep, sessionId, wevent, md5, enc, pri, sid, bb, and the aNN
// device-data slots) are byte-exact from l111l1111lI1l.java. The exact set
// of aNN device-data sub-fields the SubCollector fills is not reproduced —
// the server has no ShuMei-specific block (docs/live-validation.mdx), so
// this package is a best-effort faithful registry, not a hard gate.
package shumei

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Organization and AppID are the values the shipped game (Last War)
// configures the SDK with in SmSdkManager.callCreate(). They are
// app-specific identifiers, not SDK secrets.
const (
	Organization = "9dNCYkjHeFMYZMOjxgX8"
	AppID        = "default"
)

// SDK version and OS, reported in the device-profile's `sdkver`/`os` fields.
// The `sdkver` field is "3.14.3" (l11l11lI1lll.l111l11111I1l); the "build1"
// part (l11l11lI1lll.l111l11111Il) is combined into "3.14.3_build1" only in
// SmAntiFraud.getSDKVersion(), a public API not sent in the profile. The os
// constant l11l11lI1lll.l111l1111l1Il is "android".
const (
	SDKVersion      = "3.14.3"
	SDKVersionBuild = "build1"
	SDKVersionFull  = "3.14.3_build1" // SmAntiFraud.getSDKVersion(), not the profile field
	OSName          = "android"
)

// Region the game selects (AREA_FJNY) and the numeric area code the SDK
// derives from it (l11l11lI1lll.l1111l111111Il: "bj"->"001", "xjp"->"010",
// "fjny"->"011"). The game overrides the region-pinned host with a proxy, so
// the area code matters only as documentation of the intended datasite.
const (
	RegionName = "fjny"
	RegionCode = "011"
)

// DefaultDeviceProfileURL is the game's ShuMei device-profile endpoint
// (SmSdkManager.setUrl). The SDK's own built-in hosts are region-pinned on
// fengkongcloud.com and listed below for the record; Last War bypasses them
// entirely with this proxy. setUsingHttps(true) is the scheme.
const DefaultDeviceProfileURL = "https://shumei-api.lastwargame.com:19091/deviceprofile/v4"

// The SDK's built-in region hosts (l11l11lI1lll.java). AREA_XJP ->
// fp-sa-it(.fengkongcloud.com) / api-fp-retry-sa; AREA_FJNY -> fp-na-it-acc /
// api-fp-retry-na; default (bj) -> fp-it / api-fp-retry-bj, each paired with
// an api-fp-retry-* retry host. The game sets its own URL above, so these are
// documentation of the SDK's datasites, not something this client connects to.
const (
	SDKHostBJ   = "fp-it.fengkongcloud.com"
	SDKHostXJP  = "fp-sa-it.fengkongcloud.com"
	SDKHostFJNY = "fp-na-it-acc.fengkongcloud.com"
)

// Response codes from l1l11I1l.java: 1100 is success, 1902 asks the client to
// retry later, 1903 is a hard error. The smid lives under the reply's
// "detail" (l1l11I1l.l11l1111I11l) object as "deviceId" (l11l111llI1l.java).
const (
	CodeSuccess = 1100
	CodeRetry   = 1902
	CodeError   = 1903
)

// EncodeShape is the numeric value the native `w1` writes into the profile's
// `encode` field (see the FUN_00180d30(2.0, ...) call in the decompiled
// libsmsdk.so), i.e. "base64" in the SDK's protection pipeline. `compress`
// (value 3) lives in the native sub-payload, not this top-level struct.
const EncodeShape = 2

// ishumeiSPKIBase64 is the 2048-bit RSA SubjectPublicKeyInfo extracted from
// the embedded X.509 certificate (CN=e.ishumei.com, OU=SM, C=CN) the game
// passes to SmOption.setPublicKey. The SDK parses the certificate via
// CertificateFactory("X.509").generateCertificate(...).getPublicKey() and
// encrypts the payload with this key's public half.
const ishumeiSPKIBase64 = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEAgtU+mYnt48Oq2QKKOKER" +
	"lBycUvFTynQvzoOrv172NxDsqco+IdfgFOB+WGrOQ3RRRdB/j8v+zS/9NZt4qJag" +
	"zxZytjb7OhcJ0vJAJLgPF4YcziZJHLSegBm36D7iCjUORsafVo2zDRU5bHTrMdLR" +
	"JN2dtGOOt9b23ClOmw1cIHUGY/uO/3WX0uIUNvvnIqGum2+ePzcEs6cILeCQZDJk" +
	"dNlPKUMxV1k/x+LGqy8H7tcjWgEW5gtCBD+qx0OxRZzzaakxdzVBjNDMWwPDUt0Q" +
	"Pm0QNeaspTibBDic9wAhJx6PPToq3YrkKBpPlQHtwpdyrJatfo/PoRd53lCVNC6Z" +
	"bwIDAQAB"

// MaxResponseSize bounds the device-profile HTTP response, mirroring
// gsl.MaxGSLResponseSize's rationale (a tiny JSON never needs more).
const MaxResponseSize = 1 << 20 // 1 MiB

// Client is the minimal client interface; *http.Client satisfies it. Keeping
// it as an interface lets tests inject a fake transport the same way the
// gsl/auth packages do.
type Client interface {
	Do(*http.Request) (*http.Response, error)
}

// DeviceProfile is a best-effort approximation of the JSON body POSTed to
// /deviceprofile/v4. Field names are byte-exact from l111l1111lI1l.java
// (organization, os, sdkver, smid, appId, encode, data, appname, tn, ep,
// sessionId, wevent, md5, enc, pri, sid, bb, and the aNN device-data fields).
// It is NOT the real envelope: the SDK posts the first JSON object native
// `w1` produces, with the subscribed device-data fields populated; this
// struct only covers the subset this client can reproduce. Following the
// real SDK, the first request carries no `retry` field.
type DeviceProfile struct {
	Organization string `json:"organization"`
	OS           string `json:"os"`
	SDKVersion   string `json:"sdkver"`
	SMID         string `json:"smid,omitempty"` // empty on first registration
	AppID        string `json:"appId"`
	Encode       int    `json:"encode"` // 2 = base64
	Data         string `json:"data"`   // protected device fingerprint
	AppName      string `json:"appname,omitempty"`
	TN           string `json:"tn,omitempty"`
	EP           string `json:"ep,omitempty"`
}

// ProfileResponse mirrors the device-profile reply: {code, detail{...},
// requestId}. `detail.deviceId` is the ShuMei device fingerprint (smid).
type ProfileResponse struct {
	Code   int `json:"code"`
	Detail struct {
		DeviceID    string `json:"deviceId"`
		OldDeviceID string `json:"oldDeviceId"`
		SID         string `json:"sid"`
		C           int    `json:"c"`
		T           int    `json:"t"`
	} `json:"detail"`
	RequestID string `json:"requestId"`
}

// ErrUnassigned is returned when the server replies without a deviceId (e.g.
// code 1902, its generic "can't use this body" response).
var ErrUnassigned = errors.New("shumei: device-profile reply carried no deviceId")

func publicKey() (*rsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(ishumeiSPKIBase64)
	if err != nil {
		return nil, fmt.Errorf("shumei: decode embedded public key: %w", err)
	}
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("shumei: parse embedded public key: %w", err)
	}
	rsaPub, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("shumei: embedded key is not RSA (got %T)", pub)
	}
	return rsaPub, nil
}

// protect reproduces the native `w1` protection pipeline for the device
// payload: zlib deflate -> RSA-OAEP(SHA-256, MGF1=SHA-256, empty label) ->
// standard base64. RSA-OAEP on a 2048-bit key carries ~190 bytes, so the
// payload must be small; the deflate stage exists precisely to keep it under
// that bound.
func protect(pub *rsa.PublicKey, plaintext []byte) (string, error) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	if _, err := zw.Write(plaintext); err != nil {
		return "", fmt.Errorf("shumei: deflate: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("shumei: deflate close: %w", err)
	}
	ct, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, buf.Bytes(), nil)
	if err != nil {
		return "", fmt.Errorf("shumei: rsa-oaep: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// DeviceFingerprint builds the small device payload the native `w1` protects
// and returns it encrypted+base64, ready for the profile's `data` field.
// deviceID is the client's own device ID (32 hex + "_n3d"), same one used
// across the GSL/Login request.
func DeviceFingerprint(deviceID string) (string, error) {
	pub, err := publicKey()
	if err != nil {
		return "", err
	}
	payload := map[string]any{
		"organization": Organization,
		"os":           OSName,
		"appId":        AppID,
		"deviceId":     deviceID,
		"t":            time.Now().Unix(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("shumei: marshal device payload: %w", err)
	}
	return protect(pub, raw)
}

// BoxID registers a device with the ShuMei backend and returns its assigned
// device fingerprint. The Login request expects getDeviceId()'s `"B"`+smid
// string; on success this returns the bare `detail.deviceId`, so the caller
// would need to prepend `"B"` to match the real client. deviceID should match
// the request's own deviceId so the server can bind the fingerprint to the
// same device identity.
func BoxID(client Client, deviceID string) (string, error) {
	enc, err := DeviceFingerprint(deviceID)
	if err != nil {
		return "", err
	}

	profile := DeviceProfile{
		Organization: Organization,
		OS:           OSName,
		SDKVersion:   SDKVersion,
		AppID:        AppID,
		Encode:       EncodeShape,
		Data:         enc,
	}
	body, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("shumei: marshal profile: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, DefaultDeviceProfileURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("shumei: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Connection", "Close")

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("shumei: post device profile: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseSize))
	if err != nil {
		return "", fmt.Errorf("shumei: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("shumei: unexpected status %d: %s", resp.StatusCode, truncate(raw))
	}

	var pr ProfileResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return "", fmt.Errorf("shumei: parse response: %w", err)
	}
	if pr.Detail.DeviceID == "" {
		return "", fmt.Errorf("%w (code=%d)", ErrUnassigned, pr.Code)
	}
	return pr.Detail.DeviceID, nil
}

func truncate(b []byte) string {
	if len(b) > 256 {
		return string(b[:256]) + "..."
	}
	return string(b)
}
