package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance treasure boxes (MASTER G41, cov-core/F5), opt-in and static-only: the boxes an alliance
// gets on the world map after a siege, an invasion or the alliance-boss sandbox, WorldTreasureType
// 2 SiegeTreasure, 5 InvasionTreasure and 8 AllianceBossSandBox (EnumType.lua:15271-15292). They
// are TREASURE points (pointType 21) with a TreasurePointInfo, like the radar treasures, and
// alliance-tile-scan (sharedAllianceTileScan) already decodes the ones inside the blocks it reads
// around member HQs. Where the server places them is not in the client, so the scan may never see
// one.
//
// The client claims one on a tap (UIUtil.GetDetectTreasureReward, UIUtil.lua:4228-4320) when:
//   - fewer players than the box's world_treasure.max_reward_times (default 10) are in its
//     rewardUserList (:4286-4296, tip new_city_activity_battle_tips1025);
//   - CheckInteractAuthority holds: for these three types the point's `;`-separated allianceId
//     lists the player's alliance (SiegeTreasureObject.CheckIsSameAlliance, CS:186288-186303,
//     186547-186575; UIUtil.lua:4299-4309);
//   - IsHaveGetReward is false (GetRewardOrShareTreasureBox, UIUtil.lua:4166-4179): the player is
//     not in rewardUserList and the daily limit is not reached (TreasurePointInfo.IsHaveGetReward,
//     CS:178613-178620 -> ActDetectTreasureDataManager:CheckTreasureReachDailyLimit,
//     ActDetectTreasureDataManager.lua:92-106: world_treasure.daily_max, counted per group when the
//     row has one, against dailyGot, filled from init treasureDailyLimit[{id, num, time}] entries
//     of today (:27-35) and from each claim reply's cfgId/treasureRewardLimit (:75-89));
//   - the player is at home (UIUtil.lua:4239-4245): the point is on the server the scan read.
//
// The claim is the radar branch's request, `detect.event.claim.treasure {uuid: Long,
// targetServer: Int}` (DetectEventClaimTreasureMessage.lua:5-15; targetServer is the point's
// serverId). It is free and needs no march; the player's name shows in the box's claimer list.
// The box's expireTime, when set and past, also skips it (the point should be gone by then).
// Claims go atClaimPace apart, at most atBoxMaxClaimsPerRun a run, the box that despawns first
// first. Static-only: the daily limit's errorCode is unknown and no reply has been seen, so the
// first errorCode of any kind stops the run, and the first reply is logged at Info.
const (
	atBoxFeature         = "alliance-treasure-boxes"
	atBoxMaxClaimsPerRun = 10
	// atBoxDefaultMaxReward is GetTableData's default for max_reward_times (UIUtil.lua:4291).
	atBoxDefaultMaxReward = 10
)

// atBoxTypes are the WorldTreasureTypes this feature claims.
var atBoxTypes = map[int32]string{2: "siege", 5: "invasion", 8: "alliance-boss sandbox"}

// worldTreasureRow is one row of table world_treasure: max_reward_times, daily_max and group.
type worldTreasureRow struct{ maxReward, dailyMax, group int32 }

// worldTreasureRows holds all 98 rows of table world_treasure 40413 by id (= the point's eventId).
var worldTreasureRows = map[int64]worldTreasureRow{
	1: {2, 0, 0}, 2: {1, 0, 0}, 3: {10, 0, 0}, 4: {10, 0, 0}, 5: {10, 0, 0}, 6: {10, 0, 0},
	7: {10, 0, 0}, 8: {10, 0, 0}, 9: {10, 0, 0}, 10: {10, 0, 0}, 11: {10, 0, 0}, 12: {10, 0, 0},
	13: {10, 0, 0}, 14: {10, 0, 0}, 15: {10, 0, 0}, 30: {5, 30, 0}, 31: {5, 20, 0}, 32: {5, 10, 0},
	33: {1, 5, 0}, 34: {5, 5, 0}, 35: {1, 5, 0}, 36: {10, 0, 0}, 37: {200, 1, 0}, 38: {200, 1, 0},
	39: {5, 15, 0}, 40: {10, 0, 0}, 41: {10, 0, 0}, 42: {10, 0, 0}, 101: {10, 2, 0}, 102: {10, 2, 0},
	103: {10, 2, 0}, 104: {10, 2, 0}, 105: {10, 2, 0}, 106: {10, 2, 0}, 107: {10, 2, 0}, 108: {10, 2, 0},
	109: {10, 2, 0}, 110: {10, 2, 0}, 111: {10, 2, 0}, 112: {10, 2, 0}, 201: {3, 1, 0}, 202: {3, 1, 0},
	203: {3, 1, 0}, 204: {3, 1, 0}, 205: {3, 1, 0}, 206: {3, 1, 0}, 207: {3, 1, 0}, 208: {3, 1, 0},
	209: {3, 1, 0}, 210: {3, 1, 0}, 211: {3, 1, 0}, 212: {3, 1, 0}, 301: {20, 10, 0}, 302: {20, 10, 0},
	303: {15, 10, 0}, 304: {15, 10, 0}, 305: {12, 10, 0}, 306: {10, 10, 0}, 400: {10, 10, 0}, 401: {10, 10, 0},
	402: {10, 10, 0}, 403: {10, 10, 0}, 404: {1, 0, 0}, 405: {1, 0, 0}, 406: {1, 0, 0}, 407: {1, 0, 0},
	408: {10, 10, 0}, 409: {10, 10, 0}, 410: {10, 10, 0}, 411: {10, 10, 0}, 412: {1, 0, 0}, 413: {1, 0, 0},
	414: {1, 0, 0}, 415: {1, 0, 0}, 500: {20, 10, 0}, 501: {1, 0, 0}, 600: {20, 10, 0}, 601: {5, 10, 0},
	602: {10, 10, 602}, 603: {10, 10, 602}, 604: {10, 10, 602}, 605: {10, 10, 602}, 606: {10, 10, 602}, 607: {10, 10, 602},
	608: {10, 10, 602}, 609: {10, 10, 602}, 610: {10, 10, 602}, 611: {10, 10, 602}, 612: {10, 10, 602}, 700: {5, 30, 0},
	701: {5, 20, 0}, 702: {5, 10, 0}, 703: {10, 5, 703}, 704: {10, 5, 703}, 705: {10, 5, 703}, 706: {10, 5, 703},
	707: {10, 5, 703}, 902: {1, 0, 0},
}

func init() {
	registerFeature(Feature{
		Name: atBoxFeature,
		Summary: "OPT-IN: claim the alliance's siege, invasion and alliance-boss sandbox treasure boxes (world treasure " +
			"types 2, 5, 8) the alliance tile scan finds near member HQs (detect.event.claim.treasure, free, no march); " +
			"your name shows in the box's claimer list (static-only, not validated live)",
		Run: runAllianceTreasureBoxes,
	})
}

// atBoxDaily is the client's dailyGot: claims today by world_treasure id or group.
type atBoxDaily map[int64]int64

// newAtBoxDaily reads init treasureDailyLimit, keeping the entries of the current server day
// (IsToday). Without init `tomorrow` the day can't be told, so every entry counts as today's: a
// limit then skips a box rather than risk one claim too many.
func newAtBoxDaily(in *Init) atBoxDaily {
	d := atBoxDaily{}
	for _, e := range claimObjectsOrMap(in.Raw, "treasureDailyLimit") {
		id, ok1 := evNum(e, "id")
		num, ok2 := evNum(e, "num")
		if !ok1 || !ok2 {
			continue
		}
		ts, _ := evNum(e, "time")
		if today, ok := evClaimedToday(in, ts); today || !ok {
			d[id] = num
		}
	}
	return d
}

// reached is CheckTreasureReachDailyLimit for a world_treasure row.
func (d atBoxDaily) reached(eventID int64, row worldTreasureRow) bool {
	if row.dailyMax <= 0 {
		return false
	}
	key := eventID
	if row.group > 0 {
		key = int64(row.group)
	}
	got, ok := d[key]
	return ok && int64(row.dailyMax) <= got
}

// record applies a claim reply's cfgId and treasureRewardLimit (OnGetDigTimesMsg,
// ActDetectTreasureDataManager.lua:75-81).
func (d atBoxDaily) record(resp *sfs.SFSObject) {
	cfg, ok := evNum(resp, "cfgId")
	if !ok {
		return
	}
	n, _ := evNum(resp, "treasureRewardLimit")
	d[cfg] = n
	if row, ok := worldTreasureRows[cfg]; ok && row.group > 0 {
		d[int64(row.group)] = n
	}
}

// atBox is one box point with the values its verdict and claim use.
type atBox struct {
	pt        worldPoint
	uuid      int64
	server    int64
	eventID   int64
	row       worldTreasureRow
	rowKnown  bool
	maxReward int32
}

func (sc *fwScan) box(pt worldPoint) atBox {
	a := atBox{pt: pt, uuid: pt.UUID, server: int64(pt.ServerID), maxReward: atBoxDefaultMaxReward}
	if a.uuid == 0 {
		a.uuid = pt.Treasure.UUID
	}
	if a.server <= 0 {
		a.server = sc.server
	}
	a.eventID, _ = strconv.ParseInt(strings.TrimSpace(pt.Treasure.EventID), 10, 64)
	if a.row, a.rowKnown = worldTreasureRows[a.eventID]; a.rowKnown {
		a.maxReward = a.row.maxReward
	}
	return a
}

// boxVerdict applies the client's rules to a at the scan's clock; "" is claimable.
func (sc *fwScan) boxVerdict(a atBox, daily atBoxDaily, answered map[int64]bool) string {
	t := a.pt.Treasure
	switch {
	case sc.ownUID == "":
		return "own uid unknown"
	case a.pt.PointType != atPointTypeTreasure:
		return "not a treasure point"
	case atBoxTypes[t.Type] == "":
		return "not an alliance box"
	case a.uuid == 0:
		return "no uuid"
	case answered[a.uuid]:
		return "already answered this run"
	case !slices.Contains(strings.Split(t.AllianceID, ";"), sc.allianceID):
		return "another alliance's box"
	case a.server != sc.server:
		return "on another server"
	case t.ExpireTime > 0 && !sc.now.Before(evUnix(t.ExpireTime)):
		return "expired"
	case t.ClaimedBy(sc.ownUID):
		return "already claimed by you"
	case t.Claimers() >= int(a.maxReward):
		return "full"
	case daily.reached(a.eventID, a.row):
		return "daily limit reached"
	}
	return ""
}

// reviewBoxes logs every box point with its verdict and returns the claimable ones, the one that
// despawns first first.
func (sc *fwScan) reviewBoxes(daily atBoxDaily, answered map[int64]bool) []atBox {
	var picks []atBox
	found := 0
	for _, pt := range sc.treasures {
		if atBoxTypes[pt.Treasure.Type] == "" {
			continue
		}
		found++
		a := sc.box(pt)
		why := sc.boxVerdict(a, daily, answered)
		slog.Info(atBoxFeature+": box", "uuid", a.uuid, "treasureType", pt.Treasure.Type, "kind", atBoxTypes[pt.Treasure.Type],
			"eventId", pt.Treasure.EventID, "inTable", a.rowKnown, "claimed", pt.Treasure.Claimers(), "maxClaims", a.maxReward,
			"dailyMax", a.row.dailyMax, "alliances", len(strings.Split(pt.Treasure.AllianceID, ";")),
			"distToNearestHQ", sc.nearestHQ(int64(pt.ID)), "eligible", why == "", "reason", why)
		if why == "" {
			picks = append(picks, a)
		}
	}
	slices.SortStableFunc(picks, func(a, b atBox) int {
		ea, eb := a.pt.Treasure.ExpireTime, b.pt.Treasure.ExpireTime
		if (ea > 0) != (eb > 0) {
			return cmp.Compare(eb, ea) // a set expiry before none
		}
		return cmp.Or(cmp.Compare(ea, eb), cmp.Compare(a.uuid, b.uuid))
	})
	slog.Info(atBoxFeature+": boxes", "found", found, "eligible", len(picks))
	return picks
}

func runAllianceTreasureBoxes(conn *session.GameConn, in *Init) error {
	sc, err := sharedAllianceTileScan(conn, in, atBoxFeature)
	if sc == nil || session.ContainsNonTimeoutNetError(err) {
		return err
	}
	errs := []error{err}
	daily := newAtBoxDaily(in)
	answered := map[int64]bool{}
	picks := sc.reviewBoxes(daily, answered)
	if len(picks) > atBoxMaxClaimsPerRun {
		slog.Info(atBoxFeature+": more claimable boxes than the per-run cap; the rest wait for the next run",
			"eligible", len(picks), "cap", atBoxMaxClaimsPerRun)
		picks = picks[:atBoxMaxClaimsPerRun]
	}
	got := 0
	for i, a := range picks {
		if why := sc.boxVerdict(a, daily, answered); why != "" { // a claim this run can reach the daily limit
			slog.Info(atBoxFeature+": box no longer claimable", "uuid", a.uuid, "reason", why)
			continue
		}
		if i > 0 {
			fwSleep(atClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", a.uuid)
		p.PutInt("targetServer", int32(a.server))
		resp, code, err := fwRequest(conn, atBoxFeature+": box claim", allianceTreasureClaimCmd, p)
		if err != nil {
			errs = append(errs, err)
			break
		}
		answered[a.uuid] = true
		who := []any{"uuid", a.uuid, "treasureType", a.pt.Treasure.Type, "eventId", a.pt.Treasure.EventID}
		if code != "" {
			// Never sent live: any refusal, the unknown daily-limit code among them, ends the run.
			slog.Warn(atBoxFeature+": claim refused; stopping this run", append(who, "errorCode", code, "errorMsg", evString(resp, "errorMsg"))...)
			errs = append(errs, fmt.Errorf("%s: %s uuid %d: errorCode=%s", atBoxFeature, allianceTreasureClaimCmd, a.uuid, code))
			break
		}
		got++
		daily.record(resp)
		slog.Info(atBoxFeature+": claimed a box", append(who, "cfgId", evString(resp, "cfgId"),
			"treasureRewardLimit", evString(resp, "treasureRewardLimit"), "reward", fwReward(resp), "replyKeys", resp.Keys())...)
	}
	slog.Info(atBoxFeature+": claims done", "claimed", got, "answered", len(answered), "eligible", len(picks))
	return errors.Join(errs...)
}
