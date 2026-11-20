package game

import (
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Event task and stage claims the client offers on an event's own panel (MASTER.md G27, G28, G29,
// G52), each an OPT-IN feature of its own, claims only. Static-only, from the 1.0.370 Lua and
// tables 40413, not sent live yet. Every gate is the reply the client reads for the event plus the
// claim button's own rule; every claim reply is logged at Info, and an errorCode stops that
// event's claims (no benign codes are registered).
//
//   - doomsday-quests (G27, cov-ui-activity F5): while a Doomsday activity (type 157,
//     EnumType.lua:6416) is open the client sends activity.doomsday.quest.info {}
//     (ActivityListDataManager.lua:486-491; ActivityDoomsdayQuestInfoMessage.lua:4-6). The reply's
//     quest_info.doomsday_quests[{uuid, quest_id, state}] and quest_info.has_rewards (a CSV of
//     claimed quest ids) make a quest claimable when state == 1 and its quest_id is not in
//     has_rewards (LWDoomsdayManager.lua:98-136). The claim-all button sends
//     activity.doomsday.quest.reward {uuid: UtfString "all"} (UIDoomsdayAchievePage.lua:99-103;
//     ActivityDoomsdayQuestRewardMessage.lua:4-7).
//
//   - parkour-tasks (G28, cov-ui-activity F6): Multiple Parkour (type 301). hero.event.info.get
//     (ActivityListDataManager.lua:497-501) answers taskArr[{taskId, state}]
//     (MultipleParkourActivityManager.lua:3-15, via :1171-1172); the claim-all button is
//     interactable while a task has state 1 and sends multiply.task.reward {activityId: Int,
//     taskId: UtfString "0"} (UIActivityMultipleParkourRewardsView.lua:146-161;
//     MultipleParkourTaskRewardMessage.lua:5-9).
//
//   - front-break-tasks (G28): Front Break Sunday (type 312). hero.event.info.get (:674-678)
//     answers taskArr[{taskId, state}] and soldierBox[{id, state, target}]
//     (ActFrontBreakSundayData.lua:53-74). The claim-all button works like Parkour's and sends
//     activity.plane.feature.reward {activityId: Int, taskId: UtfString "0"}
//     (UIActivityFrontBreakSundayRewardsView.lua:120-138; FrontBreakSundayRewardMessage.lua:5-9). A
//     soldier box in state 1 (FrontBreakSundayBoxState.CanGet, EnumType.lua:18302-18306) sends
//     activity.plane.feature.get.soldier.box.reward {activityId: Int, id: Int 0}, the 0 being what
//     every box click sends (FrontBreakoutSunday.lua:423-445;
//     FrontBreakSundaySaveSoliderRewardMessage.lua:5-9). Never the stage fights, which report
//     client results.
//
//   - leading-quest-tasks (G28): Leading Quest V2 (type 219, EnumType.lua:6353). hero.event.info.get
//     (:579-583) answers taskArr[{taskId, state}] (LWLeadingQuestV2Manager.lua:14-23); each task in
//     state 1 sends power.up.task.reward {activityId: Int, taskId: UtfString}
//     (LeadingQuestItemV2.lua:156-160; PowerUpTaskRewardMessage.lua:4-8).
//
//   - hero-trial-stage (G28): Hero Trial (type 133, EnumType.lua:6374). hero.event.info.get (:377-379)
//     answers cur_stage and stageArr[{id, state, reward}] (ActivityEventInfo.lua:361); the stage
//     whose id is cur_stage is claimable in state 1 (UIHeroTrial.GetEventCanRewardCount,
//     UIHeroTrial.lua:511-526), and the button sends hero.experiment.reward {activityId: Int}
//     (:447-449; HeroExperimentRewardMessage.lua:5-8).
//
//   - crazy-rock-tasks (G29, cov-dc-events F3 and its verdict): Crazy Rock (type 186,
//     EnumType.lua:6483). music.game.activity.info {activityId: Int} (ActivityListDataManager.lua:
//     764-767; MusicGameActivityInfoMessage.lua:4-7) answers taskArr[{taskId or id, state}]
//     (ActCrazyRockDataManager.lua:29-43; ActCrazyRockTaskManager.lua:12-35). The claim-all button
//     needs a task in state 1 (GetRedDotNum, :112-122) and sends music.receive.task.reward
//     {activityId: Int, taskId: Int 0} (UIActCrazyRockTaskView.lua:129-139; :124-126;
//     MusicReceiveTaskRewardMessage.lua:4-8). The claim reply carries taskArr[{taskId, state}] and
//     reward (:38-60). Never music.game.start / music.game.over (client-reported score).
//
//   - theme-prenotice (G52, cov-dc-events F6 and its verdict): a theme activity (type 30,
//     ActivitySummary, EnumType.lua:6319) asks for its pre-notice rewards before it starts
//     (ThemeActivityManager.lua:24-41; the type-30 branch skips CheckIsSend,
//     ActivityListDataManager.lua:262-264): get.theme.activity.info {activityId: Int} answers
//     stageArr[{day, state, reward}] (:43-60). The entrance shows while preTime > 0 and preTime <=
//     now < startTime and the HQ reaches needMainCityLevel (:69-87; preTime is the noticeStartTime,
//     ActivityInfoData.lua:133-135), and day i (1..ThemeActivityNoticeDays = 3, ConstDefine.lua:368)
//     is claimable when i <= ceil((now - preTime) / 1 day) and its state is 0 or absent (:96-104).
//     Each sends receive.theme.activity.reward {activityId: Int, day: Int} (UIActivitySummaryView.lua:
//     364-367; ThemeActivityManager.lua:228-232; ClaimThemeActNoticeRewardMessage.lua:5-9). Tables
//     40413 have no type-30 row, so the sweep (which needs the type from table activity) sees none
//     until a regenerated table has one.
//
//   - sand-worm-tasks (G28): Sand Worm Hunt (type 240, EnumType.lua:6396). With the activity in the
//     list the client sends sandworm.task.info {} (ActivityListDataManager.lua:189-192;
//     SandWormHuntDataManager.lua:144-153), whose achievement[] and daily[] hold {id, state}; a task in
//     state 1 is claimable (GetCanReceive, :219-242) and its button sends sandworm.task.claim.reward
//     {taskId: UtfString} (SandWormTaskItem.lua:173-178; SandWormTaskClaimRewardMessage.lua:4-7).
//
// Not automated here (G28): Kill Zombie (107) per-level rewards need the difficulty rows and the
// monster.challenge.get.info extra data (ActionRewardItem.lua:81-160); Bingo (171), Easter Egg
// (185) and the season wasteland chests keep their state in managers this package has not read
// line by line; week.card.special.reward stays a documented deferral (feature_card_dailies.go).
const (
	doomsdayQuestsFeature   = "doomsday-quests"
	doomsdayActType         = 157
	doomsdayQuestInfoCmd    = "activity.doomsday.quest.info"
	doomsdayQuestRewardCmd  = "activity.doomsday.quest.reward"
	parkourTasksFeature     = "parkour-tasks"
	parkourActType          = 301
	parkourTaskRewardCmd    = "multiply.task.reward"
	frontBreakTasksFeature  = "front-break-tasks"
	frontBreakActType       = 312
	frontBreakTaskRewardCmd = "activity.plane.feature.reward"
	frontBreakSoldierBoxCmd = "activity.plane.feature.get.soldier.box.reward"
	leadingQuestFeature     = "leading-quest-tasks"
	leadingQuestActType     = 219
	leadingQuestRewardCmd   = "power.up.task.reward"
	heroTrialFeature        = "hero-trial-stage"
	heroTrialActType        = 133
	heroTrialRewardCmd      = "hero.experiment.reward"
	crazyRockFeature        = "crazy-rock-tasks"
	crazyRockActType        = 186
	crazyRockInfoCmd        = "music.game.activity.info"
	crazyRockRewardCmd      = "music.receive.task.reward"
	themeNoticeFeature      = "theme-prenotice"
	themeNoticeActType      = 30
	themeNoticeInfoCmd      = "get.theme.activity.info"
	themeNoticeRewardCmd    = "receive.theme.activity.reward"
	sandWormFeature         = "sand-worm-tasks"
	sandWormActType         = 240
	sandWormTaskInfoCmd     = "sandworm.task.info"
	sandWormTaskRewardCmd   = "sandworm.task.claim.reward"
	themeNoticeDays         = 3        // ThemeActivityNoticeDays
	themeNoticeDayMs        = 86400000 // OneDayTime * 1000
	eventTaskStateClaimable = 1        // TaskState.CanReceive, and the boxes' CanGet
	eventTaskClaimAllID     = "0"      // the claim-all taskId of Parkour and Front Break
)

func init() {
	for _, f := range []Feature{
		{Name: doomsdayQuestsFeature, Run: runDoomsdayQuests,
			Summary: "OPT-IN: claim the Doomsday event's completed quests with the client's claim-all (static-only, not validated live)"},
		{Name: parkourTasksFeature, Run: runParkourTasks,
			Summary: "OPT-IN: claim Multiple Parkour task rewards with the client's claim-all (static-only, not validated live)"},
		{Name: frontBreakTasksFeature, Run: runFrontBreakTasks,
			Summary: "OPT-IN: claim Front Break Sunday task rewards (claim-all) and reached soldier boxes (static-only, not validated live)"},
		{Name: leadingQuestFeature, Run: runLeadingQuestTasks,
			Summary: "OPT-IN: claim completed Leading Quest V2 tasks one by one (static-only, not validated live)"},
		{Name: heroTrialFeature, Run: runHeroTrialStage,
			Summary: "OPT-IN: claim the Hero Trial current stage's reward once it is complete (static-only, not validated live)"},
		{Name: crazyRockFeature, Run: runCrazyRockTasks,
			Summary: "OPT-IN: claim Crazy Rock (music festival) task rewards with the client's claim-all (static-only, not validated live)"},
		{Name: sandWormFeature, Run: runSandWormTasks,
			Summary: "OPT-IN: claim completed Sand Worm Hunt achievement and daily tasks one by one (static-only, not validated live)"},
		{Name: themeNoticeFeature, Run: runThemeNotice,
			Summary: "OPT-IN: claim a theme activity's pre-notice day rewards before it starts; no type-30 row in tables 40413 (static-only, not validated live)"},
	} {
		f.DefaultOn = false
		registerFeature(f)
	}
}

// evTaskLogReply logs the reply of a command no ON feature sends, at Info: its top-level keys and
// what it granted, plus attrs, so the first live run shows the shape the static reading assumed.
func evTaskLogReply(label, cmd string, p *sfs.SFSObject, attrs ...any) {
	if p == nil {
		return
	}
	slog.Info(label+" reply", append([]any{"cmd", cmd, "keys", p.Keys(), "granted", describeGrants(p)}, attrs...)...)
}

// evTaskClaimable lists the ids (idKeys tried in order) of the entries of arr whose state is 1.
func evTaskClaimable(o *sfs.SFSObject, arr string, idKeys ...string) []string {
	var out []string
	for _, t := range evObjects(o, arr) {
		if st, ok := evNum(t, "state"); !ok || st != eventTaskStateClaimable {
			continue
		}
		id := ""
		for _, k := range idKeys {
			if id = evString(t, k); id != "" {
				break
			}
		}
		out = append(out, id)
	}
	return out
}

// evTaskSend sends one claim, logs its reply and reports whether it succeeded.
func evTaskSend(b *evBatch, label, cmd string, p *sfs.SFSObject, attrs ...any) bool {
	r := b.send(label, cmd, p)
	if r == nil {
		return false
	}
	evTaskLogReply(label, cmd, r.Params, attrs...)
	return true
}

func runDoomsdayQuests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	acts := s.Open(doomsdayActType)
	if len(acts) == 0 {
		return nil
	}
	// The quest info carries no activity id: one read covers every open Doomsday entry.
	info, err := s.Info(doomsdayQuestsFeature+" quest info", doomsdayQuestInfoCmd, acts[0], sfs.NewSFSObject())
	if err != nil {
		b.add(err)
		return b.err()
	}
	due := doomsdayClaimable(info)
	slog.Info(doomsdayQuestsFeature+": quest info", "activity", acts[0].ID, "claimable", due)
	if len(due) > 0 {
		p := sfs.NewSFSObject()
		p.PutUtfString("uuid", "all")
		evTaskSend(b, doomsdayQuestsFeature+": claim all", doomsdayQuestRewardCmd, p, "claimable", len(due))
	}
	return b.err()
}

// doomsdayClaimable lists the quest ids LWDoomsdayManager:OnGetQuestInfo marks canRecieve: state 1
// and not in has_rewards.
func doomsdayClaimable(info *sfs.SFSObject) []int64 {
	q := evObject(info, "quest_info")
	if q == nil {
		return nil
	}
	got := map[int64]bool{}
	for _, f := range strings.Split(evString(q, "has_rewards"), ",") {
		if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
			got[n] = true
		}
	}
	var out []int64
	for _, e := range evObjects(q, "doomsday_quests") {
		id, ok := evNum(e, "quest_id")
		if st, sok := evNum(e, "state"); ok && sok && st == eventTaskStateClaimable && !got[id] {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

func runParkourTasks(conn *session.GameConn, in *Init) error {
	return runEventClaimAll(conn, in, parkourTasksFeature, parkourActType, parkourTaskRewardCmd)
}

func runFrontBreakTasks(conn *session.GameConn, in *Init) error {
	return runEventClaimAll(conn, in, frontBreakTasksFeature, frontBreakActType, frontBreakTaskRewardCmd)
}

// runEventClaimAll is the Parkour and Front Break claim-all: per open activity of actType, one
// hero.event.info.get and, when a task is in state 1, cmd {activityId: Int, taskId: "0"}. Front
// Break then claims its reached soldier boxes.
func runEventClaimAll(conn *session.GameConn, in *Init, feature string, actType int32, cmd string) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actType) {
		if b.dead {
			break
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		due := evTaskClaimable(info, "taskArr", "taskId")
		boxes := evTaskClaimable(info, "soldierBox", "id")
		slog.Info(feature+": event info", "activity", a.ID, "claimableTasks", due, "claimableSoldierBoxes", boxes)
		if len(due) > 0 {
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			p.PutUtfString("taskId", eventTaskClaimAllID)
			if !evTaskSend(b, feature+": claim all tasks", cmd, p, "activity", a.ID, "claimable", len(due)) {
				continue
			}
		}
		if actType == frontBreakActType && len(boxes) > 0 {
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			p.PutInt("id", 0)
			evTaskSend(b, feature+": soldier boxes", frontBreakSoldierBoxCmd, p, "activity", a.ID, "claimable", len(boxes))
		}
	}
	return b.err()
}

func runLeadingQuestTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(leadingQuestActType) {
		if b.dead {
			break
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		due := evTaskClaimable(info, "taskArr", "taskId")
		slog.Info(leadingQuestFeature+": event info", "activity", a.ID, "claimable", due)
		for _, id := range due {
			if id == "" {
				continue
			}
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			p.PutUtfString("taskId", id)
			if !evTaskSend(b, leadingQuestFeature+": task "+id, leadingQuestRewardCmd, p, "activity", a.ID) {
				break // the first refusal stops this activity's claims
			}
		}
	}
	return b.err()
}

func runHeroTrialStage(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(heroTrialActType) {
		if b.dead {
			break
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		cur, due := heroTrialClaimable(info)
		slog.Info(heroTrialFeature+": event info", "activity", a.ID, "curStage", cur, "claimable", due)
		if due {
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			evTaskSend(b, heroTrialFeature+": stage reward", heroTrialRewardCmd, p, "activity", a.ID, "stage", cur)
		}
	}
	return b.err()
}

// heroTrialClaimable reports the current stage and whether its stageArr entry is in state 1.
func heroTrialClaimable(info *sfs.SFSObject) (cur int64, due bool) {
	cur, ok := evNum(info, "cur_stage")
	if !ok {
		return 0, false
	}
	for _, st := range evObjects(info, "stageArr") {
		if id, ok := evNum(st, "id"); ok && id == cur {
			s, ok := evNum(st, "state")
			return cur, ok && s == eventTaskStateClaimable
		}
	}
	return cur, false
}

func runCrazyRockTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(crazyRockActType) {
		if b.dead {
			break
		}
		p := sfs.NewSFSObject()
		p.PutInt("activityId", a.ID)
		info, err := s.Info(crazyRockFeature+" activity info", crazyRockInfoCmd, a, p)
		if err != nil {
			b.add(err)
			continue
		}
		due := evTaskClaimable(info, "taskArr", "taskId", "id")
		slog.Info(crazyRockFeature+": activity info", "activity", a.ID, "claimable", due)
		if len(due) == 0 {
			continue
		}
		c := sfs.NewSFSObject()
		c.PutInt("activityId", a.ID)
		c.PutInt("taskId", 0)
		evTaskSend(b, crazyRockFeature+": claim all", crazyRockRewardCmd, c, "activity", a.ID, "claimable", len(due))
	}
	return b.err()
}

func runSandWormTasks(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	acts := s.Open(sandWormActType)
	if len(acts) == 0 {
		return nil
	}
	// The task info carries no activity id: one read covers the event.
	info, err := s.Info(sandWormFeature+" task info", sandWormTaskInfoCmd, acts[0], sfs.NewSFSObject())
	if err != nil {
		b.add(err)
		return b.err()
	}
	var due []string
	for _, list := range []string{"achievement", "daily"} {
		for _, t := range claimObjectsOrMap(info, list) {
			if st, ok := evNum(t, "state"); ok && st == eventTaskStateClaimable {
				if id := evString(t, "id"); id != "" {
					due = append(due, id)
				}
			}
		}
	}
	slog.Info(sandWormFeature+": task info", "activity", acts[0].ID, "claimable", due)
	for _, id := range due {
		p := sfs.NewSFSObject()
		p.PutUtfString("taskId", id)
		if !evTaskSend(b, sandWormFeature+": task "+id, sandWormTaskRewardCmd, p) {
			break // the first refusal stops the claims
		}
	}
	return b.err()
}

func runThemeNotice(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	now := evNow().UnixMilli()
	hq := activityHQLevel(in)
	for _, a := range s.All(themeNoticeActType) {
		if b.dead {
			break
		}
		pre, _ := evNum(a.Raw, "preTime")
		if !themeNoticeWindow(pre, a.StartTime(), now) || hq < a.NeedHQ {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("activityId", a.ID)
		info, err := s.Info(themeNoticeFeature+" notice info", themeNoticeInfoCmd, a, p)
		if err != nil {
			b.add(err)
			continue
		}
		due := themeNoticeDueDays(pre, now, themeNoticeClaimedDays(info))
		slog.Info(themeNoticeFeature+": notice info", "activity", a.ID, "preTime", pre, "startTime", a.StartTime(), "due", due)
		for _, day := range due {
			c := sfs.NewSFSObject()
			c.PutInt("activityId", a.ID)
			c.PutInt("day", int32(day))
			if !evTaskSend(b, themeNoticeFeature+": day reward", themeNoticeRewardCmd, c, "activity", a.ID, "day", day) {
				break
			}
		}
	}
	return b.err()
}

// themeNoticeWindow is CheckIfShowEntrance's pre-start branch: preTime > 0 and preTime <= now <
// startTime (ThemeActivityManager.lua:79-81).
func themeNoticeWindow(preTime, startTime, now int64) bool {
	return preTime > 0 && preTime <= now && now < startTime
}

// themeNoticeClaimedDays reads the info reply's stageArr the way UpdateNoticeRewardInfo stores it,
// by day (ThemeActivityManager.lua:43-60): a day with an entry is claimed unless the entry's state
// is 0, so an entry without a state counts as claimed, as in GetThemeActivityRedCount's
// `not noticeInfo[i] or noticeInfo[i].state == 0` (:96-104) and the preview's `state ~= 0`
// (UIActivitySummaryPreviewView.lua:161-168). A day with no entry stays claimable.
func themeNoticeClaimedDays(info *sfs.SFSObject) map[int64]bool {
	claimed := map[int64]bool{}
	for _, st := range evObjects(info, "stageArr") {
		day, ok := evNum(st, "day")
		if !ok {
			continue
		}
		if state, sok := evNum(st, "state"); !sok || state != 0 {
			claimed[day] = true
		}
	}
	return claimed
}

// themeNoticeDueDays lists the pre-notice days GetThemeActivityRedCount counts: i in 1..3 with i <=
// ceil((now - preTime) / 1 day) and not claimed (ThemeActivityManager.lua:96-104).
func themeNoticeDueDays(preTime, now int64, claimed map[int64]bool) []int64 {
	if now <= preTime {
		return nil
	}
	passed := (now - preTime + themeNoticeDayMs - 1) / themeNoticeDayMs
	var out []int64
	for i := int64(1); i <= themeNoticeDays && i <= passed; i++ {
		if !claimed[i] {
			out = append(out, i)
		}
	}
	return out
}
