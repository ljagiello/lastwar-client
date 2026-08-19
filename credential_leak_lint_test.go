package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestNoRawSFSObjectDumpInLogsOrErrors is the round-11 process fix (testing-rigor's "minor"
// finding): the credential-leak bug class this round's audit hunted -- a decoded/outgoing
// SFSObject's raw .String() dump flowing into a log line or wrapped error, where it might carry a
// live loginKey/accessToken/airKey/shumeiBoxId -- has now recurred across several call sites
// (login.go's push.account.login.new site fixed last round, then interactive.go, gsl.go's
// raw-body errors, crossserver.go's debug dump, and login.go's waitFor skip-logger, all in this
// one round alone) despite each individual instance being fixed as it was found. Nothing in CI
// would catch a NEW instance of the same pattern. This test is that catch: it scans every
// non-test .go source file for a slog.Info/Warn/Error/Debug or fmt.Errorf call that embeds a
// `.String()` call, and fails on anything not in the explicit allowlist below -- so a future PR
// that reintroduces this pattern fails the build instead of waiting for the next audit round to
// notice.
//
// This is a deliberately simple single-line regex scan, not a full go/ast type-checker: every
// site flagged so far in this codebase's history has been a single-line call, and keeping the
// check simple keeps false positives (and the maintenance burden of this test itself) low. It
// will not catch a `.String()` result stashed in a variable first and logged later (see conn.go's
// DoHandshake "skipped envelope" fallback, which does exactly that and is deliberately not on
// this allowlist since the regex can't see it) -- a known, acceptable gap for a lightweight
// process guard, not a substitute for the credential-leak sweep an audit round does.
func TestNoRawSFSObjectDumpInLogsOrErrors(t *testing.T) {
	// allowlist maps "file.go:<trimmed line text>" to why that exact line is safe. Every entry
	// here was individually confirmed safe by this repo's own round-11 audit (or, for lines
	// predating it, by this test's own author at the time it was added) -- add a new entry only
	// with the same level of scrutiny: confirm the specific message/data this line logs can never
	// carry loginKey/accessToken/airKey/shumeiBoxId/rt/pw, not just that it seems unlikely.
	allowlist := map[string]string{
		`buildings.go:slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())`:                                      "o is a gameplay response entry (building/mail/visitor/tech); confirmed no credential fields ever appear here (round-11 automation-logic audit)",
		`buildings.go:slog.Info("observed push.queue.add", "params", msg.Params.String())`:                                                     "gameplay push, no credential fields (round-11 automation-logic audit)",
		`buildings.go:slog.Info("observed push.build.queue.info", "params", msg.Params.String())`:                                              "gameplay push, no credential fields (round-11 automation-logic audit)",
		`buildings.go:slog.Info("observed other push", "cmd", msg.Cmd, "params", msg.Params.String())`:                                         "gameplay push, no credential fields (round-11 automation-logic audit)",
		`crossserver.go:slog.Info("handshake OK", "response", hsResp.String())`:                                                                "vanilla SFS2X Handshake response (api/cl echo only, per conn.go:DoHandshake's doc comment) -- no login/account fields exist in this response shape",
		`conn.go:slog.Info(label, "cmd", msg.Cmd, "response", msg.Params.String())`:                                                            "logCommandResult is only ever reached from post-login gameplay commands (sendAndWait's callers), no credential fields (round-11 automation-logic audit)",
		`conn.go:slog.Warn(label+" no-op (expected)", "cmd", msg.Cmd, "errorCode", code, "response", msg.Params.String())`:                     "same logCommandResult scope as above",
		`conn.go:slog.Warn(label+" no-op (status=0, no errorCode)", "cmd", msg.Cmd, "response", msg.Params.String())`:                          "same logCommandResult scope as above",
		`login.go:slog.Info("handshake OK", "response", hsResp.String())`:                                                                      "same vanilla SFS2X Handshake response as crossserver.go's copy above",
		`login.go:return nil, fmt.Errorf("SEND-CODE FAILED: errorCode=%v full=%s: %w", ec.Val, msg.Params.String(), ErrAuthRejected)`:          "account.login.send.verify.code's ack shape carries no account data, only a success/errorCode flag -- the real credential-bearing response is the separate push.account.login.new handled a few lines below with explicit redaction",
		`login.go:slog.Info("server accepted", "response", msg.Params.String())`:                                                               "same account.login.send.verify.code ack as above",
		`login.go:return nil, fmt.Errorf("LOGIN-WITH-CODE FAILED: errorCode=%v full=%s: %w", ec.Val, ackMsg.Params.String(), ErrAuthRejected)`: "account.login.new's direct response is a terse {success=true} ack per its own comment -- the real account data arrives separately as push.account.login.new, handled a few lines below with explicit redaction",
		`login.go:slog.Info("ack", "response", ackMsg.Params.String())`:                                                                        "same terse account.login.new ack as above",
		`interactive.go:slog.Info("shutting down", "signal", sig.String())`:                                                                    "sig is an os.Signal, not an SFSObject -- String() here is the standard library's, unrelated to this bug class",
		`interactive.go:slog.Error("unparseable JSON number", "key", key, "value", val.String())`:                                              "val is a json.Number, not an SFSObject -- String() here is encoding/json's, unrelated to this bug class",
	}

	seen := map[string]bool{}
	sinkRe := regexp.MustCompile(`\b(slog\.(Info|Warn|Error|Debug)|fmt\.Errorf)\(.*\.String\(\)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", name, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			trimmed := strings.TrimSpace(line)
			if !sinkRe.MatchString(trimmed) {
				continue
			}
			key := name + ":" + trimmed
			if _, ok := allowlist[key]; ok {
				seen[key] = true
				continue
			}
			t.Errorf("%s:%d: a slog.*/fmt.Errorf call embeds a raw .String() call -- this is exactly the "+
				"credential-leak pattern this repo has hit repeatedly (loginKey/accessToken/airKey/shumeiBoxId "+
				"can ride along in a decoded SFSObject with no field-level redaction). Use "+
				"SFSObject.StringRedacted() instead, or if this specific line is genuinely safe (confirmed no "+
				"credential field can ever appear in this data), add it to the allowlist in this test with a "+
				"one-line justification.\n\tline: %s", name, i+1, trimmed)
		}
	}

	for key := range allowlist {
		if !seen[key] {
			t.Errorf("allowlist entry no longer matches any source line (line was fixed/removed/reworded) -- remove this stale entry: %s", key)
		}
	}
}
