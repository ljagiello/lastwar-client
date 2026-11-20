package sfs

// Request keys put on the wire by duel-gacha-draw (internal/game/feature_duel_gacha_draw.go):
// al.duel.lottery.lottery {configId, num} (AlDuelLotteryLotteryMessage.lua:6-7). None carries a
// credential.
func init() {
	for _, k := range []string{
		"configId", // the duel gacha's config id from al.duel.lottery.get.info
		"num",      // draws in one request (1, the one-draw button)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
