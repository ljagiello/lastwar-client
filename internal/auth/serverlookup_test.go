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
	"testing"

	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
)

// TestGameServerFromPicksTheRole: the entry carrying the session's gameUid wins over
// serverList[0] and lastLoggedServer, and its gateways, port and zone come back.
func TestGameServerFromPicksTheRole(t *testing.T) {
	lsr := gslReply(t, `{"code":0,"lastLoggedServer":"72","serverList":[
		{"id":"72","ip":"a.example|b.example","port":"10072","zone":"APS72","gameUid":"999"},
		{"id":"783","ip":"c.example|d.example","port":"10783","zone":"APS783","gameUid":"1000000000000783"}]}`)
	s, err := gameServerFrom(lsr, "1000000000000783", "APS8092", "at-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (GameServer{IP: "c.example|d.example", Port: 10783, Zone: "APS783"}); s != want {
		t.Errorf("server = %+v, want %+v", s, want)
	}
}

// TestGameServerFromKeepsZoneAndAcceptsUnnamedEntry: an entry without a zone keeps the session's,
// and an entry without a gameUid can't be told apart from the role's, so it is used.
func TestGameServerFromKeepsZoneAndAcceptsUnnamedEntry(t *testing.T) {
	lsr := gslReply(t, `{"code":0,"serverList":[{"id":"1","ip":"h.example","port":"10001"}]}`)
	s, err := gameServerFrom(lsr, "uid-1", "APS1", "at-1")
	if err != nil {
		t.Fatal(err)
	}
	if want := (GameServer{IP: "h.example", Port: 10001, Zone: "APS1"}); s != want {
		t.Errorf("server = %+v, want %+v", s, want)
	}
}

func TestGameServerFromRejects(t *testing.T) {
	cases := map[string]string{
		"gsl code 211":  `{"code":211,"serverList":[{"id":"1","ip":"h","port":"1"}]}`,
		"no serverList": `{"code":0}`,
		// PickServer falls back to lastLoggedServer, which is another role of the account.
		"another role": `{"code":0,"lastLoggedServer":"72","serverList":[{"id":"72","ip":"h","port":"10072","zone":"APS72","gameUid":"999"}]}`,
		"no ip":        `{"code":0,"serverList":[{"id":"1","port":"10001","gameUid":"uid-1"}]}`,
		"no port":      `{"code":0,"serverList":[{"id":"1","ip":"h","gameUid":"uid-1"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := gameServerFrom(gslReply(t, body), "uid-1", "APS1", "at-1")
			if err == nil {
				t.Fatal("expected an error")
			}
			if name == "gsl code 211" && !errors.Is(err, gsl.ErrReauthNeeded) {
				t.Errorf("error %q should wrap gsl.ErrReauthNeeded, so main exits 2", err)
			}
		})
	}
}

// TestGameServerFromTokenDiagnostics: a reply token that differs from the session's is reported at
// Warn and the same token is not, and neither token's value reaches the log.
func TestGameServerFromTokenDiagnostics(t *testing.T) {
	for _, c := range []struct {
		name, replyTok string
		wantWarn       bool
	}{
		{"different token", "at-from-gsl-secret", true},
		{"same token", "at-session-secret", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(orig)

			body := fmt.Sprintf(`{"code":0,"at":{"token":%q,"time":1},"rt":{"token":"rt-secret","time":1},
				"serverList":[{"id":"1","ip":"h","port":"10001","gameUid":"uid-1"}]}`, c.replyTok)
			if _, err := gameServerFrom(gslReply(t, body), "uid-1", "APS1", "at-session-secret"); err != nil {
				t.Fatal(err)
			}
			logged := buf.String()
			if got := strings.Contains(logged, "level=WARN"); got != c.wantWarn {
				t.Errorf("Warn logged = %v, want %v:\n%s", got, c.wantWarn, logged)
			}
			if !strings.Contains(logged, "replyHasRefreshToken=true") {
				t.Errorf("want the refresh-token presence logged:\n%s", logged)
			}
			for _, secret := range []string{c.replyTok, "at-session-secret", "rt-secret"} {
				if strings.Contains(logged, secret) {
					t.Errorf("log contains %q:\n%s", secret, logged)
				}
			}
		})
	}
}

// failTransport fails the test on any HTTP request.
type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f.t.Errorf("unexpected HTTP request to %s", r.URL)
	return nil, errors.New("unexpected request")
}

// TestLookupGameServerNeedsGameUid: without a gameUid, ChooseOpt would pick opt=new, which asks GSL
// for a brand-new account, so the lookup refuses before any request.
func TestLookupGameServerNeedsGameUid(t *testing.T) {
	_, err := LookupGameServer(ServerLookup{HTTPClient: &http.Client{Transport: failTransport{t}}, GateHost: "http://gsl.invalid", AccessTok: "at-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
}

// TestLookupGameServerRequest decrypts the getserverlist.php form the way the server does: the
// lookup sends opt=fix with the session's device, zone and gameUid, the issuing client's platform,
// and never a refresh token or loginKey.
func TestLookupGameServerRequest(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	for _, c := range []struct {
		iosMode      bool
		wantPlatform string
	}{{true, "iOS"}, {false, gsl.Platform}} {
		t.Run(c.wantPlatform, func(t *testing.T) {
			var form url.Values
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
				plain, err := crypto.AESECBDecryptPKCS7(data, crypto.MD5HexKey(string(salt)))
				if err != nil {
					return
				}
				// The plaintext is "k=v&..." with unescaped values; ParseQuery is fine for these.
				form, _ = url.ParseQuery(string(plain))
				_, _ = fmt.Fprint(w, `{"code":0,"serverList":[{"id":"783","ip":"c.example","port":"10783","zone":"APS783","gameUid":"uid-1"}]}`)
			}))
			defer server.Close()

			s, err := LookupGameServer(ServerLookup{
				HTTPClient: gsl.DefaultHTTPClient(), GateHost: server.URL, RSAPub: &priv.PublicKey,
				DeviceID: "dev_n3d", IOSMode: c.iosMode, Zone: "APS783", GameUid: "uid-1", AccessTok: "at-1",
			})
			if err != nil {
				t.Fatal(err)
			}
			if s.IP != "c.example" || s.Port != 10783 {
				t.Errorf("server = %+v, want c.example:10783", s)
			}
			if form == nil {
				t.Fatal("fake server never decoded a request")
			}
			want := map[string]string{"opt": "fix", "platform": c.wantPlatform, "uuid": "dev_n3d", "zone": "APS783", "gameuid": "uid-1"}
			for k, v := range want {
				if got := form.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for _, k := range []string{"rt", "loginKey"} {
				if form.Has(k) {
					t.Errorf("request carries %s, want none", k)
				}
			}
		})
	}
}
