package gsl

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"lastwar-client/internal/crypto"
	"net/http"
	"net/http/httptest"
	"testing"
)

// productionIOSBuild is the build the cron's captured iOS session claims.
var productionIOSBuild = ClientBuild{PackageName: "com.lastwar.ios", Platform: "iOS", AppVersion: "1.0.370", VersionCode: "918"}

// TestProductionIOSCheckVersionQueryGolden pins the production iOS check-version query (rule 10 of
// the 1.0.370 audit brief). The client's own URL is NetworkURLConfig.GetCheckVersionURL
// (Assembly-CSharp.decompiled.cs:14144-14158).
func TestProductionIOSCheckVersionQueryGolden(t *testing.T) {
	const want = "appVersion=1.0.370&buildId=918&deviceId=&gm=0&packageName=com.lastwar.ios&platform=iOS&returnJson=1&server=APS783&table_env=table_online&uid=&unityVersion=440"
	if got := checkVersionQuery(productionIOSBuild, "APS783").Encode(); got != want {
		t.Errorf("production iOS check-version query changed:\n got %s\nwant %s", got, want)
	}
}

// TestProductionIOSServerLookupFormGolden pins the plaintext form of the production iOS GSL call (a
// no-opt server lookup; rule 10 of the 1.0.370 audit brief), decrypted the way the server does.
// The client builds it in GetServerListRequest (Assembly-CSharp.decompiled.cs:77430-77488).
func TestProductionIOSServerLookupFormGolden(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	var plain string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			return
		}
		saltCT, _ := crypto.URLSafeB64Decode(r.FormValue("uuid"))
		salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT) //nolint:staticcheck // SA1019: the fake server mirrors the protocol's PKCS#1 v1.5 salt
		if err != nil {
			return
		}
		data, _ := crypto.URLSafeB64Decode(r.FormValue("data"))
		b, err := crypto.AESECBDecryptPKCS7(data, crypto.MD5HexKey(string(salt)))
		if err != nil {
			return
		}
		plain = string(b)
		_, _ = fmt.Fprint(w, `{"code":0,"serverList":[{"id":"1"}]}`)
	}))
	defer server.Close()

	const deviceID = "0123456789ABCDEF0123456789ABCDEF_n3d"
	if _, err := GetServerListAs(DefaultHTTPClient(), server.URL, &priv.PublicKey, "iOS", deviceID, GSLOpt{}, "APS783", "1234567890123"); err != nil {
		t.Fatalf("GetServerListAs: %v", err)
	}
	const want = "uuid=0123456789ABCDEF0123456789ABCDEF_n3d&airKey=lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk&loginFlag=1&country=US&is3D=1&lang=en&simOp=&platform=iOS&isSimulator=0&zone=APS783&gameuid=1234567890123&newServer=1&openCountry=US"
	if plain != want {
		t.Errorf("production iOS GSL form changed:\n got %s\nwant %s", plain, want)
	}
}
