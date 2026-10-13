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
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestBoxID drives boxID with a test-generated key pair, so besides the wire
// shape it can decrypt the POSTed body and check each protected field holds
// what the server expects: ep wraps r, tn wraps "0", and data is the device
// data for this deviceID.
func TestBoxID(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey = %v", err)
	}
	ft := &fakeTransport{status: http.StatusOK}
	client := &http.Client{Transport: ft}

	const deviceID = "0123456789abcdef0123456789abcdef_n3d"
	dev := testDevice()
	got, err := boxID(client, &priv.PublicKey, deviceID, dev)
	if err != nil {
		t.Fatalf("boxID = %v, want nil", err)
	}
	// The Login request expects getDeviceId()'s "B" + smid string.
	if got != "Bsmid-test-0123456789" {
		t.Errorf("boxID = %q, want %q", got, "Bsmid-test-0123456789")
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
	fields := map[string]string{}
	for _, key := range []string{"data", "tn", "ep"} {
		v, ok := profile[key].(string)
		if !ok || v == "" {
			t.Fatalf("profile[%q] = %v, want a non-empty protected value", key, profile[key])
		}
		fields[key] = v
	}
	if plaintext, want := unprotect(t, priv, fields["data"], fields["ep"], fields["tn"]), mustDeviceData(t, deviceID, dev); plaintext != want {
		t.Errorf("decrypted data = %q, want deviceData(%q) = %q", plaintext, deviceID, want)
	}
}

// testInstall is a filled install with pinned values, so deviceData's output is
// fixed.
var testInstall = Install{
	AndroidID:        "3f9c1e0a7b2d4c58",
	MediaDrmID:       "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=___",
	PUUID:            "6f1c2b3a4d5e4f60a7b8c9d0e1f20314",
	FirstInstallTime: time.UnixMilli(1775000000000),
	LastUpdateTime:   time.UnixMilli(1789000000000),
}

// testDevice is a Device with every field pinned that withHostDefaults would
// otherwise fill.
func testDevice() Device {
	return Device{
		Now: time.UnixMilli(1790882444713), BootTime: time.UnixMilli(1790000000000),
		Install: testInstall, CollectTime: 287 * time.Millisecond,
	}
}

// mustDeviceData is deviceData, failing the test on an error.
func mustDeviceData(t *testing.T, deviceID string, d Device) string {
	t.Helper()
	s, err := deviceData(deviceID, d)
	if err != nil {
		t.Fatalf("deviceData = %v", err)
	}
	return s
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
			boxID, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d", Device{Install: testInstall})
			if err == nil {
				t.Fatalf("BoxID = %q, nil; want an error", boxID)
			}
			if got := errors.Is(err, ErrUnassigned); got != c.unassigned {
				t.Errorf("errors.Is(%v, ErrUnassigned) = %v, want %v", err, got, c.unassigned)
			}
		})
	}
}

// TestBoxIDNeedsInstall checks BoxID refuses an install with values unset
// before it contacts the server, rather than register a phone the caller
// never sees and so can't keep.
func TestBoxIDNeedsInstall(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	})}
	boxID, err := BoxID(client, "0123456789abcdef0123456789abcdef_n3d", Device{})
	if err == nil || !strings.Contains(err.Error(), "unset") || calls != 0 {
		t.Errorf("BoxID with no install = %q, %v after %d requests; want the unset-install error and no request", boxID, err, calls)
	}
}

// roundTripperFunc adapts a func to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestProtectRoundTrip inverts the whole native `w1` pipeline. protect takes
// the RSA key as a parameter, so a test-generated key pair can decrypt ep back
// to r.
func TestProtectRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey = %v", err)
	}
	in := mustDeviceData(t, "0123456789abcdef0123456789abcdef_n3d", Device{Install: testInstall}.withHostDefaults())
	data, ep, tn, err := protect(&priv.PublicKey, in)
	if err != nil {
		t.Fatalf("protect = %v", err)
	}
	if got := unprotect(t, priv, data, ep, tn); got != in {
		t.Errorf("inflate(decrypt(data)) = %q, want the device-data JSON", got)
	}
}

// unprotect inverts protect with the test's private key and returns the
// inflated device data, failing the test at the first malformed stage. The AES
// key is derived here from the spec (hex MD5 of r), not via aesKeyFromNonce, so
// a broken helper can't cancel itself out.
func unprotect(t *testing.T, priv *rsa.PrivateKey, data, ep, tn string) string {
	t.Helper()
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
	inflated, err := io.ReadAll(flate.NewReader(bytes.NewReader(pt[:len(pt)-pad])))
	if err != nil {
		t.Fatalf("raw inflate = %v", err)
	}
	return string(inflated)
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

// decodeDeviceData parses deviceData's JSON, keeping numbers exact.
func decodeDeviceData(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("deviceData is not valid JSON: %v\n%s", err, s)
	}
	return m
}

// TestDeviceData checks the fields deviceData adds to the template, and that it
// serializes the way the SDK does: compact, without empty top-level entries,
// and without Wi-Fi details by default. A broken template only shows up live as
// a 1902, so this is also the guard against hand edits.
func TestDeviceData(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef_n3d"
	// The SDK started one second before collecting, at 1790882443713, whose
	// java.util.Random digits TestJavaRandomDigits pins.
	d := testDevice()
	s := mustDeviceData(t, id, d)

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(s)); err != nil || compact.String() != s {
		t.Errorf("deviceData is not compact JSON (Compact error %v):\n%s", err, s)
	}
	m := decodeDeviceData(t, s)
	for key, want := range map[string]string{
		"a2":   id,
		"a97":  id,
		"a9":   "1790882444713",
		"a40":  "1790000000000",
		"a171": "882444713",
		"a170": "882444713",
		"a80":  "1790882443713-36769",
		"a133": "287",
		"a24":  testInstall.AndroidID,
		"a84":  testInstall.MediaDrmID,
		"a163": testInstall.PUUID,
		"a125": "map[fit:1775000000000 lut:1789000000000]",
		// No smid cached yet: the cache file doesn't exist, so both are 0.
		"a167": "0",
		"a168": "0",
	} {
		if got := fmt.Sprint(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
	for _, key := range []string{"a19", "a30", "a31"} {
		if v, ok := m[key]; ok {
			t.Errorf("%s = %v, want it absent: a real Last War install reads no Wi-Fi details", key, v)
		}
	}
	if v, ok := m["a126"]; ok {
		t.Errorf("a126 = %v, want it absent before the install's first registration", v)
	}
	for key, v := range m {
		if isEmpty(v) {
			t.Errorf("%s = %v, want empty entries dropped, as the SDK's serializer does", key, v)
		}
	}
	// false and 0 aren't empty to the SDK, so they stay.
	if m["a117"] != false || fmt.Sprint(m["a113"]) != "0" {
		t.Errorf("a117 = %v, a113 = %v, want false and 0 kept", m["a117"], m["a113"])
	}

	backwards := d
	backwards.Now, backwards.BootTime = d.BootTime, d.Now
	if _, err := deviceData(id, backwards); err == nil {
		t.Error("deviceData with the boot time after the collection time = nil error, want an error")
	}
	unset := d
	unset.Install = Install{}
	if _, err := deviceData(id, unset); err == nil {
		t.Error("deviceData with no install values = nil error, want an error")
	}
	for name, unsetOne := range map[string]func(*Install){
		"androidId":        func(in *Install) { in.AndroidID = "" },
		"mediaDrmId":       func(in *Install) { in.MediaDrmID = "" },
		"puuid":            func(in *Install) { in.PUUID = "" },
		"firstInstallTime": func(in *Install) { in.FirstInstallTime = time.Time{} },
		"lastUpdateTime":   func(in *Install) { in.LastUpdateTime = time.Time{} },
	} {
		partial := d
		unsetOne(&partial.Install)
		if _, err := deviceData(id, partial); err == nil {
			t.Errorf("deviceData with the install's %s unset = nil error, want an error", name)
		}
	}
	invalid := d
	invalid.Install.AndroidID = "not-an-android-id"
	if _, err := deviceData(id, invalid); err == nil {
		t.Error("deviceData with an invalid androidId = nil error, want an error")
	}
	other := d
	other.Install.DeviceID = "another-device"
	if _, err := deviceData(id, other); err == nil {
		t.Error("deviceData with another deviceId's install = nil error, want an error")
	}
}

// TestDeviceDataRegistered checks a registered install sends its cached box ID
// back as a126, with its save time in a167 and a168.
func TestDeviceDataRegistered(t *testing.T) {
	d := testDevice()
	d.Install, _ = d.Install.Registered("Bsmid-earlier", time.UnixMilli(1789500000123))
	m := decodeDeviceData(t, mustDeviceData(t, "id", d))
	for key, want := range map[string]string{
		"a126": "Bsmid-earlier",
		"a167": "1789500000123",
		"a168": "1789500000123",
	} {
		if got := fmt.Sprint(m[key]); got != want {
			t.Errorf("%s = %s, want %s", key, got, want)
		}
	}
}

// TestDeviceDataWiFi checks a Device's Wi-Fi details land the way WifiInfo
// reports them: the SSID in quotes, without HTML escaping.
func TestDeviceDataWiFi(t *testing.T) {
	d := testDevice()
	d.WiFi = &WiFi{SSID: "Home <5G> & co", BSSID: "aa:bb:cc:dd:ee:ff", IP: "192.168.1.50"}
	s := mustDeviceData(t, "0123456789abcdef0123456789abcdef_n3d", d)
	if strings.Contains(s, `\u00`) {
		t.Errorf("deviceData escapes characters the SDK sends raw:\n%s", s)
	}
	m := decodeDeviceData(t, s)
	for key, want := range map[string]string{
		"a19": "aa:bb:cc:dd:ee:ff",
		"a30": `"Home <5G> & co"`,
		"a31": "192.168.1.50",
	} {
		if m[key] != want {
			t.Errorf("%s = %v, want %q", key, m[key], want)
		}
	}

	// An unreadable host IP leaves a31 empty, so it's dropped.
	d.WiFi = &WiFi{SSID: "Home"}
	if m := decodeDeviceData(t, mustDeviceData(t, "id", d)); m["a31"] != nil || m["a19"] != nil {
		t.Errorf("a31 = %v, a19 = %v, want both dropped when empty", m["a31"], m["a19"])
	}
}

// TestWithHostDefaults checks zero fields take the host's values and set ones
// stay, without changing the caller's WiFi.
func TestWithHostDefaults(t *testing.T) {
	before := time.Now()
	d := Device{}.withHostDefaults()
	if d.Now.Before(before) || d.Now.After(time.Now()) {
		t.Errorf("Now = %v, want the current time", d.Now)
	}
	if d.BootTime.After(d.Now) {
		t.Errorf("BootTime = %v, want it no later than Now %v", d.BootTime, d.Now)
	}
	// A set Now keeps the host's uptime: the boot time moves with it.
	moved := Device{Now: time.UnixMilli(1790882444713)}.withHostDefaults()
	if diff := moved.Now.Sub(moved.BootTime) - hostUptime(); diff < -time.Second || diff > time.Second {
		t.Errorf("BootTime = %v for Now %v, want Now minus the host's uptime %v", moved.BootTime, moved.Now, hostUptime())
	}
	if d.WiFi != nil {
		t.Errorf("WiFi = %+v, want nil: no Wi-Fi details unless asked for", d.WiFi)
	}
	if d.CollectTime < 150*time.Millisecond || d.CollectTime >= 600*time.Millisecond {
		t.Errorf("CollectTime = %v, want 150 to 600 ms", d.CollectTime)
	}
	if d.Install != (Install{}) {
		t.Errorf("Install = %+v, want it left empty: withHostDefaults never makes up install values", d.Install)
	}
	if kept := testDevice().withHostDefaults(); kept.Install != testInstall || kept.CollectTime != 287*time.Millisecond {
		t.Errorf("withHostDefaults changed a set Install or CollectTime: %+v, %v", kept.Install, kept.CollectTime)
	}

	set := Device{Now: time.UnixMilli(2), BootTime: time.UnixMilli(1), WiFi: &WiFi{SSID: "Home"}}
	got := set.withHostDefaults()
	if !got.Now.Equal(set.Now) || !got.BootTime.Equal(set.BootTime) {
		t.Errorf("withHostDefaults changed set clocks: %v, %v", got.Now, got.BootTime)
	}
	if got.WiFi.IP != hostIP() || got.WiFi.SSID != "Home" {
		t.Errorf("WiFi = %+v, want SSID kept and IP %q from the host", got.WiFi, hostIP())
	}
	if set.WiFi.IP != "" {
		t.Errorf("withHostDefaults wrote the IP into the caller's WiFi")
	}
}

// TestUptime checks the host's uptime reads on the systems that support it.
func TestUptime(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("uptime reads only Linux and macOS")
	}
	got, err := uptime()
	if err != nil {
		t.Fatalf("uptime = %v", err)
	}
	if got <= 0 || got > 10*365*24*time.Hour {
		t.Errorf("uptime = %v, want a positive duration under ten years", got)
	}
}

// TestJavaRandomDigits pins the java.util.Random port to known answers from
// OpenJDK 27: five `new Random(seed).nextInt(10)` draws per seed.
func TestJavaRandomDigits(t *testing.T) {
	for seed, want := range map[int64]string{
		0:             "08975",
		1:             "58734",
		1784568338000: "17865",
		1790882443713: "36769",
		1791999999999: "48223",
	} {
		if got := javaRandomDigits(seed, 5); got != want {
			t.Errorf("javaRandomDigits(%d, 5) = %q, want %q", seed, got, want)
		}
	}
}
