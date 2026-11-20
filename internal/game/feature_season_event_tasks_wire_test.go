package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// dBloodyNightAct is a view.blood.night.act reply for server with planId plan and, when group is
// not negative, bloodNightPlanInfo.groupId group (BloodyNightData.lua:149-195 reads both).
func dBloodyNightAct(server, plan, group int32) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	if group >= 0 {
		info.PutInt("groupId", group)
	}
	return obj("serverId", server, "bloodNightPlanActDataInfo", obj("planId", plan, "bloodNightPlanInfo", info))
}

// TestSeasonEventTasksWire pins what bloody-night-tasks and jungle-trial-rewards send, as literals
// from the 1.0.370 client:
//   - view.blood.night.act {serverId: Int} (MsgDefines.lua:1894; ViewBloodNightActMessage.lua:4-7;
//     BloodyNightData.lua:87)
//   - view.blood.night.task {planId: Int} (MsgDefines.lua:2409; ViewBloodNightTaskMessage.lua:4-7;
//     BloodyNightDataManager.lua:259)
//   - blood.night.task.get.reward {planId: Int, taskId: Int} (MsgDefines.lua:2410;
//     BloodNightTaskGetRewardMessage.lua:4-8; BloodyNightDataManager.lua:322)
//   - season.monster.event.act.view {} (MsgDefines.lua:2998; SeasonMonsterEventActViewMessage.lua:4-6,
//     no params; JungleTrialDataManager.lua:30)
//   - season.monster.event.open.box {actId: Int} (MsgDefines.lua:3003; SeasonMonsterEventOpenBoxMessage.lua:4-7;
//     JungleTrialDataManager.lua:225)
//   - season.monster.event.task.view {actId: Int} (MsgDefines.lua:2999; SeasonMonsterEventTaskViewMessage.lua:4-7;
//     JungleTrialDataManager.lua:258)
//   - season.monster.event.task.get.reward {actId: Int, taskId: Int} (MsgDefines.lua:3000;
//     SeasonMonsterEventTaskGetRewardMessage.lua:4-8; JungleTrialDataManager.lua:286)
//
// Fixture literals: activity 1200009 is type 254 (BloodyNight) and 1200106 type 394 (JungleTrial)
// in table activity 40413; season_blood_night_plan row 1002 has flag 4 (tables 40413); season 4 runs
// lw_season row 85, the Darkness season (type 5, tables 40413).
func TestSeasonEventTasksWire(t *testing.T) {
	withEvNow(t, evTestNow)
	t.Run("bloody-night-tasks", func(t *testing.T) {
		conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			switch cmd {
			case "view.blood.night.act":
				return dBloodyNightAct(812, 1002, 4)
			case "view.blood.night.task":
				return obj("userBloodNightTaskArr", []*sfs.SFSObject{obj("taskId", 31, "num", 5, "state", 1)},
					"allianceBloodNightTaskArr", []*sfs.SFSObject{obj("taskId", 32, "num", 9, "state", 1)})
			}
			return obj("reward", []*sfs.SFSObject{})
		})
		if err := runBloodyNightTasks(conn, bloodyNightInit()); err != nil {
			t.Fatal(err)
		}
		dCheckWire(t, fake, []dWire{
			{"view.blood.night.act", map[string]dParam{"serverId": dInt(812)}},
			{"view.blood.night.task", map[string]dParam{"planId": dInt(1002)}},
			{"blood.night.task.get.reward", map[string]dParam{"planId": dInt(1002), "taskId": dInt(31)}},
			{"blood.night.task.get.reward", map[string]dParam{"planId": dInt(1002), "taskId": dInt(32)}},
		})
	})
	t.Run("jungle-trial-rewards", func(t *testing.T) {
		received := int64(3)
		conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			switch cmd {
			case "season.monster.event.act.view":
				return obj("actId", 77, "userSeasonMonsterActFullInfo", obj(
					"userSeasonMonsterEventActInfo", obj("receiveNum", received),
					"userSeasonMonsterEventRewardRecordInfo", obj("rewardNum", int64(4))))
			case "season.monster.event.open.box":
				received++
				return obj("reward", []*sfs.SFSObject{}, "userSeasonMonsterEventActInfo", obj("receiveNum", received))
			case "season.monster.event.task.view":
				return obj("seasonMonsterEventTaskArr", []*sfs.SFSObject{obj("taskId", 5, "num", 3, "state", 1)})
			}
			return obj("reward", []*sfs.SFSObject{})
		})
		if err := runJungleTrialRewards(conn, evInit(30, evActivity(1200106))); err != nil {
			t.Fatal(err)
		}
		dCheckWire(t, fake, []dWire{
			{"season.monster.event.act.view", dNone()},
			{"season.monster.event.open.box", map[string]dParam{"actId": dInt(77)}},
			{"season.monster.event.task.view", map[string]dParam{"actId": dInt(77)}},
			{"season.monster.event.task.get.reward", map[string]dParam{"actId": dInt(77), "taskId": dInt(5)}},
		})
	})
}
