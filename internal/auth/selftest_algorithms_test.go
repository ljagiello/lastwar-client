package auth

import (
	"lastwar-client/internal/gsl"
	"testing"
)

func TestSecurityCodeAlgorithm(t *testing.T) {
	// MD5(cmdBaseTime + salt + gameUid), UTF-8, lowercase hex.
	const ts = "1700000000"
	if got, want := securityCode(ts, "guest123"), "da23365e5910b2cf3db45f43fe970809"; got != want {
		t.Errorf("securityCode(%q, guest123) = %q, want %q", ts, got, want)
	}
	if got, want := securityCode(ts, ""), "724c23edce38133f6a71e64196441746"; got != want {
		t.Errorf("securityCode(%q, empty uid) = %q, want %q", ts, got, want)
	}
}

func TestPshFieldKnownVectors(t *testing.T) {
	const ts = "1700000000"
	if got, want := pshField(false, ts), "0028a51a469d640c9900bbbc44125e78"; got != want {
		t.Errorf("android psh(%q) = %q, want %q", ts, got, want)
	}
	// MD5(cmdBaseTime + "7" + "com.lastwar.ios"): the cmdBaseTime/psh pairs of two separate
	// captures of the real App Store app on a Mac (2026-10-13, and 1.0.364/900 on 2026-10-18).
	for _, c := range []struct{ ts, want string }{
		{"1791877000", "83413bc26feb1f628a41c04579dd708f"},
		{"1792390017", "240c39b96a881558197b3061b29d6513"},
	} {
		if got := pshField(true, c.ts); got != c.want {
			t.Errorf("ios psh(%q) = %q, want %q", c.ts, got, c.want)
		}
	}
}

func TestOneCodeAndCoreVKnownSeed(t *testing.T) {
	const rand32 = "01234567890123456789012345678901"
	oneCode, coreV := oneCodeAndCoreVFrom(rand32)
	const wantOne = "209182b3f405169778a90051124394e5366768b960b1b26314853657a819c011"
	const wantCore = "4b747073d2427e77a8936943ea901e369b335762b13ea820d26e7533b4ea3589"
	if oneCode != wantOne {
		t.Errorf("OneCode = %q, want %q", oneCode, wantOne)
	}
	if coreV != wantCore {
		t.Errorf("CoreV = %q, want %q", coreV, wantCore)
	}
}

func TestBuildLoginParamsIntegrityFields(t *testing.T) {
	for _, iosMode := range []bool{false, true} {
		p := BuildLoginParams(LoginParamsInput{IOSMode: iosMode, GameUid: "guest123", ServerID: "783"})
		ts := p.GetString("cmdBaseTime")
		if got, want := p.GetString("psh"), pshField(iosMode, ts); got != want {
			t.Errorf("IOSMode=%v psh = %q, want %q", iosMode, got, want)
		}
		if got, want := p.GetString("SecurityCode"), securityCode(ts, "guest123"); got != want {
			t.Errorf("IOSMode=%v SecurityCode = %q, want %q", iosMode, got, want)
		}
		oneCode := p.GetString("OneCode")
		coreV := p.GetString("CoreV")
		if len(oneCode) != 64 || len(coreV) != 64 {
			t.Fatalf("IOSMode=%v OneCode/CoreV lengths = %d/%d, want 64/64", iosMode, len(oneCode), len(coreV))
		}
		seed := make([]byte, 32)
		for i := range seed {
			seed[i] = oneCode[i*2+1]
			if seed[i] < '0' || seed[i] > '9' {
				t.Fatalf("IOSMode=%v OneCode[%d] = %q, want a digit", iosMode, i*2+1, seed[i])
			}
		}
		wantOne, wantCore := oneCodeAndCoreVFrom(string(seed))
		if oneCode != wantOne || coreV != wantCore {
			t.Errorf("IOSMode=%v OneCode/CoreV do not match the shared 32-digit seed", iosMode)
		}
	}
}

func TestPackageSignMatchesKnownValue(t *testing.T) {
	// sha1("com.fun.lastwar.gp") lowercase hex, computed independently.
	got := packageSignHex(gsl.PackageName)
	if len(got) != 40 {
		t.Errorf("expected 40-char sha1 hex, got %d: %q", len(got), got)
	}
	// Confirmed live against a real captured iOS Login request.
	const wantIOS = "506d9b737f4da295c6050b8d9492e00ba00605c0"
	if got := packageSignHex(iosPackageName); got != wantIOS {
		t.Errorf("packageSignHex(iosPackageName) = %q, want %q", got, wantIOS)
	}
}
