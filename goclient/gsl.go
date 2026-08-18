package main

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client build identity, pulled directly from the analyzed APK
// (jadx_out/resources/AndroidManifest.xml) so the server sees the same
// values a real 1.0.351 install would present.
const (
	packageName = "com.fun.lastwar.gp"
	appVersion  = "1.0.351"
	versionCode = "1835"
	platform    = "Android" // Versions.PlatformName / GameUtility.GetPlatformName() -- capitalized
	unityVer    = "440"
)

// onlineCheckVersionHostList, dossier §02.
var checkVersionHosts = []string{
	"https://lastwar-serverlist-cf.lastwarapp.net",
	"https://lastwar-serverlist-us-aws-ali.lastwargame.com",
	"https://lastwar-serverlist-us-gcp-ali.lastwargame.com",
}

type CheckVersionResponse struct {
	Code         flexString `json:"code"`
	Msg          string     `json:"msg"`
	UpdateType   flexString `json:"updateType"`
	DownloadURL  string     `json:"downloadurl"`
	ResMsg       string     `json:"resMsg"`
	HotUpdateMsg string     `json:"hotUpdateMsg"`
}

// flexString accepts a JSON field that the server sometimes encodes as a
// string and sometimes as a bare number (observed live: `code` on error
// responses is a JSON number, e.g. 301).
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	*f = flexString(s)
	return nil
}
func (f flexString) String() string { return string(f) }

// CheckVersion races the known gate hosts and returns the first successful
// response along with which host answered (that host becomes the base URL
// for every subsequent GSL call -- dossier §02.1).
func CheckVersion(httpClient *http.Client) (*CheckVersionResponse, string, error) {
	q := url.Values{}
	q.Set("packageName", packageName)
	q.Set("platform", platform)
	q.Set("appVersion", appVersion)
	q.Set("gm", "0")
	q.Set("server", "")
	q.Set("uid", "")
	q.Set("deviceId", "")
	q.Set("table_env", "")
	q.Set("buildId", versionCode)
	q.Set("returnJson", "1")
	q.Set("unityVersion", unityVer)

	var lastErr error
	for _, host := range checkVersionHosts {
		u := host + "/gameservice/getlsu3dversion.php?" + q.Encode()
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			continue
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: HTTP %d: %s", host, resp.StatusCode, string(body))
			continue
		}
		var cv CheckVersionResponse
		if err := json.Unmarshal(body, &cv); err != nil {
			lastErr = fmt.Errorf("%s: decode JSON: %w (body=%s)", host, err, string(body))
			continue
		}
		if cv.Code != "" {
			lastErr = fmt.Errorf("%s: server returned code=%s msg=%s", host, cv.Code, cv.Msg)
			continue
		}
		return &cv, host, nil
	}
	return nil, "", fmt.Errorf("all check-version hosts failed, last error: %w", lastErr)
}

// LoginToken mirrors the {token,time} shape seen for `at`/`rt`.
type LoginToken struct {
	Token string `json:"token"`
	Time  int64  `json:"time"`
}

type LoginServerInfo struct {
	ID      int        `json:"id"`
	Name    string     `json:"name"`
	IP      string     `json:"ip"` // "|"-delimited fallback hostnames, not a single IP
	WsIP    string     `json:"ws_ip"`
	Port    int        `json:"port"`
	Zone    string     `json:"zone"`
	GameUid string     `json:"gameUid"`
	Uid     string     `json:"uid"`
	Status  flexString `json:"status"` // observed as a JSON string, e.g. "0"
}

// AccountServerInfo is the account/login-service endpoint (distinct from a
// specific game-state server) -- used for the very first connection when no
// account/state is associated with this device yet (opt=new).
type AccountServerInfo struct {
	IP     string `json:"ip"` // "|"-delimited fallback hostnames
	Port   int    `json:"port"`
	WsIP   string `json:"ws_ip"`
	WsPort int    `json:"ws_port"`
}

type LoginServerListRespon struct {
	Code             int                `json:"code"`
	ServerList       []LoginServerInfo  `json:"serverList"`
	LoginServer      *AccountServerInfo `json:"loginServer"`
	LastLoggedServer flexString         `json:"lastLoggedServer"`
	Bin              string             `json:"bin"`
	At               *LoginToken        `json:"at"`
	Rt               *LoginToken        `json:"rt"`
}

// firstHost returns the first entry of a "|"-delimited fallback host list.
func firstHost(pipeList string) string {
	first, _, _ := strings.Cut(pipeList, "|")
	return first
}

// findServerInfo locates a Login response's `serverInfo` shard-redirect
// object, wherever it actually is. Confirmed live: it's nested one level
// down, under `p` (`{p: {eu_state, serverInfo: {ip, port, zone, ...}}, rs,
// zn, un, pi, rl, id}`), not a top-level field of the response as
// LoginMessage.CSHandleResponse's decompiled call site alone would
// suggest -- both login.go and crossserver.go originally checked the top
// level only, which meant a real serverInfo redirect (observed live: a
// server merge moving an account from one zone/host/port to a completely
// different one) was silently never detected. The top-level check is
// kept as a fallback in case a different response shape ever puts it
// there instead.
func findServerInfo(content *SFSObject) *SFSObject {
	if content == nil {
		return nil
	}
	if v, ok := content.Get("serverInfo"); ok {
		if obj, ok := v.Val.(*SFSObject); ok {
			return obj
		}
	}
	if pv, ok := content.Get("p"); ok {
		if pObj, ok := pv.Val.(*SFSObject); ok {
			if v, ok := pObj.Get("serverInfo"); ok {
				if obj, ok := v.Val.(*SFSObject); ok {
					return obj
				}
			}
		}
	}
	return nil
}

// getIntFlexible reads a field that's usually an SFS numeric type but,
// confirmed live on serverInfo's `port`, is sometimes a UTF string
// instead (the response's other numeric-looking fields, like `id`, come
// through as real numbers -- this one specifically doesn't). Falls back
// to parsing the string form so a redirect doesn't silently resolve to
// port 0 depending on which type the server happened to send this time.
func getIntFlexible(o *SFSObject, key string) int32 {
	if n := o.GetInt(key); n != 0 {
		return n
	}
	if s := o.GetString(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			return int32(n)
		}
	}
	return 0
}

// GSLOpt selects which `opt` value to send, per dossier §02.2 / §05.
type GSLOpt struct {
	Opt      string // "new" | "login" | "fix" | "refresh" | ""
	LoginKey string
	Rt       string
}

// GetServerList performs the RSA+AES-wrapped GSL POST and returns the
// decrypted, parsed response.
func GetServerList(httpClient *http.Client, gateHost string, pub *rsa.PublicKey, deviceID string, opt GSLOpt, zone, gameUid string) (*LoginServerListRespon, error) {
	gc := NewGSLCrypto(pub)

	airKey := "lwDid_" + b64OfString(deviceID)

	form := url.Values{}
	form.Set("uuid", deviceID)
	form.Set("airKey", airKey)
	form.Set("loginFlag", "1")
	form.Set("country", "US")
	form.Set("is3D", "1")
	form.Set("lang", "en")
	form.Set("simOp", "")
	form.Set("platform", platform)
	form.Set("isSimulator", "0")
	form.Set("zone", zone)
	form.Set("gameuid", gameUid)
	form.Set("newServer", "1")
	form.Set("openCountry", "US")
	switch opt.Opt {
	case "new":
		form.Set("opt", "new")
	case "login":
		form.Set("opt", "login")
		form.Set("loginKey", opt.LoginKey)
	case "fix":
		form.Set("opt", "fix")
	case "refresh":
		form.Set("opt", "refresh")
		form.Set("rt", opt.Rt)
	}

	plainForm := encodeFormSorted(form)

	uuidField, dataField, err := gc.EncryptRequest(plainForm)
	if err != nil {
		return nil, fmt.Errorf("encrypt GSL request: %w", err)
	}

	postBody := url.Values{}
	postBody.Set("uuid", uuidField)
	postBody.Set("data", dataField)

	reqURL := gateHost + "/gameservice/getserverlist.php"
	req, err := http.NewRequest(http.MethodPost, reqURL, strings.NewReader(postBody.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("getserverlist.php: HTTP %d: %s", resp.StatusCode, string(body))
	}

	// The top-level response may itself be the plaintext respon, or may
	// wrap the real payload (AES-encrypted) inside a `bin` field.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, fmt.Errorf("decode top-level GSL response: %w (body=%s)", err, string(body))
	}

	var lsr LoginServerListRespon
	if binRaw, ok := top["bin"]; ok {
		var binStr string
		if err := json.Unmarshal(binRaw, &binStr); err != nil {
			return nil, fmt.Errorf("decode bin field: %w", err)
		}
		if binStr != "" {
			plain, err := gc.DecryptResponse(binStr)
			if err != nil {
				return nil, fmt.Errorf("decrypt GSL response: %w", err)
			}
			if err := json.Unmarshal([]byte(plain), &lsr); err != nil {
				return nil, fmt.Errorf("decode decrypted GSL response: %w (plain=%s)", err, plain)
			}
			return &lsr, nil
		}
	}
	if err := json.Unmarshal(body, &lsr); err != nil {
		return nil, fmt.Errorf("decode plaintext GSL response: %w (body=%s)", err, string(body))
	}
	return &lsr, nil
}

// encodeFormSorted joins form fields as k1=v1&k2=v2&... in a stable
// (insertion-independent) order. Field order does not affect the crypto
// (ECB has no cross-block dependency) but matching the reference client's
// order is good hygiene -- see dossier §03.
func encodeFormSorted(form url.Values) string {
	order := []string{"uuid", "airKey", "loginFlag", "country", "is3D", "lang", "simOp", "platform",
		"isSimulator", "zone", "gameuid", "newServer", "openCountry", "opt", "loginKey", "rt"}
	var b strings.Builder
	first := true
	for _, k := range order {
		v, ok := form[k]
		if !ok || len(v) == 0 {
			continue
		}
		if !first {
			b.WriteByte('&')
		}
		first = false
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v[0])
	}
	return b.String()
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// b64OfString matches DeviceManager.GetDeviceUid_Transcoding's airKey
// construction, which uses PLAIN standard base64 (not URL-safe).
func b64OfString(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
