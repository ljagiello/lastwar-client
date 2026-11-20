package game

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The hand-copied half of cov-tables/F14 (MASTER B73 part 3): tables_drift_test.go reruns the
// generators, and these tests check the lists that still copy a table by hand after the 1.0.370
// fixes. The lists the fixes turned into lookups (seasonMilitaryNormalDays, seasonWeekCards,
// treasureHuntV1SubTypes, the dig activity lists, battlePassScoreItems, survivorActivityConfig,
// secretTaskStartsHeroQuality, duelRecruitWishLists) need no check.

// TestHandCopiedActivityListsMatchTheTable: each list copied from table activity must equal what
// the generated activityTable says, so a regeneration that adds, drops or moves a row fails here
// instead of stranding the feature on the old rows.
func TestHandCopiedActivityListsMatchTheTable(t *testing.T) {
	rowsOf := func(types ...int32) map[int32]activityRow {
		out := map[int32]activityRow{}
		for id, r := range activityTable {
			if slices.Contains(types, r.typ) {
				out[id] = r
			}
		}
		return out
	}

	// stageQuestActivityStage: every row of the five stage-quest types -> its tableInfoType
	// (ActivityInfoData.lua:231), the activity_stage row its panel lists.
	stageRows := rowsOf(stageQuestTypeStrongestCommander, stageQuestTypeLockhart, stageQuestTypeTaskActivity,
		stageQuestTypeTaskActivity2, stageQuestTypeKing)
	for id, r := range stageRows {
		if got, ok := stageQuestActivityStage[id]; !ok || got != int64(r.subType) {
			t.Errorf("stageQuestActivityStage[%d] = %d, %v; table activity tableInfoType %d", id, got, ok, r.subType)
		}
	}
	if len(stageQuestActivityStage) != len(stageRows) {
		t.Errorf("stageQuestActivityStage lists %d ids, the table %d rows of the five types", len(stageQuestActivityStage), len(stageRows))
	}

	// torchRelayActivityRows: every type-309 row -> its tableInfoType, an activity_torch_relay row
	// that torchRelayRows carries.
	torchRows := rowsOf(actTypeTorchRelay)
	for id, r := range torchRows {
		if got, ok := torchRelayActivityRows[id]; !ok || got != r.subType {
			t.Errorf("torchRelayActivityRows[%d] = %d, %v; table activity tableInfoType %d", id, got, ok, r.subType)
		}
		if _, ok := torchRelayRows[r.subType]; !ok {
			t.Errorf("activity %d reads activity_torch_relay row %d, which torchRelayRows lacks", id, r.subType)
		}
	}
	if len(torchRelayActivityRows) != len(torchRows) {
		t.Errorf("torchRelayActivityRows lists %d ids, the table %d type-309 rows", len(torchRelayActivityRows), len(torchRows))
	}

	// battlePassLoginPasses: the BattlePass (27) and BattlePass_new (142) rows whose showUiType is
	// a login pass view: 2 SevenDayLogin, 6 NewYearSevenDayLogin, 9 ValentineLogin.
	login := map[int32]bool{}
	for id, r := range rowsOf(actTypeBattlePass, actTypeBattlePassNew) {
		if r.showUIType == 2 || r.showUIType == 6 || r.showUIType == 9 {
			login[id] = true
		}
	}
	if !maps.Equal(login, battlePassLoginPasses) {
		t.Errorf("battlePassLoginPasses = %v, table activity's login-view passes %v", battlePassLoginPasses, login)
	}

	// armsRaceGroups: the type-125 rows with a group -> group_number, group_priority (:408-409).
	groups := map[int32][2]int32{}
	for id, r := range rowsOf(125) {
		if r.groupNumber != 0 || r.groupPriority != 0 {
			groups[id] = [2]int32{r.groupNumber, r.groupPriority}
		}
	}
	if !maps.Equal(groups, armsRaceGroups) {
		t.Errorf("armsRaceGroups = %v, table activity's type-125 groups %v", armsRaceGroups, groups)
	}

	// secretTasksActivities: the DispatchTask (108) rows, each at secretTasksNeedHQ.
	var dispatch []int64
	for id, r := range rowsOf(108) {
		dispatch = append(dispatch, int64(id))
		if r.needHQ != secretTasksNeedHQ {
			t.Errorf("activity %d (type 108) needs HQ %d, secretTasksNeedHQ is %d", id, r.needHQ, secretTasksNeedHQ)
		}
	}
	slices.Sort(dispatch)
	if !slices.Equal(dispatch, slices.Sorted(slices.Values(secretTasksActivities))) {
		t.Errorf("secretTasksActivities = %v, table activity's type-108 rows %v", secretTasksActivities, dispatch)
	}
}

// TestHandCopiedListsMatchTheDump checks the lists copied from other tables against the dump in
// LASTWAR_TABLES (the model is coverage/work/cov-tables/check2.py and check3.py). Skipped without
// it. Lists copied from Lua constants (railwayTruckStations, allianceDuelUnlocks, seasonMummyYards)
// have no table to check against.
func TestHandCopiedListsMatchTheDump(t *testing.T) {
	num := func(v any) int64 {
		n, err := strconv.ParseFloat(eventDumpScalar(v), 64)
		if err != nil {
			return -1
		}
		return int64(n)
	}
	key := func(id string) int64 {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			t.Fatalf("table key %q", id)
		}
		return n
	}

	t.Run("dailyQuestsPoints", func(t *testing.T) {
		points, hidden := map[string]int64{}, map[string]bool{}
		for id, r := range eventTableDump(t, "daily_quest") {
			points[id] = num(r["point"])
			if num(r["show"]) != 1 {
				hidden[id] = true
			}
		}
		if !maps.Equal(points, dailyQuestsPoints) {
			t.Errorf("dailyQuestsPoints = %v, daily_quest point %v", dailyQuestsPoints, points)
		}
		if !maps.Equal(hidden, dailyQuestsHidden) {
			t.Errorf("dailyQuestsHidden = %v, daily_quest rows with show != 1 %v", dailyQuestsHidden, hidden)
		}
	})

	t.Run("zoneMobBossHP", func(t *testing.T) {
		hp := map[int64]int64{}
		for id, r := range eventTableDump(t, "zone_mobilization_boss") {
			hp[key(id)] = num(r["rallyboss_hp"])
		}
		if !maps.Equal(hp, zoneMobBossHP) {
			t.Errorf("zoneMobBossHP = %v, zone_mobilization_boss rallyboss_hp %v", zoneMobBossHP, hp)
		}
	})

	t.Run("winterStormBoxes", func(t *testing.T) {
		boxes := map[int64]int64{}
		for id, r := range eventTableDump(t, "winter_battlefield_reward_boxes") {
			if num(r["type"]) == 1 {
				boxes[key(id)] = num(r["para"])
			}
		}
		if !maps.Equal(boxes, winterStormBoxes) {
			t.Errorf("winterStormBoxes = %v, winter_battlefield_reward_boxes type-1 para %v", winterStormBoxes, boxes)
		}
	})

	t.Run("railwayTruckRanges", func(t *testing.T) {
		quality := map[int64]int64{}
		for id, r := range eventTableDump(t, "train_property") {
			if num(r["type"]) == 1 {
				quality[key(id)] = num(r["quality"])
			}
		}
		for id, q := range quality {
			want := int32(0)
			if q == 5 || q == 10 {
				want = int32(q)
			}
			if got := railwayTruckQuality(id); got != want {
				t.Errorf("railwayTruckQuality(%d) = %d, train_property quality %d", id, got, q)
			}
		}
		for _, rs := range [][][2]int64{railwayURRanges, railwaySleighRanges} {
			for _, r := range rs {
				for id := r[0]; id <= r[1]; id++ {
					if _, ok := quality[id]; !ok {
						t.Errorf("truck %d is in a hand-copied range but no type-1 train_property row", id)
					}
				}
			}
		}
	})

	// truck-reroll's "free" rests on train_para row 9, its high-goods skip on row 55 and its
	// quality line on the type-1 train_property rows of quality 1-4 (fu-f-r1 minor 1).
	t.Run("truckRerollTables", func(t *testing.T) {
		quality := map[int64]int64{}
		for id, r := range eventTableDump(t, "train_property") {
			if num(r["type"]) == 1 {
				quality[key(id)] = num(r["quality"])
			}
		}
		for id, q := range quality {
			want := int32(0)
			if (q >= 1 && q <= 5) || q == 10 {
				want = int32(q)
			}
			if got := truckQuality(id); got != want {
				t.Errorf("truckQuality(%d) = %d, train_property quality %d", id, got, q)
			}
		}
		for q, rs := range truckRerollQualityRanges {
			for _, r := range rs {
				for id := r[0]; id <= r[1]; id++ {
					if got, ok := quality[id]; !ok || got != int64(q) {
						t.Errorf("truck %d is in truckRerollQualityRanges[%d], type-1 train_property quality %d (row present %v)", id, q, got, ok)
					}
				}
			}
		}
		para := eventTableDump(t, "train_para")
		if got := eventDumpScalar(para["9"]["val"]); got != truckRerollChangeCost {
			t.Errorf("truckRerollChangeCost = %q, train_para 9 %q", truckRerollChangeCost, got)
		}
		if got := eventDumpScalar(para["55"]["val"]); got != truckRerollHighGoods {
			t.Errorf("truckRerollHighGoods = %q, train_para 55 %q", truckRerollHighGoods, got)
		}
	})

	t.Run("duelCampCapacity", func(t *testing.T) {
		b := eventTableDump(t, "building")
		for lv := 1; lv < len(duelCampCapacity); lv++ {
			r, ok := b[strconv.Itoa(10103000+lv)]
			if got := num(r["para1"]); !ok || got != duelCampCapacity[lv] {
				t.Errorf("duelCampCapacity[%d] = %d, building %d para1 %d", lv, duelCampCapacity[lv], 10103000+lv, got)
			}
		}
		if _, ok := b[strconv.Itoa(10103000+len(duelCampCapacity))]; ok {
			t.Errorf("building has camp level %d, past duelCampCapacity", len(duelCampCapacity))
		}
	})

	t.Run("duelUnits", func(t *testing.T) {
		s := eventTableDump(t, "lw_soldier")
		aps := eventTableDump(t, "APS_science")
		for _, u := range duelUnits {
			r, ok := s[strconv.Itoa(int(u.id))]
			if !ok {
				t.Errorf("duelUnits %d: no lw_soldier row", u.id)
				continue
			}
			cost, _ := r["train_cost_consume"].(map[string]any)
			need := eventDumpScalar(r["train_need_science"])
			got := duelUnit{id: u.id, tier: int32(num(r["soldier_lv"])), camp: num(r["train_need_barrack"]),
				science: need != "", t11: eventDumpScalar(r["train_need_buff"]) == duelTrainT11Buff,
				iron: num(cost["1"]), food: num(cost["14"])}
			if got != u {
				t.Errorf("duelUnits %+v, lw_soldier %+v", u, got)
			}
			if need != "" {
				if sid := eventDumpScalar(aps[need]["science_id"]); sid != duelTrainScience {
					t.Errorf("lw_soldier %d needs science %s, whose science_id is %q; duelTrainScience is %s", u.id, need, sid, duelTrainScience)
				}
			}
		}
	})

	t.Run("seasonWeekCardBuildings", func(t *testing.T) {
		cards := map[int32]bool{}
		for id, r := range eventTableDump(t, "season_weekcard") {
			if c := num(r["farm_building_weekcard"]); c > 0 {
				cards[int32(c)] = true
			}
			for f := range strings.SplitSeq(eventDumpScalar(r["farm_building"]), "|") {
				if f == "" {
					continue
				}
				bid, err := strconv.ParseInt(f, 10, 32)
				if _, ok := collectCmdFor(int32(bid)); err != nil || !ok {
					t.Errorf("season_weekcard %s farm %q has no collect command", id, f)
				}
			}
		}
		if !maps.Equal(cards, seasonWeekCardBuildings) {
			t.Errorf("seasonWeekCardBuildings = %v, season_weekcard farm_building_weekcard %v", seasonWeekCardBuildings, cards)
		}
		for bid := range cards {
			if _, ok := collectCmdFor(bid); !ok {
				t.Errorf("week-card farm %d has no collect command", bid)
			}
		}
	})

	// The stage quest lists (final-r3 tests-tables-docs 8): questStageRanges must cover exactly
	// activity_stage's stage_quest ids, and stageQuestRanges must list, for every stage the five
	// stage-quest activity types read (stageQuestActivityStage's values, themselves checked against
	// table activity above), exactly the quests those stages list.
	stageQuests := func(t *testing.T) map[int64][]int64 {
		out := map[int64][]int64{}
		for id, r := range eventTableDump(t, "activity_stage") {
			qs, _ := r["stage_quest"].([]any)
			for _, q := range qs {
				out[num(q)] = append(out[num(q)], num(r["stage"]))
				if num(q) <= 0 {
					t.Errorf("activity_stage %s has stage_quest %v", id, q)
				}
			}
		}
		for q := range out {
			slices.Sort(out[q])
		}
		return out
	}

	t.Run("questStageRanges", func(t *testing.T) {
		quests := stageQuests(t)
		for q := range quests {
			if !questIsStageQuest(strconv.FormatInt(q, 10)) {
				t.Errorf("activity_stage lists quest %d, which questStageRanges lacks", q)
			}
		}
		for _, r := range questStageRanges {
			for q := r[0]; q <= r[1]; q++ {
				if _, ok := quests[q]; !ok {
					t.Errorf("questStageRanges covers %d, which no activity_stage row lists", q)
				}
			}
		}
	})

	t.Run("stageQuestRanges", func(t *testing.T) {
		read := map[int64]bool{}
		for _, stage := range stageQuestActivityStage {
			read[stage] = true
		}
		want := map[int64]string{}
		for q, stages := range stageQuests(t) {
			var in []int64
			for _, s := range stages {
				if read[s] {
					in = append(in, s)
				}
			}
			if len(in) > 0 {
				want[q] = fmt.Sprint(slices.Compact(in))
			}
		}
		got := map[int64]string{}
		for _, r := range stageQuestRanges {
			for q := r.lo; q <= r.hi; q++ {
				got[q] = fmt.Sprint(slices.Sorted(slices.Values(r.stages)))
			}
		}
		if !maps.Equal(got, want) {
			for q, s := range want {
				if got[q] != s {
					t.Errorf("activity_stage: quest %d is listed by stages %s; stageQuestRanges has %q", q, s, got[q])
				}
			}
			for q, s := range got {
				if _, ok := want[q]; !ok {
					t.Errorf("stageQuestRanges lists quest %d (stages %s), which no stage of the five types lists", q, s)
				}
			}
		}
	})

	// The lists the new opt-in features copy by hand (final-r3 new-features 6).
	t.Run("tacticalBoxRewards", func(t *testing.T) {
		got := map[int64][2]int64{}
		for id, r := range tacticalBoxRewards {
			got[id] = [2]int64{r.season, r.point}
		}
		want := map[int64][2]int64{}
		for id, r := range eventTableDump(t, "battle_card_box_reward") {
			want[key(id)] = [2]int64{num(r["season"]), num(r["point"])}
		}
		if !maps.Equal(got, want) {
			t.Errorf("tacticalBoxRewards = %v, battle_card_box_reward season/point %v", got, want)
		}
	})

	t.Run("tacticalCardNum", func(t *testing.T) {
		want := map[int64]string{}
		for id, r := range eventTableDump(t, "battle_card_reward") {
			want[key(id)] = eventDumpScalar(r["card_num"])
		}
		got := map[int64]string{}
		for id, n := range tacticalCardNum {
			got[id] = strings.ReplaceAll(fmt.Sprint(n), " ", ",")
		}
		if !maps.Equal(got, want) {
			t.Errorf("tacticalCardNum = %v, battle_card_reward card_num %v", got, want)
		}
	})

	t.Run("offSeasonGoalTypes", func(t *testing.T) {
		want := map[int64]int32{}
		for id, r := range eventTableDump(t, "activity_offSeason_task") {
			if typ := num(r["Type"]); typ < 0 {
				want[key(id)] = int32(typ)
			}
		}
		if !maps.Equal(offSeasonGoalTypes, want) {
			t.Errorf("offSeasonGoalTypes = %v, activity_offSeason_task rows with a negative Type %v", offSeasonGoalTypes, want)
		}
	})

	t.Run("letterItemIDs", func(t *testing.T) {
		vip, ok := eventTableDump(t, "item")["vip_letter"]
		if v := eventDumpScalar(vip["k2"]); !ok || v != letterVIP18ID {
			t.Errorf("letterVIP18ID = %s, item vip_letter k2 %q", letterVIP18ID, v)
		}
		var want []string
		for id, r := range eventTableDump(t, "goods") {
			if num(r["type"]) == 160 && id != eventDumpScalar(vip["k2"]) {
				want = append(want, id)
			}
		}
		if got := slices.Sorted(slices.Values(letterItemIDs)); !slices.Equal(got, slices.Sorted(slices.Values(want))) {
			t.Errorf("letterItemIDs = %v, goods of type 160 but vip_letter k2 %v", got, slices.Sorted(slices.Values(want)))
		}
	})

	t.Run("citySoldiersSparkUpgradeItem", func(t *testing.T) {
		want := map[int64]int64{}
		for id, r := range eventTableDump(t, "lw_civilization_spark") {
			need, _ := r["upgrade_need"].([]any)
			if len(need) == 0 {
				continue
			}
			if len(need) != 2 || num(need[1]) != 1 {
				t.Errorf("lw_civilization_spark %s upgrade_need %v: the feature assumes one item", id, need)
			}
			want[num(r["level"])] = num(need[0])
		}
		if !maps.Equal(citySoldiersSparkUpgradeItem, want) {
			t.Errorf("citySoldiersSparkUpgradeItem = %v, lw_civilization_spark upgrade_need %v", citySoldiersSparkUpgradeItem, want)
		}
	})

	t.Run("baseBubbleStatuses", func(t *testing.T) {
		thanks, scout := map[int32]bool{}, map[int32]bool{}
		valentine := map[int32]valentineStatus{}
		caps := map[int64]int{}
		for _, r := range eventTableDump(t, "activity_Valentine_Get") {
			caps[num(r["day_first_status"])] = int(num(r["bubble_times"]))
		}
		for id, r := range eventTableDump(t, "lw_status") {
			switch num(r["type2"]) {
			case 31: // StatusType2.BuildingRewardBubble (Thanksgiving)
				thanks[int32(key(id))] = true
			case 33: // StatusType2.BuildingRewardBubble2 (a scout march, never sent)
				scout[int32(key(id))] = true
			case 35: // the Valentine bubble
				valentine[int32(key(id))] = valentineStatus{activityID: int32(num(r["activity_id"])), dayCap: caps[key(id)]}
			}
		}
		if !maps.Equal(thanksgivingStatuses, thanks) {
			t.Errorf("thanksgivingStatuses = %v, lw_status type2 31 %v", thanksgivingStatuses, thanks)
		}
		if !maps.Equal(scoutBubbleStatuses, scout) {
			t.Errorf("scoutBubbleStatuses = %v, lw_status type2 33 %v", scoutBubbleStatuses, scout)
		}
		if !maps.Equal(valentineStatuses, valentine) {
			t.Errorf("valentineStatuses = %v, lw_status type2 35 with activity_Valentine_Get bubble_times %v", valentineStatuses, valentine)
		}
		for id, r := range eventTableDump(t, "activity_thanksgiving") {
			if n := num(r["day_receive_num"]); n != thanksgivingDayReceiveNum {
				t.Errorf("activity_thanksgiving %s day_receive_num %d, thanksgivingDayReceiveNum %d", id, n, thanksgivingDayReceiveNum)
			}
		}
	})
}
