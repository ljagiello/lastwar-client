package sfs

// Request keys put on the wire by the event features (internal/game/activities.go and
// feature_*.go from cf-features-events). All are gameplay identifiers or flags -- activity, task,
// stage, level and card ids, claim-all sentinels -- reviewed against their Lua senders; none
// carries a credential or PII.
func init() {
	for _, k := range []string{
		"activityId", // event activity id (table `activity` row)
		"actId",      // Alliance Duel per-day event id
		"aid",        // event activity id (Arms Race, Cooking, Banquet)
		"day",        // sign-in day, 0 = every claimable day
		"eventType",  // Alliance Duel event type (15)
		"id",         // Cooking/Banquet menu row id
		"index",      // chest index, -1 = every reached chest
		"stage",      // Alliance Duel chest threshold score
		"taskId",     // battle-pass task id
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
