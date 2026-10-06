package sfs

// Request keys put on the wire by the event features (internal/game/activities.go and
// feature_*.go from cf-features-events). All are gameplay identifiers or flags -- activity, task,
// stage, level and card ids, claim-all sentinels -- reviewed against their Lua senders; none
// carries a credential or PII.
func init() {
	for _, k := range []string{
		"activityId",
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
