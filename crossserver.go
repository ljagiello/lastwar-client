package main

import (
	"fmt"
	"log/slog"
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
			if err := os.WriteFile(f, EncodeObject(outer), 0600); err != nil {
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
		if ec, ok := env.Content.Get("ec"); ok {
			conn.Close()
			return nil, fmt.Errorf("CROSS-SERVER LOGIN FAILED: ec=%v full=%s", ec.Val, env.Content.String())
		}
		slog.Info("login OK", "response", env.Content.String())

		// Note: unlike login.go's equivalent redirect path (which fetches a fresh access token
		// before redialing, on the documented suspicion that a token is single-use-per-connection),
		// this path reuses p.AccessTok unchanged across the closed-and-redialed connection.
		// DoCrossServerLogin is deliberately GSL-free (see the doc comment above) -- it has no
		// httpClient/RSA pubkey/gateHost in scope to refresh a token with, so fixing this
		// properly means threading those through CrossServerLoginParams and every caller. Not
		// done here; this is a known, live-unverified gap -- if a serverInfo redirect on this
		// path starts failing with ec=28/E011 after the redial, this reused token is the first
		// thing to suspect.
		if siObj := findServerInfo(env.Content); siObj != nil && siObj.GetString("ip") != "" {
			newAddr := fmt.Sprintf("%s:%d", firstHost(siObj.GetString("ip")), getIntFlexible(siObj, "port"))
			newZone := siObj.GetString("zone")
			slog.Info("serverInfo redirect: reconnecting to new address", "newAddr", newAddr, "newZone", newZone, "oldAddr", addr, "oldZone", zone)
			conn.Close()
			addr = newAddr
			if newZone != "" {
				zone = newZone
			}
			continue
		}

		conn.conn.SetReadDeadline(time.Time{})
		return &CrossServerLoginResult{Conn: conn, Content: env.Content, Addr: addr, Zone: zone}, nil
	}
}

func serverIDFromZone(zone string) string {
	id := zone
	if len(id) > 3 && id[:3] == "APS" {
		id = id[3:]
	}
	return id
}
