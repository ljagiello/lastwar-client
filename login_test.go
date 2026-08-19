package main

import (
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
		{"longer than 8 chars", "abcdefghijklmnop", "abcd...mnop"},
		// Regression for a byte-index-vs-rune-index bug: sensitiveSFSKeys (sfsobject.go)
		// includes googleName (a Google account display name) and mail (an
		// internationalized email address), both of which can legitimately carry
		// multi-byte UTF-8. redact() used to slice s[:4]/s[len(s)-4:] by raw byte
		// index, which can land mid-rune on such input and emit invalid UTF-8 into
		// both slog's JSON sink and StringRedacted()'s raw fmt.Printf terminal sink.
		{
			name:  "CJK name-shaped input (googleName-like, 12 runes)",
			input: "田中太郎鈴木一郎佐藤次郎",
			want:  "田中太郎...佐藤次郎",
		},
		{
			name:  "internationalized email-shaped input (mail-like, 16 runes)",
			input: "田中太郎@example.com",
			want:  "田中太郎....com",
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
		})
	}
}
