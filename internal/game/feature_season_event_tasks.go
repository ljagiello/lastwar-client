package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season event task rewards, two OPT-IN, static-only features.
//
// bloody-night-tasks (MASTER G16): the Darkness season's Bloody Night task rewards. On a BloodyNight
// activity (type 254, EnumType.lua:6443) the client asks for the source server's plan,
// view.blood.night.act {serverId: Int} (ActivityListDataManager.lua:196-198;
// BloodyNightDataManager.lua:35-46; BloodyNightData.lua:79-88; ViewBloodNightActMessage.lua:4-7),
// and, when the reply is for the source server (BloodyNightDataManager.lua:133-148), takes the
// stage whose season_blood_night_plan row has id bloodNightPlanActDataInfo.planId and flag
// bloodNightPlanInfo.groupId (BloodyNightData.lua:149-195) and sends view.blood.night.task {planId:
// Int} (BloodyNightDataManager.lua:256-261; ViewBloodNightTaskMessage.lua:4-7) -> userBloodNightTaskArr
// and allianceBloodNightTaskArr [{taskId, num, state, dayTime}] (:263-286). A task in state 1
// (TaskState.CanReceive, EnumType.lua:5292-5298) is claimed with blood.night.task.get.reward
// {planId: Int, taskId: Int} (BloodyNightTaskItem.lua:173-179; BloodyNightDataManager.lua:319-324;
// BloodNightTaskGetRewardMessage.lua:4-8). Sent only in a Darkness season (lw_season type 5), where
// the client reads Bloody Night at all (BloodyNightDataManager.lua:222-226).
//
// jungle-trial-rewards (MASTER G17): the Jungle Trial (season monster event, type 394,
// EnumType.lua:6397) boxes and task rewards. On the activity the client sends
// season.monster.event.act.view {} (ActivityListDataManager.lua:193-195; JungleTrialDataManager.lua:
// 29-31) -> actId, userSeasonMonsterActFullInfo {userSeasonMonsterEventActInfo {receiveNum},
// userSeasonMonsterEventRewardRecordInfo {rewardNum}}; a box can be opened while rewardNum -
// receiveNum > 0 (:243-248), one season.monster.event.open.box {actId: Int} per box (:223-227;
// SeasonMonsterEventOpenBoxMessage.lua:4-18), whose reply's userSeasonMonsterEventActInfo replaces
// the old (:229-234). Then season.monster.event.task.view {actId: Int} (:256-260) ->
// seasonMonsterEventTaskArr [{taskId, num, state}]; a task in state 1 is claimed with
// season.monster.event.task.get.reward {actId: Int, taskId: Int} (JungleTrialTaskItem.lua:176-181;
// :284-288; SeasonMonsterEventTaskGetRewardMessage.lua:4-8). The boxes are earned by the player's
// kills; the claims need no march.
//
// No error code is registered as benign: the first replies are logged and an error stops the claims
// of that list.
const (
	bloodyNightFeature       = "bloody-night-tasks"
	bloodyNightActCmd        = "view.blood.night.act"
	bloodyNightTaskCmd       = "view.blood.night.task"
	bloodyNightRewardCmd     = "blood.night.task.get.reward"
	bloodyNightActType       = 254 // BloodyNight (EnumType.lua:6443)
	jungleTrialFeature       = "jungle-trial-rewards"
	jungleTrialActCmd        = "season.monster.event.act.view"
	jungleTrialBoxCmd        = "season.monster.event.open.box"
	jungleTrialTaskCmd       = "season.monster.event.task.view"
	jungleTrialRewardCmd     = "season.monster.event.task.get.reward"
	jungleTrialActType       = 394 // JungleTrial (EnumType.lua:6397)
	seasonTaskCanReceive     = 1   // TaskState.CanReceive (EnumType.lua:5292-5298)
	jungleTrialMaxBoxesInRun = 100
)

func init() {
	registerFeature(Feature{
		Name:    bloodyNightFeature,
		Summary: "OPT-IN: in a Darkness season, claim finished Bloody Night personal and alliance task rewards (static-only, not validated live)",
		Run:     runBloodyNightTasks,
	})
	registerFeature(Feature{
		Name:    jungleTrialFeature,
		Summary: "OPT-IN: open earned Jungle Trial boxes and claim finished Jungle Trial task rewards (static-only, not validated live)",
		Run:     runJungleTrialRewards,
	})
}

func runBloodyNightTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || evSeasonID(in) <= 0 {
		return nil
	}
	if cfg, _, ok := evSeasonConfig(in); !ok || cfg.typ != seasonMapDarkness {
		return nil
	}
	if len(Activities(conn, in).Open(bloodyNightActType)) == 0 {
		return nil
	}
	source, ok := ghostReconSourceServer(in)
	if !ok {
		slog.Info(bloodyNightFeature + ": source server unknown; skipping")
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutInt("serverId", int32(source))
	msg, err := session.SendAndWait(conn, bloodyNightFeature+": activity", bloodyNightActCmd, p)
	if err != nil {
		return err
	}
	server, _ := evNum(msg.Params, "serverId")
	act := evObject(msg.Params, "bloodNightPlanActDataInfo")
	planID, hasPlan := evNum(act, "planId")
	group, _ := evNum(evObject(act, "bloodNightPlanInfo"), "groupId")
	flag, known := seasonBloodNightPlans[int32(planID)]
	slog.Info(bloodyNightFeature+": activity", "sourceServer", source, "replyServer", server, "planId", planID,
		"groupId", group, "planInTable", known)
	if server != source || !hasPlan || !known || int64(flag) != group {
		slog.Info(bloodyNightFeature+": no Bloody Night stage of the source server the table knows; nothing to claim",
			"tableVersion", tableVersion)
		return nil
	}
	tp := sfs.NewSFSObject()
	tp.PutInt("planId", int32(planID))
	tasks, err := session.SendAndWait(conn, bloodyNightFeature+": tasks", bloodyNightTaskCmd, tp)
	if err != nil {
		return err
	}
	due := seasonTasksDue(append(evObjects(tasks.Params, "userBloodNightTaskArr"), evObjects(tasks.Params, "allianceBloodNightTaskArr")...))
	slog.Info(bloodyNightFeature+": tasks", "planId", planID, "due", due)
	for _, id := range due {
		p := sfs.NewSFSObject()
		p.PutInt("planId", int32(planID))
		p.PutInt("taskId", id)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s: task %d", bloodyNightFeature, id), bloodyNightRewardCmd, p); err != nil {
			return err
		}
	}
	return nil
}

// seasonTasksDue returns the taskId of every task in state CanReceive.
func seasonTasksDue(tasks []*sfs.SFSObject) []int32 {
	var due []int32
	for _, t := range tasks {
		st, _ := evNum(t, "state")
		if id, ok := evNum(t, "taskId"); ok && st == seasonTaskCanReceive {
			due = append(due, int32(id))
		}
	}
	return due
}

func runJungleTrialRewards(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if len(Activities(conn, in).Open(jungleTrialActType)) == 0 {
		return nil
	}
	msg, err := session.SendAndWait(conn, jungleTrialFeature+": activity", jungleTrialActCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	actID, ok := evNum(msg.Params, "actId")
	full := evObject(msg.Params, "userSeasonMonsterActFullInfo")
	received, hasReceived := evNum(evObject(full, "userSeasonMonsterEventActInfo"), "receiveNum")
	earned, hasEarned := evNum(evObject(full, "userSeasonMonsterEventRewardRecordInfo"), "rewardNum")
	slog.Info(jungleTrialFeature+": activity", "actId", actID, "hasActId", ok, "rewardNum", earned, "receiveNum", received)
	if !ok {
		return nil
	}
	if hasReceived && hasEarned {
		if err := jungleTrialOpenBoxes(conn, int32(actID), earned, received); err != nil {
			return err
		}
	}
	p := sfs.NewSFSObject()
	p.PutInt("actId", int32(actID))
	tasks, err := session.SendAndWait(conn, jungleTrialFeature+": tasks", jungleTrialTaskCmd, p)
	if err != nil {
		return err
	}
	due := seasonTasksDue(evObjects(tasks.Params, "seasonMonsterEventTaskArr"))
	slog.Info(jungleTrialFeature+": tasks", "actId", actID, "due", due)
	for _, id := range due {
		p := sfs.NewSFSObject()
		p.PutInt("actId", int32(actID))
		p.PutInt("taskId", id)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s: task %d", jungleTrialFeature, id), jungleTrialRewardCmd, p); err != nil {
			return err
		}
	}
	return nil
}

// jungleTrialOpenBoxes opens one box at a time while earned > received, taking receiveNum from each
// reply; it stops when a reply does not raise receiveNum, and after jungleTrialMaxBoxesInRun.
func jungleTrialOpenBoxes(conn *session.GameConn, actID int32, earned, received int64) error {
	for n := 0; earned > received && n < jungleTrialMaxBoxesInRun; n++ {
		p := sfs.NewSFSObject()
		p.PutInt("actId", actID)
		r, err := claimAndLog(conn, fmt.Sprintf("%s: box %d of %d", jungleTrialFeature, received+1, earned), jungleTrialBoxCmd, p)
		if err != nil {
			return err
		}
		next, ok := evNum(evObject(r.Params, "userSeasonMonsterEventActInfo"), "receiveNum")
		if !ok || next <= received {
			slog.Info(jungleTrialFeature+": the box reply did not raise receiveNum; stopping", "receiveNum", received, "reply", next)
			return nil
		}
		received = next
	}
	return nil
}
