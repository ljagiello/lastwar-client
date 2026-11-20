package game

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// heroCountdownInit adds hero-countdown buildings (bId 10222000) with the given fixCityHeroET
// values (ms; a negative value leaves the field out) to an HQ 25 init.
func heroCountdownInit(ets map[int64]int64) *Init {
	return rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		v, _ := raw.Get("building_new")
		builds := v.Val.(*sfs.SFSArray)
		for _, uuid := range slices.Sorted(maps.Keys(ets)) {
			o := NewTestBuildingSFS(uuid, heroCountdownBuilding, 1)
			if et := ets[uuid]; et >= 0 {
				o.PutLong("fixCityHeroET", et)
			}
			builds.AddSFSObject(o)
		}
		other := NewTestBuildingSFS(77, BuildingFarmland, 5) // another building with the field: ignored
		other.PutLong("fixCityHeroET", 1)
		builds.AddSFSObject(other)
	})
}

func TestHeroCountdownClaimsOnlyEndedCountdowns(t *testing.T) {
	now := time.Now()
	in := heroCountdownInit(map[int64]int64{
		11: now.Add(-time.Minute).UnixMilli(), // ended: claim
		12: now.Add(time.Hour).UnixMilli(),    // running
		13: 0,                                 // cleared
		14: -1,                                // no field
	})
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroCountdown(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := fake.requests()
	if len(reqs) != 1 || reqs[0].Cmd != heroCountdownCmd {
		t.Fatalf("sent %v, want one %s", fake.cmds(), heroCountdownCmd)
	}
	v, ok := reqs[0].Params.Get("uuid")
	if !ok || v.Type != sfs.SFSLong || v.Val.(int64) != 11 {
		t.Errorf("params %s, want uuid Long 11", reqs[0].Params.StringRedacted())
	}
}

// TestHeroCountdownNeedsABuildingUUID: the claim sends the building's uuid
// (BuildHeroCountdownManager.lua:90-98); an ended countdown on a building listed with uuid 0 has none
// to send.
func TestHeroCountdownNeedsABuildingUUID(t *testing.T) {
	past := time.Now().Add(-time.Minute).UnixMilli()
	in := heroCountdownInit(map[int64]int64{0: past, 11: past})
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroCountdown(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	var uuids []int64
	for _, r := range fake.requests() {
		uuids = append(uuids, r.Params.GetLong("uuid"))
	}
	if !slices.Equal(uuids, []int64{11}) {
		t.Errorf("claimed %v, want [11]", uuids)
	}
}

func TestHeroCountdownStopsOnAnError(t *testing.T) {
	past := time.Now().Add(-time.Minute).UnixMilli()
	in := heroCountdownInit(map[int64]int64{11: past, 12: past})
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardErr("E000001") })
	if err := runHeroCountdown(conn, in); err == nil {
		t.Error("run = nil, want the errorCode (no benign code is registered for a never-sent command)")
	}
	if got := fake.cmds(); len(got) != 1 {
		t.Errorf("sent %v, want the claim to stop after the first error", got)
	}
}

func TestHeroCountdownNothingDue(t *testing.T) {
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runHeroCountdown(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v", err, fake.cmds())
	}
	in := heroCountdownInit(map[int64]int64{12: time.Now().Add(time.Hour).UnixMilli()})
	if err := runHeroCountdown(conn, in); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("countdown running: err %v, sent %v", err, fake.cmds())
	}
}

func TestHeroCountdownIsOptIn(t *testing.T) {
	f, ok := featureRegistry[heroCountdownFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %q = %+v (registered %v), want opt-in, no duel scores, static-only summary", heroCountdownFeature, f, ok)
	}
}
