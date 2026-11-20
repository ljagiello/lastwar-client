package game

import (
	"log/slog"
	"slices"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Small leftover claims of manual play (MASTER.md G46, G47, G48), each an OPT-IN feature of its
// own, claims only. Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet. Every
// claim reply is logged at Info; an errorCode stops that feature's claims (no benign codes).
//
//   - sky-battle-boxes (G46, cov-ui-store F5 and its verdict): lw.get.sky.battle.stage {chapter:
//     Int -1, stageType: Int 0|1} (LWUIStageSkyBattleChapterView.lua:166, 195;
//     LwGetSkyBattleStageMessage.lua:4-19) answers chapterInfo[{chapter, rewardedTarget[],
//     stageInfo[{star}]}]. A chapter's boxes are table chapter_box "stars|reward;..." of
//     lw_sky_battle_chapter (stageType 0) or lw_sky_battle_chapter_upgrade (stageType 1); a box is
//     Got when rewardedTarget holds its star threshold (LWSkyBattleChapterManager.lua:99-154;
//     LWSkyBattleGrowthChapterManager.lua:144-185), and the chapter's stars are the sum of
//     stageInfo[].star (ChapterContentComponent.lua:197-202). A box not Got whose threshold the stars
//     reach sends lw.sky.battle.chapter.box.reward {chapter: Int, stageType: Int}
//     (ChapterContentComponent.lua:292-302; LwSkyBattleChapterBoxRewardMessage.lua:4-7). The claim
//     names no box, so each reply's rewardedTarget is read and the claim repeats only while it
//     changed and a reached box is left, at most once per box. The chapter view's open path is the
//     command's only sender (LWUIStageSkyBattleChapterView.lua:156-196), and the view opens only
//     while its mode is open, so a mode is read only then: stageType 0 while the init skyBattle
//     {st, et} window holds, st <= now <= et in ms with both present (IsOpen,
//     LWSkyBattleChapterManager.lua:43-48, 307-313, fed by InitMessage.lua:568; the city bubble,
//     BuildBubbleManager.lua:3773-3783, and OnEnterCity, :370-375, check it), and stageType 1 while
//     an activity of type 371 (ActSkyBattleGrowth, EnumType.lua:6499) is open (IsOpen is
//     GetOneOpenActivityByType then CheckIsSend, LWSkyBattleGrowthChapterManager.lua:717-724;
//     ActivityListDataManager.lua:1853-1864, 969-981). Tables 40413 have no type-371 row, so the
//     growth mode stays dormant until a regenerated table has one. The lowest chapter's
//     main_building_level (6 and 8 in 40413) is an extra guard. Stage fights report client
//     results: never.
//   - newbie-arena-rewards (G47, cov-ui-store F6 and its verdict): the new-server arenas, types 140
//     (ArenaNewbie) and 143 (ArenaNewbieV2) (EnumType.lua:6400, 6404). activity.arena.reward.preview
//     / activity.arena.v2.reward.preview {activityId: Int} answer achieveRewards[{id, state,
//     targetRank, reward}] (LWActivityArenaRewardsView.lua:227-234; ActivityArenaRewardPreivewMessage.lua
//     and ActivityArenaV2RewardPreviewMessage.lua:4-7). A row in state 1 is claimable and sends
//     activity.arena.v2.receive for the V2 arena's activity, else activity.arena.receive, both
//     {activityId: Int, id: Int} (LWActivityArenaRewardsItemAchieve.lua:105-111, 131-140;
//     ActivityArenaReceiveMessage.lua:4-8; ActivityArenaV2ReceiveMessage.lua:4-8). Never
//     activity.arena.buy.times (diamonds) or arena battles.
//   - birthday-set-reward (G48, cov-ui-store F8 and its verdict): the one-time reward for a set
//     birthday. Init birthday_function {birthday, rewarded} (BirthdayDataManager.lua:79-81): with a
//     non-empty birthday and rewarded not truthy (:665-679) the button sends
//     user.birthday.set.reward {} (BirthdayRewardContent.lua:91-98;
//     UserBirthdaySetRewardMessage.lua:4-6). Setting the birthday itself is a profile change and is
//     never sent.
//   - mastery-newbie-reward (G49, cov-ui-city F5): init desertTalent {level, newbieRewardStatus}
//     (MasteryManager.lua:573-576; MasteryData.lua:32-40, 74-75). GetNewbieRewardStatus is 1 (can
//     claim) when newbieRewardStatus is 1 and the mastery level reaches DataConfig mastery_newbie.k2
//     (10 in table item 40413) (MasteryManager.lua:994-1005), and the button then sends
//     lw.season.mastery.newbie.reward.claim {} (LWUIMasteryHaveChoosedComp.lua:444-452;
//     LwSeasonMasteryNewbieRewardClaimMessage.lua:4-6).
//   - counter-attack-rounds (G51, cov-dc-world F5 and its verdict): Counter Attack (type 226,
//     EnumType.lua:6428; ActivityListDataManager.lua:572-576). dark.knight.alliance.reward.info {aid:
//     Int, type: Int 1} answers allianceRoundReward[{round, state}] (CounterAttackDataManager.lua:89-96;
//     DarkKnightAllianceRewardInfoMessage.lua:4-22), a round in TaskState.CanReceive (1) being
//     claimable (:230-242). The page's claim-all sends dark.knight.alliance.award.round {aid: Int,
//     round: Int 1, all: Bool true} (CounterAttackTaskPage.lua:27-29; :113-115;
//     DarkKnightAllianceAwardRoundMessage.lua:4-14); the reply carries rewards and the refreshed
//     allianceRoundReward. The round rewards are alliance progress, so this also needs an alliance
//     (an extra guard).
//
// Not automated (G48): the first-pay stash (first.recharge.reward) needs IsHasBoughtFirstPay, whose
// function-unlock, land and first-pay-pack state Go does not model (FirstPayManager.lua:249-275,
// LWResourceLackCell.lua:1581-1595); alliance.challenge.new.reward.get waits for the user's
// decision (MASTER §6.1) and stays excluded (feature_event_claims.go). G49: the stage-feature
// chapter boxes need LWEasyStageFeatureChapterManager's doneStageIds and chapter table
// (LWUIEasyStageFeatureChapterView.lua:316-323, 388-405), and the S0 city-event tasks arrive only by
// push.city.event.s0.task (LWBeginnerDirectorManager.lua:215-238). G50 and the other G51 rows keep
// their claim state where this package has not read it (MASTER G50, G51).
const (
	skyBattleBoxesFeature = "sky-battle-boxes"
	skyBattleStageCmd     = "lw.get.sky.battle.stage"
	skyBattleBoxCmd       = "lw.sky.battle.chapter.box.reward"
	skyBattleAllChapters  = -1
	skyBattleNormalMode   = 0
	skyBattleGrowthMode   = 1
	skyBattleGrowthType   = 371 // EnumActivity.ActSkyBattleGrowth (EnumType.lua:6499)
	newbieArenaFeature    = "newbie-arena-rewards"
	newbieArenaType       = 140
	newbieArenaV2Type     = 143
	newbieArenaClaimable  = 1
	birthdaySetFeature    = "birthday-set-reward"
	birthdaySetRewardCmd  = "user.birthday.set.reward"
	masteryNewbieFeature  = "mastery-newbie-reward"
	masteryNewbieClaimCmd = "lw.season.mastery.newbie.reward.claim"
	masteryNewbieMinLevel = 10 // DataConfig mastery_newbie.k2, table item 40413
	masteryNewbieCanClaim = 1
	counterAttackFeature  = "counter-attack-rounds"
	counterAttackActType  = 226
	counterAttackInfoCmd  = "dark.knight.alliance.reward.info"
	counterAttackClaimCmd = "dark.knight.alliance.award.round"
	counterAttackRoundTyp = 1 // reward.info type 1: the alliance rounds (0 is the personal rank)
	counterAttackCanClaim = 1 // TaskState.CanReceive
)

// skyBattleModes are the two chapter tables (40413): every chapter's chapter_box thresholds (7
// rows of lw_sky_battle_chapter, 4 of lw_sky_battle_chapter_upgrade), and the lowest
// main_building_level a chapter asks.
var skyBattleModes = []struct {
	stageType  int32
	name       string
	minHQ      int32
	thresholds map[int64][]int64 // chapter -> star thresholds
}{
	{skyBattleNormalMode, "chapters", 6, map[int64][]int64{
		1: {6, 12, 18}, 2: {6, 12, 18}, 3: {6, 12, 18}, 4: {6, 12, 18}, 5: {6, 12, 18}, 6: {6, 12, 18}, 7: {6, 12, 18},
	}},
	{skyBattleGrowthMode, "growth chapters", 8, map[int64][]int64{
		1: {6, 12, 18, 24}, 2: {6, 12, 18, 24}, 3: {6, 12, 18, 24}, 4: {6, 12, 18, 24},
	}},
}

// newbieArenas maps the arena types to their preview and receive commands.
var newbieArenas = []struct {
	actType            int32
	previewCmd, getCmd string
}{
	{newbieArenaType, "activity.arena.reward.preview", "activity.arena.receive"},
	{newbieArenaV2Type, "activity.arena.v2.reward.preview", "activity.arena.v2.receive"},
}

func init() {
	for _, f := range []Feature{
		{Name: skyBattleBoxesFeature, Run: runSkyBattleBoxes,
			Summary: "OPT-IN: claim sky-battle chapter star boxes the played stages have reached (static-only, not validated live)"},
		{Name: newbieArenaFeature, Run: runNewbieArenaRewards,
			Summary: "OPT-IN: claim reached achievement rewards of the new-server arenas (types 140, 143) (static-only, not validated live)"},
		{Name: birthdaySetFeature, Run: runBirthdaySetReward,
			Summary: "OPT-IN: claim the one-time reward for a birthday the player already set (static-only, not validated live)"},
		{Name: masteryNewbieFeature, Run: runMasteryNewbieReward,
			Summary: "OPT-IN: claim the season mastery newbie reward once the mastery level reaches 10 (static-only, not validated live)"},
		{Name: counterAttackFeature, Run: runCounterAttackRounds,
			Summary: "OPT-IN: claim reached Counter Attack alliance round rewards with the client's claim-all (static-only, not validated live)"},
	} {
		f.DefaultOn = false
		registerFeature(f)
	}
}

func runSkyBattleBoxes(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	hq := activityHQLevel(in)
	b := &evBatch{conn: conn}
	// The chapter view's only opener, the city bubble, opens the growth mode whenever it is open
	// (BuildBubbleManager.lua:3773-3781), so the client never reads the normal chapters then.
	growthOpen := skyBattleModeOpen(conn, in, skyBattleGrowthMode)
	for _, m := range skyBattleModes {
		if b.dead {
			break
		}
		if hq < m.minHQ {
			continue
		}
		if m.stageType == skyBattleNormalMode && growthOpen {
			slog.Info(skyBattleBoxesFeature + ": the growth mode is open, which the city bubble opens instead; normal chapters not read")
			continue
		}
		if !skyBattleModeOpen(conn, in, m.stageType) {
			slog.Debug(skyBattleBoxesFeature+": mode closed, not read", "stageType", m.stageType)
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("chapter", skyBattleAllChapters)
		p.PutInt("stageType", m.stageType)
		r := b.send(skyBattleBoxesFeature+": "+m.name, skyBattleStageCmd, p)
		if r == nil {
			continue
		}
		info := map[int64]*sfs.SFSObject{}
		for _, c := range evObjects(r.Params, "chapterInfo") {
			if id, ok := evNum(c, "chapter"); ok {
				info[id] = c
			}
		}
		slog.Info(skyBattleBoxesFeature+": "+m.name, "stageType", m.stageType, "chapters", len(info), "keys", r.Params.Keys())
		chapters := make([]int64, 0, len(m.thresholds))
		for ch := range m.thresholds {
			chapters = append(chapters, ch)
		}
		slices.Sort(chapters)
		for _, ch := range chapters {
			if c := info[ch]; c != nil {
				slog.Info(skyBattleBoxesFeature+": chapter read", "stageType", m.stageType, "chapter", ch,
					"stars", skyBattleChapterStars(c), "rewardedTarget", evNums(c, "rewardedTarget"))
			}
		}
		for _, ch := range chapters {
			if !claimSkyBattleChapter(b, m.stageType, ch, m.thresholds[ch], info[ch]) {
				break
			}
		}
	}
	return b.err()
}

// skyBattleModeOpen is the mode's IsOpen, without which the client never opens the chapter view
// that sends lw.get.sky.battle.stage: the init skyBattle window for stageType 0, an open type-371
// activity for stageType 1.
func skyBattleModeOpen(conn *session.GameConn, in *Init, stageType int32) bool {
	switch stageType {
	case skyBattleNormalMode:
		return skyBattleWindowOpen(in.Object("skyBattle"), evNow().UnixMilli())
	case skyBattleGrowthMode:
		return len(Activities(conn, in).Open(skyBattleGrowthType)) > 0
	}
	return false
}

// skyBattleWindowOpen is LWSkyBattleChapterManager:IsOpen over init skyBattle {st, et} (ms): both
// present (Lua-truthy, so a 0 counts) and st <= nowMs <= et.
func skyBattleWindowOpen(sb *sfs.SFSObject, nowMs int64) bool {
	st, sok := evNum(sb, "st")
	et, eok := evNum(sb, "et")
	return sok && eok && nowMs >= st && nowMs <= et
}

// skyBattleChapterStars sums a chapterInfo entry's stageInfo[].star.
func skyBattleChapterStars(info *sfs.SFSObject) int64 {
	var stars int64
	for _, st := range evObjects(info, "stageInfo") {
		n, _ := evNum(st, "star")
		stars += n
	}
	return stars
}

// claimSkyBattleChapter claims one chapter's reached boxes; false when a claim failed.
func claimSkyBattleChapter(b *evBatch, stageType int32, chapter int64, thresholds []int64, info *sfs.SFSObject) bool {
	if info == nil {
		return true
	}
	stars := skyBattleChapterStars(info)
	got := evNums(info, "rewardedTarget")
	due := skyBattleBoxesDue(thresholds, stars, got)
	if len(due) == 0 {
		return true
	}
	slog.Info(skyBattleBoxesFeature+": chapter boxes due", "stageType", stageType, "chapter", chapter, "stars", stars,
		"rewardedTarget", got, "due", due)
	for range thresholds {
		p := sfs.NewSFSObject()
		p.PutInt("chapter", int32(chapter))
		p.PutInt("stageType", stageType)
		r := b.send(skyBattleBoxesFeature+": chapter box", skyBattleBoxCmd, p)
		if r == nil {
			return false
		}
		evTaskLogReply(skyBattleBoxesFeature+": chapter box", skyBattleBoxCmd, r.Params, "stageType", stageType,
			"chapter", chapter, "rewardedTarget", evNums(r.Params, "rewardedTarget"))
		if _, ok := r.Params.Get("rewardedTarget"); !ok {
			return true // nothing tells whether more boxes are left: stop, the next run re-reads
		}
		after := evNums(r.Params, "rewardedTarget")
		next := skyBattleBoxesDue(thresholds, stars, after)
		if len(next) == 0 || len(next) >= len(due) {
			return true // all claimed, or the claim changed nothing
		}
		due = next
	}
	return true
}

// skyBattleBoxesDue lists the thresholds stars reach that rewarded lacks.
func skyBattleBoxesDue(thresholds []int64, stars int64, rewarded []int64) []int64 {
	var due []int64
	for _, t := range thresholds {
		if t <= stars && !slices.Contains(rewarded, t) {
			due = append(due, t)
		}
	}
	return due
}

func runNewbieArenaRewards(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, ar := range newbieArenas {
		for _, a := range s.Open(ar.actType) {
			if b.dead {
				return b.err()
			}
			p := sfs.NewSFSObject()
			p.PutInt("activityId", a.ID)
			info, err := s.Info(newbieArenaFeature+" reward preview", ar.previewCmd, a, p)
			if err != nil {
				b.add(err)
				continue
			}
			var due []int64
			for _, r := range evObjects(info, "achieveRewards") {
				id, ok := evNum(r, "id")
				if st, sok := evNum(r, "state"); ok && sok && st == newbieArenaClaimable {
					due = append(due, id)
				}
			}
			slog.Info(newbieArenaFeature+": reward preview", "activity", a.ID, "type", ar.actType,
				"achievements", len(evObjects(info, "achieveRewards")), "claimable", due)
			for _, id := range due {
				c := sfs.NewSFSObject()
				c.PutInt("activityId", a.ID)
				c.PutInt("id", int32(id))
				if !evTaskSend(b, newbieArenaFeature+": achievement "+strconv.FormatInt(id, 10), ar.getCmd, c, "activity", a.ID) {
					break
				}
			}
		}
	}
	return b.err()
}

func runBirthdaySetReward(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	bf := in.Object("birthday_function")
	if bf == nil || evString(bf, "birthday") == "" || birthdayRewarded(bf) {
		return nil
	}
	b := &evBatch{conn: conn}
	evTaskSend(b, birthdaySetFeature+": claim", birthdaySetRewardCmd, sfs.NewSFSObject(), "birthdaySet", true) // never log the date (PII)
	return b.err()
}

func runMasteryNewbieReward(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	m := in.Object("desertTalent")
	status, _ := evNum(m, "newbieRewardStatus")
	level, _ := evNum(m, "level")
	if status != masteryNewbieCanClaim || level < masteryNewbieMinLevel {
		return nil
	}
	b := &evBatch{conn: conn}
	evTaskSend(b, masteryNewbieFeature+": claim", masteryNewbieClaimCmd, sfs.NewSFSObject(), "masteryLevel", level)
	return b.err()
}

func runCounterAttackRounds(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !claimInAlliance(in) {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(counterAttackActType) {
		if b.dead {
			break
		}
		p := sfs.NewSFSObject()
		p.PutInt("aid", a.ID)
		p.PutInt("type", counterAttackRoundTyp)
		info, err := s.Info(counterAttackFeature+" round rewards", counterAttackInfoCmd, a, p)
		if err != nil {
			b.add(err)
			continue
		}
		var due []int64
		for _, r := range evObjects(info, "allianceRoundReward") {
			round, ok := evNum(r, "round")
			if st, sok := evNum(r, "state"); ok && sok && st == counterAttackCanClaim {
				due = append(due, round)
			}
		}
		slog.Info(counterAttackFeature+": round rewards", "activity", a.ID,
			"rounds", len(evObjects(info, "allianceRoundReward")), "claimable", due)
		if len(due) == 0 {
			continue
		}
		c := sfs.NewSFSObject()
		c.PutInt("aid", a.ID)
		c.PutInt("round", 1)
		c.PutBool("all", true)
		if r := b.send(counterAttackFeature+": claim all", counterAttackClaimCmd, c); r != nil {
			evTaskLogReply(counterAttackFeature+": claim all", counterAttackClaimCmd, r.Params, "activity", a.ID,
				"claimable", due, "rewards", describeGrants(r.Params, "rewards"))
		}
	}
	return b.err()
}

// birthdayRewarded is IsHaveGetBirthdayFirstSetReward: rewarded is truthy in Lua, which every
// value but an absent field or false is (a 0 counts as claimed).
func birthdayRewarded(bf *sfs.SFSObject) bool {
	v, ok := bf.Get("rewarded")
	if !ok || v.Val == nil {
		return false
	}
	if b, isBool := v.Val.(bool); isBool {
		return b
	}
	return true
}
