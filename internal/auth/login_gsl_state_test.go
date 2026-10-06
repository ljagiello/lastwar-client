package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestResVersionFor(t *testing.T) {
	liveAndroid := &gsl.CheckVersionResponse{
		HotUpdateMsg: "GameRes,2343,1,1;DllRes,1502,1,1", LwFile2: "826|1",
		TableVersion: "39516,1,x", Locale: "23570,en",
	}

	t.Run("android reuses the reply it is given", func(t *testing.T) {
		f := newFakeGSL(t, liveCheckVersionTables, gsl.LoginServerListRespon{})
		testutil.UseFakeGSLServer(t, f.Server)
		if got := ResVersionFor(gsl.DefaultHTTPClient(), liveAndroid, false, "", "", "APS783"); got != "2343.1502_826F_39516.23570" {
			t.Errorf("ResVersionFor = %q", got)
		}
		if n := len(f.checkVersionQueries()); n != 0 {
			t.Errorf("check-version calls = %d, want 0", n)
		}
	})

	iosCases := []struct {
		name                   string
		appVersion, code       string
		wantAppVersion, wantID string
	}{
		{"ios default build", "", "", "1.0.344", "786"},
		{"ios build from the session config", "1.0.350", "801", "1.0.350", "801"},
	}
	for _, c := range iosCases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeGSL(t, liveCheckVersionTables, gsl.LoginServerListRespon{})
			testutil.UseFakeGSLServer(t, f.Server)
			if got := ResVersionFor(gsl.DefaultHTTPClient(), liveAndroid, true, c.appVersion, c.code, "APS783"); got != liveResVersion {
				t.Errorf("ResVersionFor = %q, want %q (from the iOS reply, not the Android one)", got, liveResVersion)
			}
			qs := f.checkVersionQueries()
			if len(qs) != 1 {
				t.Fatalf("check-version calls = %d, want 1", len(qs))
			}
			want := url.Values{
				"packageName": {"com.lastwar.ios"}, "platform": {"iOS"}, "appVersion": {c.wantAppVersion},
				"buildId": {c.wantID}, "server": {"APS783"}, "table_env": {"table_online"},
				"gm": {"0"}, "uid": {""}, "deviceId": {""}, "returnJson": {"1"}, "unityVersion": {"440"},
			}
			if qs[0].Encode() != want.Encode() {
				t.Errorf("iOS check-version query =\n %s\nwant\n %s", qs[0].Encode(), want.Encode())
			}
		})
	}

	fallbacks := []struct {
		name    string
		ios     bool
		cv      *gsl.CheckVersionResponse
		hosts   func(t *testing.T) []string
		wantLog string
	}{
		{"android without a reply", false, nil, nil, "lacks GameRes"},
		// The Android check-version already failed, so the iOS one is not tried: CheckVersionHosts
		// is left unreachable, and a call would add a host-failure Warn to the count below.
		{"ios without an Android reply", true, nil, func(*testing.T) []string { return []string{"http://127.0.0.1:1"} }, "skipping the iOS check-version"},
		{"ios check-version fails", true, liveAndroid, func(*testing.T) []string { return []string{"http://127.0.0.1:1"} }, "iOS check-version failed"},
		{"ios reply lacks table_version", true, liveAndroid, func(t *testing.T) []string {
			return []string{newFakeGSL(t, map[string]any{"hotUpdateMsg": "GameRes,1,1,1;DllRes,2,1,1", "lwfile2": "3|1", "locale": "4,en"}, gsl.LoginServerListRespon{}).URL}
		}, "lacks GameRes"},
	}
	for _, c := range fallbacks {
		t.Run(c.name, func(t *testing.T) {
			if c.hosts != nil {
				orig := gsl.CheckVersionHosts
				gsl.CheckVersionHosts = c.hosts(t)
				t.Cleanup(func() { gsl.CheckVersionHosts = orig })
			}
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			got := ResVersionFor(gsl.DefaultHTTPClient(), c.cv, c.ios, "", "", "APS783")
			slog.SetDefault(orig)
			if got != "" {
				t.Errorf("ResVersionFor = %q, want \"\" (BuildLoginParams then sends \"0\")", got)
			}
			// One resVersion Warn; gsl.CheckVersionAs adds its own per-host failure trail.
			if n := strings.Count(buf.String(), `level=WARN msg="resVersion:`); n != 1 || !strings.Contains(buf.String(), c.wantLog) {
				t.Errorf("want exactly one resVersion Warn mentioning %q, got:\n%s", c.wantLog, buf.String())
			}
			if c.ios && c.cv == nil && strings.Contains(buf.String(), "host failed") {
				t.Errorf("the iOS check-version was attempted without an Android reply:\n%s", buf.String())
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
		p := BuildLoginParams(LoginParamsInput{IOSMode: true, ServerID: "783", ResVersion: c.in})
		if got := p.GetString("resVersion"); got != c.wantRes {
			t.Errorf("ResVersion %q: resVersion = %q, want %q", c.in, got, c.wantRes)
		}
		if ta := p.GetString("ta"); !strings.Contains(ta, `"lw_res_version":"`+c.wantTa+`"`) {
			t.Errorf("ResVersion %q: ta lacks lw_res_version %q: %s", c.in, c.wantTa, ta)
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
	if keys := msg.Params.Keys(); len(keys) != 1 || keys[0] != "_id" || msg.Params.GetInt("_id") != checkDeviceChangeID {
		t.Errorf("params = %s, want only _id=%d", msg.Params.StringRedacted(), checkDeviceChangeID)
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
