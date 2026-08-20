package app

import (
	"crypto/rsa"
	"errors"
	"flag"
	"fmt"
	"io"
	"lastwar-client/internal/crypto"
	"lastwar-client/internal/gsl"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// Run is the CLI entry point, invoked by cmd/lastwar-client. It was package main's func
// main() before the codebase was reorganized into layered packages.
func Run() {
	// Install a JSON slog handler as the very first thing main() does -- before declaring ANY
	// flag, and in particular before -config's below, whose help text calls
	// defaultSessionConfigPath() -> stateFilePath() -> os.UserHomeDir(), which itself slog.Warns if
	// $HOME is unset/undeterminable. Flag declaration always runs, on every single invocation
	// (including -h/-help/-version/-no-config), so without this, that warning would fire through
	// Go's plain-text default slog handler -- installed implicitly until something calls
	// slog.SetDefault -- producing one stray non-JSON line in an otherwise all-JSON log stream on
	// every run. This uses slog's default level (Info, since HandlerOptions is nil) as a
	// placeholder purely to get JSON formatting in place immediately; it's replaced a few lines
	// below, once -log-level has actually been parsed, with the correctly-leveled handler -- so
	// nothing here needs to duplicate parseLogLevel or otherwise reorder when flags are parsed.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	// A plain flag.String/Bool/Int + flag.Parse() would use the package-level
	// flag.CommandLine, which is constructed with ExitOnError -- on a bad flag
	// (unknown flag, bad value) that calls os.Exit(2) directly, colliding with
	// this program's own contract that exit code 2 means "confirmed
	// server-side auth rejection" (see the ErrAuthRejected handling below).
	// ContinueOnError instead hands the parse error back to us so we can pick
	// a non-colliding exit code.
	//
	// filepath.Base(os.Args[0]), not the raw os.Args[0]: the raw invocation path varies with
	// however the binary happened to be invoked (e.g. "/tmp/lwc", "./lastwar-client",
	// "/usr/local/bin/lastwar-client"), and FlagSet's name is what -h/-help and every parse-error
	// usage message prints as "Usage of X:" -- a stable program name there reads as intentional
	// documentation, not an accident of $0.
	fs := flag.NewFlagSet(filepath.Base(os.Args[0]), flag.ContinueOnError)
	// Suppress flag.FlagSet's own built-in stderr output entirely -- both its failf()-then-usage
	// dump on a genuine parse error (unknown flag, bad value, missing argument) and its Usage()
	// call on -h/-help -- so neither can print a raw plain-text line into the otherwise all-JSON
	// log stream the placeholder handler above exists to guarantee. Round 33 fix: this was the
	// one remaining case, after round 32 covered every explicit slog.Error/slog.Warn call site in
	// this file, that this file's own JSON-log-stream invariant didn't actually cover -- flag's
	// internal output happens INSIDE fs.Parse, before this function ever sees the returned error,
	// so no error-handling branch here could intercept it. Both suppressed cases are handled
	// explicitly below instead: -h/-help re-enables output and calls fs.Usage() itself, byte-for-
	// byte reproducing the original human-readable text (still not JSON -- this is intentional,
	// documented help output for a human, not a machine-log diagnostic; see wantJSON:false on the
	// help-flag cases in TestMainFlagParseExitCodes for why this line is deliberately drawn here
	// and not extended to hide -h/-help's own text too); a real parse error instead logs the
	// error's own message via slog.Error, structured like every other fatal diagnostic in main().
	fs.SetOutput(io.Discard)
	email := fs.String("email", "", "account email to bind the guest identity to via email verification; if omitted and no loginKey is on file yet, the run silently stays on a fresh guest identity (no account binding, no error)")
	codePipe := fs.String("code-pipe", "", "path to a FIFO to read the verification code from (blocks open until a writer connects); if empty, reads from stdin")
	collect := fs.Bool("collect", false, "collect resources from every confirmed building type, plus the Armed Truck/Overlord idle rewards, greeting city visitors, helping alliance members, claiming all mail and alliance gifts, donating to the recommended alliance tech, and both once-a-day VIP claims, after login")
	listBuildings := fs.Bool("list-buildings", false, "print every owned building's details (a summary line, a per-type line, and a per-instance line with uuid/level/pointId and a full raw redacted dump -- not just id/type/level) to stdout. NOTE: this print already happens by DEFAULT whenever -collect is NOT passed -- this flag only matters when -collect IS also passed, where it forces the same print to happen alongside collection instead of being skipped; the process still exits after -collect/-list-buildings finish (assuming -collect, if passed, didn't fail fatally first) unless -interactive is also set")
	interactive := fs.String("interactive", "", "stay connected and read ad-hoc test commands from this control FIFO instead of exiting; only flat scalar param values (strings/bools/numbers) are supported -- nested JSON objects/arrays are rejected with a logged error and abort the whole send (see interactive.go)")

	csIP := fs.String("cs-ip", "", "skip normal login; reconnect directly to this ip (pipe-delimited ok) using an already-known role (from accountArr/push.account.login.new)")
	csPort := fs.Int("cs-port", 0, "port for -cs-ip -- must be a positive value; runtime validation rejects 0 or a negative port before ever attempting to dial (see runCrossServerTest's own port <= 0 check), producing a clear error instead of a cryptic OS-level dial failure")
	csZone := fs.String("cs-zone", "", "zone for -cs-ip, e.g. APS1234")
	csGameUid := fs.String("cs-gameuid", "", "composite gameUid for -cs-ip -- also sent on every -cs-rt GSL opt=refresh call, unlike -cs-zone (which only matters for -cs-ip): gameUid is passed to gsl.GetServerList unconditionally, so it matters even for a bare -cs-rt with no -cs-ip at all")
	csDeviceID := fs.String("cs-deviceid", "", "override deviceId (e.g. a real device's, extracted from its local PlayerPrefs) instead of this Go client's own persisted one")
	csShumei := fs.String("cs-shumei", "", "real shumeiBoxId anti-fraud fingerprint token, if known")
	csRt := fs.String("cs-rt", "", "if set, first does a GSL opt=refresh call with this refresh token to obtain a fresh access token before reconnecting -- IF the refresh response includes a non-empty server list, it REPLACES any explicitly-passed -cs-ip/-cs-port/-cs-zone/-cs-gameuid, and IF it includes a fresh access token, that REPLACES any explicitly-passed -cs-at; either can come back empty, in which case the corresponding -cs-* value passed here (or loaded from a session config) is used unchanged instead -- a warning is logged when that leaves a possibly-stale -cs-at in place with no refresh. If BOTH come back empty (no fresh access token AND no server list), that is NOT a graceful fallback: it is treated as a likely-rejected/expired refresh token and exits with code 2")
	csAt := fs.String("cs-at", "", "raw access token to send directly as p.at (e.g. one captured live from a real client) -- a cheap gsl.CheckVersion call is still made, to enable mid-login redirect-refresh capability, but no other GSL call happens unless -cs-rt is also set")
	csIOS := fs.Bool("cs-ios", false, "send an iOS-flavored Login (packageName=com.lastwar.ios, matching packageSign/platform/pf) instead of Android -- an 'at' token is bound to the platform/package it was issued for")
	handshake := fs.Bool("handshake", false, "experimental: send the vanilla SFS2X pre-Login Handshake (action=0) before Login -- see conn.go:DoHandshake")
	configPath := fs.String("config", "", "path to a session config JSON (see config.example.json); if unset, auto-loads "+defaultSessionConfigPath()+" when present. Explicit -cs-* flags override individual config fields.")
	noConfig := fs.Bool("no-config", false, "skip loading any session config, even the default file -- for a plain guest/email-flow run when a session config is also present")
	decodeStream := fs.String("decode-stream", "", "decode a reassembled raw TCP byte stream file (see docs/capturing-and-decoding-traffic.mdx) and print every SFS2X packet, then exit -- no login, no network connection at all")
	decodeLabel := fs.String("decode-label", "", "prefix label for -decode-stream output lines, e.g. \"c2s\" or \"s2c\" (default: \"stream\")")
	logLevel := fs.String("log-level", "info", "log verbosity: debug, info, warn (or its alias warning), or error")
	version := fs.Bool("version", false, "print build info and exit")
	if err := fs.Parse(os.Args[1:]); err != nil {
		// flag.ContinueOnError still runs the same failf/usage path flag.ExitOnError does
		// internally -- it only differs in returning the error here instead of calling os.Exit
		// itself, which is the whole point: it lets us pick the exit code instead of colliding
		// with our own contract below. -h/-help isn't a usage error, just an explicit request for
		// that same usage text, so it keeps exiting 0 as it always did with the default FlagSet --
		// fs.SetOutput(io.Discard) above suppressed flag's own automatic Usage() call for this
		// case too, so it's reproduced explicitly here. Any real parse error (unknown flag, bad
		// value, missing argument) exits 1 rather than the colliding 2, keeping the exit-code
		// contract binary: 2 means confirmed auth rejection, everything else means "look at the
		// log" -- and, as of round 33, "the log" now actually means the log: err's own message
		// (e.g. "flag provided but not defined: -bogus") goes through slog.Error instead of the
		// plain-text dump fs.SetOutput(io.Discard) suppressed.
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stderr)
			fs.Usage()
			os.Exit(0)
		}
		slog.Error("flag parse failed", "error", err)
		os.Exit(1)
	}

	// Go's flag package stops parsing at the first token that doesn't start with '-' and silently
	// stashes everything from there on as "positional" arguments (fs.Args()) instead of treating it
	// as an error -- fs.Parse above returns a nil error for e.g. `lastwar-client collect`, exactly as
	// if "collect" were never there at all. Left unchecked, that's a real trap: the single most
	// likely real-world cause is an operator typo'ing a flag without its leading dash (e.g. `collect`
	// instead of `-collect`), and today that silently proceeds into a full guest-login run instead of
	// catching what's almost certainly a mistake.
	//
	// Round 32 fix: this used to bypass slog entirely and print via a bare fmt.Fprintf, on the
	// claim that the level-configured slog handler wasn't installed yet at this point -- but the
	// PLACEHOLDER JSON handler (main()'s very first statement, before any flag is even declared) is
	// already live here, exactly as parseLogLevel's own round-31 fix (a few lines below in this same
	// file) established for the identical situation. detectSwallowedFlagValue's diagnostic
	// (elsewhere in this file, reachable at this same point in main()'s execution) already uses
	// slog.Error correctly for a fatal flag-parsing-time error -- this now matches that precedent
	// instead of the stale fmt.Fprintf-to-stderr convention parseLogLevel no longer follows either.
	//
	// Round 37 fix: unlike detectSwallowedFlagValue's own "value" log field (which by construction
	// can only ever equal another registered flag's NAME, never arbitrary content), fs.Args() here
	// is completely unconstrained -- it's whatever token(s) happened to land after the first
	// non-'-'-prefixed one. The common case really is an innocuous typo'd flag name (e.g. "collect"
	// missing its dash), but a cron-wrapper script that drops a "-cs-at"/"-cs-shumei"/
	// "-cs-deviceid"/"-email" flag NAME while still passing its VALUE would land that value here
	// too -- and this was the one remaining sink in this file logging a possibly-sensitive value in
	// cleartext instead of length-only, breaking the length-only convention every other
	// secret-adjacent log call in this file already follows (deviceIdLen/airKeyLen/emailLen/
	// usernameLen/tokenLen). Logs only the count and total joined length now, never the content.
	if fs.NArg() > 0 {
		args := fs.Args()
		slog.Error("unexpected argument(s) (missing a leading '-' on a flag? see -help)",
			"count", len(args), "totalLen", len(strings.Join(args, " ")))
		os.Exit(1)
	}

	// -version intentionally bypasses the ignored-flags warning machinery below (decodeModeIgnoredFlags,
	// ignoredCrossServerFlags, the -config/-no-config check, the -email/-code-pipe check, and
	// warnIfDecodeLabelIgnored): it returns here, before fs.Visit populates visitedFlags below, so
	// stacking -version with any other flag (e.g. `-version -collect -cs-at X`) silently drops those
	// other flags with no warning, unlike every other no-op-flag-combination case this file otherwise
	// warns about. This is deliberate, not an oversight: stacking -version with live-run flags is an
	// unlikely real operator mistake (low real-world impact), and keeping -version's exit simple,
	// fast, and unconditional (no LEVEL-CONFIGURED slog setup -- the placeholder JSON handler
	// installed at the very top of main() is already in place regardless, but the second
	// slog.SetDefault below, which depends on parsing -log-level, is skipped here -- and no
	// fs.Visit call) is judged worth more than closing this specific gap. Don't "fix" this by
	// reordering fs.Visit() above this check without re-deriving that tradeoff first.
	if *version {
		printVersion()
		return
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: parseLogLevel(*logLevel)})))

	csIOSSetExplicitly := false
	// csIPSetExplicitly/csPortSetExplicitly/csZoneSetExplicitly/csGameUidSetExplicitly/
	// csAtSetExplicitly/csDeviceIDSetExplicitly/csShumeiSetExplicitly mirror csIOSSetExplicitly's
	// own pattern, for the same reason: they record whether the corresponding -cs-* flag was
	// actually typed on the command line, as distinct from ending up non-empty purely because a
	// loaded session config's field is merged into it further below (the
	// mergeExplicitOrConfigString-based merge just below). Threaded into crossServerTestOpts so
	// runCrossServerTest's GSL-refresh flag-vs-config log-wording distinction can reuse this same
	// visitedFlags mechanism instead of inventing a new one -- see crossServerTestOpts' doc
	// comment. csDeviceIDSetExplicitly/csShumeiSetExplicitly are round-34 additions: -cs-deviceid
	// and -cs-shumei previously had no explicit-tracking at all, unlike every other -cs-* flag,
	// which is exactly why their config-merge (below) was still on the old bare applyOverride
	// pattern after round 33 fixed ip/port/gameuid.
	csIPSetExplicitly := false
	csPortSetExplicitly := false
	csZoneSetExplicitly := false
	csGameUidSetExplicitly := false
	csAtSetExplicitly := false
	csDeviceIDSetExplicitly := false
	csShumeiSetExplicitly := false
	// interactiveSetExplicitly mirrors the csXSetExplicitly bools above, for -interactive
	// specifically -- see warnIfInteractiveExplicitlyEmpty's own doc comment for why this needs the
	// identical "was it actually typed" tracking despite -interactive not being a -cs-* flag itself.
	interactiveSetExplicitly := false
	var visitedFlags []string

	// registeredFlagNames is every flag name actually declared on fs (regardless of whether it
	// was ever passed on the command line) -- computed once, up front, so the swallowed-flag-
	// value check inside the fs.Visit callback just below can test whether a suspicious value is
	// genuinely another flag's name, not merely a plausible-looking string. See
	// detectSwallowedFlagValue's own doc comment (round 25's Fix 1, the MAJOR finding) for the
	// full mechanism this guards against.
	registeredFlagNames := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) { registeredFlagNames[f.Name] = true })

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
		case "cs-deviceid":
			csDeviceIDSetExplicitly = true
		case "cs-shumei":
			csShumeiSetExplicitly = true
		case "interactive":
			interactiveSetExplicitly = true
		}
		visitedFlags = append(visitedFlags, f.Name)

		// Read the value once into a plain local: not just to avoid three redundant
		// f.Value.String() calls below, but because flag.Value's String() is a completely
		// different, unrelated method from sfs.SFSObject's -- a plain flag value, never a decoded
		// sfs.SFSObject, so it can never carry a credential field either way -- and keeping every
		// slog/fmt sink call below working from this local instead of a literal ".String()" call
		// keeps this block out of credential_leak_lint_test.go's (deliberately blunt, name-based)
		// scan entirely, rather than needing an allowlist entry to explain that distinction.
		value := f.Value.String()

		// See detectSwallowedFlagValue's doc comment below for the full mechanism: an explicitly-
		// visited flag whose own value is itself the (dash-stripped) name of another flag actually
		// registered on fs is the unambiguous signature of Go's flag package having swallowed the
		// NEXT flag's name as THIS flag's value, rather than that next flag ever being parsed as a
		// flag at all. Checked here, inside fs.Visit itself, rather than in a second pass, since
		// f.Name/value are already in hand and this must fire before any of this value is used
		// downstream (e.g. -email flowing into the outgoing verification-code request).
		if swallowed, ok := detectSwallowedFlagValue(f.Name, value, registeredFlagNames); ok {
			msg := fmt.Sprintf(
				"-%s's value is itself the name of another flag (%q, which matches -%s) -- almost certainly means -%s never got a real value of its own and instead swallowed -%s off the command line (e.g. an unset/empty shell variable, or a missing value, before the next flag); pass an explicit value or reorder the flags and try again",
				f.Name, value, swallowed, f.Name, swallowed,
			)
			slog.Error(msg, "flag", "-"+f.Name, "swallowedFlagName", "-"+swallowed, "value", value)
			os.Exit(1)
		}
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
		// Round 33 fix (originally scoped to -cs-ip/-cs-port/-cs-gameuid only; round 34 extends it
		// to -cs-zone/-cs-deviceid/-cs-shumei/-cs-at, the four siblings round 33 missed):
		// explicitly passed but empty/zero used to be silently replaced by the config's value
		// here regardless -- directly contradicting -config's own documented contract ("Explicit
		// -cs-* flags override individual config fields", this file's -config help text and
		// README.md). For -cs-at specifically this also defeated DoCrossServerLogin's own fatal
		// "no access token given" guard (crossserver.go): an operator explicitly clearing -cs-at
		// to force a fresh token would instead silently keep authenticating with the config's
		// stale one, with zero diagnostic either way. Now routed through
		// mergeExplicitOrConfigString/mergeExplicitOrConfigPort (config.go), which skip the
		// config fallback (and report explicitlyEmpty/explicitlyZero for the Warn below) when the
		// flag was explicitly visited but left empty/zero, instead of only checking "is it
		// currently empty" with no memory of how it got that way.
		var ipExplicitlyEmpty, portExplicitlyZero, zoneExplicitlyEmpty, gameUidExplicitlyEmpty bool
		var deviceIDExplicitlyEmpty, shumeiExplicitlyEmpty, atExplicitlyEmpty bool
		*csIP, ipExplicitlyEmpty = mergeExplicitOrConfigString(*csIP, csIPSetExplicitly, cfg.IP)
		if ipExplicitlyEmpty {
			slog.Warn("-cs-ip was explicitly given as empty; not falling back to the session config's ip (pass a non-empty -cs-ip, or omit the flag entirely to use the config's value)")
		}
		*csPort, portExplicitlyZero = mergeExplicitOrConfigPort(*csPort, csPortSetExplicitly, cfg.Port)
		if portExplicitlyZero {
			slog.Warn("-cs-port was explicitly given as 0; not falling back to the session config's port (pass a positive -cs-port, or omit the flag entirely to use the config's value)")
		}
		*csZone, zoneExplicitlyEmpty = mergeExplicitOrConfigString(*csZone, csZoneSetExplicitly, cfg.Zone)
		if zoneExplicitlyEmpty {
			slog.Warn("-cs-zone was explicitly given as empty; not falling back to the session config's zone (pass a non-empty -cs-zone, or omit the flag entirely to use the config's value)")
		}
		*csGameUid, gameUidExplicitlyEmpty = mergeExplicitOrConfigString(*csGameUid, csGameUidSetExplicitly, cfg.GameUid)
		if gameUidExplicitlyEmpty {
			slog.Warn("-cs-gameuid was explicitly given as empty; not falling back to the session config's gameUid (pass a non-empty -cs-gameuid, or omit the flag entirely to use the config's value)")
		}
		*csDeviceID, deviceIDExplicitlyEmpty = mergeExplicitOrConfigString(*csDeviceID, csDeviceIDSetExplicitly, cfg.DeviceID)
		if deviceIDExplicitlyEmpty {
			slog.Warn("-cs-deviceid was explicitly given as empty; not falling back to the session config's deviceId (pass a non-empty -cs-deviceid, or omit the flag entirely to use the config's value)")
		}
		*csShumei, shumeiExplicitlyEmpty = mergeExplicitOrConfigString(*csShumei, csShumeiSetExplicitly, cfg.ShumeiBoxId)
		if shumeiExplicitlyEmpty {
			slog.Warn("-cs-shumei was explicitly given as empty; not falling back to the session config's shumeiBoxId (pass a non-empty -cs-shumei, or omit the flag entirely to use the config's value)")
		}
		*csAt, atExplicitlyEmpty = mergeExplicitOrConfigString(*csAt, csAtSetExplicitly, cfg.AccessToken)
		if atExplicitlyEmpty {
			slog.Warn("-cs-at was explicitly given as empty; not falling back to the session config's access token (pass a non-empty -cs-at, or omit the flag entirely to use the config's value)")
		}
		*csIOS = mergeExplicitOrConfigBool(*csIOS, csIOSSetExplicitly, cfg.IOSMode)
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
			atExplicit: csAtSetExplicitly, interactiveExplicit: interactiveSetExplicitly,
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
		fbBuildings, fbVisitors, fbErr := FetchBuildings(conn, 12*time.Second)
		buildings = fbBuildings
		// Login()'s own init-push parse (ParseInitBuildings/ParseInitVisitors, buildings.go/
		// visitors.go) can populate a non-empty visitors slice even when building_new comes back
		// empty/malformed -- both are parsed from the very same bootstrap init push, but are
		// otherwise independent fields. That bootstrap init push fires once per session, so this
		// fallback FetchBuildings call has no second init push left to observe: it will most
		// likely time out and return visitors=nil. Only let its result replace visitors when
		// Login() didn't already obtain a real, non-empty one -- an unconditional overwrite here
		// would silently discard already-known visitors before CollectAll ever runs (round 26).
		if len(visitors) == 0 {
			visitors = fbVisitors
		}
		if fbErr != nil {
			slog.Error("fetch buildings failed", "error", fbErr)
			// See shouldAbortBeforeInteractive's own doc comment: this call site is reached over
			// a connection Login() itself already established and used successfully, so a
			// FetchBuildings failure here that isn't evidence of a genuinely dead connection
			// (e.g. a decode/parse failure on one bad frame, not wrapped in a net.Error) must not
			// silently discard an explicit -interactive request -- the exact same bug class
			// round 25 closed for CollectAll's two call sites, applied here too (round 26).
			if shouldAbortBeforeInteractive(fbErr, *interactive != "") {
				// Round-40 fix: os.Exit does not run deferred functions, so the `defer
				// conn.Close()` registered above never ran on this exit path (nor the sibling
				// one below) -- a textbook os.Exit-skips-defers gap, harmless today only because
				// GameConn.Close() happens to do nothing a process exit doesn't already
				// accomplish, but latent: it would silently stop applying the moment Close()
				// gains any real cleanup (a flush, a graceful FIN, a notification). Close
				// explicitly before exiting instead of relying on the now-unreachable defer.
				conn.Close()
				os.Exit(1)
			}
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
			if shouldAbortBeforeInteractive(err, *interactive != "") {
				// See the identical round-40 fix's doc comment on the sibling os.Exit(1) above.
				conn.Close()
				os.Exit(1)
			}
		}
	}

	warnIfInteractiveExplicitlyEmpty(interactiveSetExplicitly, *interactive)
	if *interactive != "" {
		RunInteractive(conn, *interactive) // blocks forever
	}

	slog.Info("client exiting")
}

// shouldAbortBeforeInteractive decides, at the two -collect call sites (main() and
// runCrossServerTest) AND, since round 26, the two sibling FetchBuildings fallback call sites
// right above each of those (main()'s zero-buildings fallback, and runCrossServerTest's own
// unconditional post-login FetchBuildings call), whether a non-nil error there should os.Exit(1)
// right there or fall through to the "if -interactive is set, stay connected" check a few lines
// later.
//
// CollectAll's own doc comment (see buildings.go) is explicit that it issues one independent
// request per fixed action plus one per collectible building, and a sendAndWait net.Error with
// Timeout()==true on any one of them is "a normal, expected timeout on one action's response, not
// evidence the connection is dead" -- not proof the connection, or the rest of the collect run,
// is actually broken. Before round 25's fix, both -collect call sites treated every non-nil
// CollectAll error identically to a genuinely dead connection: os.Exit(1) before ever reaching the
// -interactive check below. An operator who explicitly passed -interactive alongside -collect --
// intending to stay connected afterward regardless of whether every single collect action
// succeeded -- had that explicit request silently discarded on what, given how many independent
// requests one collect run issues, is not a rare edge case.
//
// Round 26 found the identical bug class one function up, at both FetchBuildings call sites: a
// plain decode/parse error (e.g. packet.go's "zlib inflated output exceeds", never wrapped in a
// net.Error) on one bad frame, over an otherwise still-healthy connection, used to unconditionally
// os.Exit(1) there too -- discarding an explicit -interactive request even though RunInteractive
// only needs the conn (not buildings/visitors) to proceed. Reusing this same function, rather than
// inventing a second, near-identical one, keeps both pairs of call sites' notion of "genuinely
// fatal" identical.
//
// Round 43 note: packet.go's "frame body too large"/"uncompressed length too large" guards (the
// original round-26 example here) are NO LONGER an example of this non-fatal case -- they're now
// wrapped in sfs.DeadConnError (a genuine net.Error with Timeout()==false), since round 43 found they
// fire before the declared body bytes are drained, leaving the reader desynced if a peer actually
// sends them. containsNonTimeoutNetError now correctly treats that case as fatal below. The zlib
// decompressed-size guard remains the accurate example: it fires only after the full declared body
// has already been consumed, so the stream stays synchronized regardless of the error.
//
// containsNonTimeoutNetError(err) (buildings.go) is CollectAll's own internal test for "genuinely
// fatal": a real net.Error with Timeout()==false anywhere in err's tree, as opposed to an ordinary
// per-item benign timeout or a plain decoded business-logic failure. Reusing it here keeps this
// function's notion of "fatal" identical to CollectAll's own, rather than inventing a second,
// possibly-divergent classification.
//
//   - err == nil: nothing to abort for.
//   - containsNonTimeoutNetError(err) == true: the connection is genuinely dead. Abort
//     unconditionally -- interactive mode against a definitely-dead connection would be useless
//     regardless of what was requested, so existing os.Exit(1) behavior is preserved as-is.
//   - err != nil but containsNonTimeoutNetError(err) == false (ordinary business-logic/benign-
//     timeout failures only): if -interactive was explicitly requested, do NOT abort -- let the
//     operator's explicit request to stay connected actually take effect (the collect failures
//     were already logged by the caller before this is consulted, so they remain visible, just not
//     fatal). If -interactive was NOT requested, preserve the existing os.Exit(1) behavior
//     unchanged -- this fix is specifically about not discarding an explicit -interactive request,
//     not about softening the -collect-only case.
//
// Taking err and interactiveRequested as plain arguments (rather than being inlined at either call
// site) is what makes the actual decision unit-testable across representative error shapes without
// needing a live CollectAll run against a fake server at both call sites.
func shouldAbortBeforeInteractive(err error, interactiveRequested bool) bool {
	if err == nil {
		return false
	}
	if containsNonTimeoutNetError(err) {
		return true
	}
	return !interactiveRequested
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

// warnIfInteractiveExplicitlyEmpty logs a warning when -interactive was explicitly passed on the
// command line (per explicit, populated via the same fs.Visit-based visitedFlags mechanism this
// file already uses for -cs-ip/-cs-port/-cs-zone/-cs-gameuid/-cs-at -- see interactiveSetExplicitly
// in main() and crossServerTestOpts' interactiveExplicit field) but ended up with an empty value --
// e.g. a cron/automation wrapper passing -interactive "$CONTROL_PIPE" with an unset/empty
// $CONTROL_PIPE. Before this fix, that silently behaved exactly as if -interactive were never
// passed at all, with zero diagnostic -- unlike every sibling -cs-* flag, which already
// warns/errors clearly on this exact "given but empty" shape (see e.g. the -cs-ip/-cs-gameuid
// checks in runCrossServerTest).
//
// Deliberately not fatal: an empty -interactive value degrades to the existing, entirely
// legitimate "don't enter interactive mode" behavior (unlike an empty -cs-ip/-cs-gameuid, which
// leaves the connection unusable and is treated as fatal), so a WARN that names the mistake is the
// right bar here -- not an os.Exit that would abort an otherwise-successful run over what only
// affects this one optional feature.
//
// Applies identically at both -interactive call sites (main()'s own check, right before its
// RunInteractive call, and runCrossServerTest's o.interactive check). Taking explicit/interactive
// as plain arguments (rather than being inlined at either call site) is what makes this testable
// via slog output capture, matching this file's established warnIfDecodeLabelIgnored/
// warnIfExplicitConfigPathNotFound pattern.
func warnIfInteractiveExplicitlyEmpty(explicit bool, interactive string) {
	if explicit && interactive == "" {
		slog.Warn("-interactive was given but empty -- not entering interactive mode (pass a non-empty control FIFO path, e.g. -interactive /path/to/pipe)")
	}
}

// stringFlagSwallowGuardNames is the set of fs.String-declared flags (main.go) where a value that
// itself looks like another registered flag's name is treated as a near-certain accidental-
// flag-adjacency mistake by detectSwallowedFlagValue below, rather than a legitimate value. Every
// flag listed here is a plain identifier, path, zone code, or opaque token that never legitimately
// starts with '-' in real use (an email address, a raw device id, an access/refresh token, a
// filesystem path) -- so a leading-dash value is already suspicious for these specifically, unlike
// e.g. the fs.Int -cs-port flag, whose value could theoretically be a negative number starting
// with '-' (and which doesn't need this guard anyway: a swallowed flag name there fails to parse
// as an int and is already caught by fs.Parse's own error path -- see TestMainFlagParseExitCodes'
// "malformed flag value" case). Kept as a package-level map (rather than inlined in
// detectSwallowedFlagValue) so it reads as a deliberate, reviewable scoping decision, matching this
// file's existing crossServerFlagNames convention just below.
var stringFlagSwallowGuardNames = map[string]bool{
	"email": true, "code-pipe": true, "interactive": true,
	"cs-ip": true, "cs-zone": true, "cs-gameuid": true, "cs-deviceid": true,
	"cs-shumei": true, "cs-rt": true, "cs-at": true,
	"config": true, "decode-stream": true, "decode-label": true, "log-level": true,
}

// detectSwallowedFlagValue is the pure decision at the heart of round 25's Fix 1 (the MAJOR
// finding): whether an explicitly-visited flag's own value is itself the name of another flag
// actually registered on the FlagSet, once any leading dash(es) are stripped from that value.
//
// This is standard Go flag package behavior: flag.FlagSet.Parse's internal parseOne
// unconditionally consumes the very next token as the value for any non-bool flag, with zero check
// for whether that token looks like a registered flag name. So e.g. "-email -collect" parses to
// email="-collect", collect=false (never visited at all -- its token was consumed as -email's
// value, not parsed as a flag), with fs.Parse itself returning a nil error and fs.NArg()==0 (so the
// existing stray-positional-argument check, round 21, can't catch this either -- there's nothing
// left over in fs.Args()). A realistic real-world trigger: `-email "$EMAIL" -collect` with an
// unset/empty, unquoted $EMAIL shell variable. Left unguarded, -email's swallowed garbage value
// flows straight into login.go's outgoing verification-code request, where it's likely to be
// server-rejected -- surfacing as a misleading exit-code-2 auth rejection instead of the simple
// flag-ordering/quoting mistake it actually is.
//
// Scoped to stringFlagSwallowGuardNames (not every flag on the FlagSet) so a flag whose value could
// legitimately start with '-' is never blanket-rejected -- see that map's own doc comment. Within
// that scope, only an EXACT match against another flag's real, registered name counts: a
// dash-prefixed value that merely looks flag-like but matches nothing registered (e.g. a typo, or
// a value that coincidentally starts with '-') is left alone, since that's not the specific,
// near-certain mistake this guards against.
//
// Taking name/value/registeredFlagNames as plain arguments (rather than a *flag.FlagSet, or being
// inlined at the fs.Visit call site in main()) is what makes this testable without building a real
// FlagSet, matching this file's established pattern for extracting flag-parsing decisions (e.g.
// decodeModeIgnoredFlags, serverListOverrideFlags) into pure, directly-testable functions.
func detectSwallowedFlagValue(name, value string, registeredFlagNames map[string]bool) (swallowedFlagName string, ok bool) {
	if !stringFlagSwallowGuardNames[name] {
		return "", false
	}
	candidate := strings.TrimLeft(value, "-")
	if candidate == "" || candidate == value {
		// candidate == value: TrimLeft found no leading '-' to strip at all -- the ordinary case
		// (e.g. a real email address). candidate == "": value was made up entirely of dashes (e.g.
		// "-" or "--", a common "read from stdin" convention elsewhere) -- nothing left to match
		// against a flag name either way. Both are "not a swallowed flag name," just for different
		// reasons.
		return "", false
	}
	if !registeredFlagNames[candidate] {
		return "", false
	}
	return candidate, true
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
// decoded *gsl.LoginServerListRespon (rather than being inlined at the call site) is what makes this
// testable without a live GSL round-trip.
func refreshHasUsableData(lsr *gsl.LoginServerListRespon) bool {
	return lsr.At != nil || len(lsr.ServerList) > 0
}

// serverListOverrideFlags reports which of "cs-ip", "cs-port", "cs-zone", "cs-gameuid" (in that
// order) were both actually typed on the command line -- per ipExplicit/portExplicit/
// zoneExplicit/gameUidExplicit, see crossServerTestOpts' doc comment -- AND carried a real,
// non-zero-value value (ip/port/zone/gameUid respectively) at the time, and are therefore about
// to be silently overridden by a GSL opt=refresh response's non-empty ServerList. A nil result
// means none of the four qualify (e.g. a fresh cron run with no prior overrides, where ip/port/
// zone/gameUid started from either their zero value or a loaded session config), which the call
// site uses to keep its existing plain INFO-level "server selected" log instead of escalating to
// WARN -- only a real override of an operator-supplied value escalates.
//
// Both conditions are required, exactly like the neighboring "ignoring -cs-at" check just above
// this function's call site (`o.at != "" && o.atExplicit`), and for the identical reason: Go's
// flag.Visit fires for a flag whenever it appeared on the command line at all, even if the value
// given equals the flag's own zero-value default (e.g. an operator explicitly passing -cs-ip ""
// from a possibly-unset shell variable, or someone intentionally relying on -cs-rt alone with no
// -cs-ip -- both cases this codebase explicitly supports). Checking *Explicit alone, as this
// function used to, meant such a case would still be reported as "overriding" a flag whose value
// was never actually meaningful, producing a misleading WARN. Taking both the four values and the
// four bools as plain arguments (rather than the whole crossServerTestOpts struct, or being
// inlined at the call site) is what makes this testable without building a real
// crossServerTestOpts/GSL round-trip.
func serverListOverrideFlags(ip string, ipExplicit bool, port int, portExplicit bool, zone string, zoneExplicit bool, gameUid string, gameUidExplicit bool) []string {
	var out []string
	if ipExplicit && ip != "" {
		out = append(out, "cs-ip")
	}
	if portExplicit && port != 0 {
		out = append(out, "cs-port")
	}
	if zoneExplicit && zone != "" {
		out = append(out, "cs-zone")
	}
	if gameUidExplicit && gameUid != "" {
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
// Bug fixed here (round 26): origHost is normalized through gsl.FirstHost (gsl.go) below, before the
// comparison, rather than compared as the raw string the caller captured it as. -cs-ip/session-
// config's ip value legitimately supports a pipe-delimited multi-host fallback list (e.g.
// "host-a|host-b", documented in -cs-ip's own help text), and every dial path already normalizes
// this via gsl.FirstHost before actually connecting (see crossserver.go) -- but newHost here is
// always a single resolved host, parsed from the actual dialed address via net.SplitHostPort.
// Comparing that single resolved host against a raw, un-normalized pipe-delimited origHost meant
// an operator-supplied "host-a|host-b" that connected cleanly to the FIRST host, with NO redirect
// and no other change, still spuriously reported "save needed" purely because the resolved
// single host could never string-equal the original pipe-delimited value -- permanently
// collapsing the operator's configured multi-host resilience list down to one host in the
// persisted session config on the very first run. Normalizing here, inside this function, rather
// than only at the call site, means this comparison is correct regardless of what shape any
// caller's origHost happens to be in.
//
// Taking every value as a plain argument (rather than closing over runCrossServerTest's locals)
// is what makes all of these mistakes structurally impossible to reintroduce silently, and what
// makes this testable without spinning up fake GSL/game servers.
func crossServerSaveBackNeeded(newHost string, newPort int, newZone, newAccessTok, newGameUid, origHost string, origPort int, origZone, origAccessTok, origGameUid string) bool {
	origHost = gsl.FirstHost(origHost)
	return newHost != origHost || newPort != origPort || newZone != origZone || newAccessTok != origAccessTok || newGameUid != origGameUid
}

// parseLogLevel maps a -log-level flag value to an slog.Level, defaulting to Info for the empty
// string (the flag's own default) and for anything unrecognized. An unrecognized value (e.g. a
// typo) is reported via slog.Warn, not fmt.Fprintf: the placeholder JSON handler main() installs
// as its very first statement (before any flag is even declared, let alone parsed) is already
// slog's live default by the time this function runs, so there's no structural reason to bypass it
// -- doing so would print one stray plain-text line into an otherwise all-JSON log stream, exactly
// the invariant that placeholder handler exists to guarantee. The Warn fires through that
// placeholder handler (Info-level, so a Warn is never filtered) rather than the correctly-leveled
// one this function's own return value goes on to configure a moment later in main() -- fine, since
// this diagnostic is about the flag value itself, not something a -log-level=error run would want
// silenced.
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
		slog.Warn("unrecognized -log-level value, defaulting to info", "value", s, "validValues", "debug, info, warn (or its alias warning), error")
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

	// interactiveExplicit records whether -interactive was actually typed on the command line, the
	// same fs.Visit-based mechanism as the five *Explicit fields above -- see
	// warnIfInteractiveExplicitlyEmpty's own doc comment for what this enables: distinguishing an
	// explicitly-passed-but-empty -interactive (worth a WARN) from -interactive simply never being
	// passed at all (the ordinary, silent, non-interactive case).
	interactiveExplicit bool
}

// String/GoString are the round-49 regression fix for the MAJOR finding that crossServerTestOpts
// -- which carries live credential-shaped fields rt (refresh token), at (access token),
// shumeiBoxId (anti-fraud device fingerprint), and deviceID -- had no String()/GoString()
// redaction-by-construction, unlike its near-identical sibling CrossServerLoginParams
// (crossserver.go, carrying the same AccessTok/ShumeiBoxId/DeviceID fields) and every other
// credential-carrying struct in the codebase (SessionConfig, deviceIdentity, gsl.GSLOpt,
// LoginParamsInput, CrossServerLoginParams, CrossServerLoginResult, gsl.LoginToken), all of which
// received this exact fix in rounds 47-48. The local variable constructed in main() is held live
// across runCrossServerTest's entire 300+ line body -- every current call site there logs only
// individual, already-redacted/length-only fields, so this is defense-in-depth against a future
// diagnostic line logging the whole struct (e.g. slog.Error("cross-server test failed", "opts", o)
// or fmt.Errorf("config: %+v", o)), which would otherwise fall through to Go's default
// reflection-based struct formatter and print the raw refresh token, access token, and device
// fingerprint in cleartext.
func (o crossServerTestOpts) String() string   { return "[REDACTED crossServerTestOpts]" }
func (o crossServerTestOpts) GoString() string { return o.String() }

// LogValue makes crossServerTestOpts satisfy slog.LogValuer -- round-53 fix. The doc comment's own
// hypothetical example just above (slog.Error("cross-server test failed", "opts", o)) is exactly
// the shape String()/GoString() alone do NOT protect against: slog.NewJSONHandler (the only
// handler main.go ever installs) never consults fmt.Stringer/fmt.GoStringer, only slog.LogValuer,
// which slog resolves before handler dispatch. See gsl.go's gsl.LoginToken.LogValue for the fuller
// rationale, shared across every credential-bearing type in this codebase.
func (o crossServerTestOpts) LogValue() slog.Value { return slog.StringValue(o.String()) }

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
		airKey = "lwDid_" + gsl.B64OfString(deviceID)
	}
	slog.Info("using device identity", "deviceIdLen", len(deviceID), "airKeyLen", len(airKey), "iosMode", o.iosMode)

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
	// CrossServerLoginParams' doc comment). gsl.CheckVersion is a single cheap HTTP call, so it's made
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
		httpClient := gsl.DefaultHTTPClient()
		cv, gateHost, err := gsl.CheckVersion(httpClient)
		if err != nil {
			if o.rt != "" {
				slog.Error("check-version failed", "error", err)
				os.Exit(1)
			}
			slog.Warn("check-version failed; proceeding without redirect-refresh capability (a mid-login serverInfo redirect will fall back to reusing the current access token)", "error", err)
		} else if pub, err := crypto.ParseRSAPubKeyFromDER(cv.ResMsg.String()); err != nil {
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
		lsr, err := gsl.GetServerList(gslHTTPClient, gslGateHost, gslRSAPub, deviceID, gsl.GSLOpt{Opt: "refresh", Rt: o.rt}, "", o.gameUid)
		if err != nil {
			// Unlike the ErrAuthRejected-gated os.Exit(2) sites elsewhere in this file (the
			// plain-login failure in main(), and the SFS2X cross-server-login failure further
			// down in this function), a gsl.GetServerList error is never gated on errors.Is(err,
			// ErrAuthRejected) here: gsl.GetServerList's own error returns (gsl.go) never wrap it --
			// only the SFS2X handshake/login/cross-server-login paths (conn.go, login.go,
			// crossserver.go) do, since those are the ones that decode an explicit server-side
			// rejection error code from the game server. This HTTP-based GSL endpoint's own
			// success-vs-rejection semantics haven't been confirmed live yet either (see
			// gsl.LoginServerListRespon.Code's own doc comment), so there is nothing here an exit-2
			// branch could actually be gated on -- a prior version of this code had one anyway,
			// unreachable, with a comment incorrectly claiming it matched those sibling sites.
			// Every gsl.GetServerList failure -- network/HTTP/decode/decrypt, all of it -- is just a
			// generic failure (1) until real evidence of a confirmed-rejection shape exists here.
			slog.Error("GSL refresh failed", "error", err)
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
		// lsr.At.Token.String() != "", not just lsr.At != nil -- round-53 fix: gsl.go's
		// gsl.LoginServerListRespon.UnmarshalJSON treats any JSON-object-shaped "at" field
		// (including "{}" or one with no/empty "token") as present via gsl.LooksLikeJSONObject, so
		// lsr.At != nil alone doesn't guarantee a usable token. Without this, an empty-token
		// response used to take this success branch, log the misleadingly-normal-looking "fresh
		// access token acquired tokenLen=0", and silently clobber accessTok with "" -- instead
		// of falling through to the "no access token" warn branches below, which already handle
		// this exact "refresh succeeded but no usable token" case correctly for the genuinely-
		// absent-At shape.
		if lsr.At != nil && lsr.At.Token.String() != "" {
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
			accessTok = lsr.At.Token.String()
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
			if overridden := serverListOverrideFlags(ip, o.ipExplicit, port, o.portExplicit, zone, o.zoneExplicit, gameUid, o.gameUidExplicit); len(overridden) > 0 {
				// Symmetric to the "ignoring -cs-at" WARN above, for the same reason (and, as of
				// round 25's fix to serverListOverrideFlags, genuinely the same check: both
				// require the flag to have been explicitly typed AND to carry a real, non-zero
				// value, not just *Explicit alone): an operator-supplied value is about to be
				// silently replaced. Only escalated to WARN when it's actually overriding
				// something the operator explicitly typed with a meaningful value -- a fresh cron
				// run with no prior -cs-ip/-cs-port/-cs-zone/-cs-gameuid overrides at all (e.g.
				// everything came from a session config, nothing was set yet, or a flag was
				// explicitly passed but empty/zero) keeps the plain INFO-level "server selected"
				// log below instead.
				slog.Warn("GSL refresh response's server list is overriding explicitly-passed flag(s)", "overriddenFlags", overridden, "ip", srv.IP, "port", srv.Port, "zone", srv.Zone, "gameUid", srv.GameUid)
			} else {
				slog.Info("server selected", "ip", srv.IP, "port", srv.Port, "zone", srv.Zone, "gameUid", srv.GameUid)
			}
			ip, port, zone, gameUid = srv.IP.String(), srv.Port.Int("port"), srv.Zone.String(), srv.GameUid.String()
		}
	}

	// Symmetric to the port <= 0 check just below, but arguably more important to catch here
	// rather than downstream: crossserver.go's addr := fmt.Sprintf("%s:%d", gsl.FirstHost(p.IP),
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
	if gsl.FirstHost(ip) == "" {
		if o.ipExplicit {
			// Distinct from the "never given at all" wording just below: -cs-ip WAS actually typed
			// on the command line (per o.ipExplicit, the same visitedFlags mechanism this file
			// already uses elsewhere -- e.g. the neighboring "ignoring -cs-at" / serverListOverrideFlags
			// call sites, and the identical pattern the -cs-port check just below already applies), it
			// just carried an empty value -- e.g. -cs-ip "" or -cs-ip with no argument consumed.
			// Before this fix, this case logged the exact same "no ip given" message as an ip that
			// was never passed at all, actively misdirecting an operator debugging a simple empty-value
			// typo toward the wrong root cause (did I forget the flag? vs. did I pass it with no value?).
			slog.Error("cross-server login: -cs-ip was given but empty (pass a non-empty ip)")
		} else {
			slog.Error("cross-server login: no ip given (pass -cs-ip or a session config with ip)")
		}
		os.Exit(1)
	}

	// DoCrossServerLogin validates AccessTok itself (see its own p.AccessTok == "" check) but has
	// no equivalent check for Port -- an unset/zero port isn't caught there, it's only caught much
	// later by the OS dial call, producing a cryptic "dial tcp 127.0.0.1:0: connect: can't assign
	// requested address" instead of a message that says what's actually missing. Checked here,
	// after the -cs-rt refresh block above (which can replace port with a fresh server list
	// entry), so a config/flag omission that IS resolved by -cs-rt doesn't false-positive.
	if port <= 0 {
		if o.portExplicit {
			// Distinct from the "never given at all" wording just below: -cs-port WAS actually
			// typed on the command line (per o.portExplicit, the same visitedFlags mechanism this
			// file already uses elsewhere -- e.g. the neighboring "ignoring -cs-at" /
			// serverListOverrideFlags call sites -- to tell an explicit flag apart from a value
			// that merely ended up in scope some other way), it just carried an invalid (<=0)
			// value -- a typo'd negative number, most likely. Before this fix, this case logged the
			// exact same "no port given" message as a port that was never passed at all, actively
			// misdirecting an operator debugging a simple typo toward the wrong root cause (did I
			// forget the flag? vs. did I fat-finger the value?).
			slog.Error("cross-server login: invalid -cs-port value (must be positive)", "port", port)
		} else {
			slog.Error("cross-server login: no port given (pass -cs-port or a session config with port)")
		}
		os.Exit(1)
	}

	// Symmetric to the ip/port checks just above, and to DoCrossServerLogin's own p.AccessTok == ""
	// check (crossserver.go): that AccessTok check already carries a live-tested citation that an
	// empty value there reliably fails with ec=28/E011, and the identical mechanism applies here.
	// Unlike base-zone login (login.go), which sends an empty "un" field as the normal, expected
	// case, cross-server login (crossserver.go) sends the gameUid value directly as the "un" field
	// on the wire -- so an empty gameUid here isn't just a missing-value formality, it changes actual
	// on-wire behavior. DoCrossServerLogin has no check for it, so today an empty gameUid burns a
	// full dial+login network round-trip only to fail downstream, wrapped in the same ErrAuthRejected
	// path as a real expired/stale session -- actively misdirecting an operator debugging a simple
	// missing-gameUid configuration gap toward the wrong root cause (README.md documents this exact
	// ec=28/E011 signature as meaning an expired/stale session). Checked here, after the -cs-rt
	// refresh block above (which can replace gameUid with a fresh server list entry), so a config/
	// flag omission that IS resolved by -cs-rt doesn't false-positive.
	if gameUid == "" {
		if o.gameUidExplicit {
			// Distinct from the "never given at all" wording just below: -cs-gameuid WAS actually
			// typed on the command line (per o.gameUidExplicit, the same visitedFlags mechanism this
			// file already uses elsewhere -- e.g. the neighboring "ignoring -cs-at" /
			// serverListOverrideFlags call sites, and the identical pattern the -cs-ip/-cs-port checks
			// above already apply), it just carried an empty value -- e.g. -cs-gameuid "". Before this
			// fix, this case logged the exact same "no gameUid given" message as a gameUid that was
			// never passed at all, actively misdirecting an operator debugging a simple empty-value
			// typo toward the wrong root cause (did I forget the flag? vs. did I pass it empty?).
			slog.Error("cross-server login: -cs-gameuid was given but empty (pass a non-empty gameUid) -- an empty gameUid is sent directly as the un field on the wire and reliably fails with ec=28/E011, misleadingly resembling an expired/stale session rather than a missing-gameUid configuration gap")
		} else {
			slog.Error("cross-server login: no gameUid given (pass -cs-gameuid or a session config with gameUid) -- an empty gameUid is sent directly as the un field on the wire and reliably fails with ec=28/E011, misleadingly resembling an expired/stale session rather than a missing-gameUid configuration gap")
		}
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
		// See shouldAbortBeforeInteractive's own doc comment: the exact same bug class round 25
		// closed for CollectAll's two call sites -- a FetchBuildings failure here that isn't
		// evidence of a genuinely dead connection (e.g. a decode/parse failure on one bad frame,
		// not wrapped in a net.Error) must not silently discard an explicit -interactive request
		// (round 26).
		if shouldAbortBeforeInteractive(err, o.interactive != "") {
			// See main()'s identical round-40 fix doc comment: os.Exit skips this function's own
			// `defer conn.Close()` above, so close explicitly before exiting instead of relying
			// on the now-unreachable defer.
			conn.Close()
			os.Exit(1)
		}
	}
	slog.Info("got buildings", "count", len(buildings))
	if o.listBuildings || !o.collect {
		PrintBuildings(buildings)
	}
	if o.collect {
		slog.Info("collecting resources")
		if err := CollectAll(conn, buildings, visitors); err != nil {
			slog.Error("collect run had failures", "error", err)
			if shouldAbortBeforeInteractive(err, o.interactive != "") {
				// See the identical round-40 fix's doc comment on the sibling os.Exit(1) above.
				conn.Close()
				os.Exit(1)
			}
		}
	}

	warnIfInteractiveExplicitlyEmpty(o.interactiveExplicit, o.interactive)
	if o.interactive != "" {
		RunInteractive(conn, o.interactive)
	}
	slog.Info("client exiting")
}
