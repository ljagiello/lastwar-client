package game

import (
	"fmt"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Main/side quests and chapter rewards (MASTER.md §4 #17), static-only.
//
//   - Quests: init task[] {id, num, state} (TaskManager:InitData, TaskManager.lua:24-34) and the
//     chapter's sub-tasks, init chapterTask.chapterSubTaskArray[] (ChapterTaskManager.lua:30-74),
//     are claimed with task.reward.get {id: UtfString} when state is 1 (TaskState.CanReceive,
//     EnumType.lua:5258-5264), the only check every quest cell makes. The reply's tasks[] carries
//     follow-up quests, which may already be claimable (TaskManager.lua:743-787).
//   - Chapter: chapter.task {chapterid: UtfString} once every sub-task is received (state 2) and
//     the chapter's own state is "0" (UIMainQuestItem.lua:152-157, ChapterTaskManager.lua:425-430;
//     garbageRefresh is left out, as ChapterGetReward does).
//
// Quests that belong to an activity stage (table activity_stage.stage_quest, questStageRanges) are
// left to their activity: Growth Foundation targets, for one, pay only after the foundation was
// bought (UILWGrowFoundationTargetItem.lua:120-128).
//
// Not sent: receive.player.level.reward. In 1.0.364 the client's HasReceivedLevelReward never
// reads levelRewardArr, so the claim branch of UILevelStage is unreachable and the level rewards
// arrive with push.player.add.exp instead (PlayerLevelManager.lua:158-248).
const (
	questRewardCmd  = "task.reward.get"
	chapterTaskCmd  = "chapter.task"
	questCanReceive = 1
	questReceived   = 2
	questMaxClaims  = 300 // bounds the follow-up chain in one run
)

// questStageRanges are the activity_stage.stage_quest ids in the 1.0.364 tables, as inclusive ranges.
var questStageRanges = [][2]int64{
	{2201001, 2201005}, {2202001, 2202005}, {2203001, 2203005}, {2204001, 2204005}, {2205001, 2205005},
	{2206001, 2206005}, {2207001, 2207005}, {2301001, 2301005}, {2302001, 2302005}, {2303001, 2303005},
	{2304001, 2304005}, {2305001, 2305005}, {2306001, 2306005}, {2307001, 2307005}, {2400000, 2400007},
	{2500001, 2500005}, {8000001, 8000010}, {8000101, 8000112}, {8010011, 8010015}, {8010018, 8010018},
	{8010101, 8010108}, {8010201, 8010208}, {8010301, 8010344}, {9010001, 9010008}, {9010101, 9010107},
	{9010201, 9010207}, {9100000, 9100149}, {9100300, 9100449}, {9100600, 9100749},
}

func init() {
	registerFeature(Feature{
		Name:    "quests",
		Summary: "claim completed main/side quests, chapter sub-tasks and the chapter reward; static-only",
		Run:     runQuests,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, questRewardCmd, chapterTaskCmd)
}

func questIsStageQuest(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return false
	}
	for _, r := range questStageRanges {
		if n >= r[0] && n <= r[1] {
			return true
		}
	}
	return false
}

func questID(t *sfs.SFSObject) string {
	if id := evString(t, "id"); id != "" {
		return id
	}
	return evString(t, "taskId")
}

func runQuests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	b := &evBatch{conn: conn}
	wrapper := in.Object("chapterTask")
	chapter := evObject(wrapper, "chapterTask")
	subState := map[string]int64{}
	var pending []string
	for _, t := range evObjects(wrapper, "chapterSubTaskArray") {
		id := questID(t)
		st, _ := evNum(t, "state")
		subState[id] = st
		if st == questCanReceive {
			pending = append(pending, id)
		}
	}
	for _, t := range in.Objects("task") {
		if st, _ := evNum(t, "state"); st == questCanReceive {
			pending = append(pending, questID(t))
		}
	}

	claimed := map[string]bool{}
	for len(pending) > 0 && len(claimed) < questMaxClaims && !b.dead {
		id := pending[0]
		pending = pending[1:]
		if id == "" || claimed[id] {
			continue
		}
		if _, isSub := subState[id]; !isSub && questIsStageQuest(id) {
			continue
		}
		claimed[id] = true
		p := sfs.NewSFSObject()
		p.PutUtfString("id", id)
		msg := b.send(fmt.Sprintf("quest reward %s", id), questRewardCmd, p)
		if msg == nil {
			continue
		}
		if _, isSub := subState[id]; isSub {
			subState[id] = questReceived
		}
		for _, t := range evObjects(msg.Params, "chapterTasks") {
			if st, ok := evNum(t, "state"); ok {
				subState[questID(t)] = st
			}
		}
		for _, t := range evObjects(msg.Params, "tasks") {
			if st, _ := evNum(t, "state"); st == questCanReceive {
				pending = append(pending, questID(t))
			}
		}
	}

	if chapter != nil && !b.dead && chapterClaimable(chapter, subState) {
		p := sfs.NewSFSObject()
		p.PutUtfString("chapterid", evString(chapter, "chapterid"))
		b.send("chapter reward", chapterTaskCmd, p)
	}
	return b.err()
}

// chapterClaimable: a chapter id, state "0", and as many received sub-tasks as subTasks lists.
func chapterClaimable(chapter *sfs.SFSObject, subState map[string]int64) bool {
	id := evString(chapter, "chapterid")
	if id == "" || id == "0" || evString(chapter, "state") != "0" {
		return false
	}
	all := 0
	for _, s := range strings.Split(evString(chapter, "subTasks"), "|") {
		if strings.TrimSpace(s) != "" {
			all++
		}
	}
	done := 0
	for _, st := range subState {
		if st == questReceived {
			done++
		}
	}
	return all > 0 && done >= all
}
