package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestGiftBoxWire pins both Gift Box features' wire shape as literals from the 1.0.370 client:
//
//   - get.activity.gift.box.info (MsgDefines.lua:1507) {activityId: Int}:
//     GetActivityGiftBoxInfoMessage.lua:6, sent by ActivityListDataManager.lua:316.
//   - activity.gift.box.lottery (MsgDefines.lua:1509) {activityId, fiveLottery, useFree: Int}:
//     ActivityGiftBoxLotteryMessage.lua:6-8, sent by UIGiftBoxNew.lua:530 as (activityId, type,
//     isFree). The single draw is OnClickOne(0) (:111, 489), so fiveLottery is 0; isFree is 1 while
//     useFreeTimes < cost_1_free (RefreshCostOne, :397-402), so useFree is 1.
//   - gift.box.receive (MsgDefines.lua:1513) {activityId, index: Int}: GiftBoxReceiveMessage.lua:7-8,
//     sent by GiftBoxRewardBoxItemNew.lua:86 with the entry's own index.
//
// 98001 is a table type-132 row with an activity_boxopen row.
func TestGiftBoxWire(t *testing.T) {
	withEvNow(t, evTestNow)
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "get.activity.gift.box.info" {
			return giftBoxReply(0, 0, 100, [3]int32{1, 50, 1}, [3]int32{2, 100, 0}, [3]int32{3, 200, 0})
		}
		return evOK()
	}
	in := evInit(30, evActivity(98001))
	t.Run("gift-box-free-draw", func(t *testing.T) {
		bWireCheck(t, bWireRun(t, "gift-box-free-draw", in, reply), []bWireReq{
			bReq("get.activity.gift.box.info", bInt("activityId", 98001)),
			bReq("activity.gift.box.lottery", bInt("activityId", 98001), bInt("fiveLottery", 0), bInt("useFree", 1)),
		})
	})
	t.Run("gift-box-score-boxes", func(t *testing.T) {
		bWireCheck(t, bWireRun(t, "gift-box-score-boxes", in, reply), []bWireReq{
			bReq("get.activity.gift.box.info", bInt("activityId", 98001)),
			bReq("gift.box.receive", bInt("activityId", 98001), bInt("index", 2)),
		})
	})
}
