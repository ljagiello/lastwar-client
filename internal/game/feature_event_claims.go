package game

import (
	"fmt"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Event task, box and milestone claims (MASTER.md §4 #22), static-only, each only while its event
// runs. Every gate is one read the client sends for the event plus the client's own red-dot rule.
//
//   - Crystal boss (type 412): red.boss.get.progress {} -> claimableCount > 0 sends
//     red.boss.claim.all {type: 1}; red.boss.get.achievement.info {activityId: UtfString} -> a task
//     in state 1 sends red.boss.claim.all {type: 2}; red.boss.get.march {} -> red.boss.claim.daily.
//     reward {} once dailyAtkCount reaches dailyRewardTarget and dailyRewardClaimed is 0
//     (CrystalBossDataManager.lua:522-616, 827-850, 1033-1209). type 3 ("all") is never sent by the
//     client and is not sent here.
//   - Winter storm (type 218): winter.storm.reward.info {} -> score, data[] (claimed box ids);
//     winter.storm.reward.get.all {} when a box's score threshold is reached and its id is not in
//     data (ActWinterStormManager.lua:733-796; table winter_battlefield_reward_boxes).
//   - Seven-day events (type 34): get.seven.day.act.info {activityId: Int} -> score,
//     scoreReward[{needScore, rewardFlag, needVipLevel, vipRewardFlag}]. The two showUiType-3 events
//     claim with receive.seven.day.act.all.reward {activityId: Int}, the others per box with
//     receive.seven.day.act.reward {activityId: Int, index: Int 1-based}
//     (UIActSevenDayDoomVanguardRewardItem.lua:113-138; ActSevenDayBoxItem.lua:43-87).
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
//     endTime (ms)}; while one is open, new.arena.rank.list / gale.arena.rank.list {} ->
//     rankData{battleCount, dailyReward[{needCount, rewarded}]}; new.arena.reward / gale.arena.reward
//     {target: Int 1-based} per reached box (NewPeakArenaManager.lua:218-229,
//     NewGaleArenaManager.lua:208-221). The client's -1 "all" is not used: its server meaning is
//     unverified.
//   - Flower train: flower.train.receive.reward {trainUuid: Long} for the account's own init
//     flowerTrainArr entries that arrived (arrived 1, or now >= arriveTime)
//     (BaseSingleFlowerTrainData.lua:60-70, 273-279).
//
// Not sent: zone mobilization (the attack/defend thresholds need more tracing), meteorite rewards
// (reward ids depend on a client-side season mapping), alliance.challenge.new.reward.get (it
// consumes keys), torch-relay tasks and banquet v2 (only partly traced).
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
	redBossClaimProgress   = 1
	redBossClaimAchievemnt = 2
)

// sevenDayAllRewardIDs are the type-34 events with showUiType 3, which claim all boxes at once.
var sevenDayAllRewardIDs = map[int32]bool{95003: true, 95005: true}

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
}

func runEventClaims(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	steps := []func(){
		func() { claimCrystalBoss(s, b) },
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

func claimCrystalBoss(s *ActivitySweep, b *evBatch) {
	for _, a := range s.Open(actTypeCrystalBoss) {
		if msg := b.send("crystal boss progress", redBossProgressCmd, sfs.NewSFSObject()); msg != nil {
			if n, _ := evNum(msg.Params, "claimableCount"); n > 0 {
				p := sfs.NewSFSObject()
				p.PutInt("type", redBossClaimProgress)
				b.send("crystal boss progress rewards", redBossClaimAllCmd, p)
			}
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("activityId", a.IDString())
		if msg := b.send("crystal boss achievements", redBossAchievementCmd, p); msg != nil {
			for _, t := range evObjects(msg.Params, "tasks") {
				if st, _ := evNum(t, "state"); st == eventTaskCanReceive {
					p := sfs.NewSFSObject()
					p.PutInt("type", redBossClaimAchievemnt)
					b.send("crystal boss achievement rewards", redBossClaimAllCmd, p)
					break
				}
			}
		}
		if msg := b.send("crystal boss march", redBossMarchCmd, sfs.NewSFSObject()); msg != nil {
			target, _ := evNum(msg.Params, "dailyRewardTarget")
			atk, _ := evNum(msg.Params, "dailyAtkCount")
			claimed, ok := evNum(msg.Params, "dailyRewardClaimed")
			if target > 0 && atk >= target && ok && claimed == 0 {
				b.send("crystal boss daily", redBossDailyCmd, sfs.NewSFSObject())
			}
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

// sevenDayDue returns the 1-based score boxes reached with the free or (VIP level allowing) the VIP
// reward unclaimed (ActSevenDayV2Info:CheckRedDot, :112-134; the v1 boxes read the same fields).
func sevenDayDue(info *sfs.SFSObject, vipLevel int64) []int32 {
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
		if (okFree && free == 0) || (okVip && vip == 0 && vipLevel >= needVip) {
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
		due := sevenDayDue(info, evVIPLevel(in))
		if len(due) == 0 {
			continue
		}
		if sevenDayAllRewardIDs[a.ID] {
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
		if len(sevenDayDue(info, evVIPLevel(in))) > 0 && !b.dead {
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
		data := evObject(rank.Params, "rankData")
		count, _ := evNum(data, "battleCount")
		for i, box := range evObjects(data, "dailyReward") {
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
