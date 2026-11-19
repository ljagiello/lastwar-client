package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Wish-hero claim on a recruit pool (OPT-IN, MASTER 1.0.370 G24; cov-dc-world F5 "Hero wish" and
// the aud-dailies verifier's lead). Static-only: from the 1.0.370 Lua, not sent live yet.
//
// A wish pool hands out the chosen wish hero once its wish pity is full. The recruit view shows the
// wish entrance only when IsShowWish holds (UIHeroRecruitView.lua:606-616): wishSwitch set, not
// before the start of a duration pool, wishPityReceiveNum > 0 and a non-empty officer_recruit
// wish_list (LotteryInfo.lua:468-478, 273-280). Its claim button is shown while IsCanClaimWish
// (wishSwitch, wishPityReceiveNum > 0 and wishPityCurNum >= wishPityReceiveNum,
// LotteryInfo.lua:798-803) and sends the claim only when a wish hero is chosen (wishHero non-empty,
// GetCurSelectWishHeroId :480-484; UIHeroRecruitWishEntranceComponent.lua:118-127):
// `hero.lottery.receive.wish {id: UtfString lotteryId}` (HeroLotteryClaimWishMessage.lua:5-8;
// LotteryDataManager.lua:477-479). Every field is on the init lotteryFreeInfo[] pool object
// (LotteryInfo.lua:284-295; the client's defaults are false, "", 0 and 0, :47-55). Pools of type 2
// or 3 are not listed (LotteryDataManager.lua:106-127), and a pool outside its window is not open
// (IsOpen, LotteryInfo.lua:329-335; startTime/endTime in seconds, :179-184).
//
// The claim spends nothing: it hands out the hero the pulls already earned. The wish hero is never
// chosen here (hero.lottery.switch.wish is the player's choice). The table wish list comes from
// duelRecruitWishLists (feature_duel_recruit_tickets.go): a pool it lists without a wish list is
// skipped; a pool it does not know is not, since a chosen wishHero already shows the server runs a
// wish list there. No error code is registered as benign: the first reply is logged and an error
// stops the claims.
const (
	heroWishClaimFeature = "hero-wish-claim"
	heroWishClaimCmd     = "hero.lottery.receive.wish"
)

func init() {
	registerFeature(Feature{
		Name: heroWishClaimFeature,
		Summary: "OPT-IN: claim the chosen wish hero of a recruit pool once its wish pity is full " +
			"(hero.lottery.receive.wish); never picks the wish hero (static-only, not validated live)",
		Run: runHeroWishClaim,
	})
}

// heroWishClaimable returns the ids of the init pools whose wish hero the client would let the
// player claim now.
func heroWishClaimable(in *Init, nowS int64) []string {
	var out []string
	seen := map[string]bool{}
	for _, pool := range in.Objects("lotteryFreeInfo") {
		id := evString(pool, "id")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if t, ok := evNum(pool, "type"); ok && (t == 2 || t == 3) {
			continue
		}
		start, _ := evNum(pool, "startTime")
		end, _ := evNum(pool, "endTime")
		if end > start && (nowS <= start || nowS >= end) {
			continue
		}
		if v, ok := pool.Get("wishSwitch"); !ok || v.Val == nil || v.Val == false {
			continue
		}
		if hasList, known := duelRecruitWishLists[id]; known && !hasList {
			continue
		}
		need, _ := evNum(pool, "wishPityReceiveNum")
		cur, _ := evNum(pool, "wishPityCurNum")
		if need <= 0 || cur < need || evString(pool, "wishHero") == "" {
			continue
		}
		out = append(out, id)
	}
	return out
}

func runHeroWishClaim(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(heroWishClaimFeature + ": no init push; skipping")
		return nil
	}
	ids := heroWishClaimable(in, evNow().Unix())
	if len(ids) == 0 {
		slog.Info(heroWishClaimFeature + ": no recruit pool with a full wish pity and a chosen wish hero")
		return nil
	}
	b := &evBatch{conn: conn}
	for i, id := range ids {
		p := sfs.NewSFSObject()
		p.PutUtfString("id", id)
		msg := b.send("wish hero "+id, heroWishClaimCmd, p)
		if msg == nil {
			slog.Warn(heroWishClaimFeature+": claim refused; stopping", "pool", id, "claimed", i)
			break
		}
		if i == 0 {
			slog.Info(heroWishClaimFeature+": first "+heroWishClaimCmd+" reply", "pool", id,
				"granted", describeGrants(msg.Params), "response", msg.Params.StringRedacted())
		}
	}
	return b.err()
}
