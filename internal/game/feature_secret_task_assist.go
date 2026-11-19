package game

import (
	"log/slog"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Secret Task assists (MASTER G10), read-only half: secret-task-assist-plan logs what the
// client would offer to assist; it sends no assist.
//
// The client helps an ally's finished Secret Task with `hero.dispatch.assist {uuid: Long,
// targetServer: Int}` (DispatchAssistMessage.lua:5-9), up to lw_dispatch_settings aid_count a day
// (5 in all 123 rows, tables 40413; GetDispatchSetting, ActDispatchTaskDataManager.lua:66-73),
// from the map (UIWorldPointBtn.lua:2745-2756: completionTime > 0, now >= completionTime,
// rewarded == 0, same alliance, todayAssistNum < aid_count) or from the alliance tab of the Secret
// Task page (DispatchTaskItem.lua:482-491), whose list is `hero.dispatch.alliance.list {}`, sent
// only in an alliance with the DispatchTask activity open (GetAllAllianceTasksFromServer,
// ActDispatchTaskDataManager.lua:423-435), reply ls[] (DispatchGetAllianceTasksMessage.lua:9-17).
// todayAssistNum comes with the own hero.dispatch.list reply (UpdateTodayNum, :400-412). Whether an
// assist, a friendly action the helped ally sees, is an outward action and whether it scores in
// the Alliance Duel is the user's decision (MASTER §6.1), so this file has no assist command. The
// plan reads both lists and logs the count the map gate would offer, today's assists, and the
// field names of a list entry (MASTER G10: the ls[] fields need a capture). Static-only, not
// validated live.
const (
	secretTaskAssistPlanFeature = "secret-task-assist-plan"
	secretTaskAllianceListCmd   = "hero.dispatch.alliance.list"
	secretTaskAssistDailyCap    = 5 // lw_dispatch_settings aid_count, every row (tables 40413)
)

func init() {
	registerFeature(Feature{
		Name: secretTaskAssistPlanFeature,
		Summary: "read-only: log the allies' finished Secret Tasks the client would offer to assist and today's assists; " +
			"sends only hero.dispatch.list and hero.dispatch.alliance.list, never an assist (static-only, not validated live)",
		DefaultOn: false,
		Run:       runSecretTaskAssistPlan,
	})
}

// secretTaskAssistable is the map's assist gate on one alliance list entry (UIWorldPointBtn.lua:
// 2745-2756): finished and not rewarded. The owner is not tested: the entry's owner field is not
// known yet.
func secretTaskAssistable(task *sfs.SFSObject, nowMs int64) bool {
	done, _ := claimInt(task, "completionTime")
	rewarded, _ := claimInt(task, "rewarded")
	return done > 0 && nowMs >= done && rewarded == 0
}

func runSecretTaskAssistPlan(conn *session.GameConn, in *Init) error {
	const name = secretTaskAssistPlanFeature
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info(name + ": not in an alliance")
		return nil
	}
	if hq := claimHQLevel(in); hq < secretTasksNeedHQ || !claimHasActivity(in, secretTasksActivities...) {
		slog.Info(name+": DispatchTask activity not open", "hqLevel", hq)
		return nil
	}
	own, err := session.SendAndWait(conn, name+": own task list", secretTasksListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	today, todayKnown := claimInt(own.Params, "todayAssistNum")
	list, err := session.SendAndWait(conn, name+": alliance task list", secretTaskAllianceListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	tasks := claimObjects(list.Params, "ls")
	now := claimNow(in).UnixMilli()
	var n int64
	for _, t := range tasks {
		if secretTaskAssistable(t, now) {
			n++
		}
	}
	var keys []string
	if len(tasks) > 0 {
		keys = slices.Sorted(slices.Values(tasks[0].Keys()))
	}
	slog.Info(name+": alliance Secret Tasks", "listed", len(tasks), "finishedUnrewarded", n,
		"todayAssistNum", today, "todayAssistNumInReply", todayKnown, "dailyCap", secretTaskAssistDailyCap,
		"wouldAssist", max(0, min(n, secretTaskAssistDailyCap-today)), "entryFields", keys)
	return nil
}
