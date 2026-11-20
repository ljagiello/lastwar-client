package sfs

// Request keys put on the wire by red-packets (internal/game/feature_red_packets.go):
// open.red.packet {uuid, cfgId, chatType, serverId} (OpenRedPacketMessage.lua:5-10). None carries a
// credential.
func init() {
	for _, k := range []string{
		"cfgId",    // the red_packet table id from the chat card
		"chatType", // RedPacketManager.ChannelType of the room the card was in (2, alliance)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
