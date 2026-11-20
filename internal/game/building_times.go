package game

import "lastwar-client/internal/sfs"

// Building upgrade end times as the client sees them (MASTER B72, cov-dc-social-city/F6).
//
// The client loads a building's upgrade end from init building_new[].uT and then, still inside the
// init handler, overrides it from the build queues: InitMessage.lua runs BuildManager:InitData at
// :46 and BuildQueueManager:InitQueueList at :121, and for every queue_new entry of type 0 (NORMAL)
// or 110 (SPECIAL) (LWBuildQueueType, Global/EnumType.lua:3243) UpdateQueueData calls
// BuildManager:UpdateBuildingTimeByUuid(queueData.occupyUuid, q.uT) whenever the entry has a uT
// (DataCenter/BuildQueueManager/BuildQueueManager.lua:18-55, the call at :48-50;
// BuildManager.lua:4987-4991). occupyUuid is tonumber(itemObj.itemId), 0 without an itemObj
// (BuildQueueInfo.lua:43-52). IsUpgradeFinish then reads the overridden value
// (BuildingDate.lua:453-456). Lua's `if q.uT` is true for 0 as well, so a present uT of 0 also
// overrides. The two values normally agree; no divergence has been seen live.
//
// buildingUpgradeTime is the one reader for it; finished-timers, alliance-call-help and
// duel-speedups use it.

const (
	buildQueueNormal  = 0   // LWBuildQueueType.NORMAL
	buildQueueSpecial = 110 // LWBuildQueueType.SPECIAL
)

// buildQueueUpgradeTimes maps a building uuid to the uT of the init build-queue entry occupying it.
// Were two entries to name the same building, the later one in queue_new wins (the client walks
// the list with pairs, so its order is undefined there).
func buildQueueUpgradeTimes(in *Init) map[int64]int64 {
	out := map[int64]int64{}
	for _, q := range in.Objects("queue_new") {
		if t, ok := claimInt(q, "type"); !ok || (t != buildQueueNormal && t != buildQueueSpecial) {
			continue
		}
		uT, ok := claimInt(q, "uT")
		if !ok {
			continue
		}
		occupant, ok := buildQueueOccupant(q)
		if !ok {
			continue
		}
		out[occupant] = uT
	}
	return out
}

// buildQueueOccupant returns the building uuid in a build-queue entry's itemObj.itemId, sent as a
// string or a number (tonumber in the client); ok is false without one.
func buildQueueOccupant(q *sfs.SFSObject) (int64, bool) {
	v, ok := q.Get("itemObj")
	if !ok {
		return 0, false
	}
	item, ok := v.Val.(*sfs.SFSObject)
	if !ok {
		return 0, false
	}
	id, ok := claimInt(item, "itemId")
	return id, ok && id != 0
}

// buildingUpgradeTime returns the upgrade end time (unix ms, 0 for none) the client uses for the
// init building_new entry b: the uT of the build queue occupying it when there is one, otherwise
// b's own uT. overridden is true when the build queue supplied the value.
func buildingUpgradeTime(in *Init, b *sfs.SFSObject) (uT int64, overridden bool) {
	uT, _ = claimInt(b, "uT")
	uuid, ok := claimInt(b, "uuid")
	if !ok || uuid == 0 {
		return uT, false
	}
	if q, ok := buildQueueUpgradeTimes(in)[uuid]; ok {
		return q, true
	}
	return uT, false
}
