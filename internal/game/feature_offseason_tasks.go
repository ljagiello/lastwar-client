package game

import (
	"log/slog"
	"slices"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Off-season 1 task rewards and score chests (MASTER.md G18; cov-ui-store F1 and its verdict),
// OPT-IN, claims only. Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet.
//
// Recapture (activity type 347, task group 1) and Queen of Blood (type 348, group 2) run only in
// the Season 1 off-season (EnumType.lua:6481-6482, 17734). While one is open the client reads
// city.battle.activity.gain.task.info {group: Int} (ActivityListDataManager.lua:754-762;
// OffSeason1RecaptureManager.lua:75; CityBattleS1RestGainTaskInfoMessage.lua:4-7). The reply is
// group, score and taskRequests[{configId, hasReward, reward, curNum, totalNum}], hasReward being a
// TaskState: 0 not done, 1 claimable, 2 claimed (OffSeason1TaskDataManager.lua:18-74;
// EnumType.lua:5292-5298). A row whose activity_offSeason_task Type is the group's goal type (-1
// for group 1, -2 for group 2, EnumType.lua:17735-17738) is a score chest, every other row a task
// (OffSeason1TaskDataManager.lua:76-90).
//
// The claim-all button shows while a task row is claimable and sends
// city.battle.activity.gain.task.reward {group: Int, configId: Int -1} (UIOffSeason1TaskView.lua:
// 99-100, 135-153, 180-190). A claimable goal row sends the same command with configId -2, the
// score-chest sentinel of both groups (UIOffSeason1TaskGoalItem.lua:66-72;
// CityBattleS1RestGainReceiveRewardMessage.lua:4-8). After a claim the client re-reads the info
// (:20); this feature re-reads it with its own group before it looks at the chests, since the task
// claims raise the score. Never city.battle.s1.president.choose.city.defend (a president action).
//
// Every reply is logged at Info; an errorCode stops that group (no benign codes).
const (
	offSeasonTasksFeature  = "offseason-tasks"
	offSeasonTaskInfoCmd   = "city.battle.activity.gain.task.info"
	offSeasonTaskRewardCmd = "city.battle.activity.gain.task.reward"
	offSeasonClaimAllTasks = -1 // configId: every claimable task
	offSeasonClaimChest    = -2 // configId: the score chest
	offSeasonTaskClaimable = 1  // TaskState.CanReceive
)

// offSeasonGroups maps the two activity types to their task group and the group's goal Type.
var offSeasonGroups = []struct {
	actType, group, goalType int32
}{
	{347, 1, -1}, // OffSeason1Recapture
	{348, 2, -2}, // OffSeason1QueenOfBlood
}

// offSeasonGoalTypes is activity_offSeason_task's Type of the goal (score chest) rows, tables
// 40413: 10001-10005 are Type -1, 11001-11005 Type -2; the other 36 rows are tasks. An id the
// table lacks reads as a task, as GetTableData's nil Type does in the client.
var offSeasonGoalTypes = map[int64]int32{
	10001: -1, 10002: -1, 10003: -1, 10004: -1, 10005: -1,
	11001: -2, 11002: -2, 11003: -2, 11004: -2, 11005: -2,
}

func init() {
	registerFeature(Feature{
		Name:      offSeasonTasksFeature,
		Summary:   "OPT-IN: claim the Season 1 off-season Recapture and Queen of Blood task rewards and score chests (static-only, not validated live)",
		DefaultOn: false,
		Run:       runOffSeasonTasks,
	})
}

func runOffSeasonTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	done := map[int32]bool{}
	for _, g := range offSeasonGroups {
		for _, a := range s.Open(g.actType) {
			if b.dead || done[g.group] {
				break
			}
			done[g.group] = true
			claimOffSeasonGroup(s, b, a, g.group, g.goalType)
		}
	}
	return b.err()
}

// claimOffSeasonGroup claims one group's tasks, then, from a fresh info reply, its score chest.
func claimOffSeasonGroup(s *ActivitySweep, b *evBatch, a Activity, group, goalType int32) {
	label := offSeasonTasksFeature + ": group " + strconv.Itoa(int(group))
	info, err := offSeasonInfo(s, a, group)
	if err != nil {
		b.add(err)
		return
	}
	tasks, chests := offSeasonClaimable(info, goalType)
	slog.Info(label+" task info", "activity", a.ID, "score", evString(info, "score"),
		"rows", len(evObjects(info, "taskRequests")), "claimableTasks", tasks, "claimableChests", chests)
	if len(tasks) > 0 {
		if !offSeasonClaim(b, label+" tasks", group, offSeasonClaimAllTasks) {
			return
		}
		s.Forget(offSeasonTaskInfoCmd, a)
		if info, err = offSeasonInfo(s, a, group); err != nil {
			b.add(err)
			return
		}
		tasks, chests = offSeasonClaimable(info, goalType)
		slog.Info(label+" task info after the claim", "activity", a.ID, "score", evString(info, "score"),
			"claimableTasks", tasks, "claimableChests", chests)
	}
	if len(chests) > 0 {
		offSeasonClaim(b, label+" score chest", group, offSeasonClaimChest)
	}
}

func offSeasonInfo(s *ActivitySweep, a Activity, group int32) (*sfs.SFSObject, error) {
	p := sfs.NewSFSObject()
	p.PutInt("group", group)
	return s.Info(offSeasonTasksFeature+" task info", offSeasonTaskInfoCmd, a, p)
}

// offSeasonClaim sends one claim and logs its reply; false when it failed.
func offSeasonClaim(b *evBatch, label string, group, configID int32) bool {
	p := sfs.NewSFSObject()
	p.PutInt("group", group)
	p.PutInt("configId", configID)
	r := b.send(label, offSeasonTaskRewardCmd, p)
	if r == nil {
		return false
	}
	evTaskLogReply(label, offSeasonTaskRewardCmd, r.Params, "configId", configID)
	return true
}

// offSeasonClaimable lists the configIds of the claimable task rows and goal rows in an info reply.
func offSeasonClaimable(info *sfs.SFSObject, goalType int32) (tasks, chests []int64) {
	for _, t := range evObjects(info, "taskRequests") {
		id, ok := evNum(t, "configId")
		if !ok || id <= 0 {
			continue
		}
		if st, ok := evNum(t, "hasReward"); !ok || st != offSeasonTaskClaimable {
			continue
		}
		if typ, ok := offSeasonGoalTypes[id]; ok && typ == goalType {
			chests = append(chests, id)
		} else {
			tasks = append(tasks, id)
		}
	}
	slices.Sort(tasks)
	slices.Sort(chests)
	return tasks, chests
}
