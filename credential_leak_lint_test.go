package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
		`buildings.go:slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())`:                  "o is a gameplay response entry (building/mail/visitor/tech); confirmed no credential fields ever appear here (round-11 automation-logic audit)",
		`conn.go:slog.Info(label, "cmd", msg.Cmd, "response", msg.Params.String())`:                                        "logCommandResult is only ever reached from post-login gameplay commands (sendAndWait's callers), no credential fields (round-11 automation-logic audit)",
		`conn.go:slog.Warn(label+" no-op (expected)", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.String())`: "same logCommandResult scope as above",
		`conn.go:slog.Warn(label+" no-op (status=0, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.String())`:      "same logCommandResult scope as above",
		`interactive.go:slog.Info("shutting down", "signal", sig.String())`:                                                "sig is an os.Signal, not an SFSObject -- String() here is the standard library's, unrelated to this bug class",
		`interactive.go:slog.Error("unparseable JSON number", "key", key, "value", val.String())`:                          "val is a json.Number, not an SFSObject -- String() here is encoding/json's, unrelated to this bug class",
	}

	seen := map[string]bool{}
	sinkStartRe := regexp.MustCompile(`\b(slog\.(Info|Warn|Error|Debug)|fmt\.(Errorf|Printf|Fprintf|Sprintf|Println|Print)|log\.(Printf|Print|Println))\(`)
	stringCallRe := regexp.MustCompile(`\.String\(\)`)

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
// tally and its .String() substring match can't be fooled by a stray ')' or a ".String()"-looking
// substring that only appears inside a string/rune literal or a comment.
//
// This is deliberately a lightweight line-scanner, not a full tokenizer, matching this file's
// existing design philosophy (see the doc comment on TestNoRawSFSObjectDumpInLogsOrErrors): it
// does not track raw-string state *across* physical lines, so a backtick string that spans
// multiple lines is not specially handled beyond scanning each line independently. No known
// instance of that pattern exists among this repo's sink-call sites today.
func stripStringsAndComments(line string) string {
	var out strings.Builder
	const (
		none = iota
		inDoubleQuote
		inRune
		inRawString
	)
	state := none
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
					// Rest of the physical line is a "//" comment -- stop here.
					return out.String()
				}
				out.WriteByte(c)
			default:
				out.WriteByte(c)
			}
		}
	}
	return out.String()
}

// joinSinkCall joins lines[start:] forward, using stripStringsAndComments on each line before
// tallying its paren balance, until the running depth balances back to (or below) zero -- mirroring
// how a multi-line slog.*/fmt.*/log.Print* call closes. It returns the joined text (built from the
// stripped lines, so string/rune-literal contents and "//" comments never leak into a later
// .String() substring match) and the index of the last line consumed, so the caller can resume
// scanning after it.
func joinSinkCall(lines []string, start int) (joined string, endIdx int) {
	var b strings.Builder
	depth := 0
	i := start
	for i < len(lines) {
		stripped := stripStringsAndComments(lines[i])
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
		name string
		in   string
		want string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripStringsAndComments(tt.in)
			if got != tt.want {
				t.Errorf("stripStringsAndComments(%q) = %q, want %q", tt.in, got, tt.want)
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
	if !regexp.MustCompile(`\.String\(\)`).MatchString(joined) {
		t.Fatalf("joined call text does not contain a .String() call, so the credential-leak guard "+
			"would have missed it; joined=%q", joined)
	}
}
