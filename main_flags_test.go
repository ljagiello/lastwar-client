package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestDecodeModeIgnoredFlags(t *testing.T) {
	cases := []struct {
		name    string
		visited []string
		want    []string
	}{
		{"nothing visited", nil, nil},
		{"only the three exempt flags", []string{"log-level", "decode-label", "decode-stream"}, nil},
		{"one non-exempt flag alongside decode-stream", []string{"decode-stream", "email"}, []string{"email"}},
		{
			"mixed exempt and non-exempt, order preserved",
			[]string{"log-level", "collect", "decode-label", "cs-ip", "decode-stream", "version"},
			[]string{"collect", "cs-ip", "version"},
		},
		{"no exempt flags visited at all", []string{"handshake", "no-config"}, []string{"handshake", "no-config"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := decodeModeIgnoredFlags(c.visited)
			if !slices.Equal(got, c.want) {
				t.Errorf("decodeModeIgnoredFlags(%v) = %v, want %v", c.visited, got, c.want)
			}
			// decode-stream/decode-label/log-level must never appear in the result, no matter what
			// else was visited -- this is the specific invariant the finding asked to pin down.
			for _, name := range got {
				if name == "decode-stream" || name == "decode-label" || name == "log-level" {
					t.Errorf("decodeModeIgnoredFlags(%v) incorrectly included exempt flag %q", c.visited, name)
				}
			}
		})
	}
}

func TestIgnoredCrossServerFlags(t *testing.T) {
	cases := []struct {
		name    string
		visited []string
		want    []string
	}{
		{"nothing visited", nil, nil},
		{"non-cs flags only", []string{"email", "collect", "version"}, nil},
		{
			"cs-ip and cs-rt excluded -- they gate the cross-server path, not consumed once it's taken",
			[]string{"cs-ip", "cs-rt"},
			nil,
		},
		{"one recognized cs-* flag", []string{"cs-at"}, []string{"cs-at"}},
		{
			"all seven recognized cs-* flags",
			[]string{"cs-at", "cs-port", "cs-zone", "cs-gameuid", "cs-deviceid", "cs-shumei", "cs-ios"},
			[]string{"cs-at", "cs-port", "cs-zone", "cs-gameuid", "cs-deviceid", "cs-shumei", "cs-ios"},
		},
		{
			"mixed gating, recognized, and unrelated flags, order preserved",
			[]string{"cs-ip", "cs-at", "email", "cs-ios", "cs-rt"},
			[]string{"cs-at", "cs-ios"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ignoredCrossServerFlags(c.visited)
			if !slices.Equal(got, c.want) {
				t.Errorf("ignoredCrossServerFlags(%v) = %v, want %v", c.visited, got, c.want)
			}
		})
	}
}

func TestWarnIfDecodeLabelIgnored(t *testing.T) {
	cases := []struct {
		name         string
		decodeStream string
		decodeLabel  string
		wantWarning  bool
	}{
		{"decode-label set without decode-stream", "", "c2s", true},
		{"decode-label set with decode-stream also set", "stream.bin", "c2s", false},
		{"neither set", "", "", false},
		{"decode-stream set without decode-label", "stream.bin", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(orig)

			warnIfDecodeLabelIgnored(c.decodeStream, c.decodeLabel)

			got := strings.Contains(buf.String(), "ignoring -decode-label because -decode-stream is not set")
			if got != c.wantWarning {
				t.Errorf("warnIfDecodeLabelIgnored(decodeStream=%q, decodeLabel=%q): warning present = %v, want %v (log output: %s)",
					c.decodeStream, c.decodeLabel, got, c.wantWarning, buf.String())
			}
		})
	}
}

// TestWarnIfExplicitConfigPathNotFound is the regression test for this round's Fix 2:
// config.go's loadEffectiveConfig returns (nil, "") identically for both an explicit -config path
// and the auto-derived default path when the resolved file genuinely doesn't exist yet (see its own
// doc comment for why that's correct, shared behavior) -- but before this round's fix, main() never
// logged anything to distinguish those two cases, so a typo'd or moved -config path silently
// degraded into an unrelated guest-identity run with zero diagnostic. This pins down that the new
// warning fires only for the explicit-path-genuinely-missing case, and stays silent for every other
// combination: a successfully loaded config, the default path (no -config at all), and -config
// stacked with -no-config (already covered by its own separate, pre-existing warning).
func TestWarnIfExplicitConfigPathNotFound(t *testing.T) {
	cases := []struct {
		name        string
		cfg         *SessionConfig
		configPath  string
		noConfig    bool
		wantWarning bool
	}{
		{"explicit -config path given, not found -> warns", nil, "/no/such/config.json", false, true},
		{"explicit -config path given, loaded successfully -> no warning", &SessionConfig{}, "/some/config.json", false, false},
		{"no -config given (default path), default absent -> stays silent, as today", nil, "", false, false},
		{"no -config given, default loaded successfully -> no warning", &SessionConfig{}, "", false, false},
		{
			"explicit -config path given but -no-config also set -> no warning (already covered by the -config/-no-config warning)",
			nil, "/no/such/config.json", true, false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			defer slog.SetDefault(orig)

			warnIfExplicitConfigPathNotFound(c.cfg, c.configPath, c.noConfig)

			got := strings.Contains(buf.String(), "explicit -config path not found")
			if got != c.wantWarning {
				t.Errorf("warnIfExplicitConfigPathNotFound(cfg=%v, configPath=%q, noConfig=%v): warning present = %v, want %v (log output: %s)",
					c.cfg, c.configPath, c.noConfig, got, c.wantWarning, buf.String())
			}
			if c.wantWarning && !strings.Contains(buf.String(), c.configPath) {
				t.Errorf("warnIfExplicitConfigPathNotFound(cfg=%v, configPath=%q, noConfig=%v): warning doesn't name the path (log output: %s)",
					c.cfg, c.configPath, c.noConfig, buf.String())
			}
		})
	}
}

func TestRefreshHasUsableData(t *testing.T) {
	cases := []struct {
		name       string
		at         *LoginToken
		serverList []LoginServerInfo
		want       bool
	}{
		{"At present, ServerList non-empty", &LoginToken{Token: "tok"}, []LoginServerInfo{{}}, true},
		{"At present, ServerList empty", &LoginToken{Token: "tok"}, nil, true},
		{"At absent, ServerList non-empty", nil, []LoginServerInfo{{}}, true},
		{"At absent, ServerList empty", nil, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			lsr := &LoginServerListRespon{At: c.at, ServerList: c.serverList}
			if got := refreshHasUsableData(lsr); got != c.want {
				t.Errorf("refreshHasUsableData(At=%v, ServerList=%v) = %v, want %v", c.at, c.serverList, got, c.want)
			}
		})
	}
}

// TestServerListOverrideFlags is the fast, deterministic unit test of the pure comparison
// extracted from runCrossServerTest's GSL-refresh server-list-override warning (see
// serverListOverrideFlags' doc comment in main.go, and this round's Fix 2). It directly pins down
// the flag-vs-config distinction: nothing explicit yields nil (the existing plain INFO "server
// selected" log stays as-is), while any explicitly-set flag among cs-ip/cs-port/cs-zone/cs-gameuid
// is named, in declaration order, regardless of which subset was set.
//
// It also covers round 20's Fix 1: serverListOverrideFlags used to check ONLY the four *Explicit
// "flag was typed" bools, never whether the underlying ip/port/zone/gameUid value was actually
// non-empty/non-zero. Go's flag.Visit fires for a flag whenever it appeared on the command line at
// all, even if the value given equals the flag's own zero-value default (e.g. -cs-ip "" from a
// possibly-unset shell variable in a cron wrapper, or -cs-port 0) -- so an operator who explicitly
// passed an EMPTY/zero value used to get a misleading WARN claiming that flag's value was
// "overriding" something, when no meaningful value was ever supplied. The
// "explicitly typed but empty/zero" cases below pin down that this is no longer reported as an
// override, mirroring the neighboring -cs-at check's own `o.at != "" && o.atExplicit` symmetry.
func TestServerListOverrideFlags(t *testing.T) {
	cases := []struct {
		name            string
		ip              string
		ipExplicit      bool
		port            int
		portExplicit    bool
		zone            string
		zoneExplicit    bool
		gameUid         string
		gameUidExplicit bool
		want            []string
	}{
		{"nothing explicit", "", false, 0, false, "", false, "", false, nil},
		{"only cs-ip explicit, with a real value", "1.2.3.4", true, 0, false, "", false, "", false, []string{"cs-ip"}},
		{"only cs-port explicit, with a real value", "", false, 12345, true, "", false, "", false, []string{"cs-port"}},
		{"only cs-zone explicit, with a real value", "", false, 0, false, "APS1234", true, "", false, []string{"cs-zone"}},
		{"only cs-gameuid explicit, with a real value", "", false, 0, false, "", false, "uid1", true, []string{"cs-gameuid"}},
		{
			"all four explicit with real values, declaration order preserved regardless of a different natural check order",
			"1.2.3.4", true, 12345, true, "APS1234", true, "uid1", true,
			[]string{"cs-ip", "cs-port", "cs-zone", "cs-gameuid"},
		},
		{"ip and zone only, with real values", "1.2.3.4", true, 0, false, "APS1234", true, "", false, []string{"cs-ip", "cs-zone"}},
		{
			// Round 20 regression case: -cs-ip was explicitly typed (e.g. a cron wrapper always
			// emitting -cs-ip "$IP" from a possibly-unset shell var), but its value is the empty
			// string -- the flag's own zero-value default. Before this round's fix, this was
			// indistinguishable from a real override and produced a misleading WARN.
			"cs-ip explicitly typed but empty -> not reported as overridden", "", true, 0, false, "", false, "", false, nil,
		},
		{
			// Same class of bug, for -cs-port: explicitly typed as 0 (its zero value), e.g. -cs-port 0.
			"cs-port explicitly typed but zero -> not reported as overridden", "", false, 0, true, "", false, "", false, nil,
		},
		{
			// All four explicitly typed but all left at their zero values -- none should be reported.
			"all four explicit but all empty/zero -> not reported as overridden", "", true, 0, true, "", true, "", true, nil,
		},
		{
			// Mixed: cs-ip explicitly typed with a real value, cs-port explicitly typed but empty --
			// only cs-ip should be reported.
			"cs-ip explicit with a real value, cs-port explicit but zero -> only cs-ip reported",
			"1.2.3.4", true, 0, true, "", false, "", false, []string{"cs-ip"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serverListOverrideFlags(c.ip, c.ipExplicit, c.port, c.portExplicit, c.zone, c.zoneExplicit, c.gameUid, c.gameUidExplicit)
			if !slices.Equal(got, c.want) {
				t.Errorf("serverListOverrideFlags(ip=%q, ipExplicit=%v, port=%v, portExplicit=%v, zone=%q, zoneExplicit=%v, gameUid=%q, gameUidExplicit=%v) = %v, want %v",
					c.ip, c.ipExplicit, c.port, c.portExplicit, c.zone, c.zoneExplicit, c.gameUid, c.gameUidExplicit, got, c.want)
			}
		})
	}
}

// TestCrossServerFlagNamesRecognizesExactlySevenFlags pins down that crossServerFlagNames
// recognizes precisely the 7 -cs-* flags that are consumed once the cross-server path is taken
// (i.e. every -cs-* flag except -cs-ip/-cs-rt, which instead gate whether that path is taken at
// all) -- neither more nor fewer.
func TestCrossServerFlagNamesRecognizesExactlySevenFlags(t *testing.T) {
	want := []string{"cs-at", "cs-port", "cs-zone", "cs-gameuid", "cs-deviceid", "cs-shumei", "cs-ios"}
	if len(crossServerFlagNames) != len(want) {
		t.Fatalf("crossServerFlagNames has %d entries %v, want exactly %d: %v",
			len(crossServerFlagNames), crossServerFlagNames, len(want), want)
	}
	for _, name := range want {
		if !crossServerFlagNames[name] {
			t.Errorf("crossServerFlagNames is missing %q", name)
		}
	}
}

// TestCrossServerFlagNamesMatchesDeclarations is a regression test against the exact failure mode
// this file's own history warns about for this switch/map: a new -cs-* flag added to the FlagSet in
// main() without also adding it to crossServerFlagNames (so it silently falls through the
// -cs-*-ignored warning with no diagnostic), or a stale name left in crossServerFlagNames after a
// flag is renamed or removed from main().
//
// It reads main.go's own source rather than hand-duplicating a second flag-name list here, since a
// second hand-maintained list would reproduce the exact drift risk this test exists to catch --
// instead this scans for every `fs.String/Bool/Int("cs-...", ...)` declaration, which is real
// ground truth for what's actually registered on the FlagSet in main().
func TestCrossServerFlagNamesMatchesDeclarations(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	re := regexp.MustCompile(`fs\.(?:String|Bool|Int)\("(cs-[a-z-]+)"`)
	matches := re.FindAllStringSubmatch(string(src), -1)
	if len(matches) == 0 {
		t.Fatal("found zero -cs-* flag declarations in main.go -- the regexp is likely out of sync with how flags are declared there")
	}
	declared := make(map[string]bool, len(matches))
	for _, m := range matches {
		declared[m[1]] = true
	}

	// -cs-ip and -cs-rt gate whether the cross-server path is taken at all, so they're correctly
	// declared on the FlagSet but correctly absent from crossServerFlagNames -- excluded here too.
	gating := map[string]bool{"cs-ip": true, "cs-rt": true}

	for name := range declared {
		if gating[name] {
			continue
		}
		if !crossServerFlagNames[name] {
			t.Errorf("flag %q is declared on the FlagSet in main() but missing from crossServerFlagNames -- add it there (or to this test's gating set, if it's meant to gate the cross-server path like -cs-ip/-cs-rt)", name)
		}
	}
	for name := range crossServerFlagNames {
		if !declared[name] {
			t.Errorf("crossServerFlagNames recognizes %q but no such flag is declared on the FlagSet in main() -- remove it, or check whether the flag was renamed", name)
		}
	}
}

// TestShouldAbortBeforeInteractive is the fast, deterministic unit test of the decision extracted
// from both -collect call sites (main() and runCrossServerTest) as this round's Fix 1:
// shouldAbortBeforeInteractive(err, interactiveRequested) in main.go. See its doc comment there for
// the full rationale; summarized here as the shape this table pins down:
//
//   - a nil CollectAll error never aborts, regardless of -interactive.
//   - a genuinely fatal error (containsNonTimeoutNetError(err) == true -- a real net.Error with
//     Timeout()==false anywhere in err's tree) always aborts, regardless of -interactive: staying
//     connected to a definitely-dead connection would be useless.
//   - any other non-nil error (an ordinary per-item benign-timeout net.Error, a plain decoded
//     business-logic failure, or a join of only those) aborts ONLY when -interactive was NOT
//     requested -- preserving today's -collect-only behavior unchanged -- and does NOT abort when
//     -interactive WAS requested, which is the actual bug this round fixes: before it, an operator's
//     explicit request to stay connected was silently discarded on exactly this class of error.
//
// Reuses buildings_orchestration_test.go's fakeNetError (same package) for the net.Error cases,
// rather than redefining an equivalent type here, and errors.Join to prove the joined/buried-error
// case is handled the same way containsNonTimeoutNetError itself walks it (a genuine non-timeout
// net.Error anywhere in the tree wins, even alongside otherwise-benign siblings).
func TestShouldAbortBeforeInteractive(t *testing.T) {
	genuineNetErr := fakeNetError{timeout: false}
	benignNetErr := fakeNetError{timeout: true}
	businessErr := errors.New("some decoded errorCode failure")

	cases := []struct {
		name                 string
		err                  error
		interactiveRequested bool
		want                 bool
	}{
		{"nil error, interactive requested", nil, true, false},
		{"nil error, interactive not requested", nil, false, false},
		{"genuine non-timeout net.Error, interactive requested -- still aborts (dead connection)", genuineNetErr, true, true},
		{"genuine non-timeout net.Error, interactive not requested -- aborts", genuineNetErr, false, true},
		{"benign timeout net.Error, interactive requested -- does NOT abort (this round's fix)", benignNetErr, true, false},
		{"benign timeout net.Error, interactive not requested -- still aborts (unchanged -collect-only behavior)", benignNetErr, false, true},
		{"plain business-logic error (not a net.Error at all), interactive requested -- does NOT abort", businessErr, true, false},
		{"plain business-logic error, interactive not requested -- still aborts", businessErr, false, true},
		{
			"joined error: benign timeout + genuine non-timeout net.Error buried in it, interactive requested -- still aborts",
			errors.Join(businessErr, benignNetErr, genuineNetErr),
			true,
			true,
		},
		{
			"joined error of only benign-timeout/business failures, interactive requested -- does NOT abort",
			errors.Join(businessErr, benignNetErr),
			true,
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := shouldAbortBeforeInteractive(c.err, c.interactiveRequested)
			if got != c.want {
				t.Errorf("shouldAbortBeforeInteractive(err=%v, interactiveRequested=%v) = %v, want %v", c.err, c.interactiveRequested, got, c.want)
			}
		})
	}
}
