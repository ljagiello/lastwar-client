package sfs

// Request keys put on the wire by fireworks-scan and fireworks (internal/game/feature_fireworks.go):
// al.rank, world.get.block (WorldGetBlockMessage.CSSetData) and get.fireworks.gift. None carries a
// credential. ownerUid is a game uid like the already-classified "uid": it identifies an account but
// authenticates nothing.
func init() {
	for _, k := range []string{
		"allianceId",   // al.rank: the player's alliance id
		"bigMap",       // world.get.block: 0 on the normal map
		"blockSize",    // world.get.block: tiles per AOI block side (10 at viewLvl 0)
		"clearUuidSet", // world.get.block: 1 on the first request, the server forgets sent points
		"leftBottom",   // world.get.block: tile index of the view's bottom-left corner
		"ownerUid",     // get.fireworks.gift: uid of the HQ the chest sits on
		"rightTop",     // world.get.block: tile index of the view's top-right corner
		"timeStamp",    // world.get.block: (int) of the server ms clock
		"viewLvl",      // world.get.block: server LOD 0..2
		"worldId",      // world.get.block: 0 outside battlefields
		"x",            // world.get.block: camera tile x
		"y",            // world.get.block: camera tile y
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
