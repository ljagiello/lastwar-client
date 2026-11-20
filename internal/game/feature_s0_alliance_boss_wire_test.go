package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestS0AllianceBossWire pins s0-alliance-boss's wire shape as literals from the 1.0.370 client:
//
//   - alliance.boss.s0.act.info (MsgDefines.lua:3050) {type: Int}: AllianceBossS0ActInfoMessage.lua:6,
//     sent by ReqActMainMessage (S0AllianceBossDataManager.lua:284-287), whose type defaults to 1;
//     ActivityListDataManager.lua:900 calls it with no argument.
//   - receive.alliance.boss.s0.first.reward (MsgDefines.lua:3058) {difficultyLevel: Int}:
//     ReceiveAllianceBossS0FirstRewardMessage.lua:6, sent by S0AllianceBossDataManager.lua:330.
//
// 94300 is the table's type-269 row; levels 2 and 3 are claimable (state 1).
func TestS0AllianceBossWire(t *testing.T) {
	withEvNow(t, evTestNow)
	got := bWireRun(t, "s0-alliance-boss", s0BossInit(), func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "alliance.boss.s0.act.info":
			return s0BossInfo()
		case "receive.alliance.boss.s0.first.reward":
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
	bWireCheck(t, got, []bWireReq{
		bReq("alliance.boss.s0.act.info", bInt("type", 1)),
		bReq("receive.alliance.boss.s0.first.reward", bInt("difficultyLevel", 2)),
		bReq("receive.alliance.boss.s0.first.reward", bInt("difficultyLevel", 3)),
	})
}
