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
			depth := 0
			var joined strings.Builder
			for i < len(lines) {
				depth += strings.Count(lines[i], "(") - strings.Count(lines[i], ")")
				joined.WriteString(lines[i])
				joined.WriteByte('\n')
				if depth <= 0 {
					break
				}
				i++
			}
			if !stringCallRe.MatchString(joined.String()) {
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
