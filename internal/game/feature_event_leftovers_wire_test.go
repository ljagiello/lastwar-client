package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestEventLeftoversWire pins, as literals, every command, param key, SFS type and sent constant of
// the five leftover claims:
//   - lw.get.sky.battle.stage {chapter: Int -1, stageType: Int 0|1} (MsgDefines.lua:72;
//     LwGetSkyBattleStageMessage.lua:4-8; LWUIStageSkyBattleChapterView.lua:166 sends (-1, 1), :195
//     sends (-1, 0)) and lw.sky.battle.chapter.box.reward {chapter: Int, stageType: Int 0|1}
//     (MsgDefines.lua:874; LwSkyBattleChapterBoxRewardMessage.lua:4-8; ChapterContentComponent.lua:302
//     sends the chapter id and `growthMode and 1 or 0`).
//   - activity.arena.reward.preview / activity.arena.v2.reward.preview {activityId: Int}
//     (MsgDefines.lua:1640, 1646; ActivityArenaRewardPreivewMessage.lua and
//     ActivityArenaV2RewardPreviewMessage.lua:4-7; UIArenaNewbie.lua:184, LWNewbieArenaV2Page.lua:184)
//     and activity.arena.receive / activity.arena.v2.receive {activityId: Int, id: Int}
//     (MsgDefines.lua:1639, 1648; ActivityArenaReceiveMessage.lua and ActivityArenaV2ReceiveMessage.lua:
//     4-8; LWActivityArenaRewardsItemAchieve.lua:136-138).
//   - user.birthday.set.reward {} (MsgDefines.lua:2755; UserBirthdaySetRewardMessage.lua:4-6;
//     BirthdayRewardContent.lua:97).
//   - lw.season.mastery.newbie.reward.claim {} (MsgDefines.lua:94;
//     LwSeasonMasteryNewbieRewardClaimMessage.lua:4-6; LWUIMasteryHaveChoosedComp.lua:447).
//   - dark.knight.alliance.reward.info {aid: Int, type: Int 1} (MsgDefines.lua:1315;
//     DarkKnightAllianceRewardInfoMessage.lua:4-8; CounterAttackDataManager.lua:94 sends type 1) and
//     dark.knight.alliance.award.round {aid: Int, round: Int 1, all: Bool true} (MsgDefines.lua:1316;
//     DarkKnightAllianceAwardRoundMessage.lua:4-13, the no-round branch; CounterAttackDataManager.lua:114).
//
// Activity ids are rows of table activity 40413 (activity_tables_gen.go): 100000 type 140, 100001
// type 143, 1000016 type 226. The growth row is added with type 371 (ActSkyBattleGrowth,
// EnumType.lua:6499), which 40413 lacks.
func TestEventLeftoversWire(t *testing.T) {
	skyReply := func(chapter int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
		return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			r := evOK()
			switch cmd {
			case "lw.get.sky.battle.stage":
				r.PutSFSArray("chapterInfo", cArr(skyChapter(chapter, nil, 3, 3))) // 6 stars: the 6-star box
			case "lw.sky.battle.chapter.box.reward":
				r.PutIntArray("rewardedTarget", []int32{6})
			}
			return r
		}
	}
	cWireRun(t, []cWireCase{
		{
			name: "sky-battle-boxes normal chapters",
			setup: func(*testing.T) *Init {
				return skyBattleWindow(evInit(10), evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
			},
			reply: skyReply(1),
			run:   runSkyBattleBoxes,
			want: []cWireReq{
				{"lw.get.sky.battle.stage", []cWireParam{{"chapter", sfs.SFSInt, int32(-1)}, {"stageType", sfs.SFSInt, int32(0)}}},
				{"lw.sky.battle.chapter.box.reward", []cWireParam{{"chapter", sfs.SFSInt, int32(1)}, {"stageType", sfs.SFSInt, int32(0)}}},
			},
		},
		{
			name: "sky-battle-boxes growth chapters",
			setup: func(t *testing.T) *Init {
				withSkyBattleGrowthRow(t)
				return evInit(14, evActivity(777371))
			},
			reply: skyReply(2),
			run:   runSkyBattleBoxes,
			want: []cWireReq{
				{"lw.get.sky.battle.stage", []cWireParam{{"chapter", sfs.SFSInt, int32(-1)}, {"stageType", sfs.SFSInt, int32(1)}}},
				{"lw.sky.battle.chapter.box.reward", []cWireParam{{"chapter", sfs.SFSInt, int32(2)}, {"stageType", sfs.SFSInt, int32(1)}}},
			},
		},
		{
			name:  "newbie-arena-rewards",
			setup: func(*testing.T) *Init { return evInit(30, evActivity(100000), evActivity(100001)) },
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "activity.arena.reward.preview" || cmd == "activity.arena.v2.reward.preview" {
					r.PutSFSArray("achieveRewards", cArr(obj("id", 2, "state", 1, "targetRank", 50)))
				}
				return r
			},
			run: runNewbieArenaRewards,
			want: []cWireReq{
				{"activity.arena.reward.preview", []cWireParam{{"activityId", sfs.SFSInt, int32(100000)}}},
				{"activity.arena.receive", []cWireParam{{"activityId", sfs.SFSInt, int32(100000)}, {"id", sfs.SFSInt, int32(2)}}},
				{"activity.arena.v2.reward.preview", []cWireParam{{"activityId", sfs.SFSInt, int32(100001)}}},
				{"activity.arena.v2.receive", []cWireParam{{"activityId", sfs.SFSInt, int32(100001)}, {"id", sfs.SFSInt, int32(2)}}},
			},
		},
		{
			name: "birthday-set-reward",
			setup: func(*testing.T) *Init {
				in := evInit(30)
				in.Raw.PutSFSObject("birthday_function", obj("birthday", "5-4"))
				return in
			},
			reply: func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() },
			run:   runBirthdaySetReward,
			want:  []cWireReq{{"user.birthday.set.reward", nil}},
		},
		{
			name: "mastery-newbie-reward",
			setup: func(*testing.T) *Init {
				in := evInit(30)
				in.Raw.PutSFSObject("desertTalent", obj("level", 10, "newbieRewardStatus", 1))
				return in
			},
			reply: func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() },
			run:   runMasteryNewbieReward,
			want:  []cWireReq{{"lw.season.mastery.newbie.reward.claim", nil}},
		},
		{
			name:  "counter-attack-rounds",
			setup: func(*testing.T) *Init { return counterAttackInit(true) },
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "dark.knight.alliance.reward.info" {
					r.PutSFSArray("allianceRoundReward", cArr(obj("round", 1, "state", 1))) // TaskState.CanReceive
				}
				return r
			},
			run: runCounterAttackRounds,
			want: []cWireReq{
				{"dark.knight.alliance.reward.info", []cWireParam{{"aid", sfs.SFSInt, int32(1000016)}, {"type", sfs.SFSInt, int32(1)}}},
				{"dark.knight.alliance.award.round", []cWireParam{
					{"aid", sfs.SFSInt, int32(1000016)}, {"round", sfs.SFSInt, int32(1)}, {"all", sfs.SFSBool, true}}},
			},
		},
	})
}
