package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// seasonMilitaryRankReplies answers military.act.info with level and score, and each rank-up with
// the next level and the same score.
func seasonMilitaryRankReplies(level int32, score int64) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case seasonMilitaryInfoCmd:
			return obj("militaryLevel", level, "totalMilitaryScore", score, "dailyRewardNum", 0)
		case seasonMilitaryUpCmd:
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "totalMilitaryScore", score, "dailyRewardNum", 0)
		}
		return evOK()
	}
}

// TestSeasonMilitaryRankUp: S7 group 70 thresholds 60000 (level 4) and 112000 (level 5): a score
// of 100000 at level 2 ranks up twice (2 -> 3 -> 4), each time sending the current level.
func TestSeasonMilitaryRankUp(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonMilitaryRankReplies(2, 100000))
	if err := runSeasonMilitaryRankUp(conn, evWithSeason(evInit(30, evActivity(1200224)), 7)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "military.act.info,military.up.level,military.up.level" {
		t.Fatalf("sent %s", got)
	}
	ups := fake.only(seasonMilitaryUpCmd)
	seasonParamIs(t, ups[0].Params, "viewLevel", sfs.SFSInt, int32(2))
	seasonParamIs(t, ups[1].Params, "viewLevel", sfs.SFSInt, int32(3))
}

func TestSeasonMilitaryRankUpStops(t *testing.T) {
	withEvNow(t, evTestNow)
	settling := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000), "isDailySettleIng", 1)
	}
	// scoreDrops answers each rank-up with the next level and a lower score that still passes every
	// group-60 manual threshold (level 15's unlock_score 667000), so only the drop check stops the
	// second rank-up.
	scoreDrops := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "totalMilitaryScore", int64(800000))
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	// noLevelUp answers an info at level 1 and each rank-up without militaryLevel: the client's
	// GetLevel is Mathf.Max(1, checknumber(Level)) (SeasonMilitaryInfoData.lua:56, 78-79), so the
	// level reads as 1 again and no second rank-up follows.
	noLevelUp := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return obj("totalMilitaryScore", int64(900000), "dailyRewardNum", 0)
		}
		return obj("militaryLevel", 1, "totalMilitaryScore", int64(900000))
	}
	sameLevel := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	// noScore answers each rank-up without totalMilitaryScore. The client's HandleLevelUp then reads
	// 0 (checknumber, SeasonMilitaryInfoData.lua:53-64; FuncUtil.lua:1-3) and offers no further
	// rank-up, so only one military.up.level goes out although 900000 would pass every threshold.
	noScore := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "dailyRewardNum", 0)
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	refused := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return evErr("E9")
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	s6 := evWithSeason(evInit(30, evActivity(1200101)), 6)
	ended := evActivity(1200101)
	ended.PutLong("startTime", evTestNow.Add(-50*24*time.Hour).UnixMilli())
	for name, c := range map[string]struct {
		reply   func(string, *sfs.SFSObject) *sfs.SFSObject
		in      *Init
		want    string
		wantErr bool
	}{
		"below the next threshold":     {seasonMilitaryRankReplies(3, 28999), s6, "military.act.info", false},
		"at the highest manual level":  {seasonMilitaryRankReplies(15, 9000000), s6, "military.act.info", false},
		"daily settlement in progress": {settling, s6, "military.act.info", false},
		"score drops after a rank-up":  {scoreDrops, s6, "military.act.info,military.up.level", false},
		"level does not rise":          {sameLevel, s6, "military.act.info,military.up.level", false},
		"reply without militaryLevel":  {noLevelUp, s6, "military.act.info,military.up.level", false},
		"no military activity open":    {seasonMilitaryRankReplies(2, 900000), evWithSeason(evInit(30), 6), "", false},
		"rank-up reply without score":  {noScore, s6, "military.act.info,military.up.level", false},
		"refused":                      {refused, s6, "military.act.info,military.up.level", true},
		"end-show phase":               {seasonMilitaryRankReplies(2, 900000), evWithSeason(evInit(30, ended), 6), "", false},
		"out of season":                {seasonMilitaryRankReplies(2, 900000), evInit(30, evActivity(1200101)), "", false},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			err := runSeasonMilitaryRankUp(conn, c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

func TestSeasonMilitaryCanRankUp(t *testing.T) {
	levels := []seasonMilitaryLevelRow{
		{level: 1}, {level: 2, unlockScore: 10}, {level: 3, unlockScore: 20, conditionType: 7, conditionPara: 5},
		{level: 4, unlockScore: 30, standardLevel: 1}, {level: 5, needRank: 1},
	}
	for _, c := range []struct {
		level, score int64
		progress     map[int64]int64
		want         bool
	}{
		{1, 10, nil, true},
		{1, 9, nil, false},
		{2, 20, map[int64]int64{7: 4}, false}, // the unlock condition is short
		{2, 20, map[int64]int64{7: 5}, true},
		{3, 99, nil, true},
		{4, 99, nil, false}, // standard_level: the last manual rank
		{5, 99, nil, false}, // no next row
	} {
		if got := seasonMilitaryCanRankUp(levels, c.level, c.score, c.progress); got != c.want {
			t.Errorf("level %d score %d progress %v: got %v", c.level, c.score, c.progress, got)
		}
	}
	if seasonMilitaryCanRankUp([]seasonMilitaryLevelRow{{level: 1}, {level: 2}}, 1, 99, nil) {
		t.Error("a group without a standard_level row has no manual rank-up")
	}
	// In the 40413 groups the row after the standard_level one is also the first need_rank row, so
	// each of the two tests hides the other. These rows split them: CanManualUpgrade wants
	// level < GetMaxManualLevel(), and ShowUpgradeRed refuses a next row whose need_rank is not 0
	// (SeasonMilitaryManager.lua:86-97, 337-340).
	pastCap := []seasonMilitaryLevelRow{{level: 1}, {level: 2, unlockScore: 10}, {level: 3, unlockScore: 20, standardLevel: 1},
		{level: 4, unlockScore: 30}}
	if seasonMilitaryCanRankUp(pastCap, 3, 99, nil) {
		t.Error("level 3 is the standard_level row: no manual rank-up past it, even into a need_rank 0 row")
	}
	if !seasonMilitaryCanRankUp(pastCap, 2, 99, nil) {
		t.Error("level 2 below the standard_level row 3 ranks up")
	}
	rankOnly := []seasonMilitaryLevelRow{{level: 1}, {level: 2, unlockScore: 10, needRank: 1}, {level: 3, unlockScore: 20, standardLevel: 1}}
	if seasonMilitaryCanRankUp(rankOnly, 1, 99, nil) {
		t.Error("the next row has need_rank 1: only the rank grants it, below the manual cap too")
	}
	// An unlock_condition_type of 1 is a condition: only 0 < taskType tests it (:103-108).
	cond1 := []seasonMilitaryLevelRow{{level: 1}, {level: 2, unlockScore: 10, conditionType: 1, conditionPara: 5},
		{level: 3, unlockScore: 20, standardLevel: 1}}
	if seasonMilitaryCanRankUp(cond1, 1, 99, map[int64]int64{1: 4}) || !seasonMilitaryCanRankUp(cond1, 1, 99, map[int64]int64{1: 5}) {
		t.Error("condition type 1 needs its progress of 5")
	}
}

// TestSeasonMilitaryRankUpFromAMissingLevel: an info reply without militaryLevel is level 1 to the
// client (GetLevel = Mathf.Max(1, checknumber(Level)), SeasonMilitaryInfoData.lua:78-79), so the
// first rank-up sends viewLevel 1. Group 60 (activity 1200101): 20000 passes level 2's 12000 and
// not level 3's 29000.
func TestSeasonMilitaryRankUpFromAMissingLevel(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "military.up.level" {
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "totalMilitaryScore", int64(20000))
		}
		return obj("totalMilitaryScore", int64(20000))
	})
	if err := runSeasonMilitaryRankUp(conn, evWithSeason(evInit(30, evActivity(1200101)), 6)); err != nil {
		t.Fatal(err)
	}
	ups := fake.only("military.up.level")
	if len(ups) != 1 {
		t.Fatalf("sent %v, want one rank-up", fake.cmds())
	}
	seasonParamIs(t, ups[0].Params, "viewLevel", sfs.SFSInt, int32(1))
}
