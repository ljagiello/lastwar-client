package game

import (
	"errors"
	"fmt"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"sort"
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
// The original 6 requested buildings (below) plus 7 more CONFIRMED live via
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

// Building wraps one buildInfo sfs.SFSObject with typed accessors for the
// fields documented in the dossier's City/Building domain report.
type Building struct {
	Raw *sfs.SFSObject
}

func (b Building) Uuid() int64    { return b.Raw.GetLong("uuid") }
func (b Building) BId() int32     { return b.Raw.GetInt("bId") }
func (b Building) Level() int32   { return b.Raw.GetInt("lv") }
func (b Building) PointId() int32 { return b.Raw.GetInt("pId") }

// maxRawBuildingItemsPerPush is the defensive, non-protocol-guessing ceiling on how many raw items
// ParseInitBuildings (below) and FetchBuildings' two sibling inline loops -- the "push.init.build"
// case's defaultBuilds array and the "push.add.building" case's buildings array -- will examine from
// one server-supplied array in one push, regardless of how many of those items turn out to be valid,
// distinct buildings. Unlike visitors.go's ParseInitVisitors, there is no server-supplied "expected
// building count" hint field anywhere in this codebase's protocol notes to clamp against
// (maxCollectibleBuildingsPerRun's own doc comment below already makes this exact argument for why
// buildings needs a purely defensive cap rather than trusting a server-sent hint) -- so this is a
// single hardcoded ceiling applied identically to all three loops, not a clamp on a server-sent value.
//
// Before this cap existed, all three loops scanned their raw arrays with NO bound of any kind -- not
// even an output-count bound, let alone round 26's raw-item-scan bound (visitors.go's
// ParseInitVisitors) -- so the only ceiling on record-processing cost was sfsobject.go's
// sfs.MaxDecodedNodes=300,000 decode-level budget. ParseInitBuildings in particular feeds the PRIMARY
// init-push path (login.go's waitForInitPush, called from Login() on every login, not just
// FetchBuildings' fallback), so a hostile/misbehaving -cs-ip peer could pad building_new with up to
// ~300,000 minimal malformed entries (each triggering a Warn log via requireFieldType below) and
// force a full scan-and-log cost on every single login.
//
// Set well above maxCollectibleBuildingsPerRun=300 (below): that constant bounds the POST-parse,
// POST-filter collect loop over already-parsed, already-deduped, already-recognized-type buildings,
// while this one bounds the PARSE-time scan of raw items -- most of which (malformed entries,
// unrecognized building types, duplicate uuids across the three sources) never make it into
// collectibleBuildings' output at all. So this can reasonably be several times larger without
// weakening maxCollectibleBuildingsPerRun's own protection, while still finite enough to keep each
// loop's worst-case scan-and-log cost bounded rather than open-ended.
const maxRawBuildingItemsPerPush = 2000

// ParseInitBuildings extracts the owned-building list from the bare `init`
// bootstrap push's `building_new` field (BuildManager:InitData(t) in the
// decompiled Lua) -- the real source of building data, as opposed to the
// separate, rarely-fired push.init.build/defaultBuilds.
//
// The scan of `building_new` is capped at maxRawBuildingItemsPerPush RAW items examined, not merely
// valid ones appended to the returned slice -- mirroring ParseInitVisitors' round-26 fix (visitors.go):
// a malformed entry's `continue` below must count against the cap just as much as a valid entry's
// append does, since otherwise a server-supplied array padded entirely with malformed entries would
// defeat the cap entirely (len(out) would never advance, so an output-count-only cap would never
// trigger). See maxRawBuildingItemsPerPush's own doc comment for the full threat model.
func ParseInitBuildings(initParams *sfs.SFSObject) []Building {
	var out []Building
	v, ok := initParams.Get("building_new")
	if !ok {
		return out
	}
	arr, ok := v.Val.(*sfs.SFSArray)
	if !ok {
		// Round-39 fix: present-but-wrong-typed used to be silently indistinguishable from
		// genuinely-absent, unlike alliance.go's DonateRecommendedAllianceTech's identical-shape
		// guard on allianceScience, which already warns on this exact anomaly. Diagnostic only --
		// this function still fails safe to an empty result either way -- but a hostile/malformed
		// -cs-ip peer sending building_new as the wrong type is a decode-desync signal worth
		// surfacing, not silently discarding.
		slog.Warn("ParseInitBuildings: building_new field is present but not an array", "type", fmt.Sprintf("%T", v.Val))
		return out
	}
	if len(arr.Items()) > maxRawBuildingItemsPerPush {
		slog.Warn("building_new longer than raw-item scan cap; truncating",
			"itemCount", len(arr.Items()), "cap", maxRawBuildingItemsPerPush)
	}
	for i, item := range arr.Items() {
		if i >= maxRawBuildingItemsPerPush {
			break
		}
		bi, ok := item.Val.(*sfs.SFSObject)
		if !ok {
			continue
		}
		if !session.RequireFieldType(bi, "uuid", "building_new", session.SFSFieldKindLong) {
			continue
		}
		// bId is guarded purely for consistency/diagnosability with the uuid guard immediately
		// above (round 29 audit): BId() (this file) reads it via the same GetInt zero-value-
		// coercing accessor, but unlike uuid, nothing here previously logged a Warn on a
		// wrong-typed bId -- it would just silently coerce to bId=0, which collectCmdFor's switch
		// (below) simply never matches, so the building is safely dropped from CollectAll's
		// collect loop either way. Not a correctness fix, just a diagnostic one: see
		// TestParseInitBuildingsWrongTypedBIdIsRejected.
		if !session.RequireFieldType(bi, "bId", "building_new", session.SFSFieldKindInt) {
			continue
		}
		out = append(out, Building{Raw: bi})
	}
	return out
}

// capDeadline returns the earlier of a per-push extension candidate and the caller's original
// outer deadline -- so a burst of qualifying bootstrap pushes (see the "init"/"push.init.build"
// cases below) can still extend FetchBuildings' wait somewhat, for a slow-but-legitimate
// multi-push bootstrap burst, but can never push the total wait past the timeout the caller
// originally asked for.
func capDeadline(candidate, original time.Time) time.Time {
	if candidate.After(original) {
		return original
	}
	return candidate
}

// checkNonMatchingEnvelopeCap increments *nonMatchingEnvelopes and, once it exceeds
// maxNonMatchingEnvelopesPerWait (login.go), returns a benign give-up error (deadlineExceededError
// -- not sfs.DeadConnError, since a stream of well-formed-but-irrelevant traffic isn't itself evidence
// of a dead connection). Shared by FetchBuildings' four "this envelope didn't advance real state"
// sites (non-extension, push.queue.add, push.build.queue.info, default/unrecognized cmd) --
// login.go's waitFor/waitForInitPush and conn.go's DoHandshake each only have one such site, so
// they inline the identical check rather than needing this helper.
func checkNonMatchingEnvelopeCap(nonMatchingEnvelopes *int) error {
	*nonMatchingEnvelopes++
	if *nonMatchingEnvelopes > session.MaxNonMatchingEnvelopesPerWait {
		slog.Warn("read building list: too many well-formed but non-matching envelopes processed, giving up", "nonMatchingEnvelopes", *nonMatchingEnvelopes)
		return session.DeadlineExceededError{}
	}
	return nil
}

// FetchBuildings waits for the bare init push's building_new field (the
// real post-login bootstrap source -- see the case "init" branch below and
// ParseInitBuildings' doc comment; push.init.build is a rarely-fired
// secondary push, not the primary source its old name here might suggest)
// and returns every owned building. It also opportunistically captures a
// short window of any push.queue.add/
// push.build.queue.info traffic that arrives in the same window and logs
// it -- production-queue items are separate entities from buildings
// (dossier §City/Building "two queue systems"), and seeing real examples
// is the fastest way to confirm which of Farmland/Iron Mine/Gold Mine/
// Training Base collect via a queue-item uuid vs a direct building-uuid
// action.
func FetchBuildings(conn *session.GameConn, timeout time.Duration) ([]Building, []Visitor, error) {
	var buildings []Building
	var visitors []Visitor
	// originalDeadline is the caller's actual budget (main.go passes 12s/15s at its two call
	// sites) and, unlike deadline below, is NEVER reassigned once set -- it exists solely so the
	// per-push 3-second extension below (see the "init"/"push.init.build" cases) has something
	// fixed to cap itself against. Before this fix, that extension reassigned deadline directly
	// on every qualifying push with no cap at all, so a peer that kept re-sending either push type
	// faster than the 3s window could hold this call open indefinitely -- a materially worse
	// outcome than a bounded timeout for the cron-wrapper usage main.go's own comments describe as
	// a first-class use case. This client supports connecting to arbitrary hosts via -cs-ip, which
	// this project's threat model already treats as untrusted/hostile-capable (see round 16's
	// sfs.RedactSFSValue fail-open fix for the same threat model applied elsewhere), so a hostile peer
	// deliberately doing this is a real scenario, not just a theoretical one.
	originalDeadline := time.Now().Add(timeout)
	deadline := originalDeadline
	gotInitBuild := false
	// gotAuthoritativeInit tracks specifically whether the "init" case below (the real
	// building_new/visitor.list source -- see ParseInitBuildings' doc comment) has fired this
	// session, as opposed to gotInitBuild above which is set by either "init" or
	// "push.init.build" and so can't answer that question on its own. Gates the
	// "push.init.build" case's deadline-shrink below: per this file's own doc comments,
	// push.init.build/defaultBuilds is a rarely-fired secondary push carrying inferior data, so
	// it must never be allowed to cut short the wait for the authoritative "init" push that
	// hasn't arrived yet -- only once "init" has already been captured is push.init.build just
	// icing worth a short trailing window for, not the only thing seen so far.
	gotAuthoritativeInit := false

	// seenBuildingUUIDs dedupes across the three population sources below (init/building_new,
	// push.init.build/defaultBuilds, push.add.building/buildings): if more than one fires for the
	// same uuid within one fetch window -- e.g. the bootstrap init push and a redundant
	// push.init.build both describing the same building -- appendBuilding keeps only the first
	// sighting, so callers like CollectAll never see (and redundantly collect) the same uuid twice.
	//
	// Round-39 fix: appendBuilding itself now also enforces MaxAggregateBuildingsPerFetch, not
	// just the loop-top check further below. That loop-top check only re-fires once per outer
	// iteration (i.e. once per push), but every one of the three population sources here is
	// bounded PER PUSH only by the unrelated, much larger maxRawBuildingItemsPerPush (2000) --
	// so a single hostile push carrying up to 2000 distinct-uuid entries used to be able to call
	// appendBuilding up to 2000 times, 6.67x past the documented 300-entry aggregate ceiling,
	// entirely within one outer-loop iteration before the loop-top check was ever re-consulted.
	// Checking the cap here instead means it's enforced at the one place ALL three sources funnel
	// through, regardless of how many raw items a single push claims to carry.
	seenBuildingUUIDs := make(map[int64]bool)
	appendBuilding := func(b Building) {
		if len(buildings) >= MaxAggregateBuildingsPerFetch {
			return
		}
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
	//
	// Round-40 fix: appendVisitor itself now also enforces MaxVisitorsUpperBound, mirroring
	// appendBuilding's own round-39 fix above -- this was appendBuilding's one remaining
	// structural sibling still missing the per-append guard. The loop-top check further below only
	// re-fires once per outer iteration (once per push), but a single `init` push can carry up to
	// MaxVisitorsUpperBound (300) visitors on its own (ParseInitVisitors' own per-push clamp), so
	// two near-cap consecutive pushes within one fetch window could inflate visitors to ~2x the
	// documented ceiling before the loop-top check ever re-fired -- the same single-push/
	// multi-push overshoot shape round 39 closed for buildings, left open on its own named twin.
	seenVisitorUUIDs := make(map[int64]bool)
	appendVisitor := func(v Visitor) {
		if len(visitors) >= MaxVisitorsUpperBound {
			return
		}
		uid := v.Uid()
		if seenVisitorUUIDs[uid] {
			return
		}
		seenVisitorUUIDs[uid] = true
		visitors = append(visitors, v)
	}

	consecutiveDecodeFailures := 0
	nonMatchingEnvelopes := 0
	for {
		// Round-36 fix: ParseInitVisitors/ParseInitBuildings and the two inline defaultBuilds/
		// buildings loops above are each capped PER PUSH (MaxVisitorsUpperBound=300,
		// maxRawBuildingItemsPerPush=2000), but nothing previously bounded how many qualifying
		// pushes this loop accumulates across the whole wait window -- appendBuilding/appendVisitor
		// dedupe only on uuid/uid, fields fully controlled by the peer, so a hostile -cs-ip peer
		// sending many small pushes with distinct fake uids could inflate buildings/visitors far
		// past any single push's own cap. For visitors specifically this reopens the exact hang
		// GreetVisitors' own doc comment claims is bounded: a peer that stops answering
		// visitor.operate (while still answering everything else, so the connection never reads as
		// dead) forces every accumulated entry to independently burn up to defaultCmdTimeout before
		// GreetVisitors moves on. Checked once per loop iteration (before waiting for the next
		// envelope) so it naturally stops READING further pushes too, not just appending past the
		// cap -- an attacker gains nothing by sending more once this fires.
		//
		// Round-37 fix: the building-side check below originally reused maxCollectibleBuildingsPerRun
		// (a POST-filter cap scoped to only the ~19 collectible building types CollectAll knows how
		// to collect from) to bound this PRE-filter, all-types accumulator -- silently truncating
		// -list-buildings output and CollectAll's building pool for any account whose total building
		// count (every type, not just collectible ones) exceeded 300. Now uses its own dedicated
		// MaxAggregateBuildingsPerFetch constant (see its own doc comment for the full distinction).
		if len(visitors) >= MaxVisitorsUpperBound {
			slog.Warn("aggregate visitor count across this fetch window reached the upper bound; stopping early",
				"visitorCount", len(visitors), "cap", MaxVisitorsUpperBound)
			break
		}
		if len(buildings) >= MaxAggregateBuildingsPerFetch {
			slog.Warn("aggregate building count across this fetch window reached the upper bound; stopping early",
				"buildingCount", len(buildings), "cap", MaxAggregateBuildingsPerFetch)
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		_ = conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := conn.ReadEnvelope()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				break // expected: waited long enough for this window, move on with what we have
			}
			if session.ContainsNonTimeoutNetError(err) {
				return buildings, visitors, fmt.Errorf("read building list: %w", err)
			}
			// Round-49 fix: a plain, non-net.Error ReadEnvelope failure (e.g. a sfs.DecodeObject
			// parse failure on one malformed/unrelated push) means sfs.ReadPacket already fully
			// consumed that frame's bytes off the wire before sfs.DecodeObject ever ran -- the
			// stream stays in sync, so this is not evidence the connection is dead, mirroring
			// login.go's identical round-48/49 fixes (waitForInitPush, waitFor). Previously
			// this loop returned immediately on ANY such error, silently truncating the
			// buildings/visitors accumulation for this whole fetch window instead of simply
			// skipping the one malformed push and continuing to read.
			consecutiveDecodeFailures++
			if consecutiveDecodeFailures > session.MaxConsecutiveDecodeFailures {
				// sfs.DeadConnError (packet.go): round-53 fix for the MAJOR finding that this
				// give-up branch was the one sibling among the four independent
				// maxConsecutiveDecodeFailures loops (login.go's waitFor/waitForInitPush,
				// conn.go's DoHandshake -- all fixed in round 51) that never wrapped its
				// give-up outcome in sfs.DeadConnError, returning a silent nil error instead. That
				// used to be indistinguishable from the genuinely benign timeout-break case two
				// checks above (a connection that simply never sent the awaited push), even
				// though 21+ consecutive undecodable frames is a much stronger, actively-hostile
				// signal -- exactly the shape round 51 already established as fatal for the
				// other three loops. Without this, callers (main.go's
				// shouldAbortBeforeInteractive, both runMain and runCrossServerTest call sites)
				// never learned the connection had just proven itself dead, so -interactive
				// could launch directly against it, or -collect could burn a full battery of
				// doomed requests against it, instead of aborting immediately.
				err := sfs.NewDeadConnError(fmt.Errorf("read building list: %d consecutive malformed/undecodable envelopes, giving up with whatever was already collected: %w", consecutiveDecodeFailures, err))
				slog.Warn("read building list: too many consecutive malformed/undecodable envelopes, giving up early with whatever was already collected",
					"consecutiveDecodeFailures", consecutiveDecodeFailures)
				return buildings, visitors, err
			}
			slog.Warn("read building list: failed to read/decode an envelope; continuing to wait, not treating this as a dead connection", "error", err, "consecutiveDecodeFailures", consecutiveDecodeFailures)
			continue
		}
		consecutiveDecodeFailures = 0
		msg, ok := env.AsExtension()
		if !ok {
			// maxNonMatchingEnvelopesPerWait (login.go doc comment): a non-extension envelope
			// (e.g. the client's own background heartbeat pong) costs a full
			// sfs.ReadPacket/sfs.DecodeObject/AsExtension cycle just like a well-formed-but-irrelevant
			// push does below, so it counts against the identical cap.
			if err := checkNonMatchingEnvelopeCap(&nonMatchingEnvelopes); err != nil {
				return buildings, visitors, err
			}
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
			gotAuthoritativeInit = true
			for _, b := range ParseInitBuildings(msg.Params) {
				appendBuilding(b)
			}
			for _, v := range ParseInitVisitors(msg.Params) {
				appendVisitor(v)
			}
			slog.Info("init: buildings loaded", "field", "building_new", "count", len(buildings))
			slog.Info("init: visitors loaded", "field", "visitor.list", "count", len(visitors))
			deadline = capDeadline(time.Now().Add(3*time.Second), originalDeadline)
		case "push.init.build":
			gotInitBuild = true
			if v, ok := msg.Params.Get("defaultBuilds"); ok {
				if arr, ok := v.Val.(*sfs.SFSArray); ok {
					// Raw-item-scan cap: see maxRawBuildingItemsPerPush's doc comment above
					// ParseInitBuildings for why this loop needs the identical defensive ceiling
					// ParseInitBuildings itself applies to building_new -- this is one of the two
					// sibling inline loops that constant's doc comment refers to.
					if len(arr.Items()) > maxRawBuildingItemsPerPush {
						slog.Warn("push.init.build defaultBuilds longer than raw-item scan cap; truncating",
							"itemCount", len(arr.Items()), "cap", maxRawBuildingItemsPerPush)
					}
					for i, item := range arr.Items() {
						if i >= maxRawBuildingItemsPerPush {
							break
						}
						wrapper, ok := item.Val.(*sfs.SFSObject)
						if !ok {
							continue
						}
						if biv, ok := wrapper.Get("buildInfo"); ok {
							if bi, ok := biv.Val.(*sfs.SFSObject); ok {
								if !session.RequireFieldType(bi, "uuid", "push.init.build", session.SFSFieldKindLong) {
									continue
								}
								// bId guard: see the identical guard's doc comment in
								// ParseInitBuildings above -- consistency/diagnosability only.
								if !session.RequireFieldType(bi, "bId", "push.init.build", session.SFSFieldKindInt) {
									continue
								}
								appendBuilding(Building{Raw: bi})
							} else {
								// Round-40 fix: this nested buildInfo field used to silently drop a
								// present-but-wrong-typed entry with zero diagnostic, unlike the
								// enclosing defaultBuilds array-type check three lines above (round-39
								// fix) and the uuid/bId guards two lines below it -- the identical
								// present-but-wrong-typed-vs-absent distinction this codebase applies
								// everywhere else. Diagnostic only -- this entry already fails safe to
								// "not appended" either way.
								slog.Warn("push.init.build: buildInfo field is present but not an object", "type", fmt.Sprintf("%T", biv.Val))
							}
						}
					}
				} else {
					// Round-39 fix: see ParseInitBuildings' identical-shape guard on building_new
					// for the full rationale -- diagnostic only, this case already fails safe to
					// "no buildings from this push" either way.
					slog.Warn("push.init.build: defaultBuilds field is present but not an array", "type", fmt.Sprintf("%T", v.Val))
				}
			}
			slog.Info("push.init.build: buildings loaded", "count", len(buildings))
			// Only shrink the deadline here if the authoritative "init" push (building_new --
			// see this function's doc comment and the "init" case above) has ALREADY arrived
			// this session. If it hasn't, push.init.build's defaultBuilds is all we have so
			// far -- inferior data per this file's own doc comments -- so shrinking the
			// deadline now could let a delayed-but-still-within-timeout authoritative init
			// never get read at all, silently settling for push.init.build's data instead.
			// Once init HAS already been seen, push.init.build is just icing (e.g. trailing
			// queue pushes), so the same short trailing window applies as it does after init
			// itself.
			if gotAuthoritativeInit {
				deadline = capDeadline(time.Now().Add(3*time.Second), originalDeadline)
			}
		case "push.add.building":
			if v, ok := msg.Params.Get("buildings"); ok {
				if arr, ok := v.Val.(*sfs.SFSArray); ok {
					// Raw-item-scan cap: see maxRawBuildingItemsPerPush's doc comment above
					// ParseInitBuildings -- this is the other of the two sibling inline loops that
					// constant's doc comment refers to.
					if len(arr.Items()) > maxRawBuildingItemsPerPush {
						slog.Warn("push.add.building buildings longer than raw-item scan cap; truncating",
							"itemCount", len(arr.Items()), "cap", maxRawBuildingItemsPerPush)
					}
					for i, item := range arr.Items() {
						if i >= maxRawBuildingItemsPerPush {
							break
						}
						bi, ok := item.Val.(*sfs.SFSObject)
						if !ok {
							continue
						}
						if !session.RequireFieldType(bi, "uuid", "push.add.building", session.SFSFieldKindLong) {
							continue
						}
						// bId guard: see the identical guard's doc comment in ParseInitBuildings
						// above -- consistency/diagnosability only.
						if !session.RequireFieldType(bi, "bId", "push.add.building", session.SFSFieldKindInt) {
							continue
						}
						appendBuilding(Building{Raw: bi})
					}
				} else {
					// Round-39 fix: see ParseInitBuildings' identical-shape guard on building_new
					// for the full rationale -- diagnostic only, this case already fails safe to
					// "no buildings from this push" either way.
					slog.Warn("push.add.building: buildings field is present but not an array", "type", fmt.Sprintf("%T", v.Val))
				}
			}
		case "push.queue.add":
			// maxNonMatchingEnvelopesPerWait (login.go doc comment): checked BEFORE the
			// StringRedacted() formatting/Info-log below so a peer flooding this loop with
			// irrelevant push.queue.add traffic can't force unbounded formatting cost on the
			// very iteration that finally gives up.
			if err := checkNonMatchingEnvelopeCap(&nonMatchingEnvelopes); err != nil {
				return buildings, visitors, err
			}
			// StringRedacted, not String(): this switch's default branch (and, in principle, any
			// case here) sees whatever cmd the server sends while FetchBuildings is listening, and
			// nothing in this function's control flow enforces that a credential-bearing push (e.g.
			// push.account.login.new, or an init push's chatToken) can't land here. No currently
			// reachable path does so today, but that rests on call-ordering elsewhere in the
			// client, not on anything this switch itself checks -- sfs.Redact defensively rather than
			// rely on it.
			slog.Info("observed push.queue.add", "params", msg.Params.StringRedacted())
		case "push.build.queue.info":
			if err := checkNonMatchingEnvelopeCap(&nonMatchingEnvelopes); err != nil {
				return buildings, visitors, err
			}
			slog.Info("observed push.build.queue.info", "params", msg.Params.StringRedacted())
		default:
			if err := checkNonMatchingEnvelopeCap(&nonMatchingEnvelopes); err != nil {
				return buildings, visitors, err
			}
			slog.Info("observed other push", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
		}
	}

	if !gotInitBuild {
		slog.Warn("never saw push.init.build within timeout; building list may be incomplete")
	}
	return buildings, visitors, nil
}

// PrintBuildings prints every building to stdout, unconditionally including
// a full raw field dump per instance (recognized types by name,
// unrecognized ones so we can eyeball the data and pin down Smelter/Material
// Workshop/etc.). This is the actual -list-buildings result data, so it goes
// to stdout (not slog/stderr) to keep it capturable via shell redirection,
// per the stdout=data/stderr=logs convention -version and -decode-stream
// also follow.
//
// The "building type:" blocks are printed in ascending bId order, not map iteration order: Go
// deliberately randomizes map iteration order run-to-run, so iterating byType directly (as this
// used to) made stdout's block order nondeterministic for identical input -- undermining the
// "capturable via shell redirection" purpose stated above, since a plain `diff` between two runs
// against the same account would show spurious reordering noise alongside (or instead of) any
// real change. Collecting and sorting the keys first makes two runs over identical building data
// byte-for-byte identical.
func PrintBuildings(buildings []Building) {
	byType := map[int32][]Building{}
	for _, b := range buildings {
		byType[b.BId()] = append(byType[b.BId()], b)
	}
	bIds := make([]int32, 0, len(byType))
	for bId := range byType {
		bIds = append(bIds, bId)
	}
	sort.Slice(bIds, func(i, j int) bool { return bIds[i] < bIds[j] })

	fmt.Printf("building summary: distinctTypes=%d totalInstances=%d\n", len(byType), len(buildings))
	for _, bId := range bIds {
		list := byType[bId]
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
func CollectIdleReward(conn *session.GameConn) error {
	const cmd = "lw.pve.idle.reward"
	peek := sfs.NewSFSObject()
	peek.PutInt("action", 0)
	if _, err := session.SendAndWait(conn, "idle reward available", cmd, peek); err != nil {
		return err
	}

	claim := sfs.NewSFSObject()
	claim.PutInt("action", 1)
	if _, err := session.SendAndWait(conn, "idle reward collected", cmd, claim); err != nil {
		return err
	}
	return nil
}

// maxCollectibleBuildingsPerRun is a defensive, non-protocol-guessing sanity ceiling on how many
// buildings CollectAll will issue building.production.collect requests for in one run. Unlike
// visitors.go's GreetVisitors -- whose visitor.list carries a server-sent `maxNum` sibling field
// ParseInitVisitors now enforces directly -- there is no equivalent "expected building count" field
// documented anywhere in this codebase's protocol notes, so this can't be a real protocol limit,
// only defense-in-depth: the same category of deliberately generous, non-protocol-guessing safety
// margin as sfsobject.go's sfs.MaxDecodedNodes/sfs.MaxFrameSize/sfs.MaxNestDepth.
//
// Set generously large -- no real account's building count could plausibly approach it -- while
// still finite enough to bound CollectAll's worst-case sequential runtime against a peer that simply
// never responds to something reasonable: each collect call can cost up to a full defaultCmdTimeout
// (8s, conn.go), so 300 * 8s caps the worst case at ~40 minutes instead of an unbounded hang. This
// closes the same threat model as buildings.go's own capDeadline (round 20) and
// maxVisitorsDefensiveCeiling (visitors.go), just for this loop's item COUNT instead of wait
// DURATION.
const maxCollectibleBuildingsPerRun = 300

// MaxAggregateBuildingsPerFetch is FetchBuildings' own aggregate ceiling on the RAW, unfiltered
// `buildings` accumulator across its whole wait window (round-37 fix) -- deliberately a separate
// constant from maxCollectibleBuildingsPerRun above, not a reuse of it, since the two bound
// different things: maxCollectibleBuildingsPerRun is a POST-filter cap on collectibleBuildings'
// output (only the ~19 resource-producing types CollectAll actually knows how to collect from),
// while this one bounds the PRE-filter accumulator appendBuilding fills from all three population
// sources (init/building_new, push.init.build/defaultBuilds, push.add.building/buildings) --
// Headquarters, Wall, Worker's Hut, warehouses, Hospital, Radar, Barracks, and every other
// building type count toward it identically, not just the collectible subset. Reusing
// maxCollectibleBuildingsPerRun here (as an earlier version of this fix mistakenly did) silently
// truncated -list-buildings output and CollectAll's building pool for any account whose TOTAL
// building count (all types) exceeded 300, even though only ~19 of those types were ever going
// to be collected from anyway -- a real, if generous, false ceiling on legitimate accounts, not
// just a defensive one against hostile peers. Same generous-defensive-margin reasoning as its
// sibling MaxVisitorsUpperBound (visitors.go): no real account's TOTAL building count could
// plausibly approach this, so it only ever engages against a hostile peer inflating the
// accumulator with many small pushes carrying distinct fake uuids (defeating the uuid-keyed
// dedup) -- see the loop-top check's own doc comment below for that threat model.
const MaxAggregateBuildingsPerFetch = 300

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
func CollectAll(conn *session.GameConn, buildings []Building, visitors []Visitor) error {
	var errs []error

	// The 8 fixed sub-actions plus one closure per collectible building below are each
	// independent (none scoped to any other's outcome), so an ordinary decoded business-logic
	// errorCode failure in one must not stop the rest from running -- every error, regardless of
	// kind, still gets appended to errs rather than returned immediately, same as before this
	// loop existed. A net.Error whose Timeout() is true is just as ungated: it's sendAndWait's
	// ordinary "no matching response within defaultCmdTimeout (8s)" outcome (confirmed by
	// TestWaitForTimeout in conn_wait_test.go), a normal, expected timeout on one action's
	// response, not evidence the connection is dead -- it too gets appended to errs and the loop
	// moves on to the next action. Only a net.Error whose Timeout() is false -- connection reset,
	// broken pipe, DNS failure, TLS error, etc. -- means the underlying TCP connection itself is
	// actually known-dead, so every subsequent action in this list is already doomed to
	// independently burn a full defaultCmdTimeout before failing the exact same way. FetchBuildings
	// (above) already distinguishes this class of failure via errors.As against net.Error (there,
	// Timeout()==true is the benign case since it's waiting for one thing rather than sequencing
	// independent actions); the loop below applies the same distinction with the opposite polarity
	// and breaks early only the first time a genuine non-timeout net.Error fires, instead of
	// pointlessly waiting out every remaining action's timeout in turn. The error that triggered
	// the break is still appended to errs first, so the caller's aggregated error still reports
	// what actually happened.
	//
	// Bug fixed here (round 22): the check below used to be a plain `errors.As(err, &netErr) &&
	// !netErr.Timeout()`. Three of these sub-actions -- GreetVisitors (visitors.go), ClaimAllMail
	// (mail.go), ClaimAllianceGifts (alliance.go) -- each loop over multiple items internally and,
	// per round 21's fix, already correctly distinguish a per-item Timeout()==true net.Error
	// (benign, keep going) from Timeout()==false (fatal, break that sub-action's own inner loop) --
	// but each still appends EVERY per-item error, including any earlier benign timeout, to a local
	// errs slice and returns one errors.Join(errs...) tree, not a single error. errors.As's own doc
	// comment says it "finds the first error in err's tree that matches target" via a depth-first
	// walk -- not the most severe one, and not "any" match -- so if one of those three sub-actions
	// hit an ordinary per-item timeout on item 1, then a genuine non-timeout net.Error (connection
	// actually dead) on item 2 in its own loop, the joined error it returns has the benign timeout
	// positioned first in the tree by that walk. The plain errors.As check above found THAT one,
	// saw Timeout()==true, and did not break -- even though the connection was genuinely dead and
	// every remaining action here was about to independently burn a full defaultCmdTimeout before
	// failing the same way. That silently reopened exactly the wasted-timeout problem round 21
	// fixed, for a realistic degrading-connection scenario (starts flaking with timeouts, then
	// actually dies) rather than just the original "any net.Error" over-broad case. Fixed by
	// containsNonTimeoutNetError below, which walks every branch of a possibly-joined error tree
	// instead of stopping at errors.As's first match -- see TestCollectAllAbortsOnMaskedNonTimeoutNetErrorInJoinedSubActionError.
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
	} else if len(toCollect) > maxCollectibleBuildingsPerRun {
		slog.Warn("collectible building count exceeds sanity cap; truncating",
			"count", len(toCollect), "cap", maxCollectibleBuildingsPerRun)
		toCollect = toCollect[:maxCollectibleBuildingsPerRun]
	}
	for _, b := range toCollect {
		actions = append(actions, func() error {
			cmd, _ := collectCmdFor(b.BId())
			slog.Info("attempting collect", "name", BuildingNameOf(b.BId()), "uuid", b.Uuid(), "buildingLevel", b.Level(), "cmd", cmd)
			params := sfs.NewSFSObject()
			params.PutLong("uuid", b.Uuid())
			_, err := session.SendAndWait(conn, "collect "+BuildingNameOf(b.BId()), cmd, params)
			return err
		})
	}

	for _, action := range actions {
		err := action()
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
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
