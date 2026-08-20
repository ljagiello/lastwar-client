package main

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
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

// checkVersionHosts lists the candidate CheckVersion/GetServerList gate hosts to try in order,
// dossier §02.
var checkVersionHosts = []string{
	"https://lastwar-serverlist-cf.lastwarapp.net",
	"https://lastwar-serverlist-us-aws-ali.lastwargame.com",
	"https://lastwar-serverlist-us-gcp-ali.lastwargame.com",
}

// Msg/DownloadURL/ResMsg/HotUpdateMsg are flexString, not bare string -- round-41 fix, the same
// JSON type-safety gap as their siblings Code/UpdateType, closed for this struct's four remaining
// fields too: a wrong-typed value on ANY field here fails json.Unmarshal for the WHOLE response
// (flexString's own doc comment already documents live evidence of this endpoint sending a
// bare-string-typed field, code, as a JSON number instead). ResMsg specifically is genuinely read
// (login.go's Login, main.go's -cs-rt refresh path both feed it straight into
// parseRSAPubKeyFromDER), so both call sites now convert via flexString's pre-existing String()
// accessor; DownloadURL/HotUpdateMsg are never read anywhere in this codebase, so widening them is
// behaviorally free, matching AccountServerInfo.WsPort/LoginToken.Time/LoginServerInfo.Uid's own
// precedent of hardening an unread field purely so it can't take the rest of the struct down.
type CheckVersionResponse struct {
	Code         flexString `json:"code"`
	Msg          flexString `json:"msg"`
	UpdateType   flexString `json:"updateType"`
	DownloadURL  flexString `json:"downloadurl"`
	ResMsg       flexString `json:"resMsg"`
	HotUpdateMsg flexString `json:"hotUpdateMsg"`
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

// Int parses f as a base-10 integer, returning 0 if it isn't one (empty, or a value that only
// exists as flexString for JSON-unmarshal robustness but doesn't actually look like a number in
// practice -- e.g. a corrupted/hostile response). Round-35 fix: LoginServerInfo.ID/Port and
// AccountServerInfo.Port/WsPort used to be plain `int`, so a server response carrying any one of
// them as a JSON string (the same shape LoginServerInfo.Status/LoginServerListRespon.Code are
// already confirmed-live to sometimes arrive as, on this exact endpoint/struct family) failed
// json.Unmarshal for the ENTIRE GetServerList response with an opaque type-mismatch error --
// fatal on the primary login path (login.go's Login) and the standalone -cs-rt refresh command
// (main.go), which have no fallback for a GetServerList error. Now flexString like their
// siblings, with this accessor doing the int conversion at the handful of call sites that need a
// real int (dialing, port<=0 validation) -- mirrors sfsobject.go's getIntFlexible: 0 lets the
// caller's own existing "port <= 0"-style validation produce a clear, already-tested error
// instead of this function panicking or the caller silently proceeding with a corrupted value.
//
// key names the field this value came from, purely for the malformed-value Warn's own
// isSensitiveSFSKey(key) redaction gate below -- round-42 fix, closing an asymmetry with this
// function's structural sibling getIntFlexible (same file), which received exactly this
// hardening in round 35. Both call sites today (login.go, main.go) pass the non-sensitive "port",
// so this was not exploitable in practice, but a future caller passing a sensitive key would
// otherwise leak its raw value (both directly, and a second time via strconv's own error text)
// into this Warn with no redaction at all.
func (f flexString) Int(key string) int {
	if f == "" {
		return 0
	}
	n, err := strconv.Atoi(string(f))
	if err != nil {
		redactedValue := any(string(f))
		redactedErr := any(err)
		if isSensitiveSFSKey(key) {
			redactedValue = "[REDACTED]"
			redactedErr = "[REDACTED]"
		}
		slog.Warn("GSL server-list field is present but not a valid integer; falling back to 0", "key", key, "value", redactedValue, "error", redactedErr)
		return 0
	}
	return n
}

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
	for _, host := range checkVersionHosts {
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
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxGSLResponseSize+1))
		resp.Body.Close()
		if err != nil {
			lastErr = err
			slog.Warn("check-version: host failed, trying next", "host", host, "error", err)
			continue
		}
		if len(body) > maxGSLResponseSize {
			lastErr = fmt.Errorf("%s: response body exceeds %d byte limit", host, maxGSLResponseSize)
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
		if cv.Code != "" {
			lastErr = fmt.Errorf("%s: server returned code=%s msg=%s", host, cv.Code, cv.Msg)
			slog.Warn("check-version: host failed, trying next", "host", host, "error", lastErr)
			continue
		}
		return &cv, host, nil
	}
	return nil, "", fmt.Errorf("all check-version hosts failed, last error: %w", lastErr)
}

// LoginToken mirrors the {token,time} shape seen for `at`/`rt`.
//
// Time is flexString, not a bare int64 -- round-36 fix, the one field round 35's GetServerList
// JSON type-safety sweep missed. Reached via LoginServerListRespon.At/.Rt *LoginToken, so a
// wrong-typed "time" value used to fail json.Unmarshal for the ENTIRE GetServerList response,
// same fatal-whole-response failure mode round 35 fixed for LoginServerInfo.ID/Port and
// AccountServerInfo.Port/WsPort. Time itself is never read anywhere in this codebase (only
// Token is), so this widening is behaviorally free -- matching AccountServerInfo.WsPort's own
// precedent of hardening an unread field purely so it can't take the rest of the struct down.
//
// Token is flexString, not a bare string -- round-43 fix, closing the LAST remaining bare-typed
// field in this entire GetServerList/CheckVersion response family (LoginServerInfo,
// AccountServerInfo, LoginServerListRespon, and now LoginToken have all had every field widened
// across rounds 33-43). Token is actively read at 4 call sites (login.go's primary Login path and
// its mid-redirect GSL refresh, crossserver.go's DoCrossServerLogin redirect refresh, and main.go's
// standalone -cs-rt command), all now converted to the pre-existing flexString.String() accessor.
type LoginToken struct {
	Token flexString `json:"token"`
	Time  flexString `json:"time"`
}

type LoginServerInfo struct {
	// ID and Port are flexString, not a bare int, for the same reason as Status just below (see
	// this file's round-35 fix comment on flexString.Int): a wrong-typed value on either field
	// used to fail json.Unmarshal for the entire GetServerList response.
	ID flexString `json:"id"`
	// Name/IP/WsIP/Zone are flexString, not bare string -- round-42 fix, the same JSON
	// type-safety gap as ID/Port/GameUid/Uid/Status above, closed for this struct's four
	// remaining fields too: a wrong-typed value on ANY field here fails json.Unmarshal for the
	// WHOLE GetServerList response. Zone is genuinely read (login.go's Login reads it as the
	// redial zone and resends it as the wire "zn" field on every subsequent Login), so its one
	// read site now calls the pre-existing flexString.String() accessor; Name/IP/WsIP are only
	// ever logged (flexString formats fine directly, matching ID/Port's own existing precedent)
	// -- IP specifically is ALSO read via buildBaseZoneLoginAddr(stateSrv.IP.String(), ...).
	Name flexString `json:"name"`
	IP   flexString `json:"ip"` // "|"-delimited fallback hostnames, not a single IP
	WsIP flexString `json:"ws_ip"`
	Port flexString `json:"port"`
	Zone flexString `json:"zone"`
	// GameUid is flexString, not a bare string -- round-40 fix, the same JSON type-safety gap as
	// ID/Port/Uid above, closed for this field too. UNLIKE Uid, GameUid IS actively read at
	// several call sites -- login.go's Login/waitForInitPush redirect handling, crossserver.go's
	// DoCrossServerLogin redirect handling, and main.go's -cs-rt refresh path all read it off a
	// *LoginServerInfo and feed it into ident.SaveGameUid/p.GameUid/the ip-port-zone-gameUid
	// tuple (all plain `string`), the SecurityCode HMAC, and the login payload -- so every read
	// site now calls the pre-existing .String() accessor (already used elsewhere for other
	// flexString fields) to convert back to a plain string at the point of use.
	GameUid flexString `json:"gameUid"`
	// Uid is flexString, not a bare string -- round-37 fix, the same JSON type-safety gap as
	// ID/Port above, closed for this field too. Uid is never read anywhere in this codebase
	// (unlike GameUid), so this widening is behaviorally free -- matching AccountServerInfo.WsPort's
	// and LoginToken.Time's own precedent of hardening an unread field purely so it can't take
	// the rest of the struct down.
	Uid    flexString `json:"uid"`
	Status flexString `json:"status"` // observed as a JSON string, e.g. "0"
}

// AccountServerInfo is the account/login-service endpoint (distinct from a
// specific game-state server) -- used for the very first connection when no
// account/state is associated with this device yet (opt=new).
type AccountServerInfo struct {
	// IP/WsIP are flexString, not bare string -- round-42 fix, the same reason as
	// LoginServerInfo's own Name/IP/WsIP/Zone fix just above: a wrong-typed value on ANY field
	// here fails json.Unmarshal for the whole response. Also lets applyLoginServerFallback below
	// assign these straight into LoginServerInfo.IP/WsIP (also flexString as of the same fix)
	// with no conversion needed.
	IP flexString `json:"ip"` // "|"-delimited fallback hostnames
	// Port and WsPort are flexString, not a bare int -- see LoginServerInfo's own doc comment
	// (round-35 fix) for why. WsPort is never read anywhere in this codebase today (see
	// applyLoginServerFallback's doc comment below), but a wrong-typed value on it would still
	// fail json.Unmarshal for the whole response before that unused-ness ever mattered.
	Port   flexString `json:"port"`
	WsIP   flexString `json:"ws_ip"`
	WsPort flexString `json:"ws_port"`
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

// applyLoginServerFallback synthesizes a single ServerList entry from LoginServer when the
// server returned no ServerList entries at all -- exactly the scenario AccountServerInfo's own
// doc comment describes: opt=new, i.e. the very first connection, before any account/state is
// associated with this device. Before this fallback existed, every caller of GetServerList
// (login.go's Login, unconditionally) treated an empty ServerList as a hard failure
// ("no servers returned") even when the response carried a perfectly usable LoginServer --
// the exact field its own comment says exists for this case.
//
// This has NOT been confirmed live: no capture exists in this repo of a genuine opt=new
// response with an empty ServerList (populated or not). Per this project's standing rule
// against guessing at unconfirmed server behavior, the fallback is deliberately conservative:
//   - it only fires for opt=new, matching AccountServerInfo's own documented scope exactly --
//     GetServerList's opt=fix/opt=refresh/opt=login callers (crossserver.go's DoCrossServerLogin
//     redirect-refresh, main.go's -cs-rt refresh, login.go's own opt=login/opt=fix paths) see
//     zero behavior change from this function;
//   - it only fires when ServerList is genuinely empty, so it never touches, reorders, or
//     shadows a populated ServerList;
//   - it never invents data AccountServerInfo doesn't carry. Notably, AccountServerInfo has no
//     Zone field (unlike LoginServerInfo), so the synthesized entry's Zone is left "" rather
//     than guessed -- callers that derive a zone/serverID from it (e.g. login.go's
//     serverIDFromZone) get a best-effort, not-necessarily-correct empty zone in this fallback
//     path, which is the honest reflection of what AccountServerInfo actually provides.
func applyLoginServerFallback(lsr *LoginServerListRespon, opt GSLOpt) {
	if opt.Opt != "new" || len(lsr.ServerList) != 0 || lsr.LoginServer == nil {
		return
	}
	lsr.ServerList = []LoginServerInfo{{
		IP:   lsr.LoginServer.IP,
		Port: lsr.LoginServer.Port,
		WsIP: lsr.LoginServer.WsIP,
		// WsPort has no home in LoginServerInfo (which carries no ws_port field at all -- see
		// its definition above) and, like LoginServerInfo.WsIP, ws_ip/ws_port aren't read
		// anywhere in this codebase today; nothing is lost that would otherwise be used.
		// ID, Name, Zone, GameUid, Uid, Status: left at their zero values -- AccountServerInfo
		// carries none of them, and opt=new is precisely the case where no gameUid/account
		// exists yet to fill them with.
	}}
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
// Round-39 fix: all three levels below used to collapse present-but-wrong-typed into the same
// silent nil as genuinely-absent, with zero diagnostic signal -- the identical distinction
// login.go's redirectIP/redirectZone (which read fields off this SAME serverInfo object) already
// warn on, for the same reason: this object is documented (see getIntFlexible below) as sometimes
// sending wrong-typed fields in practice. The one case that must stay silent, by the same
// absence-vs-wrong-type convention, is p.serverInfo being genuinely ABSENT (an ordinary shape for
// responses that never carry a redirect at all) -- only wrong-typed fields warn here.
func findServerInfo(content *SFSObject) *SFSObject {
	if content == nil {
		return nil
	}
	if v, ok := content.Get("serverInfo"); ok {
		if obj, ok := v.Val.(*SFSObject); ok {
			return obj
		}
		slog.Warn("findServerInfo: top-level serverInfo field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
	}
	if pv, ok := content.Get("p"); ok {
		pObj, ok := pv.Val.(*SFSObject)
		if !ok {
			slog.Warn("findServerInfo: p field is present but not an object", "type", fmt.Sprintf("%T", pv.Val))
			return nil
		}
		if v, ok := pObj.Get("serverInfo"); ok {
			if obj, ok := v.Val.(*SFSObject); ok {
				return obj
			}
			slog.Warn("findServerInfo: p.serverInfo field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
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
//
// Round 30 fix: the string-fallback path used to do a bare, unchecked int32(n) conversion on
// strconv.Atoi's result, mirroring the exact bug GetInt's own round-29 fix (sfsobject.go) closed
// for its int64 case -- on a 64-bit platform Go's int is 64-bit, so Atoi happily parses a
// numeric string outside int32's range, and the bare conversion silently wraps it (e.g.
// "4294967301" -> 5) instead of rejecting it. Both real call sites (login.go's/crossserver.go's
// port-from-redirect reads) feed this straight into buildBaseZoneLoginAddr's redial, whose only
// port guard rejects non-positive values -- a wrapped small positive port would have sailed
// straight through. Now checked against math.MinInt32/MaxInt32 before converting, degrading to
// the same 0 fallback this function already uses for an absent/empty field, exactly like GetInt's
// own fix. See TestGetIntFlexibleRejectsOutOfInt32RangeString (redirect_helpers_test.go).
//
// Round 31 fix: this function used to have no diagnostic at all for a present-but-genuinely-
// anomalous field -- either a non-empty string that isn't a valid integer literal, or a value of
// some other Go type entirely (bool, float, nested object, ...) that neither the int-shaped nor
// string-shaped path above recognizes -- silently falling all the way through to the same 0
// fallback used for a merely-absent field, with zero signal distinguishing the two. This is the
// identical present-but-wrong-typed-vs-genuinely-absent distinction login.go's redirectIP/
// redirectZone (reading the ip/zone siblings on this SAME serverInfo object) already warn on --
// both functions' own doc comments cite this function's port-string precedent as the reason that
// distinction matters, but the precedent itself was never given the matching diagnostic. Added
// below, without changing either success path's existing behavior at all.
//
// Round 32 fix: round 31's enumeration of anomaly cases above omitted a THIRD one that already
// existed in this function at the time: the out-of-int32-range numeric string case round 30's own
// fix (above) guards against. That branch still silently returned 0 with zero diagnostic, exactly
// as anomalous as the two round 31 did add warnings for -- an out-of-range "port" string is just
// as real an anomaly as a non-numeric one, and was indistinguishable in the logs from a merely-
// absent field. Now warns here too, using the same message shape as its non-numeric-string sibling
// immediately below.
//
// Round 33 fix: after three straight rounds of incrementally discovering this function still had
// an uncovered anomaly branch, a final exhaustive re-check found a FOURTH: a present, correctly-
// typed int64 Long whose VALUE doesn't fit in int32's range (GetInt's own round-29 fix already
// degrades this to 0, but silently) was still indistinguishable from a merely-absent field, since
// neither the string-fallback path nor the final wrong-Go-type check (both below) can see a
// same-value-range problem on an already-int64-typed field. Checked directly against the raw
// decoded value, immediately after the initial GetInt() call.
//
// Round 41 fix: that round-33 check was REMOVED, not kept, once GetInt itself (sfsobject.go)
// gained its own out-of-int32-range-Long Warn in round 39, closing the exact "GetInt's own fix
// degrades this to 0, but silently" gap this comment describes. From round 39 onward, the initial
// `o.GetInt(key)` call above already warns and returns 0 for this anomaly before this function's
// own duplicate check could ever run, so the round-33 block had become dead weight: every
// triggering input produced two separate Warn log lines (GetInt's own, then this function's
// identical-in-substance one) for a single anomaly, confirmed by direct reproduction. Removed
// rather than left in place, since GetInt's diagnostic already carries the same key/redacted-value
// information this function's own would have.
func getIntFlexible(o *SFSObject, key string) int32 {
	if n := o.GetInt(key); n != 0 {
		return n
	}
	// Round-35 fix: redactedValue gates the three raw-scalar "value" log args below on
	// isSensitiveSFSKey(key), matching every sibling wrong-typed-field guard in this codebase
	// (requireFieldType/warnIfWrongTypedField/redirectIP/redirectZone all log only
	// StringRedacted()/goType, never a field's own raw scalar). getIntFlexible is a generic,
	// key-parameterized helper -- today's only call sites hardcode key="port" (never sensitive),
	// but a future caller passing a sensitive key would otherwise leak its real value into these
	// three anomaly-diagnostic Warn calls with no redaction at all, unlike this function's own
	// fourth branch below, which already used the safe StringRedacted() pattern.
	redactedValue := func(v any) any {
		if isSensitiveSFSKey(key) {
			return "[REDACTED]"
		}
		return v
	}
	if s := o.GetString(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n < math.MinInt32 || n > math.MaxInt32 {
				slog.Warn("serverInfo redirect: field present as an out-of-int32-range numeric string, falling back to 0",
					"key", key, "value", redactedValue(s))
				return 0
			}
			return int32(n)
		}
		slog.Warn("serverInfo redirect: field present as a non-numeric string, falling back to 0",
			"key", key, "value", redactedValue(s))
		return 0
	}
	// Neither GetInt nor GetString produced anything -- either key is genuinely absent (silent,
	// the ordinary case) or it's present with some other Go type entirely, which neither accessor
	// recognizes -- warn only for the latter.
	if v, ok := o.Get(key); ok && v.Val != nil &&
		!sfsFieldKindAccepts(sfsFieldKindInt, v.Val) && !sfsFieldKindAccepts(sfsFieldKindString, v.Val) {
		slog.Warn("serverInfo redirect: field present but wrong-typed (neither numeric nor string) -- falling back to 0",
			"key", key, "goType", fmt.Sprintf("%T", v.Val), "raw", o.StringRedacted())
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
			applyLoginServerFallback(&lsr, opt)
			return &lsr, nil
		}
		// "bin" is present but empty. Without this branch, execution would fall through to
		// json.Unmarshal(body, &lsr) below against the ORIGINAL top-level envelope (shaped like
		// {"bin":"",...}), which has none of LoginServerListRespon's fields -- unknown/extra
		// keys are silently ignored by encoding/json, so lsr would end up completely
		// zero-valued with a nil error. Both real call sites (login.go's initial GSL call and
		// the serverInfo-redirect access-token refresh path) treat a nil error as success; the
		// refresh path in particular never logs anything in that case -- neither the "fresh
		// access token acquired" success line nor the "GSL refresh failed" fallback line fires,
		// making it a fully silent no-op. Fail loud instead: this is a decode failure, not an
		// empty-but-valid response.
		return nil, fmt.Errorf("GSL response: bin field present but empty")
	}
	if err := json.Unmarshal(body, &lsr); err != nil {
		// Not the raw body -- same reasoning as the two decode-failure branches above.
		return nil, fmt.Errorf("decode plaintext GSL response: %w (bodyLen=%d)", err, len(body))
	}
	applyLoginServerFallback(&lsr, opt)
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
