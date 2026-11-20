package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestSeasonMilitaryRankUpWire: in S7 (activity 1200224, lw_season_military_level group 70) a score
// of 100000 at level 2 passes level 3's 30000 and level 4's 60000, not level 5's 112000.
//   - military.act.info (MsgDefines.lua:2950; sent at SeasonMilitaryManager.lua:347), no params
//     (MilitaryActInfoMessage.lua:4-6).
//   - military.up.level (MsgDefines.lua:2952; sent at SeasonMilitaryManager.lua:382)
//     {viewLevel: Int the current level} (MilitaryUpLevelMessage.lua:4-7).
func TestSeasonMilitaryRankUpWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "military.act.info":
			return obj("militaryLevel", 2, "totalMilitaryScore", int64(100000), "dailyRewardNum", 0)
		case "military.up.level":
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "totalMilitaryScore", int64(100000), "dailyRewardNum", 0)
		}
		return evErr("unexpected command")
	})
	if err := runSeasonMilitaryRankUp(conn, evWithSeason(evInit(30, evActivity(1200224)), 7)); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "military.act.info"},
		{cmd: "military.up.level", params: []aWireParam{{"viewLevel", sfs.SFSInt, int32(2)}}},
		{cmd: "military.up.level", params: []aWireParam{{"viewLevel", sfs.SFSInt, int32(3)}}},
	})
}
