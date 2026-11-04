package game

import (
	"fmt"
	"log/slog"
	"slices"
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
// PersonalTargetView.lua:92-96), and lists only that activity's own stage: its subType, which is
// the table's tableInfoType (ActivityInfoData.lua:231; LockhartActivityMain.lua:189-197,
// StrongestCommanderDataManager.lua:139-145, TaskActivity.lua:157-164). So an id is claimed only
// while an activity whose stage lists it is valid. An activity id newer than tables 39879 has no
// known stage and claims nothing. The first refusal that is not benign stops the run.
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

// stageQuestRange is an inclusive run of activity_stage.stage_quest ids and the stages
// (activity_stage.stage) that list them.
type stageQuestRange struct {
	lo, hi int64
	stages []int64
}

// stageQuestRanges are the claimable stage quests in tables 39879, exactly as activity_stage lists
// them (the gaps between runs are not stage quests).
var stageQuestRanges = []stageQuestRange{
	// Strongest Commander (105), stages 1 and 101.
	{2201001, 2201005, []int64{1}}, {2202001, 2202005, []int64{1}}, {2203001, 2203005, []int64{1}},
	{2204001, 2204005, []int64{1}}, {2205001, 2205005, []int64{1}}, {2206001, 2206005, []int64{1}},
	{2207001, 2207005, []int64{1}},
	{2301001, 2301005, []int64{101}}, {2302001, 2302005, []int64{101}}, {2303001, 2303005, []int64{101}},
	{2304001, 2304005, []int64{101}}, {2305001, 2305005, []int64{101}}, {2306001, 2306005, []int64{101}},
	{2307001, 2307005, []int64{101}},
	// Lockhart (106), one quest per stage.
	{2400000, 2400000, []int64{2}}, {2400001, 2400001, []int64{94011}}, {2400002, 2400002, []int64{940141}},
	{2400003, 2400003, []int64{94023}}, {2400004, 2400004, []int64{1000085}}, {2400005, 2400005, []int64{1200018}},
	{2400006, 2400006, []int64{1200067}}, {2400007, 2400007, []int64{1200126}},
	// King personal targets (200), stage 100.
	{2500001, 2500005, []int64{100}},
	// Task events (129): stage 4 (8010012-8010015 are also stage 200's, which no activity uses),
	// 98002 and 98006 (the same quests), 98003, 98501, 98505, 98509; TaskActivity2 (135): 7002.
	{8010011, 8010015, []int64{4}},
	{9010001, 9010008, []int64{98002, 98006}}, {9010101, 9010107, []int64{98003}},
	{9100000, 9100149, []int64{98501}}, {9100300, 9100449, []int64{98505}}, {9100600, 9100749, []int64{98509}},
	{9010201, 9010207, []int64{7002}},
}

// stageQuestActivityStage is every table activity row 39879 of the five types: id -> its
// tableInfoType (tableInfo activity_stage for all of them), the stage its panel lists.
var stageQuestActivityStage = map[int32]int64{
	// Strongest Commander (105)
	11: 1, 12: 1, 94016: 1,
	94022: 101,
	// Lockhart (106)
	3007: 2, 5102: 2, 94002: 2, 94003: 2, 94004: 2, 94005: 2, 94010: 2, 94014: 2, 94015: 2,
	94011: 94011, 94018: 94011, 94019: 94011, 94020: 94011, 94021: 94011,
	94023: 94023,
	94024: 940141, 940141: 940141, 940142: 940141,
	1000085: 1000085, 1200018: 1200018, 1200067: 1200067, 1200126: 1200126,
	// Task events (129)
	3008: 4, 5103: 4, 6003: 4, 6005: 4, 14002: 4, 94303: 4, 1000225: 4, 1000226: 4, 1000227: 4, 1000271: 4,
	1000272: 4, 1000288: 4, 1000290: 4, 1000292: 4, 1000294: 4, 1000312: 4, 1000509: 4, 1010235: 4, 1010236: 4,
	1010239: 4, 1010240: 4, 1020215: 4, 1030215: 4, 1041013: 4, 1051013: 4, 8129001: 4, 8129002: 4, 8129003: 4,
	8129004: 4,
	98002:   98002, 98003: 98003, 98006: 98006, 98501: 98501, 98505: 98505, 98509: 98509,
	// TaskActivity2 (135)
	7002: 7002,
	// King (200)
	85000: 100, 85001: 100, 1000008: 100, 1000020: 100, 1000042: 100, 1000057: 100, 1000076: 100, 1000077: 100,
	1000081: 100, 1000082: 100, 1000083: 100, 1000084: 100, 1000090: 100, 1200013: 100, 1200014: 100,
	1200056: 100, 1200057: 100, 1200122: 100, 1200123: 100,
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

// stageQuestStages returns the stages that list id, or false when id is not a claimable stage
// quest.
func stageQuestStages(id string) ([]int64, bool) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, false
	}
	for _, r := range stageQuestRanges {
		if n >= r.lo && n <= r.hi {
			return r.stages, true
		}
	}
	return nil, false
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
	valid := map[int64]bool{} // the stages of the valid activities
	for _, a := range Activities(conn, in).All(types...) {
		if !stageQuestActivityValid(a, now, hq) {
			continue
		}
		stage, ok := stageQuestActivityStage[a.ID]
		if !ok {
			slog.Info(stageQuestsFeature+": running activity not in tables 39879; its stage quests wait for a table update",
				"activityId", a.ID, "activityType", a.Type)
			continue
		}
		valid[stage] = true
	}

	b := &evBatch{conn: conn}
	seen := map[string]bool{}
	idle, claimed, logged := 0, 0, false // idle: claimable quests whose stage no valid activity lists
	for _, t := range in.Objects("task") {
		id := questID(t)
		if st, _ := evNum(t, "state"); st != questCanReceive || id == "" || seen[id] {
			continue
		}
		stages, ok := stageQuestStages(id)
		if !ok {
			continue
		}
		seen[id] = true
		if !slices.ContainsFunc(stages, func(s int64) bool { return valid[s] }) {
			idle++
			continue
		}
		if claimed >= questMaxClaims {
			slog.Warn(stageQuestsFeature+": claim cap reached; leaving the rest for the next run", "cap", questMaxClaims)
			break
		}
		claimed++
		p := sfs.NewSFSObject()
		p.PutUtfString("id", id)
		msg := b.send(fmt.Sprintf("stage quest reward %s", id), questRewardCmd, p)
		if msg == nil {
			// A refusal that is not benign (or a dead connection): these claims are not validated
			// live, so one refusal stops the run rather than repeating it for every due quest.
			slog.Warn(stageQuestsFeature+": claim refused; stopping", "id", id, "claimed", claimed-1)
			break
		}
		if !logged {
			logged = true
			slog.Info(stageQuestsFeature+": first "+questRewardCmd+" reply", "id", id, "stages", fmt.Sprint(stages),
				"response", msg.Params.StringRedacted())
		}
	}
	if idle > 0 {
		slog.Info(stageQuestsFeature+": claimable stage quests left alone, no running activity lists their stage", "count", idle)
	}
	if claimed == 0 {
		slog.Info(stageQuestsFeature + ": no claimable stage quest of a running activity")
	}
	return b.err()
}
