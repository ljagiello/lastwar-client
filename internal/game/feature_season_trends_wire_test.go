package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestSeasonTrendsWire: S6 (season start 20 days ago) answers lw_season_trends group 60 with trends
// 480 (unlock_time 1, para2 1000) at 1000 and 484 (unlock_time 3, para2 10) at 10, both due.
//   - lw.season.trend.info (MsgDefines.lua:1228; sent at SeasonDataManager.lua:248), no params
//     (LwSeasonTrendInfoMessage.lua:4-6).
//   - lw.season.trend.reward (MsgDefines.lua:1231; sent at LWSeasonTrendsItem.lua:380)
//     {trend_id: Int} (LWSeasonTrendRewardMessage.lua:4-7).
func TestSeasonTrendsWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "lw.season.trend.info":
			return seasonTrendInfo(60, [3]int32{480, 1000, 0}, [3]int32{484, 10, 0})
		case "lw.season.trend.reward":
			return obj("trend_id", p.GetInt("trend_id"), "rewards", []*sfs.SFSObject{})
		}
		return evErr("unexpected command")
	})
	if err := runSeasonTrends(conn, evWithSeason(evInit(30), 6)); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "lw.season.trend.info"},
		{cmd: "lw.season.trend.reward", params: []aWireParam{{"trend_id", sfs.SFSInt, int32(480)}}},
		{cmd: "lw.season.trend.reward", params: []aWireParam{{"trend_id", sfs.SFSInt, int32(484)}}},
	})
}
