package game

import (
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"

	"lastwar-client/internal/session"
)

// A Feature is one optional automation run after the core collection (CollectAll). Each lives in
// its own feature_*.go file and registers itself from init(), so adding one never touches shared
// code. Features are claim-only by policy: nothing that spends resources or diamonds, starts a
// march/battle/dispatch, or is visible to other players runs by default (docs/AGENTS.md and
// MASTER.md §6). Run must decide eligibility from in itself and return nil when there is nothing
// to do.
type Feature struct {
	// Name is the stable key used in the session config's "features" map and by -run.
	Name string
	// Summary is a one-line description for -list-features.
	Summary string
	// DefaultOn is true only once the feature's commands have been validated live (see -run);
	// everything else must be enabled explicitly in the session config.
	DefaultOn bool
	// Duel lists the Alliance Duel score types this feature's actions earn points for (see
	// duel.go); empty for features that score nothing.
	Duel []DuelScore
	// Hold, for a feature that scores in the duel, holds it (skips its run) on days whose duel
	// scores none of Duel, so the work is done on a day it counts. The session config's
	// duelPolicy map can override it per feature.
	Hold bool
	Run  func(conn *session.GameConn, in *Init) error
}

// FeatureConfig is the session config's feature settings.
type FeatureConfig struct {
	Enabled    map[string]bool   // "features": on/off per feature name
	DuelPolicy map[string]string // "duelPolicy": "hold" or "always" per feature name
	Force      bool              // -run-anyway: run a -run feature even when the duel holds it today
}

var featureRegistry = map[string]Feature{}

// registerFeature adds f to the registry; call it from a feature file's init().
func registerFeature(f Feature) {
	if f.Name == "" || f.Run == nil {
		panic("registerFeature: feature needs a Name and a Run func")
	}
	if _, dup := featureRegistry[f.Name]; dup {
		panic("registerFeature: duplicate feature " + f.Name)
	}
	featureRegistry[f.Name] = f
}

// Features returns every registered feature, sorted by name.
func Features() []Feature {
	out := make([]Feature, 0, len(featureRegistry))
	for _, name := range slices.Sorted(maps.Keys(featureRegistry)) {
		out = append(out, featureRegistry[name])
	}
	return out
}

// FeatureEnabled reports whether f runs under cfg, the session config's "features" map: an
// explicit entry wins, otherwise f.DefaultOn.
func FeatureEnabled(f Feature, cfg map[string]bool) bool {
	if on, ok := cfg[f.Name]; ok {
		return on
	}
	return f.DefaultOn
}

// RunFeatures runs every enabled feature in name order, or, when only is non-empty, just that
// feature regardless of whether it is enabled (the -run validation path). A feature the Alliance
// Duel holds today is skipped with a log line; for -run that is an error unless cfg.Force is set,
// so validating a duel-scoring feature can't spend its points on the wrong day. Like CollectAll it
// keeps going after a failure and stops early only on a dead connection.
func RunFeatures(conn *session.GameConn, in *Init, cfg FeatureConfig, only string) error {
	if only != "" {
		f, ok := featureRegistry[only]
		if !ok {
			return fmt.Errorf("unknown feature %q (see -list-features)", only)
		}
		LogDuelStatus(conn, in)
		if held, why := duelHeld(conn, in, f, cfg.DuelPolicy); held {
			if !cfg.Force {
				return fmt.Errorf("feature %q is held for the Alliance Duel: %s (pass -run-anyway to run it regardless)", f.Name, why)
			}
			slog.Warn("running a duel-held feature anyway (-run-anyway)", "feature", f.Name, "reason", why)
		}
		slog.Info("running single feature", "feature", f.Name)
		return f.Run(conn, in)
	}
	for k := range cfg.Enabled {
		if _, ok := featureRegistry[k]; !ok {
			slog.Warn("session config enables an unknown feature; ignoring it", "feature", k)
		}
	}
	var errs []error
	for _, f := range Features() {
		if !FeatureEnabled(f, cfg.Enabled) {
			continue
		}
		if held, why := duelHeld(conn, in, f, cfg.DuelPolicy); held {
			slog.Info("feature held for the Alliance Duel", "feature", f.Name, "reason", why)
			continue
		}
		slog.Info("running feature", "feature", f.Name)
		err := f.Run(conn, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
		}
		if session.ContainsNonTimeoutNetError(err) {
			break
		}
	}
	return errors.Join(errs...)
}

// Collect is the full -collect run: the core actions (CollectAll) followed by the enabled
// features.
func Collect(conn *session.GameConn, in *Init, cfg FeatureConfig) error {
	err := collectCore(conn, in)
	if session.ContainsNonTimeoutNetError(err) {
		return err
	}
	if anyFeatureEnabled(cfg.Enabled) {
		LogDuelStatus(conn, in)
	}
	return errors.Join(err, RunFeatures(conn, in, cfg, ""))
}

func anyFeatureEnabled(cfg map[string]bool) bool {
	for _, f := range featureRegistry {
		if FeatureEnabled(f, cfg) {
			return true
		}
	}
	return false
}
