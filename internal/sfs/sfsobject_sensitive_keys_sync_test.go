package sfs

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// knownNonSensitiveSFSKeys is the reviewed allowlist for every literal Put*("key", ...) call site
// TestSensitiveSFSKeysCoversAllLiteralPutCallSites finds across this repo's own non-test .go
// source that is NOT in SensitiveSFSKeys (sfsobject.go) -- i.e. every field this repo's own
// request-building code puts onto the wire under a literal key name that has been reviewed and
// confirmed to be ordinary gameplay/protocol metadata, not a bearer credential or PII. Every entry
// here was checked against its real call site (identity.go's BuildLoginParams, login.go,
// crossserver.go, conn.go, alliance.go, buildings.go, mail.go, visitors.go) before being added:
//
//   - Protocol/envelope framing, not app data: "c" (command name / controller byte),
//     "a" (action code), "p" (the nested params object itself -- a container, not a value),
//     "r" (extension request id, always -1), "api"/"cl" (SFS2X Handshake protocol
//     version/client name), "clientTime" (ping-pong timestamp), "_id" (a future/message id).
//   - Client/build/app identity, already public by construction (matches this repo's own
//     packageName/appVersion consts, or values baked into the public APK): "packageName",
//     "packageSign" (a SHA1 of the public package name, see sfsobject.go's own comment on why
//     this class of hash was excluded), "platform", "appVersion", "versionCode", "resVersion",
//     "pf"/"pfId" (storefront identifiers), "lang", "KCPMode".
//   - Reproducible, non-secret hashes over public/non-secret inputs -- already called out and
//     excluded by sfsobject.go's own SensitiveSFSKeys doc comment: "psh", "SecurityCode",
//     "OneCode", "CoreV", "dataConfigMd5", "cmdBaseTime" (a plain unix timestamp, one of psh's/
//     SecurityCode's own hash inputs).
//   - Config/session bookkeeping, not identity: "configVersion", "configNumber", "netType",
//     "country", "fromCountry", "suggestCountry", "timeoffset", "lat" (a location-authorized
//     *flag*, not a coordinate -- see identity.go's BuildLoginParams comment on this field),
//     "referrer", "deeplinkParams", "google_available", "gmLogin",
//     "delete_account_status", "forbidden_froce_merge", "isUseLz4", "isAll", "count", "time",
//     "clientseq", "firstCmd", "serverId".
//   - Entity/gameplay identifiers that identify something but aren't bearer credentials --
//     the same class sfsobject.go's own top comment already carves "gameUid" out for
//     ("identifies an account but isn't a bearer credential by itself"): "gameUid" itself,
//     "zn" (zone name), "uuid" (a building instance id, buildings.go), "uid"
//     (a visitor/entity id, visitors.go), "uids" (a comma-joined list of mail message ids,
//     mail.go), "type"/"action"/"option"/"operate"/"scienceId" (numeric gameplay
//     action/type discriminators, alliance.go/buildings.go/mail.go/visitors.go).
//
// NOTE: "un" (the classic SFS2X username field) and "googlePlay" (part of the Google-identity
// cluster) used to be listed here as reviewed-safe, but round 28's audit reclassified both as
// PII/identity fields that belong in SensitiveSFSKeys instead (sfsobject.go) -- "un" is the
// server's real returned account username, a live cleartext-username leak login.go used to log
// unconditionally at Info level; "googlePlay" sits in the same reviewed-sensitive cluster as its
// neighbors googleName/androidDid. See sfsobject.go's SensitiveSFSKeys doc comments on those two
// entries for the full reasoning. Left out of this map now (rather than kept here AND added to
// SensitiveSFSKeys) specifically so TestKnownNonSensitiveSFSKeysDoesNotOverlapSensitiveSFSKeys
// below keeps catching this exact kind of drift.
//
// See TestSensitiveSFSKeysCoversAllLiteralPutCallSites's doc comment for what happens when a
// future key lands in neither this map nor SensitiveSFSKeys.
var knownNonSensitiveSFSKeys = map[string]bool{
	"_id":                   true,
	"a":                     true,
	"action":                true,
	"api":                   true,
	"appVersion":            true,
	"c":                     true,
	"cl":                    true,
	"clientTime":            true,
	"clientseq":             true,
	"cmdBaseTime":           true,
	"configNumber":          true,
	"configVersion":         true,
	"CoreV":                 true,
	"count":                 true,
	"country":               true,
	"dataConfigMd5":         true,
	"deeplinkParams":        true,
	"delete_account_status": true,
	"firstCmd":              true,
	"forbidden_froce_merge": true,
	"fromCountry":           true,
	"gameUid":               true,
	"gmLogin":               true,
	"google_available":      true,
	"isAll":                 true,
	"isUseLz4":              true,
	"KCPMode":               true,
	"lang":                  true,
	"lat":                   true,
	"netType":               true,
	"OneCode":               true,
	"operate":               true,
	"option":                true,
	"p":                     true,
	"packageName":           true,
	"packageSign":           true,
	"pf":                    true,
	"pfId":                  true,
	"platform":              true,
	"psh":                   true,
	"r":                     true,
	"referrer":              true,
	"resVersion":            true,
	"scienceId":             true,
	"SecurityCode":          true,
	"serverId":              true,
	"suggestCountry":        true,
	"time":                  true,
	"timeoffset":            true,
	"type":                  true,
	"uid":                   true,
	"uids":                  true,
	"uuid":                  true,
	"versionCode":           true,
	"zn":                    true,
}

// putKeyCallRe matches a literal Put*("key", ...) call for every SFSObject Put* helper this repo
// defines (sfsobject.go: PutUtfString/PutInt/PutLong/PutBool/PutDouble/PutByte/PutShort/
// PutSFSObject/PutSFSArray). Deliberately case-sensitive/exact on the "Put" prefix so it doesn't
// match the unexported `put`/`add` methods, which take an already-tagged SFSValue built elsewhere
// (or are exercised directly only by same-package tests constructing edge-case SFSValue shapes,
// e.g. sfsobject_redact_test.go) -- neither represents a new literal field name this repo's own
// request-building code introduces.
var putKeyCallRe = regexp.MustCompile(`\bPut(?:UtfString|Int|Long|Bool|Double|Byte|Short|SFSObject|SFSArray)\("([a-zA-Z0-9_]+)"`)

// stripLineComment returns line with any trailing "//..." comment removed, tracking whether the
// scan is inside a double-quoted string literal so a "//" that's part of a quoted value (e.g. a
// URL) isn't mistaken for the start of a comment. This repo has no "/* */" block comments
// (confirmed via grep across every non-test .go file), so a per-line "//" scan is sufficient --
// full-line comments (the common case; see e.g. sfsobject.go's own SensitiveSFSKeys doc comment,
// which mentions several Put*("key", ...) call sites in prose) are handled by the caller skipping
// lines whose trimmed text starts with "//" before this is even called.
func stripLineComment(line string) string {
	inString := false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if inString {
				i++ // skip the escaped character, whatever it is
			}
		case '"':
			inString = !inString
		case '/':
			if !inString && i+1 < len(line) && line[i+1] == '/' {
				return line[:i]
			}
		}
	}
	return line
}

// scanPutKeysInRepo walks every non-test .go file in dir (a flat, single-package directory -- see
// gsl_form_sync_test.go's TestEncodeFormSortedOrderMatchesGetServerListFields, which this test
// mirrors the source-scanning technique of) and extracts every literal key name passed to a
// Put*("key", ...) call, keyed by the file:line location(s) it was found at (for readable failure
// output when a key isn't classified in either list).
// repoRoot walks up from the test's working directory (the package dir) until it finds go.mod,
// returning the module root. Needed since the codebase was split into internal/<pkg> packages: the
// Put*("key", ...) call sites this test scans for now live across the whole module tree (game/gsl
// prod files), not in this package's own directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not find go.mod walking up from the test directory")
		}
		dir = parent
	}
}

func scanPutKeysInRepo(t *testing.T, root string) map[string][]string {
	t.Helper()
	found := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "docs" || name == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			code := stripLineComment(line)
			for _, m := range putKeyCallRe.FindAllStringSubmatch(code, -1) {
				key := m[1]
				found[key] = append(found[key], fmt.Sprintf("%s:%d", rel, i+1))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return found
}

// TestSensitiveSFSKeysCoversAllLiteralPutCallSites is the round-17 self-enforcing completeness
// check for SensitiveSFSKeys (sfsobject.go): five straight audit rounds (12, 14, 15, 16, and the
// original round-11 sweep) each found SensitiveSFSKeys missing real fields this repo's own
// request-building code puts onto the wire under a literal key name, purely by manual re-audit.
// This test turns that recurring manual question -- "does SensitiveSFSKeys cover every field this
// repo actually sends?" -- into a CI check, mirroring the exact source-scanning technique
// gsl_form_sync_test.go's TestEncodeFormSortedOrderMatchesGetServerListFields already uses to keep
// encodeFormSorted's `order` whitelist in sync with GetServerList's own form.Set(...) calls.
//
// It statically extracts every literal Put*("key", ...) call site across the repo's non-test .go
// source files (scanPutKeysInRepo) and asserts each extracted key is present in EITHER
// SensitiveSFSKeys (sfsobject.go) OR knownNonSensitiveSFSKeys (this file's reviewed allowlist,
// declared above). A key found by the scan that lands in neither list fails this test loudly,
// naming the key and every call site it was found at -- rather than silently doing nothing, which
// is exactly how this gap survived five audit rounds before.
//
// This is deliberately one-directional (scanned keys must be a subset of the union of the two
// lists), unlike TestEncodeFormSortedOrderMatchesGetServerListFields's exact bidirectional match:
// SensitiveSFSKeys legitimately contains several keys (e.g. "loginKey", "accessToken", "rt", "tk",
// "chatToken") that only ever arrive in a *decoded* server response, never built via a literal
// Put*(...) call in this client's own outgoing request code -- so requiring every SensitiveSFSKeys
// entry to also appear in the scan would be a false requirement, not a real drift signal.
func TestSensitiveSFSKeysCoversAllLiteralPutCallSites(t *testing.T) {
	found := scanPutKeysInRepo(t, repoRoot(t))
	if len(found) == 0 {
		t.Fatal("scanned zero Put*(\"key\", ...) call sites across the repo's non-test .go files -- " +
			"putKeyCallRe/scanPutKeysInRepo is likely out of sync with how this repo builds SFSObjects")
	}

	var uncovered []string
	for key, sites := range found {
		if SensitiveSFSKeys[key] || knownNonSensitiveSFSKeys[key] {
			continue
		}
		uncovered = append(uncovered, fmt.Sprintf("%q (used at %s)", key, strings.Join(sites, ", ")))
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("found %d literal Put*(...) key(s) that are in neither SensitiveSFSKeys (sfsobject.go) "+
			"nor knownNonSensitiveSFSKeys (sfsobject_sensitive_keys_sync_test.go) -- every key this repo "+
			"puts onto the wire must be explicitly classified: add it to SensitiveSFSKeys if it can carry "+
			"a live credential/PII, or review it and add it to knownNonSensitiveSFSKeys with a comment "+
			"explaining why it's safe. Do not leave a key unclassified:\n%s",
			len(uncovered), strings.Join(uncovered, "\n"))
	}
}

// TestKnownNonSensitiveSFSKeysDoesNotOverlapSensitiveSFSKeys guards knownNonSensitiveSFSKeys
// itself against drift in the other direction: if a key already in knownNonSensitiveSFSKeys is
// later added to SensitiveSFSKeys (e.g. a future audit round decides a previously-"safe" field
// actually needs redaction), leaving it in both maps would be silently harmless for
// TestSensitiveSFSKeysCoversAllLiteralPutCallSites (either map alone satisfies that check), but is
// a stale/misleading entry that should be removed from the allowlist rather than left to rot.
func TestKnownNonSensitiveSFSKeysDoesNotOverlapSensitiveSFSKeys(t *testing.T) {
	for key := range knownNonSensitiveSFSKeys {
		if SensitiveSFSKeys[key] {
			t.Errorf("knownNonSensitiveSFSKeys still lists %q, but it has since been added to "+
				"SensitiveSFSKeys (sfsobject.go) -- remove it from the non-sensitive allowlist", key)
		}
	}
}
