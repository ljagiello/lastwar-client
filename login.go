package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// LoginResult carries everything a caller needs after a successful login:
// the live connection and the account snapshot from push.account.login.new
// (when the email-code path ran) or the base login response otherwise.
type LoginResult struct {
	Conn      *GameConn
	Ident     *deviceIdentity
	Account   *SFSObject // push.account.login.new params, if the email path ran (nil on loginKey fast-path)
	Buildings []Building // populated if the `init` bootstrap push arrived during login (see waitForInitPush)
	Visitors  []Visitor  // populated alongside Buildings, from the same `init` push (see waitForInitPush)
}

// LoginOptions configures how Login authenticates.
type LoginOptions struct {
	Email     string // required unless a LoginKey is already persisted
	CodePipe  string // FIFO path to read the email verification code from; "" reads stdin
	Handshake bool   // experimental: send the vanilla SFS2X pre-Login Handshake (see conn.go:DoHandshake)
}

// gslOptFor picks the GSL getserverlist opt for a device identity, per
// dossier §02.2's opt table, refined empirically:
//
//	loginKey known             -> opt=login (fastest, resolves the real account directly)
//	gameUid known, no loginKey -> opt=fix
//	neither known              -> opt=new (brand new device)
func gslOptFor(ident *deviceIdentity) GSLOpt {
	switch {
	case ident.LoginKey != "":
		return GSLOpt{Opt: "login", LoginKey: ident.LoginKey}
	case ident.GameUid != "":
		return GSLOpt{Opt: "fix"}
	default:
		return GSLOpt{Opt: "new"}
	}
}

// buildBaseZoneLoginAddr builds the "host:port" dial address for the base zone SFS2X connection
// from a GSL server list entry (or a serverInfo redirect payload -- same "ip"/"port" shape),
// guarding against an empty ip: Go's "host:port" dial syntax treats an empty host as the loopback
// interface, so an unguarded fmt.Sprintf("%s:%d", "", port) wouldn't fail cleanly at all -- it
// would silently attempt a real TCP connection to 127.0.0.1/::1 and return a misleading
// "connection refused" instead of any indication that no host was ever given. Mirrors main.go's
// equivalent `if firstHost(ip) == "" { ...; os.Exit(1) }` guard on the cross-server login path,
// adapted to return an error rather than exit since this is a library-style function.
//
// Also guards against a non-positive port, mirroring main.go's own separate `if port <= 0 { ...;
// os.Exit(1) }` pre-flight check on the cross-server login path (which validates the CLI-supplied
// initial port only -- it does not, and cannot, cover this function's mid-login serverInfo
// redirect call site below, where the port comes from the server at runtime). Without this guard,
// a redirect payload with a missing, unparseable, or out-of-range `port` field -- gsl.go's
// getIntFlexible returns 0 for all three cases rather than erroring (warning for the latter two as
// of round 32, but still returning 0 either way, not erroring) -- would sail through and produce a
// "host:0" address instead of a clear error, same failure class as the empty-host case above.
//
// Used at two call sites in this file: the initial dial address built from GSL's server list, and
// the mid-login serverInfo redirect branch below (which had this exact gap until round 18 for the
// host half, and round 19 for the port half -- the redirect's ip/port are fresh values the server
// hands back mid-login, not something a caller or an earlier check can pre-validate, so the guard
// has to live here, not just at the first call site). crossserver.go's DoCrossServerLogin reuses
// this same helper for its own, byte-for-byte identical redirect branch rather than duplicating
// the guard.
//
// The GSL-entry call site itself is not observed live: reachable only if gsl.go's
// applyLoginServerFallback synthesizes an empty-IP ServerList[0] entry, which itself requires
// LoginServer.IP to also be empty -- a low-probability, nested-unconfirmed-conditions scenario.
// Guarded anyway since the failure mode (silently dialing loopback) is confusing enough to be
// worth a clear error over a cryptic one.
func buildBaseZoneLoginAddr(ip string, port int) (string, error) {
	host := firstHost(ip)
	if host == "" {
		return "", fmt.Errorf("no ip in GSL server list entry (an empty host would dial the loopback interface instead of failing clearly)")
	}
	if port <= 0 {
		return "", fmt.Errorf("no valid port in GSL server list entry (port=%d would silently build a bogus \"host:0\"-shaped address instead of failing clearly)", port)
	}
	return fmt.Sprintf("%s:%d", host, port), nil
}

// redirectIP reads a serverInfo redirect payload's "ip" field the same way the pre-round-29
// unguarded siObj.GetString("ip") did, but distinguishes a present-but-wrong-typed field (logged
// as a Warn) from a genuinely absent/empty one (silently "", exactly as before). This distinction
// matters here in a way buildings.go's requireFieldType (which collapses missing-vs-wrong-typed
// into the same Warn+reject, correct for a hard list-entry reject) does not fit: an absent/empty
// ip on this object is the ORDINARY case seen on the vast majority of logins that never need a
// redirect at all, and must stay silent, while a present-but-wrong-typed ip is a real,
// non-theoretical risk worth a Warn -- gsl.go's getIntFlexible helper exists specifically because
// this SAME serverInfo object's neighboring port field is documented as "confirmed live...
// sometimes a UTF string instead" of a number in practice, and ip could just as easily arrive
// wrong-typed too. This is exactly the field a real, live-confirmed shard redirect hinges on (see
// this file's doc comment above the redirect-handling loop below, and crossserver.go's
// DoCrossServerLogin doc comment, for why silently losing this signal is not merely theoretical).
// context names the caller for the log line's benefit (e.g. "login.go base-zone Login").
func redirectIP(siObj *SFSObject, context string) string {
	v, ok := siObj.Get("ip")
	if !ok || v.Val == nil {
		return ""
	}
	if !sfsFieldKindAccepts(sfsFieldKindString, v.Val) {
		slog.Warn("serverInfo redirect: ip field present but wrong-typed -- a live shard redirect may be silently missed",
			"context", context, "goType", fmt.Sprintf("%T", v.Val), "raw", siObj.StringRedacted())
		return ""
	}
	return siObj.GetString("ip")
}

// redirectZone is redirectIP's sibling for the serverInfo redirect payload's "zone" field: the
// same present-but-wrong-typed-vs-genuinely-absent distinction (Warn on the former, silent "" on
// the latter), applied to zone instead of ip. This matters in a DIFFERENT, arguably worse way
// than a wrong-typed ip does: a wrong-typed ip stops the redirect from being followed at all --
// there's nowhere to redial to (see redirectIP's own doc comment) -- but a wrong-typed zone does
// NOT stop anything, since ip/port can still resolve fine on their own. The connection silently
// redials to the NEW ip/port while keeping the STALE zone, a real, non-theoretical desync risk:
// both call sites below resend this (possibly stale) zone as `zn` on the redialed Login, so a
// silently-kept-stale zone means the redialed connection claims the OLD zone while actually
// talking to the NEW shard. gsl.go's getIntFlexible helper exists specifically because this SAME
// serverInfo object is documented as sometimes sending wrong-typed fields -- zone is exactly as
// exposed to that as ip and port are. context names the caller for the log line's benefit, same
// as redirectIP.
func redirectZone(siObj *SFSObject, context string) string {
	v, ok := siObj.Get("zone")
	if !ok || v.Val == nil {
		return ""
	}
	if !sfsFieldKindAccepts(sfsFieldKindString, v.Val) {
		slog.Warn("serverInfo redirect: zone field present but wrong-typed -- may silently keep a stale zone while still redialing to the new ip/port",
			"context", context, "goType", fmt.Sprintf("%T", v.Val), "raw", siObj.StringRedacted())
		return ""
	}
	return capOversizedIdentityField("zone", siObj.GetString("zone"), "", context)
}

// capOversizedIdentityField is the round-47 regression fix for the MAJOR finding that zone,
// gameUid, and accessTok -- unlike loginKey/gameUid/username, which route through
// SaveLoginKey/SaveGameUid/SaveUsername and got a maxIdentityFieldLen guard in round 46 -- are
// re-encoded via PutUtfString on every login/redial attempt with no length check at all, despite
// originating from the identical unguarded sources: a gsl.go flexString field bounded only by the
// 1MiB whole-HTTP-body maxGSLResponseSize cap, or an SFS2X serverInfo redirect field that can
// arrive tagged sfsText (bounded only by packet.go's 64MiB maxFrameSize) -- GetString cannot tell
// that tag apart from the 65535-byte-capped sfsUtfString tag it also decodes to the identical Go
// string type for. writeUtfString (sfsobject.go) hard-rejects anything over 65535 bytes, so an
// oversized value reaching PutUtfString fails EncodeObject/SendEnvelope, and that purely local
// encode failure gets wrapped in sendStageError (conn.go), which deliberately, by design, forces
// Timeout()==false -- indistinguishable from a genuine dead connection to every caller. field/
// context name the caller for the log line's benefit; fallback is the value used instead of the
// oversized one -- callers pass "" at a first-assignment site (nothing to fall back to yet) or the
// previous value at a mid-redirect refresh site (matching this codebase's existing "treat an
// anomalous value as unchanged, not corrupting" philosophy, e.g. GetServerList refresh failures
// already fall back to the stale token/zone rather than clearing it).
func capOversizedIdentityField(field, value, fallback, context string) string {
	if len(value) <= maxIdentityFieldLen {
		return value
	}
	slog.Warn(field+" exceeds identity field length cap; using fallback instead of an unencodable value",
		"context", context, "len", len(value), "cap", maxIdentityFieldLen)
	return fallback
}

// dialGame indirects DialGame (conn.go) through a package var, mirroring gsl.go's
// checkVersionHosts override pattern, so a test can substitute a fake dialer -- e.g. one that
// hands back a real GameConn with its underlying net.Conn swapped for a write-failing wrapper
// (conn_wait_test.go's writeFailConn, or login_integration_test.go's writeFailAfterConn for
// targeting a specific later send in a multi-step flow) -- rather than trying to win an
// inherently racy real-TCP "peer closed right after accept" timing game to force a deterministic
// send-stage failure. Production code always resolves this to the real DialGame; only tests ever
// reassign it, and always restore the original via t.Cleanup.
var dialGame = DialGame

// warnIfWrongTypedField logs a Warn when o has field present (non-nil) but its concrete decoded
// Go type isn't one kind's corresponding GetLong/GetInt/GetString accessor (sfsobject.go) actually
// reads -- distinct from field being genuinely absent, which every call site already, correctly,
// treats as "nothing to do" and stays silent about. Unlike buildings.go's requireFieldType (which
// collapses missing-vs-wrong-typed into the same Warn+reject, appropriate for a hard list-entry
// reject), this only adds a diagnostic for the wrong-typed case: an absent field must not itself
// log anything.
//
// context names the caller for the log line's benefit -- deliberately generic wording (not
// "...persist...", which fit only this function's original login.go call sites, all genuinely
// persistence reads): round 30 reused this same helper for mail.go call sites that have nothing to
// do with persistence (HasUnclaimedReward's rewardStatus classification, ListMail's lastMailTime
// pagination cursor), and the old hardcoded "persist" wording produced nonsensical log lines like
// "skipping mail reward status persist: rewardStatus field present but wrong-typed" there. Fixed
// (round 31) by dropping the persistence-specific word from the template entirely.
func warnIfWrongTypedField(o *SFSObject, field, context string, kind sfsFieldKind) {
	v, ok := o.Get(field)
	if !ok || v.Val == nil {
		return
	}
	if !sfsFieldKindAccepts(kind, v.Val) {
		slog.Warn("skipping "+context+": "+field+" field present but wrong-typed",
			"field", field, "goType", fmt.Sprintf("%T", v.Val))
	}
}

// maxServerListLogEntries bounds how many lsr.ServerList entries the per-entry "state server" Info
// log below will emit. GetServerList's response is server-controlled and, like buildings.go's
// maxRawBuildingItemsPerPush, mail.go's mailListRawItemCap, and visitors.go's maxVisitorsUpperBound,
// is not itself bounded by the SFS2X/HTTP protocol -- a malicious or buggy gate host returning an
// enormous ServerList would otherwise make Login() emit one slog.Info call per entry with no
// ceiling, burning CPU/log-volume proportional to an attacker-controlled response size. Set well
// above any real deployment's server count (a handful of state servers per zone bucket) so normal
// operation never truncates.
const maxServerListLogEntries = 500

// Login runs the full bootstrap: HTTP check-version, GSL getserverlist,
// SFS2X TCP connect, base zone login, and -- unless a persisted loginKey
// lets GSL resolve the account directly -- the email verification-code
// flow. Returns a live, authenticated connection with heartbeat running.
func Login(opts LoginOptions) (*LoginResult, error) {
	httpClient := defaultHTTPClient()

	slog.Info("check-version: fetch RSA pubkey and pick gate host")
	cv, gateHost, err := CheckVersion(httpClient)
	if err != nil {
		return nil, err
	}
	slog.Info("gate host", "gateHost", gateHost)
	slog.Info("check-version response", "updateType", cv.UpdateType, "resMsgLen", len(cv.ResMsg))

	pub, err := parseRSAPubKeyFromDER(cv.ResMsg.String())
	if err != nil {
		return nil, err
	}
	slog.Info("RSA pubkey", "bits", rsaModulusBitLen(pub))

	ident, err := loadOrCreateDeviceIdentity()
	if err != nil {
		return nil, err
	}
	slog.Info("device identity", "deviceIdLen", len(ident.DeviceID))
	slog.Info("air key", "airKeyLen", len(ident.AirKey()))
	// Not "username": ident.Username -- that would print the operator's real persisted account
	// username in cleartext at Info level on every run. Mirrors the emailLen pattern used
	// elsewhere in this file for the same PII-in-a-plain-string reason (see step 6 below).
	slog.Info("persisted state", "usernameLen", len(ident.Username), "gameUid", ident.GameUid, "loginKey", redact(ident.LoginKey))

	opt := gslOptFor(ident)
	slog.Info("step 2: GSL getserverlist", "opt", opt.Opt)
	lsr, err := GetServerList(httpClient, gateHost, pub, ident.DeviceID, opt, "", ident.GameUid)
	if err != nil {
		return nil, err
	}
	slog.Info("GSL getserverlist response", "code", lsr.Code, "serverListLen", len(lsr.ServerList), "lastLoggedServer", lsr.LastLoggedServer)
	if len(lsr.ServerList) == 0 {
		return nil, fmt.Errorf("no servers returned")
	}
	serverListLogCount := len(lsr.ServerList)
	if serverListLogCount > maxServerListLogEntries {
		slog.Warn("state server list longer than log cap; truncating per-entry logging",
			"serverListLen", serverListLogCount, "cap", maxServerListLogEntries)
		serverListLogCount = maxServerListLogEntries
	}
	for _, s := range lsr.ServerList[:serverListLogCount] {
		slog.Info("state server", "id", s.ID, "name", s.Name, "ip", s.IP, "port", s.Port, "zone", s.Zone, "gameUid", s.GameUid, "status", s.Status)
	}
	accessTok := ""
	if lsr.At != nil {
		accessTok = capOversizedIdentityField("accessTok", lsr.At.Token.String(), "", "login initial GSL response")
		slog.Info("access token acquired", "tokenLen", len(accessTok))
	}

	stateSrv := lsr.ServerList[0]
	zone := capOversizedIdentityField("zone", stateSrv.Zone.String(), "", "login initial GSL response")
	gameUid := capOversizedIdentityField("gameUid", stateSrv.GameUid.String(), "", "login initial GSL response")
	if gameUid != "" && gameUid != ident.GameUid {
		if err := ident.SaveGameUid(gameUid); err != nil {
			slog.Warn("failed to persist gameUid", "error", err)
		}
	}
	addr, err := buildBaseZoneLoginAddr(stateSrv.IP.String(), stateSrv.Port.Int("port"))
	if err != nil {
		return nil, fmt.Errorf("login: %w", err)
	}
	serverID := serverIDFromZone(zone)

	// Steps 3-5 (dial, login, wait-for-init) were originally a retry loop
	// mirroring the real client's own recovery from a missing `init` push
	// (report 15's init_push_missing_after_login finding -- PushInitState,
	// Assembly-CSharp.decompiled.cs:18808-18853, LoginTryCount<3: a hard
	// timeout then a full reconnect, up to 3 attempts). Tested live and
	// disabled: ANY reconnect attempt on this server -- with un="",
	// composite un, fresh access token, stale access token, any
	// combination -- reliably fails with ec=28/E011 or E005, even for a
	// brand-new guest that connected once seconds earlier. Reconnecting
	// doesn't recover a missing init; it just turns a working session
	// into a broken one. maxLoginAttempts is kept at 1 (no retry) with a
	// single longer wait window instead. The serverInfo{ip,port,zone,uid}
	// shard-redirect check (LoginMessage.CSHandleResponse,
	// Assembly-CSharp.decompiled.cs:122613-122624) is kept, and its own
	// `attempt--` guards it from being throttled by maxLoginAttempts --
	// confirmed live and genuinely necessary, not just theoretical: a
	// session config captured against one zone kept "successfully"
	// logging in weeks later after a real server merge moved the account
	// to a different zone/host/port entirely, while every subsequent
	// command timed out because the old connection no longer served this
	// account at all. This redirect field is exactly how the server says
	// so -- see the matching, more detailed writeup on
	// DoCrossServerLogin in crossserver.go, which had the same gap.
	const maxLoginAttempts = 1
	const maxRedirectHops = 3
	const initPushTimeout = 45 * time.Second

	var conn *GameConn
	var buildings []Building
	var visitors []Visitor
	var initErr error
	gotInit := false
	redirectHops := 0

	for attempt := 1; attempt <= maxLoginAttempts; attempt++ {
		if attempt > 1 {
			slog.Info("retry", "attempt", attempt, "maxAttempts", maxLoginAttempts)
		}
		slog.Info("step 3: open SFS2X TCP connection")
		slog.Info("dialing", "addr", addr, "zone", zone)
		conn, err = dialGame(addr, 10*time.Second)
		if err != nil {
			return nil, err
		}
		conn.StartHeartbeat(4*time.Second, time.Now())
		slog.Info("connected")

		if opts.Handshake {
			slog.Info("step 3b: SFS2X Handshake (experimental, see conn.go:DoHandshake)")
			hsResp, err := conn.DoHandshake(10 * time.Second)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("handshake: %w", err)
			}
			slog.Info("handshake OK", "response", hsResp.StringRedacted())
		}

		slog.Info("step 4: SFS zone login")
		// NOTE: per LoginMessage.CSSetData (Assembly-CSharp.decompiled.cs:
		// 122420+), `un` should be AccountCredentialManager.ServerInfo.uid
		// -- GSL's composite `gameUid` -- once known. Empirically that's
		// backwards for the base zone Login on THIS server: sending a
		// non-empty `un`/`p.gameUid` reliably gets ec=28/E005 (or E011),
		// reproducibly, even for a low-value guest identity that has
		// simply connected once before -- not just for an established
		// real account. `un=""` (letting the server resolve identity from
		// deviceId/airKey alone) is the only combination that has ever
		// worked, in every test run tonight. Best guess:
		// AccountCredentialManager.SetUID is only actually called from the
		// role-picker flow (UIRoleLoginView.OnClickLogin) in real play,
		// not populated automatically from a plain GSL response the way
		// this dossier's static reading assumed -- so ServerInfo.uid is
		// genuinely empty at this call site in normal operation too.
		loginParams := BuildLoginParams(LoginParamsInput{
			FutureID:  1,
			DeviceID:  ident.DeviceID,
			AirKey:    ident.AirKey(),
			GameUid:   "",
			AccessTok: accessTok,
			ServerID:  serverID,
		})
		loginContent := NewSFSObject()
		loginContent.PutUtfString("zn", zone)
		loginContent.PutUtfString("un", "")
		loginContent.PutUtfString("pw", "")
		loginContent.PutSFSObject("p", loginParams)
		if err := conn.SendEnvelope(controllerSystem, actionLogin, loginContent); err != nil {
			conn.Close()
			return nil, sendStageError{err: err}
		}
		slog.Info("login request sent, waiting for response", "gameUid", gameUid, "at", redact(accessTok))

		env, err := waitFor(conn, 15*time.Second, func(e *Envelope) bool {
			return e.Controller == controllerSystem && e.Action == actionLogin
		})
		if err != nil {
			conn.Close()
			return nil, err
		}
		if env.Content == nil {
			conn.Close()
			return nil, fmt.Errorf("LOGIN FAILED: response had no p payload")
		}
		if ec, ok := env.Content.Get("ec"); ok {
			conn.Close()
			return nil, fmt.Errorf("LOGIN FAILED: ec=%v full=%s: %w", ec.Val, env.Content.StringRedacted(), ErrAuthRejected)
		}
		// warnIfWrongTypedField below adds a diagnostic for the "field present but wrong-typed"
		// case specifically, matching the push.account.login.new un/loginKey/gameUid/
		// gameUserName siblings further down this function -- distinct from genuinely absent,
		// which the un != "" check just below already, deliberately, treats as "nothing to
		// persist" and stays silent about.
		warnIfWrongTypedField(env.Content, "un", "base-zone login response", sfsFieldKindString)
		// Not "un": env.Content.GetString("un") -- that would print the server's real returned
		// account username in cleartext at Info level on every successful login. usernameLen
		// mirrors the emailLen pattern used elsewhere in this file for the same reason.
		un := env.Content.GetString("un")
		slog.Info("login OK", "usernameLen", len(un))
		if un != "" && un != ident.Username {
			if err := ident.SaveUsername(un); err != nil {
				slog.Warn("failed to persist username", "error", err)
			} else {
				slog.Info("persisted username for future runs", "usernameLen", len(un))
			}
		}

		siObj := findServerInfo(env.Content)
		redirectIPVal := ""
		if siObj != nil {
			redirectIPVal = redirectIP(siObj, "login.go base-zone Login")
		}
		if siObj != nil && redirectIPVal != "" {
			redirectHops++
			if redirectHops > maxRedirectHops {
				conn.Close()
				return nil, fmt.Errorf("login: too many serverInfo redirects (>%d), last addr=%s zone=%s", maxRedirectHops, addr, zone)
			}
			newAddr, err := buildBaseZoneLoginAddr(redirectIPVal, int(getIntFlexible(siObj, "port")))
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("login: serverInfo redirect: %w", err)
			}
			// redirectZone (above) is redirectIP's sibling for this field -- see its doc comment
			// for why a wrong-typed zone is a real, non-theoretical desync risk even though
			// (unlike a wrong-typed ip) it doesn't stop the redirect itself from being followed.
			newZone := redirectZone(siObj, "login.go base-zone Login")
			slog.Info("serverInfo redirect: reconnecting to new address", "newAddr", newAddr, "newZone", newZone, "oldAddr", addr, "oldZone", zone)
			conn.Close()
			addr = newAddr
			if newZone != "" {
				zone = newZone
				serverID = serverIDFromZone(zone)
			}
			// Same suspected single-use-per-connection risk as crossserver.go's
			// DoCrossServerLogin redirect path (which does the equivalent token
			// refresh): this closes the connection and redials a brand-new TCP
			// session, so refresh the access token before reconnecting rather than
			// carrying the old one forward unverified.
			slog.Info("fetching fresh access token before following serverInfo redirect (suspected single-use-per-connection)")
			freshOpt := gslOptFor(ident)
			freshLsr, err := GetServerList(httpClient, gateHost, pub, ident.DeviceID, freshOpt, "", ident.GameUid)
			if err != nil {
				slog.Error("GSL refresh failed; following redirect with stale token anyway", "error", err)
			} else {
				if freshLsr.At != nil {
					accessTok = capOversizedIdentityField("accessTok", freshLsr.At.Token.String(), accessTok, "login serverInfo redirect GSL refresh")
					slog.Info("fresh access token acquired", "tokenLen", len(accessTok))
				}
				// The same refresh response also carries the account's current
				// gameUid (serverList[0].gameUid) -- propagate it the same way as
				// accessTok above. Without this, gameUid stays pinned to whatever
				// it was before this redirect even when the GSL refresh (issued
				// specifically because this account just got redirected to a new
				// shard) reports a different one. Only overwrite on a non-empty
				// value -- an empty gameUid here is more likely an unpopulated
				// field than a real "clear the uid" instruction, and clobbering a
				// known-good value with "" is not a safe default to guess at. See
				// DoCrossServerLogin's matching redirect path in crossserver.go,
				// which had this same gap.
				if len(freshLsr.ServerList) > 0 {
					if newGameUid := capOversizedIdentityField("gameUid", freshLsr.ServerList[0].GameUid.String(), "", "login serverInfo redirect GSL refresh"); newGameUid != "" && newGameUid != gameUid {
						slog.Info("serverInfo redirect: gameUid changed on GSL refresh", "oldGameUid", gameUid, "newGameUid", newGameUid)
						gameUid = newGameUid
						if err := ident.SaveGameUid(gameUid); err != nil {
							slog.Warn("failed to persist gameUid", "error", err)
						}
					}
				}
			}
			// A redirect is a deterministic server instruction, not a
			// flaky timeout -- don't let it consume the one real
			// init-push-timeout retry attempt maxLoginAttempts guards
			// (see the comment above this loop for why that's kept at
			// 1 and not bumped up casually).
			attempt--
			continue
		}

		slog.Info("step 5: waiting for init push sequence")
		conn.conn.SetReadDeadline(time.Time{})
		buildings, visitors, gotInit, initErr = waitForInitPush(conn, initPushTimeout)
		if gotInit {
			slog.Info("got init push", "buildings", len(buildings))
			break
		}
		if initErr != nil {
			// A real connection failure (reset, EOF, decode error, ...) surfaced while waiting --
			// distinct from a genuine silence-until-deadline timeout (initErr == nil in that case).
			// See waitForInitPush's doc comment for why this distinction matters: unlike a plain
			// timeout, where falling through to "giving up... continuing anyway" below is the right
			// call (the connection itself is still presumably fine, it just never got `init`), this
			// means conn is definitely dead -- fail fast instead of returning a nominally-successful
			// LoginResult that wraps a broken connection with no indication anything went wrong.
			slog.Error("no init push: connection failed while waiting", "error", initErr, "timeoutMs", initPushTimeout.Milliseconds())
			conn.Close()
			return nil, fmt.Errorf("login: connection failed while waiting for init push: %w", initErr)
		}
		slog.Error("no init push within timeout", "timeoutMs", initPushTimeout.Milliseconds())
	}
	if !gotInit {
		slog.Warn("giving up on init after all attempts; continuing anyway, building list may be empty")
	}

	result := &LoginResult{Conn: conn, Ident: ident, Buildings: buildings, Visitors: visitors}

	if opt.Opt == "login" {
		// GSL already resolved the real account via loginKey; the base
		// SFS login above logged us in directly. Nothing more to do.
		if opts.Email != "" {
			slog.Warn("ignoring -email because a loginKey is already persisted (fast-path login skips email verification)")
		}
		if opts.CodePipe != "" {
			slog.Warn("ignoring -code-pipe because a loginKey is already persisted (fast-path login skips email verification)")
		}
		slog.Info("fast-path login via loginKey complete, skipping email verification")
		return result, nil
	}

	if opts.Email == "" {
		// No email and no loginKey: stay on the guest identity the base
		// zone Login above already created. A guest is a fully playable
		// account at the SFS/game level, just with no real progress --
		// fine for exercising game mechanics without touching a real
		// account or going through email verification.
		if opts.CodePipe != "" {
			slog.Warn("ignoring -code-pipe because -email is not set (guest identity flow doesn't use email verification, so there's no code to pipe in)")
		}
		slog.Info("no -email given; staying on guest identity (no account binding)")
		return result, nil
	}

	slog.Info("step 6: request email verify code")
	sendCodeParams := NewSFSObject()
	sendCodeParams.PutUtfString("mail", opts.Email)
	sendCodeParams.PutUtfString("lang", "en")
	if err := conn.SendExtension("account.login.send.verify.code", sendCodeParams); err != nil {
		conn.Close()
		return nil, sendStageError{err: err}
	}
	slog.Info("sent account.login.send.verify.code", "emailLen", len(opts.Email))

	msg, err := waitForCmd(conn, 15*time.Second, "account.login.send.verify.code", "push.account.send.verify.code")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if ec, ok := msg.Params.Get("errorCode"); ok {
		conn.Close()
		return nil, fmt.Errorf("SEND-CODE FAILED: errorCode=%v full=%s: %w", ec.Val, msg.Params.StringRedacted(), ErrAuthRejected)
	}
	slog.Info("server accepted", "response", msg.Params.StringRedacted())
	slog.Info("verification code should now be arriving", "emailLen", len(opts.Email))

	slog.Info("step 7: waiting for verification code")
	var code string
	if opts.CodePipe != "" {
		slog.Info("waiting for a writer on code pipe", "codePipe", opts.CodePipe)
		code = readCodeFromPipe(opts.CodePipe, conn)
	} else {
		slog.Info("feed the 6-digit code on stdin")
		code = readCodeFromStdin(conn)
	}
	slog.Info("got code", "codeLen", len(code))

	slog.Info("step 8: complete login with account.login.new (type=0, mail+code)")
	finishParams := NewSFSObject()
	finishParams.PutInt("type", 0)
	finishParams.PutUtfString("mail", opts.Email)
	finishParams.PutUtfString("verifyCode", code)
	finishParams.PutUtfString("pf", "market_global")
	finishParams.PutUtfString("deviceId", ident.DeviceID)
	finishParams.PutUtfString("airKey", ident.AirKey())
	if err := conn.SendExtension("account.login.new", finishParams); err != nil {
		conn.Close()
		return nil, sendStageError{err: err}
	}

	ackMsg, err := waitForCmd(conn, 15*time.Second, "account.login.new")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if ec, ok := ackMsg.Params.Get("errorCode"); ok {
		conn.Close()
		return nil, fmt.Errorf("LOGIN-WITH-CODE FAILED: errorCode=%v full=%s: %w", ec.Val, ackMsg.Params.StringRedacted(), ErrAuthRejected)
	}
	slog.Info("ack", "response", ackMsg.Params.StringRedacted())

	// The direct response above is just a terse {success=true} ack; the
	// actual account data (gameUid, loginKey, accountArr, ...) arrives
	// separately as a push.account.login.new push.
	msg2, err := waitForCmd(conn, 15*time.Second, "push.account.login.new")
	if err != nil {
		conn.Close()
		return nil, err
	}
	if ec, ok := msg2.Params.Get("errorCode"); ok {
		conn.Close()
		// Not msg2.Params.String() -- same as the success path just below: this push's full
		// response shape carries loginKey (and accountArr) in cleartext regardless of whether
		// it's reporting success or, as here, an error, and String() does no field-level
		// redaction. Build the error from explicitly-selected, individually-redacted fields
		// instead of dumping the raw response.
		return nil, fmt.Errorf("LOGIN-WITH-CODE FAILED (push): errorCode=%v gameUid=%s loginKey=%s: %w",
			ec.Val, msg2.Params.GetString("gameUid"), redact(msg2.Params.GetString("loginKey")), ErrAuthRejected)
	}
	// Not msg2.Params.String() -- the full response carries loginKey (and
	// accountArr) in cleartext, and String() does no field-level redaction.
	slog.Info("login success", "gameUid", msg2.Params.GetString("gameUid"), "loginKey", redact(msg2.Params.GetString("loginKey")))
	result.Account = msg2.Params

	// warnIfWrongTypedField below adds a diagnostic for the "field present but wrong-typed"
	// case specifically -- distinct from genuinely absent, which the GetString-then-compare-to-""
	// checks that follow already, deliberately, treat as "nothing to persist" and stay silent
	// about. Without this, a wrong-typed field here degrades the next run to the full
	// email-verification flow (or leaves gameUid/username stale) with no log line indicating why.
	warnIfWrongTypedField(msg2.Params, "loginKey", "push.account.login.new", sfsFieldKindString)
	if lk := msg2.Params.GetString("loginKey"); lk != "" {
		if err := ident.SaveLoginKey(lk); err != nil {
			slog.Warn("failed to persist loginKey", "error", err)
		} else {
			slog.Info("persisted loginKey for future fast logins")
		}
	}
	warnIfWrongTypedField(msg2.Params, "gameUid", "push.account.login.new", sfsFieldKindString)
	if gu := msg2.Params.GetString("gameUid"); gu != "" {
		if err := ident.SaveGameUid(gu); err != nil {
			slog.Warn("failed to persist gameUid", "error", err)
		}
	}
	warnIfWrongTypedField(msg2.Params, "gameUserName", "push.account.login.new", sfsFieldKindString)
	if un := msg2.Params.GetString("gameUserName"); un != "" {
		if err := ident.SaveUsername(un); err != nil {
			slog.Warn("failed to persist username", "error", err)
		}
	}

	return result, nil
}

// maxRedactRuneScanInput bounds how large an input redact() will fully rune-scan (the []rune(s)
// conversion below) before switching to a bounded fast path that extracts just the first/last 4
// runes directly without ever materializing a full copy of the input. Round-39 fix: redact()
// previously always converted the FULL input to []rune regardless of size -- an ~4x-amplifying
// allocation for ASCII input (each 1-byte rune becomes a 4-byte int32) -- and sfsobject.go's
// redactSFSValue calls this on a sensitive field's value with NO format-time budget at all, unlike
// every other value shape StringRedacted() formats (all bounded by formatBudget/maxFormattedNodes,
// per formatSFSValueRedacted's own chargeUpTo/truncateAtRuneBoundary handling). A hostile/spoofed
// server (or crafted -decode-stream capture) can tag a field literally named
// loginKey/accessToken/airKey as an oversized string (up to maxFrameSize=64MiB), forcing an
// unbounded ~320MB-peak allocation on every StringRedacted() call that reaches it -- and this
// fires repeatedly per session (conn.go's logCommandResult on every failed response, buildings.go's
// push-handling switch on every observed push), not just once. Any input above this threshold is
// comfortably long enough -- even under a pathological 4-bytes-per-rune input -- to guarantee
// redact()'s own length-scaling formula (k := n/8, capped at 4) has already saturated at k=4, so
// the fast path below can skip straight to that shape without computing an exact rune count or
// allocating a full []rune of the input.
const maxRedactRuneScanInput = 1024

// firstNRunesPrefix returns the byte-slice prefix of s covering its first n runes (or all of s if
// it has fewer), without ever converting s to []rune -- Go's built-in `for range` over a string
// already decodes UTF-8 one rune at a time and lets this stop after n runes instead of continuing
// through the rest of a potentially huge string.
func firstNRunesPrefix(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// lastNRunesSuffix is firstNRunesPrefix's mirror for the tail: walks backward from the end of s,
// decoding one rune at a time via utf8.DecodeLastRuneInString, stopping after n runes instead of
// ever scanning forward through the rest of a potentially huge string.
func lastNRunesSuffix(s string, n int) string {
	count := 0
	end := len(s)
	for end > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:end])
		end -= size
		count++
	}
	return s[end:]
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	if len(s) > maxRedactRuneScanInput {
		return firstNRunesPrefix(s, 4) + "..." + lastNRunesSuffix(s, 4)
	}
	// Slice by rune, not byte, boundary: sensitiveSFSKeys covers fields that can
	// legitimately carry multi-byte UTF-8 (googleName is a Google account display
	// name, e.g. CJK; mail is an internationalized email address). Raw byte-index
	// slicing (the pre-fix s[:4]/s[len(s)-4:] here) can land mid-rune and emit
	// invalid UTF-8 into both slog's JSON sink and StringRedacted()'s raw
	// fmt.Printf terminal sink.
	r := []rune(s)
	n := len(r)
	if n <= 8 {
		// Byte length is >8 (checked above) but rune count is small -- a short
		// multi-byte string (e.g. a 3-rune CJK name at 3 bytes/rune = 9 bytes).
		// Not enough runes to usefully show a non-overlapping prefix/suffix
		// slice without leaking most or all of it back out, so fully redact
		// instead, same as the short-input case above.
		return "***"
	}
	// How many runes to reveal from each end. This used to be a flat 4/4
	// regardless of length, which is fine for long opaque tokens
	// (loginKey/accessToken, typically 32-64+ chars, where showing a fixed
	// prefix/suffix as a correlation aid across log lines doesn't meaningfully
	// weaken anything) but badly over-exposes realistic human password
	// lengths: sfsobject.go's redactSFSValue calls this for EVERY sensitive
	// string field, including "pw"/"password", not just long tokens, and the
	// old flat rule revealed a clear MAJORITY of a realistic password --
	// redact("Summer2024!") (11 runes) used to produce "Summ...024!", 8 of 11
	// chars (~73%) visible.
	//
	// Scale k with length instead: n/8, capped at 4. This keeps the reveal a
	// clear minority across the realistic password range (~18-25% visible for
	// 9-20 rune input, well under a 40% ceiling) while converging on exactly
	// the original first-4/last-4 shape once n reaches 32 -- long enough that
	// revealing 8 chars is itself a small minority (25%) -- and never reveals
	// more than that fixed 4/4 prefix/suffix for even longer tokens, keeping
	// the shape useful for visually correlating "is this the same token as
	// before" across log lines.
	k := n / 8
	if k > 4 {
		k = 4
	}
	return string(r[:k]) + "..." + string(r[n-k:])
}

// waitForInitPush waits for the bare `init` bootstrap push (report 14 §5:
// the real post-login init, not push.init.build), sending `login.init` --
// a real, currently-registered active-pull command exempted from the
// "must be logged in" gate for exactly this window
// (Assembly-CSharp.decompiled.cs:109304, 121428, 122381-122419) -- partway
// through the wait as a fallback, in case the push itself never arrives
// (report 15's init_push_missing_after_login finding #2). Returns the
// parsed building list, the parsed visitor list, whether `init` was
// actually seen, and the terminal read error: nil on a genuine
// silence-until-deadline timeout (the expected "server just never sent it"
// case), or the real non-timeout error (connection reset, EOF, a decode
// error, ...) when the wait ended for some other reason -- mirroring
// waitFor's sibling helper, which returns its own ReadEnvelope error
// verbatim rather than collapsing every failure mode into one generic
// "gave up" outcome. A failed SendExtension for the login.init active-pull
// fallback itself is treated identically to a failed ReadEnvelope: it's
// returned immediately as this same terminal connection-failure result
// (wrapped in sendStageError, conn.go -- round 30; see the send site
// below), not merely logged and fallen through into the next blocking
// read. A half-open connection can surface a local write error fast while
// the following ReadEnvelope genuinely blocks until the deadline --
// without this, that scenario would misreport a definite, already-logged
// connection failure as an ordinary silence-until-deadline timeout,
// denying the caller (Login) the fail-fast behavior it's specifically
// built around the initErr!=nil case for.
func waitForInitPush(conn *GameConn, timeout time.Duration) ([]Building, []Visitor, bool, error) {
	deadline := time.Now().Add(timeout)
	halfway := time.Now().Add(timeout / 2)
	sentActivePull := false
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil, false, nil
		}

		if !sentActivePull && !time.Now().Before(halfway) {
			sentActivePull = true
			slog.Info("halfway through init-wait window; sending login.init as active-pull fallback")
			req := NewSFSObject()
			req.PutInt("_id", 2)
			req.PutUtfString("dataConfigMd5", "")
			if err := conn.SendExtension("login.init", req); err != nil {
				// Same terminal treatment as a failed ReadEnvelope below: a failed send is a
				// definite, already-observed connection failure, not something to merely log and
				// keep waiting past -- see this function's doc comment for why falling through
				// here would misreport it as a plain timeout instead. Wrapped in sendStageError
				// (conn.go), matching every other direct SendEnvelope/SendExtension call site on
				// the login hot path (round 29) -- this one was missed then. Without it, a write
				// failure that itself happens to report Timeout()==true (e.g. SendExtension's own
				// write-deadline-exceeded case) would be indistinguishable from this function's
				// own benign silence-until-deadline timeout outcome (nil error) to any caller
				// checking errors.As(&netErr) && netErr.Timeout().
				wrapped := sendStageError{err: err}
				slog.Error("login.init send failed", "error", wrapped)
				return nil, nil, false, wrapped
			}
		}

		// Before the active pull has been sent, cap this read's deadline at the halfway point
		// (not the full remaining window) so a totally silent server still interrupts the read
		// in time to re-check the halfway condition above and actually send login.init -- a
		// single read deadlined at the full window would time the whole function out first, and
		// the active-pull fallback this exists for would never fire.
		readUntil := deadline
		if !sentActivePull && halfway.Before(readUntil) {
			readUntil = halfway
		}
		conn.conn.SetReadDeadline(readUntil)
		env, err := conn.ReadEnvelope()
		if err != nil {
			var netErr net.Error
			isTimeout := errors.As(err, &netErr) && netErr.Timeout()
			if isTimeout && !sentActivePull && time.Now().Before(deadline) {
				// This read was deliberately capped at the halfway point, not the real
				// deadline -- a timeout here just means "time to send the active pull," not
				// "give up."
				continue
			}
			if isTimeout {
				// Genuine silence-until-deadline: the expected, unremarkable outcome when the
				// server just never sends `init`. No error to report.
				return nil, nil, false, nil
			}
			// Round-48 fix: only a genuine, non-timeout net.Error (ReadPacket's own I/O
			// failures, wrapped in deadConnError -- see conn.go) is treated as a real
			// connection failure worth aborting on. A plain DecodeObject parse error (wrapped
			// only via fmt.Errorf, never a net.Error) means ReadPacket already fully consumed
			// this frame's bytes off the wire before DecodeObject ever ran -- the stream stays
			// in sync, exactly the same reasoning buildings.go's own containsNonTimeoutNetError
			// callers use to classify this shape of error as non-fatal. Previously ANY
			// non-timeout error here -- including a single malformed/unrecognized push -- was
			// treated identically to a genuine dead connection and aborted the entire login,
			// unlike every other unrecognized push in this same loop, which is simply skipped.
			if containsNonTimeoutNetError(err) {
				// A real connection failure, not a deadline -- surface it so the caller can
				// log what actually went wrong instead of a generic timeout message.
				return nil, nil, false, err
			}
			slog.Warn("waitForInitPush: failed to read/decode a push while waiting for init; continuing to wait, not treating this as a dead connection", "error", err)
			continue
		}
		msg, ok := env.AsExtension()
		if !ok {
			continue
		}
		if msg.Cmd == "init" {
			return dedupeBuildings(ParseInitBuildings(msg.Params)), dedupeVisitors(ParseInitVisitors(msg.Params)), true, nil
		}
		slog.Debug("skipped push while waiting for init", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
	}
}

// dedupeBuildings drops repeated-uuid entries from bs, keeping only the first occurrence of each --
// the same per-uuid dedup semantics buildings.go's FetchBuildings has applied via its
// seenBuildingUUIDs/appendBuilding closures since round 12 (see that function's doc comments for the
// full rationale: a building uuid appearing more than once in a single init push would otherwise
// cause CollectAll to issue a real, redundant building.production.collect network request for the
// same uuid twice).
//
// waitForInitPush -- not FetchBuildings -- is the PRIMARY init-push path: Login() calls it directly,
// and FetchBuildings is only a fallback reached when this path's result comes back empty. Before
// round 26, waitForInitPush returned ParseInitBuildings' raw output with no deduplication at all, so
// the primary path lacked the protection the fallback path has had since round 12. This closes that
// gap without touching buildings.go's own closures, which live in a different function and are out
// of scope for this fix.
//
// Round-48 fix: also stops appending once maxAggregateBuildingsPerFetch (buildings.go) valid,
// distinct-uuid entries have been kept, mirroring appendBuilding's own identical aggregate cap on
// FetchBuildings' fallback path -- previously this, the PRIMARY path, had no aggregate cap of its
// own at all, only ParseInitBuildings' much larger maxRawBuildingItemsPerPush (2000) raw-item-SCAN
// ceiling. Every downstream consumer of the resulting slice -- PrintBuildings (buildings.go), which
// issues one uncapped Raw.StringRedacted() format call per entry with no per-call budget of its
// own, and CollectAll -- inherited that same unbounded-relative-to-input cost from a single crafted
// init push.
func dedupeBuildings(bs []Building) []Building {
	var out []Building
	seen := make(map[int64]bool, len(bs))
	truncated := false
	for _, b := range bs {
		if len(out) >= maxAggregateBuildingsPerFetch {
			truncated = true
			break
		}
		uuid := b.Uuid()
		if seen[uuid] {
			continue
		}
		seen[uuid] = true
		out = append(out, b)
	}
	if truncated {
		slog.Warn("init push building_new longer than aggregate cap after dedup; truncating",
			"rawCount", len(bs), "cap", maxAggregateBuildingsPerFetch)
	}
	return out
}

// dedupeVisitors is dedupeBuildings' sibling for Visitor.Uid -- see that function's doc comment for
// the full rationale (buildings.go's seenVisitorUUIDs/appendVisitor has applied the identical
// protection to FetchBuildings' fallback path since round 12; GreetVisitors issues one real
// visitor.operate network call per slice entry with no dedup of its own, so a doubled visitor list
// here means a doubled real network call per uid, round 26). Round-48 fix: also caps the output at
// maxVisitorsUpperBound (visitors.go), mirroring dedupeBuildings' own identical round-48 fix and
// appendVisitor's aggregate cap on FetchBuildings' fallback path.
func dedupeVisitors(vs []Visitor) []Visitor {
	var out []Visitor
	seen := make(map[int64]bool, len(vs))
	truncated := false
	for _, v := range vs {
		if len(out) >= maxVisitorsUpperBound {
			truncated = true
			break
		}
		uid := v.Uid()
		if seen[uid] {
			continue
		}
		seen[uid] = true
		out = append(out, v)
	}
	if truncated {
		slog.Warn("init push visitor.list longer than aggregate cap after dedup; truncating",
			"rawCount", len(vs), "cap", maxVisitorsUpperBound)
	}
	return out
}

// deadlineExceededError is waitFor's own wall-clock-deadline-elapsed outcome: the loop read at
// least one envelope (none of them matched pred), and the overall timeout ran out before another
// one arrived. It satisfies net.Error with Timeout()==true so callers built on the "sendAndWait's
// ordinary timeout outcome IS ITSELF a net.Error with Timeout()==true" premise (buildings.go,
// mail.go, visitors.go, alliance.go, interactive.go -- see their errors.As-against-net.Error
// checks) treat this exit the same benign way they already treat the OTHER exit from this
// function: a genuine per-read SetReadDeadline+ReadEnvelope timeout, which returns a real
// net.Error from the network layer itself. Before this type existed, this branch returned a bare
// fmt.Errorf, which is not a net.Error at all -- indistinguishable from a genuine dead-connection
// failure to every one of those callers' errors.As checks, even though it's exactly as benign as
// the per-read-timeout exit right below it in this same function.
type deadlineExceededError struct{}

func (deadlineExceededError) Error() string   { return "timed out waiting for matching envelope" }
func (deadlineExceededError) Timeout() bool   { return true }
func (deadlineExceededError) Temporary() bool { return false }

// waitFor reads envelopes until pred matches or timeout elapses, logging
// everything it skips past along the way.
func waitFor(conn *GameConn, timeout time.Duration, pred func(*Envelope) bool) (*Envelope, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, deadlineExceededError{}
		}
		conn.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := conn.ReadEnvelope()
		if err != nil {
			return nil, err
		}
		if pred(env) {
			return env, nil
		}
		if msg, ok := env.AsExtension(); ok {
			slog.Debug("skipped push while waiting", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
		}
	}
}

// waitForCmd waits for an extension message whose cmd matches any of wantCmds.
func waitForCmd(conn *GameConn, timeout time.Duration, wantCmds ...string) (*ExtensionMessage, error) {
	env, err := waitFor(conn, timeout, func(e *Envelope) bool {
		msg, ok := e.AsExtension()
		if !ok {
			return false
		}
		return slices.Contains(wantCmds, msg.Cmd)
	})
	if err != nil {
		return nil, err
	}
	msg, _ := env.AsExtension()
	return msg, nil
}

func readCodeFromStdin(conn *GameConn) string {
	return readCodeFrom(os.Stdin, nil, conn)
}

// readCodeFromPipe opens a FIFO for reading -- this blocks until a writer
// opens the other end, which is exactly what we want: the process can sit
// here idle (heartbeat still running in the background) until the code is
// written to the pipe from a separate shell command.
//
// conn is the live, already-dialed GameConn Login() has open at this point (step 7, waiting on the
// verification code) -- see closeConnBeforeExit's own doc comment for why every os.Exit(1) site
// below now closes it first.
func readCodeFromPipe(path string, conn *GameConn) string {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		slog.Error("stat code pipe failed", "codePipe", path, "error", statErr)
		closeConnBeforeExit(conn)
		os.Exit(1)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		slog.Error("codePipe exists but is not a FIFO -- did you forget mkfifo?", "codePipe", path)
		closeConnBeforeExit(conn)
		os.Exit(1)
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Error("open code pipe", "path", path, "error", err)
		closeConnBeforeExit(conn)
		os.Exit(1)
	}
	defer f.Close()
	return readCodeFrom(f, f, conn)
}

// closeConnBeforeExit calls conn.Close() before an imminent os.Exit(1), tolerating a nil conn (the
// shape readCodeFrom's own direct unit tests use, since they never actually reach an os.Exit(1)
// branch -- see login_test.go).
//
// Round-41 fix: readCodeFromPipe/readCodeFrom's os.Exit(1) calls used to fire with no conn
// awareness at all, even though Login() has a live, already-dialed, heartbeating GameConn in
// scope at their one call site (step 7) and otherwise closes it explicitly on every other return
// path in the same function (17+ separate conn.Close() call sites) -- the identical defer-skipped-
// cleanup gap round 40 fixed in main.go/interactive.go, here reached through several stack frames
// instead of directly at the exit site.
func closeConnBeforeExit(conn *GameConn) {
	if conn != nil {
		conn.Close()
	}
}

// maxCodePipeLineSize bounds how much readCodeFrom will ever read from stdin/-code-pipe,
// mirroring interactive.go's maxControlPipeLineSize for the identical reason: an unbounded
// accumulating read would otherwise let a misbehaving writer (a broken -code-pipe script, or
// literal megabytes piped into stdin with no newline) force unbounded memory growth. A real
// verification code is a handful of characters, so this is generous headroom, not a guess at an
// actual protocol limit -- see maxControlPipeLineSize's own doc comment for the sibling case this
// mirrors.
const maxCodePipeLineSize = 64 * 1024

// readCodeFrom reads one non-blank line from r, trimmed of surrounding whitespace, blocking
// (retrying blank lines) until it gets one or the input closes.
//
// closer, if non-nil, is explicitly closed before this function's own os.Exit(1) call below --
// round-45 fix, closing the same defer-skipped-cleanup gap round 40/41 fixed for conn (see
// closeConnBeforeExit's own doc comment) but for the FIFO handle itself: readCodeFromPipe's own
// `defer f.Close()` runs in ITS stack frame, and readCodeFrom executes synchronously inside that
// same frame (not detached), so os.Exit(1) -- which skips every deferred function in the whole
// process -- also skipped readCodeFromPipe's deferred f.Close() whenever readCodeFrom reached this
// branch. readCodeFromStdin passes nil here: os.Stdin must never be closed. Practical impact was
// always minimal (os.Exit ends the process and the OS reclaims the fd regardless), but this closes
// the inconsistency with every other explicit-close-before-exit site in this file.
//
// Round-38 fix: previously used a bare bufio.Reader.ReadString('\n') with no size bound at all
// (the unbounded-memory-growth gap above) and, separately, silently discarded ReadString's own
// error whenever the returned line was non-empty -- indistinguishable, with zero diagnostic, from
// a normal newline-terminated read. ReadString returns the accumulated partial line PLUS a
// non-nil error (typically io.EOF) whenever the input closes before a '\n' is ever seen, so a
// killed/crashed -code-pipe writer that emitted only a few characters of a real code before dying
// was silently accepted as if it were the complete, intended code -- the resulting server
// rejection then surfaced as an opaque ErrAuthRejected with no hint the real cause was a
// truncated local read, not a wrong/expired code. This can't be fixed by rejecting an
// EOF-without-newline read outright, since that's also the normal shape of a legitimate final
// line with no trailing newline (e.g. `printf '123456'` into -code-pipe, or Ctrl+D right after
// typing the code) -- so this now warns instead, giving an operator debugging a rejection a
// concrete signal to check for truncation without breaking the legitimate no-trailing-newline case.
func readCodeFrom(r io.Reader, closer io.Closer, conn *GameConn) string {
	reader := bufio.NewReader(io.LimitReader(r, maxCodePipeLineSize))
	for {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			slog.Error("input closed without a code", "error", err)
			if closer != nil {
				closer.Close()
			}
			closeConnBeforeExit(conn)
			os.Exit(1)
		}
		code := strings.TrimSpace(line)
		if code != "" {
			if err != nil {
				slog.Warn("code read closed without seeing a trailing newline -- accepting it as-is, but this can also mean a truncated write from a killed/crashed writer or the size cap being hit; double check the code below if login fails",
					"codeLen", len(code), "cap", maxCodePipeLineSize, "error", err)
			}
			return code
		}
	}
}
