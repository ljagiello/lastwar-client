package game

import (
	"errors"
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance tasks (MASTER.md §4 #7): `get.alliance.task.info {isSeason: Bool}` lists taskList[]
// {taskId (Int), state, finishTime (ms), ...}, and each finished, claimable task is claimed with
// `receive.alliance.task.reward {taskId: Int}` (GetAllianceTaskInfoMessage.lua:4-7;
// ClaimAllianceTaskRewardMessage.lua:4-7). The client's CheckIfCanClaim is
// finishTime > 0 && state == 1 (AllianceTaskData.lua:52-73; state 2 is claimed).
//
// The client sends the normal list (isSeason false) only while DataConfig switch alliance_task is
// on; that switch is not in the local item table, so it comes from init dataConfig alone
// (AllianceTaskManager.lua:18-22). It sends the season list (isSeason true) only in season
// (AllianceSeasonTaskManager.lua:18-22; SeasonUtil.IsInSeason). Here "in season" is init
// playerServerSeasonInfo.open && now < seasonEndTime (ms); the client also checks the season mode
// and the battle-server group, which init alone does not give. Static-only: not sent live yet.
const (
	allianceTasksListCmd  = "get.alliance.task.info"
	allianceTasksClaimCmd = "receive.alliance.task.reward"
)

func init() {
	registerFeature(Feature{
		Name:    "alliance-tasks",
		Summary: "claim finished alliance tasks (and season alliance tasks in season)",
		Run:     runAllianceTasks,
	})
	session.RegisterBenignErrorCode("120289", allianceTasksClaimCmd)
}

// allianceTasksInSeason reports whether init playerServerSeasonInfo shows an open season.
func allianceTasksInSeason(in *Init) bool {
	s := in.Object("playerServerSeasonInfo")
	open, _ := claimBool(s, "open")
	end, ok := claimInt(s, "seasonEndTime")
	return open && ok && claimNow(in).UnixMilli() < end
}

func runAllianceTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("alliance-tasks: no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info("alliance-tasks: not in an alliance; skipping")
		return nil
	}
	var lists []bool
	if claimSwitchOn(in, "alliance_task") {
		lists = append(lists, false)
	}
	if allianceTasksInSeason(in) {
		lists = append(lists, true)
	}
	if len(lists) == 0 {
		slog.Info("alliance-tasks: switch alliance_task off and no season; skipping")
		return nil
	}
	return errors.Join(claimEach(lists, func(season bool) error {
		return allianceTasksClaimList(conn, season)
	})...)
}

func allianceTasksClaimList(conn *session.GameConn, season bool) error {
	params := sfs.NewSFSObject()
	params.PutBool("isSeason", season)
	list, err := session.SendAndWait(conn, fmt.Sprintf("alliance task list (season %v)", season), allianceTasksListCmd, params)
	if err != nil {
		return err
	}
	var due []int64
	for _, task := range claimObjects(list.Params, "taskList") {
		id, ok := claimInt(task, "taskId")
		finish, _ := claimInt(task, "finishTime")
		state, _ := claimInt(task, "state")
		if ok && finish > 0 && state == 1 {
			due = append(due, id)
		}
	}
	if len(due) == 0 {
		slog.Info("alliance-tasks: nothing claimable", "season", season)
		return nil
	}
	return errors.Join(claimEach(due, func(id int64) error {
		p := sfs.NewSFSObject()
		p.PutInt("taskId", int32(id))
		_, err := claimAndLog(conn, fmt.Sprintf("alliance task %d", id), allianceTasksClaimCmd, p)
		return err
	})...)
}
