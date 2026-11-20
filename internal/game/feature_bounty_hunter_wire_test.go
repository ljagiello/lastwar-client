package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestBountyHunterWire pins both Bounty Hunter features' wire shape as literals from the 1.0.370
// client:
//
//   - bounty.hunter.get.info (MsgDefines.lua:2396) {activityId: Int}: BountyHunterGetInfoMessage.lua:6,
//     sent by ActivityListDataManager.lua:717.
//   - bounty.hunter.receive.phase.reward (MsgDefines.lua:2731) {activityId, phaseId: Int}:
//     BountyHunterReceivePhaseRewardMessage.lua:6-7, sent by BountyHunterPhaseItemComponent.lua:102
//     with the milestone's needScore. activity_hunter row 1 (tables 40413) score_reward is
//     "300;388017006|1000;388017007|1800;388017008|3000;388017009|5000;388017010", so a score of
//     2000 with 300 claimed sends 1000 and 1800.
//   - bounty.hunter.click.chest.reward (MsgDefines.lua:2735) {activityId: Int, targetId: UtfString}:
//     BountyHunterClickChestRewardMessage.lua:6-7 (tostring(chestUuid)), sent by
//     BountyHunterSceneView.lua:942.
//   - bounty.hunter.receive.drop.reward (MsgDefines.lua:2399) {activityId: Int}:
//     BountyHunterReceiveDropRewardMessage.lua:6, sent by UIActBountyHunterRewardCtrl.lua:46.
//
// 98800 is the table's type-350 row with tableInfoType 1.
func TestBountyHunterWire(t *testing.T) {
	withEvNow(t, evTestNow)
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "bounty.hunter.get.info":
			return bountyInfo(2000, []int32{300}, "8800000000001")
		case "bounty.hunter.click.chest.reward":
			r := sfs.NewSFSObject()
			r.PutSFSArray("stashReward", freebieReward())
			return r
		}
		return evOK()
	}
	in := evInit(30, evActivity(98800))
	t.Run("bounty-hunter-milestones", func(t *testing.T) {
		bWireCheck(t, bWireRun(t, "bounty-hunter-milestones", in, reply), []bWireReq{
			bReq("bounty.hunter.get.info", bInt("activityId", 98800)),
			bReq("bounty.hunter.receive.phase.reward", bInt("activityId", 98800), bInt("phaseId", 1000)),
			bReq("bounty.hunter.receive.phase.reward", bInt("activityId", 98800), bInt("phaseId", 1800)),
		})
	})
	t.Run("bounty-hunter-chests", func(t *testing.T) {
		bWireCheck(t, bWireRun(t, "bounty-hunter-chests", in, reply), []bWireReq{
			bReq("bounty.hunter.get.info", bInt("activityId", 98800)),
			bReq("bounty.hunter.click.chest.reward", bInt("activityId", 98800), bStr("targetId", "8800000000001")),
			bReq("bounty.hunter.receive.drop.reward", bInt("activityId", 98800)),
		})
	})
}
