package sfs

// Request keys put on the wire by treasure-digs (internal/game/feature_treasure_digs.go). All are
// dig board ids, brick numbers or flags, reviewed against their Lua senders; none carries a
// credential or PII.
func init() {
	for _, k := range []string{
		"buildingUuid", // building.dig.game.*: the city ruin building's uuid
		"checkOpenAll", // hero.dispatch.dig.game.open.block: 1 when the client thinks the brick uncovers every gem
		"eventUuid",    // detect.event.dig.game.*: the radar ruin event's uuid
		"pos",          // *.dig.game.open.block: the brick number, 1..width*height
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
