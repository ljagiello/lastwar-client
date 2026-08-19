package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// Base building-type ids, definitively resolved from
// extracted/lua_decompiled/2815_Global_EnumType.lua (the authoritative
// BuildingTypes enum -- internal dev codenames, e.g. LW_BUILD_QUARRY) cross
// -referenced against extracted/locale_extracted/en.decompressed (the
// shipped English display strings, via each building's table_decompiled/
// building.lua `name` loc-key). A building instance's own `bId` field
// equals its base type id regardless of level (level is a separate `lv`
// field).
//
// The original 6 requested buildings (below) plus 6 more CONFIRMED live via
// direct testing against the real account: Oil Well, Drone Parts Workshop,
// Component Factory, and 4 Season 6 "Spore Factory" tiers all responded to
// building.production.collect with real collections (status=1).
//
// Component Factory and Tactical Institute (LW_BUILD_SQUAD_EQUIP_FACTORY,
// 10214000, and LW_BUILD_DOMINATOR_TRAIN, 10235000) were requested by name
// early on and initially mismatched to Drone Parts Workshop (10233000) and
// Armament Institute (10227000) respectively -- a static-analysis guess
// that turned out wrong, only caught because those two names' real
// buildings were never actually being collected and a live packet capture
// of the real client tapping them directly surfaced their true uuids/bIds.
// Both are locale-confirmed (building.lua row 10214000's name field is
// loc-key "2000574" = "Component Factory"; `building_name_10235000` =
// "Tactical Institute", found only after correcting for a locale-file
// parser desync at that exact offset -- see reports/ for the general
// parser-artifact pattern). Component Factory returned a real status=1
// collection (resId=630011) on first live test; Tactical Institute got
// errorCode=602026 "in production, please be patient" -- but unlike
// Tactical Center/Truck Station below, its raw building_new data has real
// prodST/prodStatus=1/prodET timestamps, so this reads as genuinely not
// due yet, not dormant. Drone Parts Workshop and Armament Institute are
// kept below too -- they're real, separately-confirmed buildings in their
// own right, just not what "Component Factory"/"Tactical Institute"
// actually turned out to mean.
//
// Tactical Center, Armament Institute, and all 4 Truck Station tiers are
// NOT confirmed the same way -- flagging this explicitly since an earlier
// pass overclaimed it. All 6 accept building.production.collect (never
// errorCode=E000001 "Building type error", so the command pairing itself
// is real -- confirmed against Training Base on building.camp.collect,
// which DOES get E000001, see the dossier), but every single attempt
// across multiple full collection runs tonight got errorCode=602026 "in
// production, please be patient" -- zero successful collections, ever, on
// any of these 6. Their raw building_new data is also missing prodST/
// prodT/prodStatus/prodET entirely (every successfully-collecting building
// has these populated) and all 6 sit at lv=1 -- consistent with a
// production cycle that has never been started, not one that's merely
// slow. No "start production"/"dispatch" command was found for any of
// them anywhere in the ~3,178-command catalog. Kept in collectCmdFor below
// on the theory that the pairing is real and something (an explicit
// start action from the real client, an unusually long first cycle, or a
// tech/unlock prerequisite) just hasn't happened yet on this account --
// but treat this as unverified, not confirmed, until one of them actually
// returns status=1.
//
// "Armed Truck" itself is RESOLVED, and it's neither of the above two
// leads: it's not a building at all, and not LW_BUILD_TRUCK_STATION_1-4
// (kept below anyway since that pairing still looks real, see above).
// Confirmed via a live packet capture of the real game client (not a
// guess): "Armed Truck" is one of two independent tracks in a dedicated
// "Truck Rewards" idle/AFK panel (the other is "Overlord"), collected via
// one account-level command, `lw.pve.idle.reward` -- see
// CollectIdleReward below. This is genuinely confirmed: the real client's
// own capture showed a real reward pool, a claim, and the pool resetting
// to empty afterward, and re-running the same three-call sequence through
// this Go client got the identical result -- a real claimed reward with
// updated account resource totals in the response, followed by an empty
// pool on the next peek.
//
// Scene/LWHummerScene is the one clearly-separate thing here: a genuinely
// distinct real-time driving/combat minigame (its own
// HummerSceneUnitType/TriggerType/BuffType enums). No network command
// referencing "hummer" exists anywhere in the ~3,178-command catalog; its
// rewards come from actually playing a timed 3D session, not a single
// request/response call. This is not automatable the way every other
// entry in this file is, and is NOT included below.
const (
	BuildingFarmland          int32 = 10201000 // LW_BUILD_FARMLAND
	BuildingIronMine          int32 = 10202000 // LW_BUILD_QUARRY
	BuildingGoldMine          int32 = 10207000 // LW_BUILD_GOLD_MILL
	BuildingSmelter           int32 = 10209000 // LW_BUILD_SMELTERY
	BuildingMaterialWorkshop  int32 = 10211000 // LW_BUILD_MATERIALS_WORKERSHOP
	BuildingTrainingBase      int32 = 10210000 // LW_BUILD_TRAINING_CENTER
	BuildingOilWell           int32 = 10221000 // LW_BUILD_PETROLEUM -- confirmed live, collects petroleum (resId=23)
	BuildingDronePartsShop    int32 = 10233000 // LW_BUILD_TACTICAL_COMPONENT -- confirmed live, collects item 7038 "Drone Parts"
	BuildingTacticalCenter    int32 = 10143000 // LW_BUILD_TACTICAL_CENTER -- no locale display name found; confirmed live as a valid production-line building type
	BuildingArmamentInst      int32 = 10227000 // LW_BUILD_... "Armament Institute" -- confirmed live as a valid production-line building type
	BuildingSporeFactory1     int32 = 842000   // LW_BUILD_SEASON6_QUARTZ_FACTORY1 -- Season 6 only; confirmed live, collects obsidian (resId=10)
	BuildingSporeFactory2     int32 = 843000   // LW_BUILD_SEASON6_QUARTZ_FACTORY2
	BuildingSporeFactory3     int32 = 844000   // LW_BUILD_SEASON6_QUARTZ_FACTORY3
	BuildingSporeFactory4     int32 = 845000   // LW_BUILD_SEASON6_QUARTZ_FACTORY4
	BuildingTruckStation1     int32 = 10138000 // LW_BUILD_TRUCK_STATION_1 -- NOT "Armed Truck" (see CollectIdleReward for the real thing); no locale display name found; unconfirmed valid production-line pairing, kept speculatively
	BuildingTruckStation2     int32 = 10139000 // LW_BUILD_TRUCK_STATION_2
	BuildingTruckStation3     int32 = 10140000 // LW_BUILD_TRUCK_STATION_3
	BuildingTruckStation4     int32 = 10141000 // LW_BUILD_TRUCK_STATION_4
	BuildingComponentFactory  int32 = 10214000 // LW_BUILD_SQUAD_EQUIP_FACTORY -- the REAL "Component Factory"; confirmed live, real status=1 collection (resId=630011)
	BuildingTacticalInstitute int32 = 10235000 // LW_BUILD_DOMINATOR_TRAIN -- the REAL "Tactical Institute"; real active production data (unlike Tactical Center below) but not yet confirmed with a status=1 collection
)

var buildingNames = map[int32]string{
	10100000:                  "Headquarters",
	10107000:                  "Wall",
	10119000:                  "Worker's Hut",
	BuildingGoldMine:          "Gold Mine",
	BuildingFarmland:          "Farmland",
	BuildingIronMine:          "Iron Mine",
	BuildingSmelter:           "Smelter",
	BuildingMaterialWorkshop:  "Material Workshop",
	BuildingTrainingBase:      "Training Base",
	BuildingOilWell:           "Oil Well",
	BuildingDronePartsShop:    "Drone Parts Workshop",
	BuildingTacticalCenter:    "Tactical Center",
	BuildingArmamentInst:      "Armament Institute",
	BuildingSporeFactory1:     "Spore Factory I",
	BuildingSporeFactory2:     "Spore Factory II",
	BuildingSporeFactory3:     "Spore Factory III",
	BuildingSporeFactory4:     "Spore Factory IV",
	BuildingTruckStation1:     "Truck Station I (not Armed Truck)",
	BuildingTruckStation2:     "Truck Station II (not Armed Truck)",
	BuildingTruckStation3:     "Truck Station III (not Armed Truck)",
	BuildingTruckStation4:     "Truck Station IV (not Armed Truck)",
	BuildingComponentFactory:  "Component Factory",
	BuildingTacticalInstitute: "Tactical Institute",
	10208000:                  "Gold Warehouse",
	10203000:                  "Food Warehouse (Bakery)",
	10206000:                  "Iron Warehouse (Steel Mill)",
	10220000:                  "Talent Hall",
	10120000:                  "Tavern",
	10103000:                  "Barracks (Military Camp)",
	10104000:                  "Drill Ground (Army Yard)",
	10124000:                  "Hospital",
	10106000:                  "Alliance Center",
	10114000:                  "Radar",
}

// BuildingNameOf returns a friendly name for a known base type id, or the
// raw id as a string if unknown.
func BuildingNameOf(bId int32) string {
	if n, ok := buildingNames[bId]; ok {
		return n
	}
	return fmt.Sprintf("(unknown type %d)", bId)
}

// Building wraps one buildInfo SFSObject with typed accessors for the
// fields documented in the dossier's City/Building domain report.
type Building struct {
	Raw *SFSObject
}

func (b Building) Uuid() int64    { return b.Raw.GetLong("uuid") }
func (b Building) BId() int32     { return b.Raw.GetInt("bId") }
func (b Building) Level() int32   { return b.Raw.GetInt("lv") }
func (b Building) PointId() int32 { return b.Raw.GetInt("pId") }

// requirePresentField reports whether o has field, logging a Warn with the raw entry (for
// diagnosability) and returning false if it's missing -- shared by every list-parsing code path
// (buildings, mail, visitors) that must tolerate a malformed/unexpected entry from the server
// without crashing or silently fabricating a zero-value id. An explicit sfsNull for field is
// treated the same as a missing field: Has() only reflects key presence, so a null-typed entry
// would otherwise slip past the guard and GetInt/GetLong/GetString would fall through to a
// zero value indistinguishable from a genuine one.
func requirePresentField(o *SFSObject, field, context string) bool {
	v, ok := o.Get(field)
	if !ok || v.Val == nil {
		slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())
		return false
	}
	return true
}

// ParseInitBuildings extracts the owned-building list from the bare `init`
// bootstrap push's `building_new` field (BuildManager:InitData(t) in the
// decompiled Lua) -- the real source of building data, as opposed to the
// separate, rarely-fired push.init.build/defaultBuilds.
func ParseInitBuildings(initParams *SFSObject) []Building {
	var out []Building
	v, ok := initParams.Get("building_new")
	if !ok {
		return out
	}
	arr, ok := v.Val.(*SFSArray)
	if !ok {
		return out
	}
	for _, item := range arr.items {
		bi, ok := item.Val.(*SFSObject)
		if !ok {
			continue
		}
		if !requirePresentField(bi, "uuid", "building_new") {
			continue
		}
		out = append(out, Building{Raw: bi})
	}
	return out
}

// FetchBuildings waits for push.init.build (the full base snapshot sent
// once after entering the city scene) and returns every owned building.
// It also opportunistically captures a short window of any push.queue.add/
// push.build.queue.info traffic that arrives in the same window and logs
// it -- production-queue items are separate entities from buildings
// (dossier §City/Building "two queue systems"), and seeing real examples
// is the fastest way to confirm which of Farmland/Iron Mine/Gold Mine/
// Training Base collect via a queue-item uuid vs a direct building-uuid
// action.
func FetchBuildings(conn *GameConn, timeout time.Duration) ([]Building, []Visitor, error) {
	var buildings []Building
	var visitors []Visitor
	deadline := time.Now().Add(timeout)
	gotInitBuild := false

	// seenBuildingUUIDs dedupes across the three population sources below (init/building_new,
	// push.init.build/defaultBuilds, push.add.building/buildings): if more than one fires for the
	// same uuid within one fetch window -- e.g. the bootstrap init push and a redundant
	// push.init.build both describing the same building -- appendBuilding keeps only the first
	// sighting, so callers like CollectAll never see (and redundantly collect) the same uuid twice.
	seenBuildingUUIDs := make(map[int64]bool)
	appendBuilding := func(b Building) {
		uuid := b.Uuid()
		if seenBuildingUUIDs[uuid] {
			return
		}
		seenBuildingUUIDs[uuid] = true
		buildings = append(buildings, b)
	}

	// seenVisitorUUIDs mirrors seenBuildingUUIDs above and exists for the identical reason: the
	// bare `init` bootstrap push is the sole source visitors are populated from (ParseInitVisitors,
	// via msg.Params' visitor.list field), but a redundant resend of that same init push within one
	// fetch window would otherwise double-append every visitor it carries, just like the
	// building_new/defaultBuilds resend case above does for buildings. GreetVisitors (visitors.go)
	// issues one real visitor.operate network call per slice entry with no dedup of its own, so a
	// doubled visitor list here means a doubled real network call per uid -- and per conn.go's
	// benignErrorCodes only covering visitor_err_coming for that call family, the second (redundant)
	// call risks a genuine, non-benign failure rather than a harmless no-op.
	seenVisitorUUIDs := make(map[int64]bool)
	appendVisitor := func(v Visitor) {
		uid := v.Uid()
		if seenVisitorUUIDs[uid] {
			return
		}
		seenVisitorUUIDs[uid] = true
		visitors = append(visitors, v)
	}

	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		conn.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := conn.ReadEnvelope()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break // expected: waited long enough for this window, move on with what we have
			}
			return buildings, visitors, fmt.Errorf("read building list: %w", err)
		}
		msg, ok := env.AsExtension()
		if !ok {
			continue
		}
		switch msg.Cmd {
		case "init":
			// The REAL post-login bootstrap (report 14 §5 / dossier §04):
			// BuildManager:InitData(t) reads buildings from t.building_new,
			// not push.init.build's defaultBuilds. This is the field that
			// actually matters; push.init.build is a rarely-fired secondary
			// push.
			gotInitBuild = true
			for _, b := range ParseInitBuildings(msg.Params) {
				appendBuilding(b)
			}
			for _, v := range ParseInitVisitors(msg.Params) {
				appendVisitor(v)
			}
			slog.Info("init: buildings loaded", "field", "building_new", "count", len(buildings))
			slog.Info("init: visitors loaded", "field", "visitor.list", "count", len(visitors))
			deadline = time.Now().Add(3 * time.Second)
		case "push.init.build":
			gotInitBuild = true
			if v, ok := msg.Params.Get("defaultBuilds"); ok {
				if arr, ok := v.Val.(*SFSArray); ok {
					for _, item := range arr.items {
						wrapper, ok := item.Val.(*SFSObject)
						if !ok {
							continue
						}
						if biv, ok := wrapper.Get("buildInfo"); ok {
							if bi, ok := biv.Val.(*SFSObject); ok {
								if !requirePresentField(bi, "uuid", "push.init.build") {
									continue
								}
								appendBuilding(Building{Raw: bi})
							}
						}
					}
				}
			}
			slog.Info("push.init.build: buildings loaded", "count", len(buildings))
			// Keep listening a little longer for queue pushes that often
			// follow immediately, but we already have what we came for.
			deadline = time.Now().Add(3 * time.Second)
		case "push.add.building":
			if v, ok := msg.Params.Get("buildings"); ok {
				if arr, ok := v.Val.(*SFSArray); ok {
					for _, item := range arr.items {
						bi, ok := item.Val.(*SFSObject)
						if !ok {
							continue
						}
						if !requirePresentField(bi, "uuid", "push.add.building") {
							continue
						}
						appendBuilding(Building{Raw: bi})
					}
				}
			}
		case "push.queue.add":
			// StringRedacted, not String(): this switch's default branch (and, in principle, any
			// case here) sees whatever cmd the server sends while FetchBuildings is listening, and
			// nothing in this function's control flow enforces that a credential-bearing push (e.g.
			// push.account.login.new, or an init push's chatToken) can't land here. No currently
			// reachable path does so today, but that rests on call-ordering elsewhere in the
			// client, not on anything this switch itself checks -- redact defensively rather than
			// rely on it.
			slog.Info("observed push.queue.add", "params", msg.Params.StringRedacted())
		case "push.build.queue.info":
			slog.Info("observed push.build.queue.info", "params", msg.Params.StringRedacted())
		default:
			slog.Info("observed other push", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
		}
	}

	if !gotInitBuild {
		slog.Warn("never saw push.init.build within timeout; building list may be incomplete")
	}
	return buildings, visitors, nil
}

// PrintBuildings prints every building to stdout, calling out full raw field
// dumps for our 8 requested target types (recognized ones by name,
// unrecognized ones so we can eyeball the data and pin down Smelter/Material
// Workshop/etc.). This is the actual -list-buildings result data, so it goes
// to stdout (not slog/stderr) to keep it capturable via shell redirection,
// per the stdout=data/stderr=logs convention -version and -decode-stream
// also follow.
func PrintBuildings(buildings []Building) {
	byType := map[int32][]Building{}
	for _, b := range buildings {
		byType[b.BId()] = append(byType[b.BId()], b)
	}
	fmt.Printf("building summary: distinctTypes=%d totalInstances=%d\n", len(byType), len(buildings))
	for bId, list := range byType {
		fmt.Printf("building type: bId=%d name=%s instances=%d\n", bId, BuildingNameOf(bId), len(list))
		for _, b := range list {
			fmt.Printf("building instance: uuid=%d buildingLevel=%d pointId=%d raw=%s\n", b.Uuid(), b.Level(), b.PointId(), b.Raw.StringRedacted())
		}
	}
}

// collectCmdFor returns the SFS extension command used to collect a given
// building type's accumulated output -- a single `uuid` (long) field to
// `building.production.collect`
// (extracted/lua_decompiled/2067_DataCenter_ProductLine_ProductLineManager.lua's
// SendCollect), never the separate `building.camp.collect` family (that one
// is real, but covers only Military Camp/Smith Shop/Tactical Chip Factory
// -- confirmed live it returns errorCode=E000001 "Building type error" for
// every type in this switch).
//
// Thirteen of these types are CONFIRMED with real status=1 collections on
// the real account: Farmland, Iron Mine, Gold Mine, Smelter, Material
// Workshop, Training Base, Oil Well, Drone Parts Workshop, Component
// Factory, and the four Spore Factory tiers.
//
// Tactical Center, Armament Institute, Tactical Institute, and the four
// Truck Station tiers are NOT confirmed -- see the long comment on the
// const block above for why they're included anyway (never E000001, so
// the pairing looks real, but no successful collection yet -- Tactical
// Institute has real production timestamps, unlike the other five, so
// it's the best-positioned of this group to flip to confirmed soon).
func collectCmdFor(bId int32) (cmd string, ok bool) {
	switch bId {
	case BuildingFarmland, BuildingIronMine, BuildingGoldMine, BuildingSmelter, BuildingMaterialWorkshop, BuildingTrainingBase,
		BuildingOilWell, BuildingDronePartsShop, BuildingTacticalCenter, BuildingArmamentInst,
		BuildingSporeFactory1, BuildingSporeFactory2, BuildingSporeFactory3, BuildingSporeFactory4,
		BuildingTruckStation1, BuildingTruckStation2, BuildingTruckStation3, BuildingTruckStation4,
		BuildingComponentFactory, BuildingTacticalInstitute:
		return "building.production.collect", true
	default:
		return "", false
	}
}

// CollectIdleReward collects "Armed Truck" -- confirmed live via a real
// packet capture of the actual game client (not a guess): it is NOT a
// building at all, despite LW_BUILD_TRUCK_STATION_1-4 existing and
// accepting building.production.collect (that was a real but wrong lead --
// zero collections ever succeeded on those, see the const block comment
// above). The real UI is a dedicated "Truck Rewards" panel bundling two
// independent idle/AFK reward tracks -- "Armed Truck" and "Overlord" --
// both driven by one account-level (not building-uuid-scoped) command,
// `lw.pve.idle.reward`, with an `action` field: 0 = peek at what's
// accumulated without claiming it, 1 = claim it. The real client's own
// capture called action=0, then action=1, then action=0 again (an
// immediate refresh showing the pool reset to empty) -- mirrored here.
// Confirmed live: the action=1 response includes real post-collection
// account `total` values for each resource, and a follow-up action=0
// call showed `reward=[]`, proving the accumulated pool was actually
// drained, not just echoed back.
func CollectIdleReward(conn *GameConn) error {
	const cmd = "lw.pve.idle.reward"
	peek := NewSFSObject()
	peek.PutInt("action", 0)
	if _, err := sendAndWait(conn, "idle reward available", cmd, peek); err != nil {
		return err
	}

	claim := NewSFSObject()
	claim.PutInt("action", 1)
	if _, err := sendAndWait(conn, "idle reward collected", cmd, claim); err != nil {
		return err
	}
	return nil
}

// CollectAll finds every instance of every confirmed resource-producing
// building type -- see collectCmdFor's switch for the full list -- and
// collects their accumulated output, plus the account-level "Armed Truck"
// idle reward (see CollectIdleReward), any present visitors (see
// GreetVisitors), pending alliance member help requests (see
// HelpAllianceMembers), unclaimed mail (see ClaimAllMail), unclaimed
// alliance gifts (see ClaimAllianceGifts), a free donation toward whichever
// alliance tech is currently recommended (see DonateRecommendedAllianceTech),
// and the two once-per-day VIP claims (see ClaimVIPDailyLoginScore and
// ClaimVIPDailyFreebie) -- none of the eight is building-uuid-scoped, so
// none can go through the same per-building loop below.
func CollectAll(conn *GameConn, buildings []Building, visitors []Visitor) error {
	var errs []error

	// The 8 fixed sub-actions plus one closure per collectible building below are each
	// independent (none scoped to any other's outcome), so an ordinary decoded business-logic
	// errorCode failure in one must not stop the rest from running -- every error, regardless of
	// kind, still gets appended to errs rather than returned immediately, same as before this
	// loop existed. A net.Error is a different kind of failure though: it means the underlying
	// TCP connection itself is known-dead (e.g. silently blackholed, no RST), so every subsequent
	// action in this list is already doomed to independently burn a full defaultCmdTimeout before
	// failing the exact same way. FetchBuildings (above) already distinguishes this class of
	// failure via errors.As against net.Error; the loop below mirrors that exact check and breaks
	// early the first time it fires, instead of pointlessly waiting out every remaining action's
	// timeout in turn. The error that triggered the break is still appended to errs first, so the
	// caller's aggregated error still reports what actually happened.
	actions := []func() error{
		func() error { return CollectIdleReward(conn) },
		func() error { return GreetVisitors(conn, visitors) },
		func() error { return ClaimAllMail(conn) },
		func() error { return HelpAllianceMembers(conn) },
		func() error { return ClaimAllianceGifts(conn) },
		func() error { return DonateRecommendedAllianceTech(conn) },
		func() error { return ClaimVIPDailyLoginScore(conn) },
		func() error { return ClaimVIPDailyFreebie(conn) },
	}

	toCollect := collectibleBuildings(buildings)
	if len(toCollect) == 0 {
		slog.Info("no matching collectible buildings found on this account")
	}
	for _, b := range toCollect {
		actions = append(actions, func() error {
			cmd, _ := collectCmdFor(b.BId())
			slog.Info("attempting collect", "name", BuildingNameOf(b.BId()), "uuid", b.Uuid(), "buildingLevel", b.Level(), "cmd", cmd)
			params := NewSFSObject()
			params.PutLong("uuid", b.Uuid())
			_, err := sendAndWait(conn, "collect "+BuildingNameOf(b.BId()), cmd, params)
			return err
		})
	}

	for _, action := range actions {
		err := action()
		errs = append(errs, err)
		var netErr net.Error
		if errors.As(err, &netErr) {
			break
		}
	}
	return errors.Join(errs...)
}

// collectibleBuildings filters buildings down to the ones collectCmdFor recognizes -- pulled out
// of CollectAll as a standalone, network-free function so it can be unit tested without a live
// connection.
func collectibleBuildings(buildings []Building) []Building {
	var out []Building
	for _, b := range buildings {
		if _, ok := collectCmdFor(b.BId()); ok {
			out = append(out, b)
		}
	}
	return out
}
