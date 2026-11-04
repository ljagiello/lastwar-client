package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"runtime/debug"
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

// The duelPolicy values; anything else is warned about and ignored (see CleanDuelPolicy).
const (
	duelPolicyHold   = "hold"
	duelPolicyAlways = "always"
)

// Run phases. RunFeatures runs the enabled features by (phase, name), so a run claims what its own
// actions earned before the points can expire: an Arms Race stage offers nothing once it has
// ended (ActivityPersonalArmsDataManager.lua:165-184), the Duel page claims only the current
// eventList entry (ActAllianceBattleInfo.lua:29-36), the daily quests start over at the server
// day (DailyTaskManager.lua:30-42), and none of hero_event 100000-120004 has a reissue_mail.
const (
	// phaseCapacity claims free what the actions need: a finished-but-unclaimed Secret Task holds
	// its dispatch slot (ActDispatchTaskDataManager.lua:778-786), and a camp whose output waits to
	// be collected can't train (BuildBubbleManager.lua:2377-2379).
	phaseCapacity = 1
	// phaseAction is every other feature, among them the actions that earn quest, Arms Race,
	// battle-pass and duel points (training, recruits, speed-ups, radar and dispatch tasks).
	phaseAction = 2
	// phaseProgress is the idempotent progress claims. Those that read a server reply (daily-quests,
	// arms-race-chests, battle-pass, event-claims, the opt-in claims) re-read it, so running them
	// last also claims what this run's actions completed. quests and stage-quests read init, so
	// they see this run's completions only on the next run.
	phaseProgress = 3
)

// featurePhases places a feature in a run phase; a feature not listed runs in phaseAction. The
// phaseAction entries are the actions named in MASTER B3 (aud-events F2, aud-dailies F3) and every
// feature with a Duel field; they are listed so the table shows what the progress claims follow.
// The phaseProgress entries are B3's five claims and the opt-in progress claims added with the
// 1.0.367 audit. battle-pass-overflow must follow battle-pass, which changes the pass's exp and
// level; it does, by name within the phase.
var featurePhases = map[string]int{
	"finished-timers":    phaseCapacity,
	"secret-tasks-claim": phaseCapacity,

	"duel-recruit-tickets": phaseAction,
	"duel-speedups":        phaseAction,
	"duel-troop-training":  phaseAction,
	"free-recruits":        phaseAction,
	"radar-claims":         phaseAction,
	"radar-execute":        phaseAction,
	"secret-tasks-start":   phaseAction,
	"treasure-digs":        phaseAction,
	"truck-deploy":         phaseAction,

	"arms-race-chests": phaseProgress,
	"battle-pass":      phaseProgress,
	"daily-quests":     phaseProgress,
	"event-claims":     phaseProgress,
	"quests":           phaseProgress,

	"alliance-premium-gifts": phaseProgress,
	"battle-pass-overflow":   phaseProgress,
	"ghost-recon":            phaseProgress,
	"stage-quests":           phaseProgress,
	"t11-idle-events":        phaseProgress,
	"torch-relay":            phaseProgress,
	"tower-up-first-rewards": phaseProgress,
	"zone-mobilization":      phaseProgress,
}

// featurePhase returns name's run phase.
func featurePhase(name string) int {
	if p, ok := featurePhases[name]; ok {
		return p
	}
	return phaseAction
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

// featuresInRunOrder returns every registered feature, sorted by (phase, name).
func featuresInRunOrder() []Feature {
	out := Features()
	slices.SortStableFunc(out, func(a, b Feature) int { return cmp.Compare(featurePhase(a.Name), featurePhase(b.Name)) })
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

// CleanDuelPolicy returns the session config's duelPolicy without the entries it can't apply,
// each warned about: a name no feature has, and a value other than "hold" or "always" (a typo such
// as "Hold" used to count as "always" and run a held spend on an off-theme day). A dropped value
// leaves that feature's own default. The map passed in is not modified: the session config is
// saved back from it.
func CleanDuelPolicy(policy map[string]string) map[string]string {
	if len(policy) == 0 {
		return policy
	}
	out := make(map[string]string, len(policy))
	for _, name := range slices.Sorted(maps.Keys(policy)) {
		v := policy[name]
		if _, ok := featureRegistry[name]; !ok {
			slog.Warn("session config sets a duelPolicy for an unknown feature; ignoring it", "feature", name, "value", v)
			continue
		}
		if v != duelPolicyHold && v != duelPolicyAlways {
			slog.Warn(`session config's duelPolicy value is neither "hold" nor "always"; ignoring it, so the feature keeps its default`,
				"feature", name, "value", v)
			continue
		}
		out[name] = v
	}
	return out
}

// runFeature runs f and turns a panic into f's error, logged with its stack, so one broken
// feature can't end the run.
func runFeature(f Feature, conn *session.GameConn, in *Init) (err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("feature panicked; continuing without it", "feature", f.Name, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return f.Run(conn, in)
}

// RunFeatures runs every enabled feature in run order, or, when only is non-empty, just that
// feature regardless of whether it is enabled (the -run validation path). The run order is
// (phase, name): first the claims that free capacity, then the actions, then the progress claims,
// which re-read what the actions earned (see featurePhases). Before the first progress claim the
// run's cached activity detail replies are dropped, since they predate the actions. A feature the
// Alliance Duel holds today is skipped with a log line; for -run that is an error unless cfg.Force
// is set, so validating a duel-scoring feature can't spend its points on the wrong day. The
// duelPolicy is cleaned first (CleanDuelPolicy). A panic in a feature becomes that feature's
// error. Like CollectAll it keeps going after a failure and stops early only on a dead connection.
// Every time gate reads conn's server clock (useServerClock).
func RunFeatures(conn *session.GameConn, in *Init, cfg FeatureConfig, only string) error {
	useServerClock(conn)
	policy := CleanDuelPolicy(cfg.DuelPolicy)
	if only != "" {
		f, ok := featureRegistry[only]
		if !ok {
			return fmt.Errorf("unknown feature %q (see -list-features)", only)
		}
		LogDuelStatus(conn, in)
		if held, why := duelHeld(conn, in, f, policy); held {
			if !cfg.Force {
				return fmt.Errorf("feature %q is held for the Alliance Duel: %s (pass -run-anyway to run it regardless)", f.Name, why)
			}
			slog.Warn("running a duel-held feature anyway (-run-anyway)", "feature", f.Name, "reason", why)
		}
		slog.Info("running single feature", "feature", f.Name)
		return runFeature(f, conn, in)
	}
	for k := range cfg.Enabled {
		if _, ok := featureRegistry[k]; !ok {
			slog.Warn("session config enables an unknown feature; ignoring it", "feature", k)
		}
	}
	var errs []error
	ranEarlier := false
	for _, f := range featuresInRunOrder() {
		if !FeatureEnabled(f, cfg.Enabled) {
			continue
		}
		if held, why := duelHeld(conn, in, f, policy); held {
			slog.Info("feature held for the Alliance Duel", "feature", f.Name, "reason", why)
			continue
		}
		if featurePhase(f.Name) == phaseProgress {
			if ranEarlier {
				forgetActivityReplies(conn, in)
				ranEarlier = false
			}
		} else {
			ranEarlier = true
		}
		slog.Info("running feature", "feature", f.Name)
		err := runFeature(f, conn, in)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.Name, err))
		}
		if session.ContainsNonTimeoutNetError(err) {
			break
		}
	}
	return errors.Join(errs...)
}

// forgetActivityReplies drops the detail replies this run's activity sweep has cached, so the
// progress claims read the state after the actions. The duel's hero.event.info.get is cached
// before any feature runs (LogDuelStatus, duelHeld), and arms-race-chests would otherwise claim
// Duel chests against the score from before this run's spends. It costs one more detail request
// per activity a progress claim reads. A dead-connection failure stays cached, so the sweep keeps
// refusing to send on that connection.
func forgetActivityReplies(conn *session.GameConn, in *Init) {
	activitySweepMu.Lock()
	defer activitySweepMu.Unlock()
	s := activitySweepCache
	if s == nil || s.conn != conn || s.in != in {
		return
	}
	n := 0
	for k, r := range s.replies {
		if r.err == nil || !session.ContainsNonTimeoutNetError(r.err) {
			delete(s.replies, k)
			n++
		}
	}
	if n > 0 {
		slog.Debug("dropped cached activity replies before the progress claims", "replies", n)
	}
}

// Collect is the full -collect run: the core actions (CollectAll) followed by the enabled
// features, every time gate reading conn's server clock (useServerClock).
func Collect(conn *session.GameConn, in *Init, cfg FeatureConfig) error {
	useServerClock(conn)
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
