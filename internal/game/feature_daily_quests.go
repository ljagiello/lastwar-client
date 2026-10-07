package game

import (
	"errors"
	"log/slog"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Daily quests and the activity-point chests (MASTER.md §4 #4), lost at the server-day reset.
//
// `daily.quest.ls {}` returns dailyQuest[] {id (UtfString), state, num, totalNum, ...},
// rewardList[] {point, info} (chest i is the i-th entry, 1-based) and curReward[] (the 1-based
// indexes of chests already claimed) (DailyTaskManager.lua:41-84; DailyTaskInfo.lua:18-40).
// TaskState is -2 NotExist, -1 NotStart, 0 in progress, 1 CanReceive, 2 Received
// (EnumType.lua:5258-5264). Each CanReceive quest is claimed with
// `daily.task.reward {taskId: UtfString}`, the id sent back unchanged
// (AllianceDailyTaskRewardMessage.lua:6). A chest is claimable when its index is not in curReward
// and the points of Received quests (table daily_quest `point`) reach rewardList[i].point
// (DailyTaskManager.lua:109-122, 150-165); the client then sends `daily.quest.reward {stage: -1}`
// whichever chest was tapped (UILWDailyTaskGoalItem.lua:28-52), and the reply lists the chests
// granted in stageArr. That -1 means "every reached chest" is inferred from stageArr being a list.
// Static-only: not sent live yet.
const (
	dailyQuestsListCmd   = "daily.quest.ls"
	dailyQuestsTaskCmd   = "daily.task.reward"
	dailyQuestsChestCmd  = "daily.quest.reward"
	dailyQuestsCanClaim  = 1
	dailyQuestsReceived  = 2
	dailyQuestsAllChests = -1
)

// dailyQuestsPoints is daily_quest.point per quest id (tables 1.0.364, identical in live 39516).
// Quest 108 has show = 0, so the client never lists or claims it (DailyTaskManager.lua:197-210);
// it is left out here for the same reason.
var dailyQuestsPoints = map[string]int64{
	"101": 10, "102": 10, "103": 10, "104": 10, "105": 10, "106": 10, "107": 10, "109": 10,
	"110": 10, "111": 10, "112": 10, "113": 10, "114": 10, "115": 10, "116": 10, "117": 10,
	"118": 20, "119": 40, "120": 50, "121": 30, "122": 30, "123": 10,
}

func init() {
	registerFeature(Feature{
		Name:    "daily-quests",
		Summary: "claim finished daily quests, then every reached activity-point chest",
		Run:     runDailyQuests,
	})
	session.RegisterBenignErrorCode("120289", dailyQuestsTaskCmd, dailyQuestsChestCmd)
	// Live 2026-10-06 19:05 PDT, minutes after the 02:00 UTC daily reset: daily.quest.ls still listed
	// quests 105 and 119 as state 1, and daily.task.reward answered {errorCode=E000000,
	// errorMsg="no rewards can be claimed"}. It is the server's "nothing to claim", scoped to this
	// command; it has not been seen at any other hour.
	session.RegisterBenignErrorCode("E000000", dailyQuestsTaskCmd)
}

func runDailyQuests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("daily-quests: no init push; skipping")
		return nil
	}
	list, err := session.SendAndWait(conn, "daily quest list", dailyQuestsListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}

	received := map[string]bool{}
	var claimable []string
	for _, q := range claimObjects(list.Params, "dailyQuest") {
		id, ok := claimString(q, "id")
		if !ok || id == "" {
			continue
		}
		switch state, _ := claimInt(q, "state"); state {
		case dailyQuestsReceived:
			received[id] = true
		case dailyQuestsCanClaim:
			if _, listed := dailyQuestsPoints[id]; listed {
				claimable = append(claimable, id)
			}
		}
	}
	errs := claimEach(claimable, func(id string) error {
		params := sfs.NewSFSObject()
		params.PutUtfString("taskId", id)
		msg, err := claimAndLog(conn, "daily quest "+id, dailyQuestsTaskCmd, params)
		if err == nil && msg != nil && !msg.Params.Has("errorCode") {
			received[id] = true
		}
		return err
	})
	if session.ContainsNonTimeoutNetError(errors.Join(errs...)) {
		return errors.Join(errs...)
	}

	var points int64
	for id := range received {
		points += dailyQuestsPoints[id]
	}
	if dailyQuestsChestReached(list.Params, points) {
		params := sfs.NewSFSObject()
		params.PutInt("stage", dailyQuestsAllChests)
		if _, err := claimAndLog(conn, "daily quest chests", dailyQuestsChestCmd, params); err != nil {
			errs = append(errs, err)
		}
	} else {
		slog.Info("daily-quests: no unclaimed chest reached", "points", points)
	}
	return errors.Join(errs...)
}

// dailyQuestsChestReached reports whether some chest in rewardList is unclaimed (its 1-based
// index is not in curReward) and points reach its threshold.
func dailyQuestsChestReached(list *sfs.SFSObject, points int64) bool {
	claimed := claimInts(list, "curReward")
	for i, chest := range claimObjects(list, "rewardList") {
		need, ok := claimInt(chest, "point")
		if ok && points >= need && !slices.Contains(claimed, int64(i+1)) {
			return true
		}
	}
	return false
}
