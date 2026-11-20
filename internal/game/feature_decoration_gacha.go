package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
)

// Decoration Gacha progress boxes and pity wish (MASTER.md G30; cov-dc-events F4 and its verdict),
// OPT-IN, claims only. Static-only, from the 1.0.370 Lua, not sent live yet.
//
// A DecorationGacha activity (type 173, EnumType.lua:6439) is read with hero.event.info.get
// (ActivityListDataManager.lua:619-623), the reply event-shop-freebies already reads for the free
// pack; the client keeps the whole reply (ActivityDecorationGachaManager.lua:366-374;
// ActivityDecorationGachaData.lua:10-20).
//
//   - Progress boxes: boxRewards[{index, target}], totalScore and boxReceive[] (the claimed entries'
//     own index). GetProgressState (ActivityDecorationGachaData.lua:135-159) is 1 (claimable) when
//     totalScore reached the entry's target and its index is not in boxReceive; the box button
//     then sends decoration.receive.box {activityId: Int, index: Int <the entry's index>}
//     (ActivityDecorationGachaProgressItemComponent.lua:113-123; ActivityDecorationGachaManager.lua:
//     656-658; ActivityDecorationGachaProgressClaimMessage.lua:4-8).
//   - Pity wish: CanClaimWish (:278-284) is pity > 0, pity <= wishScore and a wish already selected
//     (wishItem > 0 with wishNum present, GetCurSelectWishData :348-356); the wish button then sends
//     decoration.receive.wish {activityId: Int} (ActivityDecorationGachaWishComponent.lua:169-183;
//     ActivityDecorationGachaManager.lua:642-644; ActivityDecorationGachaWishClaimMessage.lua:4-7).
//
// Progress only moves when the player draws by hand. Never decoration.select.wish (a choice),
// decoration.lottery (always paid: decoration_recriut free_time is 0) or decoration.diamond.reward.
// Each claim's reply is logged at Info; an errorCode stops the feature (no benign code is
// registered).
const (
	decorationFeature       = "decoration-gacha-boxes"
	decorationBoxCmd        = "decoration.receive.box"
	decorationWishCmd       = "decoration.receive.wish"
	decorationMaxBoxesClaim = 50 // a bound on box claims per activity and run
)

func init() {
	registerFeature(Feature{
		Name:    decorationFeature,
		Summary: "OPT-IN: claim Decoration Gacha progress boxes reached and a full pity wish already chosen (static-only, not validated live)",
		Run:     runDecorationGacha,
	})
}

func runDecorationGacha(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeDecorationGacha) {
		if b.dead {
			break
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		score, _ := evNum(info, "totalScore")
		done := map[int64]bool{}
		for _, n := range evNums(info, "boxReceive") {
			done[n] = true
		}
		pity, _ := evNum(info, "pity")
		wishScore, _ := evNum(info, "wishScore")
		wishItem, _ := evNum(info, "wishItem")
		_, hasWishNum := evNum(info, "wishNum")
		boxes := evObjects(info, "boxRewards")
		slog.Info(decorationFeature+": info", "activity", a.ID, "totalScore", score, "boxes", len(boxes), "claimed", len(done),
			"pity", pity, "wishScore", wishScore, "wishItem", wishItem, "hasWishNum", hasWishNum)
		claimed := 0
		for _, bx := range boxes {
			idx, okI := evNum(bx, "index")
			target, okT := evNum(bx, "target")
			if !okI || !okT || score < target || done[idx] || claimed >= decorationMaxBoxesClaim {
				continue
			}
			p := evActivityIDParam(a)
			p.PutInt("index", int32(idx))
			if !evOptInClaim(b, decorationFeature, fmt.Sprintf("box %d (activity %d, score %d/%d)", idx, a.ID, score, target), decorationBoxCmd, p) {
				return b.err()
			}
			claimed++
		}
		if pity > 0 && pity <= wishScore && wishItem > 0 && hasWishNum {
			if !evOptInClaim(b, decorationFeature, fmt.Sprintf("pity wish item %d (activity %d)", wishItem, a.ID), decorationWishCmd,
				evActivityIDParam(a)) {
				return b.err()
			}
			claimed++
		}
		if claimed > 0 {
			s.Forget("hero.event.info.get", a)
		}
	}
	return b.err()
}
