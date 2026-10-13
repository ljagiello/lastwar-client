package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
)

// decryptingGSL is a fake getserverlist.php that decrypts each request form the way the server
// does and answers reply. forms returns every decoded form so far.
func decryptingGSL(t *testing.T, priv *rsa.PrivateKey, reply string) (server *httptest.Server, forms func() []url.Values) {
	t.Helper()
	var mu sync.Mutex
	var got []url.Values
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			return
		}
		saltCT, _ := crypto.URLSafeB64Decode(r.FormValue("uuid"))
		salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT) //nolint:staticcheck // SA1019: the fake server mirrors the protocol's PKCS#1 v1.5 salt
		if err != nil {
			return
		}
		data, _ := crypto.URLSafeB64Decode(r.FormValue("data"))
		plain, err := crypto.AESECBDecryptPKCS7(data, crypto.MD5HexKey(string(salt)))
		if err != nil {
			return
		}
		form, _ := url.ParseQuery(string(plain))
		mu.Lock()
		got = append(got, form)
		mu.Unlock()
		_, _ = fmt.Fprint(w, reply)
	}))
	t.Cleanup(server.Close)
	return server, func() []url.Values {
		mu.Lock()
		defer mu.Unlock()
		return append([]url.Values(nil), got...)
	}
}

func testRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	return priv
}

// TestRefreshTokensRequest: the refresh sends opt=refresh with the refresh token, the issuing
// client's platform, and the session's device, zone and gameUid, and never a loginKey.
func TestRefreshTokensRequest(t *testing.T) {
	priv := testRSAKey(t)
	for _, c := range []struct {
		iosMode      bool
		wantPlatform string
	}{{true, "iOS"}, {false, gsl.Platform}} {
		t.Run(c.wantPlatform, func(t *testing.T) {
			server, forms := decryptingGSL(t, priv, `{"code":0,"at":{"token":"at-new","time":1791900000},"serverList":[{"id":"783","ip":"h","port":"10783","gameUid":"uid-1"}]}`)
			if _, err := RefreshTokens(TokenRefresh{
				HTTPClient: gsl.DefaultHTTPClient(), GateHost: server.URL, RSAPub: &priv.PublicKey,
				DeviceID: "dev_n3d", IOSMode: c.iosMode, Zone: "APS783", GameUid: "uid-1", RefreshTok: "rt-old",
			}); err != nil {
				t.Fatal(err)
			}
			got := forms()
			if len(got) != 1 {
				t.Fatalf("requests = %d, want 1", len(got))
			}
			want := map[string]string{"opt": "refresh", "rt": "rt-old", "platform": c.wantPlatform, "uuid": "dev_n3d", "zone": "APS783", "gameuid": "uid-1"}
			for k, v := range want {
				if g := got[0].Get(k); g != v {
					t.Errorf("%s = %q, want %q", k, g, v)
				}
			}
			if got[0].Has("loginKey") {
				t.Error("request carries a loginKey")
			}
		})
	}
}

// TestRefreshTokensReply: what comes back from each reply shape. A rotated rt is returned; a
// missing one comes back empty so the caller keeps its own; an empty server list doesn't stop a
// fresh token; a non-zero code or a reply without an access token fails, 211/212 as
// gsl.ErrReauthNeeded.
func TestRefreshTokensReply(t *testing.T) {
	priv := testRSAKey(t)
	cases := []struct {
		name       string
		reply      string
		want       Tokens
		wantErr    bool
		wantReauth bool
	}{
		{"rotated rt", `{"code":0,"at":{"token":"at-new","time":1791900000},"rt":{"token":"rt-new","time":1791900001},"serverList":[{"id":"1","ip":"h","port":"1"}]}`,
			Tokens{AccessTok: "at-new", AccessTokTime: 1791900000, RefreshTok: "rt-new", RefreshTokTime: 1791900001}, false, false},
		{"no rt", `{"code":0,"at":{"token":"at-new","time":1791900000},"serverList":[{"id":"1","ip":"h","port":"1"}]}`,
			Tokens{AccessTok: "at-new", AccessTokTime: 1791900000}, false, false},
		{"empty server list", `{"code":0,"at":{"token":"at-new","time":"1791900000"}}`,
			Tokens{AccessTok: "at-new", AccessTokTime: 1791900000}, false, false},
		{"code 211", `{"code":211}`, Tokens{}, true, true},
		{"code 212", `{"code":212}`, Tokens{}, true, true},
		{"code 201", `{"code":201,"at":{"token":"at-new","time":1}}`, Tokens{}, true, false},
		{"no access token", `{"code":0,"serverList":[{"id":"1","ip":"h","port":"1"}]}`, Tokens{}, true, false},
		{"empty access token", `{"code":0,"at":{"token":"","time":1},"serverList":[{"id":"1","ip":"h","port":"1"}]}`, Tokens{}, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			server, _ := decryptingGSL(t, priv, c.reply)
			got, err := RefreshTokens(TokenRefresh{
				HTTPClient: gsl.DefaultHTTPClient(), GateHost: server.URL, RSAPub: &priv.PublicKey,
				DeviceID: "dev_n3d", Zone: "APS783", GameUid: "uid-1", RefreshTok: "rt-old",
			})
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if got := errors.Is(err, gsl.ErrReauthNeeded); got != c.wantReauth {
				t.Errorf("errors.Is(err, ErrReauthNeeded) = %v, want %v (err %v)", got, c.wantReauth, err)
			}
			// Tokens redacts itself when printed, so compare and report the fields.
			if got.AccessTok != c.want.AccessTok || got.RefreshTok != c.want.RefreshTok {
				t.Errorf("at/rt = %q/%q, want %q/%q", got.AccessTok, got.RefreshTok, c.want.AccessTok, c.want.RefreshTok)
			}
			if got.AccessTokTime != c.want.AccessTokTime || got.RefreshTokTime != c.want.RefreshTokTime {
				t.Errorf("at.time/rt.time = %d/%d, want %d/%d", got.AccessTokTime, got.RefreshTokTime, c.want.AccessTokTime, c.want.RefreshTokTime)
			}
		})
	}
}

// TestRefreshTokensNeedsRefreshToken: without a refresh token there is nothing to send, so no
// request is made.
func TestRefreshTokensNeedsRefreshToken(t *testing.T) {
	_, err := RefreshTokens(TokenRefresh{HTTPClient: &http.Client{Transport: failTransport{t}}, GateHost: "http://gsl.invalid", GameUid: "uid-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestRefreshTokensDoesNotLogTokens: neither the refresh token sent nor the tokens received reach
// the log, and the types holding them redact themselves.
func TestRefreshTokensDoesNotLogTokens(t *testing.T) {
	priv := testRSAKey(t)
	server, _ := decryptingGSL(t, priv, `{"code":0,"at":{"token":"at-secret-new","time":1},"rt":{"token":"rt-secret-new","time":1},"serverList":[{"id":"1","ip":"h","port":"1"}]}`)
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	r := TokenRefresh{HTTPClient: gsl.DefaultHTTPClient(), GateHost: server.URL, RSAPub: &priv.PublicKey,
		DeviceID: "dev_n3d", GameUid: "uid-1", RefreshTok: "rt-secret-old"}
	got, err := RefreshTokens(r)
	if err != nil {
		t.Fatal(err)
	}
	slog.Info("struct values", "refresh", r, "tokens", got)
	logged := buf.String() + fmt.Sprintf("%v %+v %#v %v %+v %#v", r, r, r, got, got, got)
	for _, secret := range []string{"rt-secret-old", "at-secret-new", "rt-secret-new"} {
		if strings.Contains(logged, secret) {
			t.Errorf("output contains %q:\n%s", secret, logged)
		}
	}
	if !strings.Contains(buf.String(), "refreshTokenRotated=true") {
		t.Errorf("want the rotation logged:\n%s", buf.String())
	}
}
