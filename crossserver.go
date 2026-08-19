package main

import (
	"crypto/rsa"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// CrossServerLoginResult is the outcome of reconnecting to a specific
// role/server picked from an account.login.new response's `accountArr`.
type CrossServerLoginResult struct {
	Conn    *GameConn
	Content *SFSObject // the base zone Login response

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
}

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
	// refresh AccessTok via GetServerList(opt=fix) if a serverInfo redirect
	// is hit mid-login (see the doc comment on DoCrossServerLogin). Callers
	// that already have these in scope from their own CheckVersion() call
	// (e.g. main.go's runCrossServerTest) should pass them through; callers
	// that don't leave them nil/zero and DoCrossServerLogin degrades to
	// reusing AccessTok unrefreshed across the redial, with a logged
	// warning at the point that happens.
	HTTPClient *http.Client
	RSAPub     *rsa.PublicKey
	GateHost   string
}

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
// token) failed: `at` is bound to the packageName/platform it was issued
// for, and this client always claimed Android while testing tokens that
// happened to be obtained by a real iOS session. `p.at` must be INCLUDED
// (a missing/empty token gets ec=28/E011 outright), and the platform
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

	const maxRedirects = 3
	addr := fmt.Sprintf("%s:%d", firstHost(p.IP), p.Port)
	zone := p.Zone

	for hop := 0; ; hop++ {
		if hop > 0 {
			if hop > maxRedirects {
				return nil, fmt.Errorf("cross-server login: too many serverInfo redirects (>%d), last addr=%s zone=%s", maxRedirects, addr, zone)
			}
			slog.Info("cross-server login: following serverInfo redirect", "hop", hop, "addr", addr, "zone", zone)
		}
		slog.Info("cross-server login: dialing directly (no GSL call)", "addr", addr)
		conn, err := DialGame(addr, 10*time.Second)
		if err != nil {
			return nil, err
		}
		conn.StartHeartbeat(4*time.Second, time.Now())
		slog.Info("connected")

		if p.Handshake {
			slog.Info("SFS2X handshake (experimental)")
			hsResp, err := conn.DoHandshake(10 * time.Second)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("handshake: %w", err)
			}
			slog.Info("handshake OK", "response", hsResp.String())
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
		loginContent := NewSFSObject()
		loginContent.PutUtfString("zn", zone)
		loginContent.PutUtfString("un", p.GameUid)
		loginContent.PutUtfString("pw", "")
		loginContent.PutSFSObject("p", loginParams)
		if os.Getenv("LWDEBUG_DUMP_LOGIN") != "" {
			slog.Debug("full login content", "content", loginContent.String())
		}
		if f := os.Getenv("LWDEBUG_DUMP_LOGIN_BODY"); f != "" {
			outer := NewSFSObject()
			outer.PutByte("c", controllerSystem)
			outer.PutShort("a", actionLogin)
			outer.PutSFSObject("p", loginContent)
			// 0600, not 0644 -- this dump includes p.at (the live access token), same
			// sensitivity as the session config file (see config.go's SaveSessionConfig).
			if encoded, err := EncodeObject(outer); err != nil {
				// Debug-only path -- don't fail the actual login over a failed debug dump.
				slog.Error("failed to encode login body debug dump", "path", f, "error", err)
			} else if err := os.WriteFile(f, encoded, 0600); err != nil {
				slog.Error("failed to write login body debug dump", "path", f, "error", err)
			}
		}
		if err := conn.SendEnvelope(controllerSystem, actionLogin, loginContent); err != nil {
			conn.Close()
			return nil, err
		}
		slog.Info("login request sent, waiting for response",
			"gameUid", p.GameUid, "zone", zone, "accessTok", redact(p.AccessTok), "shumeiBoxId", redact(p.ShumeiBoxId))

		env, err := waitFor(conn, 15*time.Second, func(e *Envelope) bool {
			return e.Controller == controllerSystem && e.Action == actionLogin
		})
		if err != nil {
			conn.Close()
			return nil, err
		}
		if env.Content == nil {
			conn.Close()
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: response had no p payload")
		}
		if ec, ok := env.Content.Get("ec"); ok {
			conn.Close()
			// Wrapped in ErrAuthRejected (defined in errors.go) so callers can
			// distinguish "server actively rejected this login" (ec present) from
			// a bare dial/timeout/I/O failure above, which stay unwrapped.
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: ec=%v full=%s: %w", ec.Val, env.Content.String(), ErrAuthRejected)
		}
		slog.Info("login OK")

		if siObj := findServerInfo(env.Content); siObj != nil && siObj.GetString("ip") != "" {
			newAddr := fmt.Sprintf("%s:%d", firstHost(siObj.GetString("ip")), getIntFlexible(siObj, "port"))
			newZone := siObj.GetString("zone")
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
				freshLsr, err := GetServerList(p.HTTPClient, p.GateHost, p.RSAPub, p.DeviceID, GSLOpt{Opt: "fix"}, "", p.GameUid)
				if err != nil {
					slog.Error("GSL refresh failed; following redirect with stale access token anyway", "error", err)
				} else {
					if freshLsr.At != nil {
						p.AccessTok = freshLsr.At.Token
						slog.Info("fresh access token acquired", "tokenLen", len(p.AccessTok))
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
						if newGameUid := freshLsr.ServerList[0].GameUid; newGameUid != "" && newGameUid != p.GameUid {
							slog.Info("serverInfo redirect: gameUid changed on GSL refresh", "oldGameUid", p.GameUid, "newGameUid", newGameUid)
							p.GameUid = newGameUid
						}
					}
				}
			} else {
				slog.Warn("following serverInfo redirect with UNREFRESHED access token -- no HTTPClient/RSAPub/GateHost given to DoCrossServerLogin, so it cannot fetch a fresh one before redialing; if this redial fails with ec=28/E011, this reused token is the first thing to suspect")
			}

			conn.Close()
			addr = newAddr
			if newZone != "" {
				zone = newZone
			}
			continue
		}

		conn.conn.SetReadDeadline(time.Time{})
		return &CrossServerLoginResult{Conn: conn, Content: env.Content, Addr: addr, Zone: zone, AccessTok: p.AccessTok}, nil
	}
}

func serverIDFromZone(zone string) string {
	id := zone
	if len(id) > 3 && id[:3] == "APS" {
		id = id[3:]
	}
	return id
}
