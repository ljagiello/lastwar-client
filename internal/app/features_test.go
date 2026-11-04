package app

import (
	"bytes"
	"strings"
	"testing"
)

// TestPrintFeaturesShowsTheEffectiveDuelPolicy: a duelPolicy value the run ignores ("Hold") must
// not be printed as the feature's policy; the feature's own default is.
func TestPrintFeaturesShowsTheEffectiveDuelPolicy(t *testing.T) {
	var out bytes.Buffer
	printFeatures(&out, nil, map[string]string{"duel-speedups": "Hold", "radar-claims": "always"})
	lines := map[string]string{}
	for _, l := range strings.Split(out.String(), "\n") {
		if name, _, ok := strings.Cut(l, " "); ok {
			lines[name] = l
		}
	}
	if l := lines["duel-speedups"]; !strings.Contains(l, "duel:hold ") {
		t.Errorf("duel-speedups line %q, want its default duel:hold, not the ignored \"Hold\"", l)
	}
	if l := lines["radar-claims"]; !strings.Contains(l, "duel:always") {
		t.Errorf("radar-claims line %q, want the configured duel:always", l)
	}
}
