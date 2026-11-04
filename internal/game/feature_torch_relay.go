package game

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"

	"lastwar-client/internal/session"
)

// Torch relay task and milestone claims (MASTER.md §3.2 #7, gap TORCH), opt-in and static-only:
// none of these commands has been sent live.
//
// For an open type-309 activity the client sends hero.event.info.get {activityId: UtfString}
// (ActivityListDataManager.lua:641-645) and hands the reply to ActivityTorchRelayManager:
// OnReceiveActivityData (ActivityListDataManager.lua:1183-1184; ActivityTorchRelayManager.lua:
// 36-46). Its InitServerData reads, at the reply's top level, taskArr[{id, num, state, ...}],
// totalScore (the milestone progress) and rewardProcess (the claimed milestones, 0-based)
// (ActivityTorchRelayTaskManager.lua:24-29, 108-123; TorchRelayTaskInfo.lua:22-45). The claims:
//
//   - torch.relay.task.reward {activityId: Int, taskId: UtfString} per taskArr entry in state 1
//     (TaskState.CanReceive) whose id is in the row's task_daily or task_game list, the only tasks
//     the task panels list and the red dot counts (ActivityTorchRelayTaskManager.lua:198-237,
//     271-308; UIActivityTaskItem.lua:151-153; TorchRelayTaskGetRewardMessage.lua:5-9). The id is
//     tonumber'd on parse and tostring'd back. The reply's totalScore replaces the milestone
//     progress (OnTaskGetReward, :31-35).
//   - torch.relay.process.reward {activityId: Int, index: Int 0-based} per milestone whose
//     threshold totalScore has reached and whose index is not in rewardProcess
//     (TorchRelayTaskGoalItem.lua:28-49; GetMilestonesRedNum, :239-256;
//     TorchRelayMilestonesGetProgressRewardMessage.lua:5-9). Its reply carries the new
//     rewardProcess and totalScore (OnMilesGetReward, :46-56).
//
// Never sent: torch.relay.run.go/tick/end (the relay run; its result is client-reported),
// torch.relay.level.up (spends relay items), torch.relay.help and the shares (chat). The daily
// free pack, torch.relay.daily.reward, is event-shop-freebies'. No benign error codes: an
// errorCode on a claim is logged (SendAndWait logs every reply in full, at Info on success) and
// stops that command for the activity.
const (
	torchRelayFeature  = "torch-relay"
	torchRelayTaskCmd  = "torch.relay.task.reward"
	torchRelayMilesCmd = "torch.relay.process.reward"
)

// torchRelayActivityRows maps each type-309 activity to its activity_torch_relay row: the client
// reads the row whose id is the activity's subType, table activity's tableInfoType
// (ActivityInfoData.lua:231; ActivityTorchRelayData.lua:33-40). Tables 39879.
var torchRelayActivityRows = map[int32]int32{99086: 1, 99101: 1, 99113: 2, 99162: 3}

// torchRelayRow is the part of an activity_torch_relay row (tables 39879) the claims read: the
// task_daily and task_game ids (GetDailyTaskIdList, GetMilesTaskIdList,
// ActivityTorchRelayConfigTemplate.lua:293-319) and the milestone thresholds, the first field of
// each game_task_reward_show entry (GetMilestonesPointList, :226-247).
type torchRelayRow struct {
	daily, game []int64
	milestones  []int64
}

var torchRelayRows = map[int32]torchRelayRow{
	1: {
		daily: []int64{7024100, 7024101, 7024102},
		game: []int64{7024200, 7024201, 7024202, 7024203, 7024204, 7024205, 7024206, 7024207, 7024208,
			7024209, 7024210, 7024211, 7024212, 7024213, 7024214},
		milestones: []int64{4, 8, 12, 16, 20},
	},
	2: {
		daily: []int64{7025100, 7025101, 7025102},
		game: []int64{7025200, 7025201, 7025202, 7025203, 7025204, 7025205, 7025206, 7025207, 7025208,
			7025209, 7025210, 7025211, 7025212, 7025213, 7025214},
		milestones: []int64{4, 8, 12, 16, 20},
	},
	3: {
		daily: []int64{7026100, 7026101, 7026102},
		game: []int64{7026200, 7026201, 7026202, 7026203, 7026204, 7026205, 7026206, 7026207, 7026208,
			7026209, 7026210, 7026211, 7026212, 7026213, 7026214},
		milestones: []int64{4, 8, 12, 16, 20},
	},
}

func init() {
	registerFeature(Feature{
		Name: torchRelayFeature,
		Summary: "OPT-IN: claim torch relay daily/game task rewards and reached task-score milestones; " +
			"never runs the relay or levels up (static-only, not validated live)",
		Run: runTorchRelay,
	})
}

func runTorchRelay(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeTorchRelay) {
		if b.dead {
			break
		}
		claimTorchRelay(s, b, a)
	}
	return b.err()
}

func claimTorchRelay(s *ActivitySweep, b *evBatch, a Activity) {
	rowID, ok := torchRelayActivityRows[a.ID]
	row, okRow := torchRelayRows[rowID]
	if !ok || !okRow {
		slog.Info(torchRelayFeature+": activity has no embedded activity_torch_relay row; skipped until the rows are regenerated",
			"activity", a.ID, "tableVersion", activityTableVersion)
		return
	}
	info, err := s.EventInfo(a)
	if err != nil {
		b.add(err)
		return
	}
	if t, ok := evNum(info, "activityType"); ok && t != int64(actTypeTorchRelay) {
		slog.Info(torchRelayFeature+": info reply is for another activity type; skipped", "activity", a.ID, "activityType", t)
		return
	}
	score, _ := evNum(info, "totalScore")
	claimed := evNums(info, "rewardProcess")
	tasks := evObjects(info, "taskArr")
	var due []string
	for _, t := range tasks {
		id, ok := evNum(t, "id")
		st, _ := evNum(t, "state")
		if ok && st == eventTaskCanReceive && (slices.Contains(row.daily, id) || slices.Contains(row.game, id)) {
			due = append(due, strconv.FormatInt(id, 10))
		}
	}
	slog.Info(torchRelayFeature+": activity info", "activity", a.ID, "row", rowID, "totalScore", score,
		"rewardProcess", claimed, "tasks", len(tasks), "tasksDue", due)

	for _, id := range due {
		p := evActivityIDParam(a)
		p.PutUtfString("taskId", id)
		msg := b.send(fmt.Sprintf("torch relay task %s (activity %d)", id, a.ID), torchRelayTaskCmd, p)
		if msg == nil {
			break
		}
		if n, ok := evNum(msg.Params, "totalScore"); ok {
			score = n
		}
	}
	for i, need := range row.milestones {
		if b.dead {
			return
		}
		if score < need || slices.Contains(claimed, int64(i)) {
			continue
		}
		p := evActivityIDParam(a)
		p.PutInt("index", int32(i))
		msg := b.send(fmt.Sprintf("torch relay milestone %d (activity %d)", i, a.ID), torchRelayMilesCmd, p)
		if msg == nil {
			return
		}
		if n, ok := evNum(msg.Params, "totalScore"); ok {
			score = n
		}
		if _, ok := msg.Params.Get("rewardProcess"); ok {
			claimed = evNums(msg.Params, "rewardProcess")
		}
	}
}
