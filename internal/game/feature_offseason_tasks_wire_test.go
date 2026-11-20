package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestOffSeasonTasksWire pins, as literals, every command, param key, SFS type and sent constant of
// offseason-tasks: city.battle.activity.gain.task.info {group: Int} (MsgDefines.lua:2706;
// CityBattleS1RestGainTaskInfoMessage.lua:4-7; OffSeason1RecaptureManager.lua:75 and
// OffSeason1QueenOfBloodManager.lua:134 send OffSeason1TaskGroup 1 and 2, EnumType.lua:17734) and
// city.battle.activity.gain.task.reward {group: Int, configId: Int} (MsgDefines.lua:2707;
// CityBattleS1RestGainReceiveRewardMessage.lua:4-8), whose configId is -1 from the claim-all
// (UIOffSeason1TaskView.lua:100) and -2 from a goal row (UIOffSeason1TaskGoalItem.lua:72).
//
// Activity 1000109 is type 347 and 1000110 type 348 in table activity 40413
// (activity_tables_gen.go). activity_offSeason_task 40413 has task rows 1001 (group 1, Type 1) and
// 3001 (group 2, Type 3) and goal rows 10001 (group 1, Type -1) and 11001 (group 2, Type -2).
func TestOffSeasonTasksWire(t *testing.T) {
	// Each group answers a claimable task first, then, after the claim, its claimable chest.
	reply := func(group, task, chest int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
		infos := 0
		return func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			if cmd != "city.battle.activity.gain.task.info" {
				return evOK()
			}
			infos++
			if infos == 1 {
				return offSeasonInfoReply(group, map[int32]int32{task: 1, chest: 0})
			}
			return offSeasonInfoReply(group, map[int32]int32{task: 2, chest: 1})
		}
	}
	groupReqs := func(group int32) []cWireReq {
		info := cWireReq{"city.battle.activity.gain.task.info", []cWireParam{{"group", sfs.SFSInt, group}}}
		claim := func(configID int32) cWireReq {
			return cWireReq{"city.battle.activity.gain.task.reward", []cWireParam{
				{"group", sfs.SFSInt, group}, {"configId", sfs.SFSInt, configID}}}
		}
		return []cWireReq{info, claim(-1), info, claim(-2)}
	}
	cWireRun(t, []cWireCase{
		{
			name:  "Recapture, group 1",
			setup: func(*testing.T) *Init { return evInit(30, evActivity(1000109)) },
			reply: reply(1, 1001, 10001),
			run:   runOffSeasonTasks,
			want:  groupReqs(1),
		},
		{
			name:  "Queen of Blood, group 2",
			setup: func(*testing.T) *Init { return evInit(30, evActivity(1000110)) },
			reply: reply(2, 3001, 11001),
			run:   runOffSeasonTasks,
			want:  groupReqs(2),
		},
	})
}
