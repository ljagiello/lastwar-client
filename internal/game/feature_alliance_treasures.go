package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance treasures, the alliance channel's "digs", static-only. A radar treasure task (detect_event
// type 19, the "Excavator") is dug by its owner's march (MarchTargetType.DETECT_TREASURE,
// UIWorldPointBtn.lua:2137-2157). When the dig ends, the server posts "{owner}'s treasure at (x, y)
// has been dug up!" in the alliance channel (post 600 Detect_Treasure_Fin_Info, en 801349;
// ShareDecode.lua:679-711), and the treasure stays on the world map until its expireTime for up to
// para2 claimers ("Up To 20 Players Can Compete For Excavator Rewards", en
// activity_multiple_way3).
//
// The card comes over the chat WebSocket (ChatItemPostConfig.lua:535 binds it to ChatPushMsg). A
// tap sends only the read-only detect.event.get.treasure.claim.info {uuid, source: 1 (Chat),
// targetServer} and moves the camera to the reply's pointId (ChatPushMsg.lua:57-65,
// UIChatView_v2.lua:603-621). The claim is a tap on the world point (UIUtil.lua:1851-1917,
// 4099-4157): detect.event.claim.treasure {uuid: Long, targetServer: Int}, both from the point
// (DetectEventClaimTreasureMessage.lua:4-14). No SFS command lists treasures and init carries none;
// push.detect.treasure.claim only refreshes a bubble for a uuid
// (PushDetectTreasureClaimMessage.lua:7-19). The point is an ordinary world point, so this reads it
// the way fireworks reads HQ tiles:
//
//  1. al.rank, then world.get.block for every block within atBlockRadius blocks of a member HQ
//     (allianceTileScan). The server places a radar task near its owner's HQ when it is put on the
//     map (detect.event.put.point.in.world; en radar_tips_18 "not enough free space to place this
//     radar task. Please move to a location with more available space"). The radius is a guess:
//     the plan logs each treasure's distance from its owner's HQ to tune it.
//  2. A treasure is a WorldPointInfo of pointType 21 (TREASURE, EnumType.lua:8306) carrying
//     TreasurePointInfo, field 11 (worldpoint_proto.go). The client builds a WorldTreasureObject
//     for WorldTreasureType 1 (radar), 3 (activity radar, the "party") and 11 (off-season)
//     (Assembly-CSharp.decompiled.cs:181580-181590), and claims all three through one branch.
//
// A treasure is claimable when all of these hold, as for the client:
//
//   - it is a type 1/3/11 point whose eventId is a treasure event (detect_event type 19, 23 or 38;
//     radarEventType);
//   - it was dug: complete, or 0 < completionTime <= now (TreasurePointInfo.IsComplete,
//     A-CS:177945-177956; the client opens the dig window instead before that, UIUtil.lua:1870-1912);
//   - now < expireTime, when set (the "Despawns in" countdown, A-CS:186837-186869);
//   - the point's allianceId is the player's alliance (WorldTreasureObject.CheckInteractAuthority,
//     A-CS:186605-186628). Its other branch, a point shared to the whole server after
//     detect_event.share_para seconds, never applies to type 19, whose share_para is empty;
//   - the player is not in its rewardUserList, and fewer than cap * max(multiple, 1) players are
//     (IsHaveGetReward, IsReceiveAllReward; A-CS:177905-177943), cap being para2's second field
//     (radarTreasureCaps). IsHaveGetReward's other test, CheckTreasureReachDailyLimit, looks the
//     eventId up in world_treasure, which has no detect_event ids, so it never applies here
//     (ActDetectTreasureDataManager.lua:86-100);
//   - it is on the player's server (UIUtil.lua:4109-4115).
//
// The player's own treasures are left alone: the dig is the owner's radar task, and radar tasks
// score in the Alliance Duel at the claim (radar-claims, treasure-digs). Claiming an ally's
// treasure completes no radar task of the player (score type 82, "Complete Radar Tasks"), so the
// feature declares no duel score and never waits: the treasure goes to the first cap claimers
// and despawns at expireTime. That is static inference.
//
// The claim is free and needs no march: the request has no cost key and the reply only grants
// rewards (DetectEventClaimTreasureMessage.lua:15-43). Other players see it: the treasure's
// claimer list (detect.event.get.treasure.claim.info) and, on a double reward, the server's
// alliance post 610 "{0} has received double treasure Rewards!" (en radar_tips_11). Claims go
// atClaimPace apart, at most atMaxClaimsPerRun a run, the treasure that despawns first first.
// 801348 ("Already taken. One reward per player.") and activity_wajueji_27000_tips13 ("The party
// has expired and rewards cannot be claimed") have no client sender, so they are taken as the
// server's answers and are benign. The daily cap's code is unknown, so any other errorCode stops
// the run.
//
// Not done: digging or helping to dig (a march), the dig window, the chat card, and treasures of
// other kinds (siege, sandworm, flower train, ...), which have their own rules.

const (
	allianceTreasureClaimCmd = "detect.event.claim.treasure"

	atPointTypeTreasure = 21 // WorldPointType.TREASURE (EnumType.lua:8306)
	// atBlockRadius is how many 10-tile blocks around each member HQ's block are read: 3 covers
	// at least 30 tiles in every direction. Unverified: radar tasks are placed by the server.
	atBlockRadius     = 3
	atMaxClaimsPerRun = 20
)

// atClaimPace is the gap between claims, about a person tapping one treasure after another.
var atClaimPace = time.Second

// atTreasureTypes are the WorldTreasureTypes the client claims through its radar branch
// (UIUtil.lua:4136-4157; EnumType.lua:15206-15226).
var atTreasureTypes = map[int32]string{1: "radar", 3: "activity radar", 11: "off-season radar"}

// atTreasureEventTypes are the detect_event types of treasure events: TREASURE, TREASURE_ACTIVITY
// and OFF_SEASON_TREASURE (EnumType.lua:8528-8544).
var atTreasureEventTypes = []int32{19, 23, 38}

// allianceTreasureBenignCodes are the claim's "nothing to claim" answers (unverified; see above).
var allianceTreasureBenignCodes = map[string]string{
	"801348":                        "already claimed by you",
	"activity_wajueji_27000_tips13": "expired",
}

func init() {
	registerFeature(Feature{
		Name: "alliance-treasures-plan",
		Summary: "read-only: read the world blocks around alliance members' HQs (al.rank, world.get.block) and log every " +
			"radar treasure allies dug (the alliance chat's \"treasure ... has been dug up!\") and whether it is claimable; static-only",
		Run: runAllianceTreasuresPlan,
	})
	registerFeature(Feature{
		Name: "alliance-treasures",
		Summary: "OPT-IN: claim the radar treasures allies dug (the alliance chat's \"treasure ... has been dug up!\"; " +
			"detect.event.claim.treasure, free, no march); your name shows in the treasure's claimer list and a double " +
			"reward is posted to alliance chat by the server; static-only",
		Run: runAllianceTreasures,
	})
	for code := range allianceTreasureBenignCodes {
		session.RegisterBenignErrorCode(code, allianceTreasureClaimCmd)
	}
}

// radarCapRange is a run of treasure event ids [lo, hi] sharing one claim cap
// (radar_tables_gen.go).
type radarCapRange struct {
	lo, hi int64
	cap    int32
}

// radarTreasureCap returns para2's claim cap for a treasure event; ok is false for an id the
// table lacks.
func radarTreasureCap(eventID int64) (int32, bool) {
	i := sort.Search(len(radarTreasureCaps), func(i int) bool { return radarTreasureCaps[i].hi >= eventID })
	if i < len(radarTreasureCaps) && radarTreasureCaps[i].lo <= eventID {
		return radarTreasureCaps[i].cap, true
	}
	return 0, false
}

// atTreasure is one treasure point with the values the verdict and the claim use.
type atTreasure struct {
	pt       worldPoint
	uuid     int64 // PointInfo.uuid: WorldPointInfo.uuid, else TreasurePointInfo.uuid
	server   int64 // PointInfo.serverId, else the map read
	eventID  int64
	maxClaim int32 // cap * max(multiple, 1); 0 when unknown
}

func (sc *fwScan) treasure(pt worldPoint) atTreasure {
	t := pt.Treasure
	a := atTreasure{pt: pt, uuid: pt.UUID, server: int64(pt.ServerID)}
	if a.uuid == 0 {
		a.uuid = t.UUID
	}
	if a.server <= 0 {
		a.server = sc.server
	}
	a.eventID, _ = strconv.ParseInt(strings.TrimSpace(t.EventID), 10, 64)
	if c, ok := radarTreasureCap(a.eventID); ok {
		a.maxClaim = c * max(t.Multiple, 1)
	}
	return a
}

// verdict applies the claim rules to a at the scan's clock; "" is claimable. answered holds the
// treasures this run already sent a claim for.
func (sc *fwScan) verdict(a atTreasure, answered map[int64]bool) string {
	t := a.pt.Treasure
	evType, known := radarEventType(a.eventID)
	switch {
	case a.pt.PointType != atPointTypeTreasure:
		return "not a treasure point"
	case atTreasureTypes[t.Type] == "":
		return "not a radar treasure"
	case !known || !slices.Contains(atTreasureEventTypes, evType):
		return "not a treasure event"
	case a.uuid == 0:
		return "no uuid"
	case answered[a.uuid]:
		return "already answered this run"
	case t.OwnerUID != "" && t.OwnerUID == sc.ownUID:
		return "own treasure"
	case t.AllianceID == "" || t.AllianceID != sc.allianceID:
		return "another alliance's treasure"
	case a.server != sc.server:
		return "on another server"
	case !t.Complete && (t.CompletionTime <= 0 || t.CompletionTime > sc.now.UnixMilli()):
		return "still digging"
	case t.ExpireTime > 0 && sc.now.UnixMilli() >= t.ExpireTime:
		return "expired"
	case t.ClaimedBy(sc.ownUID):
		return "already claimed by you"
	case a.maxClaim > 0 && t.Claimers() >= int(a.maxClaim):
		return "full"
	}
	return ""
}

// atChebyshev is the tile distance between two point ids (the larger of the x and y gaps); -1
// when either is off the map.
func atChebyshev(a, b int64) int {
	ax, ay, ok1 := fwTilePos(a)
	bx, by, ok2 := fwTilePos(b)
	if !ok1 || !ok2 {
		return -1
	}
	return max(ax-bx, bx-ax, ay-by, by-ay)
}

// nearestHQ is the tile distance from point to the closest scanned member HQ; -1 when none.
func (sc *fwScan) nearestHQ(point int64) int {
	best := -1
	for _, hq := range sc.roster.points {
		if d := atChebyshev(point, hq); d >= 0 && (best < 0 || d < best) {
			best = d
		}
	}
	return best
}

// atPointTypes formats the decoded points' type counts as "type:count,..." in type order.
func atPointTypes(m map[int32]int) string {
	parts := make([]string, 0, len(m))
	for _, typ := range slices.Sorted(maps.Keys(m)) {
		parts = append(parts, fmt.Sprintf("%d:%d", typ, m[typ]))
	}
	return strings.Join(parts, ",")
}

// reviewTreasures logs every treasure point with its verdict and returns the claimable ones, the
// one that despawns first first.
func (sc *fwScan) reviewTreasures(answered map[int64]bool) []atTreasure {
	slog.Info(sc.name+": treasure points", "treasurePointsPresent", len(sc.treasures) > 0, "treasurePoints", len(sc.treasures),
		"pointTypes", atPointTypes(sc.pointTypes["points"]), "alInfoTypes", atPointTypes(sc.pointTypes["alInfos"]))
	var picks []atTreasure
	for _, pt := range sc.treasures {
		a := sc.treasure(pt)
		t := pt.Treasure
		why := sc.verdict(a, answered)
		x, y, _ := fwTilePos(int64(pt.ID))
		owner := sc.roster.byUID[t.OwnerUID]
		distOwner := -1
		if owner.PointID > 0 {
			distOwner = atChebyshev(int64(pt.ID), owner.PointID)
		}
		expiresIn := int64(-1)
		if t.ExpireTime > 0 {
			expiresIn = int64(time.UnixMilli(t.ExpireTime).Sub(sc.now).Minutes())
		}
		slog.Info(sc.name+": treasure", "uuid", a.uuid, "owner", sc.ref(t.OwnerUID), "ownerRank", owner.Rank,
			"sameAlliance", t.AllianceID != "" && t.AllianceID == sc.allianceID, "eventId", t.EventID,
			"treasureType", t.Type, "kind", atTreasureTypes[t.Type], "pointType", pt.PointType, "claimed", t.Claimers(),
			"maxClaims", a.maxClaim, "multiple", t.Multiple, "complete", t.Complete, "expiresInMin", expiresIn,
			"tileX", x, "tileY", y, "distToOwnerHQ", distOwner, "distToNearestHQ", sc.nearestHQ(int64(pt.ID)),
			"eligible", why == "", "reason", why)
		if why == "" {
			picks = append(picks, a)
		}
	}
	slices.SortStableFunc(picks, func(a, b atTreasure) int {
		ea, eb := a.pt.Treasure.ExpireTime, b.pt.Treasure.ExpireTime
		if (ea > 0) != (eb > 0) {
			return cmp.Compare(eb, ea) // a set expiry before none
		}
		return cmp.Or(cmp.Compare(ea, eb), cmp.Compare(a.uuid, b.uuid))
	})
	slog.Info(sc.name+": treasures", "found", len(sc.treasures), "eligible", len(picks))
	return picks
}

func runAllianceTreasuresPlan(conn *session.GameConn, in *Init) error {
	sc, err := allianceTileScan(conn, in, "alliance-treasures", atBlockRadius)
	if sc != nil {
		sc.reviewTreasures(nil)
	}
	return err
}

func runAllianceTreasures(conn *session.GameConn, in *Init) error {
	sc, err := allianceTileScan(conn, in, "alliance-treasures", atBlockRadius)
	if sc == nil || session.ContainsNonTimeoutNetError(err) {
		return err
	}
	errs := []error{err}
	answered := map[int64]bool{}
	picks := sc.reviewTreasures(answered)
	if len(picks) > atMaxClaimsPerRun {
		slog.Info("alliance-treasures: more claimable treasures than the per-run cap; the rest wait for the next run",
			"eligible", len(picks), "cap", atMaxClaimsPerRun)
		picks = picks[:atMaxClaimsPerRun]
	}
	got := 0
	for i, a := range picks {
		if i > 0 {
			fwSleep(atClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", a.uuid)
		p.PutInt("targetServer", int32(a.server))
		resp, code, err := fwRequest(conn, "alliance-treasures: treasure claim", allianceTreasureClaimCmd, p)
		if err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				break
			}
			continue
		}
		answered[a.uuid] = true
		who := []any{"uuid", a.uuid, "owner", sc.ref(a.pt.Treasure.OwnerUID), "eventId", a.pt.Treasure.EventID}
		if code == "" {
			got++
			slog.Info("alliance-treasures: claimed a treasure", append(who, "isBigReward", evBool(resp, "isBigReward"),
				"bigRewardMultiple", evString(resp, "bigRewardMultiple"), "treasureRewardLimit", evString(resp, "treasureRewardLimit"),
				"reward", fwReward(resp), "replyKeys", resp.Keys())...)
			continue
		}
		if meaning := allianceTreasureBenignCodes[code]; meaning != "" {
			slog.Info("alliance-treasures: treasure not claimable (expected)", append(who, "errorCode", code, "meaning", meaning)...)
			continue
		}
		// The daily cap's code is unknown; stop on any unexpected answer rather than send more.
		slog.Warn("alliance-treasures: claim refused; stopping this run", append(who, "errorCode", code)...)
		errs = append(errs, fmt.Errorf("alliance-treasures: %s uuid %d: errorCode=%s", allianceTreasureClaimCmd, a.uuid, code))
		break
	}
	slog.Info("alliance-treasures: claims done", "claimed", got, "answered", len(answered), "eligible", len(picks))
	return errors.Join(errs...)
}
