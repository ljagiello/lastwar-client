package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
)

// Gift Box (type 132) free daily draw and score boxes (MASTER.md G4; cov-ui-activity F1 and its
// verdict), two OPT-IN features. Static-only, from the 1.0.370 Lua and tables 40413, not sent live
// yet. Both read get.activity.gift.box.info {activityId: Int}, the reply event-shop-freebies
// already fetches for the activity's free pack (shared through the activity sweep). The reply
// carries useFreeTimes (absent = 0), giftBoxs[{uuid, itemId}], score and scoreArr[{index,
// targetScore, state}] (ActGiftBoxInfo.lua:50-104; ActGiftBoxScoreInfo.lua:19-31).
//
//   - gift-box-free-draw: the single draw is free while useFreeTimes < the activity_boxopen row's
//     cost_1_free (RefreshCostOne, UIGiftBoxNew.lua:393-410; the row is the one whose activity is the
//     id, ActGiftBoxData.lua:207-216; without one the draw is never free). The free single draw skips
//     the daily cap and the diamond check and sends activity.gift.box.lottery {activityId: Int,
//     fiveLottery: Int 0, useFree: Int 1} (OnClickOne, :489-531; ActivityGiftBoxLotteryMessage.lua:
//     4-9). With box_num boxes already in the slots the client first asks to confirm (text 2800078);
//     that case is skipped here. A draw yields a LOCKED box in a slot; opening one costs key items
//     (open.activity.gift.box is never sent, and boxes are never deleted), so boxes drawn here stay
//     in their slots until the player opens them, and once all box_num slots are full this feature
//     stops drawing. At most one draw a run; never useFree 0 or fiveLottery 1 (diamonds).
//   - gift-box-score-boxes: a scoreArr entry with state 0 and score >= targetScore sends
//     gift.box.receive {activityId: Int, index: Int <the entry's index>} (GiftBoxRewardBoxItemNew.lua:
//     82-86; GiftBoxReceiveMessage.lua:5-9).
//
// Each claim's reply is logged at Info; an errorCode stops that feature (no benign code is
// registered). gift-box-free-draw runs before gift-box-score-boxes (same phase, by name) and drops
// the cached reply after a draw, so the score boxes see the draw's score.
const (
	giftBoxDrawFeature  = "gift-box-free-draw"
	giftBoxBoxesFeature = "gift-box-score-boxes"
	giftBoxInfoCmd      = "get.activity.gift.box.info"
	giftBoxLotteryCmd   = "activity.gift.box.lottery"
	giftBoxReceiveCmd   = "gift.box.receive"
	// activity_boxopen cost_1_free and box_num: 1 and 4 in all 47 rows (tables 40413), which cover
	// the 28 type-132 activities; giftBoxOpenActivities lists them.
	giftBoxFreeDraws = 1
	giftBoxSlots     = 4
)

// giftBoxOpenActivities are the activities with an activity_boxopen row (tables 40413): every
// type-132 row of table activity. TestGiftBoxOpenRowsMatchTheDump pins it.
var giftBoxOpenActivities = map[int32]bool{
	98001: true, 98005: true, 98008: true, 98009: true, 98010: true, 98011: true, 98012: true, 98013: true,
	98111: true, 1000217: true, 1000218: true, 1000219: true, 1000231: true, 1000232: true, 1000265: true,
	1000504: true, 1000513: true, 1010217: true, 1010218: true, 1010219: true, 1010220: true, 1020213: true,
	1030213: true, 1030232: true, 1030238: true, 1030239: true, 1030240: true, 1030241: true,
}

func init() {
	registerFeature(Feature{
		Name: giftBoxDrawFeature,
		Summary: "OPT-IN: take each Gift Box event's free single draw (never a paid draw; the locked box stays in its slot) " +
			"(static-only, not validated live)",
		Run: runGiftBoxFreeDraw,
	})
	registerFeature(Feature{
		Name:    giftBoxBoxesFeature,
		Summary: "OPT-IN: claim each Gift Box event's reached score boxes (static-only, not validated live)",
		Run:     runGiftBoxScoreBoxes,
	})
}

var giftBoxInfo = evIntInfo("gift box info response", giftBoxInfoCmd, "activityId")

func runGiftBoxFreeDraw(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeGiftBox) {
		if b.dead {
			break
		}
		if !giftBoxOpenActivities[a.ID] {
			slog.Info(giftBoxDrawFeature+": no activity_boxopen row, so the draw is never free", "activity", a.ID)
			continue
		}
		info, err := giftBoxInfo(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		used, _ := evNum(info, "useFreeTimes")
		boxes := len(evObjects(info, "giftBoxs"))
		slog.Info(giftBoxDrawFeature+": info", "activity", a.ID, "useFreeTimes", used, "boxes", boxes)
		if used >= giftBoxFreeDraws || boxes >= giftBoxSlots {
			continue
		}
		p := evActivityIDParam(a)
		p.PutInt("fiveLottery", 0)
		p.PutInt("useFree", 1)
		r := b.send(fmt.Sprintf("%s: free draw (activity %d)", giftBoxDrawFeature, a.ID), giftBoxLotteryCmd, p)
		if r == nil {
			break // a command not validated live: an error stops the feature
		}
		slog.Info(giftBoxDrawFeature+": drew", "activity", a.ID, "newGiftBoxs", len(evObjects(r.Params, "newGiftBoxs")),
			"granted", describeGrants(r.Params))
		s.Forget(giftBoxInfoCmd, a)
		break // at most one draw a run
	}
	return b.err()
}

func runGiftBoxScoreBoxes(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeGiftBox) {
		if b.dead {
			break
		}
		info, err := giftBoxInfo(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		score, _ := evNum(info, "score")
		claimed := false
		for _, e := range evObjects(info, "scoreArr") {
			st, okSt := evNum(e, "state")
			target, okT := evNum(e, "targetScore")
			idx, okI := evNum(e, "index")
			if !okSt || !okT || !okI || st != 0 || score < target {
				continue
			}
			p := evActivityIDParam(a)
			p.PutInt("index", int32(idx))
			if !evOptInClaim(b, giftBoxBoxesFeature, fmt.Sprintf("score box %d (activity %d, score %d/%d)", idx, a.ID, score, target),
				giftBoxReceiveCmd, p) {
				return b.err()
			}
			claimed = true
		}
		if claimed {
			s.Forget(giftBoxInfoCmd, a)
		}
	}
	return b.err()
}
