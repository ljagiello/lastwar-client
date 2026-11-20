package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// aGoldTreeReply is a gold.tree.act.view reply whose tree charged at finish, whose user tree week
// is week (userGoldTeeInfo, the Lua's spelling) and whose userGoldTreeDataInfo carries weekTime
// 1746979200000 when withWeekTime (UserGoldTreeDataInfo.lua:15-24; GoldTreeInfo.lua:19-30).
func aGoldTreeReply(finish time.Time, week *sfs.SFSObject, withWeekTime bool) *sfs.SFSObject {
	user := obj("userGoldTeeInfo", week, "userGoldTreeCardArr", []*sfs.SFSObject{})
	if withWeekTime {
		user.PutLong("weekTime", 1746979200000)
	}
	return obj("goldTreeInfo", obj("treeId", 1, "finishTime", finish.UnixMilli()), "userGoldTreeDataInfo", user)
}

// TestGoldTreePrayerWire: on a Wednesday (today 3) in S4 with Gold Tree 1200021 open and the 40413
// cycle "2|6", Tuesday and Wednesday are prayed.
//   - gold.tree.act.view (MsgDefines.lua:2565; sent at SeasonGoldTreeManager.lua:50), no params
//     (GoldTreeActViewMessage.lua:4-6).
//   - gold.tree.open.card (MsgDefines.lua:2566; sent at SeasonGoldTreeManager.lua:100)
//     {weekTime: Long, day: Int} (GoldTreeOpenCardMessage.lua:4-8), weekTime the view's.
func TestGoldTreePrayerWire(t *testing.T) {
	wednesday := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	withEvNow(t, wednesday)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "gold.tree.act.view":
			return aGoldTreeReply(wednesday.Add(-time.Hour),
				obj("startTime", wednesday.Add(-30*time.Hour).UnixMilli(), "endTime", wednesday.Add(100*time.Hour).UnixMilli()), true)
		case "gold.tree.open.card":
			return obj("day", p.GetInt("day"), "cardReward", []*sfs.SFSObject{})
		}
		return evErr("unexpected command")
	})
	act := evActivity(1200021)
	act.PutLong("endTime", wednesday.Add(72*time.Hour).UnixMilli())
	in := evWithSeason(evInit(30, act), 4)
	in.Raw.PutLong("tomorrow", time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC).Unix())
	if err := runGoldTreePrayer(conn, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "gold.tree.act.view"},
		{cmd: "gold.tree.open.card", params: []aWireParam{{"weekTime", sfs.SFSLong, int64(1746979200000)}, {"day", sfs.SFSInt, int32(2)}}},
		{cmd: "gold.tree.open.card", params: []aWireParam{{"weekTime", sfs.SFSLong, int64(1746979200000)}, {"day", sfs.SFSInt, int32(3)}}},
	})
}
