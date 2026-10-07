package sfs

// Request keys put by internal/game/feature_duel_speedups.go (duel-speedups). All are gameplay ids
// or the item list of a speed-up; none is a credential or PII.
func init() {
	for _, k := range []string{
		"bUUID",      // build.ccd.m.new: the upgrading building's uuid
		"isFixRuins", // build.ccd.m.new: ruin-repair flag, always false
		"itemIDs",    // build.ccd.m.new / queue.ccd.m.new: owned items to use, "tmpl;n|tmpl;n"
		"itemId",     // building.camp.accel: the same item list for the camp
		"qUUID",      // queue.ccd.m.new: the research or hospital queue uuid
		"uuid",       // building.camp.accel: the Military Camp's uuid
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
