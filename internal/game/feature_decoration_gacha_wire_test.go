package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestDecorationGachaWire pins decoration-gacha-boxes' wire shape as literals from the 1.0.370
// client:
//
//   - hero.event.info.get (MsgDefines.lua:528) {activityId: UtfString}: ActivityEventInfoGetMessage.
//     lua:7, the read ActivityListDataManager.lua:619-623 sends for type 173.
//   - decoration.receive.box (MsgDefines.lua:1972) {activityId, index: Int}:
//     ActivityDecorationGachaProgressClaimMessage.lua:6-7, sent by ActivityDecorationGachaManager.lua:
//     657 with the entry's own index.
//   - decoration.receive.wish (MsgDefines.lua:1971) {activityId: Int}:
//     ActivityDecorationGachaWishClaimMessage.lua:6, sent by ActivityDecorationGachaManager.lua:643.
//
// 98630 is a table type-173 row.
func TestDecorationGachaWire(t *testing.T) {
	withEvNow(t, evTestNow)
	got := bWireRun(t, "decoration-gacha-boxes", evInit(30, evActivity(98630)), func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return decorationReply(400, []int32{10}, 50, 50, 7001, true) // box 20 reached, the wish full
		}
		return evOK()
	})
	bWireCheck(t, got, []bWireReq{
		bReq("hero.event.info.get", bStr("activityId", "98630")),
		bReq("decoration.receive.box", bInt("activityId", 98630), bInt("index", 20)),
		bReq("decoration.receive.wish", bInt("activityId", 98630)),
	})
}
