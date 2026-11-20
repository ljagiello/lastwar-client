package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestDuelGachaWire runs duel-gacha and duel-gacha-plan through their registered Run and pins the
// wire shape as literals from the 1.0.370 client:
//
//   - al.duel.lottery.get.info (MsgDefines.lua:2690) with no params: AlDuelLotteryGetInfoMessage.lua
//     puts none, sent by AllyDuelScoreGachaManager.lua:134 while DataConfig switch
//     alliance_duel_zhuanpan is on (:131-136).
//   - al.duel.lottery.get.protect.reward (MsgDefines.lua:2689) {configId: Int}:
//     AlDuelLotteryGetProtectRewardMessage.lua:6, sent by AllyDuelScoreGachaManager.lua:212 with the
//     info reply's configId.
//
// The init turns on alliance_duel_zhuanpan and effect 92019 (EffectDefine.
// ALCOMPETE_SCORE_GACHA_UNLOCK, EnumType.lua:4480). The plan sends only the info read.
func TestDuelGachaWire(t *testing.T) {
	withEvNow(t, evTestNow)
	in := duelGachaInit(func(raw *sfs.SFSObject) {
		raw.PutSFSObject("dataConfig", duelObj("alliance_duel_zhuanpan", 1))
		raw.PutSFSObject("effect", duelObj("92019", 1.0))
	})
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "al.duel.lottery.get.info":
			return duelGachaInfo(25_000_000, 50, 50, 1) // configId 3, a full pity with one wish reward
		case "al.duel.lottery.get.protect.reward":
			return duelObj("protectNum", 0)
		}
		return evErr("999999")
	}
	for _, c := range []struct {
		feature string
		want    []bWireReq
	}{
		{"duel-gacha", []bWireReq{
			bReq("al.duel.lottery.get.info"),
			bReq("al.duel.lottery.get.protect.reward", bInt("configId", 3)),
		}},
		{"duel-gacha-plan", []bWireReq{bReq("al.duel.lottery.get.info")}},
	} {
		t.Run(c.feature, func(t *testing.T) {
			bWireCheck(t, bWireRun(t, c.feature, in, reply), c.want)
		})
	}
}
