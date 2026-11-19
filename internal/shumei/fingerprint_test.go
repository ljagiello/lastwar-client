package shumei

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestAppCert pins a56/a57 to the 1.0.370 base.apk's signing certificate:
// subject O=LCYD and Signature.hashCode() 2110632754, computed from AppCertHex.
func TestAppCert(t *testing.T) {
	if appCertSubject != "O=LCYD" || appCertHash != 2110632754 {
		t.Errorf("a56, a57 = %q, %d; want \"O=LCYD\", 2110632754", appCertSubject, appCertHash)
	}
	// Arrays.hashCode over signed bytes: 31*h + b from 1.
	for _, c := range []struct {
		in   []byte
		want int32
	}{
		{nil, 1},
		{[]byte{0x01}, 32},
		{[]byte{0x80}, 31 - 128},
		{[]byte{0xff, 0x7f}, (31-1)*31 + 127},
	} {
		if got := javaBytesHashCode(c.in); got != c.want {
			t.Errorf("javaBytesHashCode(% x) = %d, want %d", c.in, got, c.want)
		}
	}
	// getSubjectDN().toString() lists the RDNs most-specific first with ", ".
	name := pkix.Name{
		Country: []string{"US"}, Province: []string{"California"}, Locality: []string{"Mountain View"},
		Organization: []string{"Google Inc."}, OrganizationalUnit: []string{"Android"}, CommonName: "Android",
	}
	raw, err := asn1.Marshal(name.ToRDNSequence())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := javaSubjectDN(raw), "CN=Android, OU=Android, O=Google Inc., L=Mountain View, ST=California, C=US"; got != want {
		t.Errorf("javaSubjectDN = %q, want %q", got, want)
	}
}

// registerOnce runs register against a 200 reply carrying smid-test-0123456789,
// returning the box ID, the install and the decrypted device data it uploaded.
func registerOnce(t *testing.T, priv *rsa.PrivateKey, d Device, savedAt time.Time) (string, Install, map[string]any) {
	t.Helper()
	ft := &fakeTransport{status: http.StatusOK}
	id, in, err := register(&http.Client{Transport: ft}, &priv.PublicKey, testDeviceID, d, func() time.Time { return savedAt })
	if err != nil {
		t.Fatalf("register = %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(ft.body, &profile); err != nil {
		t.Fatalf("POSTed body: %v", err)
	}
	data, _ := profile["data"].(string)
	ep, _ := profile["ep"].(string)
	tn, _ := profile["tn"].(string)
	return id, in, decodeDeviceData(t, unprotect(t, priv, data, ep, tn))
}

// testDeviceID is the Login deviceId the register tests use.
const testDeviceID = "0123456789abcdef0123456789abcdef_n3d"

// TestRegister checks Register keeps the install moving the way the SDK's
// stores do: a4 and a130 count every start, a127 counts reboots, and the next
// registration sends the smid the last one cached as a2/a97.
func TestRegister(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey = %v", err)
	}
	d := testDevice()
	savedAt := time.UnixMilli(1790882445000)

	id, first, sent := registerOnce(t, priv, d, savedAt)
	if id != "Bsmid-test-0123456789" || first.BoxID != id || !first.BoxIDSavedAt.Equal(savedAt) {
		t.Errorf("first registration = %q, install box ID %q at %v; want the smid cached at %v", id, first.BoxID, first.BoxIDSavedAt, savedAt)
	}
	if first.Collections != 1 || first.AppStarts != 1 || first.BootCount != 37 || !first.LastBoot.Equal(d.BootTime) {
		t.Errorf("first install counters = %d, %d, %d, boot %v; want 1, 1, 37, %v", first.Collections, first.AppStarts, first.BootCount, first.LastBoot, d.BootTime)
	}
	if got := fmt.Sprintf("%v %v %v", sent["a4"], sent["a130"], sent["a127"]); got != "1 1 37" {
		t.Errorf("first upload a4 a130 a127 = %s, want 1 1 37", got)
	}

	// Next run, same boot: one more start and collection, no reboot, and the
	// cached smid in a2/a97.
	d.Install, d.Now = first, d.Now.Add(3*time.Hour)
	_, second, sent := registerOnce(t, priv, d, savedAt)
	if got := fmt.Sprintf("%v %v %v %v %v", sent["a4"], sent["a130"], sent["a127"], sent["a2"], sent["a97"]); got != "2 2 37 smid-test-0123456789 smid-test-0123456789" {
		t.Errorf("second upload a4 a130 a127 a2 a97 = %s", got)
	}
	if second.Collections != 2 || second.AppStarts != 2 || second.BootCount != 37 {
		t.Errorf("second install counters = %d, %d, %d; want 2, 2, 37", second.Collections, second.AppStarts, second.BootCount)
	}

	// After a reboot the boot count moves on once.
	d.Install, d.BootTime = second, d.Now.Add(-time.Hour)
	_, third, sent := registerOnce(t, priv, d, savedAt)
	if fmt.Sprint(sent["a127"]) != "38" || third.BootCount != 38 || !third.LastBoot.Equal(d.BootTime) {
		t.Errorf("after a reboot a127 = %v, install boot count %d at %v; want 38 at %v", sent["a127"], third.BootCount, third.LastBoot, d.BootTime)
	}

	// A failed upload still counts the start and collection, and keeps the
	// cached smid.
	d.Install = third
	failing := &http.Client{Transport: &fakeTransport{status: http.StatusInternalServerError}}
	if id, in, err := register(failing, &priv.PublicKey, testDeviceID, d, time.Now); err == nil || id != "" || in.Collections != 4 || in.BoxID != third.BoxID {
		t.Errorf("failed register = %q, %+v, %v; want \"\", the install counted with its box ID kept, an error", id, in, err)
	}

	// An install deviceData refuses comes back as it was, with no request.
	calls := 0
	counting := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return nil, errors.New("unexpected request")
	})}
	refused := testDevice()
	refused.AppVersion = ""
	if _, in, err := register(counting, &priv.PublicKey, testDeviceID, refused, time.Now); err == nil || in != refused.Install || calls != 0 {
		t.Errorf("register without an app version = %+v, %v after %d requests; want the install unchanged, an error, no request", in, err, calls)
	}
}

// TestInstallCollected checks only a boot clearly later than the last one
// counts as a reboot: a40's samples of one boot differ by milliseconds.
func TestInstallCollected(t *testing.T) {
	boot := time.UnixMilli(1790000000000)
	in := testInstall.collected(boot)
	if in.BootCount != testInstall.BootCount || !in.LastBoot.Equal(boot) {
		t.Errorf("first collection = boot count %d at %v, want %d at %v", in.BootCount, in.LastBoot, testInstall.BootCount, boot)
	}
	if again := in.collected(boot.Add(800 * time.Millisecond)); again.BootCount != in.BootCount {
		t.Errorf("same boot sampled 0.8 s later = boot count %d, want %d", again.BootCount, in.BootCount)
	}
	if earlier := in.collected(boot.Add(-time.Hour)); earlier.BootCount != in.BootCount {
		t.Errorf("an earlier boot time = boot count %d, want %d", earlier.BootCount, in.BootCount)
	}
	if later := in.collected(boot.Add(2 * time.Hour)); later.BootCount != in.BootCount+1 || later.Collections != 2 || later.AppStarts != 2 {
		t.Errorf("after a reboot = %+v, want one more boot and the second start and collection", later)
	}
}

// TestStorageBytes checks a69-a71 come from one plausible StatFs: available
// below free below total, whole blocks, and a total under the flash size.
func TestStorageBytes(t *testing.T) {
	for _, gb := range []int{128, 256} {
		for i := range 50 {
			id := fmt.Sprintf("%032x_n3d", i)
			for _, used := range []int64{1, storageDrift} {
				avail, free, total := storageBytes(id, gb, used)
				if avail <= 0 || avail > free || free > total || total >= int64(gb)*1e9 {
					t.Fatalf("storageBytes(%s, %d GB, %d) = %d, %d, %d; want 0 < available <= free <= total < %d GB", id, gb, used, avail, free, total, gb)
				}
				if avail%storageBlock != 0 || free%storageBlock != 0 || total%storageBlock != 0 {
					t.Errorf("storageBytes = %d, %d, %d; want whole %d-byte blocks", avail, free, total, storageBlock)
				}
			}
		}
	}
	// The same phone reports the same total and about the same free space.
	a1, f1, t1 := storageBytes("dev", 128, 1)
	a2, f2, t2 := storageBytes("dev", 128, storageDrift)
	if t1 != t2 || f1-f2 > storageDrift || a1-a2 > storageDrift {
		t.Errorf("one device's storage = %d/%d/%d then %d/%d/%d, want the same total and free space within the drift", a1, f1, t1, a2, f2, t2)
	}
}

// TestTrafficSinceBoot checks a120 only grows within one boot and its mobile
// counts stay inside the totals.
func TestTrafficSinceBoot(t *testing.T) {
	var prev map[string]any
	for _, up := range []time.Duration{time.Minute, time.Hour, 3 * time.Hour, 30 * 24 * time.Hour} {
		got := trafficSinceBoot("dev", 37, up)
		mr, mt, tr, tt := got["mr"].(int64), got["mt"].(int64), got["tr"].(int64), got["tt"].(int64)
		if mr > tr || mt > tt || tt > tr || tr <= 0 {
			t.Errorf("a120 after %v = %v, want mobile within total and sent below received", up, got)
		}
		for _, k := range []string{"mr", "mt", "tr", "tt"} {
			if prev != nil && got[k].(int64) < prev[k].(int64) {
				t.Errorf("a120.%s went from %v to %v within one boot", k, prev[k], got[k])
			}
		}
		prev = got
	}
	if fmt.Sprint(trafficSinceBoot("dev", 37, time.Hour)) == fmt.Sprint(trafficSinceBoot("dev", 38, time.Hour)) {
		t.Error("a120 is the same for two boots, want each boot its own rate")
	}
}

// TestDeviceDataTrafficOneBoot checks a120 never drops between two runs in one
// boot, although each run resamples the boot time (here 300 ms apart, as
// Now - uptime gives it).
func TestDeviceDataTrafficOneBoot(t *testing.T) {
	first := testDevice()
	second := first
	second.Now, second.BootTime = first.Now.Add(3*time.Hour), first.BootTime.Add(300*time.Millisecond)
	a := decodeDeviceData(t, mustDeviceData(t, testDeviceID, first))["a120"].(map[string]any)
	b := decodeDeviceData(t, mustDeviceData(t, testDeviceID, second))["a120"].(map[string]any)
	for _, k := range []string{"mr", "mt", "tr", "tt"} {
		if jsonInt(t, b[k]) < jsonInt(t, a[k]) {
			t.Errorf("a120.%s went from %v to %v three hours later in the same boot", k, a[k], b[k])
		}
	}
}

// TestPerRunReadings checks withHostDefaults draws a37, a72 and the storage
// drift in a phone's ranges, and keeps readings already set.
func TestPerRunReadings(t *testing.T) {
	for range 200 {
		d := Device{}.withHostDefaults()
		b := d.battery
		if d.brightness < 20 || d.brightness > 230 {
			t.Fatalf("brightness = %d, want 20 to 230", d.brightness)
		}
		if b.Scale != 100 || b.Level < 15 || b.Level > 100 || b.Temp < 250 || b.Temp > 380 || b.Voltage < 3600 || b.Voltage > 4260 {
			t.Fatalf("battery = %+v, want a level of 15-100 %%, 25-38 degrees and 3.6-4.26 V", b)
		}
		switch b.Status {
		case batteryCharging, batteryDischarging:
		case batteryFull:
			if b.Level != 100 {
				t.Fatalf("battery = %+v, full below 100 %%", b)
			}
		default:
			t.Fatalf("battery status %d, want charging, discharging or full", b.Status)
		}
		if d.storageUsed <= 0 || d.storageUsed > storageDrift {
			t.Fatalf("storageUsed = %d, want 1 to %d", d.storageUsed, storageDrift)
		}
	}
	set := testDevice().withHostDefaults()
	if set.brightness != 142 || set.battery.Level != 64 || set.storageUsed != 7<<20 {
		t.Errorf("withHostDefaults changed set readings: %d, %+v, %d", set.brightness, set.battery, set.storageUsed)
	}
}
