package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Radar rescue claims (gap G32, MASTER.md §3, cov-ui-military/F7), OPT-IN, claims only.
//
// radar-claims skips every FINISHED rescue task (detect_event type 11: 199 and 310001-310099,
// tables 40413) because its claim needs the troop-stock check. The client does that check from
// state it holds (RadarCenterDataManager.lua:1281-1313 for a single claim,
// DetectEventCompleteOneClickBtnView.lua:325-361 for Claim All): it sums `count` over the event's
// RESOURCE_ITEM (type 27) rewards whose itemId has an lw_soldier row, and claims with
// `receive.detect.event.reward {uuid: Long}` when that sum is at most
// math.modf(GetGameEffect(LW_SOLDIER_MAX_STOCK 30122)) - GetPlayerSoldiersTotalNum(); above it,
// it asks the player first (radar_army_01). The event's `reward` is flattened by
// ReturnRewardParamForView (DetectEventInfo.lua:126-128; RewardManager.lua:1040-1112): a
// RESOURCE_ITEM entry's itemId is value.id or value.itemId and its count value.num or value.add.
// GetPlayerSoldiersTotalNum with no argument sums the held type-1 (Player) soldiers only
// (SoldierDataManager.lua:79-108, 165-174), which is finishedTimersStockRoom's total; its cap is
// init effect 30122, a lower bound of GetGameEffect.
//
// The total is init's, and finished-timers runs earlier in the same run (phaseCapacity): it may
// collect ready Military Camp output and finished hospital queues, which add soldiers init does not
// show. So the camp output that is ready (prodBase) counts as held, and while a hospital or rebirth
// queue has finished (its troop count is not in init) no rescue is claimed this run. Finished
// rescues stay until claimed, so this only delays a claim. Changes init cannot show at all (a
// battle, the player's own taps after login) are not seen.
//
// This claims a FINISHED rescue only when its reward parses into at least one soldier entry with a
// count and the sum fits the room left; the room shrinks by each claim. A rescue whose reward is
// missing or unreadable, or one claimed while the stock is unknown, is skipped and logged with the
// raw reward: the reward's shape was not checked against a capture. Go never starts a rescue
// (radar-execute), so a FINISHED one exists only after the player ran it by hand. The claim scores
// a radar task in the Alliance Duel (score type 82), so like radar-claims it is held for days whose
// duel scores radar tasks. Static-only, not validated live.
const (
	radarRescueFeature = "radar-rescue-claims"
	// radarRescueMaxClaims bounds the claims per run (the radar holds at most detect_max_num 50
	// tasks, detect_level 20).
	radarRescueMaxClaims = 50
)

// radarSoldierIDs is every lw_soldier id (tables 40413, 46 rows): type 1 (Player) 3005-3015, type
// 2 4001, 4002 and 4005-4015, type 3 5005-5015, type 4 12001-12011. The client counts a reward as
// soldiers when its itemId has any lw_soldier row (hasLine, RadarCenterDataManager.lua:1287).
var radarSoldierIDs = func() map[int64]bool {
	ids := map[int64]bool{4001: true, 4002: true}
	for _, r := range [][2]int64{{3005, 3015}, {4005, 4015}, {5005, 5015}, {12001, 12011}} {
		for id := r[0]; id <= r[1]; id++ {
			ids[id] = true
		}
	}
	return ids
}()

func init() {
	registerFeature(Feature{
		Name: radarRescueFeature,
		Summary: "OPT-IN: claim finished radar rescue tasks whose rescued troops fit the soldier stock, as the client " +
			"claims them without its over-cap confirm; held for radar-scoring duel days (static-only, not validated live)",
		DefaultOn: false,
		Duel:      []DuelScore{{Type: DuelScoreRadarTask}},
		Hold:      true,
		Run:       runRadarRescueClaims,
	})
}

// radarRescueSoldiers sums the soldiers a rescue event's `reward` grants, as the client's claim
// test reads it. ok is false when the reward is absent, carries no soldier entry, or a soldier
// entry has no count: the claim is then left to the player.
func radarRescueSoldiers(ev *sfs.SFSObject) (sum int64, ok bool) {
	if ev == nil || !ev.Has("reward") {
		return 0, false
	}
	found := false
	for _, e := range claimObjectsOrMap(ev, "reward") {
		if t, _ := claimFirstInt(e, "type", "rewardType"); int32(t) != rewardTypeResourceItem {
			continue
		}
		v := evObject(e, "value")
		id, hasID := claimFirstInt(v, "id", "itemId")
		if !hasID || !radarSoldierIDs[id] {
			continue
		}
		n, hasN := claimFirstInt(v, "num", "add")
		if !hasN || n < 0 {
			return 0, false
		}
		found = true
		sum += n
	}
	return sum, found
}

// radarRescuePending is the soldiers finished-timers may add before this feature runs, as far as
// init shows them: the prodBase of every Military Camp whose output is ready (finishedTimersCampDone).
// ok is false while a hospital or rebirth queue has finished, whose healed count init lacks.
func radarRescuePending(in *Init, nowMs int64) (add int64, ok bool) {
	for _, b := range in.Objects("building_new") {
		if id, _ := claimInt(b, "bId"); id == finishedTimersMilitaryCamp && finishedTimersCampDone(b, nowMs) {
			n, _ := claimInt(b, "prodBase")
			add += max(n, 0)
		}
	}
	for _, q := range finishedTimersQueues(in) {
		if (q.Type == finishedTimersQueueHospital || q.Type == finishedTimersQueueRebirth) && q.Finished(nowMs) {
			return add, false
		}
	}
	return add, true
}

func runRadarRescueClaims(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(radarRescueFeature + ": no init push; skipping")
		return nil
	}
	params := sfs.NewSFSObject()
	params.PutBool("openWnd", false)
	info, err := session.SendAndWait(conn, "radar info", radarInfoCmd, params)
	if err != nil {
		return err
	}
	type rescue struct {
		uuid, eventID int64
		raw           *sfs.SFSObject
	}
	var rescues []rescue
	for _, o := range claimObjectsOrMap(info.Params, "events") {
		ev := parseDetectEvent(o)
		if ev.uuid != 0 && ev.state == radarStateFinished && ev.typ == radarTypeRescue {
			rescues = append(rescues, rescue{ev.uuid, ev.eventID, o})
		}
	}
	if len(rescues) == 0 {
		slog.Info(radarRescueFeature + ": no finished rescue task")
		return nil
	}
	slices.SortFunc(rescues, func(a, b rescue) int { return cmp.Compare(a.uuid, b.uuid) })
	held, stockCap, known := finishedTimersStockRoom(in)
	pending, pendingKnown := radarRescuePending(in, claimNow(in).UnixMilli())
	slog.Info(radarRescueFeature+": finished rescue tasks", "count", len(rescues), "soldiersHeld", held,
		"campOutputReady", pending, "stockCap", stockCap, "stockKnown", known)
	if !pendingKnown {
		slog.Info(radarRescueFeature + ": a hospital queue has finished and finished-timers may take its troops first; " +
			"leaving the rescues for the next run")
		return nil
	}
	held += pending
	var errs []error
	claims := 0
	for _, r := range rescues {
		label := fmt.Sprintf("radar rescue %d (event %d)", r.uuid, r.eventID)
		sum, ok := radarRescueSoldiers(r.raw)
		switch {
		case !ok:
			slog.Info(radarRescueFeature+": "+label+" skipped: its reward carries no readable soldier entry",
				"event", r.raw.StringRedacted())
			continue
		case !known:
			slog.Info(radarRescueFeature+": "+label+" skipped: init has no soldier stock cap (effect 30122) or resource_items",
				"soldiers", sum)
			continue
		case sum > stockCap-held:
			slog.Info(radarRescueFeature+": "+label+" skipped: the rescued troops exceed the stock room; the client asks first",
				"soldiers", sum, "room", stockCap-held)
			continue
		case claims >= radarRescueMaxClaims:
			slog.Info(radarRescueFeature+": claim cap reached for this run", "claims", claims)
			return errors.Join(errs...)
		}
		claims++
		p := sfs.NewSFSObject()
		p.PutLong("uuid", r.uuid)
		msg, err := claimAndLog(conn, label, radarEventCmd, p)
		if err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				break
			}
			continue
		}
		if msg != nil && !msg.Params.Has("errorCode") {
			held += sum
		}
	}
	return errors.Join(errs...)
}
