package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"testing"
)

// Confirms the GSL request/response crypto envelope round-trips end to end -- NewGSLCrypto,
// EncryptRequest, and DecryptResponse composed together, not just the lower-level AES-ECB/PKCS7
// primitives selftest_test.go already covers.
func TestGSLCryptoRoundTrip(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}

	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	b64Der := base64.StdEncoding.EncodeToString(der)

	pub, err := parseRSAPubKeyFromDER(b64Der)
	if err != nil {
		t.Fatalf("parseRSAPubKeyFromDER: %v", err)
	}

	gc := NewGSLCrypto(pub)
	plainForm := "uuid=test-device&opt=new"
	uuidField, dataField, err := gc.EncryptRequest(plainForm)
	if err != nil {
		t.Fatalf("EncryptRequest: %v", err)
	}

	// Simulate the server side: decrypt uuidField with the real private key to recover the
	// salt, exactly as the real GSL server would.
	saltCT, err := urlSafeB64Decode(uuidField)
	if err != nil {
		t.Fatalf("decode uuid field: %v", err)
	}
	salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
	if err != nil {
		t.Fatalf("rsa decrypt salt: %v", err)
	}

	recoveredKey := md5HexKey(string(salt))
	encForm, err := urlSafeB64Decode(dataField)
	if err != nil {
		t.Fatalf("decode data field: %v", err)
	}
	recoveredForm, err := aesECBDecryptPKCS7(encForm, recoveredKey)
	if err != nil {
		t.Fatalf("aes decrypt form: %v", err)
	}
	if string(recoveredForm) != plainForm {
		t.Fatalf("recovered form = %q, want %q", recoveredForm, plainForm)
	}

	// Now the response direction: the "server" encrypts a reply with the same salt-derived key,
	// and DecryptResponse (using the salt already in scope on gc from the EncryptRequest call
	// above) must recover it.
	serverReply := `{"code":0,"serverList":[]}`
	encReply, err := aesECBEncryptPKCS7([]byte(serverReply), recoveredKey)
	if err != nil {
		t.Fatalf("aes encrypt reply: %v", err)
	}
	binField := urlSafeB64Encode(encReply)

	gotReply, err := gc.DecryptResponse(binField)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if gotReply != serverReply {
		t.Fatalf("got reply %q, want %q", gotReply, serverReply)
	}
}
