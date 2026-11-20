package sfs

// Request keys put on the wire by the 1.0.370 audit's WP8 opt-in event claims (internal/game:
// feature_gift_box.go, feature_food_events.go, feature_bounty_hunter.go). Each is a gameplay number
// or a scene object id; none authenticates anything or identifies a person.
func init() {
	for _, k := range []string{
		"fiveLottery", // activity.gift.box.lottery: 1 for the five-draw, always 0 here (ActivityGiftBoxLotteryMessage.lua:6-8)
		"level",       // activity.food.party[.v2].level.reward: a banquet level box number
		"phaseId",     // bounty.hunter.receive.phase.reward: a milestone's score threshold
		"score",       // activity.mak.food.score.reward: a cooking box's score threshold
		"targetId",    // bounty.hunter.click.chest.reward: a hunt-scene chest uuid
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
