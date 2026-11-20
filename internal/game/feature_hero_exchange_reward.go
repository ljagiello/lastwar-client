package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
)

// Hero-exchange guide reward (MASTER.md G8; cov-ui-social F7 and its verdict), OPT-IN, a claim
// only. Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet.
//
// An ActHeroLevelAndStarReplace activity (type 323, EnumType.lua:6471; 1030002, 1030003, 1200024,
// 1200066 in 40413) carries receiveReward in its init activity-list entry, which the client keeps
// per id (ActivityListDataManager.lua:709-711; ActExchangeHeroDataManager.lua:11-22; an id without
// the field reads as not received). While it is not received, opening the activity page opens the
// exchange guide (UIActHeroLevelAndStarReplace.lua:375-382), whose last page's button sends
// lw.season.hero.receive.switch.reward {activityId: Int} with no cost and no exchange needed first
// (UIHeroExchangeGuideView.lua:150-168; LwSeasonHeroReceiveSwitchRewardMessage.lua:4-34; the reply
// carries goods {itemId, count} and activityInfo). The reward is the row's para_1: two of goods
// 640017 in every row.
//
// receiveReward counts as received by Lua truthiness: only an absent field or Bool false is
// claimed (a number, even 0, is truthy in Lua). Once per activity; the claim's reply is logged at
// Info and an errorCode stops the feature (no benign code is registered).
const (
	heroExchangeFeature         = "hero-exchange-reward"
	heroExchangeActType   int32 = 323 // EnumActivity.ActHeroLevelAndStarReplace
	heroExchangeRewardCmd       = "lw.season.hero.receive.switch.reward"
)

func init() {
	registerFeature(Feature{
		Name:    heroExchangeFeature,
		Summary: "OPT-IN: claim the hero-exchange guide's one-time reward of each open exchange event (static-only, not validated live)",
		Run:     runHeroExchangeReward,
	})
}

func runHeroExchangeReward(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(heroExchangeActType) {
		if b.dead {
			break
		}
		v, ok := a.Raw.Get("receiveReward")
		if ok && v.Val != nil && v.Val != false {
			continue // received, or a value Lua reads as true
		}
		slog.Info(heroExchangeFeature+": guide reward not received", "activity", a.ID, "hasField", ok, "reward", a.Para1())
		if !evOptInClaim(b, heroExchangeFeature, fmt.Sprintf("guide reward (activity %d)", a.ID), heroExchangeRewardCmd,
			evActivityIDParam(a)) {
			break
		}
	}
	return b.err()
}
