package game

import (
	"errors"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Store and subscription free chests (MASTER.md §4 #3), one of each per server day, no purchase.
//
// This sends the three single claims, each behind the gate the 1.0.364 client checks, rather than
// the one-call `week.month.card.reward.all {card_id}`:
//   - The real client never sends the one-call claim on its own; only the "Claim All" button in
//     the subscription panel does, and the panel shows it only when a row is claimable
//     (UISubscriptionListPanelView.lua:99-158). Its card_id needs the season-card activity's
//     `para` from table activity (with a date-dependent remap) or the season config, and what
//     the server answers to card_id 0 or to a claim with nothing due is unknown (§8.2 #4).
//   - The single claims are what the client itself sends without a tap (PayManager.lua:2056-2067,
//     ClaimWeekCardRewardMessage.lua:14-26, UIPlayerLevelPackageView.lua:670-679), and every
//     gate below is computable from init or one read the client also sends at login, so nothing
//     is sent that the client's UI would show as already claimed.
//
// The claims:
//   - `free.reward.receive {type: 1, isAutoClaim: 1}` (FreeRewardReceiveMessage.lua:3-7), the store
//     free reward (FreeRewardType 1). Gate: init freeRewardArr has a type-1 entry whose lastTime
//     (seconds) is before today's server-day start (RechargeManager.lua:21-39), and unlock 113 (HQ
//     ≥ 2). isAutoClaim is echoed back and only changes the tip text; 1 is what the client sends
//     when it claims without a tap.
//   - `receive.week.free.reward {}`, the free weekly package. Gate: init weekFreeReward.lastRewardTime
//     (ms) is not today (GiftPackManager.lua:152-176, 394-400).
//   - `receive.week.card.daily.free.reward {}`, the week-card free gift. Gate: the reply to
//     `get.week.card.info {isLogin: 1}` (sent by the client right after init, InitMessage.lua:663;
//     GetWeekCardListMessage.lua:4-26) carries dailyFree.lastRewardTime (ms) that is not today
//     (WeekCardManager.lua:65-76). The Lua checks no card ownership; whether the server does is
//     open (MASTER.md §8.1), so log the first reply.
//
// The season week-card free gift (`lw.season.week.card.free.reward`) is season-only (§4 #18) and
// not handled here. Static-only: none of these has been sent live yet.
const (
	freeChestsStoreCmd    = "free.reward.receive"
	freeChestsWeeklyCmd   = "receive.week.free.reward"
	freeChestsWeekInfoCmd = "get.week.card.info"
	freeChestsWeekCardCmd = "receive.week.card.daily.free.reward"

	freeChestsStoreType = 1 // FreeRewardType.PackageInStoreFreeReward (EnumType.lua:13301)
	freeChestsStoreHQ   = 2 // unlock 113 (UISubscriptionListPanelView.lua:56-61)
)

func init() {
	registerFeature(Feature{
		Name:    "free-chests",
		Summary: "claim the daily free store reward, free weekly package and week-card free gift",
		Run:     runFreeChests,
	})
	session.RegisterBenignErrorCode("120289", freeChestsStoreCmd, freeChestsWeeklyCmd, freeChestsWeekCardCmd)
}

func runFreeChests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("free-chests: no init push; skipping")
		return nil
	}
	if _, ok := in.ServerDayStart(claimNow(in)); !ok {
		slog.Warn("free-chests: init has no tomorrow, so the server day is unknown; skipping")
		return nil
	}
	var errs []error
	steps := []func() error{
		func() error { return freeChestsStore(conn, in) },
		func() error { return freeChestsWeekly(conn, in) },
		func() error { return freeChestsWeekCard(conn, in) },
	}
	for _, step := range steps {
		err := step()
		if err != nil {
			errs = append(errs, err)
		}
		if session.ContainsNonTimeoutNetError(err) {
			break
		}
	}
	return errors.Join(errs...)
}

func freeChestsStore(conn *session.GameConn, in *Init) error {
	if hq := claimHQLevel(in); hq < freeChestsStoreHQ {
		slog.Info("free-chests: store free reward locked", "hqLevel", hq)
		return nil
	}
	for _, e := range in.Objects("freeRewardArr") {
		if t, _ := claimInt(e, "type"); t != freeChestsStoreType {
			continue
		}
		last, ok := claimInt(e, "lastTime")
		if !ok {
			return nil
		}
		if due, _ := claimBeforeToday(in, last*1000); !due {
			slog.Info("free-chests: store free reward already claimed today")
			return nil
		}
		params := sfs.NewSFSObject()
		params.PutInt("type", freeChestsStoreType)
		params.PutInt("isAutoClaim", 1)
		_, err := claimAndLog(conn, "store free reward", freeChestsStoreCmd, params)
		return err
	}
	slog.Info("free-chests: init has no store free reward entry")
	return nil
}

func freeChestsWeekly(conn *session.GameConn, in *Init) error {
	w := in.Object("weekFreeReward")
	if w == nil {
		slog.Info("free-chests: init has no weekFreeReward")
		return nil
	}
	last, _ := claimInt(w, "lastRewardTime") // absent reads as 0, the client's default (GiftPackManager.lua:10)
	if due, _ := claimBeforeToday(in, last); !due {
		slog.Info("free-chests: free weekly package already claimed today")
		return nil
	}
	_, err := claimAndLog(conn, "free weekly package", freeChestsWeeklyCmd, sfs.NewSFSObject())
	return err
}

func freeChestsWeekCard(conn *session.GameConn, in *Init) error {
	params := sfs.NewSFSObject()
	params.PutInt("isLogin", 1)
	info, err := session.SendAndWait(conn, "week card info", freeChestsWeekInfoCmd, params)
	if err != nil {
		return err
	}
	var free *sfs.SFSObject
	if v, ok := info.Params.Get("dailyFree"); ok {
		free, _ = v.Val.(*sfs.SFSObject)
	}
	if free == nil {
		slog.Info("free-chests: week card info has no dailyFree")
		return nil
	}
	last, _ := claimInt(free, "lastRewardTime")
	if due, _ := claimBeforeToday(in, last); !due {
		slog.Info("free-chests: week-card free gift already claimed today")
		return nil
	}
	_, err = claimAndLog(conn, "week-card free gift", freeChestsWeekCardCmd, sfs.NewSFSObject())
	return err
}
