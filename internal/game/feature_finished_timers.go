package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Finished-timer collection (MASTER.md §4 #12): the free completion of work whose timer has already
// run out. None of these is an instant finish: each request carries only the uuid (or nothing),
// and the 1.0.364 client sends it only once the timer has ended (datacenter.md §(c)2; docs/
// city-building.mdx:35 and :40 wrongly call the first two paid). The paid paths are the separate
// `build.ccd.*` / `queue.ccd.*` speed-ups and `free.building.up.new {gold}`, never sent here.
//
//   - Upgrades: `free.building.upgrade.finish {uuid: Long}` for each init building_new entry with
//     uT > 0 && now >= uT (ms; BuildingDate.lua:439-442; FreeBuildingUpgradeFinishMessage.lua:3-9;
//     BuildManager.lua:2892-2992). Skipped: the wormhole sub-building (792000) at level 0
//     (:2938-2940), and HQ level 7 while defend_wall.protectEndTime (ms) >= now, because finishing
//     HQ 8 drops the newbie shield and the client asks first (:2944-2959); HQ 7 is also skipped
//     when init has no defend_wall.
//   - Repairs: `user.finish.fix.building {uuid: Long}` when dEndT > 0 && now >= dEndT (ms;
//     BuildingDate.lua:446-449; UserFinishFixBuildingMessage.lua:3-6).
//   - Queues: `queue.finish {uuid: Long}` (QueueFinishMessage.lua:3-8) for each init queue_new
//     entry in state Finish -- (sT != 0 && sT == uT) || (uT != 0 && now >= uT), with startTime /
//     updateTime overriding sT / uT (QueueInfo.lua:34-124) -- of type 6 research
//     (ScienceManager.lua:120-139), 3 hospital (HospitalManager.lua:355-372) or 117 rebirth
//     hospital (RebirthHospitalManager.lua:347-364). Other types are not sent: pastures use
//     queue.finish.batch, the dragon hospital is battlefield-only and auto-sent, T11 breaks are
//     optional.
//   - Camp output: `building.camp.collect {uuid: Long}` (BuildingCampCollectMessage.lua:3-6) for the
//     Military Camp (10103000), Smith Shop (10101000) and Tactical Chip Factory (10232000) with
//     lv > 0 && prodST > 0 && now >= prodET (BuildingUtils.lua:1813-1852; BuildBubbleManager.lua:
//     587-598, 2289-2321). These three are the only building types the command accepts (Go saw
//     E000001 "Building type error" for production lines). For the chip factory the city bubble
//     sends building.camp.collect, whose reply handler has a chip branch (:45-61); the factory panel
//     sends `chip.collect {}` instead (TacticalChipFactoryCreatePage.lua:576-578), so that is the
//     fallback when the factory answers E000001.
//
// Troop stock cap: the client refuses to take healed troops, or Military Camp output, when the
// player's soldier total is at LW_SOLDIER_MAX_STOCK (effect 30122), and camp output larger than the
// cap (HospitalManager.lua:357-362; BuildingUtils.lua:1889-1902, tip 120083). The total is the
// resource items 3005-3015 (table lw_soldier, type 1) from init resource_items
// (SoldierDataManager.lua:75-162). The cap is GetGameEffect(30122), a sum of eight sources of which
// init effect carries only the base, so init effect["30122"] is a lower bound: checking against it
// can only skip a collect the client would allow, never the reverse. Without effect["30122"] or
// resource_items, hospital, rebirth and camp collects are skipped. Static-only: not sent live yet.
const (
	finishedTimersUpgradeCmd = "free.building.upgrade.finish"
	finishedTimersRepairCmd  = "user.finish.fix.building"
	finishedTimersQueueCmd   = "queue.finish"
	finishedTimersCampCmd    = "building.camp.collect"
	finishedTimersChipCmd    = "chip.collect"

	finishedTimersMilitaryCamp  = 10103000
	finishedTimersSmithShop     = 10101000
	finishedTimersChipFactory   = 10232000
	finishedTimersWormholeSub   = 792000
	finishedTimersShieldHQLevel = 7

	finishedTimersQueueHospital = 3
	finishedTimersQueueResearch = 6
	finishedTimersQueueRebirth  = 117

	finishedTimersStockEffect = "30122" // LW_SOLDIER_MAX_STOCK (EnumType.lua:4442)
)

func init() {
	registerFeature(Feature{
		Name:    "finished-timers",
		Summary: "collect ended timers for free: finished upgrades, repairs, research/heal queues and camp output",
		Run:     runFinishedTimers,
	})
	session.RegisterBenignErrorCode("120289", finishedTimersUpgradeCmd, finishedTimersRepairCmd,
		finishedTimersQueueCmd, finishedTimersCampCmd, finishedTimersChipCmd)
}

// finishedTimersQueue is one init queue_new entry, with the client's field precedence.
type finishedTimersQueue struct {
	UUID, Type, Start, End int64
	Raw                    *sfs.SFSObject
}

func finishedTimersQueues(in *Init) []finishedTimersQueue {
	var out []finishedTimersQueue
	for _, q := range in.Objects("queue_new") {
		uuid, ok := claimInt(q, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		e := finishedTimersQueue{UUID: uuid, Raw: q}
		e.Type, _ = claimInt(q, "type")
		e.Start, _ = claimFirstInt(q, "startTime", "sT")
		e.End, _ = claimFirstInt(q, "updateTime", "uT")
		out = append(out, e)
	}
	return out
}

// Finished is QueueInfo:SetQueueState's Finish state.
func (q finishedTimersQueue) Finished(nowMs int64) bool {
	return (q.Start != 0 && q.Start == q.End) || (q.End != 0 && nowMs >= q.End)
}

// Working is QueueInfo:SetQueueState's Work state.
func (q finishedTimersQueue) Working(nowMs int64) bool {
	return (q.Start == 0 || q.Start != q.End) && nowMs < q.End
}

// finishedTimersStockRoom returns the soldier total and the (lower-bound) stock cap; ok is false
// when init lacks either.
func finishedTimersStockRoom(in *Init) (total, limit int64, ok bool) {
	effects := in.Object("effect")
	v, has := effects.Get(finishedTimersStockEffect)
	if !has {
		return 0, 0, false
	}
	var capF float64
	switch n := v.Val.(type) {
	case float64:
		capF = n
	case float32:
		capF = float64(n)
	default:
		i, iok := claimIntValue(n)
		if !iok {
			return 0, 0, false
		}
		capF = float64(i)
	}
	if _, has := in.Raw.Get("resource_items"); !has {
		return 0, 0, false
	}
	for _, it := range claimObjectsOrMap(in.Raw, "resource_items") {
		id, _ := claimInt(it, "itemId")
		if id < 3005 || id > 3015 {
			continue
		}
		if n, ok := claimFirstInt(it, "number", "count"); ok && n > 0 {
			total += n
		}
	}
	return total, int64(math.Floor(capF)), true
}

func runFinishedTimers(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("finished-timers: no init push; skipping")
		return nil
	}
	now := claimNow(in).UnixMilli()
	soldiers, stockCap, stockKnown := finishedTimersStockRoom(in)
	stockRoom := stockKnown && soldiers < stockCap

	var actions []func() error
	send := func(label, cmd string, uuid int64) {
		actions = append(actions, func() error {
			p := sfs.NewSFSObject()
			p.PutLong("uuid", uuid)
			_, err := claimAndLog(conn, label, cmd, p)
			return err
		})
	}

	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		lv, _ := claimInt(b, "lv")
		name := fmt.Sprintf("%s %d", BuildingNameOf(int32(bID)), uuid)

		if uT, _ := claimInt(b, "uT"); uT > 0 && now >= uT {
			switch {
			case bID == finishedTimersWormholeSub && lv == 0:
			case bID == claimHQBuildingID && lv == finishedTimersShieldHQLevel && finishedTimersShieldUp(in, now):
				slog.Info("finished-timers: leaving the HQ 8 upgrade to the player while the shield is up", "uuid", uuid)
			default:
				send("finish upgrade "+name, finishedTimersUpgradeCmd, uuid)
			}
		}
		if dEndT, _ := claimInt(b, "dEndT"); dEndT > 0 && now >= dEndT {
			send("finish repair "+name, finishedTimersRepairCmd, uuid)
		}
		if finishedTimersCampDone(b, now) {
			switch bID {
			case finishedTimersMilitaryCamp:
				prodBase, _ := claimInt(b, "prodBase")
				if !stockRoom || prodBase > stockCap {
					slog.Info("finished-timers: camp output skipped (troop stock full or unknown)", "uuid", uuid,
						"soldiers", soldiers, "stockCap", stockCap, "known", stockKnown)
					continue
				}
				send("collect camp "+name, finishedTimersCampCmd, uuid)
			case finishedTimersSmithShop:
				send("collect smith "+name, finishedTimersCampCmd, uuid)
			case finishedTimersChipFactory:
				actions = append(actions, func() error { return finishedTimersChip(conn, uuid) })
			}
		}
	}

	for _, q := range finishedTimersQueues(in) {
		if !q.Finished(now) {
			continue
		}
		switch q.Type {
		case finishedTimersQueueResearch:
			send(fmt.Sprintf("finish research queue %d", q.UUID), finishedTimersQueueCmd, q.UUID)
		case finishedTimersQueueHospital, finishedTimersQueueRebirth:
			if !stockRoom {
				slog.Info("finished-timers: healed troops left in the hospital (troop stock full or unknown)",
					"queue", q.UUID, "type", q.Type, "soldiers", soldiers, "stockCap", stockCap, "known", stockKnown)
				continue
			}
			send(fmt.Sprintf("finish hospital queue %d (type %d)", q.UUID, q.Type), finishedTimersQueueCmd, q.UUID)
		}
	}

	if len(actions) == 0 {
		slog.Info("finished-timers: no ended timer to collect")
		return nil
	}
	return errors.Join(claimEach(actions, func(a func() error) error { return a() })...)
}

// finishedTimersShieldUp reports whether the city shield is up (defend_wall.protectEndTime >= now),
// treating a missing defend_wall as up so HQ 7 is never finished blind.
func finishedTimersShieldUp(in *Init, nowMs int64) bool {
	end, ok := claimInt(in.Object("defend_wall"), "protectEndTime")
	return !ok || end >= nowMs
}

// finishedTimersCampDone is BuildingFunctioningFinish: lv > 0, a production cycle started and its
// end passed.
func finishedTimersCampDone(b *sfs.SFSObject, nowMs int64) bool {
	lv, _ := claimInt(b, "lv")
	start, _ := claimInt(b, "prodST")
	end, ok := claimInt(b, "prodET")
	return lv > 0 && start > 0 && ok && end > 0 && nowMs >= end
}

// finishedTimersChip collects the Tactical Chip Factory the way the city bubble does and falls back
// to the factory panel's chip.collect {} if the server rejects the building type.
func finishedTimersChip(conn *session.GameConn, uuid int64) error {
	p := sfs.NewSFSObject()
	p.PutLong("uuid", uuid)
	label := fmt.Sprintf("collect chip factory %d", uuid)
	msg, err := claimAndLog(conn, label, finishedTimersCampCmd, p)
	if err != nil && msg != nil && msg.Params.GetString("errorCode") == "E000001" {
		_, err = claimAndLog(conn, label+" (chip.collect)", finishedTimersChipCmd, sfs.NewSFSObject())
	}
	return err
}
