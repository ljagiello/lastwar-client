package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Star-ceremony emoji (thumbs) reward boxes (MASTER G21, aud-alliance/F5, cov-ui-social/F2),
// opt-in and static-only. The weekly star ceremony's reply, the alliance.star.gain.ceremony.info.new
// that alliance-extras also reads (allianceStarCeremony, same gates), carries
// emojiThumbsRewardInfo {canReward, claimedIndex, count} (AllianceStarManager.lua:350, 413-423).
// While canReward is true the reward panel's box sends
// `alliance.star.ceremony.quest.emoji.reward.new {}` (UIAllianceStarMainRewardPanel.lua:28-38,
// 87-113; AllianceStarCeremonyQuestEmojiRewardNewMessage.lua:4-6), whose reply carries the
// rewards and the next emojiThumbsRewardInfo (OnAllianceStarCeremonyQuestEmojiRewardNew). The
// boxes are three stages of count (alliance_weeklyStar_config.k11, 1/5/10 by default), so at most
// three claims a run. Free and visible to no one. count grows with the player's own ceremony
// thumbs (alliance.star.thumbs.up.new), a visible action Go never sends: this only collects what
// the player earned by hand. Whether the server wants the ceremony scene entered first
// (alliance.star.request.enter) is not visible client-side. Each reply is logged in full at Info
// (it names no player); the first errorCode stops the run.
const (
	allianceStarEmojiFeature = "alliance-star-emoji"
	allianceStarEmojiCmd     = "alliance.star.ceremony.quest.emoji.reward.new"
	allianceStarEmojiBoxes   = 3
)

func init() {
	registerFeature(Feature{
		Name: allianceStarEmojiFeature,
		Summary: "OPT-IN: claim the alliance star ceremony's emoji (thumbs) reward boxes the player earned by hand; " +
			"never sends thumbs (static-only, not validated live)",
		Run: runAllianceStarEmoji,
	})
}

func runAllianceStarEmoji(conn *session.GameConn, in *Init) error {
	b := &evBatch{conn: conn}
	msg := allianceStarCeremony(b, in)
	if msg == nil {
		return b.err()
	}
	info := evObject(msg.Params, "emojiThumbsRewardInfo")
	for i := 0; ; i++ {
		claimedIndex, _ := evNum(info, "claimedIndex")
		count, _ := evNum(info, "count")
		slog.Info(allianceStarEmojiFeature+": emoji reward box", "present", info != nil, "canReward", evBool(info, "canReward"),
			"claimedIndex", claimedIndex, "count", count, "claimedThisRun", i)
		if info == nil || !evBool(info, "canReward") || b.dead {
			break
		}
		if i == allianceStarEmojiBoxes {
			slog.Warn(allianceStarEmojiFeature + ": still claimable after three boxes; stopping")
			break
		}
		r := b.send("alliance star emoji reward box", allianceStarEmojiCmd, sfs.NewSFSObject())
		if r == nil {
			break
		}
		info = evObject(r.Params, "emojiThumbsRewardInfo")
	}
	return b.err()
}
