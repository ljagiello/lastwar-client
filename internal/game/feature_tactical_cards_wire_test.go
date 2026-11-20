package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestTacticalCardsWire pins, as literals, every command, param key, SFS type and sent value of the
// two tactical card claims:
//   - battle.card.score.info {} (MsgDefines.lua:2432; BattleCardScoreInfoMessage.lua:5-7;
//     TacticalCardDataManager.lua:73) and battle.card.score.reward {} (MsgDefines.lua:2430;
//     BattleCardScoreRewardMessage.lua:5-7; CardBoxRewardEntrance.lua:138).
//   - battle.card.collect.list {} (MsgDefines.lua:2427; GetBattleCardBookDataMessage.lua:5-7;
//     TacticalCardDataManager.lua:242) and battle.card.collect.reward {index: Int, cardType: Int,
//     cfgId: Int} (MsgDefines.lua:2428; ClaimBattleCardStageRewardMessage.lua:5-10; UITCCardBookView.lua:
//     226 sends the stage index, the card type tab and the collection's cfgId).
//
// Season 5 is past battle_card_param.k6 "4;2" (table item 40413); row 501 of battle_card_box_reward
// 40413 is season 5, point 1000; cfgId 50101 is a battle_card_reward 40413 row.
func TestTacticalCardsWire(t *testing.T) {
	cWireRun(t, []cWireCase{
		{
			name:  "tactical-card-box-points",
			setup: func(*testing.T) *Init { return tacticalInit(5, 30*24*time.Hour) },
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "battle.card.score.info" {
					r.PutInt("score", 1000)
					r.PutIntArray("rewardedList", nil)
				}
				return r
			},
			run: runTacticalBoxPoints,
			want: []cWireReq{
				{"battle.card.score.info", nil},
				{"battle.card.score.reward", nil},
			},
		},
		{
			name: "tactical-card-collection",
			setup: func(*testing.T) *Init {
				return tacticalInit(5, 10*24*time.Hour, tacticalCollect(50101, 1, "0;2|1;1"))
			},
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "battle.card.collect.list" {
					r.PutSFSArray("collectCards", cArr(obj("cardType", 1, "cards", "1")))
				}
				return r
			},
			run: runTacticalCollection,
			want: []cWireReq{
				{"battle.card.collect.list", nil},
				{"battle.card.collect.reward", []cWireParam{
					{"index", sfs.SFSInt, int32(1)}, {"cardType", sfs.SFSInt, int32(1)}, {"cfgId", sfs.SFSInt, int32(50101)}}},
			},
		},
	})
}
