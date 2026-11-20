package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestAllianceStarEmojiWire pins alliance-star-emoji's wire shape as literals from the 1.0.370
// client. Every command here has no params:
//
//   - alliance.star.gain.activity.info.new (MsgDefines.lua:2442): AllianceStarGainActivityInfoNew
//     Message.lua puts none, sent by AllianceStarManager.lua:87.
//   - alliance.star.gain.ceremony.info.new (MsgDefines.lua:2441): AllianceStarGainCeremonyInfoNew
//     Message.lua puts none, sent by UIAlStarBubbleTip.lua:64.
//   - alliance.star.ceremony.quest.emoji.reward.new (MsgDefines.lua:2446):
//     AllianceStarCeremonyQuestEmojiRewardNewMessage.lua puts none, sent by
//     UIAllianceStarMainRewardPanel.lua:34 while emojiThumbsRewardInfo.canReward holds.
//
// The ceremony reply's box is claimable and the claim's reply says the next one is not.
func TestAllianceStarEmojiWire(t *testing.T) {
	withEvNow(t, evTestNow)
	got := bWireRun(t, "alliance-star-emoji", evAllianceInit("alliance_star"), func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "alliance.star.gain.activity.info.new":
			r := sfs.NewSFSObject()
			r.PutBool("hasCeremony", true)
			return r
		case "alliance.star.gain.ceremony.info.new":
			r := starCeremony(true, nil)
			r.PutSFSObject("emojiThumbsRewardInfo", emojiInfo(true, 0, 1))
			return r
		case "alliance.star.ceremony.quest.emoji.reward.new":
			r := sfs.NewSFSObject()
			r.PutSFSObject("emojiThumbsRewardInfo", emojiInfo(false, 1, 1))
			return r
		}
		return evErr("unexpected command")
	})
	bWireCheck(t, got, []bWireReq{
		bReq("alliance.star.gain.activity.info.new"),
		bReq("alliance.star.gain.ceremony.info.new"),
		bReq("alliance.star.ceremony.quest.emoji.reward.new"),
	})
}
