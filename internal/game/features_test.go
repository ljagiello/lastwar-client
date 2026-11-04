package game

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// withRegistry swaps in a test registry for the duration of the test.
func withRegistry(t *testing.T, fs ...Feature) *[]string {
	t.Helper()
	saved := featureRegistry
	featureRegistry = map[string]Feature{}
	t.Cleanup(func() { featureRegistry = saved })
	var ran []string
	for _, f := range fs {
		name := f.Name
		inner := f.Run
		f.Run = func(c *session.GameConn, in *Init) error {
			ran = append(ran, name)
			if inner != nil {
				return inner(c, in)
			}
			return nil
		}
		registerFeature(f)
	}
	return &ran
}

func TestFeatureEnabled(t *testing.T) {
	on := Feature{Name: "a", DefaultOn: true}
	off := Feature{Name: "b"}
	cases := []struct {
		f    Feature
		cfg  map[string]bool
		want bool
	}{
		{on, nil, true},
		{off, nil, false},
		{on, map[string]bool{"a": false}, false},
		{off, map[string]bool{"b": true}, true},
		{off, map[string]bool{"other": true}, false},
	}
	for _, c := range cases {
		if got := FeatureEnabled(c.f, c.cfg); got != c.want {
			t.Errorf("FeatureEnabled(%s default=%v, %v) = %v, want %v", c.f.Name, c.f.DefaultOn, c.cfg, got, c.want)
		}
	}
}

func TestRunFeaturesRunsEnabledInNameOrder(t *testing.T) {
	ran := withRegistry(t,
		Feature{Name: "zeta", DefaultOn: true},
		Feature{Name: "alpha", DefaultOn: true},
		Feature{Name: "off-by-default"},
		Feature{Name: "disabled", DefaultOn: true},
	)
	cfg := map[string]bool{"disabled": false, "unknown-feature": true}
	if err := RunFeatures(nil, &Init{}, FeatureConfig{Enabled: cfg}, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*ran, ","); got != "alpha,zeta" {
		t.Errorf("ran %q, want alpha,zeta", got)
	}
}

func TestRunFeaturesOnlyIgnoresEnablement(t *testing.T) {
	ran := withRegistry(t, Feature{Name: "off-by-default"}, Feature{Name: "other", DefaultOn: true})
	if err := RunFeatures(nil, &Init{}, FeatureConfig{}, "off-by-default"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*ran, ","); got != "off-by-default" {
		t.Errorf("ran %q, want only off-by-default", got)
	}
	if err := RunFeatures(nil, &Init{}, FeatureConfig{}, "nope"); err == nil || !strings.Contains(err.Error(), "unknown feature") {
		t.Errorf("RunFeatures(unknown) error = %v, want an unknown-feature error", err)
	}
}

func TestRunFeaturesAggregatesErrors(t *testing.T) {
	boom := errors.New("boom")
	withRegistry(t,
		Feature{Name: "a", DefaultOn: true, Run: func(*session.GameConn, *Init) error { return boom }},
		Feature{Name: "b", DefaultOn: true},
	)
	err := RunFeatures(nil, &Init{}, FeatureConfig{}, "")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "a: boom") {
		t.Errorf("RunFeatures error = %v, want it to wrap boom with the feature name", err)
	}
}

// TestFeaturePhasesNameRegisteredFeatures checks the run-phase table against the real registry: a
// renamed feature would otherwise drop silently back to the action phase.
func TestFeaturePhasesNameRegisteredFeatures(t *testing.T) {
	for name, phase := range featurePhases {
		if _, ok := featureRegistry[name]; !ok {
			t.Errorf("featurePhases names %q, which is not a registered feature", name)
		}
		if phase < phaseCapacity || phase > phaseProgress {
			t.Errorf("featurePhases[%q] = %d, want %d..%d", name, phase, phaseCapacity, phaseProgress)
		}
	}
	for _, name := range []string{"finished-timers", "secret-tasks-claim"} {
		if featurePhase(name) != phaseCapacity {
			t.Errorf("%s must run first: it frees capacity the actions use", name)
		}
	}
	for _, name := range []string{"arms-race-chests", "battle-pass", "daily-quests", "event-claims", "quests",
		"alliance-premium-gifts", "battle-pass-overflow", "ghost-recon", "stage-quests", "t11-idle-events",
		"torch-relay", "tower-up-first-rewards", "zone-mobilization"} {
		if featurePhase(name) != phaseProgress {
			t.Errorf("%s must run last: it claims the progress this run's actions earn", name)
		}
	}
}

// TestBattlePassOverflowRunsAfterBattlePass: the overflow boxes are counted from the pass's exp
// and level, which battle-pass's claims change, so battle-pass-overflow must run after it.
func TestBattlePassOverflowRunsAfterBattlePass(t *testing.T) {
	order := featuresInRunOrder()
	at := func(name string) int {
		return slices.IndexFunc(order, func(f Feature) bool { return f.Name == name })
	}
	bp, over := at("battle-pass"), at("battle-pass-overflow")
	if bp < 0 || over < 0 {
		t.Fatalf("battle-pass at %d, battle-pass-overflow at %d; want both registered", bp, over)
	}
	if bp > over {
		t.Errorf("battle-pass runs at %d, after battle-pass-overflow at %d", bp, over)
	}
}

// TestFeaturePhasesRunDuelScorersBeforeProgressClaims: every feature that earns duel points is an
// action, so it runs before the claims of the points it earns.
func TestFeaturePhasesRunDuelScorersBeforeProgressClaims(t *testing.T) {
	for _, f := range Features() {
		if len(f.Duel) > 0 && featurePhase(f.Name) != phaseAction {
			t.Errorf("%s scores in the duel but runs in phase %d, want the action phase %d", f.Name, featurePhase(f.Name), phaseAction)
		}
	}
}

func TestFeaturesInRunOrderIsDeterministic(t *testing.T) {
	first := featuresInRunOrder()
	for i := 1; i < len(first); i++ {
		a, b := first[i-1], first[i]
		pa, pb := featurePhase(a.Name), featurePhase(b.Name)
		if pa > pb || (pa == pb && a.Name >= b.Name) {
			t.Errorf("run order has %s (phase %d) before %s (phase %d)", a.Name, pa, b.Name, pb)
		}
	}
	for range 20 {
		again := featuresInRunOrder()
		if len(again) != len(first) {
			t.Fatalf("run order length changed: %d, then %d", len(first), len(again))
		}
		for i := range again {
			if again[i].Name != first[i].Name {
				t.Fatalf("run order changed at %d: %s, then %s", i, first[i].Name, again[i].Name)
			}
		}
	}
}

func TestRunFeaturesRunsByPhaseThenName(t *testing.T) {
	ran := withRegistry(t,
		Feature{Name: "quests", DefaultOn: true},
		Feature{Name: "zeta", DefaultOn: true},
		Feature{Name: "secret-tasks-start", DefaultOn: true},
		Feature{Name: "daily-quests", DefaultOn: true},
		Feature{Name: "finished-timers", DefaultOn: true},
		Feature{Name: "alpha", DefaultOn: true},
		Feature{Name: "secret-tasks-claim", DefaultOn: true},
	)
	const want = "finished-timers,secret-tasks-claim,alpha,secret-tasks-start,zeta,daily-quests,quests"
	for range 5 {
		*ran = nil
		if err := RunFeatures(nil, &Init{}, FeatureConfig{}, ""); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(*ran, ","); got != want {
			t.Fatalf("ran %q, want %q", got, want)
		}
	}
	*ran = nil
	if err := RunFeatures(nil, &Init{}, FeatureConfig{}, "quests"); err != nil || strings.Join(*ran, ",") != "quests" {
		t.Errorf("-run quests: err=%v ran=%v, want only quests", err, *ran)
	}
}

// TestRunFeaturesRereadsActivityRepliesBeforeProgressClaims: the duel reply TodayDuel caches before
// the features run carries the score from before this run's spends, so the progress claims must
// fetch it again; a dead-connection failure stays cached.
func TestRunFeaturesRereadsActivityRepliesBeforeProgressClaims(t *testing.T) {
	saved := activitySweepCache
	t.Cleanup(func() { activitySweepCache = saved })
	const duelKey, deadKey = "hero.event.info.get#24", "get.battlepass.info#7"
	dead := fmt.Errorf("activity 7: %w", &net.OpError{Op: "read", Net: "tcp", Err: errors.New("connection reset")})
	in := &Init{Raw: sfs.NewSFSObject()}
	seed := func() {
		s := Activities(nil, in)
		s.replies[duelKey] = activityReply{obj: sfs.NewSFSObject()}
		s.replies[deadKey] = activityReply{err: dead}
	}
	var atAction, atProgress []bool
	cached := func(at *[]bool) func(*session.GameConn, *Init) error {
		return func(*session.GameConn, *Init) error {
			s := Activities(nil, in)
			_, duel := s.replies[duelKey]
			_, deadKept := s.replies[deadKey]
			*at = append(*at, duel, deadKept)
			return nil
		}
	}
	withRegistry(t,
		Feature{Name: "alpha", DefaultOn: true, Run: cached(&atAction)},
		Feature{Name: "arms-race-chests", DefaultOn: true, Run: cached(&atProgress)},
	)
	seed()
	if err := RunFeatures(nil, in, FeatureConfig{}, ""); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(atAction) != "[true true]" || fmt.Sprint(atProgress) != "[false true]" {
		t.Errorf("cached (duel, dead) seen by the action %v and by the progress claim %v, want [true true] and [false true]", atAction, atProgress)
	}

	// With no action run first, the cached replies are still current and are kept.
	atAction, atProgress = nil, nil
	seed()
	if err := RunFeatures(nil, in, FeatureConfig{Enabled: map[string]bool{"alpha": false}}, ""); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(atProgress) != "[true true]" {
		t.Errorf("progress claim with no action before it saw cached (duel, dead) %v, want [true true]", atProgress)
	}
}

func TestRunFeaturesRecoversFromAPanic(t *testing.T) {
	logs := captureActivityLog(t)
	ran := withRegistry(t,
		Feature{Name: "a", DefaultOn: true, Run: func(*session.GameConn, *Init) error {
			var o *sfs.SFSObject
			o.PutInt("x", 1) // a write to a nil object panics
			return nil
		}},
		Feature{Name: "b", DefaultOn: true},
	)
	err := RunFeatures(nil, &Init{}, FeatureConfig{}, "")
	if err == nil || !strings.Contains(err.Error(), "a: panic:") {
		t.Errorf("RunFeatures error = %v, want a's panic as its error", err)
	}
	if got := strings.Join(*ran, ","); got != "a,b" {
		t.Errorf("ran %q, want a,b: the panic must not end the run", got)
	}
	out := logs.String()
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "feature panicked") || !strings.Contains(out, "feature=a") ||
		!strings.Contains(out, "features_test.go") {
		t.Errorf("log = %s, want an Error naming feature a with its stack", out)
	}

	*ran = nil
	if err := RunFeatures(nil, &Init{}, FeatureConfig{}, "a"); err == nil || !strings.Contains(err.Error(), "panic:") {
		t.Errorf("-run a error = %v, want the panic as an error", err)
	}
}

func TestRunFeaturesCleansDuelPolicy(t *testing.T) {
	logs := captureActivityLog(t)
	ran := withRegistry(t,
		Feature{Name: "spend", DefaultOn: true, Duel: []DuelScore{{Type: DuelScoreSpeedUp}}, Hold: true},
		Feature{Name: "claim", DefaultOn: true},
	)
	in := &Init{} // no init, so no duel day: a held feature stays held
	run := func(policy map[string]string, only string) (string, error) {
		*ran = nil
		err := RunFeatures(nil, in, FeatureConfig{DuelPolicy: policy}, only)
		return strings.Join(*ran, ","), err
	}

	typo := map[string]string{"spend": "Hold", "spennd": "always"}
	if got, err := run(typo, ""); err != nil || got != "claim" {
		t.Errorf(`policy "Hold": ran %q (err %v), want claim only: a bad value must leave the feature's own hold`, got, err)
	}
	if typo["spend"] != "Hold" || typo["spennd"] != "always" {
		t.Errorf("RunFeatures modified the session config's duelPolicy: %v", typo)
	}
	out := logs.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "feature=spennd") || !strings.Contains(out, "value=Hold") {
		t.Errorf("log = %s, want a Warn for the unknown name and one for the bad value", out)
	}
	if _, err := run(typo, "spend"); err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf(`-run spend with policy "Hold": err = %v, want it held`, err)
	}
	if got, err := run(map[string]string{"spend": "always"}, ""); err != nil || got != "claim,spend" {
		t.Errorf(`policy "always": ran %q (err %v), want claim,spend`, got, err)
	}
	got := CleanDuelPolicy(map[string]string{"spend": "hold", "claim": "always", "x": "hold", "spend2": ""})
	if fmt.Sprint(got) != "map[claim:always spend:hold]" {
		t.Errorf("CleanDuelPolicy = %v, want map[claim:always spend:hold]", got)
	}
}

func TestRegisterFeatureRejectsDuplicates(t *testing.T) {
	withRegistry(t, Feature{Name: "dup"})
	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate feature did not panic")
		}
	}()
	registerFeature(Feature{Name: "dup", Run: func(*session.GameConn, *Init) error { return nil }})
}

func TestInitAccessorsAndServerDay(t *testing.T) {
	raw := sfs.NewSFSObject()
	raw.PutLong("tomorrow", 1791252000) // 2026-10-06 02:00 UTC, from a live init
	vip := sfs.NewSFSObject()
	vip.PutInt("loginScoreState", 1)
	raw.PutSFSObject("vip", vip)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(sfs.NewSFSObject())
	arr.AddInt(7)
	raw.PutSFSArray("collect_reward", arr)
	in := &Init{Raw: raw}

	if in.Object("vip").GetInt("loginScoreState") != 1 || in.Object("missing") != nil {
		t.Error("Object accessor")
	}
	if len(in.Array("collect_reward")) != 2 || len(in.Objects("collect_reward")) != 1 {
		t.Error("Array/Objects accessors")
	}
	var nilInit *Init
	if nilInit.Object("vip") != nil || nilInit.Array("x") != nil {
		t.Error("nil Init accessors must return nil")
	}

	tomorrow := time.Unix(1791252000, 0)
	for _, c := range []struct {
		now  time.Time
		want time.Time
	}{
		{tomorrow.Add(-time.Hour), tomorrow.Add(-24 * time.Hour)}, // same server day as the init
		{tomorrow.Add(time.Minute), tomorrow},                     // init went stale across the reset
		{tomorrow.Add(-49 * time.Hour), tomorrow.Add(-72 * time.Hour)},
	} {
		got, ok := in.ServerDayStart(c.now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("ServerDayStart(%v) = %v, %v; want %v", c.now.UTC(), got.UTC(), ok, c.want.UTC())
		}
	}
	if _, ok := (&Init{Raw: sfs.NewSFSObject()}).ServerDayStart(time.Now()); ok {
		t.Error("ServerDayStart without tomorrow must report !ok")
	}
}
