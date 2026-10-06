package sfs

// Request keys put by internal/game/feature_duel_secret_task_starts.go (duel-secret-tasks), the
// hero.dispatch.start params of DispatchStartMessage.lua:4-9. Gameplay values, not credentials or
// PII.
func init() {
	for _, k := range []string{
		"heroList",      // hero.dispatch.start: the dispatched heroes' uuids (LongArray)
		"marchDuration", // hero.dispatch.start: the fake march's travel time in ms
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
