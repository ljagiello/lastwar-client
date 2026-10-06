package sfs

// Reviewed request keys added by the duel spend features: sLevel (a unit tier, 1-11) and sNum (a
// unit count) of building.camp.training (internal/game/feature_duel_troop_training.go).
func init() {
	for _, k := range []string{"sLevel", "sNum"} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
