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

// maxGSLResponseSize bounds the HTTP responses read via io.ReadAll below
// (CheckVersion, GetServerList). Reading an untrusted HTTP body without a
// cap is the same trivial multi-GB OOM vector packet.go's maxFrameSize
// guards against on the TCP side, just tighter: these are small JSON/text
// config responses, never expected to exceed a few KB.
const maxGSLResponseSize = 1 << 20 // 1 MiB

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
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = flexString(s)
		return nil
	}
	// Not a JSON string (e.g. a bare number like 301) -- use the raw bytes as-is.
	*f = flexString(b)
	return nil
}
func (f flexString) String() string { return string(f) }

// CheckVersion tries the known gate hosts in order (NOT concurrently, despite earlier wording --
// this is a plain sequential fallback: each host gets the full httpClient timeout before moving
// to the next) and returns the first successful response along with which host answered (that
// host becomes the base URL for every subsequent GSL call -- dossier §02.1).
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
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxGSLResponseSize+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if len(body) > maxGSLResponseSize {
			lastErr = fmt.Errorf("%s: response body exceeds %d byte limit", host, maxGSLResponseSize)
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
	// Code is logged on every call site (see login.go and main.go's "GSL getserverlist
	// response"/"GSL refresh response" log lines) but, unlike CheckVersionResponse.Code (checked
	// against "" in CheckVersion above), it is NOT checked for a rejection value here: this
	// endpoint's own success-vs-rejection code values haven't been confirmed live yet -- no
	// captured getserverlist.php response with a real rejection has been observed, and this
	// project's own history has twice been burned by guessing at unconfirmed server behavior
	// instead of waiting for evidence. Left deliberately open rather than guessed at, mirroring
	// alliance.go's honestly-left-open donation-cooldown gap (see
	// DonateRecommendedAllianceTech's doc comment) -- a future round should add a check here once
	// a real rejection response for this specific endpoint has actually been captured.
	//
	// Code is flexString, not a bare int, matching CheckVersionResponse.Code and
	// LoginServerInfo.Status: this project has confirmed live that CheckVersionResponse.Code (a
	// sibling endpoint's own `code` field) comes back as either a JSON string or a bare number
	// depending on context (see flexString's doc comment). getserverlist.php's `code` hasn't
	// itself been observed doing this yet, but if it ever does, a bare int here would make
	// json.Unmarshal fail with an opaque type-mismatch error instead of surfacing the real
	// rejection code -- flexString tolerates both shapes without guessing at what either one means.
	Code             flexString         `json:"code"`
	ServerList       []LoginServerInfo  `json:"serverList"`
	LoginServer      *AccountServerInfo `json:"loginServer"`
	LastLoggedServer flexString         `json:"lastLoggedServer"`
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

	plainForm, err := encodeFormSorted(form)
	if err != nil {
		return nil, fmt.Errorf("encode GSL request form: %w", err)
	}

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
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxGSLResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxGSLResponseSize {
		return nil, fmt.Errorf("getserverlist.php: response body exceeds %d byte limit", maxGSLResponseSize)
	}
	if resp.StatusCode != http.StatusOK {
		// Same reasoning as the three decode-failure branches below: a getserverlist.php
		// response body -- even one accompanying a non-200 status -- may legitimately carry a
		// live at/rt session token, so it must never be echoed into an error. A byte length is
		// enough to diagnose the failure.
		return nil, fmt.Errorf("getserverlist.php: HTTP %d (bodyLen=%d)", resp.StatusCode, len(body))
	}

	// The top-level response may itself be the plaintext respon, or may
	// wrap the real payload (AES-encrypted) inside a `bin` field.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		// Not the raw body -- on success this response legitimately carries a live at/rt
		// session token (see LoginServerListRespon.At/Rt below), so a decode-failure error must
		// never echo it back. A byte length is enough to diagnose a malformed response.
		return nil, fmt.Errorf("decode top-level GSL response: %w (bodyLen=%d)", err, len(body))
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
				// Not the raw plaintext -- it's the AES-decrypted response and, on success,
				// carries a live at/rt session token. See the bodyLen comment above.
				return nil, fmt.Errorf("decode decrypted GSL response: %w (plainLen=%d)", err, len(plain))
			}
			return &lsr, nil
		}
	}
	if err := json.Unmarshal(body, &lsr); err != nil {
		// Not the raw body -- same reasoning as the two decode-failure branches above.
		return nil, fmt.Errorf("decode plaintext GSL response: %w (bodyLen=%d)", err, len(body))
	}
	return &lsr, nil
}

// encodeFormSorted joins form fields as k1=v1&k2=v2&... in a stable
// (insertion-independent) order. Field order does not affect the crypto
// (ECB has no cross-block dependency) but matching the reference client's
// order is good hygiene -- see dossier §03.
//
// Unlike url.Values.Encode(), values are written verbatim, not
// percent-encoded: the reference client's plaintext form body is built the
// same way, and percent-encoding it would just be extra bytes the server
// doesn't expect. A raw '=' inside a value is harmless -- every field here
// is parsed key=value splitting on the FIRST '=' only, and '=' is routinely
// present anyway as base64 padding in airKey (confirmed live: a test build
// with a real base64-derived airKey failed until this exact check was
// narrowed from "&=" to "&" alone). A raw '&' inside a value is the one
// real corruption risk -- it would be misread as a field separator, so it's
// still rejected below. Of the callers, only opt.LoginKey round-trips
// through a local file with no format validation, so it's the one value
// here that isn't inherently safe by construction, but the check applies to
// every field at the one point they all funnel through.
//
// A key present in `form` but absent from `order` is also rejected (rather
// than silently skipped, which is what the loop below would otherwise do):
// this is the exact silent-field-drop failure mode this project has already
// been bitten by once before, just via a different mechanism -- see
// TestEncodeFormSortedOrderMatchesGetServerListFields (gsl_form_sync_test.go)
// for the source-level drift check that normally keeps `order` and
// GetServerList's form.Set(...) calls in sync today. That test only catches
// drift between the two hand-maintained lists as they exist in gsl.go's
// source; this check is the runtime backstop that fires no matter how a
// stray key ends up in `form` (a future caller, a typo'd field name, etc.),
// turning what would otherwise be a silently vanished field into an
// immediately diagnosable error.
func encodeFormSorted(form url.Values) (string, error) {
	order := []string{"uuid", "airKey", "loginFlag", "country", "is3D", "lang", "simOp", "platform",
		"isSimulator", "zone", "gameuid", "newServer", "openCountry", "opt", "loginKey", "rt"}
	var b strings.Builder
	first := true
	consumed := 0
	for _, k := range order {
		v, ok := form[k]
		if !ok || len(v) == 0 {
			continue
		}
		if strings.Contains(v[0], "&") {
			return "", fmt.Errorf("encodeFormSorted: field %q value contains '&', would corrupt the form", k)
		}
		if !first {
			b.WriteByte('&')
		}
		first = false
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(v[0])
		consumed++
	}
	if consumed != len(form) {
		return "", fmt.Errorf("encodeFormSorted: form has %d field(s) but only %d are known to the `order` whitelist -- a field would be silently dropped from the outgoing GSL request", len(form), consumed)
	}
	return b.String(), nil
}

func defaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// b64OfString matches DeviceManager.GetDeviceUid_Transcoding's airKey
// construction, which uses PLAIN standard base64 (not URL-safe).
func b64OfString(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
