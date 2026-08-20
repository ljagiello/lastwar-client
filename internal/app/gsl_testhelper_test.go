package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

// testRSAPubKeyDER returns a fresh 2048-bit RSA public key, DER+base64 encoded, for the game
// package's fake-GSL-server tests. Duplicated from internal/gsl's own copy: test helpers can't be
// shared across package boundaries.
func testRSAPubKeyDER(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}
