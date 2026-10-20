package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Live check-version values (iOS 1.0.344/786 with table_env=table_online, coverage reports
// capture.md §1.4 and tables.md (b)1), shortened past the first separator of each field.
var liveCheckVersionTables = map[string]any{
	"updateType":    "3",
	"hotUpdateMsg":  "GameRes,2159,1,2;DataTable,1166,294,1491981336;Lua,1543,260,1;DllRes,1505,3,4;",
	"lwfile2":       "821|1|2|3|4;820|1|2",
	"table_version": "39516,19518361,f1d9e6a3a8ec1610607583645e3f8377,3418147826;39500|48261|1",
	"locale":        "23570,pl,ja,en",
}

const liveResVersion = "2159.1505_821F_39516.23570"

// fakeGSL serves check-version with resMsg plus cvFields, and getserverlist.php with resp. It
// records every check-version query.
type fakeGSL struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
}

func newFakeGSL(t *testing.T, cvFields map[string]any, resp gsl.LoginServerListRespon) *fakeGSL {
	t.Helper()
	pub := testutil.RSAPubKeyDER(t)
	f := &fakeGSL{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			f.mu.Lock()
			f.queries = append(f.queries, r.URL.Query())
			f.mu.Unlock()
			body := map[string]any{"resMsg": pub}
			for k, v := range cvFields {
				body[k] = v
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGSL) checkVersionQueries() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.queries...)
}

// resVersionLoginServer accepts the base-zone Login, reports its p.resVersion, and sends init.
func resVersionLoginServer(got chan<- string) func(*session.GameConn) {
	return func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if pv, ok := env.Content.Get("p"); ok {
			if p, ok := pv.Val.(*sfs.SFSObject); ok {
				got <- p.GetString("resVersion")
			}
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
}

// TestLoginSendsResVersionFromCheckVersion covers MASTER §5.1 #7/#8 on the main path: check-version
// goes out with table_env=table_online, and the Login carries the resVersion derived from it, or
// "0" when the reply lacks a part.
func TestLoginSendsResVersionFromCheckVersion(t *testing.T) {
	cases := []struct {
		name     string
		cvFields map[string]any
		want     string
	}{
		{"all parts present", liveCheckVersionTables, liveResVersion},
		{"no table_version or locale", map[string]any{"hotUpdateMsg": "GameRes,1,1,1;DllRes,2,1,1", "lwfile2": "3|1"}, "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			got := make(chan string, 1)
			addr := session.StartFakeGameServer(t, resVersionLoginServer(got))
			host, port := testutil.SplitHostPortInt(t, addr)
			f := newFakeGSL(t, c.cvFields, gsl.LoginServerListRespon{
				Code:       "0",
				ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS1", GameUid: "uid-1"}},
				At:         &gsl.LoginToken{Token: "tok-1"},
			})
			testutil.UseFakeGSLServer(t, f.Server)

			result, err := Login(LoginOptions{})
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			defer func() { _ = result.Conn.Close() }()

			select {
			case rv := <-got:
				if rv != c.want {
					t.Errorf("Login p.resVersion = %q, want %q", rv, c.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fake server never received a Login")
			}
			qs := f.checkVersionQueries()
			if len(qs) != 1 {
				t.Fatalf("check-version calls = %d, want 1 (Android mode reuses the gate-host reply)", len(qs))
			}
			if qs[0].Get("table_env") != "table_online" || qs[0].Get("packageName") != gsl.PackageName {
				t.Errorf("check-version query = %v, want table_env=table_online as %s", qs[0], gsl.PackageName)
			}
		})
	}
}

// TestLoginRejectsGSLResultCodes covers MASTER §5.1 #4: a non-zero GSL code, or code 0 with no
// servers, fails Login before any game connection, and 211/212 are marked as needing re-auth.
func TestLoginRejectsGSLResultCodes(t *testing.T) {
	cases := []struct {
		name   string
		resp   gsl.LoginServerListRespon
		want   error
		reauth bool
	}{
		{"211", gsl.LoginServerListRespon{Code: "211"}, gsl.ErrCredentialsInvalid, true},
		{"212", gsl.LoginServerListRespon{Code: "212", ServerList: []gsl.LoginServerInfo{{IP: "127.0.0.1", Port: "1"}}}, gsl.ErrAccountMappingInvalid, true},
		{"201", gsl.LoginServerListRespon{Code: "201"}, gsl.ErrNotAvailable, false},
		{"213", gsl.LoginServerListRespon{Code: "213"}, gsl.ErrEnvelopeCrypto, false},
		{"0 with no servers", gsl.LoginServerListRespon{Code: "0", At: &gsl.LoginToken{Token: "tok-1"}}, gsl.ErrEmptyServerList, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			dialed := false
			orig := dialGame
			dialGame = func(addr string, timeout time.Duration) (*session.GameConn, error) {
				dialed = true
				return nil, errors.New("must not dial")
			}
			t.Cleanup(func() { dialGame = orig })
			testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, c.resp))

			result, err := Login(LoginOptions{})
			if err == nil {
				_ = result.Conn.Close()
				t.Fatal("Login succeeded, want a GSL rejection")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("err = %v, want it to wrap %v", err, c.want)
			}
			if got := errors.Is(err, gsl.ErrReauthNeeded); got != c.reauth {
				t.Errorf("errors.Is(err, gsl.ErrReauthNeeded) = %v, want %v", got, c.reauth)
			}
			if dialed {
				t.Error("Login dialed the game server after a GSL rejection")
			}
		})
	}
}

// TestLoginPicksTheAccountsServer covers MASTER §5.1 #5 on the main path: the entry carrying the
// persisted gameUid wins over lastLoggedServer and serverList[0], and with no gameUid known the
// lastLoggedServer entry wins over serverList[0]. The losing entries point at an address nothing
// listens on, so picking one fails the dial.
func TestLoginPicksTheAccountsServer(t *testing.T) {
	cases := []struct {
		name            string
		persistedUid    string
		reachableUid    string
		lastLoggedFirst bool // lastLoggedServer names the unreachable first entry
	}{
		{"gameUid match beats lastLoggedServer and [0]", "uid-mine", "uid-mine", true},
		{"lastLoggedServer beats [0] when no gameUid is known", "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if c.persistedUid != "" {
				ident, err := LoadOrCreateDeviceIdentity()
				if err != nil {
					t.Fatal(err)
				}
				if err := ident.SaveGameUid(c.persistedUid); err != nil {
					t.Fatal(err)
				}
			}
			addr := session.StartFakeGameServer(t, session.FakeInitPushServer(nil))
			host, port := testutil.SplitHostPortInt(t, addr)
			last := "2"
			if c.lastLoggedFirst {
				last = "1"
			}
			testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
				Code:             "0",
				LastLoggedServer: gsl.FlexString(last),
				ServerList: []gsl.LoginServerInfo{
					{ID: "1", IP: "127.0.0.1", Port: "1", Zone: "APS1", GameUid: "uid-other"},
					{ID: "2", IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS2", GameUid: gsl.FlexString(c.reachableUid)},
				},
				At: &gsl.LoginToken{Token: "tok-1"},
			}))

			result, err := Login(LoginOptions{})
			if err != nil {
				t.Fatalf("Login: %v (it dialed the wrong server entry)", err)
			}
			defer func() { _ = result.Conn.Close() }()
			if c.reachableUid != "" && result.Ident.GameUid != c.reachableUid {
				t.Errorf("Ident.GameUid = %q, want %q", result.Ident.GameUid, c.reachableUid)
			}
		})
	}
}

func TestResVersionFrom(t *testing.T) {
	liveAndroid := &gsl.CheckVersionResponse{
		HotUpdateMsg: "GameRes,2343,1,1;DllRes,1502,1,1", LwFile2: "826|1",
		TableVersion: "39516,1,x", Locale: "23570,en",
	}
	liveIOS := &gsl.CheckVersionResponse{
		HotUpdateMsg: "GameRes,2159,1,2;DllRes,1505,3,4;", LwFile2: "821|1|2|3|4;820|1|2",
		TableVersion: "39516,19518361,f1d9e6a3a8ec1610607583645e3f8377,3418147826;39500|48261|1",
		Locale:       "23570,pl,ja,en",
	}
	if got := ResVersionFrom(liveAndroid, false, "", ""); got != "2343.1502_826F_39516.23570" {
		t.Errorf("android ResVersionFrom = %q", got)
	}
	if got := ResVersionFrom(liveIOS, true, "", ""); got != liveResVersion {
		t.Errorf("ios ResVersionFrom = %q, want %q", got, liveResVersion)
	}

	incomplete := &gsl.CheckVersionResponse{HotUpdateMsg: "GameRes,1,1,1;DllRes,2,1,1", LwFile2: "3|1", Locale: "4,en"}
	for _, c := range []struct {
		name string
		cv   *gsl.CheckVersionResponse
		want string
	}{
		{"no reply", nil, "no check-version reply"},
		{"reply lacks table_version", incomplete, "lacks GameRes"},
	} {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			got := ResVersionFrom(c.cv, true, "", "")
			slog.SetDefault(orig)
			if got != "" {
				t.Errorf("ResVersionFrom = %q, want \"\"", got)
			}
			if n := strings.Count(buf.String(), `level=WARN msg="resVersion:`); n != 1 || !strings.Contains(buf.String(), c.want) {
				t.Errorf("want one resVersion Warn mentioning %q, got:\n%s", c.want, buf.String())
			}
		})
	}
}

// TestCheckVersionForClaimsTheLoginBuild: -cs-ios fetches check-version as the iOS build, and the
// Android path still fetches the Android build. There is no second, other-platform request.
func TestCheckVersionForClaimsTheLoginBuild(t *testing.T) {
	cases := []struct {
		name, app, code, wantApp, wantID, wantPkg, wantPlatform string
		ios                                                     bool
	}{
		{"android", "", "", gsl.AppVersion, gsl.VersionCode, gsl.PackageName, "Android", false},
		{"ios default", "", "", "1.0.344", "786", "com.lastwar.ios", "iOS", true},
		{"ios from the session config", "1.0.350", "801", "1.0.350", "801", "com.lastwar.ios", "iOS", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGSL(t, liveCheckVersionTables, gsl.LoginServerListRespon{})
			testutil.UseFakeGSLServer(t, f.Server)
			if _, _, err := CheckVersionFor(gsl.DefaultHTTPClient(), c.ios, c.app, c.code, "APS783"); err != nil {
				t.Fatal(err)
			}
			qs := f.checkVersionQueries()
			if len(qs) != 1 {
				t.Fatalf("check-version calls = %d, want 1", len(qs))
			}
			want := url.Values{
				"packageName": {c.wantPkg}, "platform": {c.wantPlatform}, "appVersion": {c.wantApp},
				"buildId": {c.wantID}, "server": {"APS783"}, "table_env": {"table_online"},
				"gm": {"0"}, "uid": {""}, "deviceId": {""}, "returnJson": {"1"}, "unityVersion": {"440"},
			}
			if qs[0].Encode() != want.Encode() {
				t.Errorf("check-version query =\n %s\nwant\n %s", qs[0].Encode(), want.Encode())
			}
		})
	}
}

// loginRecordingGSL is a check-version and getserverlist stand-in that records the check-version
// query and the decrypted getserverlist form. Login reaches it through CheckVersionHosts.
func loginRecordingGSL(t *testing.T, tables map[string]any, resp gsl.LoginServerListRespon) (forms, queries func() []url.Values) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pub := base64.StdEncoding.EncodeToString(der)
	var mu sync.Mutex
	var gotForms, gotQueries []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			mu.Lock()
			gotQueries = append(gotQueries, r.URL.Query())
			mu.Unlock()
			body := map[string]any{"resMsg": pub}
			for k, v := range tables {
				body[k] = v
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
			form := url.Values{}
			if err := r.ParseForm(); err == nil {
				saltCT, _ := crypto.URLSafeB64Decode(r.FormValue("uuid"))
				if salt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, saltCT); err == nil { //nolint:staticcheck // SA1019: the fake mirrors the protocol's PKCS#1 v1.5 salt
					data, _ := crypto.URLSafeB64Decode(r.FormValue("data"))
					if plain, err := crypto.AESECBDecryptPKCS7(data, crypto.MD5HexKey(string(salt))); err == nil {
						form, _ = url.ParseQuery(string(plain))
					}
				}
			}
			mu.Lock()
			gotForms = append(gotForms, form)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	testutil.UseFakeGSLServer(t, srv)
	return func() []url.Values {
			mu.Lock()
			defer mu.Unlock()
			return append([]url.Values(nil), gotForms...)
		}, func() []url.Values {
			mu.Lock()
			defer mu.Unlock()
			return append([]url.Values(nil), gotQueries...)
		}
}

// TestLoginIOSModeUsesTheIOSIdentity: with IOSMode, check-version, GSL, the zone Login, and
// account.login.new all claim iOS. The Android path is unchanged when IOSMode is false.
func TestLoginIOSModeUsesTheIOSIdentity(t *testing.T) {
	for _, ios := range []bool{false, true} {
		t.Run(map[bool]string{false: "android", true: "ios"}[ios], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			wantPkg, wantPlatform, wantPF := gsl.PackageName, "Android", "market_global"
			if ios {
				wantPkg, wantPlatform, wantPF = iosPackageName, "iOS", "AppStore"
			}
			loginP := make(chan *sfs.SFSObject, 1)
			finishPF := make(chan string, 1)
			addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
				env, err := server.ReadEnvelope()
				if err != nil {
					return
				}
				if pv, ok := env.Content.Get("p"); ok {
					if p, ok := pv.Val.(*sfs.SFSObject); ok {
						loginP <- p
					}
				}
				ok := sfs.NewSFSObject()
				ok.PutBool("success", true)
				if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, ok); err != nil {
					return
				}
				if err := server.SendExtension("init", sfs.NewSFSObject()); err != nil {
					return
				}
				if _, err := session.ReadNextExtension(server); err != nil {
					return
				}
				if err := server.SendExtension("account.login.send.verify.code", ok); err != nil {
					return
				}
				finish, err := session.ReadNextExtension(server)
				if err != nil {
					return
				}
				finishPF <- finish.Params.GetString("pf")
				if err := server.SendExtension("account.login.new", ok); err != nil {
					return
				}
				push := sfs.NewSFSObject()
				push.PutUtfString("loginKey", "lk")
				push.PutUtfString("gameUid", "uid-1")
				_ = server.SendExtension("push.account.login.new", push)
			})
			host, port := testutil.SplitHostPortInt(t, addr)
			forms, queries := loginRecordingGSL(t, liveCheckVersionTables, gsl.LoginServerListRespon{
				Code:       "0",
				ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS1"}},
				At:         &gsl.LoginToken{Token: "tok-1"},
			})
			pipe := mkfifoT(t, t.TempDir(), "code.pipe")
			go func() {
				f, err := os.OpenFile(pipe, os.O_WRONLY, 0)
				if err != nil {
					return
				}
				defer func() { _ = f.Close() }()
				_, _ = f.WriteString("123456\n")
			}()

			result, err := Login(LoginOptions{Email: "player@example.com", CodePipe: pipe, IOSMode: ios})
			if err != nil {
				t.Fatalf("Login: %v", err)
			}
			defer func() { _ = result.Conn.Close() }()

			qs := queries()
			if len(qs) != 1 {
				t.Fatalf("check-version calls = %d, want 1", len(qs))
			}
			if qs[0].Get("packageName") != wantPkg || qs[0].Get("platform") != wantPlatform {
				t.Errorf("check-version package/platform = %s/%s, want %s/%s", qs[0].Get("packageName"), qs[0].Get("platform"), wantPkg, wantPlatform)
			}
			gotForms := forms()
			if len(gotForms) != 1 {
				t.Fatalf("GSL calls = %d, want 1", len(gotForms))
			}
			if gotForms[0].Get("platform") != wantPlatform {
				t.Errorf("GSL platform = %q, want %s", gotForms[0].Get("platform"), wantPlatform)
			}
			select {
			case p := <-loginP:
				if p.GetString("packageName") != wantPkg || p.GetString("platform") != map[bool]string{false: "1", true: "0"}[ios] || p.GetString("pf") != wantPF {
					t.Errorf("Login package/platform/pf = %s/%s/%s", p.GetString("packageName"), p.GetString("platform"), p.GetString("pf"))
				}
				if got, want := p.GetString("psh"), pshField(ios, p.GetString("cmdBaseTime")); got != want {
					t.Errorf("psh = %q, want %q", got, want)
				}
				if p.GetString("resVersion") != liveResVersion {
					t.Errorf("resVersion = %q, want %q from the %s check-version", p.GetString("resVersion"), liveResVersion, wantPlatform)
				}
				if ios && (p.Has("AndroidID") || p.Has("google_available")) {
					t.Error("iOS Login still carries Android-only fields")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fake server never received a Login")
			}
			select {
			case pf := <-finishPF:
				if pf != wantPF {
					t.Errorf("account.login.new pf = %q, want %q", pf, wantPF)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("fake server never received account.login.new")
			}
		})
	}
}

func TestBuildLoginParamsResVersion(t *testing.T) {
	cases := []struct {
		in, wantRes, wantTa string
	}{
		{"", "0", "0.0"},
		{liveResVersion, liveResVersion, liveResVersion},
	}
	for _, c := range cases {
		for _, iosMode := range []bool{true, false} {
			p := BuildLoginParams(LoginParamsInput{IOSMode: iosMode, ServerID: "783", ResVersion: c.in})
			if got := p.GetString("resVersion"); got != c.wantRes {
				t.Errorf("IOSMode=%v ResVersion %q: resVersion = %q, want %q", iosMode, c.in, got, c.wantRes)
			}
			if ta := p.GetString("ta"); !strings.Contains(ta, `"lw_res_version":"`+c.wantTa+`"`) {
				t.Errorf("IOSMode=%v ResVersion %q: ta lacks lw_res_version %q: %s", iosMode, c.in, c.wantTa, ta)
			}
		}
	}
}

// TestStorefrontPFFollowsTheIdentity covers MASTER §5.1 #28: the Login and account.login.new both
// carry the identity's storefront.
func TestStorefrontPFFollowsTheIdentity(t *testing.T) {
	for _, c := range []struct {
		ios  bool
		want string
	}{{false, "market_global"}, {true, "AppStore"}} {
		loginPF := BuildLoginParams(LoginParamsInput{IOSMode: c.ios, ServerID: "1"}).GetString("pf")
		if loginPF != c.want {
			t.Errorf("iOS=%v: Login pf = %q, want %q", c.ios, loginPF, c.want)
		}
		if got := accountLoginNewParams("a@example.com", "123456", loginPF, "dev", "air").GetString("pf"); got != c.want {
			t.Errorf("iOS=%v: account.login.new pf = %q, want %q", c.ios, got, c.want)
		}
	}
}

// TestSendCheckDeviceChange covers MASTER §5.1 #18: check.device.change goes out with only _id,
// without waiting for the reply (the server here never sends one), and its reply does not disturb
// the next command's wait.
func TestSendCheckDeviceChange(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	got := make(chan *session.ExtensionMessage, 1)
	go func() {
		msg, err := session.ReadNextExtension(server)
		if err != nil {
			return
		}
		got <- msg
		// net.Pipe is synchronous, so read the next command first; then the late
		// check.device.change reply arrives ahead of that command's own reply.
		if _, err := session.ReadNextExtension(server); err != nil {
			return
		}
		reply := sfs.NewSFSObject()
		reply.PutBool("r", false)
		_ = server.SendExtension("check.device.change", reply)
		_ = server.SendExtension("next.cmd", sfs.NewSFSObject())
	}()

	if err := SendCheckDeviceChange(client); err != nil {
		t.Fatalf("SendCheckDeviceChange: %v", err)
	}
	msg := <-got
	if msg.Cmd != "check.device.change" {
		t.Errorf("cmd = %q, want check.device.change", msg.Cmd)
	}
	// The first request on a fresh connection gets _id 2 (Login took 1).
	if keys := msg.Params.Keys(); len(keys) != 1 || keys[0] != "_id" || msg.Params.GetInt("_id") != 2 {
		t.Errorf("params = %s, want only _id=2", msg.Params.StringRedacted())
	}
	if _, err := session.SendAndWait(client, "next", "next.cmd", sfs.NewSFSObject()); err != nil {
		t.Errorf("next command after check.device.change: %v", err)
	}
}

// TestSendCheckDeviceChangeSendFailure: a failed send is reported as a non-timeout error.
func TestSendCheckDeviceChangeSendFailure(t *testing.T) {
	client, _ := session.NewPipeGameConnPair(t)
	_ = client.Close()
	err := SendCheckDeviceChange(client)
	var stage session.SendStageError
	if !errors.As(err, &stage) {
		t.Errorf("err = %v, want a session.SendStageError", err)
	}
}
