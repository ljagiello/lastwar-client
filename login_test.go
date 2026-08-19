package main

import "testing"

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
