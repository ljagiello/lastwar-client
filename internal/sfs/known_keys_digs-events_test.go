package sfs

// Request keys added by the dig-game features (internal/game/feature_treasure_hunt.go,
// feature_dig_vaults.go), reviewed against their Lua senders; none carries a credential or PII.
func init() {
	for _, k := range []string{
		"bigRewardIndex", // choose.dig[.v2].big.reward: the grand-prize slot, 1-based
		"digIndex",       // start.activity.dig[.v2]: the card dug, 1-12
		"pos",            // *.dig.game.open / *.alliance.block.reward / alliance.boss.dig.game.reward: a vault stone index
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
