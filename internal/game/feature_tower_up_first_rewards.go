package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Tower-up and Overlord (dominator-up) first-clear chests (gap TOWERUP, MASTER.md §3.2 #8),
// OPT-IN, claims only.
//
// Init carries towerUpStageInfo{stageId, firstRewardedArr} and dominatorUpStageInfo{dominatorId,
// firstRewardedArr} (LWTowerUpStageManager:InitData and LWDominatorUpStageManager:InitData,
// called from InitMessage.lua:308-313; the stage is tonumber'd, 0 when absent). The first-reward
// stages are the rows of lw_towerup (lw_towerup_B is never selected in Lua 833; its set is the
// same anyway) and lw_dominatorup with a non-empty first_reward (TowerUpTemplateManager and
// DominatorUpTemplateManager:GetFirstRewardStage). A stage at or below the current one that is
// not in firstRewardedArr is CanClaim (GetStageFirstRewardType; a missing firstRewardedArr leaves
// the dict nil, so every reached stage is), and any CanClaim stage shows the claim-all button,
// which sends {id: Int -1} (UIJeepAdventureFirstRewardsView.lua:93-95, 97-127;
// LWReveiveTowerUpFirstRewardMessage.lua:5-8 and LWReveiveDominatorUpFirstRewardMessage.lua:5-8,
// PutInt("id", id or -1)). The panel with that button shows only with the server-only switch
// truck_first_reward on (UIJeepAdventureMainView.lua:271; CheckSwitch is false for an absent key,
// DataConfig.lua:76-88, and table item 39879 has no such row), so an absent switch holds the
// claims back too.
//
// Whether -1 pays every pending chest server-side is open (MASTER.md L21), so each reply's
// firstRewardedArr and reward are logged. Static-only, not validated live.
const (
	towerUpFirstRewardsFeature = "tower-up-first-rewards"
	towerUpFirstRewardCmd      = "lw.receive.towerup.first.reward"
	dominatorUpFirstRewardCmd  = "lw.receive.dominator.up.first.reward"
	towerUpFirstRewardSwitch   = "truck_first_reward"
	jeepFirstRewardAll         = -1 // the claim-all id
	jeepFirstRewardStep        = 5  // tables 39879: first_reward on every 5th stage, from stage 5
)

// jeepFirstRewardTracks are the two Jeep Adventure pages. last is the highest first-reward stage
// in tables 39879: lw_towerup (and lw_towerup_B) 5..495, 99 chests; lw_dominatorup 5..400, 80.
var jeepFirstRewardTracks = []struct {
	name, infoKey, stageKey, cmd string
	last                         int64
}{
	{"tower-up", "towerUpStageInfo", "stageId", towerUpFirstRewardCmd, 495},
	{"dominator-up", "dominatorUpStageInfo", "dominatorId", dominatorUpFirstRewardCmd, 400},
}

func init() {
	registerFeature(Feature{
		Name:      towerUpFirstRewardsFeature,
		Summary:   "OPT-IN: claim the tower-up and Overlord first-clear chests of reached stages with the client's claim-all (static-only, not validated live)",
		DefaultOn: false,
		Run:       runTowerUpFirstRewards,
	})
}

func runTowerUpFirstRewards(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if !evSwitchOn(in, towerUpFirstRewardSwitch) {
		v, present := in.Object("dataConfig").Get(towerUpFirstRewardSwitch)
		slog.Info(towerUpFirstRewardsFeature+": switch "+towerUpFirstRewardSwitch+" is not on; the client hides the claim",
			"present", present, "value", v.Val)
		return nil
	}
	b := &evBatch{conn: conn}
	for _, t := range jeepFirstRewardTracks {
		if b.dead {
			break
		}
		info := in.Object(t.infoKey)
		if info == nil {
			continue
		}
		stage, _ := evNum(info, t.stageKey)
		claimed := evNums(info, "firstRewardedArr")
		due := jeepFirstRewardsDue(stage, claimed, t.last)
		slog.Info(towerUpFirstRewardsFeature+": "+t.name, "stage", stage, "claimed", len(claimed), "due", len(due), "dueStages", due)
		if len(due) == 0 {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("id", jeepFirstRewardAll)
		r := b.send(towerUpFirstRewardsFeature+": "+t.name+" chests", t.cmd, p)
		if r == nil {
			continue // logged by SendAndWait and kept in b
		}
		attrs := []any{"due", len(due), "granted", describeGrants(r.Params)}
		if _, ok := r.Params.Get("firstRewardedArr"); ok {
			after := evNums(r.Params, "firstRewardedArr")
			attrs = append(attrs, "replyFirstRewardedArr", len(after), "stillDue", jeepFirstRewardsDue(stage, after, t.last))
		} else {
			attrs = append(attrs, "replyFirstRewardedArr", "absent")
		}
		slog.Info(towerUpFirstRewardsFeature+": "+t.name+" chests claimed", attrs...)
	}
	return b.err()
}

// jeepFirstRewardsDue lists the first-reward stages (every jeepFirstRewardStep up to last) at or
// below stage that claimed lacks: GetStageFirstRewardType's CanClaim.
func jeepFirstRewardsDue(stage int64, claimed []int64, last int64) []int64 {
	got := map[int64]bool{}
	for _, id := range claimed {
		got[id] = true
	}
	var due []int64
	for id := int64(jeepFirstRewardStep); id <= min(stage, last); id += jeepFirstRewardStep {
		if !got[id] {
			due = append(due, id)
		}
	}
	return due
}
