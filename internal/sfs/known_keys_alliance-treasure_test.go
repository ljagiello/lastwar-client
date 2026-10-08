package sfs

// Request keys put on the wire by alliance-treasures (internal/game/feature_alliance_treasures.go):
// detect.event.claim.treasure {uuid, targetServer}. Neither carries a credential.
func init() {
	for _, k := range []string{
		"targetServer", // detect.event.claim.treasure: the server the treasure point is on
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
