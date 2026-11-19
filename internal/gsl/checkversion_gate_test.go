package gsl

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"lastwar-client/internal/crypto"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

// onlyHostTransport answers requests to host through next and refuses every other host, so a test
// that leaves the built-in gate lists in place can never reach a real gate server.
type onlyHostTransport struct {
	host string
	next http.RoundTripper
}

func (t onlyHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != t.host {
		return nil, fmt.Errorf("test transport: refusing %s (only %s is allowed)", r.URL.Host, t.host)
	}
	return t.next.RoundTrip(r)
}

// TestGrayPercent checks GrayUtils.GetPercentageFromMd5 (Assembly-CSharp.decompiled.cs:36030-36035)
// against values computed independently (Python: struct.unpack('<Q', md5(s)[:8])[0] % 100 + 1).
func TestGrayPercent(t *testing.T) {
	for in, want := range map[string]int{
		"0123456789ABCDEF0123456789ABCDEF_n3d": 97,
		"dev":                                  64,
		"test-device":                          32,
		"device-12_n3d":                        1,
		"device-21_n3d":                        4,
	} {
		if got := GrayPercent(in); got != want {
			t.Errorf("GrayPercent(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestCheckVersionHostsFor covers NetworkURLConfig.GetHostListByCurGroupType
// (Assembly-CSharp.decompiled.cs:14131-14141): a device in the 1-5 bucket asks the early-access
// hosts, any other the online list, and a replaced CheckVersionHosts is always kept.
func TestCheckVersionHostsFor(t *testing.T) {
	if !slices.Equal(CheckVersionHosts, onlineCheckVersionHosts) || !slices.Equal(EACheckVersionHosts, onlineEACheckVersionHosts) {
		t.Fatal("the built-in gate lists were replaced before this test")
	}
	cases := []struct {
		deviceID string
		wantEA   bool
	}{
		{"device-21_n3d", true},                         // bucket 4
		{"device-12_n3d", true},                         // bucket 1
		{"0123456789ABCDEF0123456789ABCDEF_n3d", false}, // bucket 97
		{"", false},
	}
	for _, c := range cases {
		hosts, ea := checkVersionHostsFor(c.deviceID)
		want := onlineCheckVersionHosts
		if c.wantEA {
			want = onlineEACheckVersionHosts
		}
		if ea != c.wantEA || !slices.Equal(hosts, want) {
			t.Errorf("checkVersionHostsFor(%q) = %v, %v; want %v, %v", c.deviceID, hosts, ea, want, c.wantEA)
		}
	}

	orig := CheckVersionHosts
	CheckVersionHosts = []string{"http://replaced.invalid"}
	defer func() { CheckVersionHosts = orig }()
	if hosts, ea := checkVersionHostsFor("device-21_n3d"); ea || !slices.Equal(hosts, CheckVersionHosts) {
		t.Errorf("with CheckVersionHosts replaced, an early-access device got %v (ea=%v), want the replaced list", hosts, ea)
	}
}

// TestCheckVersionAsAsksEarlyAccessHosts runs check-version for an early-access device with the
// built-in online list left in place: the request must go to the early-access list only.
func TestCheckVersionAsAsksEarlyAccessHosts(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_ = json.NewEncoder(w).Encode(map[string]any{"resMsg": pub})
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	client := &http.Client{Transport: onlyHostTransport{host: u.Host, next: http.DefaultTransport}}

	origEA := EACheckVersionHosts
	EACheckVersionHosts = []string{server.URL}
	defer func() { EACheckVersionHosts = origEA }()

	_, host, err := CheckVersionAs(client, productionIOSBuild, "APS783", "device-21_n3d")
	if err != nil {
		t.Fatalf("CheckVersionAs: %v", err)
	}
	if host != server.URL || hits != 1 {
		t.Errorf("answered by %q after %d hits, want the early-access host %q once", host, hits, server.URL)
	}
}

// TestCheckVersionAsForcedUpdateStops: updateType 2 is tested before code and stops the run
// (Assembly-CSharp.decompiled.cs:9617-9639). No other host is asked, and the error wraps
// ErrForcedUpdate whatever else the reply says.
func TestCheckVersionAsForcedUpdateStops(t *testing.T) {
	for _, updateType := range []any{"2", 2, " 2"} {
		t.Run(fmt.Sprint(updateType), func(t *testing.T) {
			var secondHit bool
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"updateType": updateType, "code": 301, "downloadurl": "https://store.invalid/app"})
			}))
			defer first.Close()
			second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondHit = true
				_ = json.NewEncoder(w).Encode(map[string]any{"resMsg": "x"})
			}))
			defer second.Close()
			orig := CheckVersionHosts
			CheckVersionHosts = []string{first.URL, second.URL}
			defer func() { CheckVersionHosts = orig }()

			cv, host, err := CheckVersionAs(DefaultHTTPClient(), productionIOSBuild, "APS783", "dev")
			if !errors.Is(err, ErrForcedUpdate) {
				t.Fatalf("err = %v, want ErrForcedUpdate", err)
			}
			if cv == nil || host != first.URL || !ForcedUpdate(cv.UpdateType.String()) {
				t.Errorf("cv = %+v, host = %q; want the forced-update reply from %q", cv, host, first.URL)
			}
			if secondHit {
				t.Error("the second host was asked after a forced update")
			}
		})
	}
}

// TestCheckVersionAsUpdateTypesThatProceed: 0, 1 (optional update) and 3 go on as before.
func TestCheckVersionAsUpdateTypesThatProceed(t *testing.T) {
	pub := testRSAPubKeyDER(t)
	for _, updateType := range []string{"", "0", "1", "3", "12"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"updateType": updateType, "resMsg": pub})
		}))
		orig := CheckVersionHosts
		CheckVersionHosts = []string{server.URL}
		_, _, err := CheckVersionAs(DefaultHTTPClient(), productionIOSBuild, "APS783", "dev")
		CheckVersionHosts = orig
		server.Close()
		if err != nil {
			t.Errorf("updateType %q: err = %v, want nil", updateType, err)
		}
	}
}

func TestCheckVersionResponseNewPacketZstdAndShumeiSDKOn(t *testing.T) {
	var none *CheckVersionResponse
	if !none.NewPacketZstd() || !none.ShumeiSDKOn() {
		t.Error("without a reply, NewPacketZstd and ShumeiSDKOn must keep today's behaviour (true)")
	}
	switches := func(on ...int) *CheckVersionResponse {
		s := make([]byte, 0, 2*52)
		for i := range 52 {
			if i > 0 {
				s = append(s, '|')
			}
			if slices.Contains(on, i) {
				s = append(s, '1')
			} else {
				s = append(s, '0')
			}
		}
		return &CheckVersionResponse{ClientConfig: FlexString(s)}
	}
	cases := []struct {
		cv           *CheckVersionResponse
		zstd, shumei bool
	}{
		{switches(), true, false},
		{switches(25), true, true},
		{switches(4), false, false},
		{switches(5, 25), false, true},
		{&CheckVersionResponse{}, true, false},
	}
	for i, c := range cases {
		if got := c.cv.NewPacketZstd(); got != c.zstd {
			t.Errorf("case %d: NewPacketZstd = %v, want %v", i, got, c.zstd)
		}
		if got := c.cv.ShumeiSDKOn(); got != c.shumei {
			t.Errorf("case %d: ShumeiSDKOn = %v, want %v", i, got, c.shumei)
		}
	}
}

// TestGetServerListForDeviceFields: country, lang and simOp come from the device
// (GetServerListRequest, Assembly-CSharp.decompiled.cs:77433-77437); the zero device keeps the
// US/en/"" this client has always sent.
func TestGetServerListForDeviceFields(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	cases := []struct {
		dev  FormDevice
		want map[string]string
	}{
		{FormDevice{Platform: "iOS"}, map[string]string{"platform": "iOS", "country": "US", "lang": "en", "simOp": "", "isSimulator": "0"}},
		{FormDevice{Platform: "Android", Country: "PL", Lang: "pl", SimOp: "26006"},
			map[string]string{"platform": "Android", "country": "PL", "lang": "pl", "simOp": "26006", "isSimulator": "0"}},
	}
	for _, c := range cases {
		var form url.Values
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
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
			form, _ = url.ParseQuery(string(plain))
			_, _ = fmt.Fprint(w, `{"code":0,"serverList":[{"id":"1"}]}`)
		}))
		if _, err := GetServerListFor(DefaultHTTPClient(), server.URL, &priv.PublicKey, c.dev, "dev", GSLOpt{}, "APS1", "g"); err != nil {
			t.Fatalf("GetServerListFor: %v", err)
		}
		server.Close()
		for k, v := range c.want {
			if got := form.Get(k); got != v {
				t.Errorf("%+v: %s = %q, want %q", c.dev, k, got, v)
			}
		}
	}
}
