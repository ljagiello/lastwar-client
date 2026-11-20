package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Zone mobilization daily tasks (MASTER G33, cov-ui-store/F3, aud-alliance verify "missed" 1),
// opt-in and static-only. While the event runs (zoneMobRunning, the gate zone-mobilization uses),
// the task tab sends `get.zone.mobilization.task.info {action: Int 1}`
// (LWUIZoneMobilizationTaskView.lua:81-84; GetZoneMobilizationTaskInfoMessage.lua:4-7), whose
// reply's taskArr[{taskId, num, state, reward}] lists the daily tasks
// (LWZoneMobilizationManager.lua:199-216; LWZoneMobilizationDailyTaskInfo.lua:18-26). A task with
// state 1 (TaskState.CanReceive, EnumType.lua:5258-5264) shows a claim button that sends
// `task.reward.get {id: UtfString}` with the taskId as is
// (LWZoneMobilizationDailyTaskItemRender.lua:85-87, 102-107; TaskRewardGetMessage.lua:4-8), the
// command the ON quests feature sends for init task[]; Go sends its decimal string. If the server
// also lists these tasks in init task[], quests already claims them and these claims answer
// "already executed" (benign, registered by quests): the task info reply is logged at Info with
// each task's id, state and whether init task[] lists it, to settle that on the first live run.
// The supply and resource points of action 0 have no claim sender and are left out. The first
// non-benign errorCode stops the run.
const (
	zoneMobTasksFeature   = "zone-mobilization-tasks"
	zoneMobTaskInfoCmd    = "get.zone.mobilization.task.info"
	zoneMobTaskActionList = 1 // the daily-task tab's action (action 0 is the supply list)
	zoneMobTasksMaxClaims = 30
)

func init() {
	registerFeature(Feature{
		Name: zoneMobTasksFeature,
		Summary: "OPT-IN: claim the finished zone mobilization daily tasks (task.reward.get) while the event runs " +
			"(static-only, not validated live)",
		Run: runZoneMobilizationTasks,
	})
}

func runZoneMobilizationTasks(conn *session.GameConn, in *Init) error {
	b := &evBatch{conn: conn}
	if zoneMobRunning(b, in, zoneMobTasksFeature) == nil {
		return b.err()
	}
	p := sfs.NewSFSObject()
	p.PutInt("action", zoneMobTaskActionList)
	msg := b.send("zone mobilization daily tasks", zoneMobTaskInfoCmd, p)
	if msg == nil {
		return b.err()
	}
	inInit := map[string]bool{}
	for _, t := range in.Objects("task") {
		inInit[questID(t)] = true
	}
	var due []string
	tasks := evObjects(msg.Params, "taskArr")
	for _, t := range tasks {
		id := evString(t, "taskId")
		st, _ := evNum(t, "state")
		num, _ := evNum(t, "num")
		slog.Info(zoneMobTasksFeature+": task", "taskId", id, "state", st, "num", num, "inInitTasks", inInit[id])
		if id != "" && id != "0" && st == questCanReceive {
			due = append(due, id)
		}
	}
	slog.Info(zoneMobTasksFeature+": daily tasks", "tasks", len(tasks), "claimable", len(due), "hasTaskArr", msg.Params.Has("taskArr"))
	if len(due) > zoneMobTasksMaxClaims {
		due = due[:zoneMobTasksMaxClaims]
	}
	for _, id := range due {
		q := sfs.NewSFSObject()
		q.PutUtfString("id", id)
		if b.send("zone mobilization daily task "+id, questRewardCmd, q) == nil {
			break
		}
	}
	return b.err()
}
