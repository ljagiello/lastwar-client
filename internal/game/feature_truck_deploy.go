package game

import (
	"cmp"
	"errors"
	"log/slog"
	"math"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Truck deploy (OPT-IN): send the account's own UR trade trucks, the sleigh included, and never
// anything below UR. That is the owner's hard rule; a lower-quality truck stays parked.
// Static-only: from the 1.0.364 Lua, not sent live yet.
//
// The client's path is LWMyStationDataManager:TryDepartureTrain:
//  1. formation.save.template {index: 4+slot, heroInfos, action: 4, chipEquipGroup?, squadNo?};
//  2. train.send {uuid, squadNo, squadNoClient: slot, heroInfo, chipEquipGroup?}.
//
// The reply carries dailySendCount (RailwayUtil.HandleDepartureTrain). A truck can leave in
// TruckStationState.Ready (GetTruckStationStateByTrainData): the station is past FirstReward,
// sendTime is 0, world squad #truck-index exists, and today's sends are below train_para 5 (4 in
// the 1.0.364 table) plus game effect 90103. The cross-server gate of the cargo claim applies too.
//
// Squads:
//   - The defence templates (formation_template index 5..8).
//   - A squad is busy while any of the account's trucks names it in marchInfo.squadNoClient
//     (GetBusyDefenceFormationIndexList).
//   - As a stricter rule than the client, a squad is also skipped if one of its heroes is on a
//     truck that is out.
//   - The strongest free squad goes to the best truck. The UI defaults to
//     GetMaxBattlePowerFreeDefenceFormation; the batch path goes by truck index instead.
//   - On truck_tips10007 ("The current Squad is escorting a Truck", an errorCode with the truck
//     uuid) the next squad is tried.
//
// The Alliance Duel scores a UR dispatch (type 98, value "5") on Tue and Sat, but unused sends are
// lost at the daily reset, so this always runs (DUEL.md §7).
const (
	truckDeployFeature   = "truck-deploy"
	truckDeploySendCmd   = "train.send"
	truckDeployBaseSends = 4       // train_para 5
	truckDeploySendsAdd  = "90103" // EffectDefine.MAX_DAILY_COUNT_ADD
)

// railwayTruckStations are LW_BUILD_TRUCK_STATION_1..4, truck slots 1..4
// (LWMyStationDataManager TruckBuildId2Index).
var railwayTruckStations = map[int64]int32{10138000: 1, 10139000: 2, 10140000: 3, 10141000: 4}

func init() {
	registerFeature(Feature{
		Name:    truckDeployFeature,
		Summary: "OPT-IN: deploy the account's own UR trade trucks (the sleigh too) with the strongest free defence squad; never sends anything below UR",
		Duel:    []DuelScore{{Type: DuelScoreURTruck, Value: "5"}},
		Run:     runTruckDeploy,
	})
}

// ownTruck is one of the account's own trucks.
type ownTruck struct {
	uuid, cfgID, sendTime int64
	slot                  int32 // 1..4 from the truck-station building, 0 if unknown
	squad                 int32 // marchInfo.squadNoClient
	heroIDs               []int64
}

func railwayOwnTrucks(in *Init, station *sfs.SFSObject) []ownTruck {
	bID := map[int64]int64{}
	for _, b := range in.Objects("building_new") {
		uuid, _ := evNum(b, "uuid")
		id, _ := evNum(b, "bId")
		bID[uuid] = id
	}
	var out []ownTruck
	for _, t := range claimObjects(station, "myTrainList") {
		var tr ownTruck
		tr.uuid, _ = claimInt(t, "uuid")
		tr.cfgID, _ = claimInt(t, "cfgId")
		tr.sendTime, _ = claimInt(t, "sendTime")
		rel, _ := claimInt(t, "relationId")
		tr.slot = railwayTruckStations[bID[rel]]
		mi := evObject(t, "marchInfo")
		if n, ok := claimInt(mi, "squadNoClient"); ok {
			tr.squad = int32(n)
		}
		if hi := evObject(mi, "heroInfo"); hi != nil {
			for _, k := range hi.Keys() {
				if id, ok := claimInt(evObject(hi, k), "id"); ok && id != 0 {
					tr.heroIDs = append(tr.heroIDs, id)
				}
			}
		}
		out = append(out, tr)
	}
	return out
}

func runTruckDeploy(conn *session.GameConn, in *Init) error {
	const feature = truckDeployFeature
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if !railwayAtHome(in) {
		slog.Info(feature + ": the city is in another warzone and " + railwayCrossSwitch + " is off; skipping, as the client does")
		return nil
	}
	station := railwayStation(conn, in, feature)
	now := claimNow(in).UnixMilli()
	if !railwayStationOpen(in, station, now) {
		slog.Info(feature + ": the railway station isn't open yet (fog, start time or opening reward); skipping")
		return nil
	}
	add, _ := initEffect(in, truckDeploySendsAdd)
	maxSends := truckDeployBaseSends + int64(math.Floor(add))
	sent, ok := claimInt(station, "dailySendCount")
	if !ok || sent >= maxSends {
		slog.Info(feature+": daily send count unknown or used up", "sent", sent, "max", maxSends, "known", ok)
		return nil
	}

	unlocked := railwayUnlockedSquads(in)
	trucks := railwayOwnTrucks(in, station)
	busy := map[int32]bool{}
	busyHeroIDs := map[int64]bool{}
	var ready []ownTruck
	for _, tr := range trucks {
		if tr.squad > 0 {
			busy[tr.squad] = true
		}
		if tr.sendTime > 0 {
			for _, id := range tr.heroIDs {
				busyHeroIDs[id] = true
			}
			continue
		}
		if _, ok := unlocked[tr.slot]; tr.slot == 0 || !ok {
			continue
		}
		if !railwayIsURTruck(tr.cfgID) {
			slog.Info(feature+": truck left parked: below UR", "uuid", tr.uuid, "cfgId", tr.cfgID)
			continue
		}
		ready = append(ready, tr)
	}
	if len(ready) == 0 {
		slog.Info(feature + ": no parked UR truck")
		return nil
	}

	heroID := map[int64]int64{}
	for _, h := range claimObjectsOrMap(in.Raw, "userHero") {
		uuid, _ := claimInt(h, "uuid")
		id, _ := claimInt(h, "heroId")
		heroID[uuid] = id
	}
	stats := railwayHeroStats(conn)
	type ranked struct {
		railwaySquad
		power int64
	}
	var squads []ranked
	for _, sq := range railwaySquads(in, railwayDefenceAction) {
		if busy[sq.slot] || sq.mainHeroes() == 0 {
			continue
		}
		if slices.ContainsFunc(sq.heroes, func(h railwayHero) bool { id := heroID[h.uuid]; return id != 0 && busyHeroIDs[id] }) {
			slog.Info(feature+": squad skipped: one of its heroes is out with a truck", "squad", sq.slot)
			continue
		}
		squads = append(squads, ranked{sq, railwayPower(in, sq, stats, false)})
	}
	slices.SortStableFunc(squads, func(a, b ranked) int { return cmp.Compare(b.power, a.power) })
	slices.SortStableFunc(ready, func(a, b ownTruck) int {
		return cmp.Or(cmp.Compare(railwayTruckQuality(b.cfgID), railwayTruckQuality(a.cfgID)), cmp.Compare(a.slot, b.slot))
	})

	var errs []error
	for _, tr := range ready {
		if sent >= maxSends || len(squads) == 0 {
			break
		}
		for len(squads) > 0 {
			sq := squads[0]
			squads = squads[1:]
			if err := railwaySaveSquad(conn, feature, sq.railwaySquad, railwayDefenceAction); err != nil {
				// Any save failure stops the run, a timeout included (a late reply could match).
				return errors.Join(append(errs, err)...)
			}
			p := sfs.NewSFSObject()
			p.PutLong("uuid", tr.uuid)
			p.PutInt("squadNo", sq.squadNo)
			p.PutInt("squadNoClient", sq.slot)
			p.PutSFSArray("heroInfo", railwayHeroArray(sq.railwaySquad))
			if sq.chip >= 1 && sq.chip <= 4 {
				p.PutInt("chipEquipGroup", sq.chip)
			}
			msg, err := session.SendAndWait(conn, feature+" send", truckDeploySendCmd, p)
			if msg == nil {
				// A timeout or a dead connection: a late reply could match the next send.
				return errors.Join(append(errs, err)...)
			}
			if !msg.Params.Has("errorCode") {
				sent++ // never let a reply lower the count
				if n, ok := claimInt(msg.Params, "dailySendCount"); ok {
					sent = max(sent, n)
				}
				slog.Info(feature+": UR truck sent", "uuid", tr.uuid, "quality", railwayTruckQuality(tr.cfgID),
					"squad", sq.slot, "squadPowerAtLeast", sq.power, "sentToday", sent)
				break
			}
			if msg.Params.Has("uuid") {
				slog.Info(feature+": squad is escorting a truck; trying the next one", "squad", sq.slot)
				continue
			}
			// Any other refusal (e.g. no sends left) stops the run.
			return errors.Join(append(errs, err)...)
		}
	}
	return errors.Join(errs...)
}
