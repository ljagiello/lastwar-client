package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestWorldBossKillsWire pins world-boss-kill-rewards' wire shape as literals from the 1.0.370
// client:
//
//   - user.get.all.berserk.boss.march (MsgDefines.lua:1956) with no params:
//     UserGetAllBerserkBossMarchMessage.lua puts none, sent by LWBerserkBossManager.lua:58.
//   - berserk.boss.reward (MsgDefines.lua:1964) {uuid: Long}: BerserkBossRewardMessage.lua:6, sent by
//     LWBerserkBossManager.lua:102.
//
// 80004 is a table type-307 row; boss 8800000000011 (a uuid past Int32) is dead and was hit.
func TestWorldBossKillsWire(t *testing.T) {
	withEvNow(t, evTestNow)
	in := worldBossInit([3]int64{8800000000011, 2, 0})
	got := bWireRun(t, "world-boss-kill-rewards", in, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "user.get.all.berserk.boss.march" {
			return worldBossMarches([2]int64{8800000000011, 0})
		}
		return evOK()
	})
	bWireCheck(t, got, []bWireReq{
		bReq("user.get.all.berserk.boss.march"),
		bReq("berserk.boss.reward", bLong("uuid", 8800000000011)),
	})
}
