package sfs

// Request keys put on the wire by radar-execute, radar-overflow and radar-inventory
// (internal/game/feature_radar_execute.go, feature_radar_overflow.go). All are radar task ids or
// flags, reviewed against their Lua senders; none carries a credential or PII.
func init() {
	for _, k := range []string{
		"eventType", // detect.event.help.start/end: DetectEventType.HELPER (18)
		"openWnd",   // get.detect.info: whether the radar window is open
		"uuid",      // radar task uuid (start.pick.garbage, finish.sampling, receive.detect.event.reward, ...)
		"uuidList",  // detect.event.batch.put.point.in.world: radar task uuids joined with "|"
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
