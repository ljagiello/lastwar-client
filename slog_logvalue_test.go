package main

import (
	"bytes"
	"crypto/rsa"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"log/slog"
	"strings"
	"testing"
)

// TestCredentialTypesLogValueProtectsJSONHandler is the round-53 regression test for the MAJOR
// finding that every credential-bearing type in this codebase relying solely on String()/
// GoString() for redaction was completely unprotected the moment one was passed as a raw slog
// attribute value: main.go installs slog.NewJSONHandler exclusively, and encoding/json never
// consults fmt.Stringer/fmt.GoStringer -- only slog.LogValuer, which slog resolves before handler
// dispatch. Confirmed via direct reproduction (the same technique the audit itself used): logging
// each type through the EXACT slog.NewJSONHandler construction main.go uses, with a
// uniquely-identifiable marker string in its credential field, and asserting that marker never
// appears in the JSON output while the type's own redacted placeholder does.
func TestCredentialTypesLogValueProtectsJSONHandler(t *testing.T) {
	const marker = "MUST-NOT-LEAK-1234567890"

	tests := []struct {
		name  string
		value any
		want  string
	}{
		{"SessionConfig", SessionConfig{AccessToken: marker}, "[REDACTED SessionConfig]"},
		{"CrossServerLoginResult", CrossServerLoginResult{AccessTok: marker}, "[REDACTED CrossServerLoginResult]"},
		{"CrossServerLoginParams", CrossServerLoginParams{AccessTok: marker}, "[REDACTED CrossServerLoginParams]"},
		{"LoginToken", gsl.LoginToken{Token: marker}, "[REDACTED LoginToken]"},
		{"GSLOpt", gsl.GSLOpt{LoginKey: marker}, "[REDACTED GSLOpt]"},
		{"deviceIdentity", deviceIdentity{LoginKey: marker}, "[REDACTED deviceIdentity]"},
		{"LoginParamsInput", LoginParamsInput{AccessTok: marker}, "[REDACTED LoginParamsInput]"},
		{"LoginOptions", LoginOptions{Email: marker}, "[REDACTED LoginOptions]"},
		{"crossServerTestOpts", crossServerTestOpts{at: marker}, "[REDACTED crossServerTestOpts]"},
		{"SFSValue", sfs.SFSValue{Type: sfs.SFSUtfString, Val: marker}, "[REDACTED SFSValue]"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			logger.Info("test message", "value", tt.value)

			got := buf.String()
			if strings.Contains(got, marker) {
				t.Errorf("%s: JSON log output contains the raw marker value -- LogValue() did not protect it: %s", tt.name, got)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("%s: JSON log output does not contain the expected redacted placeholder %q: %s", tt.name, tt.want, got)
			}
		})
	}
}

// TestCrossServerLoginParamsLogValueWithGSLPlumbing is a companion to
// TestCredentialTypesLogValueProtectsJSONHandler above, specifically for
// CrossServerLoginParams: its HTTPClient/RSAPub fields hold live pointers that, if LogValue() were
// ever accidentally removed and the struct fell through to a raw json.Marshal, would themselves
// fail to marshal (an *rsa.PublicKey's exported fields are also not directly JSON-safe in every
// Go version) -- proving LogValue() short-circuits before json.Marshal ever has to touch them, not
// just that the string result happens to omit the credential.
func TestCrossServerLoginParamsLogValueWithGSLPlumbing(t *testing.T) {
	const marker = "MUST-NOT-LEAK-cross-server-plumbing"

	p := CrossServerLoginParams{
		AccessTok:  marker,
		RSAPub:     &rsa.PublicKey{},
		HTTPClient: nil,
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	logger.Info("test message", "value", p)

	got := buf.String()
	if strings.Contains(got, marker) {
		t.Errorf("JSON log output contains the raw marker value: %s", got)
	}
	if !strings.Contains(got, "[REDACTED CrossServerLoginParams]") {
		t.Errorf("JSON log output does not contain the expected redacted placeholder: %s", got)
	}
}
