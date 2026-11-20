package auth

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/shumei"
)

var (
	afuidRe = regexp.MustCompile(`^(\d{13})-(\d{1,19})$`)
	uuidRe  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
)

// TestNewAndroidIdentity covers 1.0.370 audit B90 and B31: a new Android identity's deviceId is its
// Android ID + "_n3d" (Udid.getUid without an advertising id, com/sdkmanager/utils/Udid.java:75-83),
// and it gets an afuid "<first-launch ms>-<63-bit>" launched after its install time and a lowercase
// random UUID distinct_id, all kept across runs.
func TestNewAndroidIdentity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	first, err := LoadOrCreateDeviceIdentity(false)
	if err != nil {
		t.Fatal(err)
	}
	if first.DeviceID != first.AndroidID+"_n3d" || len(first.AndroidID) != 16 {
		t.Errorf("deviceId = %q, want the Android ID %q + _n3d", first.DeviceID, first.AndroidID)
	}
	a := first.Android
	m := afuidRe.FindStringSubmatch(a.Afuid)
	if m == nil {
		t.Fatalf("afuid = %q, want <13-digit ms>-<decimal>", a.Afuid)
	}
	launch, _ := strconv.ParseInt(m[1], 10, 64)
	if n, err := strconv.ParseUint(m[2], 10, 64); err != nil || n > math.MaxInt64 {
		t.Errorf("afuid random part %q is not a non-negative 63-bit value", m[2])
	}
	if d := launch - a.InstallTime.UnixMilli(); d < 1000 || d > int64(10*time.Minute/time.Millisecond) {
		t.Errorf("first launch is %d ms after the install, want seconds to minutes after it", d)
	}
	if a.InstallTime.After(time.Now()) || time.Since(a.InstallTime) > time.Hour {
		t.Errorf("install time %v, want a few minutes ago", a.InstallTime)
	}
	if !uuidRe.MatchString(a.DistinctID) {
		t.Errorf("distinct_id = %q, want a lowercase random UUID", a.DistinctID)
	}

	second, err := LoadOrCreateDeviceIdentity(false)
	if err != nil {
		t.Fatal(err)
	}
	if second.DeviceID != first.DeviceID || second.Android.Afuid != a.Afuid || second.Android.DistinctID != a.DistinctID ||
		!second.Android.InstallTime.Equal(a.InstallTime) {
		t.Error("a second run changed the Android identity")
	}
}

// TestIOSIdentityKeepsItsShape: iOS mode creates the deviceId it always has and no Android install
// state, and an existing deviceId is never rewritten on either platform.
func TestIOSIdentityKeepsItsShape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ident, err := LoadOrCreateDeviceIdentity(true)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}_n3d$`).MatchString(ident.DeviceID) || strings.HasPrefix(ident.DeviceID, ident.AndroidID) {
		t.Errorf("iOS deviceId = %q, want 32 random hex + _n3d", ident.DeviceID)
	}
	if ident.Android != (AndroidInstall{}) {
		t.Error("iOS mode created Android install values")
	}
	if _, err := os.Stat(androidInstallStatePath()); !os.IsNotExist(err) {
		t.Errorf("iOS mode wrote the Android install state (stat err %v)", err)
	}
	again, err := LoadOrCreateDeviceIdentity(false)
	if err != nil {
		t.Fatal(err)
	}
	if again.DeviceID != ident.DeviceID {
		t.Errorf("an existing deviceId was rewritten: %q -> %q", ident.DeviceID, again.DeviceID)
	}
}

// TestAndroidInstallStateCorruptIsAnError: a corrupt install file is never silently replaced.
func TestAndroidInstallStateCorruptIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.WriteFile(androidInstallStatePath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateDeviceIdentity(false); err == nil {
		t.Fatal("a corrupt Android install file was accepted")
	}
}

// TestBuildLoginParamsAndroid370 covers the Android Login fields of 1.0.370 audit B30, B31, B91,
// B94 and B95.
func TestBuildLoginParamsAndroid370(t *testing.T) {
	const deviceID = "0123456789abcdef_n3d"
	hw := shumei.HardwareFor(deviceID)
	install := AndroidInstall{InstallTime: time.Date(2026, 3, 4, 5, 6, 7, 890e6, time.UTC), Afuid: "1772600767890-42", DistinctID: "d-1"}
	p := BuildLoginParams(LoginParamsInput{DeviceID: deviceID, AndroidID: "0123456789abcdef", ServerID: "783", GameUid: "g", Android: install})

	if got := p.GetInt("google_available"); got != 1 {
		t.Errorf("google_available = %d, want 1 (B30)", got)
	}
	if p.GetString("afuid") != install.Afuid || p.GetString("distinct_id") != install.DistinctID {
		t.Errorf("afuid/distinct_id = %q/%q, want the install's (B31)", p.GetString("afuid"), p.GetString("distinct_id"))
	}
	if got := p.GetString("simOp"); got != hw.SimOperator || got == "" {
		t.Errorf("simOp = %q, want the SIM operator %q mt names (B91)", got, hw.SimOperator)
	}

	var ta map[string]any
	if err := json.Unmarshal([]byte(p.GetString("ta")), &ta); err != nil {
		t.Fatal(err)
	}
	if ta["#device_type"] != "Phone" {
		t.Errorf("ta #device_type = %v, want Phone (B94)", ta["#device_type"])
	}
	if ta["#install_time"] != "2026-03-04 05:06:07.890" {
		t.Errorf("ta #install_time = %v, want the install's (B95)", ta["#install_time"])
	}
	// Independent of the code under test: GiB, rounded half up to one decimal.
	r1 := func(d float64) string { return strconv.FormatFloat(math.Round(d*10)/10, 'f', 1, 64) }
	ramTotal := float64(hw.RAMBytes) / (1 << 30)
	if want := r1(ramTotal*0.35) + "/" + r1(ramTotal); ta["#ram"] != want {
		t.Errorf("ta #ram = %v, want %s (B95)", ta["#ram"], want)
	}
	if !strings.HasSuffix(fmt.Sprint(ta["#ram"]), "/8.0") && !strings.HasSuffix(fmt.Sprint(ta["#ram"]), "/12.0") {
		t.Errorf("ta #ram = %v, want a total of 8.0 or 12.0 GiB", ta["#ram"])
	}
	diskTotal := float64(hw.StorageGB)*1e9/(1<<30) - androidSystemGiB
	if want := r1(diskTotal*0.45) + "/" + r1(diskTotal); ta["#disk"] != want {
		t.Errorf("ta #disk = %v, want %s (B95)", ta["#disk"], want)
	}

	// iOS keeps what it sent: no #device_type, the 2000 install time, no simOp.
	ios := BuildLoginParams(LoginParamsInput{IOSMode: true, ServerID: "783", GameUid: "g", Android: install})
	if s := ios.GetString("ta"); strings.Contains(s, "#device_type") || !strings.Contains(s, `"#install_time":"2000-01-01 00:00:00.000"`) {
		t.Errorf("iOS ta changed: %s", s)
	}
	if ios.GetString("simOp") != "" || ios.GetString("afuid") != "" || ios.GetString("distinct_id") != "" {
		t.Error("iOS took the Android install's values")
	}
}

// TestFormDeviceAndroidSimOp: the Android GSL form names the same SIM operator (B91).
func TestFormDeviceAndroidSimOp(t *testing.T) {
	const deviceID = "0123456789abcdef_n3d"
	if f := formDevice(false, deviceID, CapturedDevice{}); f.SimOp != shumei.HardwareFor(deviceID).SimOperator || f.Platform != "Android" {
		t.Errorf("Android form device = %+v, want the phone's SIM operator", f)
	}
	if f := formDevice(true, deviceID, CapturedDevice{}); f.SimOp != "" || f.Platform != "iOS" {
		t.Errorf("iOS form device = %+v, want no SIM operator", f)
	}
}
