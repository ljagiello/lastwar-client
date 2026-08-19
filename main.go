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
	listBuildings := fs.Bool("list-buildings", false, "print every owned building (id, type, level) to stdout. NOTE: this print already happens by DEFAULT whenever -collect is NOT passed -- this flag only matters when -collect IS also passed, where it forces the same print to happen alongside collection instead of being skipped; the process still exits after -collect/-list-buildings finish (assuming -collect, if passed, didn't fail fatally first) unless -interactive is also set")
	interactive := fs.String("interactive", "", "stay connected and read ad-hoc test commands from this control FIFO instead of exiting; only flat scalar param values (strings/bools/numbers) are supported -- nested JSON objects/arrays are rejected with a logged error and abort the whole send (see interactive.go)")

	csIP := fs.String("cs-ip", "", "skip normal login; reconnect directly to this ip (pipe-delimited ok) using an already-known role (from accountArr/push.account.login.new)")
	csPort := fs.Int("cs-port", 0, "port for -cs-ip")
	csZone := fs.String("cs-zone", "", "zone for -cs-ip, e.g. APS1234")
	csGameUid := fs.String("cs-gameuid", "", "composite gameUid for -cs-ip")
	csDeviceID := fs.String("cs-deviceid", "", "override deviceId (e.g. a real device's, extracted from its local PlayerPrefs) instead of this Go client's own persisted one")
	csShumei := fs.String("cs-shumei", "", "real shumeiBoxId anti-fraud fingerprint token, if known")
	csRt := fs.String("cs-rt", "", "if set, first does a GSL opt=refresh call with this refresh token to obtain a fresh access token before reconnecting -- IF the refresh response includes a non-empty server list, it REPLACES any explicitly-passed -cs-ip/-cs-port/-cs-zone/-cs-gameuid, and IF it includes a fresh access token, that REPLACES any explicitly-passed -cs-at; either can come back empty, in which case the corresponding -cs-* value passed here (or loaded from a session config) is used unchanged instead -- a warning is logged when that leaves a possibly-stale -cs-at in place with no refresh")
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

	// Go's flag package stops parsing at the first token that doesn't start with '-' and silently
	// stashes everything from there on as "positional" arguments (fs.Args()) instead of treating it
	// as an error -- fs.Parse above returns a nil error for e.g. `lastwar-client collect`, exactly as
	// if "collect" were never there at all. Left unchecked, that's a real trap: the single most
	// likely real-world cause is an operator typo'ing a flag without its leading dash (e.g. `collect`
	// instead of `-collect`), and today that silently proceeds into a full guest-login run instead of
	// catching what's almost certainly a mistake. slog isn't configured yet at this point (that
	// happens further below, after the -version short-circuit), so this reports via stderr directly,
	// matching parseLogLevel's own fmt.Fprintf-to-stderr convention for the same reason.
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument(s): %s (missing a leading '-' on a flag? see -help)\n", strings.Join(fs.Args(), " "))
		os.Exit(1)
	}

	// -version intentionally bypasses the ignored-flags warning machinery below (decodeModeIgnoredFlags,
	// ignoredCrossServerFlags, the -config/-no-config check, the -email/-code-pipe check, and
	// warnIfDecodeLabelIgnored): it returns here, before fs.Visit populates visitedFlags below, so
	// stacking -version with any other flag (e.g. `-version -collect -cs-at X`) silently drops those
	// other flags with no warning, unlike every other no-op-flag-combination case this file otherwise
	// warns about. This is deliberate, not an oversight: stacking -version with live-run flags is an
	// unlikely real operator mistake (low real-world impact), and keeping -version's exit simple,
	// fast, and unconditional (no slog setup, no fs.Visit call) is judged worth more than closing this
	// specific gap. Don't "fix" this by reordering fs.Visit() above this check without re-deriving
	// that tradeoff first.
	if *version {
		printVersion()
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	csIOSSetExplicitly := false
	// csIPSetExplicitly/csPortSetExplicitly/csZoneSetExplicitly/csGameUidSetExplicitly/
	// csAtSetExplicitly mirror csIOSSetExplicitly's own pattern, for the same reason: they record
	// whether the corresponding -cs-* flag was actually typed on the command line, as distinct from
	// ending up non-empty purely because a loaded session config's field is merged into it further
	// below (the "*csAt = applyOverride(cfg.AccessToken, *csAt)" style merge). Threaded into
	// crossServerTestOpts so runCrossServerTest's GSL-refresh flag-vs-config log-wording distinction
	// can reuse this same visitedFlags mechanism instead of inventing a new one -- see
	// crossServerTestOpts' doc comment.
	csIPSetExplicitly := false
	csPortSetExplicitly := false
	csZoneSetExplicitly := false
	csGameUidSetExplicitly := false
	csAtSetExplicitly := false
	var visitedFlags []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "cs-ios":
			csIOSSetExplicitly = true
		case "cs-ip":
			csIPSetExplicitly = true
		case "cs-port":
			csPortSetExplicitly = true
		case "cs-zone":
			csZoneSetExplicitly = true
		case "cs-gameuid":
			csGameUidSetExplicitly = true
		case "cs-at":
			csAtSetExplicitly = true
		}
		visitedFlags = append(visitedFlags, f.Name)
	})

	if *decodeStream != "" {
		if ignored := decodeModeIgnoredFlags(visitedFlags); len(ignored) > 0 {
			slog.Warn("ignoring other flags because -decode-stream is set (no login or network connection happens in this mode)", "ignoredFlags", ignored)
		}
		runDecode(*decodeLabel, *decodeStream)
		return
	}

	// Symmetric to the decode-mode warning just above (for the opposite direction): -decode-label
	// only has any effect as a prefix on -decode-stream's own output (see runDecode /
	// decodeModeIgnoredFlags' doc comment) -- reaching here already means -decode-stream is unset
	// (the block above returns otherwise), so -decode-label being set at this point is silently a
	// no-op the rest of this file's flags don't rely on either.
	warnIfDecodeLabelIgnored(*decodeStream, *decodeLabel)

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
	warnIfExplicitConfigPathNotFound(cfg, *configPath, *noConfig)

	// Symmetric to the -email/-code-pipe-ignored warnings just below (for the opposite direction):
	// if any -cs-* flag OTHER than -cs-ip/-cs-rt was explicitly set on the command line but the
	// cross-server dispatch branch below won't actually be taken, that flag is otherwise silently
	// discarded and this falls through to the full guest/email login flow instead -- easy to miss
	// (e.g. a typo'd -cs-ip that got dropped by config merging, or -cs-at set while forgetting
	// -cs-rt) without any warning. Checked here, after config merging, so a config-supplied ip
	// correctly counts as "cross-server WILL be taken" and doesn't produce a false warning.
	if *csIP == "" && *csRt == "" {
		if ignored := ignoredCrossServerFlags(visitedFlags); len(ignored) > 0 {
			slog.Warn("ignoring -cs-* flags because neither -cs-ip nor -cs-rt is set (falling through to the normal guest/email login flow instead of cross-server reconnect)", "ignoredFlags", ignored)
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
			ipExplicit: csIPSetExplicitly, portExplicit: csPortSetExplicitly,
			zoneExplicit: csZoneSetExplicitly, gameUidExplicit: csGameUidSetExplicitly,
			atExplicit: csAtSetExplicitly,
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

// decodeModeIgnoredFlags returns which of the given visited (explicitly set on the command line)
// flag names are dead weight in -decode-stream mode. -decode-label is honored there (it labels
// that mode's own output), and -log-level is honored in every mode (it configures slog before any
// of this runs) -- every other explicitly-set flag, including -decode-stream itself, is ignored,
// since -decode-stream mode never logs in, connects, or touches a session config at all. Taking the
// already-visited names as a plain slice (rather than calling flag.FlagSet.Visit itself) is what
// makes this testable without spawning a subprocess or building a real FlagSet.
func decodeModeIgnoredFlags(visited []string) []string {
	var ignored []string
	for _, name := range visited {
		if name != "decode-stream" && name != "decode-label" && name != "log-level" {
			ignored = append(ignored, name)
		}
	}
	return ignored
}

// warnIfDecodeLabelIgnored logs a warning if -decode-label was explicitly given a non-empty value
// but -decode-stream was not set -- -decode-label only has any effect as a prefix on
// -decode-stream's own output (see decodeModeIgnoredFlags' doc comment), so outside that mode it's
// silently a no-op that was otherwise never flagged, unlike every other no-op-flag-combination case
// this file warns about. Taking decodeStream/decodeLabel as plain arguments (rather than being
// inlined at the call site in main()) is what makes this testable via slog output capture, without
// building a real FlagSet or invoking main() as a subprocess.
func warnIfDecodeLabelIgnored(decodeStream, decodeLabel string) {
	if decodeStream == "" && decodeLabel != "" {
		slog.Warn("ignoring -decode-label because -decode-stream is not set")
	}
}

// warnIfExplicitConfigPathNotFound logs a warning when an explicit -config path was given but
// loadEffectiveConfig came back with nothing to load there (cfg == nil) -- config.go's own
// loadEffectiveConfig returns (nil, "") on os.IsNotExist(err) identically for both an explicit
// -config path and the auto-derived default path (deliberately -- see its doc comment: a first run
// with no config yet is a normal, expected case for the default path), so main() itself has to be
// the one to tell those two situations apart and only warn about the explicit one. An operator
// running unattended (cron, etc.) with a typo'd or moved -config path would otherwise get zero
// diagnostic: the run just silently degrades into an unrelated guest-identity flow.
//
// Deliberately not fatal/os.Exit: this could still be a legitimate "not created yet" first run
// (e.g. a fresh deploy pointing -config at a path a prior step hasn't written yet), so a WARN visible
// even at the default log level is the right bar -- giving visibility without forcing a behavior
// change for what may be intentional.
//
// noConfig is passed separately (rather than relying on cfg alone) because main() only calls
// loadEffectiveConfig at all when -no-config is NOT set -- with -no-config set, cfg is nil for a
// completely different, already-warned-about reason (see the "ignoring -config because -no-config is
// also set" warning above this call site), not because the explicit path was missing; this must not
// double-warn or misattribute that case.
//
// Taking cfg/configPath/noConfig as plain arguments (rather than being inlined at the call site in
// main()) is what makes this testable via slog output capture, matching warnIfDecodeLabelIgnored's
// own pattern just above.
func warnIfExplicitConfigPathNotFound(cfg *SessionConfig, configPath string, noConfig bool) {
	if cfg == nil && configPath != "" && !noConfig {
		slog.Warn("explicit -config path not found; continuing without it", "path", configPath)
	}
}

// crossServerFlagNames are the -cs-* flags whose only effect is on the cross-server reconnect path
// dispatched from -cs-ip/-cs-rt (see runCrossServerTest) -- -cs-ip and -cs-rt themselves are
// excluded since those two are what GATE that path, not flags merely consumed once it's taken. Kept
// as a package-level map (rather than inlined in ignoredCrossServerFlags) so a test can cross-check
// it against the FlagSet's actual -cs-* declarations and catch the two ways it can drift: a new
// -cs-* flag added to the FlagSet but forgotten here, or a stale name left here after a flag is
// renamed/removed.
var crossServerFlagNames = map[string]bool{
	"cs-at":       true,
	"cs-port":     true,
	"cs-zone":     true,
	"cs-gameuid":  true,
	"cs-deviceid": true,
	"cs-shumei":   true,
	"cs-ios":      true,
}

// ignoredCrossServerFlags returns which of the given visited (explicitly set on the command line)
// flag names are -cs-* flags that get silently discarded when neither -cs-ip nor -cs-rt is set --
// see the call site in main() for why that combination falls through to the plain guest/email login
// flow instead of cross-server reconnect.
func ignoredCrossServerFlags(visited []string) []string {
	var ignored []string
	for _, name := range visited {
		if crossServerFlagNames[name] {
			ignored = append(ignored, name)
		}
	}
	return ignored
}

// refreshHasUsableData reports whether a GSL opt=refresh response gives runCrossServerTest anything
// to act on: either a fresh access token (At) or at least one server list entry. A response with
// neither is not actionable -- see the call site in runCrossServerTest for why that case fails
// clearly there instead of silently falling through to stale in-scope values. Taking the already-
// decoded *LoginServerListRespon (rather than being inlined at the call site) is what makes this
// testable without a live GSL round-trip.
func refreshHasUsableData(lsr *LoginServerListRespon) bool {
	return lsr.At != nil || len(lsr.ServerList) > 0
}

// serverListOverrideFlags reports which of "cs-ip", "cs-port", "cs-zone", "cs-gameuid" (in that
// order) were actually typed on the command line -- per ipExplicit/portExplicit/zoneExplicit/
// gameUidExplicit, see crossServerTestOpts' doc comment -- and are therefore about to be silently
// overridden by a GSL opt=refresh response's non-empty ServerList. A nil result means none of the
// four were explicitly set (e.g. a fresh cron run with no prior overrides, where ip/port/zone/
// gameUid started from either their zero value or a loaded session config), which the call site
// uses to keep its existing plain INFO-level "server selected" log instead of escalating to WARN --
// only a real override of an operator-supplied value escalates. Taking the four bools as plain
// arguments (rather than the whole crossServerTestOpts struct, or being inlined at the call site) is
// what makes this testable without building a real crossServerTestOpts/GSL round-trip.
func serverListOverrideFlags(ipExplicit, portExplicit, zoneExplicit, gameUidExplicit bool) []string {
	var out []string
	if ipExplicit {
		out = append(out, "cs-ip")
	}
	if portExplicit {
		out = append(out, "cs-port")
	}
	if zoneExplicit {
		out = append(out, "cs-zone")
	}
	if gameUidExplicit {
		out = append(out, "cs-gameuid")
	}
	return out
}

// crossServerSaveBackNeeded reports whether runCrossServerTest has anything new to persist back
// to the session config: the FINAL address/zone/access-token/gameUid a cross-server connection
// actually used (newHost/newPort/newZone/newAccessTok/newGameUid, taken from
// CrossServerLoginResult) differing from what was ORIGINALLY on disk/passed on the command line
// (origHost/origPort/origZone/origAccessTok/origGameUid), before any -cs-rt refresh could have
// reassigned those.
//
// Bug fixed here (round 12): runCrossServerTest used to diff against its own ip/port/zone locals
// AFTER a -cs-rt refresh block already reassigned them to the GSL response's values -- so in the
// ordinary case (refresh succeeds, DoCrossServerLogin doesn't ALSO hit an unrelated serverInfo
// redirect of its own), that comparison was always post-refresh-value against post-refresh-value,
// always false, and a freshly obtained -cs-rt access token was silently never persisted. It also
// never compared AccessTok at all, only host/port/zone.
//
// Bug fixed here (round 13): the same class of bug applied to GameUid -- it was never compared
// either, so a -cs-rt refresh that changed ONLY GameUid (leaving host/port/zone/accessTok
// unchanged) was silently never persisted.
//
// Taking every value as a plain argument (rather than closing over runCrossServerTest's locals)
// is what makes all of these mistakes structurally impossible to reintroduce silently, and what
// makes this testable without spinning up fake GSL/game servers.
func crossServerSaveBackNeeded(newHost string, newPort int, newZone, newAccessTok, newGameUid, origHost string, origPort int, origZone, origAccessTok, origGameUid string) bool {
	return newHost != origHost || newPort != origPort || newZone != origZone || newAccessTok != origAccessTok || newGameUid != origGameUid
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

	// ipExplicit/portExplicit/zoneExplicit/gameUidExplicit/atExplicit record whether the
	// corresponding -cs-ip/-cs-port/-cs-zone/-cs-gameuid/-cs-at flag was actually typed on the
	// command line (populated via fs.Visit in main(), the same visitedFlags mechanism
	// ignoredCrossServerFlags already uses), as opposed to ip/port/zone/gameUid/at ending up
	// non-empty purely because a loaded session config's field was merged into it (see the
	// "*csAt = applyOverride(cfg.AccessToken, *csAt)" style merge in main()). runCrossServerTest
	// uses these to distinguish a GSL opt=refresh response overriding the operator's own explicit
	// choice (worth a WARN, and worth naming the flag in the message) from it merely overriding/
	// leaving-in-place a config-loaded default (expected, unremarkable, or worded without
	// misattributing it to a flag the operator never typed).
	ipExplicit, portExplicit, zoneExplicit, gameUidExplicit, atExplicit bool
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

	// Snapshot the values as they stood BEFORE the -cs-rt refresh block below can reassign
	// ip/port/zone/gameUid/accessTok to the GSL opt=refresh response's values. The save-back check
	// further down needs to diff the connection's final result against what's actually on
	// disk/was actually passed in -- not against an already-mutated intermediate value -- see
	// crossServerSaveBackNeeded's doc comment for the bug this specifically fixes.
	origIP, origPort, origZone, origAccessTok, origGameUid := ip, port, zone, accessTok, gameUid

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
		if !refreshHasUsableData(lsr) {
			// A nil error only means the HTTP round-trip and envelope decrypt succeeded -- gsl.go
			// deliberately doesn't validate lsr.Code yet (no live evidence exists for what a
			// semantically-rejected-but-HTTP-200 refresh looks like on this endpoint). Neither a
			// fresh access token nor a server list means this response is useless: falling through
			// would silently reuse the stale accessTok/ip/port/zone/gameUid already in scope,
			// producing a confusing downstream DoCrossServerLogin failure instead of failing clearly
			// here, at the point where the actual problem is known.
			slog.Error("GSL refresh returned no usable data -- likely rejected server-side, recapture the refresh token", "code", lsr.Code)
			// Exit code 2, not the generic 1: a GSL refresh call that comes back with neither a
			// fresh access token nor a usable server list is semantically a rejected/stale
			// session, not a transient local failure -- exactly the class of failure README.md's
			// "Exit code 2 means the session itself is stale ... Login/auth failures (both the
			// plain-login and cross-server-reconnect paths) exit 2 specifically" cron-wrapper
			// contract promises, unqualified, for this path too. Matches the sibling
			// ErrAuthRejected-gated exit-2 sites elsewhere in this function/file.
			os.Exit(2)
		}
		if lsr.At != nil {
			if o.at != "" && o.atExplicit {
				// Only warn when -cs-at was actually typed on the command line: an operator who
				// explicitly passed -cs-at presumably wanted THAT token used, so silently
				// replacing it is worth flagging. When o.at is non-empty purely because a loaded
				// session config's accessToken field was merged into it (o.atExplicit false),
				// replacing a stale config-stored token with a freshly refreshed one is exactly
				// what -cs-rt is FOR -- not a surprising override of operator intent -- so no
				// warning fires for that case.
				slog.Warn("ignoring -cs-at because -cs-rt is set (the GSL refresh response's access token replaces it)")
			}
			accessTok = lsr.At.Token
			slog.Info("fresh access token acquired", "tokenLen", len(accessTok))
		} else if o.at != "" {
			// Symmetric to the "ignoring -cs-at" warning just above: the refresh call succeeded
			// (refreshHasUsableData already confirmed it returned SOMETHING actionable, just not
			// an access token specifically -- e.g. a server-list-only response), but with no `at`
			// field to replace it, accessTok stays whatever o.at already was. That's silent
			// today: nothing currently points out that this run is proceeding with a possibly-
			// stale access token and got no refresh at all, which is exactly the kind of thing
			// worth knowing before an ec=28/E011 failure downstream -- unlike the warning just
			// above, this one fires regardless of whether the token came from -cs-at or a
			// session config, since the operational risk (an unrefreshed, possibly-stale token)
			// is identical either way; only the wording changes, so it doesn't misattribute a
			// config-sourced value to a flag the operator never actually typed.
			if o.atExplicit {
				slog.Warn("GSL refresh response carried no access token -- continuing with the original -cs-at unrefreshed", "accessTokLen", len(o.at))
			} else {
				slog.Warn("GSL refresh response carried no access token -- continuing with the session config's access token, unrefreshed", "accessTokLen", len(o.at))
			}
		} else {
			// Symmetric to the o.at != "" branch just above, for the opposite (and worse) case: the
			// refresh call succeeded (refreshHasUsableData already confirmed it returned SOMETHING
			// actionable, just not an access token) and there was no -cs-at/session-config access
			// token in scope to begin with either. That leaves accessTok as the empty string it
			// already was -- unlike the "unrefreshed but present" case above, this isn't a stale
			// token, it's LITERALLY no token at all, and the DoCrossServerLogin call below will very
			// likely fail (it validates AccessTok == "" itself). Worth flagging clearly here, at the
			// point where the actual cause is known, rather than only via that downstream failure.
			slog.Warn("GSL refresh response carried no access token, and none was already set -- this run has zero access token and will very likely fail downstream", "code", lsr.Code)
		}
		if len(lsr.ServerList) > 0 {
			srv := lsr.ServerList[0]
			if overridden := serverListOverrideFlags(o.ipExplicit, o.portExplicit, o.zoneExplicit, o.gameUidExplicit); len(overridden) > 0 {
				// Symmetric to the "ignoring -cs-at" WARN above, for the same reason: an
				// operator-supplied value is about to be silently replaced. Only escalated to
				// WARN when it's actually overriding something the operator explicitly typed --
				// a fresh cron run with no prior -cs-ip/-cs-port/-cs-zone/-cs-gameuid overrides at
				// all (e.g. everything came from a session config, or nothing was set yet) keeps
				// the plain INFO-level "server selected" log below instead.
				slog.Warn("GSL refresh response's server list is overriding explicitly-passed flag(s)", "overriddenFlags", overridden, "ip", srv.IP, "port", srv.Port, "zone", srv.Zone, "gameUid", srv.GameUid)
			} else {
				slog.Info("server selected", "ip", srv.IP, "port", srv.Port, "zone", srv.Zone, "gameUid", srv.GameUid)
			}
			ip, port, zone, gameUid = srv.IP, srv.Port, srv.Zone, srv.GameUid
		}
	}

	// Symmetric to the port <= 0 check just below, but arguably more important to catch here
	// rather than downstream: crossserver.go's addr := fmt.Sprintf("%s:%d", firstHost(p.IP),
	// p.Port) collapses an empty ip to just ":<port>", and Go's "host:port" dial syntax treats an
	// empty host as the LOOPBACK interface -- so this doesn't fail cleanly at all, it silently
	// attempts a real TCP connection to 127.0.0.1/::1 and returns a misleading "connection
	// refused" instead of any indication that no host was ever given. Reachable in practice: a
	// bare -cs-rt with no -cs-ip (and no session config supplying one) leaves ip empty here, and
	// if the GSL opt=refresh response's server list comes back empty, refreshHasUsableData's own
	// check above only guards against BOTH a missing access token AND a missing server list --
	// an empty server list alone, alongside a fresh access token, still passes that check and
	// falls through with ip left exactly as empty as it started. Checked here, after the -cs-rt
	// refresh block above (which can replace ip with a fresh server list entry), so a config/flag
	// omission that IS resolved by -cs-rt doesn't false-positive.
	if firstHost(ip) == "" {
		slog.Error("cross-server login: no ip given (pass -cs-ip or a session config with ip)")
		os.Exit(1)
	}

	// DoCrossServerLogin validates AccessTok itself (see its own p.AccessTok == "" check) but has
	// no equivalent check for Port -- an unset/zero port isn't caught there, it's only caught much
	// later by the OS dial call, producing a cryptic "dial tcp 127.0.0.1:0: connect: can't assign
	// requested address" instead of a message that says what's actually missing. Checked here,
	// after the -cs-rt refresh block above (which can replace port with a fresh server list
	// entry), so a config/flag omission that IS resolved by -cs-rt doesn't false-positive.
	if port <= 0 {
		slog.Error("cross-server login: no port given (pass -cs-port or a session config with port)")
		os.Exit(1)
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

	// A serverInfo redirect (e.g. a real server merge moving this account to a different
	// zone/host/port) leaves result.Addr/Zone different from what was actually passed in --
	// and, independently, a -cs-rt refresh (whether or not any redirect also happened) can leave
	// result.AccessTok and/or result.GameUid different from what was actually passed in/on disk.
	// Persist the resolved values back to the session config, if we loaded one, comparing against
	// the ORIGINAL pre-refresh snapshot (origIP/origPort/origZone/origAccessTok/origGameUid, not
	// the possibly-already-refreshed ip/port/zone/gameUid locals -- see crossServerSaveBackNeeded's
	// doc comment) so the next run connects directly, and with a still-valid token/gameUid, instead
	// of re-following the same redirect or reusing values this run already knows were superseded.
	if o.configSavePath != "" {
		if newHost, newPortStr, splitErr := net.SplitHostPort(result.Addr); splitErr == nil {
			if newPort, atoiErr := strconv.Atoi(newPortStr); atoiErr == nil {
				if crossServerSaveBackNeeded(newHost, newPort, result.Zone, result.AccessTok, result.GameUid, origIP, origPort, origZone, origAccessTok, origGameUid) {
					updated := &SessionConfig{
						IP: newHost, Port: newPort, Zone: result.Zone,
						GameUid: result.GameUid, DeviceID: deviceID,
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
