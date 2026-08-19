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
	// A plain flag.String/Bool/Int + flag.Parse() would use the package-level
	// flag.CommandLine, which is constructed with ExitOnError -- on a bad flag
	// (unknown flag, bad value) that calls os.Exit(2) directly, colliding with
	// this program's own contract that exit code 2 means "confirmed
	// server-side auth rejection" (see the ErrAuthRejected handling below).
	// ContinueOnError instead hands the parse error back to us so we can pick
	// a non-colliding exit code.
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	email := fs.String("email", "", "account email to log in with (only needed if no loginKey is on file yet)")
	codePipe := fs.String("code-pipe", "", "path to a FIFO to read the verification code from (blocks open until a writer connects); if empty, reads from stdin")
	collect := fs.Bool("collect", false, "collect resources from every confirmed building type, plus the Armed Truck idle reward, greeting city visitors, helping alliance members, claiming all mail and alliance gifts, donating to the recommended alliance tech, and both once-a-day VIP claims, after login")
	listBuildings := fs.Bool("list-buildings", false, "print every owned building (id, type, level); the process still exits after -collect/-list-buildings finish unless -interactive is also set")
	interactive := fs.String("interactive", "", "stay connected and read ad-hoc test commands from this control FIFO instead of exiting")

	csIP := fs.String("cs-ip", "", "skip normal login; reconnect directly to this ip (pipe-delimited ok) using an already-known role (from accountArr/push.account.login.new)")
	csPort := fs.Int("cs-port", 0, "port for -cs-ip")
	csZone := fs.String("cs-zone", "", "zone for -cs-ip, e.g. APS1234")
	csGameUid := fs.String("cs-gameuid", "", "composite gameUid for -cs-ip")
	csDeviceID := fs.String("cs-deviceid", "", "override deviceId (e.g. a real device's, extracted from its local PlayerPrefs) instead of this Go client's own persisted one")
	csShumei := fs.String("cs-shumei", "", "real shumeiBoxId anti-fraud fingerprint token, if known")
	csRt := fs.String("cs-rt", "", "if set, first does a GSL opt=refresh call with this refresh token to obtain a fresh access token before reconnecting -- the refresh response's server list also REPLACES any explicitly-passed -cs-ip/-cs-port/-cs-zone/-cs-gameuid, and the fresh access token REPLACES any explicitly-passed -cs-at")
	csAt := fs.String("cs-at", "", "raw access token to send directly as p.at (e.g. one captured live from a real client) -- a cheap CheckVersion call is still made, to enable mid-login redirect-refresh capability, but no other GSL call happens unless -cs-rt is also set")
	csIOS := fs.Bool("cs-ios", false, "send an iOS-flavored Login (packageName=com.lastwar.ios, matching packageSign/platform/pf) instead of Android -- an 'at' token is bound to the platform/package it was issued for")
	handshake := fs.Bool("handshake", false, "experimental: send the vanilla SFS2X pre-Login Handshake (action=0) before Login -- see conn.go:DoHandshake")
	configPath := fs.String("config", "", "path to a session config JSON (see config.example.json); if unset, auto-loads "+defaultSessionConfigPath()+" when present. Explicit -cs-* flags override individual config fields.")
	noConfig := fs.Bool("no-config", false, "skip loading any session config, even the default file -- for a plain guest/email-flow run when a session config is also present")
	decodeStream := fs.String("decode-stream", "", "decode a reassembled raw TCP byte stream file (see docs/capturing-and-decoding-traffic.mdx) and print every SFS2X packet, then exit -- no login, no network connection at all")
	decodeLabel := fs.String("decode-label", "", "prefix label for -decode-stream output lines, e.g. \"c2s\" or \"s2c\" (default: \"stream\")")
	logLevel := fs.String("log-level", "info", "log verbosity: debug, info, warn, or error")
	version := fs.Bool("version", false, "print build info and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag.ContinueOnError still runs the same failf/usage path flag.ExitOnError does (it
		// prints the error and the usage text to stderr internally on every parse failure) -- it
		// only differs in returning the error here instead of calling os.Exit itself, which is
		// the whole point: it lets us pick the exit code instead of colliding with our own
		// contract below. -h/-help isn't a usage error, just an explicit request for that same
		// usage text, so it keeps exiting 0 as it always did with the default FlagSet. Any real
		// parse error (unknown flag, bad value, missing argument) exits 1 rather than the
		// colliding 2, keeping the exit-code contract binary: 2 means confirmed auth rejection,
		// everything else means "look at the log".
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(0)
		}
		os.Exit(1)
	}

	if *version {
		printVersion()
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	csIOSSetExplicitly := false
	var ignoredInDecodeMode []string
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "cs-ios" {
			csIOSSetExplicitly = true
		}
		// -decode-label is honored in -decode-stream mode (it labels that mode's own output), and
		// -log-level is honored in every mode (it configures slog before any of this runs) -- every
		// other flag that was explicitly set is dead weight in -decode-stream mode, since that mode
		// never logs in, connects, or touches a session config at all. flag.Visit only visits flags
		// actually set on the command line, so this naturally covers any flag added in the future
		// without needing its name added here too.
		if f.Name != "decode-stream" && f.Name != "decode-label" && f.Name != "log-level" {
			ignoredInDecodeMode = append(ignoredInDecodeMode, f.Name)
		}
	})

	if *decodeStream != "" {
		if len(ignoredInDecodeMode) > 0 {
			slog.Warn("ignoring other flags because -decode-stream is set (no login or network connection happens in this mode)", "ignoredFlags", ignoredInDecodeMode)
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

	// Symmetric to the -email/-code-pipe-ignored warnings just below (for the opposite direction):
	// if any -cs-* flag OTHER than -cs-ip/-cs-rt was explicitly set on the command line but the
	// cross-server dispatch branch below won't actually be taken, that flag is otherwise silently
	// discarded and this falls through to the full guest/email login flow instead -- easy to miss
	// (e.g. a typo'd -cs-ip that got dropped by config merging, or -cs-at set while forgetting
	// -cs-rt) without any warning. Checked here, after config merging, so a config-supplied ip
	// correctly counts as "cross-server WILL be taken" and doesn't produce a false warning.
	if *csIP == "" && *csRt == "" {
		var ignoredCSFlags []string
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "cs-at", "cs-port", "cs-zone", "cs-gameuid", "cs-deviceid", "cs-shumei", "cs-ios":
				ignoredCSFlags = append(ignoredCSFlags, f.Name)
			}
		})
		if len(ignoredCSFlags) > 0 {
			slog.Warn("ignoring -cs-* flags because neither -cs-ip nor -cs-rt is set (falling through to the normal guest/email login flow instead of cross-server reconnect)", "ignoredFlags", ignoredCSFlags)
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
		// Login() itself already made its one attempt at the `init` push --
		// see the comment above maxLoginAttempts in login.go for why that's
		// kept at a single attempt rather than a retry loop. This is just one
		// last chance to catch a late `init` before giving up entirely.
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

// parseLogLevel maps a -log-level flag value to an slog.Level, defaulting to Info for the empty
// string (the flag's own default) and for anything unrecognized -- but an unrecognized value (e.g.
// a typo) is reported to stderr first, since slog isn't configured yet at the point this runs: its
// return value is what configures slog's own level a moment later in main(), so there's no logger
// to slog.Warn through yet.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		fmt.Fprintf(os.Stderr, "unrecognized -log-level %q, defaulting to info (valid values: debug, warn, error, info)\n", s)
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

	// GSL plumbing (HTTP client + gate host + RSA pubkey), threaded into CrossServerLoginParams so a
	// mid-login serverInfo redirect can refresh AccessTok instead of reusing a stale one (see
	// CrossServerLoginParams' doc comment). CheckVersion is a single cheap HTTP call, so it's made
	// unconditionally here rather than only under -cs-rt: a plain SessionConfig reconnect with no
	// -cs-rt at all (the primary case crossserver.go's own redirect-following doc comment is about --
	// surviving a real server-merge zone migration) could otherwise never reach this safety net.
	//
	// -cs-rt additionally NEEDS this to make its own opt=refresh call below, so a failure here stays
	// fatal (os.Exit) in that case, matching this function's existing error-handling posture for
	// -cs-rt. For every other path, this is purely a best-effort enhancement on top of a reconnect
	// that doesn't otherwise need any GSL/HTTP capability -- a failure here just logs a warning and
	// leaves gslHTTPClient/gslRSAPub/gslGateHost nil, falling back to today's documented degraded
	// behavior (DoCrossServerLogin reusing the current access token unrefreshed, with its own logged
	// warning, if a redirect is actually hit) instead of failing the whole run over it.
	var gslHTTPClient *http.Client
	var gslRSAPub *rsa.PublicKey
	var gslGateHost string
	{
		httpClient := defaultHTTPClient()
		cv, gateHost, err := CheckVersion(httpClient)
		if err != nil {
			if o.rt != "" {
				slog.Error("check-version failed", "error", err)
				os.Exit(1)
			}
			slog.Warn("check-version failed; proceeding without redirect-refresh capability (a mid-login serverInfo redirect will fall back to reusing the current access token)", "error", err)
		} else if pub, err := parseRSAPubKeyFromDER(cv.ResMsg); err != nil {
			if o.rt != "" {
				slog.Error("parse RSA pubkey failed", "error", err)
				os.Exit(1)
			}
			slog.Warn("parse RSA pubkey failed; proceeding without redirect-refresh capability (a mid-login serverInfo redirect will fall back to reusing the current access token)", "error", err)
		} else {
			gslHTTPClient, gslRSAPub, gslGateHost = httpClient, pub, gateHost
		}
	}

	if o.rt != "" {
		slog.Info("GSL getserverlist (opt=refresh)")
		lsr, err := GetServerList(gslHTTPClient, gslGateHost, gslRSAPub, deviceID, GSLOpt{Opt: "refresh", Rt: o.rt}, "", o.gameUid)
		if err != nil {
			slog.Error("GSL refresh failed", "error", err)
			// Exit code 2 marks a confirmed server-side auth rejection specifically -- see the
			// matching comment in main() above. A network/HTTP/decode/decrypt failure that never
			// reached a confirmed rejection is just a generic failure (1), matching the two
			// sibling ErrAuthRejected-gated exits elsewhere in this file.
			if errors.Is(err, ErrAuthRejected) {
				os.Exit(2)
			}
			os.Exit(1)
		}
		slog.Info("GSL refresh response", "code", lsr.Code, "serverListLen", len(lsr.ServerList))
		if lsr.At != nil {
			if o.at != "" {
				slog.Warn("ignoring -cs-at because -cs-rt is set (the GSL refresh response's access token replaces it)")
			}
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
						ShumeiBoxId: o.shumeiBoxId, AccessToken: result.AccessTok,
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
