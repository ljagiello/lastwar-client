package game

import (
	"errors"
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
	if err := RunFeatures(nil, &Init{}, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*ran, ","); got != "alpha,zeta" {
		t.Errorf("ran %q, want alpha,zeta", got)
	}
}

func TestRunFeaturesOnlyIgnoresEnablement(t *testing.T) {
	ran := withRegistry(t, Feature{Name: "off-by-default"}, Feature{Name: "other", DefaultOn: true})
	if err := RunFeatures(nil, &Init{}, nil, "off-by-default"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*ran, ","); got != "off-by-default" {
		t.Errorf("ran %q, want only off-by-default", got)
	}
	if err := RunFeatures(nil, &Init{}, nil, "nope"); err == nil || !strings.Contains(err.Error(), "unknown feature") {
		t.Errorf("RunFeatures(unknown) error = %v, want an unknown-feature error", err)
	}
}

func TestRunFeaturesAggregatesErrors(t *testing.T) {
	boom := errors.New("boom")
	withRegistry(t,
		Feature{Name: "a", DefaultOn: true, Run: func(*session.GameConn, *Init) error { return boom }},
		Feature{Name: "b", DefaultOn: true},
	)
	err := RunFeatures(nil, &Init{}, nil, "")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "a: boom") {
		t.Errorf("RunFeatures error = %v, want it to wrap boom with the feature name", err)
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
