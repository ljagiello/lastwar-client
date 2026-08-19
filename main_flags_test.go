package main

import (
	"bytes"
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
func TestServerListOverrideFlags(t *testing.T) {
	cases := []struct {
		name                                                    string
		ipExplicit, portExplicit, zoneExplicit, gameUidExplicit bool
		want                                                    []string
	}{
		{"nothing explicit", false, false, false, false, nil},
		{"only cs-ip explicit", true, false, false, false, []string{"cs-ip"}},
		{"only cs-port explicit", false, true, false, false, []string{"cs-port"}},
		{"only cs-zone explicit", false, false, true, false, []string{"cs-zone"}},
		{"only cs-gameuid explicit", false, false, false, true, []string{"cs-gameuid"}},
		{
			"all four explicit, declaration order preserved regardless of a different natural check order",
			true, true, true, true,
			[]string{"cs-ip", "cs-port", "cs-zone", "cs-gameuid"},
		},
		{"ip and zone only", true, false, true, false, []string{"cs-ip", "cs-zone"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := serverListOverrideFlags(c.ipExplicit, c.portExplicit, c.zoneExplicit, c.gameUidExplicit)
			if !slices.Equal(got, c.want) {
				t.Errorf("serverListOverrideFlags(ip=%v, port=%v, zone=%v, gameUid=%v) = %v, want %v",
					c.ipExplicit, c.portExplicit, c.zoneExplicit, c.gameUidExplicit, got, c.want)
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
