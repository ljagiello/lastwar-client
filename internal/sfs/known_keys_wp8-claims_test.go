package sfs

// Request keys put on the wire by the wp8 claim features (internal/game/feature_ghost_recon.go;
// the others reuse keys already listed). ownerServer is a game server id, the same class as the
// already-classified "serverId": it identifies a server and authenticates nothing.
func init() {
	for _, k := range []string{
		"ownerServer", // ghost.recon.batch.reward: the player's source server id (PlayerInfo:GetSourceServerId)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
