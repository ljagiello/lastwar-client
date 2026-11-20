package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestSurvivorPackWire pins survivor-pack's wire shape as literals from the 1.0.370 client:
//
//   - survivor.visitor.info (MsgDefines.lua:3034) {activityId: Int}: SurvivorVisitorInfoMessage.lua:6,
//     sent by UISurvivorPackPopupView.lua:489.
//   - survivor.visitor.receive.free (MsgDefines.lua:3036) {activityId, id: Int}:
//     SurvivorVisitorReceiveFreeMessage.lua:6-7, sent by UISurvivorPackPopupView.lua:502.
//   - survivor.visitor.receive.bubble (MsgDefines.lua:3035) {activityId, id: Int}:
//     SurvivorVisitorReceiveBubbleMessage.lua:6-7, sent by SurvivorPackManager.lua:457.
//   - survivor.visitor.receive.score (MsgDefines.lua:3037) {activityId, index: Int}:
//     SurvivorVisitorReceiveScoreMessage.lua:6-7, sent by UISurvivorPackRewardItem.lua:77.
//
// 1110002 is the table's type-188 row (tableInfoType 1). Day 3, survivor 1 claimed and listed:
// survivors 2 and 3 (activity_survivor_list 40413: group "1", day = id) get free then bubble. The
// score item is activity_survivor row 1's score_item 993113, and its bar_reward scores are 5, 10, 20,
// 40, 50: 12 items reach stages 0 and 1, and 0 is claimed.
func TestSurvivorPackWire(t *testing.T) {
	withEvNow(t, evTestNow)
	in := survivorInit(48*time.Hour, []int32{1}, []int32{1}, 0)
	in.Raw.PutSFSArray("items", bObjArray(obj("itemId", "993113", "count", 12)))
	got := bWireRun(t, "survivor-pack", in, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	bWireCheck(t, got, []bWireReq{
		bReq("survivor.visitor.info", bInt("activityId", 1110002)),
		bReq("survivor.visitor.receive.free", bInt("activityId", 1110002), bInt("id", 2)),
		bReq("survivor.visitor.receive.bubble", bInt("activityId", 1110002), bInt("id", 2)),
		bReq("survivor.visitor.receive.free", bInt("activityId", 1110002), bInt("id", 3)),
		bReq("survivor.visitor.receive.bubble", bInt("activityId", 1110002), bInt("id", 3)),
		bReq("survivor.visitor.receive.score", bInt("activityId", 1110002), bInt("index", 1)),
	})
}
