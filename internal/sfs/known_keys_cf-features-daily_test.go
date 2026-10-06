package sfs

// Request keys put by the claim features in internal/game/feature_*.go (cf-features-daily). All
// are gameplay ids, flags or counters for claim commands; none is a credential or PII.
func init() {
	for _, k := range []string{
		"uuidArr",  // gather.collect.reward: Collect Rewards entry uuids
		"uuidList", // hero.dispatch.batch.reward: dispatch task uuids (LongArray via PutValue)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
