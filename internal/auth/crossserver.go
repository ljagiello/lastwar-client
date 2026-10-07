package auth

import (
	"errors"
	"fmt"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"log/slog"
	"os"
	"slices"
	"time"
)

// CrossServerLoginResult is the outcome of reconnecting to a specific
// role/server picked from an account.login.new response's `accountArr`.
type CrossServerLoginResult struct {
	Conn    *session.GameConn
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

	// AccessTok is the access token actually used to log in. A serverInfo redirect reuses the
	// same token, as the real client does, so today this always equals
	// CrossServerLoginParams.AccessTok; callers persisting a session config save this field.
	AccessTok string

	// GameUid is the FINAL gameUid actually logged in with -- this differs
	// from CrossServerLoginParams.GameUid whenever a serverInfo redirect was
	// followed and carried a different serverInfo.uid. Callers that persist
	// connection details (e.g. a session config file) should save this, not the
	// original input, so the next run targets the role's current gameUid
	// instead of a stale one.
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
	AppVersion  string // build the token was issued under, if known; see LoginParamsInput.AppVersion
	VersionCode string
	ResVersion  string // see LoginParamsInput.ResVersion and ResVersionFor; "" sends "0"

	// DialGame, if non-nil, replaces the package-level dialGame (ultimately session.DialGame) used
	// to open the game socket -- and is used for every redial in the serverInfo-redirect loop, not
	// just the first dial. It exists purely so a test can substitute an in-memory net.Pipe transport
	// for the real TCP dial, which is what lets the whole login round-trip run deterministically under
	// virtual time inside a testing/synctest bubble (a real socket blocks in the netpoller, which
	// synctest cannot virtualize). Production always leaves this nil.
	DialGame func(addr string, timeout time.Duration) (*session.GameConn, error)
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
//
// The redirect is followed the way the real client follows it
// (LoginMessage.CSHandleResponse, A-CS:82799-82858): serverInfo.uid replaces
// the gameUid, and the redial reuses the same access token with no GSL call
// in between. This function used to fetch a "fresh" token via GSL opt=fix
// first, on a suspicion that tokens were single-use per connection; the real
// client never does that, and since that GSL call claims platform=Android it
// could only swap an iOS-issued token for an incompatible one.
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
	addrs, err := baseZoneLoginAddrs(p.IP, p.Port)
	if err != nil {
		return nil, fmt.Errorf("cross-server login: %w", err)
	}
	var addr string
	zone := p.Zone

	for hop := 0; ; hop++ {
		if hop > 0 {
			if hop > maxRedirects {
				return nil, fmt.Errorf("cross-server login: too many serverInfo redirects (>%d), last addr=%s zone=%s", maxRedirects, addr, zone)
			}
			slog.Info("cross-server login: following serverInfo redirect", "hop", hop, "addr", addr, "zone", zone)
		}
		slog.Info("cross-server login: dialing directly (no GSL call)", "addr", addrs[0], "fallbacks", len(addrs)-1)
		dial := dialGame
		if p.DialGame != nil {
			dial = p.DialGame
		}
		conn, dialed, err := dialFirst(dial, addrs)
		if err != nil {
			return nil, err
		}
		addr = dialed
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
			AppVersion:  p.AppVersion,
			VersionCode: p.VersionCode,
			ResVersion:  p.ResVersion,
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
			outer.PutByte("c", session.ControllerSystem)
			outer.PutShort("a", session.ActionLogin)
			outer.PutSFSObject("p", loginContent)
			// 0600, not 0644 -- this dump includes p.at (the live access token), same
			// sensitivity as the session config file (see config.go's SaveSessionConfig).
			// Written via AtomicWriteStateFile (identity.go: temp-file-in-same-dir, fsync,
			// chmod 0600, then rename) rather than a plain os.WriteFile+os.Chmod -- that older
			// pattern left a torn-write window (truncate-then-write as separate syscalls, no
			// fsync) and, on a pre-existing target left behind at 0644 by some other process,
			// briefly published the freshly-written access token at that looser mode before the
			// follow-up Chmod caught up. AtomicWriteStateFile is what config.go's
			// SaveSessionConfig and identity.go's saveStateFile themselves now use for exactly
			// this reason.
			if encoded, err := sfs.EncodeObject(outer); err != nil {
				// Debug-only path -- don't fail the actual login over a failed debug dump.
				slog.Error("failed to encode login body debug dump", "path", f, "error", err)
			} else if err := AtomicWriteStateFile(f, string(encoded)); err != nil {
				slog.Error("failed to write login body debug dump", "path", f, "error", err)
			}
		}
		if err := conn.SendEnvelope(session.ControllerSystem, session.ActionLogin, loginContent); err != nil {
			_ = conn.Close()
			return nil, session.SendStageError{Err: err}
		}
		slog.Info("login request sent, waiting for response",
			"gameUid", p.GameUid, "zone", zone, "accessTok", sfs.Redact(p.AccessTok), "shumeiBoxId", sfs.Redact(p.ShumeiBoxId))

		env, err := session.WaitFor(conn, 15*time.Second, func(e *session.Envelope) bool {
			return e.Controller == session.ControllerSystem && e.Action == session.ActionLogin
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
			cause := session.ErrAuthRejected
			ep, _ := env.Content.Get("ep")
			if codes, _ := ep.Val.([]string); slices.Contains(codes, "E011") {
				cause = session.ErrTokenRejected
			}
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: ec=%v full=%s: %w", ec.Val, env.Content.StringRedacted(), cause)
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
			newAddrs, err := baseZoneLoginAddrs(redirectIPVal, int(getIntFlexible(siObj, "port")))
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
			// serverInfo.uid is the account's gameUid on the new shard: it becomes `un` and goes
			// into SecurityCode on the redialed Login. Absent or empty keeps the current one.
			newGameUid := redirectUid(siObj, "crossserver.go cross-server Login")
			slog.Info("serverInfo redirect: reconnecting to new address with the same access token", "newAddr", newAddrs[0], "newZone", newZone,
				"oldAddr", addr, "oldZone", zone, "newGameUid", newGameUid, "oldGameUid", p.GameUid, "accessTokLen", len(p.AccessTok))

			_ = conn.Close()
			addrs = newAddrs
			if newZone != "" {
				zone = newZone
			}
			if newGameUid != "" {
				p.GameUid = newGameUid
			}
			continue
		}

		_ = conn.SetReadDeadline(time.Time{})
		return &CrossServerLoginResult{Conn: conn, Content: env.Content, Addr: addr, Zone: zone, AccessTok: p.AccessTok, GameUid: p.GameUid}, nil
	}
}

// dialFirst dials addrs in order and returns the first connection that succeeds, along with its
// address, logging each failure. The real client races every gateway at once and keeps the first to
// answer its a=29 ping; trying them in turn is the simpler fallback with the same effect when one is
// unreachable. A single-address list fails with that dial's own error, unwrapped.
func dialFirst(dial func(addr string, timeout time.Duration) (*session.GameConn, error), addrs []string) (*session.GameConn, string, error) {
	var errs []error
	for _, addr := range addrs {
		conn, err := dial(addr, 10*time.Second)
		if err == nil {
			return conn, addr, nil
		}
		if len(addrs) > 1 {
			slog.Warn("cross-server login: gateway dial failed", "addr", addr, "error", err)
		}
		errs = append(errs, err)
	}
	if len(errs) == 1 {
		return nil, "", errs[0]
	}
	return nil, "", errors.Join(errs...)
}

func serverIDFromZone(zone string) string {
	id := zone
	if len(id) > 3 && id[:3] == "APS" {
		id = id[3:]
	}
	return id
}
