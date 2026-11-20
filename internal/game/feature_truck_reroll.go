package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Truck reroll (OPT-IN, MASTER G42, aud-railway/F9; the user ruled it in on 2026-11-19): the free
// "random" refresh of an own parked low-quality truck, so a later truck-deploy run may find a UR
// one. Static-only: from the 1.0.370 Lua and tables 40413, not sent live yet. It never deploys,
// never spends item 1520001 (train_para 12) or diamonds, and never sends train.batch.change (the
// ticket/diamond batch path, UILWTruckSuperDeparturePanelView.lua:981-1001).
//
// The client's path is the departure view's random button (UILWTrainDepartureView.lua:351-387):
// cost = myTrain:GetChangeCost(); with cost <= the held 1520001 count it sends
//
//	train.change {uuid: Long}  (LWMyStationDataManager:TryChangeTrain, :247-251;
//	                            ChangeTrainMessage.lua:5-8)
//
// and the reply's myTrain replaces the truck (ChangeTrainMessage.lua:13-19, OnTrainChanged :253-256,
// OnMyTrainRefreshOne :601-610, keyed by the truck station, so the uuid may change). A truck is
// rerolled only when all of these hold:
//   - the departure view would open: the city is at home or train_cross_server is on
//     (RailwayUtil.OpenUITruckDeparture, RailwayUtil.lua:66-72), and the truck is in
//     TruckStationState.Ready (LWMyStationDataManager.lua:750-777): the station is past
//     FirstReward, sendTime is 0, world squad #truck-index exists, and today's sends are below
//     train_para 5 plus effect 90103. Every opener of the view checks Ready (MyStationBtn.lua:
//     123-126, DepartureBtn.lua:63-70, RailwayUtil.lua:313-314; the train list shows its button
//     only then, MyTrainItem.lua:105-109). These are truck-deploy's gates;
//   - it is the account's own truck: an entry of myTrainList (an ownerId, when present, must be
//     the account's uid);
//   - its quality, looked up from cfgId in train_property (TrainData.lua:18-31; the wire carries
//     no quality), is known and below 4 (truckRerollQualityRanges). That is the line the client
//     draws when it asks "leave with the free refresh unused?" (UILWTrainDepartureView.lua:57-58)
//     and the aud-railway F9 verdict's: a reroll is random and can lower a truck, and an SSR
//     (quality 4) is one the player may send by hand. A UR truck or sleigh, or an id this table
//     doesn't know, is never rerolled;
//   - the reroll is free: changeCountByGold is present and the train_para 9 cost at index
//     changeCountByGold + 1 (Lua, 1-based; the last entry past the end) is 0
//     (TrainData:GetChangeCost, TrainData.lua:666-672; GET_CHANGE_COST,
//     LWMyStationDataManager.lua:63-73). Without changeCountByGold the client's cost is 1;
//   - its goods are readable and don't hold the "high goods" of train_para 55, which the client
//     confirms first (truck_tips10001, UILWTrainDepartureView.lua:371-379; JudgeTruckIsContainHighGoods,
//     TrainData.lua:839-867; GetHighQualityIncludeItem, LWMyStationDataManager.lua:1230-1249).
//     The sleigh's own confirmation (truck_tips10004, :362-370) never applies here.
//
// At most one train.change per truck per run, one a second; the reply's myTrain is logged at Info
// with the new quality, and this feature never looks at the new truck again. Any errorCode, a
// timeout or a reply without myTrain stops the run's rerolls with an error; no code is benign.
// When changeCountByGold resets is not in the client (aud-railway open question 3), so "free"
// rests on the server-sent count and the static 40413 row; nothing in the request names a cost.
//
// Run order: it should run before truck-deploy, which re-reads train.data and so sends a truck
// rerolled to UR in the same run (a run that reads the login's trainData instead sees it on the
// next run). A reroll scores in no Alliance Duel row (table score, 40413: the only truck row is
// type 98, a UR dispatch), so it declares no Duel.

const (
	truckRerollFeature = "truck-reroll"
	truckRerollCmd     = "train.change"

	// truckRerollChangeCost is train_para row 9 (40413), the per-reroll costs in item 1520001.
	truckRerollChangeCost = "0,1"
	// truckRerollHighGoods is train_para row 55 (40413): "itemId;count|...", any one met asks.
	truckRerollHighGoods = "2270000;2|500105;2"
	// truckRerollBelowQuality: only qualities below this are rerolled (UILWTrainDepartureView.lua:57).
	truckRerollBelowQuality = 4
	truckRerollPace         = time.Second
)

// truckRerollQualityRanges are the train_property rows with type 1 (truck) and quality 1..4 (N, R,
// SR, SSR; TrainData:GetQualityString) in tables 40413. UR (5) and the sleigh (10) are in
// railway_squads.go.
var truckRerollQualityRanges = map[int32][][2]int64{
	1: {{1, 30}, {501, 535}, {1031, 1035}},
	2: {{31, 60}, {536, 570}, {2031, 2035}},
	3: {{61, 90}, {571, 605}, {3031, 3035}},
	4: {{91, 120}, {606, 640}, {4031, 4035}},
}

// trSleep is time.Sleep; tests replace it.
var trSleep = time.Sleep

func init() {
	registerFeature(Feature{
		Name: truckRerollFeature,
		Summary: "OPT-IN, 1.0.370: the one free reroll (train.change) of each own parked truck below SSR, never " +
			"one carrying high goods, never a paid one; logs the new quality and never deploys " +
			"(static-only, not validated live)",
		Run: runTruckReroll,
	})
}

// truckQuality is the train_property quality of truck cfgId: 1..4 from truckRerollQualityRanges,
// 5 or 10 from railwayTruckQuality, 0 for an id neither table knows.
func truckQuality(cfgID int64) int32 {
	if q := railwayTruckQuality(cfgID); q != 0 {
		return q
	}
	for q, ranges := range truckRerollQualityRanges {
		for _, r := range ranges {
			if cfgID >= r[0] && cfgID <= r[1] {
				return q
			}
		}
	}
	return 0
}

// truckRerollCost is TrainData:GetChangeCost: with changeCountByGold n present, entry n+1 of
// train_para 9 (Lua, 1-based), or the last entry when there is none; 1 when n is absent.
func truckRerollCost(t *sfs.SFSObject) int64 {
	n, ok := claimInt(t, "changeCountByGold")
	if !ok {
		return 1
	}
	var costs []int64
	for _, s := range strings.Split(truckRerollChangeCost, ",") {
		c, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return 1
		}
		costs = append(costs, c)
	}
	if n >= 0 && n < int64(len(costs)) {
		return costs[n]
	}
	return costs[len(costs)-1]
}

// truckHighGoods is GetHighQualityIncludeItem: item id -> the count that makes a truck's goods
// "high" (train_para 55).
func truckHighGoods() map[int64]int64 {
	out := map[int64]int64{}
	for _, part := range strings.Split(truckRerollHighGoods, "|") {
		kv := strings.Split(part, ";")
		if len(kv) != 2 {
			continue
		}
		id, err1 := strconv.ParseInt(kv[0], 10, 64)
		n, err2 := strconv.ParseInt(kv[1], 10, 64)
		if err1 == nil && err2 == nil {
			out[id] = n
		}
	}
	return out
}

// truckGoodsHigh is JudgeTruckIsContainHighGoods over GetFullRewardData (extraGoods.full then
// baseGoods.full, TrainData.lua:304-319, 839-867): it sums each item's count across both and
// reports whether any train_para 55 item reaches its count. readable is false, and the truck must
// be skipped, when the goods can't be judged: baseGoods or extraGoods missing (the client indexes
// both), no base cargo at all, or an entry that doesn't decode. An item entry is {type, value:
// {id, num}}; any other entry is {type, value: number}, and the client then counts type as the id
// with the value scaled up by a resource effect, so such an entry whose type is a high-goods id
// counts as high whatever its value.
func truckGoodsHigh(t *sfs.SFSObject) (high, readable bool) {
	base, extra := evObject(t, "baseGoods"), evObject(t, "extraGoods")
	if base == nil || extra == nil || len(claimObjects(base, "full")) == 0 {
		return false, false
	}
	limits := truckHighGoods()
	counts := map[int64]int64{}
	forced := map[int64]bool{}
	for _, g := range []*sfs.SFSObject{extra, base} {
		v, ok := g.Get("full")
		if !ok {
			continue // mergeArray takes a nil list as empty
		}
		arr, ok := v.Val.(*sfs.SFSArray)
		if !ok {
			return false, false
		}
		for _, it := range arr.Items() {
			e, ok := it.Val.(*sfs.SFSObject)
			if !ok {
				return false, false
			}
			typ, okType := claimInt(e, "type")
			if item := evObject(e, "value"); item != nil {
				id, okID := claimInt(item, "id")
				num, okNum := claimInt(item, "num")
				if !okID || !okNum {
					return false, false
				}
				counts[id] += num
				continue
			}
			if _, okVal := duelFloat(e, "value"); !okVal || !okType {
				return false, false
			}
			if _, isHigh := limits[typ]; isHigh {
				forced[typ] = true
			}
		}
	}
	for id, n := range limits {
		if forced[id] || counts[id] >= n {
			return true, true
		}
	}
	return false, true
}

func runTruckReroll(conn *session.GameConn, in *Init) error {
	const feature = truckRerollFeature
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if !railwayAtHome(in) {
		slog.Info(feature + ": the city is in another warzone and " + railwayCrossSwitch + " is off; skipping, as the client does")
		return nil
	}
	station := railwayStation(conn, in, feature)
	if !railwayStationOpen(in, station, claimNow(in).UnixMilli()) {
		slog.Info(feature + ": the railway station isn't open yet (fog, start time or opening reward); skipping")
		return nil
	}
	add, _ := initEffect(in, truckDeploySendsAdd)
	maxSends := truckDeployBaseSends + int64(math.Floor(add))
	if sent, ok := claimInt(station, "dailySendCount"); !ok || sent >= maxSends {
		slog.Info(feature+": daily send count unknown or used up; the departure view doesn't open", "sent", sent,
			"max", maxSends, "known", ok)
		return nil
	}

	uid, _ := railwayOwnIDs(in)
	// railwayOwnTrucks reads only some fields; the reroll's come from the same entry, joined by
	// uuid. A uuid listed twice is ambiguous and skipped.
	raw := map[int64]*sfs.SFSObject{}
	seen := map[int64]int{}
	for _, t := range claimObjects(station, "myTrainList") {
		if id, ok := claimInt(t, "uuid"); ok {
			raw[id] = t
			seen[id]++
		}
	}
	unlocked := railwayUnlockedSquads(in)
	done := map[int64]bool{}
	sentAny := false
	for _, tr := range railwayOwnTrucks(in, station) {
		t := raw[tr.uuid]
		if tr.uuid == 0 || t == nil || seen[tr.uuid] != 1 || done[tr.uuid] || tr.sendTime != 0 {
			continue
		}
		if _, ok := unlocked[tr.slot]; tr.slot == 0 || !ok {
			continue
		}
		if owner, ok := claimString(t, "ownerId"); ok && owner != "" && owner != uid {
			slog.Info(feature+": truck skipped: not the account's own", "uuid", tr.uuid)
			continue
		}
		q := truckQuality(tr.cfgID)
		if q == 0 || q >= truckRerollBelowQuality {
			slog.Info(feature+": truck kept: quality unknown or SSR and up", "uuid", tr.uuid, "cfgId", tr.cfgID, "quality", q)
			continue
		}
		if cost := truckRerollCost(t); cost != 0 {
			slog.Info(feature+": truck kept: no free reroll left", "uuid", tr.uuid, "quality", q, "cost", cost)
			continue
		}
		if high, ok := truckGoodsHigh(t); !ok || high {
			slog.Info(feature+": truck kept: its goods are high or unreadable (the client asks first)", "uuid", tr.uuid,
				"quality", q, "readable", ok)
			continue
		}

		if sentAny {
			trSleep(truckRerollPace)
		}
		sentAny = true
		done[tr.uuid] = true
		p := sfs.NewSFSObject()
		p.PutLong("uuid", tr.uuid)
		msg, err := session.SendAndWait(conn, feature, truckRerollCmd, p)
		if msg == nil {
			return errors.Join(fmt.Errorf("%s: %s for truck %d got no reply", feature, truckRerollCmd, tr.uuid), err)
		}
		if msg.Params.Has("errorCode") {
			code, _ := claimString(msg.Params, "errorCode")
			slog.Info(feature+": "+truckRerollCmd+" refused; stopping", "uuid", tr.uuid, "errorCode", code)
			return fmt.Errorf("%s: %s for truck %d refused: %s", feature, truckRerollCmd, tr.uuid, code)
		}
		mt := evObject(msg.Params, "myTrain")
		if mt == nil {
			slog.Info(feature+": "+truckRerollCmd+" reply has no myTrain; stopping", "uuid", tr.uuid, "keys", msg.Params.Keys())
			return fmt.Errorf("%s: %s for truck %d: reply has no myTrain", feature, truckRerollCmd, tr.uuid)
		}
		newUUID, _ := claimInt(mt, "uuid")
		newCfg, _ := claimInt(mt, "cfgId")
		newCount, newCountOK := claimInt(mt, "changeCountByGold")
		oldCount, _ := claimInt(t, "changeCountByGold")
		slog.Info(feature+": truck rerolled", "uuid", tr.uuid, "cfgId", tr.cfgID, "quality", q,
			"newUuid", newUUID, "newCfgId", newCfg, "newQuality", truckQuality(newCfg),
			"changeCountByGold", oldCount, "newChangeCountByGold", newCount, "newCountKnown", newCountOK)
	}
	if !sentAny {
		slog.Info(feature + ": no parked truck with a free reroll")
	}
	return nil
}
