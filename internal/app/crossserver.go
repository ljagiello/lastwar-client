package app

import (
	"crypto/rsa"
	"fmt"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// CrossServerLoginResult is the outcome of reconnecting to a specific
// role/server picked from an account.login.new response's `accountArr`.
type CrossServerLoginResult struct {
	Conn    *GameConn
	Content *sfs.SFSObject // the base zone Login response

	// Addr/Zone are the FINAL address/zone actually connected to -- these
	// differ from CrossServerLoginParams' IP/Port/Zone whenever a
	// serverInfo redirect was followed (see the doc comment on
	// DoCrossServerLogin). Callers that persist connection details (e.g.
	// a session config file) should save these, not the original inputs,
	// so the next run connects directly instead of re-following the same
	// redirect every time.
	Addr string
	Zone string

	// AccessTok is the FINAL access token actually used to log in -- this differs
	// from CrossServerLoginParams.AccessTok whenever a serverInfo redirect was
	// followed and the mid-redirect GSL refresh (see below) obtained a new one.
	// Callers that persist connection details (e.g. a session config file) should
	// save this, not the original input, so the next run doesn't retry a token
	// this connection already knows was superseded.
	AccessTok string

	// GameUid is the FINAL gameUid actually logged in with -- this differs
	// from CrossServerLoginParams.GameUid whenever a serverInfo redirect was
	// followed and the mid-redirect GSL refresh (see below) returned a
	// changed gameUid. Callers that persist connection details (e.g. a
	// session config file) should save this, not the original input, so the
	// next run targets the role's current gameUid instead of a stale one.
	GameUid string
}

// String/GoString are the round-48 regression fix for the MINOR finding that
// CrossServerLoginResult -- which carries AccessTok, a live credential -- had no
// redaction-by-construction, the same class of gap round 47/48 closed for
// gsl.LoginToken/deviceIdentity/SessionConfig. No current call site logs a *CrossServerLoginResult
// directly, so this is defense-in-depth, not an active leak fix.
func (r CrossServerLoginResult) String() string   { return "[REDACTED CrossServerLoginResult]" }
func (r CrossServerLoginResult) GoString() string { return r.String() }

// LogValue makes CrossServerLoginResult satisfy slog.LogValuer -- round-53 fix, the same gap
// String()/GoString() alone leaves open for every credential-bearing type in this codebase: those
// two methods are invisible to slog.NewJSONHandler (the only handler main.go ever installs),
// since encoding/json never consults fmt.Stringer/fmt.GoStringer, only slog.LogValuer. See
// config.go's SessionConfig.LogValue for the full rationale.
func (r CrossServerLoginResult) LogValue() slog.Value { return slog.StringValue(r.String()) }

// CrossServerLoginParams mirrors the fields UIRoleLoginView:OnClickLogin
// pulls off a role entry (ip/port/zone/gameUid) plus the deviceId/airKey
// this device already presented.
type CrossServerLoginParams struct {
	IP          string // pipe-delimited fallback hosts, same shape as GSL server entries
	Port        int
	Zone        string
	GameUid     string
	DeviceID    string
	AirKey      string
	AccessTok   string // included as p.at -- confirmed live this IS required, see below
	ShumeiBoxId string // real anti-fraud device fingerprint, if known
	Handshake   bool   // experimental: send the vanilla SFS2X pre-Login Handshake (see conn.go:DoHandshake)
	IOSMode     bool   // send an iOS-flavored identity instead of Android; see LoginParamsInput.IOSMode

	// HTTPClient/RSAPub/GateHost are OPTIONAL GSL plumbing, needed only to
	// refresh AccessTok via gsl.GetServerList(opt=fix) if a serverInfo redirect
	// is hit mid-login (see the doc comment on DoCrossServerLogin). Callers
	// that already have these in scope from their own gsl.CheckVersion() call
	// (e.g. main.go's runCrossServerTest) should pass them through; callers
	// that don't leave them nil/zero and DoCrossServerLogin degrades to
	// reusing AccessTok unrefreshed across the redial, with a logged
	// warning at the point that happens.
	HTTPClient *http.Client
	RSAPub     *rsa.PublicKey
	GateHost   string
}

// String/GoString are CrossServerLoginResult's sibling for CrossServerLoginParams -- which
// carries AccessTok/GameUid/ShumeiBoxId, live credentials -- the same round-48 fix. Every current
// call site already logs individually-redacted fields (sfs.Redact(p.AccessTok)/sfs.Redact(p.ShumeiBoxId)),
// not the struct directly, so this is defense-in-depth, not an active leak fix.
func (p CrossServerLoginParams) String() string   { return "[REDACTED CrossServerLoginParams]" }
func (p CrossServerLoginParams) GoString() string { return p.String() }

// LogValue makes CrossServerLoginParams satisfy slog.LogValuer -- see CrossServerLoginResult's
// identical round-53 fix above for the full rationale.
func (p CrossServerLoginParams) LogValue() slog.Value { return slog.StringValue(p.String()) }

// DoCrossServerLogin reimplements the client's CrossServerLogin FSM state
// (Assembly-CSharp.decompiled.cs:108752-108812): UIRoleLoginView.OnClickLogin
// sets AccountCredentialManager's server/zone/uid directly from the picked
// accountArr entry -- WITHOUT another GSL HTTP round trip -- then dials that
// server and sends the same base SFS `Login` (un=gameUid, zn=zone, pw="")
// used everywhere else.
//
// The E005/E011 rejections seen throughout this project were NOT caused by
// including `p.at`, or by `un` being non-empty, or by any reconnect being
// inherently blocked -- all previously live-tested and ruled out. Root
// cause, confirmed by a byte-for-byte replay of a real client's captured
// Login packet succeeding where our own serialization (same account, same
// token) failed: `at` is bound to the PackageName/Platform it was issued
// for, and this client always claimed Android while testing tokens that
// happened to be obtained by a real iOS session. `p.at` must be INCLUDED
// (a missing/empty token gets ec=28/E011 outright), and the Platform
// fields must match whatever identity actually obtained the token
// (IOSMode) -- see identity.go's BuildLoginParams.
//
// Bug fixed here: this function used to accept the login response
// unconditionally and hand back a connection, even when that response
// carried a `serverInfo` shard-redirect (the same field login.go's
// waitForInitPush already checked for, on the *other* login path). Real
// symptom, confirmed live: a session config captured against zone APS783
// kept "successfully" logging in weeks later, but every single subsequent
// command timed out -- push.init.build never arrived, nor did responses
// to any hand-sent command -- because the account's zone had actually been
// migrated server-side (a live game server merge) to a new zone/host/port
// (observed live: APS783 -> APS8092, entirely different IP/port), and the
// old connection was talking to a shard that no longer serves this
// account at all. The server's own Login response says so directly, in
// `serverInfo{ip,port,zone}` -- this just wasn't being read. Now it
// follows the redirect (closes the stale connection, redials the new
// address, resends Login with the new zone) up to a small bounded number
// of hops rather than treating the first response as final.
func DoCrossServerLogin(p CrossServerLoginParams) (*CrossServerLoginResult, error) {
	if p.AccessTok == "" {
		return nil, fmt.Errorf("cross-server login: no access token given (pass -cs-at, -cs-rt, or a session config with accessToken) -- an empty token reliably fails with ec=28/E011")
	}
	// Round-47 fix: p.Zone/p.GameUid/p.AccessTok are re-encoded verbatim via PutUtfString on every
	// hop of the loop below (zn/un/p.at), but -- unlike loginKey/gameUid/username, which route
	// through SaveLoginKey/SaveGameUid/SaveUsername and got a maxIdentityFieldLen guard in round
	// 46 -- nothing previously capped these fields' length here, and every current caller sources
	// them from an unguarded gsl.go gsl.FlexString field (main.go's -cs-rt refresh flow) or an
	// unguarded SFS2X serverInfo redirect (see capOversizedIdentityField's doc comment, login.go).
	// Rejecting synchronously here, before any connection is even dialed, is strictly better than
	// letting an oversized value fail deep inside SendEnvelope's encode step, where sendStageError
	// (conn.go) deliberately, by design, makes that local encode failure indistinguishable from a
	// genuine dead connection.
	if len(p.Zone) > maxIdentityFieldLen {
		return nil, fmt.Errorf("cross-server login: zone too long (%d bytes, max %d)", len(p.Zone), maxIdentityFieldLen)
	}
	if len(p.GameUid) > maxIdentityFieldLen {
		return nil, fmt.Errorf("cross-server login: gameUid too long (%d bytes, max %d)", len(p.GameUid), maxIdentityFieldLen)
	}
	if len(p.AccessTok) > maxIdentityFieldLen {
		return nil, fmt.Errorf("cross-server login: accessTok too long (%d bytes, max %d)", len(p.AccessTok), maxIdentityFieldLen)
	}

	const maxRedirects = 3
	// buildBaseZoneLoginAddr (login.go) is the same helper the redirect branch below and both of
	// login.go's Login() call sites use -- it rejects an empty host or non-positive port with a
	// clear error instead of silently building a "host:0" or ":<port>"-shaped address that Go's
	// "host:port" dial syntax would treat as the loopback interface. main.go's runCrossServerTest
	// happens to pre-validate both of these before calling DoCrossServerLogin today, but this is
	// an exported, reusable function in its own right (see the doc comment above) -- it must not
	// rely on a specific caller's external guards to avoid a silent loopback dial.
	addr, err := buildBaseZoneLoginAddr(p.IP, p.Port)
	if err != nil {
		return nil, fmt.Errorf("cross-server login: %w", err)
	}
	zone := p.Zone

	for hop := 0; ; hop++ {
		if hop > 0 {
			if hop > maxRedirects {
				return nil, fmt.Errorf("cross-server login: too many serverInfo redirects (>%d), last addr=%s zone=%s", maxRedirects, addr, zone)
			}
			slog.Info("cross-server login: following serverInfo redirect", "hop", hop, "addr", addr, "zone", zone)
		}
		slog.Info("cross-server login: dialing directly (no GSL call)", "addr", addr)
		conn, err := dialGame(addr, 10*time.Second)
		if err != nil {
			return nil, err
		}
		conn.StartHeartbeat(4*time.Second, time.Now())
		slog.Info("connected")

		if p.Handshake {
			slog.Info("SFS2X handshake (experimental)")
			hsResp, err := conn.DoHandshake(10 * time.Second)
			if err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("handshake: %w", err)
			}
			slog.Info("handshake OK", "response", hsResp.StringRedacted())
		}

		loginParams := BuildLoginParams(LoginParamsInput{
			FutureID:    1,
			DeviceID:    p.DeviceID,
			AirKey:      p.AirKey,
			GameUid:     p.GameUid,
			AccessTok:   p.AccessTok,
			ServerID:    serverIDFromZone(zone),
			ShumeiBoxId: p.ShumeiBoxId,
			IOSMode:     p.IOSMode,
		})
		loginContent := sfs.NewSFSObject()
		loginContent.PutUtfString("zn", zone)
		loginContent.PutUtfString("un", p.GameUid)
		loginContent.PutUtfString("pw", "")
		loginContent.PutSFSObject("p", loginParams)
		if os.Getenv("LWDEBUG_DUMP_LOGIN") != "" {
			// Redacted, not a raw dump -- loginContent's nested "p" object carries the live
			// access token (p.at) and shumeiBoxId in cleartext (see identity.go's
			// BuildLoginParams), the same sensitivity LWDEBUG_DUMP_LOGIN_BODY already treats
			// them with below and the "login request sent" log a few lines down already
			// redacts them with.
			slog.Debug("full login content", "content", loginContent.StringRedacted())
		}
		if f := os.Getenv("LWDEBUG_DUMP_LOGIN_BODY"); f != "" {
			outer := sfs.NewSFSObject()
			outer.PutByte("c", controllerSystem)
			outer.PutShort("a", actionLogin)
			outer.PutSFSObject("p", loginContent)
			// 0600, not 0644 -- this dump includes p.at (the live access token), same
			// sensitivity as the session config file (see config.go's SaveSessionConfig).
			// Written via atomicWriteStateFile (identity.go: temp-file-in-same-dir, fsync,
			// chmod 0600, then rename) rather than a plain os.WriteFile+os.Chmod -- that older
			// pattern left a torn-write window (truncate-then-write as separate syscalls, no
			// fsync) and, on a pre-existing target left behind at 0644 by some other process,
			// briefly published the freshly-written access token at that looser mode before the
			// follow-up Chmod caught up. atomicWriteStateFile is what config.go's
			// SaveSessionConfig and identity.go's saveStateFile themselves now use for exactly
			// this reason.
			if encoded, err := sfs.EncodeObject(outer); err != nil {
				// Debug-only path -- don't fail the actual login over a failed debug dump.
				slog.Error("failed to encode login body debug dump", "path", f, "error", err)
			} else if err := atomicWriteStateFile(f, string(encoded)); err != nil {
				slog.Error("failed to write login body debug dump", "path", f, "error", err)
			}
		}
		if err := conn.SendEnvelope(controllerSystem, actionLogin, loginContent); err != nil {
			_ = conn.Close()
			return nil, sendStageError{err: err}
		}
		slog.Info("login request sent, waiting for response",
			"gameUid", p.GameUid, "zone", zone, "accessTok", sfs.Redact(p.AccessTok), "shumeiBoxId", sfs.Redact(p.ShumeiBoxId))

		env, err := waitFor(conn, 15*time.Second, func(e *Envelope) bool {
			return e.Controller == controllerSystem && e.Action == actionLogin
		})
		if err != nil {
			_ = conn.Close()
			return nil, err
		}
		if env.Content == nil {
			_ = conn.Close()
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: response had no p payload")
		}
		if ec, ok := env.Content.Get("ec"); ok {
			_ = conn.Close()
			// Wrapped in ErrAuthRejected (defined in errors.go) so callers can
			// distinguish "server actively rejected this login" (ec present) from
			// a bare dial/timeout/I/O failure above, which stay unwrapped.
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: ec=%v full=%s: %w", ec.Val, env.Content.StringRedacted(), ErrAuthRejected)
		}
		slog.Info("login OK")

		siObj := gsl.FindServerInfo(env.Content)
		redirectIPVal := ""
		if siObj != nil {
			redirectIPVal = redirectIP(siObj, "crossserver.go cross-server Login")
		}
		if siObj != nil && redirectIPVal != "" {
			// redirectIP (login.go) distinguishes a present-but-wrong-typed ip from a genuinely
			// absent one, logging a Warn for the former -- see its doc comment for why that gap
			// (only port was hardened via getIntFlexible, not ip) was a real, non-theoretical risk.
			// buildBaseZoneLoginAddr (login.go) guards against an empty resolved host --
			// same "serverInfo" redirect shape and same gap login.go's Login() had until
			// round 18: only siObj.GetString("ip") != "" was checked above, which doesn't
			// catch inputs like "" or "|1.2.3.4" or a bare "|" that gsl.FirstHost resolves down
			// to an empty host. An unguarded fmt.Sprintf("%s:%d", "", port) wouldn't fail --
			// Go's "host:port" dial syntax treats an empty host as the loopback interface,
			// so this would silently redial 127.0.0.1/::1 instead of erroring clearly.
			newAddr, err := buildBaseZoneLoginAddr(redirectIPVal, int(getIntFlexible(siObj, "port")))
			if err != nil {
				_ = conn.Close()
				return nil, fmt.Errorf("cross-server login: serverInfo redirect: %w", err)
			}
			// redirectZone (login.go) is redirectIP's sibling for this field -- see its doc
			// comment for why a wrong-typed zone is a real, non-theoretical desync risk even
			// though (unlike a wrong-typed ip) it doesn't stop the redirect itself from being
			// followed: ip/port can still resolve fine on their own, so this would otherwise
			// silently redial to the new address while keeping the stale zone.
			newZone := redirectZone(siObj, "crossserver.go cross-server Login")
			slog.Info("serverInfo redirect: reconnecting to new address", "newAddr", newAddr, "newZone", newZone, "oldAddr", addr, "oldZone", zone)

			// Before closing this connection and redialing, refresh AccessTok via GSL --
			// mirroring login.go's equivalent redirect path, on the same documented
			// suspicion that a token is single-use-per-connection. Only possible when the
			// caller supplied HTTPClient/RSAPub/GateHost (DoCrossServerLogin is otherwise
			// deliberately GSL-free, see the doc comment above); callers that don't fall
			// back to reusing p.AccessTok unrefreshed, logged loudly below so an ec=28/E011
			// failure right after this redial is immediately diagnosable.
			if p.HTTPClient != nil && p.RSAPub != nil && p.GateHost != "" {
				slog.Info("fetching fresh access token before following serverInfo redirect (suspected single-use-per-connection)")
				freshLsr, err := gsl.GetServerList(p.HTTPClient, p.GateHost, p.RSAPub, p.DeviceID, gsl.GSLOpt{Opt: "fix"}, "", p.GameUid)
				if err != nil {
					slog.Error("GSL refresh failed; following redirect with stale access token anyway", "error", err)
				} else {
					// Only overwrite on a non-empty refreshed token -- mirrors the gameUid guard
					// just below (same reasoning, and the identical round-53 fix login.go's
					// matching redirect path also got): freshLsr.At can be non-nil with an empty
					// Token (gsl.go's gsl.LoginServerListRespon.UnmarshalJSON treats any
					// JSON-object-shaped "at" field, including "{}" or one with no/empty
					// "token", as present via gsl.LooksLikeJSONObject), and an empty token here is
					// more likely an unpopulated/degraded refresh response than a real
					// "clear the token" instruction. Clobbering the caller-supplied, already-
					// working p.AccessTok with "" would break the very redial this refresh
					// exists to support.
					if freshLsr.At != nil {
						if newAccessTok := capOversizedIdentityField("accessTok", freshLsr.At.Token.String(), p.AccessTok, "cross-server login serverInfo redirect GSL refresh"); newAccessTok != "" {
							p.AccessTok = newAccessTok
							slog.Info("fresh access token acquired", "tokenLen", len(p.AccessTok))
						} else {
							slog.Warn("serverInfo redirect GSL refresh returned an empty access token; keeping the existing one", "tokenLen", len(p.AccessTok))
						}
					}
					// The same refresh response also carries the account's current
					// gameUid (serverList[0].gameUid) -- propagate it the same way as
					// AccessTok above. Without this, p.GameUid stays pinned to whatever
					// the caller originally passed in even when the GSL refresh (issued
					// specifically because this account just got redirected to a new
					// shard) reports a different one, and the stale value is what ends up
					// folded into SecurityCode and sent as `un` on the redialed
					// connection. Only overwrite on a non-empty value -- an empty
					// gameUid here is more likely an unpopulated field than a real
					// "clear the uid" instruction, and clobbering a known-good value with
					// "" is not a safe default to guess at.
					if len(freshLsr.ServerList) > 0 {
						if newGameUid := capOversizedIdentityField("gameUid", freshLsr.ServerList[0].GameUid.String(), "", "cross-server login serverInfo redirect GSL refresh"); newGameUid != "" && newGameUid != p.GameUid {
							slog.Info("serverInfo redirect: gameUid changed on GSL refresh", "oldGameUid", p.GameUid, "newGameUid", newGameUid)
							p.GameUid = newGameUid
						}
					}
				}
			} else {
				slog.Warn("following serverInfo redirect with UNREFRESHED access token -- no HTTPClient/RSAPub/GateHost given to DoCrossServerLogin, so it cannot fetch a fresh one before redialing; if this redial fails with ec=28/E011, this reused token is the first thing to suspect")
			}

			_ = conn.Close()
			addr = newAddr
			if newZone != "" {
				zone = newZone
			}
			continue
		}

		_ = conn.conn.SetReadDeadline(time.Time{})
		return &CrossServerLoginResult{Conn: conn, Content: env.Content, Addr: addr, Zone: zone, AccessTok: p.AccessTok, GameUid: p.GameUid}, nil
	}
}

func serverIDFromZone(zone string) string {
	id := zone
	if len(id) > 3 && id[:3] == "APS" {
		id = id[3:]
	}
	return id
}
