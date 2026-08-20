package crypto

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"math/big"
	"strings"
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

	pub, err := ParseRSAPubKeyFromDER(b64Der)
	if err != nil {
		t.Fatalf("ParseRSAPubKeyFromDER: %v", err)
	}

	gc := NewGSLCrypto(pub)
	plainForm := "uuid=test-device&opt=new"
	uuidField, dataField, err := gc.EncryptRequest(plainForm)
	if err != nil {
		t.Fatalf("EncryptRequest: %v", err)
	}

	// Simulate the server side: decrypt uuidField with the real private key to recover the
	// salt, exactly as the real GSL server would.
	saltCT, err := URLSafeB64Decode(uuidField)
	if err != nil {
		t.Fatalf("decode uuid field: %v", err)
	}
	salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT)
	if err != nil {
		t.Fatalf("rsa decrypt salt: %v", err)
	}

	recoveredKey := MD5HexKey(string(salt))
	encForm, err := URLSafeB64Decode(dataField)
	if err != nil {
		t.Fatalf("decode data field: %v", err)
	}
	recoveredForm, err := AESECBDecryptPKCS7(encForm, recoveredKey)
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
	encReply, err := AESECBEncryptPKCS7([]byte(serverReply), recoveredKey)
	if err != nil {
		t.Fatalf("aes encrypt reply: %v", err)
	}
	binField := URLSafeB64Encode(encReply)

	gotReply, err := gc.DecryptResponse(binField)
	if err != nil {
		t.Fatalf("DecryptResponse: %v", err)
	}
	if gotReply != serverReply {
		t.Fatalf("got reply %q, want %q", gotReply, serverReply)
	}
}

// TestGSLCryptoEncryptRequestFailurePreservesSalt is the round 26 regression test: EncryptRequest
// used to set g.salt to the freshly generated salt *before* the RSA and AES encryption steps that
// can still fail, so a failed call left g.salt pointing at a salt value that was never actually
// sent to the server. DecryptResponse's only guard against operating without a real salt is an
// empty-string check on g.salt, so that stale-but-unused salt would silently satisfy the guard and
// blow up later with a confusing PKCS7Unpad error instead of the intended "no salt in scope"
// message. This drives EncryptRequest's RSA step to fail deterministically -- without needing a
// malformed key that could panic checkPub -- by using a structurally valid but undersized RSA
// public key: rsa.EncryptPKCS1v15 computes k = pub.Size() and rejects any message where
// k-11 < len(msg) before doing any real RSA math, so a tiny modulus is enough to force
// ErrMessageTooLong for our 20-byte salt.
func TestGSLCryptoEncryptRequestFailurePreservesSalt(t *testing.T) {
	tinyPub := &rsa.PublicKey{N: big.NewInt(3233), E: 17}

	// Case 1: a prior successful EncryptRequest already left a real salt in scope. A subsequent
	// failing call must leave that salt completely untouched -- not overwritten with the new,
	// never-sent salt.
	gc := NewGSLCrypto(tinyPub)
	const preSalt = "pre-existing-salt-from-a-prior-successful-call"
	gc.salt = preSalt

	if _, _, err := gc.EncryptRequest("uuid=test-device&opt=new"); err == nil {
		t.Fatalf("EncryptRequest: expected error from undersized RSA key, got nil")
	}
	if gc.salt != preSalt {
		t.Fatalf("gc.salt after failed EncryptRequest = %q, want unchanged %q", gc.salt, preSalt)
	}

	// Case 2: no prior successful call, so g.salt starts empty. A failing EncryptRequest call
	// must leave it empty, and a subsequent DecryptResponse call must still get the intended
	// "no salt in scope" error rather than proceeding with a stale, never-sent salt into a
	// confusing downstream PKCS7Unpad failure.
	gc2 := NewGSLCrypto(tinyPub)
	if _, _, err := gc2.EncryptRequest("uuid=test-device&opt=new"); err == nil {
		t.Fatalf("EncryptRequest: expected error from undersized RSA key, got nil")
	}
	if gc2.salt != "" {
		t.Fatalf("gc2.salt after failed EncryptRequest = %q, want empty", gc2.salt)
	}

	_, err := gc2.DecryptResponse("irrelevant-bin-field")
	if err == nil {
		t.Fatalf("DecryptResponse after failed EncryptRequest: expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no salt in scope") {
		t.Fatalf("DecryptResponse after failed EncryptRequest: got error %q, want it to contain %q", err.Error(), "no salt in scope")
	}
}

// TestParseRSAPubKeyFromDER is the round-39 regression test covering ParseRSAPubKeyFromDER's
// three error branches, previously exercised only indirectly (and only on the success path) via
// TestGSLCryptoRoundTrip above. resMsg is attacker-influenced network input (dossier §02's
// CheckVersion response), so each of these anomaly shapes is a real, non-theoretical input this
// function must reject with a distinct, identifiable error rather than panicking.
func TestParseRSAPubKeyFromDER(t *testing.T) {
	t.Run("invalid base64", func(t *testing.T) {
		_, err := ParseRSAPubKeyFromDER("not-valid-base64!!!")
		if err == nil {
			t.Fatal("ParseRSAPubKeyFromDER: expected error for invalid base64, got nil")
		}
		if !strings.Contains(err.Error(), "base64 decode") {
			t.Fatalf("got error %q, want it to contain %q", err.Error(), "base64 decode")
		}
	})
	t.Run("valid base64 but invalid DER", func(t *testing.T) {
		_, err := ParseRSAPubKeyFromDER(base64.StdEncoding.EncodeToString([]byte("not a DER-encoded SubjectPublicKeyInfo")))
		if err == nil {
			t.Fatal("ParseRSAPubKeyFromDER: expected error for invalid DER, got nil")
		}
		if !strings.Contains(err.Error(), "parse SubjectPublicKeyInfo") {
			t.Fatalf("got error %q, want it to contain %q", err.Error(), "parse SubjectPublicKeyInfo")
		}
	})
	t.Run("valid DER but non-RSA key", func(t *testing.T) {
		ecdsaPriv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("generate ECDSA key: %v", err)
		}
		der, err := x509.MarshalPKIXPublicKey(&ecdsaPriv.PublicKey)
		if err != nil {
			t.Fatalf("marshal ECDSA public key: %v", err)
		}
		_, err = ParseRSAPubKeyFromDER(base64.StdEncoding.EncodeToString(der))
		if err == nil {
			t.Fatal("ParseRSAPubKeyFromDER: expected error for non-RSA key, got nil")
		}
		if !strings.Contains(err.Error(), "not RSA") {
			t.Fatalf("got error %q, want it to contain %q", err.Error(), "not RSA")
		}
	})
}
