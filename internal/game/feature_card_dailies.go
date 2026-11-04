package game

import (
	"fmt"
	"sync"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Owner-only card dailies (MASTER.md §4 #20), static-only: the daily claim of a card the account
// already owns. Nothing here buys a card; the card pages' buy buttons, week.card.special.choose and
// golloes.active are never sent.
//
//   - Month card: month.card.reward {itemId: UtfString} while init golloesMonthCard.endTime (ms) is
//     in the future and its time (ms, last claim) is not today (MonthCardNewInfo.lua:29-99). The
//     client seeds itemId from the recharge row of type WelfareTagType.MonthCard (2) and lets init
//     itemId override it (MonthCardNewManager.lua:33-44, MonthCardNewInfo.lua:41-44).
//   - Week cards: get.week.card.info {isLogin: Int 1} lists them (week cards are not in init); a
//     Collectable card (type_function 2) still running whose lastReceiveTime is not today is claimable,
//     and receive.all.week.card.reward {} claims them all (WeekCardData.lua:124-150,
//     WeekCardMain.lua:124-152). The info read is shared with free-chests (weekCardInfo).
//   - Hero month cards: get.hero.month.card.info {} -> cards[]; a bought card (buy 1) inside its
//     window pays each reached rewardArr day still in state 0 with receive.hero.month.card.reward
//     {activityId: Int, day: Long}; missed days stay claimable (HeroMonthCardManager.lua:108-127).
//   - Golloes camp: receive.golloes.reward {type: Int} for the Explorer (1) and Trader (2) stashes,
//     while init golloesData.spyRewardNum / caravanRewardNum > 0 and the camp (10145000) is built
//     (GolloesCampItem.lua:112-150, 215-222).
//   - Truck privilege: truck.monthcard.privilege.click.reward {} while the insurance activity (373)
//     runs and init truckMonthCardPrivilege has freeReward, or vipReward with a running month card
//     (UITruckRewardInsuranceView.lua:589-598).
//
// Not here: the season week card's daily (lw.season.week.card.daily.reward, the opt-in
// season-week-card feature, feature_season_week_card.go); the free chests (free-chests; the
// week.month.card.reward.all claim-all is not sent, MASTER.md §4 #3); week.card.special.reward
// (whether one call claims every accrued day needs a live capture); receive.golloes.daily.free.reward,
// gated on the Grocery Store (724000), which has no rows in the 1.0.364 building tables.
const (
	monthCardRewardCmd     = "month.card.reward"
	weekCardInfoCmd        = "get.week.card.info"
	weekCardAllRewardCmd   = "receive.all.week.card.reward"
	heroMonthCardInfoCmd   = "get.hero.month.card.info"
	heroMonthCardRewardCmd = "receive.hero.month.card.reward"
	golloesRewardCmd       = "receive.golloes.reward"
	truckPrivilegeCmd      = "truck.monthcard.privilege.click.reward"
	golloesCampBuildingID  = 10145000 // LW_BUILD_PARKINGLOT_FOUR (EnumType.lua:3639)
	weekCardCollectable    = 2        // WeekCardFucntionType.Collectable
	// monthCardItemID is para1 of recharge row 1140000, the only row of type
	// WelfareTagType.MonthCard (2, WelfareTagType.lua:4) in tables 39879.
	monthCardItemID = "800001"
)

// weekCardInfoCache holds the one get.week.card.info reply of a login, keyed by (conn, init) like
// ActivitySweep: the client sends it once, at login (WeekCardManager.lua:23-25), and both
// card-dailies and free-chests read it.
var (
	weekCardInfoMu    sync.Mutex
	weekCardInfoCache struct {
		conn *session.GameConn
		in   *Init
		msg  *session.ExtensionMessage
		err  error
	}
)

// weekCardInfo returns the reply to get.week.card.info {isLogin: Int 1}, sending it on the first
// call for (conn, in) only; the reply or the failure is returned as session.SendAndWait gave it.
func weekCardInfo(conn *session.GameConn, in *Init) (*session.ExtensionMessage, error) {
	weekCardInfoMu.Lock()
	defer weekCardInfoMu.Unlock()
	if c := &weekCardInfoCache; c.conn != nil && c.conn == conn && c.in == in {
		return c.msg, c.err
	}
	p := sfs.NewSFSObject()
	p.PutInt("isLogin", 1)
	msg, err := session.SendAndWait(conn, "week card info", weekCardInfoCmd, p)
	weekCardInfoCache.conn, weekCardInfoCache.in, weekCardInfoCache.msg, weekCardInfoCache.err = conn, in, msg, err
	return msg, err
}

func init() {
	registerFeature(Feature{
		Name:    "card-dailies",
		Summary: "claim the daily rewards of owned month/week/hero cards, golloes stashes and truck privilege; static-only",
		Run:     runCardDailies,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, monthCardRewardCmd, weekCardAllRewardCmd, heroMonthCardRewardCmd,
		golloesRewardCmd, truckPrivilegeCmd)
}

func runCardDailies(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if _, ok := in.ServerDayStart(evNow()); !ok {
		return nil
	}
	b := &evBatch{conn: conn}
	now := evNow().UnixMilli()
	monthCard := in.Object("golloesMonthCard")
	monthCardOwned := false
	if end, ok := evNum(monthCard, "endTime"); ok && end > now {
		monthCardOwned = true
		itemID := evString(monthCard, "itemId")
		if itemID == "" {
			itemID = monthCardItemID
		}
		if evNotToday(in, monthCard, "time") {
			p := sfs.NewSFSObject()
			p.PutUtfString("itemId", itemID)
			b.send("month card daily", monthCardRewardCmd, p)
		}
	}
	if !b.dead {
		claimWeekCards(b, in, now)
	}
	if !b.dead {
		claimHeroMonthCards(b, now)
	}
	if !b.dead && evHasBuilding(in, golloesCampBuildingID) {
		golloes := in.Object("golloesData")
		for _, g := range []struct {
			key string
			typ int32
		}{{"spyRewardNum", 1}, {"caravanRewardNum", 2}} {
			if n, _ := evNum(golloes, g.key); n > 0 && !b.dead {
				p := sfs.NewSFSObject()
				p.PutInt("type", g.typ)
				b.send(fmt.Sprintf("golloes stash (type %d)", g.typ), golloesRewardCmd, p)
			}
		}
	}
	if !b.dead {
		priv := in.Object("truckMonthCardPrivilege")
		open := len(Activities(conn, in).Open(actTypeMonthCardInsure)) > 0
		if priv != nil && open && (evNonEmpty(priv, "freeReward") || (monthCardOwned && evNonEmpty(priv, "vipReward"))) {
			b.send("truck month card privilege", truckPrivilegeCmd, sfs.NewSFSObject())
		}
	}
	return b.err()
}

func claimWeekCards(b *evBatch, in *Init, now int64) {
	if b.dead {
		return
	}
	msg, err := weekCardInfo(b.conn, in)
	b.add(err)
	if err != nil || msg == nil || msg.Params.Has("errorCode") {
		return
	}
	for _, c := range evObjects(msg.Params, "weekCards") {
		fn, _ := evNum(c, "type_function")
		end, _ := evNum(c, "endTime")
		if fn == weekCardCollectable && end > now && evNotToday(in, c, "lastReceiveTime") {
			b.send("week cards daily", weekCardAllRewardCmd, sfs.NewSFSObject())
			return
		}
	}
}

func claimHeroMonthCards(b *evBatch, now int64) {
	msg := b.send("hero month card info", heroMonthCardInfoCmd, sfs.NewSFSObject())
	if msg == nil || msg.Params.Has("errorCode") {
		return
	}
	for _, c := range evObjects(msg.Params, "cards") {
		buy, _ := evNum(c, "buy")
		start, ok1 := evNum(c, "startTime")
		end, ok2 := evNum(c, "endTime")
		aid, ok3 := evNum(c, "activityId")
		if buy != 1 || !ok1 || !ok2 || !ok3 || now < start || now > end {
			continue
		}
		reached := float64(now-start)/86400000 + 1
		for _, r := range evObjects(c, "rewardArr") {
			state, _ := evNum(r, "state")
			day, ok := evNum(r, "day")
			if !ok || state != 0 || reached < float64(day) || b.dead {
				continue
			}
			p := sfs.NewSFSObject()
			p.PutInt("activityId", int32(aid))
			p.PutLong("day", day)
			b.send(fmt.Sprintf("hero month card %d day %d", aid, day), heroMonthCardRewardCmd, p)
		}
	}
}

// evHasBuilding reports whether init building_new has bId at level 1 or more.
func evHasBuilding(in *Init, bID int32) bool {
	for _, bd := range in.Buildings {
		if bd.BId() == bID && bd.Level() > 0 {
			return true
		}
	}
	return false
}
