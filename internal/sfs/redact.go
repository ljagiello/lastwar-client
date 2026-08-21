package sfs

import "unicode/utf8"

// MaxRedactRuneScanInput bounds how large an input Redact() will fully rune-scan (the []rune(s)
// conversion below) before switching to a bounded fast path that extracts just the first/last 4
// runes directly without ever materializing a full copy of the input. Round-39 fix: Redact()
// previously always converted the FULL input to []rune regardless of size -- an ~4x-amplifying
// allocation for ASCII input (each 1-byte rune becomes a 4-byte int32) -- and sfsobject.go's
// RedactSFSValue calls this on a sensitive field's value with NO format-time budget at all, unlike
// every other value shape StringRedacted() formats (all bounded by formatBudget/maxFormattedNodes,
// per formatSFSValueRedacted's own chargeUpTo/truncateAtRuneBoundary handling). A hostile/spoofed
// server (or crafted -decode-stream capture) can tag a field literally named
// loginKey/accessToken/airKey as an oversized string (up to MaxFrameSize=64MiB), forcing an
// unbounded ~320MB-peak allocation on every StringRedacted() call that reaches it -- and this
// fires repeatedly per session (conn.go's logCommandResult on every failed response, buildings.go's
// push-handling switch on every observed push), not just once. Any input above this threshold is
// comfortably long enough -- even under a pathological 4-bytes-per-rune input -- to guarantee
// Redact()'s own length-scaling formula (k := n/8, capped at 4) has already saturated at k=4, so
// the fast path below can skip straight to that shape without computing an exact rune count or
// allocating a full []rune of the input.
const MaxRedactRuneScanInput = 1024

// FirstNRunesPrefix returns the byte-slice prefix of s covering its first n runes (or all of s if
// it has fewer), without ever converting s to []rune -- Go's built-in `for range` over a string
// already decodes UTF-8 one rune at a time and lets this stop after n runes instead of continuing
// through the rest of a potentially huge string.
func FirstNRunesPrefix(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// LastNRunesSuffix is FirstNRunesPrefix's mirror for the tail: walks backward from the end of s,
// decoding one rune at a time via utf8.DecodeLastRuneInString, stopping after n runes instead of
// ever scanning forward through the rest of a potentially huge string.
func LastNRunesSuffix(s string, n int) string {
	count := 0
	end := len(s)
	for end > 0 && count < n {
		_, size := utf8.DecodeLastRuneInString(s[:end])
		end -= size
		count++
	}
	return s[end:]
}

func Redact(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "***"
	}
	if len(s) > MaxRedactRuneScanInput {
		return FirstNRunesPrefix(s, 4) + "..." + LastNRunesSuffix(s, 4)
	}
	// Slice by rune, not byte, boundary: SensitiveSFSKeys covers fields that can
	// legitimately carry multi-byte UTF-8 (googleName is a Google account display
	// name, e.g. CJK; mail is an internationalized email address). Raw byte-index
	// slicing (the pre-fix s[:4]/s[len(s)-4:] here) can land mid-rune and emit
	// invalid UTF-8 into both slog's JSON sink and StringRedacted()'s raw
	// fmt.Printf terminal sink.
	r := []rune(s)
	n := len(r)
	if n <= 8 {
		// Byte length is >8 (checked above) but rune count is small -- a short
		// multi-byte string (e.g. a 3-rune CJK name at 3 bytes/rune = 9 bytes).
		// Not enough runes to usefully show a non-overlapping prefix/suffix
		// slice without leaking most or all of it back out, so fully Redact
		// instead, same as the short-input case above.
		return "***"
	}
	// How many runes to reveal from each end. This used to be a flat 4/4
	// regardless of length, which is fine for long opaque tokens
	// (loginKey/accessToken, typically 32-64+ chars, where showing a fixed
	// prefix/suffix as a correlation aid across log lines doesn't meaningfully
	// weaken anything) but badly over-exposes realistic human password
	// lengths: sfsobject.go's RedactSFSValue calls this for EVERY sensitive
	// string field, including "pw"/"password", not just long tokens, and the
	// old flat rule revealed a clear MAJORITY of a realistic password --
	// Redact("Summer2024!") (11 runes) used to produce "Summ...024!", 8 of 11
	// chars (~73%) visible.
	//
	// Scale k with length instead: n/8, capped at 4. This keeps the reveal a
	// clear minority across the realistic password range (~18-25% visible for
	// 9-20 rune input, well under a 40% ceiling) while converging on exactly
	// the original first-4/last-4 shape once n reaches 32 -- long enough that
	// revealing 8 chars is itself a small minority (25%) -- and never reveals
	// more than that fixed 4/4 prefix/suffix for even longer tokens, keeping
	// the shape useful for visually correlating "is this the same token as
	// before" across log lines.
	k := min(n/8, 4)
	return string(r[:k]) + "..." + string(r[n-k:])
}
