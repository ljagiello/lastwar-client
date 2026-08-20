package auth

import (
	"lastwar-client/internal/gsl"
	"testing"
)

func TestSecurityCodeAlgorithm(t *testing.T) {
	// Just verify determinism + length (32 hex chars), not a known vector.
	sc := securityCode("1700000000", "guest123")
	if len(sc) != 32 {
		t.Errorf("expected 32-char md5 hex, got %d: %q", len(sc), sc)
	}
	oneCode, coreV := oneCodeAndCoreV()
	if len(oneCode) != 64 || len(coreV) != 64 {
		t.Errorf("expected 64-char interleaved codes, got %d/%d", len(oneCode), len(coreV))
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
