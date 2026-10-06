package sfs

// Keys added by the cf-core-actions changes, reviewed as ordinary gameplay paging parameters:
// "index"/"len" are alliance.reward.list's list offset and page size (alliance.go
// claimAllianceGifts, mirroring AllianceGiftListMessage.lua's PutInt("index")/PutInt("len")).
func init() {
	for _, k := range []string{"index", "len"} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
