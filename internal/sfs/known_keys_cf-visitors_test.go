package sfs

// Reviewed request keys added by the visitor features: activityId is a public event id
// (visitor.receive.reward, internal/game/feature_activity_visitors.go).
func init() {
	for _, k := range []string{"activityId"} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
