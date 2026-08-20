package main

import (
	"bufio"
	"bytes"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestGslOptFor covers gslOptFor's 3-way priority order (login > fix > new). The "loginKey set"
// case also sets GameUid, so a priority-order regression (e.g. checking GameUid first) would be
// caught here rather than only in a case where the fields never overlap.
func TestGslOptFor(t *testing.T) {
	cases := []struct {
		name         string
		ident        *deviceIdentity
		wantOpt      string
		wantLoginKey string
	}{
		{
			name:         "loginKey set (wins over gameUid, both present)",
			ident:        &deviceIdentity{LoginKey: "lk-123", GameUid: "uid-456"},
			wantOpt:      "login",
			wantLoginKey: "lk-123",
		},
		{
			name:    "gameUid set, no loginKey",
			ident:   &deviceIdentity{GameUid: "uid-456"},
			wantOpt: "fix",
		},
		{
			name:    "neither set",
			ident:   &deviceIdentity{},
			wantOpt: "new",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := gslOptFor(c.ident)
			if got.Opt != c.wantOpt {
				t.Errorf("Opt = %q, want %q", got.Opt, c.wantOpt)
			}
			if got.LoginKey != c.wantLoginKey {
				t.Errorf("LoginKey = %q, want %q", got.LoginKey, c.wantLoginKey)
			}
		})
	}
}

func TestRedact(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"empty string", "", ""},
		{"short string (<=8 chars)", "abcd1234", "***"},
		{"exactly 8 chars", "12345678", "***"},
		// Round-33 regression: the ASCII "exactly 8 chars" case above has byte length == rune
		// count == 8, so it can't distinguish the outer len(s)<=8 byte-length prefilter from the
		// inner n<=8 rune-count check a few lines into redact()'s body -- a regression loosening
		// either one to "< 8" would still pass it. An exactly-8-rune, all-multi-byte CJK string
		// has byte length 24 (> 8, so it clears the outer prefilter) but rune count exactly 8,
		// isolating the inner rune-count check specifically: it must still fully redact to "***",
		// not fall through to the general k=n/8 formula (which would reveal 1 rune from each end).
		{"exactly 8 runes, all multi-byte (byte length 24)", "田中太郎鈴木一郎", "***"},
		// 16 runes: k = n/8 = 2, so this reveals 2 runes from each end, not the
		// old flat 4/4 (see the scaling-formula regression cases below for why).
		{"longer than 8 chars", "abcdefghijklmnop", "ab...op"},
		// Round-35 regression: n/7 and n/8 diverge only for n in {14,15,21,22,23,28,29,30,31} (n<=8
		// short-circuits above; n>=32 is capped to k=4 under either divisor) -- every OTHER case in
		// this table happens to land outside that narrow band, so a regression changing the divisor
		// constant from 8 to 7 would pass the whole suite unnoticed (confirmed via mutation testing).
		// 22 runes: k = 22/8 = 2 (not 22/7 = 3), isolating the divisor specifically.
		{"22 runes isolates the n/8 divisor from n/7 (round-35 mutation-testing gap)", "abcdefghijklmnopqrstuv", "ab...uv"},
		// Regression for a byte-index-vs-rune-index bug: sensitiveSFSKeys (sfsobject.go)
		// includes googleName (a Google account display name) and mail (an
		// internationalized email address), both of which can legitimately carry
		// multi-byte UTF-8. redact() used to slice s[:4]/s[len(s)-4:] by raw byte
		// index, which can land mid-rune on such input and emit invalid UTF-8 into
		// both slog's JSON sink and StringRedacted()'s raw fmt.Printf terminal sink.
		{
			name:  "CJK name-shaped input (googleName-like, 12 runes)",
			input: "田中太郎鈴木一郎佐藤次郎",
			want:  "田...郎",
		},
		{
			name:  "internationalized email-shaped input (mail-like, 16 runes)",
			input: "田中太郎@example.com",
			want:  "田中...om",
		},
		// Regression for the majority-exposure bug: redact() used to apply the same
		// flat first-4/last-4 shape to any string over 8 runes regardless of which
		// sensitive key invoked it, and sfsobject.go's redactSFSValue calls it for
		// EVERY sensitive string field -- including "pw"/"password", not just long
		// opaque tokens. For a realistic human password length (9-20 runes), that
		// revealed a clear MAJORITY: redact("Summer2024!") (11 runes) used to
		// produce "Summ...024!", exposing 8 of 11 characters (~73%). The generic
		// "9-20 rune input reveals a clear minority" check below (not just this
		// exact-match case) is what actually proves the fix, since an exact-match
		// assertion alone would not have caught the original majority-exposure bug.
		{
			name:  "realistic password length (11 runes) reveals only a small minority",
			input: "Summer2024!",
			want:  "S...!",
		},
		// Long opaque tokens (loginKey/accessToken, typically 32-64+ chars) must
		// keep a useful fixed-width prefix/suffix for visually correlating "is this
		// the same token as before" across log lines -- the scaling formula must not
		// over-redact these down to uselessness. 40 runes: k = min(4, 40/8) = 4,
		// converging back on the original first-4/last-4 shape.
		{
			name:  "long opaque token (40 chars) keeps a useful first-4/last-4 shape",
			input: "ABCD" + strings.Repeat("x", 32) + "WXYZ",
			want:  "ABCD...WXYZ",
		},
		// Round-39 regression: an input well past maxRedactRuneScanInput (1024 bytes) must take
		// the bounded fast path (firstNRunesPrefix/lastNRunesSuffix) and still produce the
		// identical first-4/last-4 shape the full rune-scan path would have, proving the fast
		// path's output isn't just fast but also correct.
		{
			name:  "huge ASCII input (2000 bytes) takes the bounded fast path, same first-4/last-4 shape",
			input: "ABCD" + strings.Repeat("x", 1992) + "WXYZ",
			want:  "ABCD...WXYZ",
		},
		{
			name:  "huge multi-byte input (well over the byte-length threshold) takes the bounded fast path with valid UTF-8 boundaries",
			input: "田中太郎" + strings.Repeat("鈴", 500) + "佐藤次郎",
			want:  "田中太郎...佐藤次郎",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redact(c.input)
			if got != c.want {
				t.Errorf("redact(%q) = %q, want %q", c.input, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("redact(%q) = %q is not valid UTF-8", c.input, got)
			}
			if c.input != "" && got != "***" && strings.Contains(got, c.input) {
				t.Errorf("redact(%q) = %q leaks the full original secret", c.input, got)
			}
			// For a realistic password length (9-20 runes), the number of runes
			// that appear unredacted in the output must be a clear MINORITY of
			// the input -- no more than 40% -- not the majority the old flat
			// first-4/last-4 rule exposed (8 of 11, ~73%, for an 11-rune
			// password). Checked generically off the actual output (not the
			// "want" literal above) so this keeps failing if the scaling
			// formula regresses back toward flat 4/4 exposure.
			if n := utf8.RuneCountInString(c.input); n >= 9 && n <= 20 {
				visible := utf8.RuneCountInString(strings.ReplaceAll(got, "...", ""))
				maxVisible := n * 40 / 100
				if visible > maxVisible {
					t.Errorf("redact(%q) = %q reveals %d of %d runes (%d%%), want at most %d runes (40%% of input length) -- majority-exposure regression for a realistic password length",
						c.input, got, visible, n, visible*100/n, maxVisible)
				}
			}
		})
	}
}

// TestRedactHugeInputAvoidsFullRuneScanAllocation is the round-39 regression test for the MAJOR
// finding that redact() (login.go) used to unconditionally convert its ENTIRE input to []rune
// regardless of size -- an ~4x-amplifying allocation for ASCII input -- with sfsobject.go's
// redactSFSValue calling this on a sensitive field's value with NO format-time budget at all
// (unlike every other value shape StringRedacted() formats). A hostile/spoofed server could tag a
// field literally named loginKey/accessToken as an oversized string (up to maxFrameSize=64MiB),
// forcing an unbounded ~320MB-peak allocation on every StringRedacted() call that reaches it.
// TestRedact's own huge-input cases above prove the fast path's OUTPUT is correct; this proves the
// fast path is actually bounded -- measuring actual BYTES allocated (not allocation event COUNT,
// which a single big []rune backing array wouldn't distinguish from a handful of tiny ones) via
// runtime.MemStats, asserting redact() on a 10MB input allocates only a small number of bytes, not
// bytes proportional to the input size (a full []rune(s) conversion of a 10MB ASCII string alone
// would allocate a ~40MB backing array -- four bytes per rune -- trivially distinguishable from
// the handful of small string headers/slices the bounded fast path needs).
func TestRedactHugeInputAvoidsFullRuneScanAllocation(t *testing.T) {
	const hugeSize = 10 * 1024 * 1024 // 10MB -- far past maxRedactRuneScanInput (1024 bytes)
	huge := "ABCD" + strings.Repeat("x", hugeSize) + "WXYZ"

	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got := redact(huge)
	runtime.ReadMemStats(&after)

	if got != "ABCD...WXYZ" {
		t.Fatalf("redact(huge 10MB input) = %q, want %q", got, "ABCD...WXYZ")
	}
	// A full []rune(huge) conversion alone would allocate roughly 4x hugeSize bytes (~40MB) for
	// the backing array; the bounded fast path needs only a handful of tiny string
	// headers/slices. 1MB is generous headroom above what the fast path actually needs, while
	// remaining utterly incompatible with an allocation pattern proportional to a 10MB input.
	const maxWantAllocBytes = 1024 * 1024
	if allocBytes := after.TotalAlloc - before.TotalAlloc; allocBytes > maxWantAllocBytes {
		t.Errorf("redact(huge 10MB input) allocated %d bytes, want at most %d -- the fast path is not actually bounded, redact() likely still scans/allocates proportional to input size", allocBytes, maxWantAllocBytes)
	}
}

// TestReadCodeFromNewlineTerminatedStaysSilent is the baseline case for
// TestReadCodeFromEOFTerminatedWarnsButStillAccepts below: a normal, complete,
// newline-terminated code must return correctly with no diagnostic at all.
func TestReadCodeFromNewlineTerminatedStaysSilent(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := readCodeFrom(strings.NewReader("123456\n"), nil)
	slog.SetDefault(orig)

	if got != "123456" {
		t.Errorf("readCodeFrom = %q, want %q", got, "123456")
	}
	if buf.String() != "" {
		t.Errorf("expected no log output for a normal newline-terminated read, got:\n%s", buf.String())
	}
}

// TestReadCodeFromEOFTerminatedWarnsButStillAccepts is the round-38 regression test for the MAJOR
// finding that readCodeFrom (login.go) used to silently accept a code read that closed via EOF
// before ever seeing a trailing newline -- indistinguishable, with zero diagnostic, from a normal
// complete read. bufio.Reader.ReadString('\n') returns the accumulated partial line PLUS a
// non-nil error (io.EOF) whenever the delimiter is never seen, so a killed/crashed -code-pipe
// writer that emitted only a partial code was silently treated as if it had sent the complete,
// intended code. This can't be rejected outright (a legitimate final line with no trailing
// newline, e.g. `printf '123456'`, produces the identical signature), so this proves the fix
// instead warns -- giving an operator debugging a downstream rejection a concrete signal -- while
// still returning the read value rather than discarding it.
func TestReadCodeFromEOFTerminatedWarnsButStillAccepts(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := readCodeFrom(strings.NewReader("123456"), nil) // no trailing newline -- EOF terminates it
	slog.SetDefault(orig)

	if got != "123456" {
		t.Errorf("readCodeFrom = %q, want %q (an EOF-terminated read must still be accepted, not discarded)", got, "123456")
	}
	logged := buf.String()
	if !strings.Contains(logged, "trailing newline") {
		t.Errorf("expected a Warn mentioning the missing trailing newline, got:\n%s", logged)
	}
	if !strings.Contains(logged, "codeLen=6") {
		t.Errorf("expected the Warn to include codeLen=6, got:\n%s", logged)
	}
}

// TestReadCodeFromBoundedBySizeCap is the round-38 regression test for the MAJOR finding that
// readCodeFrom had no size bound at all (unlike interactive.go's maxControlPipeLineSize for its
// own FIFO reads) -- a misbehaving writer sending unbounded data with no newline could force
// unbounded memory growth. Feeds far more than maxCodePipeLineSize bytes with no newline and
// proves the call returns promptly with output capped at the size limit, not hanging or growing
// without bound, and that the missing-trailing-newline Warn fires here too (the cap is hit before
// any newline is ever seen, the identical signature to a truncated write).
func TestReadCodeFromBoundedBySizeCap(t *testing.T) {
	huge := strings.Repeat("A", maxCodePipeLineSize*4) // far more than the cap, no newline anywhere

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := readCodeFrom(strings.NewReader(huge), nil)
	slog.SetDefault(orig)

	if len(got) > maxCodePipeLineSize {
		t.Fatalf("readCodeFrom returned %d bytes, want at most %d (maxCodePipeLineSize) -- the size cap did not bound the read", len(got), maxCodePipeLineSize)
	}
	if len(got) == 0 {
		t.Fatalf("readCodeFrom returned an empty string for a huge, non-empty input")
	}
	logged := buf.String()
	if !strings.Contains(logged, "trailing newline") {
		t.Errorf("expected a Warn mentioning the missing trailing newline (the size cap was hit before any newline), got:\n%s", logged)
	}
}

// TestCloseConnBeforeExitClosesNonNilConnExactlyOnce is the round-41 regression test for the
// MINOR finding that readCodeFromPipe/readCodeFrom's os.Exit(1) paths used to fire with zero conn
// awareness, even though Login() has a live, already-dialed, heartbeating GameConn in scope at
// their one call site and otherwise closes it explicitly on every other return path in the same
// function -- the identical defer-skipped-cleanup gap round 40 fixed in main.go/interactive.go.
// closeConnBeforeExit itself never calls os.Exit, so (unlike readCodeFrom's own os.Exit(1) branch)
// this can be tested directly in-process without a subprocess re-exec. Proves both halves: a
// non-nil conn's underlying net.Conn.Close() is invoked exactly once, and a nil conn (the shape
// readCodeFrom's own direct unit tests above use, since none of them ever reach an os.Exit(1)
// branch) doesn't panic.
func TestCloseConnBeforeExitClosesNonNilConnExactlyOnce(t *testing.T) {
	t.Run("non-nil conn closes exactly once", func(t *testing.T) {
		underlying := &countingCloseConn{}
		conn := &GameConn{conn: underlying, reader: bufio.NewReaderSize(underlying, 4096)}

		closeConnBeforeExit(conn)

		if got := underlying.callCount(); got != 1 {
			t.Errorf("underlying net.Conn.Close() called %d times, want exactly 1", got)
		}
	})
	t.Run("nil conn does not panic", func(t *testing.T) {
		closeConnBeforeExit(nil)
	})
}
