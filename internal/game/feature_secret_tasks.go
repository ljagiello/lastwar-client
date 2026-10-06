package game

import (
	"fmt"
	"log/slog"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Secret Tasks (hero dispatch, MASTER.md §4 #8), claim half only: rewards of dispatch tasks that
// have finished. Starting a task (`hero.dispatch.start`) needs a squad choice and is never sent;
// neither is the paid `dispatch.recover.reward` (§6).
//
// `hero.dispatch.list {}` returns ls[] {uuid (Long), cfgId, completionTime (ms, 0 = not started),
// rewarded (0/1), ...} (DispatchGetTasksMessage.lua:4-6; ActDispatchTaskDataManager.lua:350-392).
// A task is claimable when completionTime > 0 && now >= completionTime && rewarded == 0
// (:678-710). The client's TryRewardAll (:968-984) claims one such task with
// `hero.dispatch.reward {uuid: Long}` and several with `hero.dispatch.batch.reward {uuidList:
// LongArray}` (DispatchRewardMessage.lua:4-7; DispatchBatchRewardMessage.lua:3-6); both only add
// rewards. The client sends the list only while a DispatchTask activity (type 108) is valid, which
// for this type means HQ >= its needMainCityLevel with no time check (:340-349;
// ActivityInfoData.lua:445-486). Static-only: not sent live yet.
const (
	secretTasksListCmd  = "hero.dispatch.list"
	secretTasksOneCmd   = "hero.dispatch.reward"
	secretTasksBatchCmd = "hero.dispatch.batch.reward"
)

// secretTasksActivities are the type-108 (DispatchTask) rows of table activity, each with
// needMainCityLevel 5 (tables 1.0.364, identical in live 39516).
var secretTasksActivities = []int64{94101, 94102}

const secretTasksNeedHQ = 5

func init() {
	registerFeature(Feature{
		Name:    "secret-tasks-claim",
		Summary: "claim finished Secret Task (hero dispatch) rewards; never starts a task",
		Run:     runSecretTasks,
	})
	session.RegisterBenignErrorCode("120289", secretTasksOneCmd, secretTasksBatchCmd)
}

// claimHasActivity reports whether init activity lists one of ids. The client walks init activity
// as a map keyed by activity id (ActivityListDataManager.lua:100-108); an array of {id} objects is
// accepted too.
func claimHasActivity(in *Init, ids ...int64) bool {
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if acts := in.Object("activity"); acts != nil {
		for _, k := range acts.Keys() {
			if id, err := strconv.ParseInt(k, 10, 64); err == nil && want[id] {
				return true
			}
		}
	}
	for _, a := range in.Objects("activity") {
		if id, ok := claimInt(a, "id"); ok && want[id] {
			return true
		}
	}
	return false
}

func runSecretTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("secret-tasks-claim: no init push; skipping")
		return nil
	}
	if hq := claimHQLevel(in); hq < secretTasksNeedHQ || !claimHasActivity(in, secretTasksActivities...) {
		slog.Info("secret-tasks-claim: DispatchTask activity not open", "hqLevel", hq)
		return nil
	}
	list, err := session.SendAndWait(conn, "secret task list", secretTasksListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	now := claimNow(in).UnixMilli()
	var due []int64
	for _, task := range claimObjects(list.Params, "ls") {
		uuid, ok := claimInt(task, "uuid")
		done, _ := claimInt(task, "completionTime")
		rewarded, _ := claimInt(task, "rewarded")
		if ok && uuid != 0 && done > 0 && now >= done && rewarded == 0 {
			due = append(due, uuid)
		}
	}
	switch len(due) {
	case 0:
		slog.Info("secret-tasks-claim: no finished task to claim")
		return nil
	case 1:
		params := sfs.NewSFSObject()
		params.PutLong("uuid", due[0])
		_, err := claimAndLog(conn, fmt.Sprintf("secret task %d", due[0]), secretTasksOneCmd, params)
		return err
	}
	params := sfs.NewSFSObject()
	params.PutValue("uuidList", claimLongArray(due))
	label := fmt.Sprintf("secret tasks (%d)", len(due))
	msg, err := session.SendAndWait(conn, label, secretTasksBatchCmd, params)
	if err == nil && msg != nil && !msg.Params.Has("errorCode") {
		// The batch reply is array[] {reward, data, ...}, one entry per task (:13-31).
		for _, item := range claimObjects(msg.Params, "array") {
			slog.Info(label+" granted "+describeGrants(item), "cmd", secretTasksBatchCmd)
		}
	}
	return err
}
