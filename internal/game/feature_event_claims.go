package game

import (
	"fmt"
	"log/slog"
	"regexp"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Event task, box and milestone claims (MASTER.md §4 #22), static-only, each only while its event
// runs. Every gate is one read the client sends for the event plus the client's own red-dot rule.
//
//   - Crystal boss (type 412), 1.0.370 CrystalBossDataManager.lua. The client asks for nothing
//     unless CanRequestActivityData holds (:414-416): the show window [readyTime or startTime,
//     endViewTime or rewardTime or endTime] contains now (IsActivityTimeOpen :386-394;
//     ActivityInfoData.lua:436-453), and the HQ reaches the N of the table row's condition "1|N"
//     (IsUnlockConditionMet :396-412; 80152 is "1|20"). It then sends red.boss.get.march {} first
//     (RequestPanelData :502-518) and takes the activity id from that reply; with none it asks and
//     claims nothing more (HandleMarchData :756-760, 824-827; every request and claim checks
//     IsValidActivityId). From the march reply: red.boss.get.progress {}, whose reply is dropped
//     unless its season is the account's (:901-916), claimableCount > 0 sends red.boss.claim.all
//     {type: 1}; red.boss.get.achievement.info {activityId: UtfString, the march reply's}, whose
//     reply is dropped when it names another activity (:1132-1134), a damage task (type 1) or an
//     attack-count task (type 2) of the current season in state 1 sends red.boss.claim.all
//     {type: 2} (RebuildAchievementCache :1108-1127); red.boss.claim.daily.reward {} once the march
//     reply's dailyAtkCount reaches dailyRewardTarget and dailyRewardClaimed is 0 (:666-683). type
//     3 ("all") is never sent by the client and is not sent here.
//   - Winter storm (type 218): winter.storm.reward.info {} -> score, data[] (claimed box ids);
//     winter.storm.reward.get.all {} when a box's score threshold is reached and its id is not in
//     data (ActWinterStormManager.lua:733-796; table winter_battlefield_reward_boxes).
//   - Seven-day events (type 34): get.seven.day.act.info {activityId: Int} -> score,
//     scoreReward[{needScore, rewardFlag, needVipLevel, vipRewardFlag}]. The showUiType-3 events
//     (DoomVanguard: 95003, 95005) claim with receive.seven.day.act.all.reward {activityId: Int}
//     when the free or the VIP reward of a reached box is unclaimed
//     (UIActSevenDayDoomVanguardRewardItem.lua:124-143). The others (showUiType 0 and 2 open
//     ActSevenDay, EnumType.lua:14419-14436) claim per box with receive.seven.day.act.reward
//     {activityId: Int, index: Int 1-based} only while the box's free reward is unclaimed: the box
//     cell reads rewardFlag alone (ActSevenDayBoxItem.lua:51, 93-96).
//   - Seven-day v2 (type 174): seven.day.v2.info {activityId: Int} -> dayActs[] (three per day,
//     each with tasks[{id, state}]); seven.day.v2.receive.task {taskId: UtfString, activityId: Int}
//     for state-1 tasks of unlocked days, then seven.day.v2.receive.score {activityId: Int} when a
//     score box is due (ActSevenDayV2Info.lua:30-153).
//   - Bounty hunter (type 350): bounty.hunter.get.info {activityId: Int} -> stageKey.stashReward;
//     bounty.hunter.receive.drop.reward {activityId: Int} when it is non-empty
//     (BountyHunterActData.lua:99-101).
//   - Activity tasks (type 154 only, never the paid ContinuePay type 136): hero.event.info.get ->
//     taskArr[{taskId, state}], achieveArr[{targetScore}], achieve_score, score_receives[];
//     activity.task.reward {activityId: Int, taskId: UtfString} per state-1 task, then
//     activity.score.reward {activityId: Int, index: Int} for each next milestone reached
//     (ActTaskManager; UI senders in ui-1 chunk "ActTask").
//   - Peak and gale arenas: arena.info {} -> newArenaInfo/galeArenaInfo {showTime, startTime,
//     endTime (ms)}; while one is open, new.arena.rank.list / gale.arena.rank.list {} -> battleCount
//     and dailyReward[{needCount, rewarded}] at the reply's top level (the handlers keep the whole
//     reply as rankData: NewArenaRankListMessage.lua:12, NewPeakArenaManager.lua:142-150,
//     NewGaleArenaManager.lua:132-140); new.arena.reward / gale.arena.reward {target: Int 1-based}
//     per box with battleCount >= needCount and rewarded 0 (GetNeedGetRewardList,
//     NewPeakArenaManager.lua:239-250, NewGaleArenaManager.lua:229-240; NewPeakArenaTipBoxItem.lua:
//     58-80). The client's -1 "all" is not used: its server meaning is unverified.
//   - Flower train: flower.train.receive.reward {trainUuid: Long} for the account's own init
//     flowerTrainArr entries that arrived (arrived 1, or now >= arriveTime)
//     (BaseSingleFlowerTrainData.lua:60-70, 273-279).
//
// Not sent: meteorite rewards (reward ids depend on a client-side season mapping),
// alliance.challenge.new.reward.get (it consumes keys) and banquet v2 (only partly traced). Zone
// mobilization and torch-relay tasks are the opt-in zone-mobilization and torch-relay features
// (feature_zone_mobilization.go, feature_torch_relay.go), off until validated live.
const (
	redBossProgressCmd     = "red.boss.get.progress"
	redBossAchievementCmd  = "red.boss.get.achievement.info"
	redBossMarchCmd        = "red.boss.get.march"
	redBossClaimAllCmd     = "red.boss.claim.all"
	redBossDailyCmd        = "red.boss.claim.daily.reward"
	winterStormInfoCmd     = "winter.storm.reward.info"
	winterStormClaimCmd    = "winter.storm.reward.get.all"
	sevenDayInfoCmd        = "get.seven.day.act.info"
	sevenDayAllCmd         = "receive.seven.day.act.all.reward"
	sevenDayOneCmd         = "receive.seven.day.act.reward"
	sevenDayV2InfoCmd      = "seven.day.v2.info"
	sevenDayV2TaskCmd      = "seven.day.v2.receive.task"
	sevenDayV2ScoreCmd     = "seven.day.v2.receive.score"
	bountyHunterInfoCmd    = "bounty.hunter.get.info"
	bountyHunterDropCmd    = "bounty.hunter.receive.drop.reward"
	actTaskRewardCmd       = "activity.task.reward"
	actScoreRewardCmd      = "activity.score.reward"
	arenaInfoCmd           = "arena.info"
	flowerTrainCmd         = "flower.train.receive.reward"
	eventTaskCanReceive    = 1
	sevenDayV2TasksPerDay  = 3
	sevenDayV2DaySeconds   = 86400
	redBossSilentError     = "E100172"
	redBossClaimProgress   = 1
	redBossClaimAchievemnt = 2
	// CrystalBossAchievementDefine.Type (CrystalBossAchievementDefine.lua:2).
	redBossAchievementDamage = 1
	redBossAchievementAttack = 2
	// sevenDayDoomVanguardUI is SevenDayType.DoomVanguard (EnumType.lua:14419-14423), the
	// showUiType whose view claims every box at once.
	sevenDayDoomVanguardUI = 3
)

// redBossConditionRe is CrystalBossDataManager:IsUnlockConditionMet's pattern
// "^%s*(%d+)%s*|%s*(%d+)%s*$" (CrystalBossDataManager.lua:406).
var redBossConditionRe = regexp.MustCompile(`^\s*(\d+)\s*\|\s*(\d+)\s*$`)

// winterStormBoxes is table winter_battlefield_reward_boxes (box id -> score), the type-1 rows.
var winterStormBoxes = map[int64]int64{201: 45000, 202: 90000, 203: 135000, 204: 225000, 205: 337500, 206: 450000}

// eventArenas are the two arenas with daily battle boxes.
var eventArenas = []struct{ infoKey, rankCmd, rewardCmd string }{
	{"newArenaInfo", "new.arena.rank.list", "new.arena.reward"},
	{"galeArenaInfo", "gale.arena.rank.list", "gale.arena.reward"},
}

func init() {
	registerFeature(Feature{
		Name: "event-claims",
		Summary: "claim reached event tasks/boxes: crystal boss, winter storm, seven-day (v1/v2), bounty stash, " +
			"activity tasks, arena daily boxes, arrived flower trains; static-only",
		Run: runEventClaims,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, redBossClaimAllCmd, redBossDailyCmd, winterStormClaimCmd, sevenDayAllCmd,
		sevenDayOneCmd, sevenDayV2TaskCmd, sevenDayV2ScoreCmd, bountyHunterDropCmd, actTaskRewardCmd, actScoreRewardCmd,
		flowerTrainCmd, eventArenas[0].rewardCmd, eventArenas[1].rewardCmd)
	// The crystal-boss handlers show every errorCode as a tip except this one, which they drop
	// silently (CrystalBossMessageHelper.lua:2-11).
	session.RegisterBenignErrorCode(redBossSilentError, redBossClaimAllCmd, redBossDailyCmd)
}

func runEventClaims(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	steps := []func(){
		func() { claimCrystalBoss(s, b, in) },
		func() { claimWinterStorm(s, b) },
		func() { claimSevenDay(s, b, in) },
		func() { claimSevenDayV2(s, b, in) },
		func() { claimBountyStash(s, b) },
		func() { claimActivityTasks(s, b) },
		func() { claimArenaBoxes(b) },
		func() { claimFlowerTrains(b, in) },
	}
	for _, step := range steps {
		if b.dead {
			break
		}
		step()
	}
	return b.err()
}

// redBossWindowOpen is CrystalBossDataManager:IsActivityTimeOpen (CrystalBossDataManager.lua:
// 386-394): GetShowStartTime (readyTime when > 0, else startTime) < GetShowEndTime (endViewTime
// when > 0, else rewardTime when > 0, else endTime with lastTime over it; ActivityInfoData.lua:
// 139-147, 436-453), and now within both, with no tolerance. It reaches past endTime, where the
// sweep's Open (CheckIsSend) stops.
func redBossWindowOpen(a Activity, nowMs int64) bool {
	start := a.StartTime()
	if ready, _ := evNum(a.Raw, "readyTime"); ready > 0 {
		start = ready
	}
	end := a.EndTime()
	if reward, _ := evNum(a.Raw, "rewardTime"); reward > 0 {
		end = reward
	}
	if view := a.EndViewTime(); view > 0 {
		end = view
	}
	return start < end && start <= nowMs && nowMs <= end
}

// redBossUnlocked is CrystalBossDataManager:IsUnlockConditionMet (CrystalBossDataManager.lua:
// 396-412): an empty condition is unlocked; "1|N" needs HQ >= N; any other condition, and a row
// the table lacks, is locked.
func redBossUnlocked(a Activity, hq int32) (bool, string) {
	row, ok := activityRowOf(a.ID)
	if !ok {
		return false, "activity has no table row"
	}
	if row.condition == "" {
		return true, ""
	}
	m := redBossConditionRe.FindStringSubmatch(row.condition)
	var kind, need int64
	var err1, err2 error
	if m != nil {
		kind, err1 = strconv.ParseInt(m[1], 10, 64)
		need, err2 = strconv.ParseInt(m[2], 10, 64)
	}
	if m == nil || err1 != nil || err2 != nil || kind != 1 {
		return false, fmt.Sprintf("condition %q is not 1|N", row.condition)
	}
	if int64(hq) < need {
		return false, fmt.Sprintf("HQ %d is below the condition's %d", hq, need)
	}
	return true, ""
}

// redBossValidID is the reply's activityId as the client keeps it: IsValidActivityId rejects nil,
// "" and "0" (CrystalBossDataManager.lua:25-31).
func redBossValidID(o *sfs.SFSObject) string {
	if id := evString(o, "activityId"); id != "" && id != "0" {
		return id
	}
	return ""
}

// redBossAchievementDue is RebuildAchievementCache (CrystalBossDataManager.lua:1108-1127, tasks
// parsed at :1145-1170): a task whose type is damage, or attack count with the current season,
// and whose id is a positive number (BuildKey, CrystalBossAchievementDefine.lua:13-20), counts as
// claimable in state 1.
func redBossAchievementDue(reply *sfs.SFSObject, season int64) bool {
	for _, t := range evObjects(reply, "tasks") {
		typ, _ := evNum(t, "type")
		id, _ := evNum(t, "id")
		if id <= 0 || (typ != redBossAchievementDamage && typ != redBossAchievementAttack) {
			continue
		}
		if typ == redBossAchievementAttack {
			if s, _ := evNum(t, "season"); s != season {
				continue
			}
		}
		if st, _ := evNum(t, "state"); st == eventTaskCanReceive {
			return true
		}
	}
	return false
}

func claimCrystalBoss(s *ActivitySweep, b *evBatch, in *Init) {
	now := evNow().UnixMilli()
	// SeasonUtil.GetSeason: init playerServerSeasonInfo.seasonId, 0 without one (SeasonUtil.lua:
	// 286-298), with no open-season test.
	season, _ := evNum(in.Object("playerServerSeasonInfo"), "seasonId")
	for _, a := range s.All(actTypeCrystalBoss) {
		if b.dead {
			return
		}
		if !redBossWindowOpen(a, now) {
			slog.Debug("crystal boss: outside the show window; the client asks for nothing", "activity", a.ID)
			continue
		}
		if ok, why := redBossUnlocked(a, s.hq); !ok {
			slog.Info("crystal boss: locked; the client asks for nothing", "activity", a.ID, "reason", why)
			continue
		}
		march := b.send("crystal boss march", redBossMarchCmd, sfs.NewSFSObject())
		if march == nil {
			continue
		}
		id := redBossValidID(march.Params)
		if id == "" {
			slog.Info("crystal boss: the march reply carries no activityId; the client asks and claims nothing more",
				"activity", a.ID)
			continue
		}
		if msg := b.send("crystal boss progress", redBossProgressCmd, sfs.NewSFSObject()); msg != nil {
			if rs, ok := evNum(msg.Params, "season"); !ok || rs != season {
				slog.Info("crystal boss: progress reply for another season (or none); dropped as the client does",
					"activity", a.ID, "replySeason", rs, "hasSeason", ok, "season", season)
			} else if n, _ := evNum(msg.Params, "claimableCount"); n > 0 {
				p := sfs.NewSFSObject()
				p.PutInt("type", redBossClaimProgress)
				b.send("crystal boss progress rewards", redBossClaimAllCmd, p)
			}
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("activityId", id)
		if msg := b.send("crystal boss achievements", redBossAchievementCmd, p); msg != nil {
			if rid := redBossValidID(msg.Params); rid != "" && rid != id {
				slog.Info("crystal boss: achievement reply for another activity; dropped as the client does",
					"activity", id, "replyActivity", rid)
			} else if redBossAchievementDue(msg.Params, season) {
				p := sfs.NewSFSObject()
				p.PutInt("type", redBossClaimAchievemnt)
				b.send("crystal boss achievement rewards", redBossClaimAllCmd, p)
			}
		}
		target, _ := evNum(march.Params, "dailyRewardTarget")
		atk, _ := evNum(march.Params, "dailyAtkCount")
		claimed, ok := evNum(march.Params, "dailyRewardClaimed")
		if target > 0 && atk >= target && ok && claimed == 0 {
			b.send("crystal boss daily", redBossDailyCmd, sfs.NewSFSObject())
		}
	}
}

func claimWinterStorm(s *ActivitySweep, b *evBatch) {
	if len(s.Open(actTypeWinterStorm)) == 0 {
		return
	}
	msg := b.send("winter storm boxes", winterStormInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	score, _ := evNum(msg.Params, "score")
	claimed := map[int64]bool{}
	for _, id := range evNums(msg.Params, "data") {
		claimed[id] = true
	}
	for id, need := range winterStormBoxes {
		if score >= need && !claimed[id] {
			b.send("winter storm boxes", winterStormClaimCmd, sfs.NewSFSObject())
			return
		}
	}
}

// sevenDayDue returns the 1-based score boxes reached with the free reward unclaimed, or, with
// vipTrack, the free or (VIP level allowing) the VIP reward unclaimed. The two-flag rule is the v2
// reward item's (ActSevenDayV2Info:CheckRedDot, :112-134; UIActSevenDayV2RewardItem.lua:157-184)
// and the DoomVanguard view's; the v1 per-box cell sends on rewardFlag alone
// (ActSevenDayBoxItem.lua:51, 93-96).
func sevenDayDue(info *sfs.SFSObject, vipLevel int64, vipTrack bool) []int32 {
	score, _ := evNum(info, "score")
	var due []int32
	for i, r := range evObjects(info, "scoreReward") {
		need, ok := evNum(r, "needScore")
		if !ok || score < need {
			continue
		}
		free, okFree := evNum(r, "rewardFlag")
		vip, okVip := evNum(r, "vipRewardFlag")
		needVip, _ := evNum(r, "needVipLevel")
		if (okFree && free == 0) || (vipTrack && okVip && vip == 0 && vipLevel >= needVip) {
			due = append(due, int32(i+1))
		}
	}
	return due
}

func evVIPLevel(in *Init) int64 {
	n, _ := evNum(evObject(in.Object("vip"), "vipInfo"), "level")
	return n
}

func claimSevenDay(s *ActivitySweep, b *evBatch, in *Init) {
	for _, a := range s.Open(actTypeSevenDay) {
		if b.dead {
			return
		}
		info, err := evIntInfo("seven-day info response", sevenDayInfoCmd, "activityId")(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		claimAll := a.ShowUIType() == sevenDayDoomVanguardUI
		due := sevenDayDue(info, evVIPLevel(in), claimAll)
		if len(due) == 0 {
			continue
		}
		if claimAll {
			b.send(fmt.Sprintf("seven-day boxes (activity %d)", a.ID), sevenDayAllCmd, evActivityIDParam(a))
			continue
		}
		for _, idx := range due {
			p := evActivityIDParam(a)
			p.PutInt("index", idx)
			b.send(fmt.Sprintf("seven-day box %d (activity %d)", idx, a.ID), sevenDayOneCmd, p)
		}
	}
}

func claimSevenDayV2(s *ActivitySweep, b *evBatch, in *Init) {
	fetch := evIntInfo("seven-day v2 info response", sevenDayV2InfoCmd, "activityId")
	for _, a := range s.Open(actTypeSevenDayV2) {
		if b.dead {
			return
		}
		info, err := fetch(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		acts := evObjects(info, "dayActs")
		groups := (len(acts) + sevenDayV2TasksPerDay - 1) / sevenDayV2TasksPerDay
		elapsed := (evNow().UnixMilli() - a.StartTime()) / 1000
		days := 0
		for i := 1; i <= groups; i++ {
			if elapsed <= int64(i*sevenDayV2DaySeconds) {
				days = i
				break
			}
		}
		if days == 0 {
			days = groups
		}
		claimedTask := false
		for i, act := range acts {
			if i/sevenDayV2TasksPerDay+1 > days {
				break
			}
			for _, t := range evObjects(act, "tasks") {
				id := evString(t, "id")
				if st, _ := evNum(t, "state"); st != eventTaskCanReceive || id == "" || b.dead {
					continue
				}
				p := sfs.NewSFSObject()
				p.PutUtfString("taskId", id)
				p.PutInt("activityId", a.ID)
				if b.send(fmt.Sprintf("seven-day v2 task %s", id), sevenDayV2TaskCmd, p) != nil {
					claimedTask = true
				}
			}
		}
		if claimedTask && !b.dead {
			s.Forget(sevenDayV2InfoCmd, a)
			if info, err = fetch(s, a); err != nil {
				b.add(err)
				continue
			}
		}
		if len(sevenDayDue(info, evVIPLevel(in), true)) > 0 && !b.dead {
			b.send(fmt.Sprintf("seven-day v2 boxes (activity %d)", a.ID), sevenDayV2ScoreCmd, evActivityIDParam(a))
		}
	}
}

func claimBountyStash(s *ActivitySweep, b *evBatch) {
	fetch := evIntInfo("bounty hunter info response", bountyHunterInfoCmd, "activityId")
	for _, a := range s.Open(actTypeBountyHunter) {
		if b.dead {
			return
		}
		info, err := fetch(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		if evNonEmpty(evObject(info, "stageKey"), "stashReward") {
			b.send(fmt.Sprintf("bounty hunter stash (activity %d)", a.ID), bountyHunterDropCmd, evActivityIDParam(a))
		}
	}
}

func claimActivityTasks(s *ActivitySweep, b *evBatch) {
	for _, a := range s.Open(actTypeActTask) {
		if b.dead {
			return
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		if t, ok := evNum(info, "activityType"); !ok || t != int64(actTypeActTask) {
			continue
		}
		claimedTask := false
		for _, t := range evObjects(info, "taskArr") {
			id := evString(t, "taskId")
			if st, _ := evNum(t, "state"); st != eventTaskCanReceive || id == "" || b.dead {
				continue
			}
			p := evActivityIDParam(a)
			p.PutUtfString("taskId", id)
			if b.send(fmt.Sprintf("activity task %s", id), actTaskRewardCmd, p) != nil {
				claimedTask = true
			}
		}
		if claimedTask && !b.dead {
			s.Forget("hero.event.info.get", a)
			if info, err = s.EventInfo(a); err != nil {
				b.add(err)
				continue
			}
		}
		score, _ := evNum(info, "achieve_score")
		achieve := evObjects(info, "achieveArr")
		n := 0
		if v, ok := info.Get("score_receives"); ok {
			switch arr := v.Val.(type) {
			case *sfs.SFSArray:
				n = len(arr.Items())
			default:
				n = len(evNums(info, "score_receives"))
			}
		}
		for n < len(achieve) && !b.dead {
			target, ok := evNum(achieve[n], "targetScore")
			if !ok || score < target {
				break
			}
			p := evActivityIDParam(a)
			p.PutInt("index", int32(n))
			if b.send(fmt.Sprintf("activity milestone %d (activity %d)", n, a.ID), actScoreRewardCmd, p) == nil {
				break
			}
			n++
		}
	}
}

func claimArenaBoxes(b *evBatch) {
	msg := b.send("arena info", arenaInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	now := evNow().UnixMilli()
	for _, ar := range eventArenas {
		info := evObject(msg.Params, ar.infoKey)
		show, _ := evNum(info, "showTime")
		start, ok1 := evNum(info, "startTime")
		end, ok2 := evNum(info, "endTime")
		if show == 0 || !ok1 || !ok2 || now < start || now > end || b.dead {
			continue
		}
		rank := b.send(ar.rankCmd, ar.rankCmd, sfs.NewSFSObject())
		if rank == nil {
			continue
		}
		count, _ := evNum(rank.Params, "battleCount")
		for i, box := range evObjects(rank.Params, "dailyReward") {
			need, ok := evNum(box, "needCount")
			done, _ := evNum(box, "rewarded")
			if !ok || count < need || done != 0 || b.dead {
				continue
			}
			p := sfs.NewSFSObject()
			p.PutInt("target", int32(i+1))
			b.send(fmt.Sprintf("%s box %d", ar.rewardCmd, i+1), ar.rewardCmd, p)
		}
	}
}

func claimFlowerTrains(b *evBatch, in *Init) {
	me := evString(in.Object("user"), "uid")
	if me == "" {
		return
	}
	now := evNow().UnixMilli()
	for _, t := range in.Objects("flowerTrainArr") {
		uuid, ok := evNum(t, "uuid")
		if !ok || evString(t, "uid") != me || b.dead {
			continue
		}
		arrived, _ := evNum(t, "arrived")
		at, hasAt := evNum(t, "arriveTime")
		if arrived != 1 && (!hasAt || now < at) {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutLong("trainUuid", uuid)
		b.send("flower train", flowerTrainCmd, p)
	}
}
