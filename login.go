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
// a redirect payload with a missing or unparseable `port` field -- gsl.go's getIntFlexible
// silently returns 0 for either case rather than erroring -- would sail through and produce a
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

	pub, err := parseRSAPubKeyFromDER(cv.ResMsg)
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
	slog.Info("persisted state", "username", ident.Username, "gameUid", ident.GameUid, "loginKey", redact(ident.LoginKey))

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
	for _, s := range lsr.ServerList {
		slog.Info("state server", "id", s.ID, "name", s.Name, "ip", s.IP, "port", s.Port, "zone", s.Zone, "gameUid", s.GameUid, "status", s.Status)
	}
	accessTok := ""
	if lsr.At != nil {
		accessTok = lsr.At.Token
		slog.Info("access token acquired", "tokenLen", len(accessTok))
	}

	stateSrv := lsr.ServerList[0]
	zone := stateSrv.Zone
	gameUid := stateSrv.GameUid
	if gameUid != "" && gameUid != ident.GameUid {
		if err := ident.SaveGameUid(gameUid); err != nil {
			slog.Warn("failed to persist gameUid", "error", err)
		}
	}
	addr, err := buildBaseZoneLoginAddr(stateSrv.IP, stateSrv.Port)
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
		conn, err = DialGame(addr, 10*time.Second)
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
			return nil, err
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
		slog.Info("login OK", "un", env.Content.GetString("un"))
		if un := env.Content.GetString("un"); un != "" && un != ident.Username {
			if err := ident.SaveUsername(un); err != nil {
				slog.Warn("failed to persist username", "error", err)
			} else {
				slog.Info("persisted username for future runs", "username", un)
			}
		}

		if siObj := findServerInfo(env.Content); siObj != nil && siObj.GetString("ip") != "" {
			redirectHops++
			if redirectHops > maxRedirectHops {
				conn.Close()
				return nil, fmt.Errorf("login: too many serverInfo redirects (>%d), last addr=%s zone=%s", maxRedirectHops, addr, zone)
			}
			newAddr, err := buildBaseZoneLoginAddr(siObj.GetString("ip"), int(getIntFlexible(siObj, "port")))
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("login: serverInfo redirect: %w", err)
			}
			newZone := siObj.GetString("zone")
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
					accessTok = freshLsr.At.Token
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
					if newGameUid := freshLsr.ServerList[0].GameUid; newGameUid != "" && newGameUid != gameUid {
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
		return nil, err
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
		code = readCodeFromPipe(opts.CodePipe)
	} else {
		slog.Info("feed the 6-digit code on stdin")
		code = readCodeFromStdin()
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
		return nil, err
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

	if lk := msg2.Params.GetString("loginKey"); lk != "" {
		if err := ident.SaveLoginKey(lk); err != nil {
			slog.Warn("failed to persist loginKey", "error", err)
		} else {
			slog.Info("persisted loginKey for future fast logins")
		}
	}
	if gu := msg2.Params.GetString("gameUid"); gu != "" {
		_ = ident.SaveGameUid(gu)
	}
	if un := msg2.Params.GetString("gameUserName"); un != "" {
		_ = ident.SaveUsername(un)
	}

	return result, nil
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	return s[:4] + "..." + s[len(s)-4:]
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
// returned immediately as this same terminal connection-failure result,
// not merely logged and fallen through into the next blocking read. A
// half-open connection can surface a local write error fast while the
// following ReadEnvelope genuinely blocks until the deadline -- without
// this, that scenario would misreport a definite, already-logged
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
				// here would misreport it as a plain timeout instead.
				slog.Error("login.init send failed", "error", err)
				return nil, nil, false, err
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
			// A real connection failure, not a deadline -- surface it so the caller can log
			// what actually went wrong instead of a generic timeout message.
			return nil, nil, false, err
		}
		msg, ok := env.AsExtension()
		if !ok {
			continue
		}
		if msg.Cmd == "init" {
			return ParseInitBuildings(msg.Params), ParseInitVisitors(msg.Params), true, nil
		}
		slog.Debug("skipped push while waiting for init", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
	}
}

// waitFor reads envelopes until pred matches or timeout elapses, logging
// everything it skips past along the way.
func waitFor(conn *GameConn, timeout time.Duration, pred func(*Envelope) bool) (*Envelope, error) {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timed out waiting for matching envelope")
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

func readCodeFromStdin() string {
	return readCodeFrom(os.Stdin)
}

// readCodeFromPipe opens a FIFO for reading -- this blocks until a writer
// opens the other end, which is exactly what we want: the process can sit
// here idle (heartbeat still running in the background) until the code is
// written to the pipe from a separate shell command.
func readCodeFromPipe(path string) string {
	fi, statErr := os.Stat(path)
	if statErr != nil {
		slog.Error("stat code pipe failed", "codePipe", path, "error", statErr)
		os.Exit(1)
	}
	if fi.Mode()&os.ModeNamedPipe == 0 {
		slog.Error("codePipe exists but is not a FIFO -- did you forget mkfifo?", "codePipe", path)
		os.Exit(1)
	}
	f, err := os.Open(path)
	if err != nil {
		slog.Error("open code pipe", "path", path, "error", err)
		os.Exit(1)
	}
	defer f.Close()
	return readCodeFrom(f)
}

func readCodeFrom(r io.Reader) string {
	reader := bufio.NewReader(r)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			slog.Error("input closed without a code", "error", err)
			os.Exit(1)
		}
		code := strings.TrimSpace(line)
		if code != "" {
			return code
		}
	}
}
