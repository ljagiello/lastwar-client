package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestCitySoldiersWire: the count-battle building (LW_BUILD_COUNT_BATTLE 10213000, EnumType.lua:3686)
// holds 120 soldiers and the Civilization Spark (LW_CIVILIZATION_SPARK 10230000, :3800) 80, with a
// level-5 Drill Ground (LW_BUILD_ARMY_YARD 10104000, :3656) and room in the stock.
//   - lw.claim.stage.soldier (MsgDefines.lua:1296; sent at BuildBubbleManager.lua:4288), no params
//     (LWClaimStageSoldierMessage.lua:5-7).
//   - lw.claim.spark.soldier (MsgDefines.lua:2987; sent at BuildBubbleManager.lua:4290), no params
//     (LWClaimSparkSoldierMessage.lua:4-6).
func TestCitySoldiersWire(t *testing.T) {
	in := citySoldiersInit([][4]int64{{1, 10213000, 3, 120}, {2, 10230000, 1, 80}, {3, 10104000, 5, 0}}, 10_000, 1_000, -1, nil)
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "lw.claim.stage.soldier", "lw.claim.spark.soldier":
			return obj("soldierItemId", "3010", "soldierNum", 120)
		}
		return rewardErr("unexpected command")
	})
	if err := runCitySoldiers(conn, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aRewardSent(fake), []aWireReq{{cmd: "lw.claim.stage.soldier"}, {cmd: "lw.claim.spark.soldier"}})
}
