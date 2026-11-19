package game

import (
	"log/slog"
	"math"
	"slices"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Exchange of expired event items (MASTER.md G45; cov-dc-social-city F9 and its verdict), OPT-IN.
// Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet. It converts items the game
// already treats as expired into what goods_exchange.af_goods names; the client asks for a
// confirmation before it converts a whole stack (UILWExpiredItemExchangeView.lua:208-223), so this
// stays opt-in.
//
// After init the client reads activity.goods.exchange.list {} (InitMessage.lua:698), whose dataArr
// [{id, expireTime (s), timeType (default 1), para}] is the server's expiry per item
// (ActivityGoodsExchangeListMessage.lua:8-13; ItemExchangeManager.lua:55-67). An item with a
// goods_exchange row is expired when (ItemExchangeTemplate.lua:62-107): its row's times_type is 8
// and none of the listed activities is valid, or 7 and no activity of that type is open, or
// otherwise when the server's expireTime > 0 has passed, with timeType 4 also needing the count
// below the server's para (timeType 1 and 3 need only the time). The table's own times date is not
// used. SendExchangeItemMsg then sends activity.goods.recovery {goodsId: UtfString, num: Int} for
// at most the owned count (ItemExchangeManager.lua:42-53; ExpiredItemExchangeMessage.lua:4-8),
// and the reply carries reward.
//
// Only the server-expiry branch is automated: the times_type 7 and 8 rows (521050, 521052,
// 521062, 521063) depend on whether activities are valid, which a table older than the live one
// could misjudge, so they are left to the player. The other path, an item whose otherPara
// expireTime passed (ItemData:CheckItemIsExpiredInOtherParamData), is not automated either.
const (
	expiredItemsFeature    = "expired-items"
	expiredItemsListCmd    = "activity.goods.exchange.list"
	expiredItemsRecoverCmd = "activity.goods.recovery"
	expiredTimeTypeCount   = 4 // timeType whose expiry also needs the count below para
)

// expiredItemExchangeIDs are the goods_exchange rows of tables 40413 whose times_type is neither 7
// nor 8 (125 of 129); the row id is the item id.
var expiredItemExchangeIDs = []int64{
	620001, 620005, 620012, 620013, 620014, 620015, 620016, 621005, 641000, 653000,
	653001, 653003, 653005, 653013, 653030, 653031, 653033, 653035, 653036, 653039,
	653041, 653043, 653044, 653046, 653048, 653054, 653056, 653058, 653061, 653062,
	653063, 653065, 653088, 653113, 653114, 653119, 653120, 653122, 653125, 653126,
	653201, 654101, 654102, 654103, 654105, 654111, 654112, 654119, 654130, 654132,
	654143, 654144, 654148, 654149, 654151, 654170, 654171, 654172, 654181, 654182,
	654189, 654190, 654200, 654217, 654218, 654225, 654226, 654246, 654247, 654249,
	654251, 654252, 654254, 654270, 654271, 654272, 654274, 670001, 710007, 710008,
	710022, 710029, 710034, 710035, 710039, 710040, 710052, 710058, 710059, 710062,
	710070, 710077, 710078, 710079, 710080, 710081, 710082, 710087, 710095, 710101,
	710102, 710103, 710104, 991001, 991002, 991003, 991004, 991006, 991011, 992001,
	992002, 992007, 992008, 992100, 992101, 992103, 992104, 992105, 992106, 992108,
	992109, 992110, 992111, 992112, 992114,
}

func init() {
	registerFeature(Feature{
		Name:      expiredItemsFeature,
		Summary:   "OPT-IN: exchange owned event items the server reports as expired for their goods_exchange replacement (static-only, not validated live)",
		DefaultOn: false,
		Run:       runExpiredItems,
	})
}

// expiredItem is one owned item due for the exchange.
type expiredItem struct {
	id, count int64
}

func runExpiredItems(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	owned := expiredOwnedCounts(in)
	if len(owned) == 0 {
		return nil
	}
	b := &evBatch{conn: conn}
	r := b.send(expiredItemsFeature+": expiry list", expiredItemsListCmd, sfs.NewSFSObject())
	if r == nil {
		return b.err()
	}
	due := expiredItemsDue(r.Params, owned, evNow().Unix())
	slog.Info(expiredItemsFeature+": expiry list", "entries", len(evObjects(r.Params, "dataArr")),
		"ownedExchangeable", len(owned), "due", due)
	for _, it := range due {
		p := sfs.NewSFSObject()
		p.PutUtfString("goodsId", strconv.FormatInt(it.id, 10))
		p.PutInt("num", int32(min(it.count, math.MaxInt32)))
		if !evTaskSend(b, expiredItemsFeature+": exchange", expiredItemsRecoverCmd, p, "goodsId", it.id, "num", it.count) {
			break // the first refusal stops the exchanges
		}
	}
	return b.err()
}

// expiredOwnedCounts is GetItemCount for every owned item with an automated goods_exchange row.
func expiredOwnedCounts(in *Init) map[int64]int64 {
	out := map[int64]int64{}
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		id, ok := claimFirstInt(it, "itemId", "goodsId")
		if !ok || !slices.Contains(expiredItemExchangeIDs, id) {
			continue
		}
		if n, ok := claimFirstInt(it, "count", "num", "itemLeftCnt"); ok && n > 0 {
			out[id] = max(out[id], n)
		}
	}
	return out
}

// expiredItemsDue applies IsExpiredNow's server branch to the list reply: expireTime > 0 and <=
// nowSec, timeType 1 or 3, or 4 with the count below para.
func expiredItemsDue(list *sfs.SFSObject, owned map[int64]int64, nowSec int64) []expiredItem {
	var due []expiredItem
	for _, e := range evObjects(list, "dataArr") {
		id, ok := evNum(e, "id")
		count := owned[id]
		if !ok || count <= 0 {
			continue
		}
		exp, _ := evNum(e, "expireTime")
		if exp <= 0 || exp > nowSec {
			continue
		}
		tt, ok := evNum(e, "timeType")
		if !ok {
			tt = 1
		}
		switch tt {
		case 1, 3:
		case expiredTimeTypeCount:
			if para, ok := evNum(e, "para"); !ok || count >= para {
				continue
			}
		default:
			continue
		}
		due = append(due, expiredItem{id: id, count: count})
	}
	slices.SortFunc(due, func(a, b expiredItem) int { return int(a.id - b.id) })
	return slices.CompactFunc(due, func(a, b expiredItem) bool { return a.id == b.id })
}
