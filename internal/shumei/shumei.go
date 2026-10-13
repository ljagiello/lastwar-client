// Package shumei reimplements the ShuMei (数美) device-fingerprinting
// registration flow the SDK performs against /deviceprofile/v4, enough to
// obtain a real `shumeiBoxId` for the Login request — instead of relying on
// the empty placeholder the game server also accepts.
//
// The Login's `shumeiBoxId` is what the SDK's getDeviceId() returns: `"B"` +
// the server-assigned `smid` (88 chars) once one exists, or a `"D"`-prefixed
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
//     the native `w1`), then protects it with a hybrid scheme: a 16-char
//     random nonce `r` whose MD5 (lowercase hex) is the AES-256 key, the
//     device-data JSON raw-deflated and AES-256-CBC encrypted under that key
//     (IV "0102030405060708"), and `r` itself RSA-OAEP(SHA-256)-wrapped with
//     the embedded certificate as `ep`.
//
//  3. The profile JSON is POSTed to /deviceprofile/v4 as
//     Content-Type: application/octet-stream. The body is
//     {organization, os, appId, encode:2, compress:3, data, tn, ep}.
//
//  4. The reply is {"code":<int>,"detail":{"deviceId":...},
//     "requestId":"..."}. code 1100 success, 1902 the server's generic
//     "can't use this" reply (any body it can't decode), 1903 hard error.
//
// Status (live-tested 2026-10-11): the full device-data fingerprint below,
// protected with the hybrid scheme, returns code 1100 and a real smid. The
// server validates the protection: an empty or minimal `data`/`tn`/`ep`
// returns 1902. The device-data template is a spoofed Android device profile
// (the client runs on a desktop, not a phone), so the smid it mints is for a
// device that does not exist — see the note in BoxID.
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
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
)

// Organization and AppID are the values the shipped game (Last War)
// configures the SDK with in SmSdkManager.callCreate(). They are
// app-specific identifiers, not SDK secrets.
const (
	Organization = "9dNCYkjHeFMYZMOjxgX8"
	AppID        = "default"
)

// OSName is the device-profile's `os` field (l11l11lI1lll.l111l1111l1Il). The
// SDK version "3.14.3" (l11l11lI1lll.l111l11111I1l) is not a deviceprofile
// field: it is the cloudconf request's `sdkver`, and here only the `a7` slot
// inside the protected device data (with "build1" in `a136`).
const OSName = "android"

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

// EncodeShape/CompressShape are the numeric values the native `w1` writes into
// the profile's `encode`/`compress` fields (see the FUN_00180d30(2.0, ...) and
// FUN_00180d30(3.0, ...) calls in the decompiled libsmsdk.so): "base64" and
// "deflate" in the SDK's protection pipeline.
const (
	EncodeShape   = 2
	CompressShape = 3
)

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

// DeviceProfile is the JSON body POSTed to /deviceprofile/v4. Field names are
// byte-exact from the native `w1` (FUN_0010c160 in the decompiled libsmsdk.so):
// organization, os, appId, encode (2), compress (3), data, tn, ep. The first
// request carries no `retry` field (only the background resend adds it).
type DeviceProfile struct {
	Organization string `json:"organization"`
	OS           string `json:"os"`
	AppID        string `json:"appId"`
	Encode       int    `json:"encode"`   // 2 = base64
	Compress     int    `json:"compress"` // 3 = deflate
	Data         string `json:"data"`     // AES-256-CBC(raw-deflate(aNN-json))
	TN           string `json:"tn"`       // RSA-OAEP(cert, "0"), ignored by the server
	EP           string `json:"ep"`       // RSA-OAEP(cert, r), the AES key source
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

// randomNonce returns a 16-char random alphanumeric string, the `r` the native
// `w1` generates per request.
func randomNonce() (string, error) {
	const alnum = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 16)
	for i := range b {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alnum))))
		if err != nil {
			return "", fmt.Errorf("shumei: random nonce: %w", err)
		}
		b[i] = alnum[n.Int64()]
	}
	return string(b), nil
}

// aesKeyFromNonce derives the AES-256 key from `r`: MD5(r) as lowercase hex
// ASCII (32 bytes). This is the native `w1` scheme (FUN_0010c160).
func aesKeyFromNonce(r string) []byte {
	sum := md5.Sum([]byte(r))
	return []byte(hex.EncodeToString(sum[:]))
}

// pkcs7Pad pads data to a multiple of blockSize using PKCS#7 (identical to the
// Java AES/CBC/PKCS5Padding the SDK uses).
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(data, bytes.Repeat([]byte{byte(padding)}, padding)...)
}

// protect reproduces the native `w1` protection pipeline for the device-data
// JSON: raw deflate -> AES-256-CBC (key = MD5(r), IV "0102030405060708") ->
// base64, plus RSA-OAEP(SHA-256) of `r` (as `ep`) and of "0" (as `tn`).
func protect(pub *rsa.PublicKey, innerJSON string) (data, ep, tn string, err error) {
	r, err := randomNonce()
	if err != nil {
		return "", "", "", err
	}
	key := aesKeyFromNonce(r)

	// raw deflate (RFC 1951, no zlib header) — matches the SDK's deflate stage.
	var compressed bytes.Buffer
	zw, err := flate.NewWriter(&compressed, flate.DefaultCompression)
	if err != nil {
		return "", "", "", fmt.Errorf("shumei: deflate: %w", err)
	}
	if _, err := zw.Write([]byte(innerJSON)); err != nil {
		return "", "", "", fmt.Errorf("shumei: deflate write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return "", "", "", fmt.Errorf("shumei: deflate close: %w", err)
	}

	// AES-256-CBC with the fixed IV "0102030405060708".
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", "", "", fmt.Errorf("shumei: aes: %w", err)
	}
	iv := []byte("0102030405060708")
	padded := pkcs7Pad(compressed.Bytes(), block.BlockSize())
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	data = base64.StdEncoding.EncodeToString(ct)

	// RSA-OAEP(SHA-256, MGF1=SHA-256, empty label) of `r` and of "0".
	epBytes, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte(r), nil)
	if err != nil {
		return "", "", "", fmt.Errorf("shumei: rsa-oaep ep: %w", err)
	}
	tnBytes, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte("0"), nil)
	if err != nil {
		return "", "", "", fmt.Errorf("shumei: rsa-oaep tn: %w", err)
	}
	return data, base64.StdEncoding.EncodeToString(epBytes), base64.StdEncoding.EncodeToString(tnBytes), nil
}

// innerTemplate is the `aNN` device-data fingerprint the SDK's SubCollector
// fills. It is a spoofed Android device profile (a Samsung SM-G988N running
// Android 12), because this client runs on a desktop and has no real device
// telemetry. The `__DEVICE_ID__` placeholder is replaced with the caller's
// device ID so the fingerprint binds to the same identity as the Login request.
//
// The server validates the protection (an empty/minimal fingerprint returns
// 1902) but accepts this full template as-is, empty fields included.
const innerTemplate = `{
  "a1": "all", "a4": "1", "a5": "default",
  "a2": "__DEVICE_ID__",
  "a6": "android", "a9": 1784568338000, "a10": "12", "a7": "3.14.3",
  "a62": "/data/user/0/com.fun.lastwar.gp/files",
  "a64": { "enable": "0", "service": [], "suc": "1" },
  "a36": "1080,1920,320", "a45": "26006",
  "a143": "exynos990", "a33": "exynos990", "a34": 2457000, "a37": 128, "a32": 8, "a48": 8000000000,
  "a56": "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US",
  "a57": -1802451281, "a31": "192.168.1.100", "a11": "default", "a30": "\"Netia Mobile_5G\"", "a29": "",
  "a44": "WIFI",
  "a46": { "board": "z3q", "model": "SM-G988N", "brand": "samsung", "manufacturer": "samsung",
           "fingerprint": "samsung/z3qksx/z3q:12/NRD90M/1201240816:user/release-keys",
           "cpu_abi": "arm64-v8a", "cpu_abi2": "", "radioVersion": "" },
  "a47": [ "1,InvenSense", "4,AKM", "9,Bosch", "13,Bosch" ],
  "a18": { "ro.debuggable": "0", "ro.boot.hardware": "exynos990", "gsm.sim.state": "READY",
           "gsm.operator.alpha": "Netia", "sys.usb.state": "android,adb" },
  "a19": "b4:11:22:33:44:55", "a24": "8f7a9a1b751fa784", "a39": "com.fun.lastwar.gp", "a38": "1.0.352",
  "a40": 1784500000000, "a136": "build1", "a96": "9dNCYkjHeFMYZMOjxgX8",
  "a63": [ "com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME" ],
  "a68": [], "a69": 120000000000, "a70": 130000000000, "a71": 128000000000, "a127": 12,
  "a99": "", "a100": "", "a101": "", "a113": 0, "a130": 3, "a116": {}, "a117": false,
  "a120": { "mr": 0, "mt": 0, "tr": 123456789, "tt": 987654321 }, "a121": 0, "a124": "",
  "a125": { "fit": 1783000000000, "lut": 1784560000000 }, "a126": "", "a135": 0, "a139": 0, "a142": 1,
  "a153": {}, "a154": { "pkg": "com.sec.android.app.launcher", "label": "One UI Home", "versionName": "14.5.01.7" },
  "a133": 250, "a144": "", "a155": 0, "a160": 0, "a161": 0, "a162": [],
  "a163": "ffeeddccbbaa99887766554433221100", "a164": 0, "a165": 0,
  "a167": 1784500000000, "a168": 1784500000000, "a169": "", "a170": 12345678, "a171": 12345678,
  "a172": {}, "a173": 0, "a174": 1, "a15": "false",
  "a72": { "status": 2, "level": 85, "scale": 100, "temp": 320, "vol": 4200 },
  "a73": 0, "a74": 0, "a75": 0, "a76": "", "a77": {}, "a78": [], "a79": "",
  "a80": "__DEVICE_ID__", "a83": "1011100", "a84": "e2b31bdf2f77091e",
  "a85": [], "a86": "0111001",
  "a88": "locateServiceName:android.location.ILocationManager$Stub$Proxy|phoneServiceName:com.android.internal.telephony.ITelephony$Stub$Proxy",
  "a90": 32, "a92": "1080,1920", "a93": 1, "a97": "__DEVICE_ID__",
  "a102": [], "a103": "", "a107": {}, "a108": "", "a110": "", "a114": [], "a115": [],
  "a131": "pl", "a132": "pl", "a141": 1, "a156": [], "a157": 0, "a159": ""
}`

// deviceData fills innerTemplate's `__DEVICE_ID__` slots (a2, a80, a97) with
// deviceID, giving the device-data JSON that protect encrypts.
func deviceData(deviceID string) string {
	return strings.ReplaceAll(innerTemplate, "__DEVICE_ID__", deviceID)
}

// BoxID registers a device with the ShuMei backend and returns its assigned
// device fingerprint, already prefixed with `"B"` to match the Login request's
// `shumeiBoxId` field. deviceID should match the request's own deviceId so the
// server can bind the fingerprint to the same device identity.
//
// Note on scope: the device-data fingerprint this mints is a spoofed Android
// profile for a device that does not exist. The game server does not require a
// real shumeiBoxId (an empty one is accepted), so this is an optional
// best-effort path, not a hard gate.
func BoxID(client Client, deviceID string) (string, error) {
	pub, err := publicKey()
	if err != nil {
		return "", err
	}
	return boxID(client, pub, deviceID)
}

// boxID is BoxID with the RSA key as a parameter, the same way protect takes
// it, so a test can use its own key pair and decrypt the POSTed body.
func boxID(client Client, pub *rsa.PublicKey, deviceID string) (string, error) {
	data, ep, tn, err := protect(pub, deviceData(deviceID))
	if err != nil {
		return "", err
	}

	profile := DeviceProfile{
		Organization: Organization,
		OS:           OSName,
		AppID:        AppID,
		Encode:       EncodeShape,
		Compress:     CompressShape,
		Data:         data,
		TN:           tn,
		EP:           ep,
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
	return "B" + pr.Detail.DeviceID, nil
}

func truncate(b []byte) string {
	if len(b) > 256 {
		return string(b[:256]) + "..."
	}
	return string(b)
}
