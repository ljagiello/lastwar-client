package shumei

import (
	"bytes"
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDecodeCloudConfBuiltin runs the SDK's own built-in cloud config
// (l11l111l1lll.java's constant and key, extracted to testdata) through the
// decode path: it must give the md5 the first request sends and the gps/tel
// exclusions a fresh install reports.
func TestDecodeCloudConfBuiltin(t *testing.T) {
	raw, err := os.ReadFile("testdata/cloudconf_builtin.json")
	if err != nil {
		t.Fatal(err)
	}
	var builtin struct {
		Key    string          `json:"key"`
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(raw, &builtin); err != nil {
		t.Fatal(err)
	}
	key, err := base64.StdEncoding.DecodeString(builtin.Key)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := decodeCloudConf(builtin.Detail, key)
	if err != nil {
		t.Fatalf("decodeCloudConf(built-in) = %v", err)
	}
	if conf.MD5 != builtinCloudConfMD5 || !reflect.DeepEqual(conf.Excluded, builtinCloudConfExcluded) {
		t.Errorf("built-in config = md5 %s, excluded %v; want %s, %v", conf.MD5, conf.Excluded, builtinCloudConfMD5, builtinCloudConfExcluded)
	}
	if _, err := decodeCloudConf(builtin.Detail, make([]byte, 32)); err == nil {
		t.Error("decodeCloudConf with the wrong key = nil error, want an error")
	}
}

// TestExclusionSet pins a85's HashSet order. gps and protect share bucket 11
// of 16, so their order is insertion order: the cloud set's names go in
// first.
func TestExclusionSet(t *testing.T) {
	if got, want := exclusionSet(builtinCloudConfExcluded), []string{"input", "tel", "gps", "protect"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a85 for the built-in config = %v, want %v", got, want)
	}
	if got, want := exclusionSet(nil), []string{"input", "protect"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a85 for a config that turns nothing off = %v, want %v", got, want)
	}
	// A name notCollect already has appears once.
	if got := exclusionSet([]string{"input"}); !reflect.DeepEqual(got, []string{"input", "protect"}) {
		t.Errorf("a85 with input twice = %v, want it once", got)
	}
	// Same bucket: insertion order decides.
	if got := javaHashSetOrder([]string{"protect", "gps"}); !reflect.DeepEqual(got, []string{"protect", "gps"}) {
		t.Errorf("javaHashSetOrder(protect, gps) = %v, want insertion order within the bucket", got)
	}
	for s, want := range map[string]uint32{"": 0, "input": 0x5fb57ca, "protect": 0xed94d6cf, "\U0001F600": 0xd83d*31 + 0xde00} {
		if got := javaStringHashCode(s); got != want {
			t.Errorf("javaStringHashCode(%q) = %#x, want %#x", s, got, want)
		}
	}
	// 13 names pass the 0.75 load of 16, so the table is 32 wide.
	many := strings.Split("a,b,c,d,e,f,g,h,i,j,k,l,m", ",")
	if got := javaHashSetOrder(many); len(got) != 13 || got[0] != "a" {
		t.Errorf("javaHashSetOrder(13 letters) = %v", got)
	}
}

// TestDeviceDataExclusions checks a cached config's exclusions gate the slots
// the way the collector's checks of a85's set do (l111l1111llIl.java): by a
// group name or the slot's own key; one name of a coupled gate drops its whole
// group; a primitive slot goes out as its zero. a85 lists the names either way.
func TestDeviceDataExclusions(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef_n3d"
	withConfig := func(excluded string) map[string]any {
		t.Helper()
		d := testDevice()
		d.WiFi = &WiFi{SSID: "Home", BSSID: "aa:bb:cc:dd:ee:ff", IP: "192.168.1.50"}
		d.Install.CloudConfMD5 = strings.Repeat("ab", 16)
		d.Install.CloudConfExcluded = excluded
		return decodeDeviceData(t, mustDeviceData(t, id, d))
	}

	// A config that turns nothing off fills every slot these cases gate.
	all := withConfig("")
	gatedHere := []string{"a19", "a30", "a31", "a72", "a45", "a131", "a132", "a143", "a34", "a32", "a56", "a57", "a69", "a70", "a71", "a85"}
	for _, key := range gatedHere {
		if _, ok := all[key]; !ok {
			t.Fatalf("%s is absent with nothing turned off; the cases below need it filled", key)
		}
	}

	// Wi-Fi's ssid and bssid groups and a72's own key: a85 lists the three
	// names and the slots they gate are left out; wifiip isn't off, so a31 stays.
	m := withConfig("ssid,bssid,a72")
	for _, key := range []string{"a19", "a30", "a72"} {
		if v, ok := m[key]; ok {
			t.Errorf("%s = %v with ssid,bssid,a72 turned off, want it absent", key, v)
		}
	}
	if m["a31"] != "192.168.1.50" {
		t.Errorf("a31 = %v, want it kept: wifiip isn't turned off", m["a31"])
	}
	if want := []any{"input", "bssid", "protect", "ssid", "a72"}; !reflect.DeepEqual(m["a85"], want) {
		t.Errorf("a85 = %v, want %v", m["a85"], want)
	}

	for _, c := range []struct {
		excluded      string
		absent, kept  []string
		zero, nonzero []string
	}{
		// One key of a coupled gate turns off the whole group.
		{excluded: "a131", absent: []string{"a45", "a131", "a132"}},
		{excluded: "operator", absent: []string{"a45", "a131", "a132"}},
		{excluded: "a33", absent: []string{"a143", "a34"}, kept: []string{"a32"}},
		{excluded: "cpuinfo", absent: []string{"a143", "a34"}, kept: []string{"a32"}},
		{excluded: "a57", absent: []string{"a56", "a57"}},
		{excluded: "a70", absent: []string{"a69", "a70", "a71"}},
		// A group name inside the operator gate turns off only its own slot.
		{excluded: "simCountryISO", absent: []string{"a131"}, kept: []string{"a45", "a132"}},
		{excluded: "networkCountryIso", absent: []string{"a132"}, kept: []string{"a45", "a131"}},
		{excluded: "wifiip,network,sensor,launcherInfo,drmId,adid,virtualUid", absent: []string{"a31", "a44", "a47", "a154", "a84", "a24", "a60"}, kept: []string{"a19", "a30"}},
		// a85 is gated by its own key; a primitive goes out as its zero.
		{excluded: "a85,a170,a174", absent: []string{"a85"}, zero: []string{"a170", "a174"}, nonzero: []string{"a171"}},
	} {
		m := withConfig(c.excluded)
		for _, key := range c.absent {
			if v, ok := m[key]; ok {
				t.Errorf("%s off: %s = %v, want it absent", c.excluded, key, v)
			}
		}
		for _, key := range c.kept {
			if _, ok := m[key]; !ok {
				t.Errorf("%s off: %s is absent, want it kept", c.excluded, key)
			}
		}
		for _, key := range c.zero {
			if got := fmt.Sprint(m[key]); got != "0" {
				t.Errorf("%s off: %s = %s, want the primitive's 0", c.excluded, key, got)
			}
		}
		for _, key := range c.nonzero {
			if jsonInt(t, m[key]) == 0 {
				t.Errorf("%s off: %s = 0, want it filled", c.excluded, key)
			}
		}
	}

	// The built-in config's gps and tel gate no slot: only a85 differs.
	builtin := withConfig("gps,tel")
	delete(builtin, "a85")
	delete(all, "a85")
	if !reflect.DeepEqual(builtin, all) {
		t.Errorf("gps,tel off changed a slot besides a85:\n%v\nwant\n%v", builtin, all)
	}
}

// TestCloudConfRequest pins the request body: JSONObject's insertion order,
// the built-in config's md5 and an empty bb on a first start, and pri wrapping
// the reply key.
func TestCloudConfRequest(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{7}, 32)
	body, err := cloudConfRequest(&priv.PublicKey, testInstall, "1790882443713-36769", key)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Data struct {
			Pri string `json:"pri"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	want := `{"organization":"9dNCYkjHeFMYZMOjxgX8","data":{"os":"android","sdkver":"3.14.3","md5":"dcce7a4761131c32012fa53ccb09d77b","enc":1,"bb":"","sid":"1790882443713-36769","pri":"` + parsed.Data.Pri + `"}}`
	if string(body) != want {
		t.Errorf("cloudconf body =\n%s\nwant\n%s", body, want)
	}
	ct, err := base64.StdEncoding.DecodeString(parsed.Data.Pri)
	if err != nil {
		t.Fatalf("pri is not base64: %v", err)
	}
	if got, err := rsa.DecryptOAEP(sha256.New(), nil, priv, ct, nil); err != nil || !bytes.Equal(got, key) {
		t.Errorf("pri decrypts to % x (%v), want the reply key", got, err)
	}

	// A later start sends the smid it stored and the cached config's md5.
	later, _ := testInstall.Registered("Bsmid-9", time.UnixMilli(1789500000000))
	later.CloudConfMD5 = strings.Repeat("ab", 16)
	body, err = cloudConfRequest(&priv.PublicKey, later, "s", key)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"md5":"abababababababababababababababab","enc":1,"bb":"smid-9","sid":"s"`)) {
		t.Errorf("later cloudconf body = %s, want the cached md5 and bb smid-9", body)
	}
}

// encryptConf returns a cloudconf reply detail carrying config, encrypted as
// the server does for key: zlib, AES-256-CBC with the fixed IV, base64.
func encryptConf(t *testing.T, key []byte, ver int, config string) map[string]any {
	t.Helper()
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	_, _ = zw.Write([]byte(config))
	_ = zw.Close()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	padded := pkcs7Pad(z.Bytes(), aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, cloudConfIV).CryptBlocks(ct, padded)
	return map[string]any{"ver": ver, "enc": 1, "length": len(config), "data": base64.StdEncoding.EncodeToString(ct)}
}

// TestDecodeCloudConfForms checks both config forms: ver 1's flat switches
// (false, or anything optBoolean doesn't read as true, turns a name off) and
// the older nested object (true turns it off).
func TestDecodeCloudConfForms(t *testing.T) {
	key := bytes.Repeat([]byte{3}, 32)
	for _, c := range []struct {
		name   string
		ver    int
		config string
		want   []string
	}{
		{"flat", 1, `{"sensitive.wifi":false,"sensitive.tel":true,"other":false,"sensitive.gps":"no","sensitive.apps":"TRUE"}`, []string{"wifi", "gps"}},
		{"nested", 0, `{"sensitive":{"wifi":true,"tel":false,"gps":"true"},"sensitive.x":false}`, []string{"wifi", "gps"}},
		{"nested not an object", 0, `{"sensitive":[1]}`, nil},
	} {
		detail, _ := json.Marshal(encryptConf(t, key, c.ver, c.config))
		conf, err := decodeCloudConf(detail, key)
		if err != nil {
			t.Fatalf("%s: decodeCloudConf = %v", c.name, err)
		}
		sum := md5.Sum([]byte(c.config))
		if conf.MD5 != hex.EncodeToString(sum[:]) || !reflect.DeepEqual(conf.Excluded, c.want) {
			t.Errorf("%s: = md5 %s, excluded %v; want the text's md5 and %v", c.name, conf.MD5, conf.Excluded, c.want)
		}
	}
}

// cloudConfServer answers /v3/cloudconf with config encrypted under the key
// the request's pri wraps (or with status when it isn't 200), and
// /deviceprofile/v4 with a smid, recording each request.
type cloudConfServer struct {
	t      *testing.T
	priv   *rsa.PrivateKey
	config string
	status int
	paths  []string
	sid    string
	upload map[string]any
	// mangle, when set, edits the reply's detail before it is sent.
	mangle func(detail map[string]any)
}

func (s *cloudConfServer) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	s.paths = append(s.paths, req.URL.Path)
	reply := `{"code":1100,"detail":{"deviceId":"smid-new"},"requestId":"r-1"}`
	switch req.URL.Path {
	case "/v3/cloudconf":
		if req.Header.Get("Content-Type") != "application/octet-stream" {
			s.t.Errorf("cloudconf Content-Type = %q", req.Header.Get("Content-Type"))
		}
		if s.status != http.StatusOK {
			return &http.Response{StatusCode: s.status, Body: io.NopCloser(strings.NewReader("busy")), Header: http.Header{}}, nil
		}
		var r struct {
			Data struct {
				SID string `json:"sid"`
				Pri string `json:"pri"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &r); err != nil {
			s.t.Fatalf("cloudconf body: %v", err)
		}
		s.sid = r.Data.SID
		ct, _ := base64.StdEncoding.DecodeString(r.Data.Pri)
		key, err := rsa.DecryptOAEP(sha256.New(), nil, s.priv, ct, nil)
		if err != nil {
			s.t.Fatalf("pri: %v", err)
		}
		conf := encryptConf(s.t, key, 1, s.config)
		if s.mangle != nil {
			s.mangle(conf)
		}
		detail, _ := json.Marshal(conf)
		reply = fmt.Sprintf(`{"code":1100,"requestId":"r-c","detail":%s}`, detail)
	case "/deviceprofile/v4":
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		data, _ := p["data"].(string)
		ep, _ := p["ep"].(string)
		tn, _ := p["tn"].(string)
		s.upload = decodeDeviceData(s.t, unprotect(s.t, s.priv, data, ep, tn))
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(reply)), Header: http.Header{}}, nil
}

// TestRegisterCloudConf checks a start POSTs cloudconf before the device
// profile with a80's session ID, caches the reply's config in the install,
// and that the next start sends its md5 back and its exclusions in a85. A
// failed cloudconf leaves the cached config and the registration alone.
func TestRegisterCloudConf(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const config = `{"sensitive.gps":false,"sensitive.wifi":false,"sensitive.tel":true}`
	srv := &cloudConfServer{t: t, priv: priv, config: config, status: http.StatusOK}
	d := testDevice()
	id, in, err := register(&http.Client{Transport: srv}, &priv.PublicKey, testDeviceID, d, time.Now)
	if err != nil || id != "Bsmid-new" {
		t.Fatalf("register = %q, %v; want Bsmid-new", id, err)
	}
	if want := []string{"/v3/cloudconf", "/deviceprofile/v4"}; !reflect.DeepEqual(srv.paths, want) {
		t.Errorf("requests = %v, want %v", srv.paths, want)
	}
	if srv.sid != fmt.Sprint(srv.upload["a80"]) {
		t.Errorf("cloudconf sid %q, a80 %v; want the same session ID", srv.sid, srv.upload["a80"])
	}
	// This start collected under the config already in force: the built-in one.
	if got := fmt.Sprint(srv.upload["a85"]); got != "[input tel gps protect]" {
		t.Errorf("first a85 = %s, want the built-in config's", got)
	}
	sum := md5.Sum([]byte(config))
	if in.CloudConfMD5 != hex.EncodeToString(sum[:]) || in.CloudConfExcluded != "gps,wifi" {
		t.Errorf("cached config = %q, %q; want the reply's md5 and gps,wifi", in.CloudConfMD5, in.CloudConfExcluded)
	}
	if err := in.Validate(); err != nil {
		t.Errorf("install after register: Validate() = %v", err)
	}

	// The next start reports the cached config, and a failed cloudconf keeps
	// it without failing the upload.
	d.Install = in
	srv.paths, srv.status = nil, http.StatusServiceUnavailable
	id, next, err := register(&http.Client{Transport: srv}, &priv.PublicKey, testDeviceID, d, time.Now)
	if err != nil || id != "Bsmid-new" || len(srv.paths) != 2 {
		t.Fatalf("register after a failed cloudconf = %q, %v after %v; want Bsmid-new after both requests", id, err, srv.paths)
	}
	if want := fmt.Sprint(exclusionSet([]string{"gps", "wifi"})); fmt.Sprint(srv.upload["a85"]) != want {
		t.Errorf("second a85 = %v, want %s from the cached config", srv.upload["a85"], want)
	}
	if next.CloudConfMD5 != in.CloudConfMD5 || next.CloudConfExcluded != in.CloudConfExcluded {
		t.Errorf("after a failed cloudconf the cache = %q, %q; want it kept", next.CloudConfMD5, next.CloudConfExcluded)
	}
}

// TestSensitiveOffFollowsJavaSplit: the flat parser names a switch
// key.split("\\.")[1] and collects the names in a HashSet
// (l11l111l1lll.java:95-111). Java's split drops trailing empty strings, so
// "sensitive.wifi.scan" and "sensitive.wifi." both name wifi (once),
// "sensitive..x" names "", and "sensitive." throws on index 1 and is skipped.
func TestSensitiveOffFollowsJavaSplit(t *testing.T) {
	config := `{"sensitive.wifi.scan":false,"sensitive.":false,"sensitive.wifi.":false,"sensitive..x":false,"sensitive.gps":false}`
	got, err := sensitiveOff([]byte(config), true)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"wifi", "", "gps"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sensitiveOff = %q, want %q", got, want)
	}
	nested, err := sensitiveOff([]byte(`{"sensitive":{"tel":true,"tel":true}}`), false)
	if err != nil || !reflect.DeepEqual(nested, []string{"tel"}) {
		t.Errorf("nested sensitiveOff = %q, %v; want [tel] once", nested, err)
	}
}

// TestDecodeCloudConfReadsTheConfigLikeTheSDK: getInt("length") must succeed
// (l11l111l1lll.java:249; a numeric string counts, as org.json coerces it),
// and data goes through Base64.decode(..., DEFAULT) (l1l1l11Ill.java:649-655),
// which skips line breaks and takes unpadded input.
func TestDecodeCloudConfReadsTheConfigLikeTheSDK(t *testing.T) {
	key := bytes.Repeat([]byte{5}, 32)
	// A config whose ciphertext's base64 ends in padding, so the unpadded
	// case tests something.
	config := `{"sensitive.wifi":false}`
	for pad := ""; !strings.HasSuffix(encryptConf(t, key, 1, config)["data"].(string), "="); pad += "x" {
		config = `{"sensitive.wifi":false,"pad":"` + pad + `"}`
	}
	for _, c := range []struct {
		name   string
		mangle func(d map[string]any)
		ok     bool
	}{
		{"as sent", func(map[string]any) {}, true},
		{"no length", func(d map[string]any) { delete(d, "length") }, false},
		{"length a numeric string", func(d map[string]any) { d["length"] = "24" }, true},
		{"length not a number", func(d map[string]any) { d["length"] = "many" }, false},
		{"data wrapped every 76 characters", func(d map[string]any) {
			s := d["data"].(string)
			var b strings.Builder
			for len(s) > 76 {
				b.WriteString(s[:76] + "\n")
				s = s[76:]
			}
			b.WriteString(s + "\n")
			d["data"] = b.String()
		}, true},
		{"data without padding", func(d map[string]any) { d["data"] = strings.TrimRight(d["data"].(string), "=") }, true},
	} {
		d := encryptConf(t, key, 1, config)
		if !strings.HasSuffix(d["data"].(string), "=") {
			t.Fatal("the fixture's data must end in padding for the unpadded case to test anything")
		}
		c.mangle(d)
		detail, _ := json.Marshal(d)
		conf, err := decodeCloudConf(detail, key)
		if (err == nil) != c.ok {
			t.Errorf("%s: decodeCloudConf error = %v, want ok %v", c.name, err, c.ok)
			continue
		}
		if c.ok && !reflect.DeepEqual(conf.Excluded, []string{"wifi"}) {
			t.Errorf("%s: excluded %v, want [wifi]", c.name, conf.Excluded)
		}
	}
}

// TestRegisterUndecodableCloudConfFallsBackToTheBuiltIn: a reply whose detail
// carries "data" goes to the SDK's success handler even when it does not
// decode; that leaves the config in force null and saves the reply, so the
// built-in config is in force from then on (l11l111l1lll.java:30-39,
// 296-309): the cache is cleared, and the next start sends the built-in md5
// and exclusions. A detail without "data" is the error path (empty handler,
// :79-86): the cache stays.
func TestRegisterUndecodableCloudConfFallsBackToTheBuiltIn(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	cached := testInstall
	cached.CloudConfMD5, cached.CloudConfExcluded = strings.Repeat("cd", 16), "wifi"
	for _, c := range []struct {
		name      string
		mangle    func(d map[string]any)
		wantCache [2]string
	}{
		{"data that does not decode", func(d map[string]any) { delete(d, "length") }, [2]string{"", ""}},
		{"data under the wrong key", func(d map[string]any) {
			d["data"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
		}, [2]string{"", ""}},
		{"no data", func(d map[string]any) { delete(d, "data") }, [2]string{cached.CloudConfMD5, "wifi"}},
	} {
		srv := &cloudConfServer{t: t, priv: priv, config: `{"sensitive.tel":false}`, status: http.StatusOK, mangle: c.mangle}
		d := testDevice()
		d.Install = cached
		id, in, err := register(&http.Client{Transport: srv}, &priv.PublicKey, testDeviceID, d, time.Now)
		if err != nil || id != "Bsmid-new" {
			t.Fatalf("%s: register = %q, %v; want Bsmid-new (a cloudconf problem never fails the upload)", c.name, id, err)
		}
		if got := [2]string{in.CloudConfMD5, in.CloudConfExcluded}; got != c.wantCache {
			t.Errorf("%s: cache = %q, want %q", c.name, got, c.wantCache)
		}
		if err := in.Validate(); err != nil {
			t.Errorf("%s: install Validate() = %v", c.name, err)
		}
		if c.wantCache[0] == "" {
			body, err := cloudConfRequest(&priv.PublicKey, in, "s", bytes.Repeat([]byte{1}, 32))
			if err != nil || !bytes.Contains(body, []byte(`"md5":"dcce7a4761131c32012fa53ccb09d77b"`)) {
				t.Errorf("%s: next cloudconf body %s (%v), want the built-in md5", c.name, body, err)
			}
			if got := in.cloudExcluded(); !reflect.DeepEqual(got, []string{"gps", "tel"}) {
				t.Errorf("%s: exclusions in force %v, want the built-in gps, tel", c.name, got)
			}
		}
	}
}
