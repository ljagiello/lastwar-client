package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestEventMilestonesWire pins event-milestone-boxes' wire shape as literals from the 1.0.370
// client:
//
//   - get.lucky.roll.info (MsgDefines.lua:978) {activityId: Int}: GetLuckyRollInfoMessage.lua:7,
//     sent by ActivityListDataManager.lua:270.
//   - receive.lucky.roll.stage.reward (MsgDefines.lua:981) {activityId, stage: Int}:
//     ReceiveLuckyRollStageRewardMessage.lua:7-8, sent by UILuckyRollBox.lua:97 with data.stage.
//   - hero.event.info.get (MsgDefines.lua:528) {activityId: UtfString}: ActivityEventInfoGetMessage.
//     lua:7, tostring(activityId) at every send site (e.g. ActMonopoly.lua:540).
//   - blue.shop.box.reward (MsgDefines.lua:1754) {activityId, index: Int}: BlueShopBoxRewardMessage.
//     lua:7-8, sent by UILWTitaniumBlueBoxRewardItemRender.lua:81 with the entry's own index.
//   - slots.process.reward (MsgDefines.lua:1705) {activityId, index: Int}: SlotsProcessRewardMessage.
//     lua:6-7, sent by UIActSlotProgressItem.lua:88 with self.index - 1 (box 2 sends 1).
//   - slots.task.reward (MsgDefines.lua:1707) {activityId: Int, taskId: UtfString}:
//     SlotsTaskRewardMessage.lua:6-7, sent by UIActSlotTaskItem.lua:189.
//
// 14 (type 124), 5301 (type 156) and 99050 (type 159, tableInfoType 1) are table activity rows;
// activity_slots_info row 1's first two thresholds are 30 and 100.
func TestEventMilestonesWire(t *testing.T) {
	withEvNow(t, evTestNow)
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case "get.lucky.roll.info":
			r.PutSFSObject("rollInfo", obj("totalLotteryCount", 25))
			r.PutSFSArray("stageArr", bObjArray(obj("stage", 1, "needLotteryNum", 10, "state", 1),
				obj("stage", 2, "needLotteryNum", 20, "state", 0)))
		case "hero.event.info.get":
			switch p.GetString("activityId") {
			case "5301":
				r.PutInt("totalScore", 600)
				r.PutSFSArray("boxRewards", bObjArray(obj("index", 1, "target", 100), obj("index", 2, "target", 500)))
				r.PutSFSArray("boxReceive", eventBoxInts(1))
			case "99050":
				r.PutInt("totalScore", 150)
				r.PutSFSArray("rewardProcess", eventBoxInts(0))
				r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"901": 1}))
			}
		default:
			return evOK()
		}
		return r
	}
	got := bWireRun(t, "event-milestone-boxes", evInit(30, evActivity(14), evActivity(5301), evActivity(99050)), reply)
	bWireCheck(t, got, []bWireReq{
		bReq("get.lucky.roll.info", bInt("activityId", 14)),
		bReq("receive.lucky.roll.stage.reward", bInt("activityId", 14), bInt("stage", 2)),
		bReq("hero.event.info.get", bStr("activityId", "5301")),
		bReq("blue.shop.box.reward", bInt("activityId", 5301), bInt("index", 2)),
		bReq("hero.event.info.get", bStr("activityId", "99050")),
		bReq("slots.process.reward", bInt("activityId", 99050), bInt("index", 1)),
		bReq("slots.task.reward", bInt("activityId", 99050), bStr("taskId", "901")),
	})
}
