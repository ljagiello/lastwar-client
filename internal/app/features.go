package app

import (
	"fmt"
	"io"

	"lastwar-client/internal/game"
)

// printFeatures implements -list-features: every optional feature with its default, whether the
// session config (cfg, may be nil) turns it on, and its Alliance Duel policy after the config's
// duelPolicy overrides: "hold" runs only on duel days that score it, "always" every run, "-" for
// features that score nothing in the duel.
func printFeatures(w io.Writer, cfg map[string]bool, policy map[string]string) {
	for _, f := range game.Features() {
		state := "off"
		if game.FeatureEnabled(f, cfg) {
			state = "ON"
		}
		def := "off"
		if f.DefaultOn {
			def = "on"
		}
		duel := "-"
		if len(f.Duel) > 0 {
			duel = "always"
			if f.Hold {
				duel = "hold"
			}
			if p, ok := policy[f.Name]; ok {
				duel = p
			}
		}
		_, _ = fmt.Fprintf(w, "%-32s %-3s (default %s) duel:%-6s  %s\n", f.Name, state, def, duel, f.Summary)
	}
}
