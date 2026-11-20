package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Secret Task assists (MASTER G10; cov-core/F6, cov-ui-activity/F7, cov-ui-military/F2).
// secret-task-assist helps the allies' finished Secret Tasks the way the alliance tab of the Secret
// Task page does; secret-task-assist-plan only reads and logs what it would assist.
//
// The client: the alliance tab's list is `hero.dispatch.alliance.list {}`, sent only in an alliance
// with the DispatchTask activity open (GetAllAllianceTasksFromServer, ActDispatchTaskDataManager.lua:
// 423-435); its reply ls[] (DispatchGetAllianceTasksMessage.lua:9-17) is stored by uuid with its
// cfgId's lw_dispatch_tasks row (UpdateOneAllianceTask, :550-562). A row is drawn only with that
// table row present (DispatchTaskItem.lua:252-253); on the alliance tab its button is hidden only
// while completionTime > now (:334, 367-378), and it sends `hero.dispatch.assist {uuid: Long,
// targetServer: Int}` with the entry's own targetServer while todayAssistNum < aid_count (OnGoClick,
// :483-490; DispatchAssistMessage.lua:5-9), else tip 456225. aid_count is lw_dispatch_settings[1000 +
// starLevel].aid_count, row 1001 as the fallback (GetDispatchSetting, ActDispatchTaskDataManager.lua:
// 66-73): 5 in all 123 rows (tables 40413). todayAssistNum comes with the own `hero.dispatch.list`
// reply and with every assist reply (UpdateTodayNum, :400-412; DispatchAssistMessage.lua:22). The
// assist reply adds reward to the helper, the task's lw_dispatch_tasks aid_extra_reward
// (DispatchTaskItem.lua:327-332), and drops the task from the list (DispatchAssistMessage.lua:17-27);
// any errorCode is only shown as a tip (:13-15); 456225 is the tip of the client's own cap check.
//
// secret-task-assist sends that, with the map's stricter gate (UIWorldPointBtn.lua:2745-2756:
// completionTime > 0, now >= completionTime, rewarded == 0) and fail-closed field checks: an entry
// is assisted only with uuid, cfgId (a lw_dispatch_tasks row), completionTime, rewarded and an Int
// targetServer, the fields the client's parser and the tab read, all present; an own list without
// todayAssistNum assists nothing. An entry is another owner's unless its uuid is in the own list's
// ls[] or its ownerId (a field of the live own-list entries, docs/live-validation.mdx) is the
// player's uid. At most 5 - todayAssistNum (the own list's) assists a run, whatever the assist
// replies' todayAssistNum says (it can only end the run earlier), about 1 s apart, colour first
// (UR before SSR before SR) and then the earliest finished: the colour and completionTime keys of
// the tab's SortTasks (ActDispatchTaskDataManager.lua:448-497), without its is_special and
// task_star keys.
// The first errorCode stops the run with an error; no code is registered as benign.
//
// Never sent: hero.dispatch.steal (the map's action on another alliance's task, UIWorldPointBtn.lua:
// 2757-2768), hero.dispatch.start or anything else that dispatches the player's heroes.
//
// Alliance Duel: an assist scores in no duel row, so neither feature declares Duel. The six theme
// rows (heroevent 110000-110005, score and condition_score, tables 40413) list 86 score ids; the
// only Secret Task one is 90204, type 99 value 5 (task colour UR), "Perform a UR secret task", on
// Base Expansion and Enemy Buster. score's other type-99 rows, 80027 and 800027, belong to other
// events, and no score type is an assist. 90204's gotype2 93 opens the Secret Task window
// (QuestGoType.OpenUIDispatchTaskMain, GoToUtil.lua:779-780), and no Secret Task file reads the
// duel. Static-only: a push.act.score.obtain logged after hero.dispatch.assist on a type-99 day
// would show otherwise.
const (
	secretTaskAssistPlanFeature = "secret-task-assist-plan"
	secretTaskAssistFeature     = "secret-task-assist"
	secretTaskAllianceListCmd   = "hero.dispatch.alliance.list"
	secretTaskAssistCmd         = "hero.dispatch.assist"
	secretTaskAssistDailyCap    = 5 // lw_dispatch_settings aid_count, every row (tables 40413)
)

// secretTaskAssistPace is the gap between assists, about a person tapping one row after another.
var secretTaskAssistPace = time.Second

// secretTaskAssistSleep is time.Sleep; tests replace it.
var secretTaskAssistSleep = time.Sleep

func init() {
	registerFeature(Feature{
		Name: secretTaskAssistPlanFeature,
		Summary: "read-only: log the allies' finished Secret Tasks the client would offer to assist and today's assists; " +
			"sends only hero.dispatch.list and hero.dispatch.alliance.list, never an assist (static-only, not validated live)",
		DefaultOn: false,
		Run:       runSecretTaskAssistPlan,
	})
	registerFeature(Feature{
		Name: secretTaskAssistFeature,
		Summary: "OPT-IN: assist alliance members' finished Secret Tasks from the alliance tab (hero.dispatch.assist, free, " +
			"at most 5 a day); the helped ally sees the assist; never steals and never dispatches heroes " +
			"(static-only, not validated live)",
		DefaultOn: false,
		Run:       runSecretTaskAssist,
	})
}

// secretTaskAssistable is the map's assist gate on one alliance list entry (UIWorldPointBtn.lua:
// 2745-2756): finished and not rewarded. secretTaskAssistTargets adds the field and owner checks.
func secretTaskAssistable(task *sfs.SFSObject, nowMs int64) bool {
	done, _ := claimInt(task, "completionTime")
	rewarded, _ := claimInt(task, "rewarded")
	return done > 0 && nowMs >= done && rewarded == 0
}

// secretTaskAssistTarget is one alliance list entry the run may assist.
type secretTaskAssistTarget struct {
	uuid, cfgID, completion, color int64
	server                         int32
}

// secretTaskAssistFields are the ls[] fields an assist needs (UpdateOneAllianceTask,
// ActDispatchTaskDataManager.lua:550-557; DispatchTaskItem.lua:342-372, 487).
var secretTaskAssistFields = []string{"uuid", "cfgId", "completionTime", "rewarded", "targetServer"}

// secretTaskAssistTargets returns the entries of ls to assist at nowMs, in assist order, and how many
// were left out for each reason. own holds the uuids of the player's own tasks, uid the player's uid
// ("" when unknown).
func secretTaskAssistTargets(ls []*sfs.SFSObject, own map[int64]bool, uid string, nowMs int64) ([]secretTaskAssistTarget, map[string]int) {
	skipped := map[string]int{}
	seen := map[int64]bool{}
	var out []secretTaskAssistTarget
	for _, e := range ls {
		missing := false
		for _, k := range secretTaskAssistFields {
			if _, ok := claimInt(e, k); !ok {
				missing = true
			}
		}
		uuid, _ := claimInt(e, "uuid")
		cfgID, _ := claimInt(e, "cfgId")
		server, _ := claimInt(e, "targetServer")
		cfg, known := secretTaskStartsCfgs[cfgID]
		switch {
		case missing || uuid == 0 || server <= 0 || server > math.MaxInt32:
			skipped["missingField"]++
		case !known:
			skipped["unknownCfgId"]++
		case own[uuid] || (uid != "" && idString(e, "ownerId") == uid):
			skipped["own"]++
		case !secretTaskAssistable(e, nowMs):
			skipped["notFinishedOrRewarded"]++
		case seen[uuid]:
			skipped["duplicate"]++
		default:
			seen[uuid] = true
			done, _ := claimInt(e, "completionTime")
			out = append(out, secretTaskAssistTarget{uuid: uuid, cfgID: cfgID, completion: done, color: cfg.color,
				server: int32(server)})
		}
	}
	slices.SortStableFunc(out, func(a, b secretTaskAssistTarget) int {
		return cmp.Or(cmp.Compare(b.color, a.color), cmp.Compare(a.completion, b.completion))
	})
	return out, skipped
}

// secretTaskAssistState is what both features read: today's assists and the alliance list.
type secretTaskAssistState struct {
	today      int64
	todayKnown bool
	own        map[int64]bool
	tasks      []*sfs.SFSObject
}

// secretTaskAssistRead gates on the alliance and the DispatchTask activity and reads the own and the
// alliance task lists. A nil state with a nil error means the gate is closed.
func secretTaskAssistRead(conn *session.GameConn, in *Init, name string) (*secretTaskAssistState, error) {
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil, nil
	}
	if !claimInAlliance(in) {
		slog.Info(name + ": not in an alliance")
		return nil, nil
	}
	if hq := claimHQLevel(in); hq < secretTasksNeedHQ || !claimHasActivity(in, secretTasksActivities...) {
		slog.Info(name+": DispatchTask activity not open", "hqLevel", hq)
		return nil, nil
	}
	ownList, err := session.SendAndWait(conn, name+": own task list", secretTasksListCmd, sfs.NewSFSObject())
	if err != nil {
		return nil, err
	}
	st := &secretTaskAssistState{own: map[int64]bool{}}
	st.today, st.todayKnown = claimInt(ownList.Params, "todayAssistNum")
	for _, t := range claimObjects(ownList.Params, "ls") {
		if uuid, ok := claimInt(t, "uuid"); ok {
			st.own[uuid] = true
		}
	}
	list, err := session.SendAndWait(conn, name+": alliance task list", secretTaskAllianceListCmd, sfs.NewSFSObject())
	if err != nil {
		return nil, err
	}
	st.tasks = claimObjects(list.Params, "ls")
	return st, nil
}

// secretTaskAssistEntryFields is the sorted field names of the first list entry, for the logs
// (MASTER G10: the ls[] fields are unconfirmed live).
func secretTaskAssistEntryFields(tasks []*sfs.SFSObject) []string {
	if len(tasks) == 0 {
		return nil
	}
	return slices.Sorted(slices.Values(tasks[0].Keys()))
}

func runSecretTaskAssistPlan(conn *session.GameConn, in *Init) error {
	const name = secretTaskAssistPlanFeature
	st, err := secretTaskAssistRead(conn, in, name)
	if err != nil || st == nil {
		return err
	}
	now := claimNow(in).UnixMilli()
	var n int64
	for _, t := range st.tasks {
		if secretTaskAssistable(t, now) {
			n++
		}
	}
	targets, skipped := secretTaskAssistTargets(st.tasks, st.own, ownUID(in), now)
	would := 0
	if st.todayKnown {
		would = int(max(0, min(int64(len(targets)), secretTaskAssistDailyCap-st.today)))
	}
	slog.Info(name+": alliance Secret Tasks", "listed", len(st.tasks), "finishedUnrewarded", n,
		"todayAssistNum", st.today, "todayAssistNumInReply", st.todayKnown, "dailyCap", secretTaskAssistDailyCap,
		"eligible", len(targets), "skipped", skipped, "wouldAssist", would,
		"entryFields", secretTaskAssistEntryFields(st.tasks))
	return nil
}

func runSecretTaskAssist(conn *session.GameConn, in *Init) error {
	const name = secretTaskAssistFeature
	st, err := secretTaskAssistRead(conn, in, name)
	if err != nil || st == nil {
		return err
	}
	if !st.todayKnown {
		slog.Warn(name + ": the own task list has no todayAssistNum; not assisting")
		return nil
	}
	targets, skipped := secretTaskAssistTargets(st.tasks, st.own, ownUID(in), claimNow(in).UnixMilli())
	if skipped["missingField"] > 0 {
		slog.Warn(name+": alliance tasks without a field the assist needs were skipped", "count", skipped["missingField"],
			"need", secretTaskAssistFields, "entryFields", secretTaskAssistEntryFields(st.tasks))
	}
	slog.Info(name+": alliance Secret Tasks", "listed", len(st.tasks), "eligible", len(targets), "skipped", skipped,
		"todayAssistNum", st.today, "dailyCap", secretTaskAssistDailyCap)
	switch {
	case st.today >= secretTaskAssistDailyCap:
		slog.Info(name + ": today's assists are used up")
		return nil
	case len(targets) == 0:
		slog.Info(name + ": no ally task to assist")
		return nil
	}

	// budget bounds the run by the own list's count whatever the replies say; a reply's
	// todayAssistNum can only end it earlier.
	budget := secretTaskAssistDailyCap - st.today
	var assisted int64
	today := st.today
	for i, t := range targets {
		if assisted >= budget || today >= secretTaskAssistDailyCap {
			break
		}
		if i > 0 {
			secretTaskAssistSleep(secretTaskAssistPace)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", t.uuid)
		p.PutInt("targetServer", t.server)
		label := fmt.Sprintf("%s: assist %d", name, t.uuid)
		msg, err := session.SendAndWait(conn, label, secretTaskAssistCmd, p)
		var resp *sfs.SFSObject
		if msg != nil {
			resp = msg.Params
		}
		if i == 0 && resp != nil {
			code, _ := claimString(resp, "errorCode")
			n, hasN := claimInt(resp, "todayAssistNum")
			slog.Info(name+": first "+secretTaskAssistCmd+" reply", "errorCode", code, "todayAssistNum", n,
				"hasTodayAssistNum", hasN, "uuid", idString(resp, "uuid"), "sentUuid", t.uuid, "cfgId", t.cfgID,
				"targetServer", t.server, "hasReward", resp.Has("reward"), "replyKeys", resp.Keys())
		}
		if err != nil {
			return errors.Join(err, fmt.Errorf("%s: stopped after %d of %d assists", name, assisted, len(targets)))
		}
		assisted++
		if n, ok := claimInt(resp, "todayAssistNum"); ok {
			today = n
		} else {
			today++
		}
		slog.Info(name+": assisted an ally's Secret Task", "uuid", t.uuid, "cfgId", t.cfgID,
			"granted", describeGrants(resp), "todayAssistNum", today)
	}
	slog.Info(name+": assists done", "assisted", assisted, "eligible", len(targets), "todayAssistNum", today)
	return nil
}
