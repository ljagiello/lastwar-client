package gsl

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"lastwar-client/internal/crypto"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"
)

// Live check-version values (iOS 1.0.344/786, table_env=table_online; coverage reports capture.md
// §1.4 and tables.md (b)1), shortened where a field only matters up to its first separator.
const (
	liveHotUpdateMsg = "GameRes,2159,123,456;DataTable,1166,294,1491981336;Lua,1543,260,1;Download,1006,1,1;PackageRes,1004,1,1;DllRes,1505,789,1011;"
	liveLwFile2      = "821|1000|2000|3000|4000;820|10|20;819|10|20"
	liveTableVersion = "39516,19518361,f1d9e6a3a8ec1610607583645e3f8377,3418147826;39500|48261|1;39487|477907|2"
	liveLocale       = "23570,pl,ja,en,ru,fr,tr,ar,vi,vr,th,gn_CN,ko,es,zh_CN,id,pt,it,de,zh_TW"
	liveResVersion   = "2159.1505_821F_39516.23570"
)

func TestCheckVersionQuery(t *testing.T) {
	ios := ClientBuild{PackageName: "com.lastwar.ios", Platform: "iOS", AppVersion: "1.0.344", VersionCode: "786"}
	cases := []struct {
		name  string
		build ClientBuild
		zone  string
		want  map[string]string
	}{
		{"android, zone unknown", AndroidBuild, "", map[string]string{
			"packageName": PackageName, "platform": "Android", "appVersion": AppVersion, "buildId": VersionCode, "server": ""}},
		{"ios, session zone", ios, "APS783", map[string]string{
			"packageName": "com.lastwar.ios", "platform": "iOS", "appVersion": "1.0.344", "buildId": "786", "server": "APS783"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q := checkVersionQuery(c.build, c.zone)
			want := map[string]string{"gm": "0", "uid": "", "deviceId": "", "table_env": "table_online", "returnJson": "1", "unityVersion": "440"}
			for k, v := range c.want {
				want[k] = v
			}
			if len(q) != len(want) {
				t.Errorf("query has keys %v, want exactly %d keys", slices.Sorted(maps.Keys(q)), len(want))
			}
			for k, v := range want {
				if got := q.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
	}
}

// TestCheckVersionSendsTableEnvAndZone checks the request on the wire and that the reply's table
// fields are decoded.
func TestCheckVersionSendsTableEnvAndZone(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	var got url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"resMsg": pub, "updateType": "3", "hotUpdateMsg": liveHotUpdateMsg, "lwfile2": liveLwFile2,
			"table_version": liveTableVersion, "locale": liveLocale, "client_config": "0|0|0|0|1",
			"checkok": 1, "warmup": "w", "firstLaunchForceUpdateMsg": "GameRes,2103;DllRes,1441;lwfile2,738",
		})
	}))
	defer server.Close()
	origHosts := CheckVersionHosts
	CheckVersionHosts = []string{server.URL}
	defer func() { CheckVersionHosts = origHosts }()

	cv, _, err := CheckVersion(DefaultHTTPClient(), "APS783")
	if err != nil {
		t.Fatalf("CheckVersion: %v", err)
	}
	if got.Get("table_env") != "table_online" || got.Get("server") != "APS783" || got.Get("deviceId") != "" || got.Get("uid") != "" {
		t.Errorf("query = %v, want table_env=table_online server=APS783 and empty uid/deviceId", got)
	}
	if cv.TableVersion.String() != liveTableVersion || cv.Locale.String() != liveLocale || cv.LwFile2.String() != liveLwFile2 {
		t.Errorf("table fields not decoded: table_version=%q locale=%q lwfile2=%q", cv.TableVersion, cv.Locale, cv.LwFile2)
	}
	if cv.CheckOK != "1" || cv.Warmup != "w" || cv.FirstLaunchForceUpdateMsg == "" || !cv.ClientSwitchOn(4) {
		t.Errorf("other fields not decoded: checkok=%q warmup=%q firstLaunch=%q client_config=%q", cv.CheckOK, cv.Warmup, cv.FirstLaunchForceUpdateMsg, cv.ClientConfig)
	}
	if rv := cv.ResVersion(); rv != liveResVersion {
		t.Errorf("ResVersion() = %q, want %q", rv, liveResVersion)
	}
}

func TestCheckVersionResponseResVersion(t *testing.T) {
	live := func() *CheckVersionResponse {
		return &CheckVersionResponse{HotUpdateMsg: liveHotUpdateMsg, LwFile2: liveLwFile2, TableVersion: liveTableVersion, Locale: liveLocale}
	}
	cases := []struct {
		name   string
		mutate func(cv *CheckVersionResponse)
		want   string
	}{
		{"live sample", func(*CheckVersionResponse) {}, liveResVersion},
		{"manifest names in any case and order", func(cv *CheckVersionResponse) {
			cv.HotUpdateMsg = "dllres,1505,1,1;GAMERES,2159,1,1"
		}, liveResVersion},
		{"no table_version (table_env empty)", func(cv *CheckVersionResponse) { cv.TableVersion = "" }, ""},
		{"no locale", func(cv *CheckVersionResponse) { cv.Locale = "" }, ""},
		{"no lwfile2", func(cv *CheckVersionResponse) { cv.LwFile2 = "" }, ""},
		{"no DllRes entry", func(cv *CheckVersionResponse) { cv.HotUpdateMsg = "GameRes,2159,1,1" }, ""},
		{"no GameRes entry", func(cv *CheckVersionResponse) { cv.HotUpdateMsg = "DllRes,1505,1,1" }, ""},
		{"non-numeric part", func(cv *CheckVersionResponse) { cv.Locale = "DevLocale,en" }, ""},
		{"overlong part", func(cv *CheckVersionResponse) { cv.TableVersion = "1234567890,1,x" }, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cv := live()
			c.mutate(cv)
			if got := cv.ResVersion(); got != c.want {
				t.Errorf("ResVersion() = %q, want %q", got, c.want)
			}
		})
	}
	var nilCV *CheckVersionResponse
	if got := nilCV.ResVersion(); got != "" {
		t.Errorf("nil ResVersion() = %q, want \"\"", got)
	}
}

func TestCheckVersionResponseClientSwitchOn(t *testing.T) {
	cv := &CheckVersionResponse{ClientConfig: "1|0| 1 |0"}
	for id, want := range map[int]bool{-1: false, 0: true, 1: false, 2: true, 3: false, 4: false, 44: false} {
		if got := cv.ClientSwitchOn(id); got != want {
			t.Errorf("ClientSwitchOn(%d) = %v, want %v", id, got, want)
		}
	}
}

func TestLoginServerListResponErr(t *testing.T) {
	one := []LoginServerInfo{{ID: "1"}}
	cases := []struct {
		name     string
		code     FlexString
		list     []LoginServerInfo
		want     error // nil means success
		reauth   bool
		wantCode string
	}{
		{"code 0", "0", one, nil, false, ""},
		{"code absent reads as 0", "", one, nil, false, ""},
		{"code 0, empty list is E120", "0", nil, ErrEmptyServerList, false, `code="0"`},
		{"201 not available", "201", one, ErrNotAvailable, false, `code="201"`},
		{"211 credentials invalid", "211", nil, ErrCredentialsInvalid, true, `code="211"`},
		{"212 mapping invalid", "212", one, ErrAccountMappingInvalid, true, `code="212"`},
		{"213 envelope crypto", "213", nil, ErrEnvelopeCrypto, false, `code="213"`},
		{"other code is generic E116", "301", one, ErrRejected, false, `code="301"`},
		{"overlong code is truncated", FlexString(strings.Repeat("9", 40)), nil, ErrRejected, false, `code="9999999999999999..."`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := (&LoginServerListRespon{Code: c.code, ServerList: c.list}).Err()
			if c.want == nil {
				if err != nil {
					t.Fatalf("Err() = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, c.want) {
				t.Fatalf("Err() = %v, want it to wrap %v", err, c.want)
			}
			if got := errors.Is(err, ErrReauthNeeded); got != c.reauth {
				t.Errorf("errors.Is(err, ErrReauthNeeded) = %v, want %v", got, c.reauth)
			}
			if !strings.Contains(err.Error(), c.wantCode) {
				t.Errorf("Err() = %q, want it to name %s", err, c.wantCode)
			}
		})
	}

	// A numeric JSON code decodes to the same result as a string one.
	var lsr LoginServerListRespon
	if err := json.Unmarshal([]byte(`{"code":211,"serverList":[]}`), &lsr); err != nil {
		t.Fatal(err)
	}
	if err := lsr.Err(); !errors.Is(err, ErrCredentialsInvalid) {
		t.Errorf("numeric code 211: Err() = %v, want ErrCredentialsInvalid", err)
	}
}

func TestLoginServerListResponPickServer(t *testing.T) {
	list := []LoginServerInfo{
		{ID: "72", Zone: "APS72", GameUid: "uid-other"},
		{ID: "783", Zone: "APS783", GameUid: "uid-mine"},
		{ID: "900", Zone: "APS900", GameUid: ""},
	}
	cases := []struct {
		name, gameUid, lastLogged, wantZone string
	}{
		{"gameUid match wins over lastLoggedServer", "uid-mine", "72", "APS783"},
		{"lastLoggedServer when no gameUid match", "uid-unknown", "900", "APS900"},
		{"lastLoggedServer when gameUid unknown", "", "783", "APS783"},
		{"first entry when nothing matches", "uid-unknown", "1234", "APS72"},
		{"lastLoggedServer 0 means none", "", "0", "APS72"},
		{"empty gameUid never matches an entry without one", "", "", "APS72"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lsr := &LoginServerListRespon{ServerList: list, LastLoggedServer: FlexString(c.lastLogged)}
			if got := lsr.PickServer(c.gameUid); got == nil || got.Zone.String() != c.wantZone {
				t.Errorf("PickServer(%q) = %+v, want zone %s", c.gameUid, got, c.wantZone)
			}
		})
	}
	if got := (&LoginServerListRespon{}).PickServer("x"); got != nil {
		t.Errorf("PickServer on an empty list = %+v, want nil", got)
	}
}

func TestChooseOpt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	thirtyDays := int64(30 * 24 * 60 * 60)
	cases := []struct {
		name string
		s    OptState
		want GSLOpt
	}{
		{"loginKey wins over everything", OptState{LoginKey: "lk", GameUid: "g", AccessTok: "at", RefreshTok: "rt"}, GSLOpt{Opt: "login", LoginKey: "lk"}},
		{"no gameUid is a new device", OptState{AccessTok: "at", RefreshTok: "rt"}, GSLOpt{Opt: "new"}},
		{"no rt", OptState{GameUid: "g", AccessTok: "at"}, GSLOpt{Opt: "fix"}},
		{"no rt and no at", OptState{GameUid: "g"}, GSLOpt{Opt: "fix"}},
		{"rt but no at", OptState{GameUid: "g", RefreshTok: "rt"}, GSLOpt{Opt: "refresh", Rt: "rt"}},
		{"at exactly 30 days old", OptState{GameUid: "g", AccessTok: "at", AccessTokTime: now.Unix() - thirtyDays, RefreshTok: "rt"}, GSLOpt{Opt: "refresh", Rt: "rt"}},
		{"at one second short of 30 days", OptState{GameUid: "g", AccessTok: "at", AccessTokTime: now.Unix() - thirtyDays + 1, RefreshTok: "rt"}, GSLOpt{}},
		{"fresh at and rt send no opt", OptState{GameUid: "g", AccessTok: "at", AccessTokTime: now.Unix() - 60, RefreshTok: "rt"}, GSLOpt{}},
		{"unknown at.time is never stale", OptState{GameUid: "g", AccessTok: "at", RefreshTok: "rt"}, GSLOpt{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ChooseOpt(c.s, now); got != c.want {
				t.Errorf("ChooseOpt = {Opt:%q LoginKey:%q Rt:%q}, want {Opt:%q LoginKey:%q Rt:%q}",
					got.Opt, got.LoginKey, got.Rt, c.want.Opt, c.want.LoginKey, c.want.Rt)
			}
		})
	}
}

func TestOptStateRedacts(t *testing.T) {
	const secret = "MUST-NOT-LEAK-opt-state"
	s := OptState{LoginKey: secret, AccessTok: secret, RefreshTok: secret}
	for _, out := range []string{s.String(), s.GoString(), fmt.Sprintf("%+v", struct{ S OptState }{s}), s.LogValue().String()} {
		if strings.Contains(out, secret) {
			t.Errorf("redacted output %q contains the secret", out)
		}
	}
}

// TestGetServerListOptFields decrypts the request the way the server does and checks the opt
// fields ChooseOpt's outcomes produce, including "no opt" for a fresh at.
func TestGetServerListOptFields(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	cases := []struct {
		name    string
		opt     GSLOpt
		want    map[string]string
		without []string
	}{
		{"no opt", GSLOpt{}, nil, []string{"opt", "rt", "loginKey"}},
		{"refresh", GSLOpt{Opt: "refresh", Rt: "rt-1"}, map[string]string{"opt": "refresh", "rt": "rt-1"}, []string{"loginKey"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
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
				_, _ = fmt.Fprint(w, `{"code":0,"serverList":[{"id":"1"}]}`)
			}))
			defer server.Close()

			if _, err := GetServerList(DefaultHTTPClient(), server.URL, &priv.PublicKey, "dev", c.opt, "APS1", "g"); err != nil {
				t.Fatalf("GetServerList: %v", err)
			}
			if form == nil {
				t.Fatal("fake server never decoded a request")
			}
			for k, v := range c.want {
				if got := form.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for _, k := range c.without {
				if form.Has(k) {
					t.Errorf("request carries %s=%q, want no %s field", k, form.Get(k), k)
				}
			}
		})
	}
}
