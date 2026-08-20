package auth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestDeviceIdentityLogValueRedacts verifies deviceIdentity's LogValue() protects its secret
// LoginKey from the slog JSON handler -- the auth-package half of the former app-package
// TestCredentialTypesLogValueProtectsJSONHandler table (split out when deviceIdentity became an
// auth-internal type the app test package could no longer construct).
func TestDeviceIdentityLogValueRedacts(t *testing.T) {
	const marker = "MUST-NOT-LEAK-1234567890"
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test message", "value", deviceIdentity{LoginKey: marker})
	got := buf.String()
	if strings.Contains(got, marker) {
		t.Errorf("JSON log output contains the raw marker -- LogValue() did not protect it: %s", got)
	}
	if !strings.Contains(got, "[REDACTED deviceIdentity]") {
		t.Errorf("JSON log output missing the redacted placeholder: %s", got)
	}
}
