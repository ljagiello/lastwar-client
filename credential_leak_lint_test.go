package main

import (
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
// Round-14 addition: since SFSObject.String() itself is now safe by default (it delegates to
// StringRedacted(), see sfsobject.go), the genuinely dangerous call is now .unsafeRawString() --
// the renamed, still-unredacted raw dump, meant to be called only from within sfsobject.go itself.
// This pattern flags both: .unsafeRawString() because it's the real, live risk, and plain
// .String() for defense-in-depth (see TestNoRawSFSObjectDumpInLogsOrErrors's doc comment for why
// that's kept even though it's no longer unsafe for SFSObject specifically).
var stringCallRe = regexp.MustCompile(`\.(String|unsafeRawString)\(\)`)

// TestNoRawSFSObjectDumpInLogsOrErrors is the round-11 process fix (testing-rigor's "minor"
// finding), broadened in round 12 after the audit found the original guard's blind spots let a
// real instance (decode.go's -decode-stream tool, buildings.go's -list-buildings dump) slip past
// undetected: the credential-leak bug class this audit series has hunted -- a decoded/outgoing
// SFSObject's raw .String() dump flowing into a log line, wrapped error, or printed output, where
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
// Round-14 change of purpose: SFSObject.String() itself is now safe by default -- it delegates to
// StringRedacted() (see sfsobject.go), so a bare .String() call on an SFSObject can no longer leak
// a credential no matter what sink it flows into, and neither can an implicit fmt.Stringer
// auto-invocation via %v/Println/slog's Any-kind formatting (a path this text-scanning guard could
// never see anyway, since no literal ".String()" appears in source for that -- closing that gap
// required the structural fix in sfsobject.go, not a broader regex here). The raw, unredacted dump
// that used to live under the name String() was renamed to the unexported unsafeRawString(), which
// does NOT satisfy fmt.Stringer and is only meant to be called from within sfsobject.go itself (by
// formatSFSValue's recursive raw-dump path). So this guard's real, live purpose going forward is
// catching an accidental call to that unsafeRawString() escape hatch from outside sfsobject.go --
// stringCallRe below now flags ".unsafeRawString()" the same way it flags ".String()". Plain
// ".String()" stays flagged too, purely for defense-in-depth/historical reasons (four rounds of
// this bug class living under that exact name is reason enough to keep watching it, even though it
// no longer needs to be) -- see the allowlist entries below for the SFSObject.String() call sites,
// which are now unconditionally safe rather than "safe because we checked the data."
func TestNoRawSFSObjectDumpInLogsOrErrors(t *testing.T) {
	// allowlist maps "file.go:<trimmed text of the line where the sink call starts>" to why that
	// call is safe. Every entry here was individually confirmed safe by this repo's own audit
	// rounds -- add a new entry only with the same level of scrutiny: confirm the specific
	// message/data this line logs can never carry a credential field (see sensitiveSFSKeys in
	// sfsobject.go for the current known list), not just that it seems unlikely. Round 12's audit
	// specifically re-verified every entry below against this repo's own docs/*.mdx and found two
	// (the Handshake-response entries) were WRONG -- the response does carry a session token
	// (`tk`) -- so both call sites were switched to StringRedacted() instead of being re-allowlisted.
	allowlist := map[string]string{
		`buildings.go:slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())`:                  "o.String() is now unconditionally safe (round 14: String() delegates to StringRedacted(), see sfsobject.go) regardless of what o contains -- kept in this allowlist rather than removed only because this guard still flags plain .String() generically for defense-in-depth (see the doc comment above), not because this specific line's data was re-verified safe",
		`conn.go:slog.Info(label, "cmd", msg.Cmd, "response", msg.Params.String())`:                                        "msg.Params.String() is now unconditionally safe (round 14: String() delegates to StringRedacted()) -- same reasoning as the buildings.go entry above",
		`conn.go:slog.Warn(label+" no-op (expected)", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.String())`: "same reasoning as the conn.go entry above",
		`conn.go:slog.Warn(label+" no-op (status=0, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.String())`:      "same reasoning as the conn.go entry above",
		`interactive.go:slog.Info("shutting down", "signal", sig.String())`:                                                "sig is an os.Signal, not an SFSObject -- String() here is the standard library's, unrelated to this bug class",
		`interactive.go:slog.Error("unparseable JSON number", "key", key, "value", val.String())`:                          "val is a json.Number, not an SFSObject -- String() here is encoding/json's, unrelated to this bug class",
	}

	seen := map[string]bool{}

	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
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
		relName := filepath.ToSlash(path) // filepath.WalkDir(".", ...) already yields root-relative paths with no "./" prefix

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
				"verifyCode/deviceId/chatToken/tk can ride along in a decoded SFSObject with no field-level "+
				"redaction). Use SFSObject.StringRedacted() instead, or if this specific line is genuinely safe "+
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
