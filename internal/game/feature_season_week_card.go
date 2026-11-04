package game

import (
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season week card (OPT-IN, MASTER.md §3.2 #1 SWC): the card's free daily chest, which needs no
// purchase, and the daily reward of a card the account bought. Static-only: from the 833 Lua, not
// sent live yet.
//
// At login, in season, the client asks for the card of the open SeasonPeriodicCard (228) activity
// when its id is above 0 and table season_weekcard has it (SeasonDataManager.lua:244-256):
//   - lw.season.week.card.info {card_id: Int} -> card_id, end_time, daily_reward, free_reward (ms)
//     (LWSeasonWeekCardInfoMessage.lua:4-7; SeasonPeriodicCardData.lua:29-76). A reply without
//     card_id stores no card (SeasonPeriodicCardManager.lua:27-29), so nothing is claimable.
//   - lw.season.week.card.daily.reward {card_id: Int} for a bought card (end_time > now) whose
//     daily_reward is not today (SeasonPeriodicCardData.lua:102-126; UILWActPeriodicCard.lua:514-516).
//   - then lw.season.week.card.free.reward {card_id: Int} while free_reward is absent or not today
//     (SeasonPeriodicCardData.lua:128-135). The season panel's chest checks nothing else
//     (UILWActPeriodicCard.lua:386-401, 518-524), and the client chains it after the daily
//     (SeasonPeriodicCardManager.lua:86-96). Like free-chests, it is skipped when init dataConfig
//     turns free_chest_receive off (that chain checks it, :90-93).
//
// card_id is table data, not a field of the init entry: ParseActivityData copies no `para` from the
// server (ActivityInfoData.lua:124-192). FetchActivityConfigData takes table activity's `para`
// and, for type 228, swaps in condition_giftbag's card once the activity's startTime falls on or
// after that server date (ActivityInfoData.lua:200-222, 351-357). seasonWeekCards holds those
// columns. An open 228 activity missing from it is skipped and logged.
//
// No error code is registered as benign: the first replies are logged and an error stops that
// claim.
const (
	seasonWeekCardFeature  = "season-week-card"
	seasonWeekCardInfoCmd  = "lw.season.week.card.info"
	seasonWeekCardDailyCmd = "lw.season.week.card.daily.reward"
	seasonWeekCardFreeCmd  = "lw.season.week.card.free.reward"
)

// seasonWeekCardRow is the part of a type-228 row of table activity the card id comes from: para,
// and condition_giftbag "y-m-d|card".
type seasonWeekCardRow struct {
	card     int32     // para
	swapOn   time.Time // condition_giftbag's date, as UTC midnight of that calendar day
	swapCard int32     // condition_giftbag's card
}

// seasonWeekCardSwapDate is the condition_giftbag date of every type-228 row ("2026-7-29").
var seasonWeekCardSwapDate = time.Date(2026, time.July, 29, 0, 0, 0, 0, time.UTC)

// seasonWeekCards are the six type-228 rows of table activity (tables 39879), seasons 1 to 6.
// Every card they name has a season_weekcard row (10001-10013), the client's last gate
// (SeasonDataManager.lua:251-253).
var seasonWeekCards = map[int32]seasonWeekCardRow{
	1000021: {10002, seasonWeekCardSwapDate, 10008},
	1000043: {10003, seasonWeekCardSwapDate, 10009},
	1000068: {10004, seasonWeekCardSwapDate, 10010},
	1200008: {10005, seasonWeekCardSwapDate, 10011},
	1200052: {10006, seasonWeekCardSwapDate, 10012},
	1200111: {10007, seasonWeekCardSwapDate, 10013},
}

func init() {
	registerFeature(Feature{
		Name:    seasonWeekCardFeature,
		Summary: "OPT-IN: claim the season week card's free daily chest and a bought card's daily reward (static-only, not validated live)",
		Run:     runSeasonWeekCard,
	})
}

// seasonServerOffset is UITimeManager's changeDeltaTime, the server clock's offset from UTC, read
// off the server-day anchor: the day starts at server midnight, so a 02:00 UTC reset is UTC-2, the
// client's default -7200000 (UITimeManager.lua:10, 123-127). It is taken within [-12h, +12h).
func seasonServerOffset(dayStart time.Time) time.Duration {
	u := dayStart.UTC()
	tod := u.Sub(u.Truncate(24 * time.Hour))
	if tod > 12*time.Hour {
		return 24*time.Hour - tod
	}
	return -tod
}

// seasonWeekCardID is the card id the client derives for a (GetConditionGiftbagCardId): the
// swapped card when a's startTime is on or after the swap date in server time, else para. ok is
// false when seasonWeekCards lacks a.
func seasonWeekCardID(a Activity, dayStart time.Time) (int32, bool) {
	row, ok := seasonWeekCards[a.ID]
	if !ok {
		return 0, false
	}
	start := a.StartTime()
	if start > 0 && !time.UnixMilli(start).UTC().Add(seasonServerOffset(dayStart)).Before(row.swapOn) {
		return row.swapCard, true
	}
	return row.card, true
}

func runSeasonWeekCard(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(seasonWeekCardFeature + ": no init push; skipping")
		return nil
	}
	dayStart, ok := in.ServerDayStart(evNow())
	if !ok {
		slog.Info(seasonWeekCardFeature + ": init has no tomorrow, so the server day is unknown; skipping")
		return nil
	}
	if evSeasonID(in) <= 0 {
		slog.Info(seasonWeekCardFeature + ": not in a season; skipping")
		return nil
	}
	open := Activities(conn, in).Open(actTypeSeasonPeriodicCard)
	if len(open) == 0 {
		slog.Info(seasonWeekCardFeature + ": no open season week card activity (228); skipping")
		return nil
	}
	a := open[0] // GetOneOpenActivityByType
	cardID, ok := seasonWeekCardID(a, dayStart)
	if !ok {
		slog.Info(seasonWeekCardFeature+": activity is not in the season week card table; skipping until it is added",
			"activity", a.ID, "tableVersion", activityTableVersion)
		return nil
	}
	b := &evBatch{conn: conn}
	p := sfs.NewSFSObject()
	p.PutInt("card_id", cardID)
	msg := b.send("season week card info", seasonWeekCardInfoCmd, p)
	if msg == nil || msg.Params.Has("errorCode") {
		return b.err()
	}
	info := msg.Params
	replyID, hasID := evNum(info, "card_id")
	end, _ := evNum(info, "end_time")
	daily, _ := evNum(info, "daily_reward")
	free, _ := evNum(info, "free_reward")
	bought := end > evNow().UnixMilli()
	dailyDue := bought && evNotToday(in, info, "daily_reward")
	freeDue := evNotToday(in, info, "free_reward")
	slog.Info(seasonWeekCardFeature+": card info", "activity", a.ID, "card_id", cardID, "replyCardId", replyID,
		"end_time", end, "daily_reward", daily, "free_reward", free, "bought", bought, "dailyDue", dailyDue, "freeDue", freeDue)
	if !hasID || replyID != int64(cardID) {
		slog.Info(seasonWeekCardFeature+": the reply is not for the card asked about, so the client would show no card; skipping",
			"card_id", cardID, "replyCardId", replyID, "hasCardId", hasID)
		return b.err()
	}
	if dailyDue {
		seasonWeekCardClaim(b, "season week card daily", seasonWeekCardDailyCmd, cardID)
	}
	switch {
	case !freeDue:
	case freeChestSwitchOff(in):
		slog.Info(seasonWeekCardFeature + ": init dataConfig turns " + freeChestSwitch + " off; skipping the free chest")
	default:
		seasonWeekCardClaim(b, "season week card free chest", seasonWeekCardFreeCmd, cardID)
	}
	return b.err()
}

// seasonWeekCardClaim sends one {card_id: Int} claim and logs what it granted (the reply's
// rewards, SeasonPeriodicCardManager.lua:61-66, 83-98).
func seasonWeekCardClaim(b *evBatch, label, cmd string, cardID int32) {
	if b.dead {
		return
	}
	p := sfs.NewSFSObject()
	p.PutInt("card_id", cardID)
	_, err := claimAndLog(b.conn, label, cmd, p, "rewards")
	b.add(err)
}
