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
	scoreDrops := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return obj("militaryLevel", 3, "totalMilitaryScore", int64(10))
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	sameLevel := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
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
}
