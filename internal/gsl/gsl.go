package gsl

import (
	"bytes"
	"cmp"
	"crypto/md5"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Client build identity of the audited Android build, 1.0.370/1911 (apkm info.json, posted
// 2026-11-18), what Application.version and PM_getVersionCode return
// (Assembly-CSharp.decompiled.cs:57831-57832). It is signed with the same certificate as 1.0.351,
// so psh and packageSign are unchanged. A session config's appVersion/versionCode replace it.
const (
	PackageName = "com.fun.lastwar.gp"
	AppVersion  = "1.0.370"
	VersionCode = "1911"
	Platform    = "Android" // Versions.PlatformName / GameUtility.GetPlatformName() -- capitalized
	unityVer    = "440"
)

// MaxGSLResponseSize bounds the HTTP responses read via io.ReadAll below
// (CheckVersion, GetServerList). Reading an untrusted HTTP body without a
// cap is the same trivial multi-GB OOM vector packet.go's sfs.MaxFrameSize
// guards against on the TCP side, just tighter: these are small JSON/text
// config responses, never expected to exceed a few KB.
const MaxGSLResponseSize = 1 << 20 // 1 MiB

// onlineCheckVersionHosts is the client's onlineCheckVersionHostList and onlineEACheckVersionHosts
// its online_ea_CheckVersionHostList (ConstURLConfig, Assembly-CSharp.decompiled.cs:13831-13833,
// 1.0.370).
var (
	onlineCheckVersionHosts = []string{
		"https://lastwar-serverlist-cf.lastwarapp.net",
		"https://lastwar-serverlist-us-aws-ali.lastwargame.com",
		"https://lastwar-serverlist-us-gcp-ali.lastwargame.com",
	}
	onlineEACheckVersionHosts = []string{
		"https://lastwar-serverlist-ea-us-aws.lastwargame.com",
		"https://lastwar-serverlist-ea-us-gcp.lastwargame.com",
	}
)

// CheckVersionHosts lists the candidate CheckVersion/GetServerList gate hosts to try in order,
// dossier §02. EACheckVersionHosts is the list a device in the early-access bucket uses instead
// (checkVersionHostsFor). Tests replace either.
var (
	CheckVersionHosts   = slices.Clone(onlineCheckVersionHosts)
	EACheckVersionHosts = slices.Clone(onlineEACheckVersionHosts)
)

// eaGrayPercent is the bucket bound up to which a device asks the early-access hosts.
const eaGrayPercent = 5

// GrayPercent is GrayUtils.GetPercentageFromMd5 (Assembly-CSharp.decompiled.cs:36030-36035): the
// first 8 bytes of MD5(input) as a little-endian uint64, mod 100, plus 1. The client feeds it the
// device uid, "_n3d" suffix included (DeviceManager.GetDeviceUid, :73399-73435).
func GrayPercent(input string) int {
	sum := md5.Sum([]byte(input))
	return int(binary.LittleEndian.Uint64(sum[:8])%100) + 1
}

// checkVersionHostsFor is NetworkURLConfig.GetHostListByCurGroupType
// (Assembly-CSharp.decompiled.cs:14131-14141): a device whose GrayPercent is at most 5 asks the
// early-access hosts, every other one the online list. The early-access list stands in only for the
// built-in online list (the client's URLGroupType.Online): a caller that replaced CheckVersionHosts,
// as tests do, keeps its own list.
func checkVersionHostsFor(deviceID string) (hosts []string, earlyAccess bool) {
	if deviceID != "" && slices.Equal(CheckVersionHosts, onlineCheckVersionHosts) && GrayPercent(deviceID) <= eaGrayPercent {
		return EACheckVersionHosts, true
	}
	return CheckVersionHosts, false
}

// ErrForcedUpdate is a check-version or Login reply with updateType 2: the server no longer
// accepts the build this client claims. The real client stops on both: check-version enters
// LoadingError "error force update" before it reads any other field
// (Assembly-CSharp.decompiled.cs:9619-9626), and a Login reply moves it to AppUpdate (:13666-13669)
// or, on a cross-server Login, to loading error 9 (:71486-71494). It never calls GSL or logs in.
var ErrForcedUpdate = errors.New("forced update (updateType 2): the server no longer accepts this build; " +
	"move the session config's appVersion/versionCode (or the built-in defaults) to the current release")

// ForcedUpdate reports whether an updateType value means a forced update. The client's
// check-version casts the JSON value with (string) and then ToInt
// (Assembly-CSharp.decompiled.cs:9619-9631): a string "2" stops it, while a numeric 2 makes the
// cast throw, which it logs and then reads the rest of the reply. Go also stops on a numeric 2,
// the safe side for a server that no longer takes this build.
func ForcedUpdate(updateType string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(updateType))
	return err == nil && n == 2
}

// Msg/DownloadURL/ResMsg/HotUpdateMsg are FlexString, not bare string -- round-41 fix, the same
// JSON type-safety gap as their siblings Code/UpdateType, closed for this struct's four remaining
// fields too: a wrong-typed value on ANY field here fails json.Unmarshal for the WHOLE response
// (FlexString's own doc comment already documents live evidence of this endpoint sending a
// bare-string-typed field, code, as a JSON number instead). ResMsg specifically is genuinely read
// (login.go's Login, main.go's -cs-rt refresh path both feed it straight into
// crypto.ParseRSAPubKeyFromDER), so both call sites now convert via FlexString's pre-existing String()
// accessor; DownloadURL/HotUpdateMsg are never read anywhere in this codebase, so widening them is
// behaviorally free, matching AccountServerInfo.WsPort/LoginToken.Time/LoginServerInfo.Uid's own
// precedent of hardening an unread field purely so it can't take the rest of the struct down.
//
// The remaining fields are the rest of what the 1.0.364 client parses (CheckResVersionState,
// A-CS:9506-9857). TableVersion and Locale only arrive when the request carries
// table_env=table_online (confirmed live 2026-10-05, coverage tables report). Formats:
//   - HotUpdateMsg: ";"-separated "name,version,size,crc32" manifest entries (GameRes, DllRes, ...).
//   - LwFile2: "version|bzSize|bzCrc[|size|crc];patches..." for the Lua bundle.
//   - TableVersion: "ver,size,md5[,crc][;patch|...]"; Locale: "ver,lang1,lang2,...".
//   - ClientConfig: "|"-separated "0"/"1" switches indexed by ClientSwitch id (A-CS:44158-44177).
//   - CheckOK: present only in store-review mode. Warmup, FirstLaunchForceUpdateMsg: asset
//     pre-download and first-launch experiment data, not protocol-relevant.
type CheckVersionResponse struct {
	Code                      FlexString `json:"code"`
	Msg                       FlexString `json:"msg"`
	UpdateType                FlexString `json:"updateType"`
	DownloadURL               FlexString `json:"downloadurl"`
	ResMsg                    FlexString `json:"resMsg"`
	HotUpdateMsg              FlexString `json:"hotUpdateMsg"`
	LwFile2                   FlexString `json:"lwfile2"`
	TableVersion              FlexString `json:"table_version"`
	Locale                    FlexString `json:"locale"`
	ClientConfig              FlexString `json:"client_config"`
	CheckOK                   FlexString `json:"checkok"`
	Warmup                    FlexString `json:"warmup"`
	FirstLaunchForceUpdateMsg FlexString `json:"firstLaunchForceUpdateMsg"`
}

// ResVersion derives the Login `resVersion` a fully hot-updated client at this check-version's
// state reports: "{GameRes}.{DllRes}_{lwfile2 version}F_{table_version}.{locale}"
// (GetResVersion, A-CS:49098-49105; "F" marks the LWLF Lua bundle format). Each part is the first
// field of its source: the live iOS 1.0.344 values (GameRes 2159, DllRes 1505, lwfile2 821, table
// 39516, locale 23570) give "2159.1505_821F_39516.23570", the resVersion the real iOS app sent in
// the 2026-10-04 capture. It returns "" when any part is missing or not a plain number of at most
// 9 digits (TableVersion and Locale are absent without table_env=table_online); callers then keep
// sending "0", which the server has accepted for months.
func (cv *CheckVersionResponse) ResVersion() string {
	if cv == nil {
		return ""
	}
	gameRes := hotUpdateManifestVersion(cv.HotUpdateMsg.String(), "gameres")
	dllRes := hotUpdateManifestVersion(cv.HotUpdateMsg.String(), "dllres")
	lua := leadingField(cv.LwFile2.String(), "|")
	table := leadingField(cv.TableVersion.String(), ",")
	locale := leadingField(cv.Locale.String(), ",")
	for _, part := range []string{gameRes, dllRes, lua, table, locale} {
		if !isVersionNumber(part) {
			return ""
		}
	}
	return gameRes + "." + dllRes + "_" + lua + "F_" + table + "." + locale
}

// hotUpdateManifestVersion returns the version (second field) of the hotUpdateMsg entry named
// name, matched case-insensitively: the reply says "GameRes"/"DllRes" and the client looks the
// manifests up as "gameres"/"dllres".
func hotUpdateManifestVersion(hotUpdateMsg, name string) string {
	for entry := range strings.SplitSeq(hotUpdateMsg, ";") {
		fields := strings.Split(strings.TrimSpace(entry), ",")
		if len(fields) >= 2 && strings.EqualFold(fields[0], name) {
			return strings.TrimSpace(fields[1])
		}
	}
	return ""
}

// leadingField returns the first sep-delimited field of the first ";"-delimited segment of s.
func leadingField(s, sep string) string {
	segment, _, _ := strings.Cut(s, ";")
	field, _, _ := strings.Cut(segment, sep)
	return strings.TrimSpace(field)
}

// isVersionNumber reports whether s is 1-9 ASCII digits. Real versions have 3-5; the cap keeps a
// malformed reply from growing the Login field.
func isVersionNumber(s string) bool {
	if s == "" || len(s) > 9 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ClientSwitchOn reports whether client_config switch id is "1". Out-of-range ids are off, as in
// ClientSwitch.IsOn.
func (cv *CheckVersionResponse) ClientSwitchOn(id int) bool {
	if cv == nil || id < 0 {
		return false
	}
	switches := strings.Split(cv.ClientConfig.String(), "|")
	return id < len(switches) && strings.TrimSpace(switches[id]) == "1"
}

// NewPacketZstd reports whether the client frames with the new packet class and Zstd, and so puts
// isUseLz4 true in its Login: NetPacketConst.useNewPacket && useZStd, both false once switch 4
// (DISABLE_NEW_NET_PACKET) or 5 (DISABLE_ZSTD) is on (Assembly-CSharp.decompiled.cs:9744-9745,
// 83176-83179). Without a reply (cv nil) it reports true, what this client sends today.
func (cv *CheckVersionResponse) NewPacketZstd() bool {
	return cv == nil || (!cv.ClientSwitchOn(4) && !cv.ClientSwitchOn(5))
}

// ShumeiSDKOn reports whether switch 25 (ENABLE_SHUMEI_SDK) is on. With it off the client asks the
// ShuMei SDK for nothing and its Login carries shumeiBoxId "" (ShumeiSdkManager.IsFunctionOpen and
// GetLoginSmsdkId, Assembly-CSharp.decompiled.cs:59937, 60044-60058). Without a reply (cv nil) it
// reports true, so a run whose check-version failed keeps today's behaviour.
func (cv *CheckVersionResponse) ShumeiSDKOn() bool {
	return cv == nil || cv.ClientSwitchOn(25)
}

// FlexString accepts a JSON field that the server sometimes encodes as a
// string and sometimes as a bare number (observed live: `code` on error
// responses is a JSON number, e.g. 301).
type FlexString string

func (f *FlexString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = FlexString(s)
		return nil
	}
	// Not a JSON string (e.g. a bare number like 301) -- use the raw bytes as-is.
	*f = FlexString(b)
	return nil
}
func (f FlexString) String() string { return string(f) }

// Int parses f as a base-10 integer, returning 0 if it isn't one (empty, or a value that only
// exists as FlexString for JSON-unmarshal robustness but doesn't actually look like a number in
// practice -- e.g. a corrupted/hostile response). Round-35 fix: LoginServerInfo.ID/Port and
// AccountServerInfo.Port/WsPort used to be plain `int`, so a server response carrying any one of
// them as a JSON string (the same shape LoginServerInfo.Status/LoginServerListRespon.Code are
// already confirmed-live to sometimes arrive as, on this exact endpoint/struct family) failed
// json.Unmarshal for the ENTIRE GetServerList response with an opaque type-mismatch error --
// fatal on the primary login path (login.go's Login) and the standalone -cs-rt refresh command
// (main.go), which have no fallback for a GetServerList error. Now FlexString like their
// siblings, with this accessor doing the int conversion at the handful of call sites that need a
// real int (dialing, port<=0 validation) -- mirrors sfsobject.go's GetIntFlexible: 0 lets the
// caller's own existing "port <= 0"-style validation produce a clear, already-tested error
// instead of this function panicking or the caller silently proceeding with a corrupted value.
//
// key names the field this value came from, purely for the malformed-value Warn's own
// sfs.IsSensitiveSFSKey(key) redaction gate below -- round-42 fix, closing an asymmetry with this
// function's structural sibling GetIntFlexible (same file), which received exactly this
// hardening in round 35. Both call sites today (login.go, main.go) pass the non-sensitive "port",
// so this was not exploitable in practice, but a future caller passing a sensitive key would
// otherwise leak its raw value (both directly, and a second time via strconv's own error text)
// into this Warn with no redaction at all.
func (f FlexString) Int(key string) int {
	if f == "" {
		return 0
	}
	n, err := strconv.Atoi(string(f))
	if err != nil {
		redactedValue := any(string(f))
		redactedErr := any(err)
		if sfs.IsSensitiveSFSKey(key) {
			redactedValue = "[REDACTED]"
			redactedErr = "[REDACTED]"
		}
		slog.Warn("GSL server-list field is present but not a valid integer; falling back to 0", "key", key, "value", redactedValue, "error", redactedErr)
		return 0
	}
	return n
}

// ClientBuild is the client build a check-version request claims to be.
type ClientBuild struct {
	PackageName string
	Platform    string // "Android" or "iOS", as GameUtility.GetPlatformName() spells it
	AppVersion  string
	VersionCode string // sent as buildId
}

// AndroidBuild is the build this client presents by default: the constants above.
var AndroidBuild = ClientBuild{PackageName: PackageName, Platform: Platform, AppVersion: AppVersion, VersionCode: VersionCode}

// tableEnv is ClientConfig.TableEnvName, "table_" + TABLE_ENV, where TABLE_ENV is "online" in a
// shipping build (A-CS:14094, 43157).
const tableEnv = "table_online"

// CheckVersion fetches check-version as AndroidBuild. zone is the session's zone ("APS783"), sent
// as `server` the way the real client sends its cached SERVER_ZONE, or "" when not known yet.
// deviceID picks the gate host list (checkVersionHostsFor); it is never sent.
func CheckVersion(httpClient *http.Client, zone, deviceID string) (*CheckVersionResponse, string, error) {
	return CheckVersionAs(httpClient, AndroidBuild, zone, deviceID)
}

// checkVersionQuery builds the getlsu3dversion.php query the 1.0.364 client sends
// (GetCheckVersionURL, A-CS:14083-14097). uid and deviceId are always empty here: check-version is
// anonymous for this client, and nothing identifying is ever put into it.
func checkVersionQuery(build ClientBuild, zone string) url.Values {
	q := url.Values{}
	q.Set("packageName", build.PackageName)
	q.Set("platform", build.Platform)
	q.Set("appVersion", build.AppVersion)
	q.Set("gm", "0")
	q.Set("server", zone)
	q.Set("uid", "")
	q.Set("deviceId", "")
	q.Set("table_env", tableEnv)
	q.Set("buildId", build.VersionCode)
	q.Set("returnJson", "1")
	q.Set("unityVersion", unityVer)
	return q
}

// CheckVersionAs tries the known gate hosts in order (NOT concurrently, despite earlier wording --
// this is a plain sequential fallback: each host gets the full httpClient timeout before moving
// to the next) and returns the first successful response along with which host answered (that
// host becomes the base URL for every subsequent GSL call -- dossier §02.1). build is the client
// build the request claims; zone and deviceID are as for CheckVersion.
//
// A reply with updateType 2 ends the search: it returns that reply, its host and an error wrapping
// ErrForcedUpdate, which the caller must treat as the end of the run (no GSL call, no Login).
func CheckVersionAs(httpClient *http.Client, build ClientBuild, zone, deviceID string) (*CheckVersionResponse, string, error) {
	q := checkVersionQuery(build, zone)
	hosts, earlyAccess := checkVersionHostsFor(deviceID)
	if earlyAccess {
		slog.Info("check-version: this device is in the early-access bucket; asking the early-access gate hosts, as the real client does",
			"grayPercent", GrayPercent(deviceID), "hosts", hosts)
	}

	// Round-42 fix: every continue branch below now also logs its own host+error at Warn before
	// moving to the next host, closing a real diagnostic gap -- previously only `lastErr` (the
	// LAST-tried host's failure) survived to the final combined error, silently discarding every
	// earlier host's distinct failure reason with zero operator-visible trace. Concretely: host1
	// returning a 200 with a body that fails json.Unmarshal (an actionable "API shape changed"
	// signal) followed by host2 timing out and host3 refusing the connection used to surface only
	// host3's least-diagnostic error to the operator. The final `fmt.Errorf` below is unchanged
	// (still names only the last-tried host's error, for backward-compatible brevity) -- this Warn
	// trail is the mechanism for recovering the earlier hosts' reasons, not a replacement for it.
	var lastErr error
	for _, host := range hosts {
		u := host + "/gameservice/getlsu3dversion.php?" + q.Encode()
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			lastErr = err
			slog.Warn("check-version: host failed, trying next", "host", host, "error", err)
			continue
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			lastErr = err
			slog.Warn("check-version: host failed, trying next", "host", host, "error", err)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, MaxGSLResponseSize+1))
		_ = resp.Body.Close()
		if err != nil {
			lastErr = err
			slog.Warn("check-version: host failed, trying next", "host", host, "error", err)
			continue
		}
		if len(body) > MaxGSLResponseSize {
			lastErr = fmt.Errorf("%s: response body exceeds %d byte limit", host, MaxGSLResponseSize)
			slog.Warn("check-version: host failed, trying next", "host", host, "error", lastErr)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("%s: HTTP %d: %s", host, resp.StatusCode, string(body))
			slog.Warn("check-version: host failed, trying next", "host", host, "error", lastErr)
			continue
		}
		var cv CheckVersionResponse
		if err := json.Unmarshal(body, &cv); err != nil {
			lastErr = fmt.Errorf("%s: decode JSON: %w (body=%s)", host, err, string(body))
			slog.Warn("check-version: host failed, trying next", "host", host, "error", lastErr)
			continue
		}
		// updateType: 0 and 3 proceed, 1 offers an optional store update, 2 forces one. The client
		// tests a forced update before it looks at code (CheckResVersionState.OnRequestSuccess,
		// Assembly-CSharp.decompiled.cs:9617-9639), and a forced update is a valid answer, so no
		// other host is asked.
		if ForcedUpdate(cv.UpdateType.String()) {
			slog.Error("check-version: the server demands a forced update of this build; the real client stops here, and so does this run",
				"host", host, "packageName", build.PackageName, "appVersion", build.AppVersion, "versionCode", build.VersionCode)
			return &cv, host, fmt.Errorf("check-version %s: %w", host, ErrForcedUpdate)
		}
		if cv.Code != "" {
			lastErr = fmt.Errorf("%s: server returned code=%s msg=%s", host, cv.Code, cv.Msg)
			slog.Warn("check-version: host failed, trying next", "host", host, "error", lastErr)
			continue
		}
		// Switches that change the wire protocol or the Login (ClientSwitch,
		// Assembly-CSharp.decompiled.cs:44386-44480): 4/5 turn the new packet class and Zstd off
		// (the Login then carries no isUseLz4, see NewPacketZstd), 21 enables the account-select
		// state, 25 the ShuMei SDK (see ShumeiSDKOn), 26 moves the game socket to WebSocket, 29 adds
		// the forward header to every frame, 36 is the ShuMei create-account risk ban, 44 falls back
		// to WebSocket on every 2nd connect retry, 46 is the iOS native socket. This client
		// implements none of the transport ones. Logged at Info, not Warn: their live values are not
		// known yet, and Go works as it is.
		slog.Info("check-version: tables and client switches", "packageName", build.PackageName, "updateType", cv.UpdateType,
			"hasTableVersion", cv.TableVersion != "", "hasLocale", cv.Locale != "",
			"disableNewNetPacket4", cv.ClientSwitchOn(4), "disableZstd5", cv.ClientSwitchOn(5),
			"accountSelect21", cv.ClientSwitchOn(21), "shumeiSDK25", cv.ClientSwitchOn(25),
			"serverWS26", cv.ClientSwitchOn(26), "crossForward29", cv.ClientSwitchOn(29),
			"shumeiRiskBan36", cv.ClientSwitchOn(36), "wsFallback44", cv.ClientSwitchOn(44), "iosNativeSocket46", cv.ClientSwitchOn(46))
		if !cv.NewPacketZstd() {
			slog.Warn("check-version: ClientSwitch 4 or 5 is on, so the real client frames without the new packet class or Zstd and its Login carries no isUseLz4; "+
				"this client leaves isUseLz4 out of the Login but keeps its own framing",
				"disableNewNetPacket4", cv.ClientSwitchOn(4), "disableZstd5", cv.ClientSwitchOn(5))
		}
		return &cv, host, nil
	}
	return nil, "", fmt.Errorf("all check-version hosts failed, last error: %w", lastErr)
}

// LoginToken mirrors the {token,time} shape seen for `at`/`rt`.
//
// Time is FlexString, not a bare int64 -- round-36 fix, the one field round 35's GetServerList
// JSON type-safety sweep missed. Reached via LoginServerListRespon.At/.Rt *LoginToken, so a
// wrong-typed "time" value used to fail json.Unmarshal for the ENTIRE GetServerList response,
// same fatal-whole-response failure mode round 35 fixed for LoginServerInfo.ID/Port and
// AccountServerInfo.Port/WsPort. Time itself is never read anywhere in this codebase (only
// Token is), so this widening is behaviorally free -- matching AccountServerInfo.WsPort's own
// precedent of hardening an unread field purely so it can't take the rest of the struct down.
//
// Token is FlexString, not a bare string -- round-43 fix, closing the LAST remaining bare-typed
// field in this entire GetServerList/CheckVersion response family (LoginServerInfo,
// AccountServerInfo, LoginServerListRespon, and now LoginToken have all had every field widened
// across rounds 33-43). Token is read by login.go's primary Login path, owndevice.go and main.go's
// standalone -cs-rt command, all through the FlexString.String() accessor.
type LoginToken struct {
	Token FlexString `json:"token"`
	Time  FlexString `json:"time"`
}

// String/GoString are the round-47 regression fix for the MAJOR finding that LoginToken --
// unlike the sfs.SFSObject/sfs.SFSArray/sfs.SFSValue family, which got exactly this redaction-by-construction
// treatment in rounds 14-15 for the identical reason -- carried a live bearer access/refresh token
// in its Token field with nothing structurally stopping a future debug/error call site from
// logging the struct (or a *LoginServerListRespon containing it, via its At/Rt fields) directly.
// Every CURRENT call site (login.go/main.go/crossserver.go) already extracts and wraps only the
// individual token string via Token.String()+sfs.Redact() before logging -- this is defense-in-depth
// for whatever comes next, not a fix for an actively-triggered leak. fmt's struct-field printer
// checks each field for a Stringer/GoStringer implementation even when the CONTAINING struct
// doesn't implement one itself, so this also redacts LoginToken automatically wherever it appears
// nested inside a %v/%+v/%#v of a *LoginServerListRespon (e.g. a future fmt.Errorf("...: %+v",
// lsr)). Blanket-masks unconditionally, mirroring sfs.SFSValue.String()'s own "no per-field key
// context to lean on" reasoning -- Time is unread anywhere in this codebase, so redacting it too
// alongside Token is behaviorally free.
//
// Deliberately NOT also a json.Marshaler: LoginToken is the direct json.Unmarshal target of a
// real GSL HTTP response (never marshaled in production -- this client only ever receives it) and
// several tests round-trip realistic fixtures through encoding/json.Marshal to simulate that wire
// response (see login_integration_test.go's newFakeGSLServer and crossserver_test.go's inline GSL
// fakes); a MarshalJSON override here would silently replace every such fixture's real token with
// the redacted placeholder, breaking every test that asserts on propagated token content. The
// concrete threat this closes (a String()/GoString()-checking call site) doesn't need it.
//
// Round-53 update: LogValue() below closes the specific gap this comment used to describe as
// "left open" -- a LoginToken (or a *LoginServerListRespon whose At/Rt IS the LoginToken itself)
// passed DIRECTLY as a raw slog attribute value, e.g. slog.Any("token", tok). slog resolves
// LogValuer before ever reaching a handler's own formatting, so this protects every handler
// (including slog.NewJSONHandler, the only one main.go installs, which never consults
// fmt.Stringer/fmt.GoStringer at all). What remains open, unaffected by LogValue(): a LoginToken
// NESTED inside a larger struct that is itself passed as the attribute (e.g.
// slog.Any("resp", lsr) where lsr is a *LoginServerListRespon) -- json.Marshal recurses into
// struct fields on its own terms and has no concept of slog.LogValuer, so a future call shaped
// like that would still serialize Token/Time as plain JSON. Closing that would require
// *LoginServerListRespon itself to implement LogValuer (or MarshalJSON, ruled out above for this
// exact struct family) -- a larger, separate fix, not attempted here.
func (t LoginToken) String() string   { return "[REDACTED LoginToken]" }
func (t LoginToken) GoString() string { return t.String() }

// LogValue makes LoginToken satisfy slog.LogValuer -- see this type's own doc comment above (the
// "Round-53 update" paragraph) for what this does and does not close.
func (t LoginToken) LogValue() slog.Value { return slog.StringValue(t.String()) }

type LoginServerInfo struct {
	// ID and Port are FlexString, not a bare int, for the same reason as Status just below (see
	// this file's round-35 fix comment on FlexString.Int): a wrong-typed value on either field
	// used to fail json.Unmarshal for the entire GetServerList response.
	ID FlexString `json:"id"`
	// Name/IP/WsIP/Zone are FlexString, not bare string -- round-42 fix, the same JSON
	// type-safety gap as ID/Port/GameUid/Uid/Status above, closed for this struct's four
	// remaining fields too: a wrong-typed value on ANY field here fails json.Unmarshal for the
	// WHOLE GetServerList response. Zone is genuinely read (login.go's Login reads it as the
	// redial zone and resends it as the wire "zn" field on every subsequent Login), so its one
	// read site now calls the pre-existing FlexString.String() accessor; Name/IP/WsIP are only
	// ever logged (FlexString formats fine directly, matching ID/Port's own existing precedent)
	// -- IP specifically is ALSO read via buildBaseZoneLoginAddr(stateSrv.IP.String(), ...).
	Name FlexString `json:"name"`
	IP   FlexString `json:"ip"` // "|"-delimited fallback hostnames, not a single IP
	WsIP FlexString `json:"ws_ip"`
	Port FlexString `json:"port"`
	Zone FlexString `json:"zone"`
	// GameUid is FlexString, not a bare string -- round-40 fix, the same JSON type-safety gap as
	// ID/Port/Uid above, closed for this field too. UNLIKE Uid, GameUid IS actively read at
	// several call sites -- login.go's Login/waitForInitPush redirect handling, crossserver.go's
	// DoCrossServerLogin redirect handling, and main.go's -cs-rt refresh path all read it off a
	// *LoginServerInfo and feed it into ident.SaveGameUid/p.GameUid/the ip-port-zone-gameUid
	// tuple (all plain `string`), the SecurityCode HMAC, and the login payload -- so every read
	// site now calls the pre-existing .String() accessor (already used elsewhere for other
	// FlexString fields) to convert back to a plain string at the point of use.
	GameUid FlexString `json:"gameUid"`
	// Uid is FlexString, not a bare string -- round-37 fix, the same JSON type-safety gap as
	// ID/Port above, closed for this field too. Uid is never read anywhere in this codebase
	// (unlike GameUid), so this widening is behaviorally free -- matching AccountServerInfo.WsPort's
	// and LoginToken.Time's own precedent of hardening an unread field purely so it can't take
	// the rest of the struct down.
	Uid    FlexString `json:"uid"`
	Status FlexString `json:"status"` // observed as a JSON string, e.g. "0"
}

// AccountServerInfo is the reply's loginServer: the account server the 1.0.370 client opens a
// socket to only for the account-select screen (ClientSwitch 21, a device with no gameUid;
// Assembly-CSharp.decompiled.cs:12797-12802, 9018-9041). It is never a game server, and this
// client never connects to it.
type AccountServerInfo struct {
	// IP/WsIP are FlexString, not bare string -- round-42 fix, the same reason as
	// LoginServerInfo's own Name/IP/WsIP/Zone fix just above: a wrong-typed value on ANY field
	// here fails json.Unmarshal for the whole response.
	IP FlexString `json:"ip"` // "|"-delimited fallback hostnames
	// Port and WsPort are FlexString, not a bare int -- see LoginServerInfo's own doc comment
	// (round-35 fix) for why. Neither is read anywhere in this codebase, but a wrong-typed value
	// would still fail json.Unmarshal for the whole response.
	Port   FlexString `json:"port"`
	WsIP   FlexString `json:"ws_ip"`
	WsPort FlexString `json:"ws_port"`
}

type LoginServerListRespon struct {
	// Code is the GSL result code; Err (below) maps it the way the 1.0.364 client does. The
	// codes come from static analysis only: no rejected getserverlist.php reply has been
	// captured live yet.
	//
	// Code is FlexString, not a bare int, matching CheckVersionResponse.Code and
	// LoginServerInfo.Status: this project has confirmed live that CheckVersionResponse.Code (a
	// sibling endpoint's own `code` field) comes back as either a JSON string or a bare number
	// depending on context (see FlexString's doc comment). getserverlist.php's `code` hasn't
	// itself been observed doing this yet, but if it ever does, a bare int here would make
	// json.Unmarshal fail with an opaque type-mismatch error instead of surfacing the real
	// rejection code -- FlexString tolerates both shapes without guessing at what either one means.
	Code             FlexString         `json:"code"`
	ServerList       []LoginServerInfo  `json:"serverList"`
	LoginServer      *AccountServerInfo `json:"loginServer"`
	LastLoggedServer FlexString         `json:"lastLoggedServer"`
	At               *LoginToken        `json:"at"`
	Rt               *LoginToken        `json:"rt"`
}

// UnmarshalJSON tolerates LoginServer/At/Rt arriving as a non-object JSON shape (e.g. `[]`, PHP's
// json_encode's common encoding for an empty associative array) instead of `{}`/`null` -- round-44
// fix, closing the last remaining JSON-shape-tolerance gap in this response family after rounds
// 33-43 widened every scalar field to FlexString. Unlike a scalar field, a struct-pointer field
// can't simply be widened to FlexString, so this instead pre-inspects each of the three raw values
// via LooksLikeJSONObject and only decodes into the real struct type when it actually looks like a
// JSON object -- any other shape leaves the field nil, the same "absent" behavior every consumer
// already expects (login.go's/crossserver.go's/main.go's `lsr.At != nil` checks), instead of
// failing json.Unmarshal for the
// ENTIRE GetServerList response. This has never been observed live -- it's the same speculative
// defense-in-depth already applied to every sibling scalar field in this struct family, just
// extended to the three fields a simple type-widen can't reach.
//
// Round-45 fix: ServerList gets the identical treatment via a new looksLikeJSONArray sibling check
// -- round 44's own doc comment above claimed this closed the LAST shape-tolerance gap in this
// struct, but left ServerList itself (a slice, not a struct pointer, but with the exact same
// "PHP's json_encode can emit an object instead of an array for a non-sequentially-keyed
// associative array" failure mode) still going through the plain shadow-struct field with zero
// tolerance. If serverList ever arrives as a JSON object instead of an array, it now degrades to
// nil (empty) -- what Err already reports as ErrEmptyServerList (E120) -- instead of failing
// json.Unmarshal for the entire response.
//
// The shadow struct below must be kept in sync with LoginServerListRespon's own field list by
// hand -- a future field added to one and not the other will compile but silently stop round-
// tripping through this custom decoder.
func (l *LoginServerListRespon) UnmarshalJSON(b []byte) error {
	var raw struct {
		Code             FlexString      `json:"code"`
		ServerList       json.RawMessage `json:"serverList"`
		LastLoggedServer FlexString      `json:"lastLoggedServer"`
		LoginServer      json.RawMessage `json:"loginServer"`
		At               json.RawMessage `json:"at"`
		Rt               json.RawMessage `json:"rt"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	l.Code = raw.Code
	l.LastLoggedServer = raw.LastLoggedServer

	if looksLikeJSONArray(raw.ServerList) {
		var v []LoginServerInfo
		if err := json.Unmarshal(raw.ServerList, &v); err != nil {
			return fmt.Errorf("serverList: %w", err)
		}
		l.ServerList = v
	}
	if LooksLikeJSONObject(raw.LoginServer) {
		var v AccountServerInfo
		if err := json.Unmarshal(raw.LoginServer, &v); err != nil {
			return fmt.Errorf("loginServer: %w", err)
		}
		l.LoginServer = &v
	}
	if LooksLikeJSONObject(raw.At) {
		var v LoginToken
		if err := json.Unmarshal(raw.At, &v); err != nil {
			return fmt.Errorf("at: %w", err)
		}
		l.At = &v
	}
	if LooksLikeJSONObject(raw.Rt) {
		var v LoginToken
		if err := json.Unmarshal(raw.Rt, &v); err != nil {
			return fmt.Errorf("rt: %w", err)
		}
		l.Rt = &v
	}
	return nil
}

// GSL result errors. The 1.0.364 client accepts a getserverlist.php reply only when code == 0 and
// serverList is non-empty, and handles the failures as below (A-CS:12465-12495, 12665-12705;
// static only).
var (
	// ErrReauthNeeded is wrapped by the 211 and 212 errors: the server no longer accepts this
	// device's at/rt/loginKey, and only a fresh login (email verification, then a new loginKey or
	// a fresh capture of the real app) gets new ones.
	ErrReauthNeeded = errors.New("re-auth needed")
	// ErrCredentialsInvalid is code 211: the client clears at, rt and loginKey ([AT]ClearAT_211).
	ErrCredentialsInvalid = fmt.Errorf("GSL code 211, at/rt/loginKey invalid: %w", ErrReauthNeeded)
	// ErrAccountMappingInvalid is code 212: the client also clears the uid and server info
	// ([AT]ClearGUID&AT_212).
	ErrAccountMappingInvalid = fmt.Errorf("GSL code 212, at/rt/loginKey and the uid/server mapping invalid: %w", ErrReauthNeeded)
	// ErrNotAvailable is code 201 (E116_201): the client shows "not available" and quits.
	ErrNotAvailable = errors.New("GSL code 201, not available")
	// ErrEnvelopeCrypto is code 213 (E213, ERROR_CHECK_SERVER_LIST_ENCRYPT).
	ErrEnvelopeCrypto = errors.New("GSL code 213, request envelope encryption error")
	// ErrRejected is any other non-zero code (the client's generic E116).
	ErrRejected = errors.New("GSL rejected the request")
	// ErrEmptyServerList is code 0 with no server entries (E120).
	ErrEmptyServerList = errors.New("GSL returned no servers (E120)")
)

// Err returns nil for an accepted reply (code 0, or absent, which the client's JsonUtility also
// reads as 0, plus a non-empty server list), and otherwise one of the errors above, wrapped with the
// code. An empty server list is E120 whatever else the reply carries: loginServer is the account
// server, not a game server (Assembly-CSharp.decompiled.cs:12690-12704, 12797-12802).
func (l *LoginServerListRespon) Err() error {
	code := strings.TrimSpace(l.Code.String())
	var err error
	switch code {
	case "", "0":
		if len(l.ServerList) != 0 {
			return nil
		}
		err = ErrEmptyServerList
	case "201":
		err = ErrNotAvailable
	case "211":
		err = ErrCredentialsInvalid
	case "212":
		err = ErrAccountMappingInvalid
	case "213":
		err = ErrEnvelopeCrypto
	default:
		err = ErrRejected
	}
	if len(code) > 16 {
		code = code[:16] + "..."
	}
	return fmt.Errorf("getserverlist.php: code=%q: %w", code, err)
}

// PickServer returns the server entry to log in to, or nil when the list is empty: the entry
// carrying gameUid (when gameUid is known), else the one whose id is lastLoggedServer, else the
// first. The real client picks GetLastLoggedServerInfo() ?? serverList[0] (A-CS:12730-12734,
// 77709-77722), where lastLoggedServer 0 means none; matching the account's own gameUid first keeps
// a multi-role account on the role this client has been playing.
func (l *LoginServerListRespon) PickServer(gameUid string) *LoginServerInfo {
	if len(l.ServerList) == 0 {
		return nil
	}
	if gameUid != "" {
		for i := range l.ServerList {
			if l.ServerList[i].GameUid.String() == gameUid {
				return &l.ServerList[i]
			}
		}
	}
	if last := strings.TrimSpace(l.LastLoggedServer.String()); last != "" && last != "0" {
		for i := range l.ServerList {
			if strings.TrimSpace(l.ServerList[i].ID.String()) == last {
				return &l.ServerList[i]
			}
		}
	}
	return &l.ServerList[0]
}

// LooksLikeJSONObject reports whether raw is a non-empty JSON value whose first non-whitespace
// byte is '{' -- used by LoginServerListRespon's UnmarshalJSON above to distinguish a genuine
// object (decode normally) from `null`/absent (leave nil, no error) or an unexpected non-object
// shape like `[]` (also leave nil, no error, rather than failing the whole response).
func LooksLikeJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// looksLikeJSONArray is LooksLikeJSONObject's sibling for ServerList -- reports whether raw is a
// non-empty JSON value whose first non-whitespace byte is '[', distinguishing a genuine array
// (decode normally) from `null`/absent or an unexpected non-array shape like `{}` (both leave the
// field nil/empty, no error, rather than failing the whole response).
func looksLikeJSONArray(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '['
}

// FirstHost returns the first entry of a "|"-delimited fallback host list.
func FirstHost(pipeList string) string {
	first, _, _ := strings.Cut(pipeList, "|")
	return first
}

// FindServerInfo locates a Login response's `serverInfo` shard-redirect
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
// Round-39 fix: all three levels below used to collapse present-but-wrong-typed into the same
// silent nil as genuinely-absent, with zero diagnostic signal -- the identical distinction
// login.go's redirectIP/redirectZone (which read fields off this SAME serverInfo object) already
// warn on, for the same reason: this object is documented (see GetIntFlexible below) as sometimes
// sending wrong-typed fields in practice. The one case that must stay silent, by the same
// absence-vs-wrong-type convention, is p.serverInfo being genuinely ABSENT (an ordinary shape for
// responses that never carry a redirect at all) -- only wrong-typed fields warn here.
func FindServerInfo(content *sfs.SFSObject) *sfs.SFSObject {
	if content == nil {
		return nil
	}
	if v, ok := content.Get("serverInfo"); ok {
		if obj, ok := v.Val.(*sfs.SFSObject); ok {
			return obj
		}
		slog.Warn("FindServerInfo: top-level serverInfo field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
	}
	if pv, ok := content.Get("p"); ok {
		pObj, ok := pv.Val.(*sfs.SFSObject)
		if !ok {
			slog.Warn("FindServerInfo: p field is present but not an object", "type", fmt.Sprintf("%T", pv.Val))
			return nil
		}
		if v, ok := pObj.Get("serverInfo"); ok {
			if obj, ok := v.Val.(*sfs.SFSObject); ok {
				return obj
			}
			slog.Warn("FindServerInfo: p.serverInfo field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
		}
	}
	return nil
}

// FindLoginReplyField returns the object of a game-server Login reply that holds key, looked up
// where FindServerInfo looks for serverInfo: the reply itself, else its p object; nil when neither
// has it. The real client reads updateType and new_version off the same message as serverInfo
// (LoginMessage.CSHandleResponse, Assembly-CSharp.decompiled.cs:82830-82896).
func FindLoginReplyField(content *sfs.SFSObject, key string) *sfs.SFSObject {
	if content.Has(key) {
		return content
	}
	if pv, ok := content.Get("p"); ok {
		if p, ok := pv.Val.(*sfs.SFSObject); ok && p.Has(key) {
			return p
		}
	}
	return nil
}

// GSLOpt selects which `opt` value to send, per dossier §02.2 / §05. Opt "" sends no opt field at
// all, which is what the real client does while it holds a fresh at and an rt (see ChooseOpt).
type GSLOpt struct {
	Opt      string // "new" | "login" | "fix" | "refresh" | ""
	LoginKey string
	Rt       string
}

// accessTokenRefreshAge is how old at.time may get before the real client refreshes the access
// token: 2592000 s, 30 days.
const accessTokenRefreshAge = 30 * 24 * time.Hour

// OptState is the persisted credential state the real client picks its GSL opt from
// (AccountCredentialManager.AuthTokens plus ServerInfo.uid).
type OptState struct {
	LoginKey      string
	GameUid       string
	AccessTok     string
	AccessTokTime int64 // GSL at.time: unix seconds, read by the client as the issue time; 0 = unknown
	RefreshTok    string
}

func (s OptState) String() string   { return "[REDACTED OptState]" }
func (s OptState) GoString() string { return s.String() }

// LogValue makes OptState satisfy slog.LogValuer, like every other credential-bearing type here.
func (s OptState) LogValue() slog.Value { return slog.StringValue(s.String()) }

// ChooseOpt is the 1.0.364 client's GSL opt table (GetServerListRequest/GetServerList,
// A-CS:77091-77266; static only):
//
//	loginKey set                          -> opt=login, loginKey=<loginKey>
//	no gameUid                            -> opt=new
//	no rt                                 -> opt=fix
//	at empty, or at.time is >= 30 days old -> opt=refresh, rt=<rt>
//	otherwise                             -> no opt field
//
// An unknown at.time (0) never counts as stale, as in the client. Without an rt this reduces to
// login, then new, then fix.
func ChooseOpt(s OptState, now time.Time) GSLOpt {
	switch {
	case s.LoginKey != "":
		return GSLOpt{Opt: "login", LoginKey: s.LoginKey}
	case s.GameUid == "":
		return GSLOpt{Opt: "new"}
	case s.RefreshTok == "":
		return GSLOpt{Opt: "fix"}
	}
	stale := s.AccessTokTime > 0 && now.Unix()-s.AccessTokTime >= int64(accessTokenRefreshAge/time.Second)
	if s.AccessTok == "" || stale {
		return GSLOpt{Opt: "refresh", Rt: s.RefreshTok}
	}
	return GSLOpt{}
}

// String/GoString are the round-48 regression fix for the MINOR finding that GSLOpt -- which
// carries LoginKey/Rt, live credentials -- had no redaction-by-construction, the same class of gap
// round 47/48 closed for LoginToken/deviceIdentity/SessionConfig. Every current call site only
// logs the .Opt field (login.go), so this is defense-in-depth, not an active leak fix -- rated
// minor rather than major since GSLOpt is short-lived (constructed and consumed within a single
// GetServerList call) rather than held across a whole multi-hundred-line flow.
func (o GSLOpt) String() string   { return "[REDACTED GSLOpt]" }
func (o GSLOpt) GoString() string { return o.String() }

// LogValue makes GSLOpt satisfy slog.LogValuer -- see LoginToken's identical round-53 fix above
// for the full rationale.
func (o GSLOpt) LogValue() slog.Value { return slog.StringValue(o.String()) }

// GetServerList performs the RSA+AES-wrapped GSL POST as an Android client (GetServerListAs with
// Platform) and returns the decrypted, parsed response.
func GetServerList(httpClient *http.Client, gateHost string, pub *rsa.PublicKey, deviceID string, opt GSLOpt, zone, gameUid string) (*LoginServerListRespon, error) {
	return GetServerListAs(httpClient, gateHost, pub, Platform, deviceID, opt, zone, gameUid)
}

// GetServerListAs is GetServerList with the form's `platform` field set to platform: the issuing
// client's GameUtility.GetPlatformName(), "Android" or "iOS" (csharp-native §2.3). A session the iOS
// app issued sends "iOS", as that app does.
func GetServerListAs(httpClient *http.Client, gateHost string, pub *rsa.PublicKey, platform, deviceID string, opt GSLOpt, zone, gameUid string) (*LoginServerListRespon, error) {
	return GetServerListFor(httpClient, gateHost, pub, FormDevice{Platform: platform}, deviceID, opt, zone, gameUid)
}

// FormDevice is what a getserverlist.php form says about the device besides its id
// (GetServerListRequest, Assembly-CSharp.decompiled.cs:77430-77441, 1.0.370). An empty Country or
// Lang sends the "US"/"en" this client has always sent.
type FormDevice struct {
	Platform string // GameUtility.GetPlatformName(): "Android" or "iOS"
	Country  string // GlobalData.fromCountry, the same value as the Login's fromCountry
	Lang     string // Localization.GetLanguageName(), the same value as the Login's lang
	SimOp    string // SDKManager.GetSimOperator(): the SIM's MCC+MNC on Android, "" here on iOS
}

// GetServerListFor is GetServerListAs with the device fields of dev.
func GetServerListFor(httpClient *http.Client, gateHost string, pub *rsa.PublicKey, dev FormDevice, deviceID string, opt GSLOpt, zone, gameUid string) (*LoginServerListRespon, error) {
	gc := crypto.NewGSLCrypto(pub)

	airKey := "lwDid_" + B64OfString(deviceID)

	form := url.Values{}
	form.Set("uuid", deviceID)
	form.Set("airKey", airKey)
	form.Set("loginFlag", "1")
	form.Set("country", cmp.Or(dev.Country, "US"))
	form.Set("is3D", "1")
	form.Set("lang", cmp.Or(dev.Lang, "en"))
	form.Set("simOp", dev.SimOp)
	form.Set("platform", dev.Platform)
	// isSimulator stays "0" on both platforms. The Android client sends "1" whenever
	// HasIllegalDeviceFeature() holds, which includes Build.SERIAL "unknown" (the AndroidID this
	// client sends; com/sdkmanager/SdkManager.java:1005-1046), but no real 1.0.370 Android form has
	// been captured to confirm that, so it isn't flipped on static evidence (1.0.370 audit B29).
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
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxGSLResponseSize+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxGSLResponseSize {
		return nil, fmt.Errorf("getserverlist.php: response body exceeds %d byte limit", MaxGSLResponseSize)
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
		// "bin" is present but empty. Without this branch, execution would fall through to
		// json.Unmarshal(body, &lsr) below against the ORIGINAL top-level envelope (shaped like
		// {"bin":"",...}), which has none of LoginServerListRespon's fields -- unknown/extra
		// keys are silently ignored by encoding/json, so lsr would end up completely
		// zero-valued with a nil error, which a caller could mistake for a real (if empty)
		// reply. Fail loud instead: this is a decode failure, not an empty-but-valid response.
		return nil, fmt.Errorf("GSL response: bin field present but empty")
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

func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second}
}

// B64OfString matches DeviceManager.GetDeviceUid_Transcoding's airKey
// construction, which uses PLAIN standard base64 (not URL-safe).
func B64OfString(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}
