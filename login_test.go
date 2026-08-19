package main

import "testing"

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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redact(c.input); got != c.want {
				t.Errorf("redact(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}
