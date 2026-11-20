package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestZoneMobilizationTasksWire pins what zone-mobilization-tasks sends, as literals from the
// 1.0.370 client:
//   - get.zone.mobilization.info {tabType: Int 4}, the shared running check (MsgDefines.lua:2227;
//     GetZoneMobilizationInfoMessage.lua:4-7; LWZoneMobilizationManager.lua:100, refreshed with 4 at
//     :195)
//   - get.zone.mobilization.task.info {action: Int 1} (MsgDefines.lua:2230;
//     GetZoneMobilizationTaskInfoMessage.lua:4-7; LWUIZoneMobilizationTaskView.lua:82 sends 1,
//     LWUIZoneMobilizationSuppliesListView.lua:33 sends 0)
//   - task.reward.get {id: UtfString taskId} (MsgDefines.lua:7; TaskRewardGetMessage.lua:4-8;
//     LWUIZoneMobilizationDailyTaskItemRender.lua:104-106)
func TestZoneMobilizationTasksWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "get.zone.mobilization.info":
			return zoneInfo(true)
		case "get.zone.mobilization.task.info":
			return obj("taskArr", []*sfs.SFSObject{obj("taskId", 9001, "num", 5, "state", 1), obj("taskId", 9002, "num", 5, "state", 2),
				obj("taskId", "9003", "num", 1, "state", 1)})
		}
		return evOK()
	})
	if err := runZoneMobilizationTasks(conn, zoneInit(10, true)); err != nil {
		t.Fatal(err)
	}
	dCheckWire(t, fake, []dWire{
		{"get.zone.mobilization.info", map[string]dParam{"tabType": dInt(4)}},
		{"get.zone.mobilization.task.info", map[string]dParam{"action": dInt(1)}},
		{"task.reward.get", map[string]dParam{"id": dStr("9001")}},
		{"task.reward.get", map[string]dParam{"id": dStr("9003")}},
	})
}
