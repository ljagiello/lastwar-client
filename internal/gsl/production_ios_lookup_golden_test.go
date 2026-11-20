package gsl_test

import (
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"lastwar-client/internal/auth"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
)

// localOnlyTransport sends a request only to the test server's host and fails every other one, so
// the lookup below can never reach a real GSL host.
type localOnlyTransport struct {
	t    *testing.T
	host string
}

func (l localOnlyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != l.host {
		l.t.Errorf("request to %s: only the local test server may be called", r.URL.Host)
		return nil, fmt.Errorf("refused host %s", r.URL.Host)
	}
	return http.DefaultTransport.RoundTrip(r)
}

// TestProductionIOSLookupGameServerFormGolden pins the plaintext GSL form of the production iOS
// lookup on the path the cron takes (final-r3 auth-transport M8): auth.LookupGameServer, which
// builds the form's device fields with auth.FormDevice and sends with gsl.GetServerListFor. With no
// captured device, the form is byte for byte TestProductionIOSServerLookupFormGolden's (the
// production session config has no captured device yet). With a captured device, its fromCountry
// and lang fill country and lang, and nothing else moves. The client builds the form in
// GetServerListRequest (Assembly-CSharp.decompiled.cs:77430-77488). The device values are made up.
func TestProductionIOSLookupGameServerFormGolden(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	const deviceID = "0123456789ABCDEF0123456789ABCDEF_n3d"
	const gameUID = "1234567890123"
	for _, c := range []struct {
		name   string
		device auth.CapturedDevice
		want   string
	}{
		{"no captured device", auth.CapturedDevice{},
			"uuid=0123456789ABCDEF0123456789ABCDEF_n3d&airKey=lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk&loginFlag=1&country=US&is3D=1&lang=en&simOp=&platform=iOS&isSimulator=0&zone=APS783&gameuid=1234567890123&newServer=1&openCountry=US"},
		{"captured device", auth.CapturedDevice{FromCountry: "CA", Lang: "fr"},
			"uuid=0123456789ABCDEF0123456789ABCDEF_n3d&airKey=lwDid_MDEyMzQ1Njc4OUFCQ0RFRjAxMjM0NTY3ODlBQkNERUZfbjNk&loginFlag=1&country=CA&is3D=1&lang=fr&simOp=&platform=iOS&isSimulator=0&zone=APS783&gameuid=1234567890123&newServer=1&openCountry=US"},
	} {
		t.Run(c.name, func(t *testing.T) {
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
				_, _ = fmt.Fprintf(w, `{"code":0,"serverList":[{"id":"783","ip":"c.example","port":"10783","zone":"APS783","gameUid":%q}]}`, gameUID)
			}))
			defer server.Close()
			u, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: localOnlyTransport{t: t, host: u.Host}, Timeout: gsl.DefaultHTTPClient().Timeout}

			s, err := auth.LookupGameServer(auth.ServerLookup{
				HTTPClient: client, GateHost: server.URL, RSAPub: &priv.PublicKey, DeviceID: deviceID,
				IOSMode: true, Zone: "APS783", GameUid: gameUID, AccessTok: "at-test", Device: c.device,
			})
			if err != nil {
				t.Fatalf("LookupGameServer: %v", err)
			}
			if s.IP != "c.example" || s.Port != 10783 {
				t.Errorf("server %s:%d, want c.example:10783", s.IP, s.Port)
			}
			if plain != c.want {
				t.Errorf("production iOS lookup form changed:\n got %s\nwant %s", plain, c.want)
			}
		})
	}
}
