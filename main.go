package main

import (
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

func main() {
	email := flag.String("email", "", "account email to log in with (only needed if no loginKey is on file yet)")
	codePipe := flag.String("code-pipe", "", "path to a FIFO to read the verification code from (blocks open until a writer connects); if empty, reads from stdin")
	collect := flag.Bool("collect", false, "collect resources from every confirmed building type, plus the Armed Truck idle reward, greeting city visitors, helping alliance members, claiming all mail and alliance gifts, donating to the recommended alliance tech, and both once-a-day VIP claims, after login")
	listBuildings := flag.Bool("list-buildings", false, "print every owned building (id, type, level); the process still exits after -collect/-list-buildings finish unless -interactive is also set")
	interactive := flag.String("interactive", "", "stay connected and read ad-hoc test commands from this control FIFO instead of exiting")

	csIP := flag.String("cs-ip", "", "skip normal login; reconnect directly to this ip (pipe-delimited ok) using an already-known role (from accountArr/push.account.login.new)")
	csPort := flag.Int("cs-port", 0, "port for -cs-ip")
	csZone := flag.String("cs-zone", "", "zone for -cs-ip, e.g. APS1234")
	csGameUid := flag.String("cs-gameuid", "", "composite gameUid for -cs-ip")
	csDeviceID := flag.String("cs-deviceid", "", "override deviceId (e.g. a real device's, extracted from its local PlayerPrefs) instead of this Go client's own persisted one")
	csShumei := flag.String("cs-shumei", "", "real shumeiBoxId anti-fraud fingerprint token, if known")
	csRt := flag.String("cs-rt", "", "if set, first does a GSL opt=refresh call with this refresh token to obtain a fresh access token before reconnecting -- the refresh response's server list also REPLACES any explicitly-passed -cs-ip/-cs-port/-cs-zone/-cs-gameuid")
	csAt := flag.String("cs-at", "", "raw access token to send directly as p.at, skipping any GSL call entirely (e.g. one captured live from a real client)")
	csIOS := flag.Bool("cs-ios", false, "send an iOS-flavored Login (packageName=com.lastwar.ios, matching packageSign/platform/pf) instead of Android -- an 'at' token is bound to the platform/package it was issued for")
	handshake := flag.Bool("handshake", false, "experimental: send the vanilla SFS2X pre-Login Handshake (action=0) before Login -- see conn.go:DoHandshake")
	configPath := flag.String("config", "", "path to a session config JSON (see config.example.json); if unset, auto-loads "+defaultSessionConfigPath()+" when present. Explicit -cs-* flags override individual config fields.")
	noConfig := flag.Bool("no-config", false, "skip loading any session config, even the default file -- for a plain guest/email-flow run when a session config is also present")
	decodeStream := flag.String("decode-stream", "", "decode a reassembled raw TCP byte stream file (see docs/capturing-and-decoding-traffic.mdx) and print every SFS2X packet, then exit -- no login, no network connection at all")
	decodeLabel := flag.String("decode-label", "", "prefix label for -decode-stream output lines, e.g. \"c2s\" or \"s2c\" (default: \"stream\")")
	logLevel := flag.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	version := flag.Bool("version", false, "print build info and exit")
	flag.Parse()

	if *version {
		printVersion()
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	csIOSSetExplicitly := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "cs-ios" {
			csIOSSetExplicitly = true
		}
	})

	if *decodeStream != "" {
		if *collect || *listBuildings || *interactive != "" || *email != "" || *csIP != "" || *csRt != "" {
			slog.Warn("ignoring all other flags because -decode-stream is set (no login or network connection happens in this mode)")
		}
		runDecode(*decodeLabel, *decodeStream)
		return
	}

	if *noConfig && *configPath != "" {
		slog.Warn("ignoring -config because -no-config is also set")
	}

	var cfg *SessionConfig
	var cfgSource string
	if !*noConfig {
		cfg, cfgSource = loadEffectiveConfig(*configPath)
	}
	if cfg != nil {
		slog.Info("loaded session config", "path", cfgSource)
		*csIP = applyOverride(cfg.IP, *csIP)
		if *csPort == 0 {
			*csPort = cfg.Port
		}
		*csZone = applyOverride(cfg.Zone, *csZone)
		*csGameUid = applyOverride(cfg.GameUid, *csGameUid)
		*csDeviceID = applyOverride(cfg.DeviceID, *csDeviceID)
		*csShumei = applyOverride(cfg.ShumeiBoxId, *csShumei)
		*csAt = applyOverride(cfg.AccessToken, *csAt)
		if !csIOSSetExplicitly {
			*csIOS = cfg.IOSMode
		}
	}

	if *csIP != "" || *csRt != "" {
		if *email != "" {
			slog.Warn("ignoring -email because -cs-ip/-cs-rt is set (cross-server reconnect doesn't use the email flow)")
		}
		if *codePipe != "" {
			slog.Warn("ignoring -code-pipe because -cs-ip/-cs-rt is set (cross-server reconnect doesn't use the email flow)")
		}
		runCrossServerTest(crossServerTestOpts{
			ip: *csIP, port: *csPort, zone: *csZone, gameUid: *csGameUid,
			deviceID: *csDeviceID, shumeiBoxId: *csShumei, rt: *csRt, at: *csAt,
			iosMode: *csIOS, interactive: *interactive, handshake: *handshake,
			collect: *collect, listBuildings: *listBuildings, configSavePath: cfgSource,
		})
		return
	}

	result, err := Login(LoginOptions{Email: *email, CodePipe: *codePipe, Handshake: *handshake})
	if err != nil {
		slog.Error("login failed", "error", err)
		// Exit code 2 (rather than the generic 1) specifically marks a
		// confirmed server-side auth rejection (ErrAuthRejected) -- the
		// class of failure the README documents as needing a fresh
		// capture, not a transient retry -- so a cron wrapper can
		// distinguish it without parsing the JSON log body. A network/
		// dial/local-I/O failure that never reached that point is just a
		// generic failure (1): it may well clear up on its own.
		if errors.Is(err, ErrAuthRejected) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	conn := result.Conn
	defer conn.Close()

	buildings := result.Buildings
	visitors := result.Visitors
	if len(buildings) == 0 {
		// Login's own retry loop already tried (and logged) 3 attempts at
		// the `init` push; this is just a last-chance listen (e.g. the
		// loginKey fast-path, where Login returns immediately without
		// running that loop) before giving up.
		slog.Info("fetching building list (push.init.build)")
		buildings, visitors, err = FetchBuildings(conn, 12*time.Second)
		if err != nil {
			slog.Error("fetch buildings failed", "error", err)
			os.Exit(1)
		}
	}
	slog.Info("got buildings", "count", len(buildings))

	if *listBuildings || !*collect {
		PrintBuildings(buildings)
	}

	if *collect {
		slog.Info("collecting resources")
		if err := CollectAll(conn, buildings, visitors); err != nil {
			slog.Error("collect run had failures", "error", err)
			os.Exit(1)
		}
	}

	if *interactive != "" {
		RunInteractive(conn, *interactive) // blocks forever
	}

	slog.Info("client exiting")
}

// parseLogLevel maps a -log-level flag value to an slog.Level, defaulting to Info for anything
// unrecognized (including the empty string).
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// printVersion prints build/VCS info from the Go toolchain's embedded build metadata (populated
// automatically when built with VCS stamping enabled, the default for "go build" inside a git
// checkout -- no ldflags or build-time flags needed).
func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("lastwar-client (build info unavailable)")
		return
	}
	revision, dirty := "unknown", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = " (modified)"
			}
		}
	}
	fmt.Printf("lastwar-client %s%s (go %s)\n", revision, dirty, info.GoVersion)
}

type crossServerTestOpts struct {
	ip, zone, gameUid, deviceID, shumeiBoxId, rt, at, interactive string
	port                                                          int
	handshake, iosMode, collect, listBuildings                    bool
	configSavePath                                                string // if non-empty, persist a resolved serverInfo redirect back here (see runCrossServerTest)
}

// runCrossServerTest exercises DoCrossServerLogin directly, using an
// already-known role's connection details (captured from a prior
// account.login.new/push.account.login.new response) instead of running
// the email flow again. If deviceID is given, it overrides this Go
// client's own persisted device identity -- e.g. to reuse a real device's
// identity/fingerprint extracted from its local PlayerPrefs. If rt is
// given, first refreshes the access token via GSL opt=refresh (using that
// device identity) before attempting the SFS reconnect.
func runCrossServerTest(o crossServerTestOpts) {
	ident, err := loadOrCreateDeviceIdentity()
	if err != nil {
		slog.Error("load device identity failed", "error", err)
		os.Exit(1)
	}
	deviceID, airKey := ident.DeviceID, ident.AirKey()
	if o.deviceID != "" {
		deviceID = o.deviceID
		airKey = "lwDid_" + b64OfString(deviceID)
	}
	slog.Info("using device identity", "deviceIdLen", len(deviceID), "airKeyLen", len(airKey))

	accessTok := o.at
	ip, port, zone, gameUid := o.ip, o.port, o.zone, o.gameUid
	// Captured only when -cs-rt runs its own GSL round trip below, then threaded into
	// CrossServerLoginParams so a mid-login serverInfo redirect can refresh AccessTok instead of
	// reusing a stale one (see CrossServerLoginParams' doc comment). Left nil for a bare -cs-ip
	// run: DoCrossServerLogin is deliberately GSL-free in that mode ("dialing directly, no GSL
	// call" -- see its own doc comment), so we don't add a surprise network round trip just to
	// get redirect-refresh capability; it degrades to the existing logged-warning fallback.
	var gslHTTPClient *http.Client
	var gslRSAPub *rsa.PublicKey
	var gslGateHost string
	if o.rt != "" {
		httpClient := defaultHTTPClient()
		cv, gateHost, err := CheckVersion(httpClient)
		if err != nil {
			slog.Error("check-version failed", "error", err)
			os.Exit(1)
		}
		pub, err := parseRSAPubKeyFromDER(cv.ResMsg)
		if err != nil {
			slog.Error("parse RSA pubkey failed", "error", err)
			os.Exit(1)
		}
		gslHTTPClient, gslRSAPub, gslGateHost = httpClient, pub, gateHost
		slog.Info("GSL getserverlist (opt=refresh)")
		lsr, err := GetServerList(httpClient, gateHost, pub, deviceID, GSLOpt{Opt: "refresh", Rt: o.rt}, "", o.gameUid)
		if err != nil {
			slog.Error("GSL refresh failed", "error", err)
			// Exit code 2 marks authentication/session failures specifically -- see the matching comment in main() above.
			os.Exit(2)
		}
		slog.Info("GSL refresh response", "code", lsr.Code, "serverListLen", len(lsr.ServerList))
		if lsr.At != nil {
			accessTok = lsr.At.Token
			slog.Info("fresh access token acquired", "tokenLen", len(accessTok))
		}
		if len(lsr.ServerList) > 0 {
			srv := lsr.ServerList[0]
			ip, port, zone, gameUid = srv.IP, srv.Port, srv.Zone, srv.GameUid
			slog.Info("server selected", "ip", ip, "port", port, "zone", zone, "gameUid", gameUid)
		}
	}

	result, err := DoCrossServerLogin(CrossServerLoginParams{
		IP: ip, Port: port, Zone: zone, GameUid: gameUid,
		DeviceID: deviceID, AirKey: airKey,
		AccessTok: accessTok, ShumeiBoxId: o.shumeiBoxId,
		Handshake: o.handshake, IOSMode: o.iosMode,
		HTTPClient: gslHTTPClient, RSAPub: gslRSAPub, GateHost: gslGateHost,
	})
	if err != nil {
		slog.Error("cross-server login failed", "error", err)
		// Exit code 2 marks a confirmed server-side auth rejection specifically -- see the
		// matching comment in main() above. A bare TCP dial failure never reaches that point,
		// so it falls through to the generic exit code 1.
		if errors.Is(err, ErrAuthRejected) {
			os.Exit(2)
		}
		os.Exit(1)
	}
	conn := result.Conn
	defer conn.Close()

	// A serverInfo redirect (e.g. a real server merge moving this account
	// to a different zone/host/port) leaves result.Addr/Zone different
	// from what was actually passed in. Persist the resolved address back
	// to the session config, if we loaded one, so the next run connects
	// directly instead of re-following the same redirect every time.
	if o.configSavePath != "" {
		if newHost, newPortStr, splitErr := net.SplitHostPort(result.Addr); splitErr == nil {
			if newPort, atoiErr := strconv.Atoi(newPortStr); atoiErr == nil {
				if newHost != ip || newPort != port || result.Zone != zone {
					updated := &SessionConfig{
						IP: newHost, Port: newPort, Zone: result.Zone,
						GameUid: gameUid, DeviceID: deviceID,
						ShumeiBoxId: o.shumeiBoxId, AccessToken: accessTok,
						IOSMode: o.iosMode,
					}
					if err := SaveSessionConfig(updated, o.configSavePath); err != nil {
						slog.Warn("failed to persist redirected server address to session config", "path", o.configSavePath, "error", err)
					} else {
						slog.Info("persisted redirected server address to session config", "path", o.configSavePath, "newIP", newHost, "newPort", newPort, "newZone", result.Zone)
					}
				}
			}
		}
	}

	slog.Info("fetching building list (push.init.build)")
	buildings, visitors, err := FetchBuildings(conn, 15*time.Second)
	if err != nil {
		slog.Error("fetch buildings failed", "error", err)
		os.Exit(1)
	}
	slog.Info("got buildings", "count", len(buildings))
	if o.listBuildings || !o.collect {
		PrintBuildings(buildings)
	}
	if o.collect {
		slog.Info("collecting resources")
		if err := CollectAll(conn, buildings, visitors); err != nil {
			slog.Error("collect run had failures", "error", err)
			os.Exit(1)
		}
	}

	if o.interactive != "" {
		RunInteractive(conn, o.interactive)
	}
	slog.Info("client exiting")
}
