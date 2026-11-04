package game

import (
	"fmt"
	"log/slog"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Activity-stage quests (OPT-IN, MASTER.md §3.2 #13 STAGEQ; cov-ui-activity F3 and its verdict).
// Static-only: from the 833 Lua and tables 39879, not sent live yet.
//
// The quests feature leaves every activity_stage.stage_quest id to its activity
// (questStageRanges, feature_quests.go). The free stage events claim theirs with the same
// task.reward.get {id: UtfString} when the quest's TaskManager state is 1 (TaskState.CanReceive),
// with no purchase gate: Strongest Commander (UIStageMissionItem.lua:20-26 via
// UIActivityCenterTableCtrl.lua:177-179), task events (TaskActivityItem.lua:170-176), Lockhart
// (LockhartActivityMain.lua:186-199, 282-284) and the King's personal targets
// (PersonalTargetItem.lua:118-122). TaskManager holds init task[] {id, num, state}
// (TaskManager.lua:27-35), so the state is already in init and no detail request is needed.
//
// Each sending UI is reachable only while its activity is valid (ActivityInfoData:GetValidType,
// ActivityInfoData.lua:455-493; the personal-target view closes when the King activity is not,
// PersonalTargetView.lua:92-96), so an id is claimed only while an activity of its type is.
//
// Left out on purpose: Growth Foundation (127, stage types 3 and 5: 8000001-8000010,
// 8000101-8000112) pays only after the foundation was bought (UILWGrowFoundationTargetItem.lua:
// 131-136); Hero Trial (133, stage types 6-27: 8010101-8010108, 8010201-8010208, 8010301-8010344)
// claims its stages with hero.experiment.reward, never task.reward.get (UIHeroTrial.lua:447-453);
// stage type 200's 8010018 has no activity row. Follow-up quests in a reply's tasks[] are
// not chased: those belong to the quests feature.
const stageQuestsFeature = "stage-quests"

// The activity types (EnumActivity, Global/EnumType.lua:6290-6343) whose stage quests are claimed.
const (
	stageQuestTypeStrongestCommander int32 = 105
	stageQuestTypeLockhart           int32 = 106
	stageQuestTypeTaskActivity       int32 = 129
	stageQuestTypeTaskActivity2      int32 = 135
	stageQuestTypeKing               int32 = 200
)

// stageQuestRange is an inclusive run of activity_stage.stage_quest ids and the type of the
// activities that list them (table activity: tableInfo activity_stage, tableInfoType = the stage).
type stageQuestRange struct {
	lo, hi  int64
	actType int32
}

// stageQuestRanges are the claimable stage quests in tables 39879, exactly as activity_stage lists
// them (the gaps between runs are not stage quests).
var stageQuestRanges = []stageQuestRange{
	// Strongest Commander, stage types 1 and 101.
	{2201001, 2201005, stageQuestTypeStrongestCommander}, {2202001, 2202005, stageQuestTypeStrongestCommander},
	{2203001, 2203005, stageQuestTypeStrongestCommander}, {2204001, 2204005, stageQuestTypeStrongestCommander},
	{2205001, 2205005, stageQuestTypeStrongestCommander}, {2206001, 2206005, stageQuestTypeStrongestCommander},
	{2207001, 2207005, stageQuestTypeStrongestCommander},
	{2301001, 2301005, stageQuestTypeStrongestCommander}, {2302001, 2302005, stageQuestTypeStrongestCommander},
	{2303001, 2303005, stageQuestTypeStrongestCommander}, {2304001, 2304005, stageQuestTypeStrongestCommander},
	{2305001, 2305005, stageQuestTypeStrongestCommander}, {2306001, 2306005, stageQuestTypeStrongestCommander},
	{2307001, 2307005, stageQuestTypeStrongestCommander},
	// Lockhart, stage types 2, 94011, 940141, 94023, 1000085, 1200018, 1200067, 1200126.
	{2400000, 2400007, stageQuestTypeLockhart},
	// King personal targets, stage type 100.
	{2500001, 2500005, stageQuestTypeKing},
	// Task events: stage types 4 (8010012-8010015 are also stage type 200's), 98002/98006, 98003,
	// 98501, 98505, 98509; and 7002 for TaskActivity2.
	{8010011, 8010015, stageQuestTypeTaskActivity},
	{9010001, 9010008, stageQuestTypeTaskActivity}, {9010101, 9010107, stageQuestTypeTaskActivity},
	{9100000, 9100149, stageQuestTypeTaskActivity}, {9100300, 9100449, stageQuestTypeTaskActivity},
	{9100600, 9100749, stageQuestTypeTaskActivity},
	{9010201, 9010207, stageQuestTypeTaskActivity2},
}

func init() {
	registerFeature(Feature{
		Name: stageQuestsFeature,
		Summary: "OPT-IN: claim reached free activity-stage quests (Strongest Commander, Lockhart, task events, " +
			"King personal targets) with task.reward.get while their activity runs; never Growth Foundation or " +
			"Hero Trial (static-only, not validated live)",
		Run: runStageQuests,
	})
	// No benign code of its own: task.reward.get's evAlreadyExecuted is registered by the quests
	// feature for the same command.
}

// stageQuestType returns the activity type whose stage lists id, or false when id is not a
// claimable stage quest.
func stageQuestType(id string) (int32, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return 0, false
	}
	for _, r := range stageQuestRanges {
		if n >= r.lo && n <= r.hi {
			return r.actType, true
		}
	}
	return 0, false
}

// stageQuestActivityValid is ActivityInfoData:GetValidType == Now for these types: the HQ reaches
// needMainCityLevel and now is within the show window, from readyTime (else startTime) to
// endViewTime (else rewardTime, else endTime) (ActivityInfoData.lua:436-453, 455-493).
func stageQuestActivityValid(a Activity, nowMs int64, hq int32) bool {
	start := a.StartTime()
	if t, _ := evNum(a.Raw, "readyTime"); t > 0 {
		start = t
	}
	end := a.EndTime()
	if t, _ := evNum(a.Raw, "rewardTime"); t > 0 {
		end = t
	}
	if t := a.EndViewTime(); t > 0 {
		end = t
	}
	return hq >= a.NeedHQ && nowMs >= start && nowMs <= end
}

func runStageQuests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(stageQuestsFeature + ": no init push; skipping")
		return nil
	}
	types := []int32{stageQuestTypeStrongestCommander, stageQuestTypeLockhart, stageQuestTypeTaskActivity,
		stageQuestTypeTaskActivity2, stageQuestTypeKing}
	now, hq := evNow().UnixMilli(), activityHQLevel(in)
	valid := map[int32]bool{}
	for _, a := range Activities(conn, in).All(types...) {
		if stageQuestActivityValid(a, now, hq) {
			valid[a.Type] = true
		}
	}

	b := &evBatch{conn: conn}
	seen := map[string]bool{}
	idle := map[int32]int{} // claimable quests whose activity type is not running
	claimed, logged := 0, false
	for _, t := range in.Objects("task") {
		id := questID(t)
		if st, _ := evNum(t, "state"); st != questCanReceive || id == "" || seen[id] {
			continue
		}
		typ, ok := stageQuestType(id)
		if !ok {
			continue
		}
		seen[id] = true
		if !valid[typ] {
			idle[typ]++
			continue
		}
		if b.dead {
			break
		}
		if claimed >= questMaxClaims {
			slog.Warn(stageQuestsFeature+": claim cap reached; leaving the rest for the next run", "cap", questMaxClaims)
			break
		}
		claimed++
		p := sfs.NewSFSObject()
		p.PutUtfString("id", id)
		msg := b.send(fmt.Sprintf("stage quest reward %s", id), questRewardCmd, p)
		if msg != nil && !logged {
			logged = true
			slog.Info(stageQuestsFeature+": first "+questRewardCmd+" reply", "id", id, "activityType", typ, "response", msg.Params.StringRedacted())
		}
	}
	for _, typ := range types {
		if n := idle[typ]; n > 0 {
			slog.Info(stageQuestsFeature+": claimable stage quests left alone, no activity of their type is running", "activityType", typ, "count", n)
		}
	}
	if claimed == 0 {
		slog.Info(stageQuestsFeature + ": no claimable stage quest of a running activity")
	}
	return b.err()
}
