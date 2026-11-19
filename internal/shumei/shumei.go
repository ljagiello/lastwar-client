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
	"hash/fnv"
	"io"
	"math/big"
	mrand "math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// androidIDRe is Settings.Secure.ANDROID_ID: 16 lowercase hex digits, the a24
// the SDK sends unhashed (Last War doesn't set needUsingMD5).
var androidIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)

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

// TargetSDKVersion is a90, the app's ApplicationInfo.targetSdkVersion
// (l111l1111llIl.java:828-829, l111l11111lIl.java:405-411): the 1.0.370
// manifest's uses-sdk targetSdkVersion. It belongs to the build, so it moves
// with gsl.AppVersion when a new APK raises it.
const TargetSDKVersion = 36

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
// byte-exact from the native `w1` (FUN_0010f8d0 and the FUN_0010c160 it calls,
// in the decompiled libsmsdk.so):
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

// baseTemplate is the constant part of the `aNN` device-data fingerprint: the
// app-level and generic-Android slots that are the same on every Android 12
// phone of the same SDK version. The device-specific half (build, SoC, screen,
// sensors, launcher, SIM; see hardware.go) is filled by deviceData from the
// catalog entry selected for the deviceId, so two devices don't share one
// fixed phone profile. It started as C485's C# ShumeiInnerTemplate, shared in
// https://github.com/ljagiello/lastwar-client/discussions/21, and was checked
// slot by slot against the 1.0.370 SDK (3.14.3) collector, l111l1111llIl.java.
//
// deviceData adds the fields that change between collections: the stored smid
// (a2, a97), the clocks (a9, a40, a170, a171), the SDK session ID (a80), the
// collection time (a133), the app version (a38), the per-run readings
// (a37 brightness, a72 battery, a120 traffic, a69/a70 free storage), the Wi-Fi
// details (a19, a30, a31) when a Device carries them, the app's signing
// certificate (a56, a57) and the target SDK (a90). It also adds the per-install
// values from Device.Install: a24, a84, a125, a126, a163, a167, a168, a60 and
// the counters a4, a127 and a130.
//
// App-level slots, from the 1.0.370 manifest and SmSdkManager.callCreate():
//
//   - a83 is seven checkSelfPermission digits (l1111l111111Il.java:583-589):
//     READ_PHONE_STATE, WRITE_EXTERNAL_STORAGE, WRITE_SETTINGS,
//     ACCESS_WIFI_STATE, ACCESS_NETWORK_STATE, ACCESS_FINE_LOCATION,
//     ACCESS_COARSE_LOCATION. The manifest declares only the fourth and fifth
//     of those that can be granted (WRITE_EXTERNAL_STORAGE is declared but not
//     granted to a target-36 app), so "0001100".
//   - a86 is the option status string (SmAntiFraud.java:233-243): transport,
//     cloudConf, usingMD5, usingHttps, a callback, a non-empty notCollect,
//     checkCrt. SmSdkManager keeps the defaults, sets HTTPS, registers a
//     callback and two notCollect names, so "1101110".
//   - a5 is the option's channel (l111l1111llIl.java:807-808), which
//     SmSdkManager never sets: "" is dropped, so there is no a5. "default" is
//     a11, the appId.
//   - a141 counts displays and a156 lists them only when there is more than one
//     (l111l1111llIl.java:837-843); a phone has one, so a141 is 0 and a156 is
//     absent.
//
// The empty entries stay here to document which slots this device leaves
// empty. deviceData drops them, the way the SDK's serializer does. a63, the
// input-method list, is left out entirely: SmSdkManager puts "input" in the
// SDK's notCollect set, and the collector skips a63 when that set holds
// "input" (l111l1111llIl), so Last War never sends it. a85 is that set itself,
// which the collector reports unless the set holds "a85". The SDK merges the
// cloud config's exclusions into it, and until a cloudconf reply has been
// cached, that config is the SDK's built-in default (l11l111l1lll), which
// excludes "gps" and "tel". So a fresh install most likely sends {"input",
// "tel", "gps", "protect"}, in Java's HashSet order. A later server reply could
// change the cloud half; this client doesn't fetch the cloud config.
//
// a88 is left as it was: the SDK reports the class names of the "location" and
// "phone" service binders (l1111l111111Il.java:389-403), whose runtime value is
// Android framework behaviour no capture of this SDK build has shown yet.
const baseTemplate = `{
  "a1": "all",
  "a6": "android", "a7": "3.14.3",
  "a62": "/data/user/0/com.fun.lastwar.gp/files",
  "a64": { "enable": "0", "service": [], "suc": "1" },
  "a11": "default",
  "a44": "WIFI",
  "a39": "com.fun.lastwar.gp",
  "a136": "build1", "a96": "9dNCYkjHeFMYZMOjxgX8",
  "a68": [],
  "a99": "", "a100": "", "a101": "", "a113": 0, "a116": {}, "a117": false,
  "a121": 0, "a124": "",
  "a135": 0, "a139": 0, "a142": 1,
  "a153": {}, "a144": "", "a155": 0, "a160": 0, "a161": 0, "a162": [],
  "a164": 0, "a165": 0,
  "a169": "",
  "a172": {}, "a173": 0, "a174": 1, "a15": "false",
  "a73": 0, "a74": 0, "a75": 0, "a76": "", "a77": {}, "a78": [], "a79": "",
  "a83": "0001100",
  "a85": [ "input", "tel", "gps", "protect" ], "a86": "1101110",
  "a88": "locateServiceName:android.location.ILocationManager$Stub$Proxy|phoneServiceName:com.android.internal.telephony.ITelephony$Stub$Proxy",
  "a93": 1,
  "a102": [], "a103": "", "a107": {}, "a108": "", "a110": "", "a114": [], "a115": [],
  "a141": 0, "a157": 0, "a159": ""
}`

// Device holds the fingerprint values that change during a device's life: the
// clocks the SDK samples on every collection, the Wi-Fi details it reads when
// the app may see them, the readings it takes on every run, and the values
// one install keeps. BoxID and Register fill each zero clock, Wi-Fi IP and
// collection time with the host's real value or a random one, and draw the
// run's readings. They never make up install values: the caller passes them.
type Device struct {
	// Now is the collection time, a9 (System.currentTimeMillis()). Zero means
	// the current time.
	Now time.Time

	// BootTime is when the device booted, a40 (the SDK's median of 11
	// currentTimeMillis()-elapsedRealtime() samples). a171 (elapsedRealtime())
	// and a170 (uptimeMillis()) count from it. Zero means Now minus the host's
	// uptime.
	BootTime time.Time

	// AndroidID is a24, Settings.Secure.ANDROID_ID (16 lowercase hex digits).
	// It is one per device, not per install: the caller passes the device
	// identity's persisted value, so a24 and the Login's ta #device_id agree.
	// It must be set: BoxID refuses an empty one rather than invent it.
	AndroidID string

	// WiFi adds the connected network's details. Nil is what a real Last War
	// install sends: the SDK reads them only when the app holds
	// ACCESS_FINE_LOCATION (l1l11l1Illl), Last War doesn't request it, so a19,
	// a30 and a31 come back empty and the serializer leaves them out.
	WiFi *WiFi

	// Install is what this install keeps between registrations. It must be
	// filled (NewInstall or Install.Fill): BoxID refuses one with values unset
	// rather than invent a phone the caller never sees, so the install sent is
	// always the one the caller keeps.
	Install Install

	// CollectTime is how long the SDK took to collect the device data, a133
	// (the milliseconds from the start of l111l1111llIl's collection to the
	// end). Zero means a random time from 150 to 600 ms.
	CollectTime time.Duration

	// AppVersion is the app's versionName, a38 (PackageInfo.versionName): the
	// build the Login claims, so the fingerprint and the Login name the same
	// app. It must be set: BoxID refuses an empty one rather than guess.
	AppVersion string

	// brightness and battery are what the SDK reads at collection time:
	// Settings.System "screen_brightness" (a37, l1l11I111ll.java:50-63) and the
	// sticky ACTION_BATTERY_CHANGED extras (a72, l11l1111Il.java:17-28). They
	// move between runs on a real phone, so withHostDefaults draws them per run.
	brightness int
	battery    battery

	// storageUsed is how much the phone's free space shrank since a fixed
	// baseline (see storageBytes), drawn per run so a69/a70 aren't byte-equal
	// on every registration.
	storageUsed int64
}

// battery is a72: the status, level, scale, temperature (tenths of a degree
// Celsius) and voltage (mV) extras of Android's ACTION_BATTERY_CHANGED.
type battery struct {
	Status, Level, Scale, Temp, Voltage int
}

// Android's BatteryManager status values the per-run battery uses.
const (
	batteryCharging    = 2
	batteryDischarging = 3
	batteryFull        = 5
)

// randomBattery returns a plausible a72 reading: a level from 15 to 100 %, a
// charging, discharging or full status that fits it, 25 to 38 degrees and a
// voltage that follows the level the way a Li-ion cell's does.
func randomBattery() battery {
	b := battery{Level: 15 + mrand.N(86), Scale: 100, Temp: 250 + mrand.N(131)}
	switch {
	case b.Level == 100 && mrand.N(2) == 0:
		b.Status = batteryFull
	case mrand.N(4) == 0:
		b.Status = batteryCharging
	default:
		b.Status = batteryDischarging
	}
	b.Voltage = 3600 + b.Level*6 + mrand.N(60)
	return b
}

// WiFi is the connected network, as Android's WifiInfo reports it to an app
// holding the location permission.
type WiFi struct {
	SSID  string // a30, unquoted: getSSID() adds the quotes
	BSSID string // a19, the access point's MAC
	IP    string // a31, the device's IPv4 on that network; "" means the host's LAN address
}

// withHostDefaults fills d's zero clocks and Wi-Fi IP with the host's real
// values, and a zero CollectTime and unset readings (brightness, battery,
// storage drift) with random ones. It leaves Install as given.
func (d Device) withHostDefaults() Device {
	if d.Now.IsZero() {
		d.Now = time.Now()
	}
	if d.BootTime.IsZero() {
		d.BootTime = d.Now.Add(-hostUptime())
	}
	if d.WiFi != nil && d.WiFi.IP == "" {
		w := *d.WiFi
		w.IP = hostIP()
		d.WiFi = &w
	}
	if d.CollectTime == 0 {
		d.CollectTime = randDuration(150*time.Millisecond, 600*time.Millisecond).Truncate(time.Millisecond)
	}
	if d.brightness == 0 {
		// The 0-255 slider range; the extremes are rare on a phone in use.
		d.brightness = 20 + mrand.N(211)
	}
	if d.battery == (battery{}) {
		d.battery = randomBattery()
	}
	if d.storageUsed == 0 {
		d.storageUsed = 1 + mrand.N(int64(storageDrift))
	}
	return d
}

// sdkStartLead is how long before collecting the device data the app started
// the SDK (SmAntiFraud.create), whose clock a80 records.
const sdkStartLead = time.Second

// deviceData builds the device-data JSON that protect encrypts: baseTemplate
// plus the deviceId-selected hardware, deviceID and d's clock, Wi-Fi and
// install values. It serializes the way the SDK's reflection serializer
// (l1l1l11Ill) does: compact, and without top-level entries that are empty
// strings, lists or maps. d must already carry its host defaults.
func deviceData(deviceID string, d Device) (string, error) {
	if d.BootTime.After(d.Now) {
		return "", fmt.Errorf("shumei: boot time %s is after the collection time %s",
			d.BootTime.Format(time.RFC3339), d.Now.Format(time.RFC3339))
	}
	if d.AppVersion == "" {
		return "", errors.New("shumei: no app version for a38 (set Device.AppVersion)")
	}
	if d.AndroidID == "" || !androidIDRe.MatchString(d.AndroidID) {
		return "", errors.New("shumei: no valid android id for a24 (set Device.AndroidID, 16 lowercase hex digits)")
	}
	if d.battery == (battery{}) || d.storageUsed == 0 {
		return "", errors.New("shumei: no per-run readings for a69/a70/a72 (withHostDefaults sets them)")
	}
	in := d.Install
	if in.MediaDrmID == "" || in.PUUID == "" || in.AppUID == "" || in.FirstInstallTime.IsZero() || in.LastUpdateTime.IsZero() || in.BootCount == 0 {
		return "", errors.New("shumei: install values are unset (Install.Fill sets them)")
	}
	if err := in.Validate(); err != nil {
		return "", fmt.Errorf("shumei: install: %w", err)
	}
	if _, ok := in.For(deviceID); !ok {
		return "", errors.New("shumei: the install belongs to a different deviceId")
	}
	dec := json.NewDecoder(strings.NewReader(baseTemplate))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return "", fmt.Errorf("shumei: parse device-data template: %w", err)
	}

	// a2 and a97 are the smid the SDK stored from its last registration
	// (l111l1111llIl.java:789-790, 804-805; l111l11I1IIIl.java:29-35, 76-105):
	// none on a first registration, so the serializer drops both. The SDK never
	// sees the game's deviceId (SmSdkManager.callCreate passes none).
	if smid := strings.TrimPrefix(in.BoxID, "B"); smid != "" {
		m["a2"], m["a97"] = smid, smid
	}
	now, boot := d.Now.UnixMilli(), d.BootTime.UnixMilli()
	m["a9"] = now
	m["a40"] = boot
	m["a171"] = now - boot
	// uptimeMillis() leaves out deep sleep. The host's sleep time isn't
	// measured, so this equals elapsedRealtime().
	m["a170"] = now - boot
	m["a80"] = sessionID(d.Now)
	m["a133"] = d.CollectTime.Milliseconds()
	m["a38"] = d.AppVersion
	m["a90"] = TargetSDKVersion
	m["a56"], m["a57"] = appCertSubject, appCertHash

	// The device-specific half, from the catalog entry this deviceId selects.
	// A pure function of deviceId, so one device stays the same phone and
	// distinct devices report distinct, internally-consistent hardware.
	hw := HardwareFor(deviceID)
	m["a10"] = hw.AndroidVersion
	m["a36"] = hw.Screen
	m["a45"] = hw.SimOperator
	// a131 and a132 are TelephonyManager's SIM and network country ISO codes
	// (l111l1111llIl.java:300-313, l11l11Il1l.java:37-53).
	m["a131"], m["a132"] = hw.SimCountryISO, hw.NetworkCountryISO
	// a143 is /proc/cpuinfo's "Hardware" line and a33 its "Processor" or
	// "model name" line (l111l111llIl.java:226-246). The catalog has no captured
	// cpuinfo, so a33 is left out rather than made up.
	m["a143"] = hw.SoC
	m["a34"] = hw.MaxFreq
	m["a32"] = hw.Cores
	m["a48"] = hw.RAMBytes
	m["a46"] = hw.Build
	// a29 is Build.getRadioVersion() (l111l1111llIl.java:327-328), the same
	// call a46's radioVersion reads (l11l1111Il1l.java:12-19).
	m["a29"] = hw.Build["radioVersion"]
	m["a47"] = hw.Sensors
	m["a18"] = hw.Props
	m["a154"] = hw.Launcher
	// a92 is a36's "width,height" without the dpi component.
	m["a92"] = screenPrefix(hw.Screen)
	m["a69"], m["a70"], m["a71"] = storageBytes(deviceID, hw.StorageGB, d.storageUsed)

	m["a37"] = d.brightness
	m["a72"] = map[string]any{
		"status": d.battery.Status, "level": d.battery.Level, "scale": d.battery.Scale,
		"temp": d.battery.Temp, "vol": d.battery.Voltage,
	}
	m["a120"] = trafficSinceBoot(deviceID, d.BootTime, d.Now.Sub(d.BootTime))

	m["a24"] = d.AndroidID
	m["a84"] = in.MediaDrmID
	m["a163"] = in.PUUID
	m["a60"] = in.AppUID
	m["a125"] = map[string]any{"fit": in.FirstInstallTime.UnixMilli(), "lut": in.LastUpdateTime.UnixMilli()}
	m["a126"] = in.BoxID
	var saved int64
	if !in.BoxIDSavedAt.IsZero() {
		saved = in.BoxIDSavedAt.UnixMilli()
	}
	m["a167"], m["a168"] = saved, saved
	// The counters as this start and collection leave them.
	c := in.collected(d.BootTime)
	m["a4"] = strconv.Itoa(c.Collections)
	m["a130"] = c.AppStarts
	m["a127"] = c.BootCount

	if w := d.WiFi; w != nil {
		m["a19"] = w.BSSID
		if w.SSID != "" {
			m["a30"] = `"` + w.SSID + `"`
		}
		m["a31"] = w.IP
	}

	for k, v := range m {
		if isEmpty(v) {
			delete(m, k)
		}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return "", fmt.Errorf("shumei: marshal device data: %w", err)
	}
	return strings.TrimSuffix(b.String(), "\n"), nil
}

// sessionID returns a80, the SDK session ID: the time SmAntiFraud.create ran
// (sdkStartLead before the collection at now) in milliseconds, "-", then five
// java.util.Random digits seeded with that time (l11l11Il11l.java:11-17). The
// cloudconf request's sid is the same value.
func sessionID(now time.Time) string {
	start := now.Add(-sdkStartLead).UnixMilli()
	return fmt.Sprintf("%d-%s", start, javaRandomDigits(start, 5))
}

// deviceHash returns a stable 64-bit hash of the deviceId and a salt, for the
// per-device values that must not move between runs.
func deviceHash(deviceID, salt string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(salt))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(deviceID))
	return h.Sum64()
}

const (
	// storageBlock is the filesystem block size StatFs counts in.
	storageBlock = 4096
	// storageReserved is what getFreeBytes counts and getAvailableBytes
	// doesn't: the blocks /data keeps back for the system (128 MiB, f2fs's
	// reserve_root=32768 on Android 12 phones).
	storageReserved = 128 << 20
	// storageDrift bounds Device.storageUsed, how much of the free space one
	// run's reading differs by.
	storageDrift = 512 << 20
)

// storageBytes returns a69, a70 and a71: getAvailableBytes, getFreeBytes and
// getTotalBytes of one StatFs of the external storage (l111l1111llIl.java:
// 411-414, l11l111lIll.java:120-126), so available <= free <= total always.
// The total is the data partition: the flash size less about a tenth for the
// system partitions. How full the phone is follows the deviceId (20 to 70 %
// free), less the run's drift.
func storageBytes(deviceID string, storageGB int, used int64) (avail, free, total int64) {
	total = int64(storageGB) * 1e9 * 9 / 10 / storageBlock * storageBlock
	freePct := int64(20 + deviceHash(deviceID, "storage")%51)
	free = max(total*freePct/100-used, storageReserved) / storageBlock * storageBlock
	return free - storageReserved, free, total
}

// trafficSinceBoot returns a120, TrafficStats' mobile and total bytes received
// and sent since boot (l1l11l1Illl.java:42-48). Within one boot they only grow,
// so each is the uptime times a rate that follows the device and the boot: 2 to
// 20 KB/s received on average, a tenth to a quarter of that sent, and up to a
// third of both over the mobile network.
func trafficSinceBoot(deviceID string, boot time.Time, uptime time.Duration) map[string]any {
	h := deviceHash(deviceID, "traffic"+strconv.FormatInt(boot.UnixMilli(), 10))
	rxRate := int64(2000 + h%18001)
	txPct := int64(10 + (h>>16)%16)
	mobilePct := int64((h >> 32) % 34)
	secs := int64(uptime / time.Second)
	tr := rxRate * secs
	tt := tr * txPct / 100
	return map[string]any{"mr": tr * mobilePct / 100, "mt": tt * mobilePct / 100, "tr": tr, "tt": tt}
}

// isEmpty reports whether the SDK's serializer would skip v: null, an empty
// string, an empty collection or an empty map (l1l1l11Ill.l111l11111lIl).
func isEmpty(v any) bool {
	switch v := v.(type) {
	case nil:
		return true
	case string:
		return v == ""
	case []any:
		return len(v) == 0
	case map[string]any:
		return len(v) == 0
	}
	return false
}

// screenPrefix returns a92, a36's "width,height" without its dpi component:
// a36 is "width,height,dpi", a92 is the same screen minus the density.
func screenPrefix(screen string) string {
	if i := strings.LastIndex(screen, ","); i >= 0 {
		return screen[:i]
	}
	return screen
}

// javaRandomDigits returns n digits drawn with java.util.Random(seed).nextInt(10).
// a80 is the SDK start time in milliseconds, "-", then five such digits seeded
// with that same time (l11l11Il11l).
func javaRandomDigits(seed int64, n int) string {
	const multiplier, mask = 0x5DEECE66D, 1<<48 - 1
	s := (uint64(seed) ^ multiplier) & mask
	digits := make([]byte, n)
	for i := range digits {
		for {
			s = (s*multiplier + 0xB) & mask
			bits := int32(s >> 17) // next(31)
			v := bits % 10
			// nextInt(bound)'s rejection test, relying on int32 overflow as Java does.
			if bits-v+9 >= 0 {
				digits[i] = byte('0' + v)
				break
			}
		}
	}
	return string(digits)
}

// BoxID registers a device with the ShuMei backend and returns its assigned
// device fingerprint, already prefixed with `"B"` to match the Login request's
// `shumeiBoxId` field. deviceID is the Login's deviceId: it picks the phone
// (HardwareFor) and must own dev.Install, but the SDK never sends it. dev
// carries the install values, which must be filled (see Device.Install), the
// app version the Login claims (Device.AppVersion), and the clock and Wi-Fi
// fields; zero clocks and no Wi-Fi report the host's clocks and no Wi-Fi, like
// a real Last War install.
//
// BoxID is Register's device-profile upload alone: it returns no install, so
// a caller that keeps only Install.Registered's result reports the same
// counters (a4, a127, a130) on every run. Register keeps them moving.
//
// Note on scope: the device-data fingerprint this mints is a spoofed Android
// profile for a device that does not exist. The game server does not require a
// real shumeiBoxId (an empty one is accepted), so this is an optional
// best-effort path, not a hard gate.
func BoxID(client Client, deviceID string, dev Device) (string, error) {
	pub, err := publicKey()
	if err != nil {
		return "", err
	}
	return boxID(client, pub, deviceID, dev.withHostDefaults())
}

// Register does what one start of the SDK on dev's install does: it collects
// the device data and uploads it to /deviceprofile/v4. It returns the box ID
// ("B" + smid, "" when the upload failed), the install as that start leaves
// it, and the upload's error. The install counts the start and the collection
// (a4, a130, and a127 when the phone rebooted) even when the upload fails, as
// the SDK counts before it uploads, and caches the box ID that came back
// (Install.Registered). Saving it whenever it differs from dev.Install keeps
// the counters moving from run to run. An install deviceData refuses comes
// back unchanged, with nothing sent.
func Register(client Client, deviceID string, dev Device) (string, Install, error) {
	pub, err := publicKey()
	if err != nil {
		return "", dev.Install, err
	}
	return register(client, pub, deviceID, dev.withHostDefaults(), time.Now)
}

// register is Register with the RSA key and the clock as parameters, so a test
// can decrypt the upload and pin the box ID's save time. dev must already carry
// its host defaults.
func register(client Client, pub *rsa.PublicKey, deviceID string, dev Device, now func() time.Time) (string, Install, error) {
	inner, err := deviceData(deviceID, dev)
	if err != nil {
		return "", dev.Install, err
	}
	in := dev.Install.collected(dev.BootTime)
	id, err := upload(client, pub, inner)
	if err != nil {
		return "", in, err
	}
	in, _ = in.Registered(id, now())
	return id, in, nil
}

// boxID is BoxID with the RSA key as a parameter, the same way protect takes
// it, so a test can use its own key pair and decrypt the POSTed body. dev must
// already carry its host defaults.
func boxID(client Client, pub *rsa.PublicKey, deviceID string, dev Device) (string, error) {
	inner, err := deviceData(deviceID, dev)
	if err != nil {
		return "", err
	}
	return upload(client, pub, inner)
}

// upload protects the device-data JSON inner and POSTs it to /deviceprofile/v4,
// returning "B" + the smid the reply assigns.
func upload(client Client, pub *rsa.PublicKey, inner string) (string, error) {
	data, ep, tn, err := protect(pub, inner)
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
