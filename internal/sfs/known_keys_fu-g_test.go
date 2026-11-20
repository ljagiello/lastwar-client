package sfs

// Request keys put on the wire by the chat-driven twins (FU-G): alliance-treasures-chat
// (internal/game/feature_alliance_treasures_chat.go) sends detect.event.get.treasure.claim.info
// {uuid, source, targetServer} (DetectEventGetTreasureClaimInfoMessage.lua:5-17), and
// fireworks-chat (internal/game/feature_fireworks_chat.go) find.fireworks.gift.world.point
// {uuid, ownerUid} (FindFireworksGiftWorldPointMessage.lua:4-8). uuid, targetServer and ownerUid
// are classified already. None carries a credential.
func init() {
	for _, k := range []string{
		"source", // SeeDetectEventGetTreasureClaimInfoType: 1 Chat, 2 World (EnumType.lua:15270)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
