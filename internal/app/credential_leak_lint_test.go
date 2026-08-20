package app

import (
	"lastwar-client/internal/sfs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// sinkStartRe matches the opening of any slog.*/fmt.*/log.* call this guard treats as a "sink" --
// somewhere a value can flow into a log line, wrapped error, or printed output. Hoisted to package
// level (rather than a local inside TestNoRawSFSObjectDumpInLogsOrErrors) so
// TestSinkStartReMatchesAllSinkForms can exercise it directly.
//
// Round-14 broadening: the original set only covered slog's Info/Warn/Error/Debug, fmt's
// Errorf/Printf/Fprintf/Sprintf/Println/Print, and log's Printf/Print/Println -- missing fmt's
// remaining Sprint/Sprintln/Fprint/Fprintln forms, the log.Fatal/Fatalf/Fatalln family (which also
// print their arguments before exiting), and slog.Log (the level-parameterized form). Any of these
// could carry a raw .String()/.unsafeRawString() dump right past the old pattern undetected -- no
// live instance of any of these forms exists in this repo today (confirmed by grep), but the gap
// was real.
var sinkStartRe = regexp.MustCompile(`\b(slog\.(Info|Warn|Error|Debug|Log)|fmt\.(Errorf|Printf|Fprintf|Sprintf|Println|Print|Sprint|Sprintln|Fprint|Fprintln)|log\.(Printf|Print|Println|Fatal|Fatalf|Fatalln))\(`)

// stringCallRe matches a raw-dump call this guard treats as dangerous when it appears inside a
// sink call. Hoisted to package level for the same reason as sinkStartRe above.
//
// Round-14 addition: since sfs.SFSObject.String() itself is now safe by default (it delegates to
// StringRedacted(), see sfsobject.go), the raw, unredacted dump that used to live under the name
// String() was renamed to the unexported unsafeRawString(). Round 15 then deleted
// unsafeRawString() (and its formatSFSValue() recursion helper) entirely as dead code, once a
// `deadcode` audit confirmed nothing called them anymore -- no such method exists anywhere in this
// codebase today. This pattern still flags both names: .unsafeRawString() is kept purely as
// defense-in-depth against a hypothetical future reintroduction of that escape hatch, and plain
// .String() for the same historical reasons (see TestNoRawSFSObjectDumpInLogsOrErrors's doc
// comment for why that's kept even though it's no longer unsafe for sfs.SFSObject specifically).
// Neither alternative matches anything dangerous in this codebase as it stands today.
var stringCallRe = regexp.MustCompile(`\.(String|unsafeRawString)\(\)`)

// TestNoRawSFSObjectDumpInLogsOrErrors is the round-11 process fix (testing-rigor's "minor"
// finding), broadened in round 12 after the audit found the original guard's blind spots let a
// real instance (decode.go's -decode-stream tool, buildings.go's -list-buildings dump) slip past
// undetected: the credential-leak bug class this audit series has hunted -- a decoded/outgoing
// sfs.SFSObject's raw .String() dump flowing into a log line, wrapped error, or printed output, where
// it might carry a live loginKey/accessToken/airKey/shumeiBoxId/verifyCode/deviceId/chatToken/tk
// -- has recurred across many call sites despite each individual instance being fixed as found.
// This test is the catch: it scans every non-test .go source file for a slog.*/fmt.*/log.Print*
// call that embeds a `.String()` call (joining multi-line calls first, so a call whose `.String()`
// argument sits on a different physical line than the sink's opening paren is still caught), and
// fails on anything not in the explicit allowlist below -- so a future PR that reintroduces this
// pattern fails the build instead of waiting for the next audit round to notice.
//
// Known, accepted limitation: this cannot catch a `.String()` result stashed in a variable and
// logged several statements later (see conn.go's DoHandshake "skipped envelope" fallback, which
// used to do exactly that -- now fixed to StringRedacted() directly, but a *future* instance of
// this specific indirection pattern would still evade the scan). A full go/ast-based data-flow
// check would close this; not implemented here since no current instance of the gap exists to
// justify the added complexity.
//
// Round-13 fix: the multi-line join logic used to tally parens with a raw
// strings.Count(lines[i], "(") - strings.Count(lines[i], ")") over each physical line's full text,
// including characters that just happen to sit inside a Go string/rune literal or after a "//"
// comment marker. A sink call whose first physical line contained an unmatched ')' inside a string
// literal (e.g. a log message like "...unexpected value)") -- or a trailing comment with a stray
// ')' -- could make the running depth hit zero (or below) on that very first line, stopping the
// join loop before it ever reached a later physical line holding the actual raw .String() dump:
// a silent bypass of this whole guard. Fixed by stripping string/rune-literal contents and "//"
// comments (via stripStringsAndComments) before counting parens or matching .String(), so only
// characters that are actually part of Go syntax participate in the balance. See
// TestStripStringsAndComments and TestJoinSinkCallClosesParenCountBypass for the regression
// coverage that proves this.
//
// Round-14 change of purpose: sfs.SFSObject.String() itself is now safe by default -- it delegates to
// StringRedacted() (see sfsobject.go), so a bare .String() call on an sfs.SFSObject can no longer leak
// a credential no matter what sink it flows into, and neither can an implicit fmt.Stringer
// auto-invocation via %v/Println/slog's Any-kind formatting (a path this text-scanning guard could
// never see anyway, since no literal ".String()" appears in source for that -- closing that gap
// required the structural fix in sfsobject.go, not a broader regex here). The raw, unredacted dump
// that used to live under the name String() was renamed to the unexported unsafeRawString(), which
// did NOT satisfy fmt.Stringer and was only ever meant to be called from within sfsobject.go itself
// (by formatSFSValue's recursive raw-dump path).
//
// Round-15 update: unsafeRawString() and formatSFSValue() were deleted entirely as dead code
// (confirmed via `go run golang.org/x/tools/cmd/deadcode@latest .` -- nothing called either one
// anymore, since String() had delegated straight to StringRedacted() since round 14). Neither
// function exists anywhere in this codebase today. So this guard's purpose going forward is pure
// defense-in-depth, not catching an active, currently-reachable escape hatch: stringCallRe still
// flags ".unsafeRawString()" in case that name is ever reintroduced by a future contributor
// reaching for the same "I need the raw dump" pattern, and flags plain ".String()" for the same
// historical reasons (four rounds of this bug class living under that exact name is reason enough
// to keep watching it, even though it's no longer unsafe for sfs.SFSObject specifically) -- see the
// allowlist entries below for the sfs.SFSObject.String() call sites, which are now unconditionally
// safe rather than "safe because we checked the data."
func TestNoRawSFSObjectDumpInLogsOrErrors(t *testing.T) {
	// allowlist maps "file.go:<trimmed text of the line where the sink call starts>" to why that
	// call is safe. Every entry here was individually confirmed safe by this repo's own audit
	// rounds -- add a new entry only with the same level of scrutiny: confirm the specific
	// message/data this line logs can never carry a credential field (see sfs.SensitiveSFSKeys in
	// sfsobject.go for the current known list), not just that it seems unlikely. Round 12's audit
	// specifically re-verified every entry below against this repo's own docs/*.mdx and found two
	// (the Handshake-response entries) were WRONG -- the response does carry a session token
	// (`tk`) -- so both call sites were switched to StringRedacted() instead of being re-allowlisted.
	allowlist := map[string]string{
		`buildings.go:slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())`:                  "o.String() is now unconditionally safe (round 14: String() delegates to StringRedacted(), see sfsobject.go) regardless of what o contains -- kept in this allowlist rather than removed only because this guard still flags plain .String() generically for defense-in-depth (see the doc comment above), not because this specific line's data was re-verified safe",
		`conn.go:slog.Info(label, "cmd", msg.Cmd, "response", msg.Params.String())`:                                        "msg.Params.String() is now unconditionally safe (round 14: String() delegates to StringRedacted()) -- same reasoning as the buildings.go entry above",
		`conn.go:slog.Warn(label+" no-op (expected)", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.String())`: "same reasoning as the conn.go entry above",
		`conn.go:slog.Warn(label+" no-op (status=0, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.String())`:      "same reasoning as the conn.go entry above",
		`interactive.go:slog.Info("shutting down", "signal", sig.String())`:                                                "sig is an os.Signal, not an sfs.SFSObject -- String() here is the standard library's, unrelated to this bug class",
		`interactive.go:slog.Error("no matching response within "+defaultCmdTimeout.String(), "error", err)`:               "defaultCmdTimeout is a time.Duration (const defaultCmdTimeout = 8 * time.Second, conn.go) -- String() here is the standard library's Duration.String(), unrelated to this bug class, same as the sig/val entries immediately above",
	}

	seen := map[string]bool{}

	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "docs" || n == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		relName := name // basename key, matching the allowlist entries below (unique across the module)

		for i := 0; i < len(lines); i++ {
			if !sinkStartRe.MatchString(lines[i]) {
				continue
			}
			// Join lines forward from the sink call's start until parens balance back to (or
			// below) zero, so a .String() argument on a later physical line is still caught.
			startIdx := i
			joined, endIdx := joinSinkCall(lines, i)
			i = endIdx
			if !stringCallRe.MatchString(joined) {
				continue
			}
			trimmedStart := strings.TrimSpace(lines[startIdx])
			key := relName + ":" + trimmedStart
			if _, ok := allowlist[key]; ok {
				seen[key] = true
				continue
			}
			t.Errorf("%s:%d: a slog.*/fmt.*/log.Print* call embeds a raw .String() call -- this is exactly the "+
				"credential-leak pattern this repo has hit repeatedly (loginKey/accessToken/airKey/shumeiBoxId/"+
				"verifyCode/deviceId/chatToken/tk can ride along in a decoded sfs.SFSObject with no field-level "+
				"redaction). Use sfs.SFSObject.StringRedacted() instead, or if this specific line is genuinely safe "+
				"(confirmed no credential field can ever appear in this data), add it to the allowlist in this "+
				"test with a one-line justification.\n\tline: %s", relName, startIdx+1, trimmedStart)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	for key := range allowlist {
		if !seen[key] {
			t.Errorf("allowlist entry no longer matches any source line (line was fixed/removed/reworded) -- remove this stale entry: %s", key)
		}
	}
}

// getStringSensitiveKeyRe matches a `.GetString("key")` call, capturing the literal key name --
// used by TestNoSensitiveGetStringLoggedRaw (below) to find call sites that extract a field's raw
// Go string value by key. Unlike a `.String()`/`.StringRedacted()` dump of a whole object,
// GetString(key) returns a bare string with no field-name context at all once it's in the caller's
// hands, so it bypasses StringRedacted()'s key-by-key sfs.SensitiveSFSKeys check entirely -- there is
// nothing left downstream that could sfs.Redact it.
var getStringSensitiveKeyRe = regexp.MustCompile(`\.GetString\("(\w+)"\)`)

// redactWrappedGetStringRe matches the one form TestNoSensitiveGetStringLoggedRaw treats as SAFE:
// the extracted value immediately wrapped in sfs.Redact() (login.go's own first4...last4 shortening
// helper), e.g. `sfs.Redact(msg2.Params.GetString("loginKey"))` -- the pattern every current call site
// that legitimately logs a sensitive field's actual *value* (as opposed to just its length) already
// uses.
var redactWrappedGetStringRe = regexp.MustCompile(`sfs.Redact\([\w.]*GetString\("(\w+)"\)\)`)

// sensitiveGetStringOccurrence describes one `.GetString("key")` occurrence found within a single
// sink call's joined source text (see findUnsafeSensitiveGetStringCalls) -- key is the sensitive key
// name involved, and offset is this occurrence's own byte position within the scanned text, used to
// compute which physical line it actually sits on (which may differ from the line the sink call
// itself starts on, for a multi-line call).
type sensitiveGetStringOccurrence struct {
	key    string
	offset int
}

// findUnsafeSensitiveGetStringCalls scans rawJoined (the raw, unstripped joined text of one
// slog.*/fmt.*/log.Print* sink call -- see joinSinkCall) for every `.GetString("key")` occurrence
// whose key is registered in sfs.SensitiveSFSKeys and that is NOT wrapped in sfs.Redact(...) AT THAT SAME
// OCCURRENCE.
//
// Round-29 fix: this used to be computed differently, via a `safeKeys map[string]bool` populated
// per matched KEY NAME from every redactWrappedGetStringRe match anywhere in the block, then ANY
// getStringSensitiveKeyRe match for that key name was skipped if safeKeys[key] was true. That is a
// real false-negative gap: a hypothetical line like
// `slog.Info("x", "masked", sfs.Redact(o.GetString("loginKey")), "raw", o.GetString("loginKey"))` would
// set safeKeys["loginKey"]=true from the wrapped occurrence, and the SECOND, genuinely-unsafe raw
// occurrence of GetString("loginKey") in the SAME sink call would then also be incorrectly skipped,
// purely because it shares a key name with the safe one elsewhere in the block.
//
// Fixed by checking safety POSITIONALLY instead of by key name: a raw occurrence is safe only if its
// own exact source span falls inside one of redactWrappedGetStringRe's own match spans -- which
// necessarily contains that exact occurrence's `.GetString("key")` text, since that's the literal
// substring the wrapping pattern matches around. A genuinely-unsafe raw occurrence elsewhere in the
// same block, even one sharing a key name with a safe wrapped occurrence, therefore no longer
// escapes detection. See TestFindUnsafeSensitiveGetStringCallsCatchesSameKeyPositionalFalseNegative
// for the regression coverage proving this.
func findUnsafeSensitiveGetStringCalls(rawJoined string) []sensitiveGetStringOccurrence {
	safeSpans := redactWrappedGetStringRe.FindAllStringIndex(rawJoined, -1)

	var out []sensitiveGetStringOccurrence
	for _, m := range getStringSensitiveKeyRe.FindAllStringSubmatchIndex(rawJoined, -1) {
		start, end := m[0], m[1]
		key := rawJoined[m[2]:m[3]]
		if !sfs.SensitiveSFSKeys[key] {
			continue
		}
		safe := false
		for _, span := range safeSpans {
			if start >= span[0] && end <= span[1] {
				safe = true
				break
			}
		}
		if safe {
			continue
		}
		out = append(out, sensitiveGetStringOccurrence{key: key, offset: start})
	}
	return out
}

// TestNoSensitiveGetStringLoggedRaw is the round-28 sibling of TestNoRawSFSObjectDumpInLogsOrErrors
// above, closing a related but distinct gap in the same credential-leak bug class. That test scans
// for a raw .String()/.StringRedacted()-style dump of a *whole* sfs.SFSObject reaching a log/error sink;
// this one scans for a *single field's* raw value reaching one via .GetString(key) -- a call that
// returns a bare Go string with no way for anything downstream to know which key it came from, so
// it can never be redacted after the fact the way a full StringRedacted() dump can.
//
// This is exactly the shape of the round-28 finding: login.go used to call
// `slog.Info("login OK", "un", env.Content.GetString("un"))`, logging the server's real returned
// account username in cleartext at Info level on every successful login -- even after "un" was
// registered in sfs.SensitiveSFSKeys (sfsobject.go), since StringRedacted()'s redaction never runs on
// this call path at all.
//
// Scans every non-test .go source file for a slog.*/fmt.*/log.Print* sink call, using
// sinkStartRe/joinSinkCall from the sibling test above only to find where a (possibly multi-line)
// sink call ends -- unlike that test, this one then re-joins the ORIGINAL, unstripped lines in that
// range rather than joinSinkCall's paren-balance-stripped text: stripStringsAndComments deliberately
// erases string-literal CONTENTS (so a stray paren inside one can't confuse the paren tally), which
// would erase the very key name -- `"un"` inside `GetString("un")` -- this scan needs to see. It
// then delegates to findUnsafeSensitiveGetStringCalls (above) to flag a `.GetString("key")` call for
// a key registered in sfs.SensitiveSFSKeys, unless that same OCCURRENCE (not just the same key name
// somewhere in the block -- see that function's own doc comment for the round-29 fix this
// implements) is wrapped in sfs.Redact(...) (the one safe pattern this repo uses for logging a
// sensitive field's value, e.g. login.go's `sfs.Redact(msg2.Params.GetString("loginKey"))`).
//
// Known, accepted limitation, mirroring the sibling test's own documented gap: this cannot catch a
// GetString(...) result stashed in a local variable and logged several statements later -- only the
// DIRECT, same-sink-call embedding is caught (a full go/ast-based data-flow check would close this
// too; not implemented here for the same "no current instance of the gap exists to justify the
// added complexity" reason the sibling test's doc comment gives).
func TestNoSensitiveGetStringLoggedRaw(t *testing.T) {
	// allowlist mirrors TestNoRawSFSObjectDumpInLogsOrErrors' own allowlist shape: "file.go:<trimmed
	// sink-call start line>" -> justification. Empty today -- every current GetString(sensitiveKey)
	// call embedded directly in a sink is either wrapped in sfs.Redact() (safe, and never matched by
	// this scan to begin with) or was fixed by the round-28 change this test guards.
	allowlist := map[string]string{}

	seen := map[string]bool{}

	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == ".git" || n == "docs" || n == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		lines := strings.Split(string(data), "\n")
		relName := name // basename key, matching the allowlist entries below (unique across the module)

		for i := 0; i < len(lines); i++ {
			if !sinkStartRe.MatchString(lines[i]) {
				continue
			}
			startIdx := i
			_, endIdx := joinSinkCall(lines, i)
			i = endIdx
			// Deliberately the RAW lines, not joinSinkCall's paren-stripped text -- see this test's
			// doc comment above for why the stripped version would hide the key name entirely.
			rawJoined := strings.Join(lines[startIdx:endIdx+1], "\n")

			for _, occ := range findUnsafeSensitiveGetStringCalls(rawJoined) {
				// The occurrence's own physical line, computed from its byte offset within
				// rawJoined -- may differ from startIdx+1 (the sink call's OPENING line) for a
				// multi-line sink call, so this points at the actual offending line rather than
				// always the call's first line.
				lineNum := startIdx + strings.Count(rawJoined[:occ.offset], "\n") + 1
				trimmedStart := strings.TrimSpace(lines[startIdx])
				allowKey := relName + ":" + trimmedStart
				if _, ok := allowlist[allowKey]; ok {
					seen[allowKey] = true
					continue
				}
				t.Errorf("%s:%d: a slog.*/fmt.*/log.Print* call embeds .GetString(%q) directly -- %q is a "+
					"registered sensitive key (sfs.SensitiveSFSKeys, sfsobject.go), so this bypasses "+
					"StringRedacted()'s field-by-field redaction entirely and logs the real value in "+
					"cleartext. Log its length instead (mirroring the emailLen/usernameLen pattern), or "+
					"wrap it in sfs.Redact(...) if the value itself is genuinely useful to log, or add this "+
					"line to the allowlist in this test with a one-line justification if it's genuinely "+
					"safe.\n\tline: %s", relName, lineNum, occ.key, occ.key, trimmedStart)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir: %v", err)
	}

	for key := range allowlist {
		if !seen[key] {
			t.Errorf("allowlist entry no longer matches any source line (line was fixed/removed/reworded) -- remove this stale entry: %s", key)
		}
	}
}

// stripStringsAndComments removes the contents of Go double-quoted strings, single-quoted rune
// literals, backtick-delimited raw strings, and any trailing "//" line comment from a single
// physical line, leaving only characters that are actually part of Go syntax (call parens,
// identifiers, operators, etc.). This exists so TestNoRawSFSObjectDumpInLogsOrErrors's paren-depth
// tally and its .String()/.unsafeRawString() substring match can't be fooled by a stray ')' or a
// look-alike substring that only appears inside a string/rune literal or a comment.
//
// openRaw reports whether the scan enters this line already inside a raw string left open by a
// previous physical line (a backtick with no matching close yet); the returned stillOpenRaw
// reports the same thing for the *end* of this line, so a caller scanning multiple physical lines
// in sequence can thread the state from one call to the next instead of resetting to "not in any
// string" at the start of every line.
//
// Round-14 fix: this used to always start each line at state "none", with no way for a caller to
// say "we're still inside a raw string from before" -- so a backtick raw string spanning multiple
// physical lines was invisible past its first line. On a later line, the still-open raw string's
// leftover content (up to its real closing backtick) got scanned as if it were live Go code, and
// the backtick that actually closes it got misread as *opening* a brand new raw string instead --
// silently swallowing everything after it on that line, including a real .String()/
// .unsafeRawString() call, as if it too were raw-string content. See
// TestJoinSinkCallCarriesRawStringStateAcrossLines for the regression coverage that proves the fix
// (threading this state via joinSinkCall) closes that bypass.
//
// Double-quoted strings and rune literals are not given the same cross-line treatment: unescaped
// newlines inside either are a Go syntax error, so neither can legitimately span physical lines in
// valid source, unlike backtick raw strings.
//
// This remains deliberately a lightweight line-scanner, not a full tokenizer, matching this file's
// existing design philosophy (see the doc comment on TestNoRawSFSObjectDumpInLogsOrErrors).
func stripStringsAndComments(line string, openRaw bool) (out string, stillOpenRaw bool) {
	var b strings.Builder
	const (
		none = iota
		inDoubleQuote
		inRune
		inRawString
	)
	state := none
	if openRaw {
		state = inRawString
	}
	escaped := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch state {
		case inDoubleQuote, inRune:
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' {
				escaped = true
				continue
			}
			if (state == inDoubleQuote && c == '"') || (state == inRune && c == '\'') {
				state = none
			}
			continue
		case inRawString:
			if c == '`' {
				state = none
			}
			continue
		default: // none
			switch c {
			case '"':
				state = inDoubleQuote
			case '\'':
				state = inRune
			case '`':
				state = inRawString
			case '/':
				if i+1 < len(line) && line[i+1] == '/' {
					// Rest of the physical line is a "//" comment -- stop here. state is "none" in
					// this branch (the switch on c is only reached when state == none), so the
					// still-open-raw-string result is unaffected.
					return b.String(), false
				}
				b.WriteByte(c)
			default:
				b.WriteByte(c)
			}
		}
	}
	return b.String(), state == inRawString
}

// joinSinkCall joins lines[start:] forward, using stripStringsAndComments on each line before
// tallying its paren balance, until the running depth balances back to (or below) zero -- mirroring
// how a multi-line slog.*/fmt.*/log.Print* call closes. It returns the joined text (built from the
// stripped lines, so string/rune-literal contents and "//" comments never leak into a later
// .String()/.unsafeRawString() substring match) and the index of the last line consumed, so the
// caller can resume scanning after it.
//
// Round-14 fix: carries stripStringsAndComments' raw-string-open state from one line's call to the
// next (openRaw), instead of always passing false -- so a raw string left open at the end of one
// physical line is correctly still open at the start of the next, closing the multi-line
// raw-string bypass described on stripStringsAndComments' doc comment.
func joinSinkCall(lines []string, start int) (joined string, endIdx int) {
	var b strings.Builder
	depth := 0
	openRaw := false
	i := start
	for i < len(lines) {
		var stripped string
		stripped, openRaw = stripStringsAndComments(lines[i], openRaw)
		depth += strings.Count(stripped, "(") - strings.Count(stripped, ")")
		b.WriteString(stripped)
		b.WriteByte('\n')
		if depth <= 0 {
			break
		}
		i++
	}
	return b.String(), i
}

// TestStripStringsAndComments is a permanent regression test for stripStringsAndComments,
// covering the specific cases that motivated the round-13 fix: parens hiding inside string/rune
// literals and trailing comments must not survive stripping, while ordinary code (including a
// real .String() call) must pass through untouched.
func TestStripStringsAndComments(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		want        string
		openRaw     bool // input: line starts already inside a raw string left open by a previous line
		wantOpenRaw bool // output: line ends still inside an open raw string
	}{
		{
			name: "plain code with no literals or comments is unchanged",
			in:   `slog.Info("x", msg.String())`,
			want: `slog.Info(, msg.String())`,
		},
		{
			name: "stray close-paren inside a double-quoted string is stripped",
			in:   `slog.Info("something with a stray paren )",`,
			want: `slog.Info(,`,
		},
		{
			name: "escaped quote inside a string does not end the string early",
			in:   `slog.Info("she said \"hi)\" today", x)`,
			want: `slog.Info(, x)`,
		},
		{
			name: "stray close-paren inside a rune literal is stripped",
			in:   `if c == ')' { foo() }`,
			want: `if c ==  { foo() }`,
		},
		{
			name: "stray close-paren inside a backtick raw string is stripped",
			in:   "re := regexp.MustCompile(`weird)pattern`)",
			want: `re := regexp.MustCompile()`,
		},
		{
			name: "trailing // comment with a stray paren is stripped entirely",
			in:   `foo(bar) // note: unbalanced ) on purpose`,
			want: `foo(bar) `,
		},
		{
			name: "a .String()-looking substring inside a string literal is stripped",
			in:   `slog.Info("call .String() on it", x)`,
			want: `slog.Info(, x)`,
		},
		{
			name:        "an unterminated backtick raw string reports still-open at end of line",
			in:          "slog.Info(`unterminated raw string with a stray )",
			want:        `slog.Info(`,
			wantOpenRaw: true,
		},
		{
			name:        "a line starting inside an open raw string strips up to its closing backtick",
			in:          "leftover raw content with a stray ) here` and real code msg.String())",
			openRaw:     true,
			want:        ` and real code msg.String())`,
			wantOpenRaw: false,
		},
		{
			name:        "a line starting inside an open raw string that never closes stays open",
			in:          "still more raw content, no closing backtick on this line either",
			openRaw:     true,
			want:        ``,
			wantOpenRaw: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, gotOpenRaw := stripStringsAndComments(tt.in, tt.openRaw)
			if got != tt.want {
				t.Errorf("stripStringsAndComments(%q, %v) = %q, want %q", tt.in, tt.openRaw, got, tt.want)
			}
			if gotOpenRaw != tt.wantOpenRaw {
				t.Errorf("stripStringsAndComments(%q, %v) stillOpenRaw = %v, want %v", tt.in, tt.openRaw, gotOpenRaw, tt.wantOpenRaw)
			}
		})
	}
}

// TestJoinSinkCallClosesParenCountBypass is a permanent regression test proving the round-13 fix
// actually closes the bypass described in TestNoRawSFSObjectDumpInLogsOrErrors's doc comment:
// before the fix, a stray ')' inside a string literal on the sink call's first physical line could
// make the naive strings.Count(lines[i], "(") - strings.Count(lines[i], ")") tally hit zero
// immediately, stopping the join loop before it ever reached a later physical line holding the
// actual raw .String() dump -- so the guard never saw it. With stripStringsAndComments feeding the
// tally, the stray ')' inside the string no longer cancels out the call's real opening '(', so the
// join correctly continues on to the second line and the .String() call is caught.
func TestJoinSinkCallClosesParenCountBypass(t *testing.T) {
	lines := []string{
		`slog.Info("something with a stray paren )",`,
		`	"response", msg.String())`,
	}

	// Sanity check: confirm this fixture actually reproduces the pre-fix bug condition, i.e. that
	// naively counting every paren character in line 0's raw text (including the one inside the
	// string literal) would bring the running depth down to (or below) zero after line 0 alone --
	// which is exactly what let the old code stop joining before reaching line 1.
	naiveDepth := strings.Count(lines[0], "(") - strings.Count(lines[0], ")")
	if naiveDepth > 0 {
		t.Fatalf("test fixture no longer reproduces the pre-fix bypass condition (naive paren depth "+
			"after line 0 = %d, want <= 0) -- update the fixture so it still demonstrates the bug this "+
			"test guards against", naiveDepth)
	}

	joined, endIdx := joinSinkCall(lines, 0)
	if endIdx != len(lines)-1 {
		t.Fatalf("joinSinkCall(lines, 0) stopped at line %d, want it to consume all %d lines (endIdx=%d) "+
			"-- the stray ')' inside the string literal on line 0 incorrectly balanced the call's opening "+
			"paren, reproducing the round-13 bypass", endIdx, len(lines), len(lines)-1)
	}
	if !stringCallRe.MatchString(joined) {
		t.Fatalf("joined call text does not contain a .String() call, so the credential-leak guard "+
			"would have missed it; joined=%q", joined)
	}
}

// TestJoinSinkCallCarriesRawStringStateAcrossLines is a permanent regression test for the round-14
// fix described on stripStringsAndComments' and joinSinkCall's doc comments: a backtick raw string
// left open at the end of one physical line must still be treated as open at the start of the
// next, or a real .String()/.unsafeRawString() call further down gets swallowed as if it were part
// of the still-open raw string's content.
//
// The fixture: line 0 opens a sink call and, within the same call, a backtick raw string that is
// NOT closed on that line. Line 1 contains the rest of that raw string's content (including a
// stray ')' that must stay stripped), the backtick that actually closes it, and then real code --
// a genuine .String() call and the sink call's real closing paren.
func TestJoinSinkCallCarriesRawStringStateAcrossLines(t *testing.T) {
	lines := []string{
		"slog.Info(`unterminated raw string with a stray",
		"closing backtick here` and a real call msg.String())",
	}

	// Sanity check: confirm this fixture actually reproduces the pre-fix bug condition, i.e. that
	// stripping each line independently with no raw-string state carried over from the previous
	// line (the old, always-openRaw=false-per-line behavior) hides the real .String() call: line 1
	// gets rescanned from state "none", so its own closing backtick is misread as *opening* a new
	// raw string, swallowing "and a real call msg.String())" as if it were raw content.
	strippedOldWay0, _ := stripStringsAndComments(lines[0], false)
	strippedOldWay1, _ := stripStringsAndComments(lines[1], false) // bug: always starts fresh, ignoring line 0's still-open raw string
	oldJoined := strippedOldWay0 + "\n" + strippedOldWay1 + "\n"
	if stringCallRe.MatchString(oldJoined) {
		t.Fatalf("test fixture no longer reproduces the pre-fix bypass condition (stripping each line "+
			"independently still finds the .String() call) -- update the fixture so it still demonstrates "+
			"the bug this test guards against; oldJoined=%q", oldJoined)
	}

	joined, endIdx := joinSinkCall(lines, 0)
	if endIdx != len(lines)-1 {
		t.Fatalf("joinSinkCall(lines, 0) stopped at line %d, want it to consume all %d lines (endIdx=%d) "+
			"-- the still-open raw string from line 0 was not carried into line 1, reproducing the round-14 "+
			"multi-line raw-string bypass", endIdx, len(lines), len(lines)-1)
	}
	if !stringCallRe.MatchString(joined) {
		t.Fatalf("joined call text does not contain a .String() call, so the credential-leak guard "+
			"would have missed it (multi-line raw-string bypass not closed); joined=%q", joined)
	}
}

// TestSinkStartReMatchesAllSinkForms is a small table-driven test confirming the round-14
// broadening of sinkStartRe (fmt.Sprint/Sprintln/Fprint/Fprintln, log.Fatal/Fatalf/Fatalln,
// slog.Log) actually matches the new forms, alongside the previously-supported forms continuing to
// match and an unrelated call continuing not to.
func TestSinkStartReMatchesAllSinkForms(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"fmt.Sprint (bare) now matches", `x := fmt.Sprint(a, b)`, true},
		{"fmt.Sprintln now matches", `x := fmt.Sprintln(a, b)`, true},
		{"fmt.Fprint (bare) now matches", `fmt.Fprint(w, a)`, true},
		{"fmt.Fprintln now matches", `fmt.Fprintln(w, a)`, true},
		{"log.Fatal now matches", `log.Fatal(err)`, true},
		{"log.Fatalf now matches", `log.Fatalf("boom: %v", err)`, true},
		{"log.Fatalln now matches", `log.Fatalln(err)`, true},
		{"slog.Log now matches", `slog.Log(ctx, slog.LevelInfo, "msg")`, true},
		{"previously-supported fmt.Sprintf still matches", `fmt.Sprintf("%v", x)`, true},
		{"previously-supported slog.Info still matches", `slog.Info("msg", "k", v)`, true},
		{"previously-supported log.Printf still matches", `log.Printf("boom: %v", err)`, true},
		{"unrelated call does not match", `strings.TrimSpace(x)`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sinkStartRe.MatchString(tt.line); got != tt.want {
				t.Errorf("sinkStartRe.MatchString(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestStringCallReMatchesUnsafeRawStringToo is a small table-driven test confirming the round-14
// addition to stringCallRe: it must flag .unsafeRawString() the same way it flags .String(), while
// not flagging unrelated method calls (like StringRedacted(), which is the safe one).
func TestStringCallReMatchesUnsafeRawStringToo(t *testing.T) {
	tests := []struct {
		name string
		line string
		want bool
	}{
		{"plain .String() still matches", `o.String()`, true},
		{"new .unsafeRawString() matches", `val.unsafeRawString()`, true},
		{"StringRedacted() does not match (it's the safe one)", `o.StringRedacted()`, false},
		{"unrelated call does not match", `o.GetString("k")`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringCallRe.MatchString(tt.line); got != tt.want {
				t.Errorf("stringCallRe.MatchString(%q) = %v, want %v", tt.line, got, tt.want)
			}
		})
	}
}

// TestGetStringSensitiveKeyReMatches is a small table-driven test confirming getStringSensitiveKeyRe
// (round-28) matches an ordinary `.GetString("key")` call and captures the literal key name, while
// not matching an unrelated call shape -- mirroring the direct-unit-test pattern this file already
// uses for sinkStartRe/stringCallRe above (round-29: this regex previously had no dedicated test of
// its own, unlike every other regex this file defines).
func TestGetStringSensitiveKeyReMatches(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    bool
		wantKey string
	}{
		{"plain GetString call matches, captures key", `o.GetString("loginKey")`, true, "loginKey"},
		{"GetString call on a chained selector matches", `msg.Params.GetString("un")`, true, "un"},
		{"key with digits/underscores matches", `o.GetString("phone_model2")`, true, "phone_model2"},
		{"GetInt does not match", `o.GetInt("level")`, false, ""},
		{"GetString with no arguments does not match", `o.GetString()`, false, ""},
		{"unrelated call does not match", `strings.TrimSpace(x)`, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := getStringSensitiveKeyRe.FindStringSubmatch(tt.line)
			got := m != nil
			if got != tt.want {
				t.Fatalf("getStringSensitiveKeyRe.MatchString(%q) = %v, want %v", tt.line, got, tt.want)
			}
			if got && m[1] != tt.wantKey {
				t.Errorf("getStringSensitiveKeyRe captured key %q, want %q", m[1], tt.wantKey)
			}
		})
	}
}

// TestRedactWrappedGetStringReMatches is a small table-driven test confirming
// redactWrappedGetStringRe (round-28) matches the one safe form -- a GetString(key) call
// immediately wrapped in sfs.Redact(...) -- while not matching a bare, unwrapped GetString(key) call or
// one wrapped in something else. Same round-29 "no dedicated test" gap as getStringSensitiveKeyRe
// above.
func TestRedactWrappedGetStringReMatches(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		want    bool
		wantKey string
	}{
		{"sfs.Redact-wrapped call on a chained selector matches", `sfs.Redact(msg2.Params.GetString("loginKey"))`, true, "loginKey"},
		{"sfs.Redact-wrapped call on a bare receiver matches", `sfs.Redact(o.GetString("at"))`, true, "at"},
		{"bare unwrapped GetString does not match", `o.GetString("loginKey")`, false, ""},
		{"GetString wrapped in something other than sfs.Redact does not match", `strings.ToUpper(o.GetString("loginKey"))`, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := redactWrappedGetStringRe.FindStringSubmatch(tt.line)
			got := m != nil
			if got != tt.want {
				t.Fatalf("redactWrappedGetStringRe.MatchString(%q) = %v, want %v", tt.line, got, tt.want)
			}
			if got && m[1] != tt.wantKey {
				t.Errorf("redactWrappedGetStringRe captured key %q, want %q", m[1], tt.wantKey)
			}
		})
	}
}

// TestFindUnsafeSensitiveGetStringCallsCatchesSameKeyPositionalFalseNegative is the round-29
// regression test for the false-negative gap findUnsafeSensitiveGetStringCalls' own doc comment
// describes: a genuinely-unsafe raw GetString(key) occurrence in the same sink call as a safe
// sfs.Redact(...)-wrapped occurrence of the SAME key name must still be caught, not incorrectly skipped
// just because a key-name-only safety check (the pre-round-29 logic) would have marked that whole
// key name as safe from the wrapped occurrence alone.
func TestFindUnsafeSensitiveGetStringCallsCatchesSameKeyPositionalFalseNegative(t *testing.T) {
	// A sanity check that "loginKey" really is registered as sensitive, so this test fixture
	// actually exercises the intended code path.
	if !sfs.SensitiveSFSKeys["loginKey"] {
		t.Fatal("test fixture assumes \"loginKey\" is a registered sensitive key (sfs.SensitiveSFSKeys, sfsobject.go)")
	}

	rawJoined := `slog.Info("x", "masked", sfs.Redact(o.GetString("loginKey")), "raw", o.GetString("loginKey"))`

	got := findUnsafeSensitiveGetStringCalls(rawJoined)
	if len(got) != 1 {
		t.Fatalf("findUnsafeSensitiveGetStringCalls found %d unsafe occurrence(s), want exactly 1 (the second, "+
			"unwrapped GetString(\"loginKey\") call) -- got: %+v", len(got), got)
	}
	if got[0].key != "loginKey" {
		t.Errorf("flagged occurrence key = %q, want %q", got[0].key, "loginKey")
	}
	// The flagged occurrence must be the SECOND (unwrapped) one, not the first (safely wrapped) one
	// -- confirm its offset falls after the sfs.Redact(...) call closes.
	wrapEnd := strings.Index(rawJoined, "))") + len("))")
	if got[0].offset < wrapEnd {
		t.Errorf("flagged occurrence at offset %d falls inside/before the sfs.Redact(...)-wrapped call "+
			"(which ends at %d) -- the WRAPPED occurrence was incorrectly flagged instead of the raw one",
			got[0].offset, wrapEnd)
	}

	// Sanity check this fixture actually reproduces the pre-round-29 bug condition: the OLD,
	// key-name-only logic would have found zero unsafe occurrences here (since "loginKey" appearing
	// wrapped once anywhere in the block was enough to mark the whole key name safe), even though a
	// genuinely-unsafe raw occurrence is present.
	oldSafeKeys := map[string]bool{}
	for _, m := range redactWrappedGetStringRe.FindAllStringSubmatch(rawJoined, -1) {
		oldSafeKeys[m[1]] = true
	}
	oldFoundUnsafe := false
	for _, m := range getStringSensitiveKeyRe.FindAllStringSubmatch(rawJoined, -1) {
		if sfs.SensitiveSFSKeys[m[1]] && !oldSafeKeys[m[1]] {
			oldFoundUnsafe = true
		}
	}
	if oldFoundUnsafe {
		t.Fatal("test fixture no longer reproduces the pre-round-29 false-negative condition (the old, " +
			"key-name-only logic would have caught this too) -- update the fixture so it still demonstrates " +
			"the bug this test guards against")
	}
}
