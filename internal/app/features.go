package app

import (
	"fmt"
	"io"

	"lastwar-client/internal/game"
)

// printFeatures implements -list-features: every optional feature with its default and whether
// the session config (cfg, may be nil) turns it on.
func printFeatures(w io.Writer, cfg map[string]bool) {
	for _, f := range game.Features() {
		state := "off"
		if game.FeatureEnabled(f, cfg) {
			state = "ON"
		}
		def := "off"
		if f.DefaultOn {
			def = "on"
		}
		_, _ = fmt.Fprintf(w, "%-32s %-3s (default %s)  %s\n", f.Name, state, def, f.Summary)
	}
}
