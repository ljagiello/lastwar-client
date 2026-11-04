package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// jeepInit is an Init with the truck_first_reward switch (when on) and the two stage infos as the
// init push carries them (LWTowerUpStageManager/LWDominatorUpStageManager:InitData). A nil info
// is left out.
func jeepInit(on bool, tower, dominator *sfs.SFSObject) *Init {
	in := evAllianceInit()
	if on {
		evObject(in.Raw, "dataConfig").PutUtfString(towerUpFirstRewardSwitch, "1")
	}
	if tower != nil {
		in.Raw.PutSFSObject("towerUpStageInfo", tower)
	}
	if dominator != nil {
		in.Raw.PutSFSObject("dominatorUpStageInfo", dominator)
	}
	return in
}

// stageInfo is a stage info object: stageKey (stageId or dominatorId) and firstRewardedArr as an
// IntArray, or as an SFSArray of Ints when sfsArray is set; claimed nil leaves the array out.
func stageInfo(stageKey string, stage int32, claimed []int32, sfsArray bool) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutInt(stageKey, stage)
	switch {
	case claimed == nil:
	case sfsArray:
		a := sfs.NewSFSArray()
		for _, id := range claimed {
			a.AddValue(sfs.SFSValue{Type: sfs.SFSInt, Val: id})
		}
		o.PutSFSArray("firstRewardedArr", a)
	default:
		o.PutIntArray("firstRewardedArr", claimed)
	}
	return o
}

func runJeepFake(t *testing.T, in *Init, reply func(cmd string) *sfs.SFSObject) (*evFake, error) {
	t.Helper()
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if reply != nil {
			if r := reply(cmd); r != nil {
				return r
			}
		}
		r := evOK()
		r.PutIntArray("firstRewardedArr", []int32{5, 10, 15, 20})
		return r
	})
	err := runTowerUpFirstRewards(conn, in)
	for _, r := range fake.requests() {
		if v, _ := r.Params.Get("id"); v.Type != sfs.SFSInt || r.Params.GetInt("id") != jeepFirstRewardAll {
			t.Errorf("%s id = %#v, want Int -1 (the claim-all)", r.Cmd, v)
		}
	}
	return fake, err
}

func TestTowerUpFirstRewardsClaimsReachedUnclaimedStages(t *testing.T) {
	in := jeepInit(true,
		stageInfo("stageId", 23, []int32{5, 10}, false),    // 15 and 20 are due
		stageInfo("dominatorId", 12, []int32{5, 10}, true), // nothing due
	)
	fake, err := runJeepFake(t, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{towerUpFirstRewardCmd}) {
		t.Errorf("sent %v, want only the tower-up claim", got)
	}
}

func TestTowerUpFirstRewardsBothTracksAndStringStage(t *testing.T) {
	tower := stageInfo("stageId", 0, []int32{5}, false)
	tower.PutUtfString("stageId", "10")                                  // the client tonumber()s it
	in := jeepInit(true, tower, stageInfo("dominatorId", 7, nil, false)) // no firstRewardedArr: 5 is due
	fake, err := runJeepFake(t, in, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{towerUpFirstRewardCmd, dominatorUpFirstRewardCmd}) {
		t.Errorf("sent %v, want both claims", got)
	}
}

func TestTowerUpFirstRewardsGates(t *testing.T) {
	due := stageInfo("stageId", 50, nil, false)
	for _, c := range []struct {
		name string
		in   *Init
	}{
		{"switch off", func() *Init {
			in := jeepInit(false, due, nil)
			evObject(in.Raw, "dataConfig").PutUtfString(towerUpFirstRewardSwitch, "0")
			return in
		}()},
		{"switch absent", jeepInit(false, due, nil)},
		{"no stage infos", jeepInit(true, nil, nil)},
		{"every reached chest claimed", jeepInit(true, stageInfo("stageId", 14, []int32{5, 10}, false), nil)},
		{"below the first chest", jeepInit(true, stageInfo("stageId", 4, []int32{}, false), stageInfo("dominatorId", 0, nil, false))},
	} {
		t.Run(c.name, func(t *testing.T) {
			fake, err := runJeepFake(t, c.in, nil)
			if err != nil || len(fake.cmds()) != 0 {
				t.Errorf("err %v, sent %v; want nothing", err, fake.cmds())
			}
		})
	}
}

func TestTowerUpFirstRewardsErrorKeepsTheOtherTrack(t *testing.T) {
	in := jeepInit(true, stageInfo("stageId", 5, nil, false), stageInfo("dominatorId", 5, nil, false))
	fake, err := runJeepFake(t, in, func(cmd string) *sfs.SFSObject {
		if cmd == towerUpFirstRewardCmd {
			return evErr(evAlreadyExecuted)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s reply reported (no benign codes for this feature)", err, evAlreadyExecuted)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{towerUpFirstRewardCmd, dominatorUpFirstRewardCmd}) {
		t.Errorf("sent %v, want both claims once", got)
	}
}

func TestJeepFirstRewardsDue(t *testing.T) {
	all := func(last int64) []int64 {
		var out []int64
		for id := int64(5); id <= last; id += 5 {
			out = append(out, id)
		}
		return out
	}
	for _, c := range []struct {
		name    string
		stage   int64
		claimed []int64
		last    int64
		want    []int64
	}{
		{"none reached", 4, nil, 495, nil},
		{"first reached", 5, nil, 495, []int64{5}},
		{"between chests", 23, []int64{5, 10}, 495, []int64{15, 20}},
		{"claimed out of order", 20, []int64{20, 5}, 495, []int64{10, 15}},
		{"tower-up backlog", 495, nil, 495, all(495)},
		{"past the last tower-up chest", 600, all(495), 495, nil},
		{"dominator capped at 400", 999, nil, 400, all(400)},
	} {
		if got := jeepFirstRewardsDue(c.stage, c.claimed, c.last); !slices.Equal(got, c.want) {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	if n := len(all(495)) + len(all(400)); n != 179 {
		t.Errorf("tables 39879 hold 179 first-clear chests, the ranges give %d", n)
	}
}

func TestTowerUpFirstRewardsIsOptIn(t *testing.T) {
	f, ok := featureRegistry[towerUpFirstRewardsFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, DefaultOn false, no duel scores, static-only summary", f)
	}
}
