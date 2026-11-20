package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestHeroWishClaimWire pins hero-wish-claim's wire shape as literals from the 1.0.370 client:
// hero.lottery.receive.wish (MsgDefines.lua:2061) {id: UtfString lotteryId}:
// HeroLotteryClaimWishMessage.lua:7, sent by LotteryDataManager.lua:478. Nothing else is sent: the
// pools come from init lotteryFreeInfo, and the wish hero is never switched.
func TestHeroWishClaimWire(t *testing.T) {
	withEvNow(t, evTestNow)
	in := wishInit(wishPool{id: "221501", wishSwitch: true, need: 200, cur: 200, hero: "100005"},
		wishPool{id: "221602", wishSwitch: true, need: 200, cur: 230, hero: "100007"})
	got := bWireRun(t, "hero-wish-claim", in, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	bWireCheck(t, got, []bWireReq{
		bReq("hero.lottery.receive.wish", bStr("id", "221501")),
		bReq("hero.lottery.receive.wish", bStr("id", "221602")),
	})
}
