package testutil

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// UseDecryptingGSL is UseInMemoryGSL with a fake that also reads the requests: it keeps the RSA
// private key behind the check-version reply's public key, decrypts each getserverlist.php form
// the way the server does, and records it. gslResponses answer the calls in order, repeating the
// last. forms returns the decoded forms so far.
func UseDecryptingGSL(t *testing.T, gslResponses ...gsl.LoginServerListRespon) (client *http.Client, forms func() []url.Values) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	pub := base64.StdEncoding.EncodeToString(der)

	var mu sync.Mutex
	var got []url.Values
	call := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			_ = json.NewEncoder(w).Encode(gsl.CheckVersionResponse{ResMsg: gsl.FlexString(pub)})
		case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
			form := url.Values{}
			if err := r.ParseForm(); err == nil {
				saltCT, _ := crypto.URLSafeB64Decode(r.FormValue("uuid"))
				if salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT); err == nil { //nolint:staticcheck // SA1019: the fake server mirrors the protocol's PKCS#1 v1.5 salt
					data, _ := crypto.URLSafeB64Decode(r.FormValue("data"))
					if plain, err := crypto.AESECBDecryptPKCS7(data, crypto.MD5HexKey(string(salt))); err == nil {
						form, _ = url.ParseQuery(string(plain))
					}
				}
			}
			mu.Lock()
			got = append(got, form)
			idx := min(call, len(gslResponses)-1)
			call++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(gslResponses[idx])
		default:
			http.NotFound(w, r)
		}
	})

	orig := gsl.CheckVersionHosts
	gsl.CheckVersionHosts = []string{"http://gsl.invalid"}
	t.Cleanup(func() { gsl.CheckVersionHosts = orig })
	return &http.Client{Transport: recorderRoundTripper{h: handler}}, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), got...)
	}
}
