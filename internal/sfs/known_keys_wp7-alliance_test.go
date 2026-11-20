package sfs

// Request keys put on the wire by the 1.0.370 audit's alliance package (WP7): base-bubbles
// (internal/game/feature_base_bubbles.go: thanksgiving.receive.base.reward,
// valentine.owner.base.reward) and s0-alliance-boss (internal/game/feature_s0_alliance_boss.go:
// receive.alliance.boss.s0.first.reward). None carries a credential. otherUid is a game uid like the
// already-classified "uid" and "ownerUid": it identifies an account but authenticates nothing.
func init() {
	for _, k := range []string{
		"difficultyLevel", // receive.alliance.boss.s0.first.reward: the S0 boss difficulty level
		"otherUid",        // thanksgiving.receive.base.reward: uid of the HQ the bubble sits on
		"statusId",        // thanksgiving/valentine base rewards: the lw_status id of the bubble
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
