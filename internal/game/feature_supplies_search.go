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

// Supplies search (gap G44, MASTER.md §3, cov-dc-world/F6), OPT-IN, claims only: the one
// risk-free step of the push-your-luck search, then its reward.
//
// A radar event or a supplies-search building (LW_BUILD_SUPPLIES_SEARCH_1-3, 10150000, 10151000,
// 10152000, EnumType.lua:3792-3794) can carry suppliesSearchInfo {cfgId, curLevel, isFail, ...}
// (DetectEventInfo.lua:117-118; BuildingDate.lua:228-230); no radar event type marks it. The
// client opens it from the event's world point (UIWorldPointBtn.lua:2632-2636) or the building's
// bubble (BuildBubbleManager.lua:901-903, 4013-4014) into UISuppliesSearchView, where Collect sends
// `detect.event.supplies.search.next {eventUuid: Long}` or `building.supplies.search.next
// {buildingUuid: Long}` and Leave sends `detect.event.get.supplies.search.reward {eventUuid}` or
// `building.get.supplies.search.reward {buildingUuid}` (SuppliesSearchManager.lua:25-39;
// Net/Msgs/SuppliesSearch/*). The next reply carries the new suppliesSearchInfo
// (SuppliesSearchManager.lua:65-79). Each Collect moves up one level with that level's rate from
// monopoly_search.level_rate; isFail 1 ends the search on the fail rewards; Leave is offered from
// level 1 (UISuppliesSearchView.lua:125-200). Level 1's rate is 10000 (100 %) in all 171
// monopoly_search rows of tables 40413 ("1;10000|2;8000|3;6000|4;4000|5;3000"), so the first
// Collect cannot fail; every later one can.
//
// So this sends next only for a search at curLevel 0 that has not failed, and the reward only when
// the next reply shows curLevel >= 1 and no failure. It never sends a second next: going past
// level 1 is a gamble for the player to take (MASTER.md §6.1 #13). A radar event must already be on
// the map (state 0, not expired): the client reaches the window only through its world point, and
// this sends no placement. A radar search completes a radar task, which scores in the Alliance Duel
// (score type 82) like a radar ruin's chest, so the radar form runs only on a day whose duel
// scores radar tasks; the building form runs every day. No request carries a cost, but what the
// table's win_diamond / fail_diamond values mean was not traced, so every reply's gold or
// remainGold is checked against init user.gold and a drop trips the diamond tripwire. Every reply
// is logged in full (SendAndWait). Static-only, not validated live.
const (
	suppliesSearchFeature   = "supplies-search"
	suppliesRadarNextCmd    = "detect.event.supplies.search.next"
	suppliesRadarRewardCmd  = "detect.event.get.supplies.search.reward"
	suppliesBuildNextCmd    = "building.supplies.search.next"
	suppliesBuildRewardCmd  = "building.get.supplies.search.reward"
	suppliesSearchMaxClaims = 20
)

// suppliesSearchBuildings are LW_BUILD_SUPPLIES_SEARCH_1-3 (EnumType.lua:3792-3794).
var suppliesSearchBuildings = []int32{10150000, 10151000, 10152000}

func init() {
	registerFeature(Feature{
		Name: suppliesSearchFeature,
		Summary: "OPT-IN: take the one risk-free step (level 1, 100 %) of a radar or building supplies search, then its reward; " +
			"never searches further; radar searches only on radar-scoring duel days (static-only, not validated live)",
		DefaultOn: false,
		// The radar form completes a radar task (score 82) and is held per event on other days, so the
		// feature itself runs every day for the building form.
		Duel: []DuelScore{{Type: DuelScoreRadarTask}},
		Run:  runSuppliesSearch,
	})
}

// suppliesSearchState is a suppliesSearchInfo the run acts on.
type suppliesSearchState struct {
	level    int64
	hasLevel bool
	failed   bool
}

func parseSuppliesSearch(o *sfs.SFSObject) (suppliesSearchState, bool) {
	if o == nil {
		return suppliesSearchState{}, false
	}
	var s suppliesSearchState
	s.level, s.hasLevel = claimInt(o, "curLevel")
	fail, _ := claimInt(o, "isFail")
	s.failed = fail == 1
	return s, true
}

// fresh is a search the risk-free step applies to: curLevel present and 0, not failed.
func (s suppliesSearchState) fresh() bool { return s.hasLevel && s.level == 0 && !s.failed }

// suppliesSearchRun is one run's sender and diamond check.
type suppliesSearchRun struct {
	conn    *session.GameConn
	gold    int64
	goldSet bool
	claims  int
	errs    []error
	halt    bool
}

func (r *suppliesSearchRun) send(label, cmd, key string, uuid int64) (*sfs.SFSObject, bool) {
	if r.halt {
		return nil, false
	}
	p := sfs.NewSFSObject()
	p.PutLong(key, uuid)
	msg, err := session.SendAndWait(r.conn, label, cmd, p)
	if err != nil {
		r.errs = append(r.errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			r.halt = true
		}
		return nil, false
	}
	if msg.Params.Has("errorCode") {
		return nil, false
	}
	r.checkGold(label, cmd, msg.Params)
	return msg.Params, !r.halt
}

// checkGold trips the diamond tripwire when a reply's gold or remainGold is below the balance seen
// so far (init user.gold, PlayerInfo.lua:630-632), and halts the run.
func (r *suppliesSearchRun) checkGold(label, cmd string, reply *sfs.SFSObject) {
	for _, key := range []string{"gold", "remainGold"} {
		g, ok := claimInt(reply, key)
		if !ok {
			continue
		}
		if r.goldSet && g < r.gold {
			reason := fmt.Sprintf("%s (%s): diamonds fell from %d to %d", label, cmd, r.gold, g)
			slog.Error(suppliesSearchFeature + ": " + reason + "; stopping")
			tripwireTrip(suppliesSearchFeature, cmd, reason)
			r.errs = append(r.errs, errors.New(suppliesSearchFeature+": "+reason))
			r.halt = true
			return
		}
		r.gold, r.goldSet = g, true
	}
}

// search takes the risk-free step and, when it landed on level 1 or above without failing, the
// reward.
func (r *suppliesSearchRun) search(label, nextCmd, rewardCmd, key string, uuid int64) {
	if r.claims >= suppliesSearchMaxClaims {
		slog.Info(suppliesSearchFeature+": claim cap reached for this run", "claims", r.claims)
		r.halt = true
		return
	}
	r.claims++
	reply, ok := r.send(label+" step 1", nextCmd, key, uuid)
	if !ok {
		return
	}
	st, has := parseSuppliesSearch(evObject(reply, "suppliesSearchInfo"))
	switch {
	case !has || !st.hasLevel:
		slog.Warn(suppliesSearchFeature+": "+label+": the step's reply carries no suppliesSearchInfo.curLevel; leaving it to the player",
			"reply", reply.StringRedacted())
		return
	case st.failed || st.level < 1:
		slog.Warn(suppliesSearchFeature+": "+label+": the 100 % step did not reach level 1; leaving it to the player",
			"curLevel", st.level, "failed", st.failed)
		return
	}
	if _, ok := r.send(label+" reward", rewardCmd, key, uuid); ok {
		slog.Info(suppliesSearchFeature+": "+label+" collected at level 1", "uuid", uuid)
	}
}

func runSuppliesSearch(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(suppliesSearchFeature + ": no init push; skipping")
		return nil
	}
	// The feature writes the diamond tripwire (checkGold), so it also honours one set by any run:
	// nothing is sent until the file is cleared.
	if err := tripwireErr(suppliesSearchFeature); err != nil {
		return err
	}
	r := &suppliesSearchRun{conn: conn}
	if g, ok := claimInt(in.Object("user"), "gold"); ok {
		r.gold, r.goldSet = g, true
	}
	r.buildings(in)
	if !r.halt {
		r.radar(in)
	}
	return errors.Join(r.errs...)
}

// buildings searches every supplies-search building whose search is fresh.
func (r *suppliesSearchRun) buildings(in *Init) {
	for _, b := range in.Buildings {
		if r.halt {
			return
		}
		if !slices.Contains(suppliesSearchBuildings, b.BId()) {
			continue
		}
		info := evObject(b.Raw, "suppliesSearchInfo")
		label := fmt.Sprintf("supplies search building %d (%d)", b.Uuid(), b.BId())
		st, has := parseSuppliesSearch(info)
		if !has || !st.fresh() {
			slog.Info(suppliesSearchFeature+": "+label+" not at a fresh level 0", "suppliesSearchInfo", has, "curLevel", st.level,
				"failed", st.failed)
			continue
		}
		r.search(label, suppliesBuildNextCmd, suppliesBuildRewardCmd, "buildingUuid", b.Uuid())
	}
}

// radar searches every fresh supplies search among the radar events on the map, on a day whose
// Alliance Duel scores radar tasks.
func (r *suppliesSearchRun) radar(in *Init) {
	p := sfs.NewSFSObject()
	p.PutBool("openWnd", false)
	msg, err := session.SendAndWait(r.conn, "radar info", radarInfoCmd, p)
	if err != nil {
		r.errs = append(r.errs, err)
		r.halt = r.halt || session.ContainsNonTimeoutNetError(err)
		return
	}
	type found struct {
		ev  detectEvent
		raw *sfs.SFSObject
	}
	var due []found
	nowMs := evNow().UnixMilli()
	for _, o := range claimObjectsOrMap(msg.Params, "events") {
		info := evObject(o, "suppliesSearchInfo")
		if info == nil {
			continue
		}
		ev := parseDetectEvent(o)
		label := fmt.Sprintf("supplies search radar event %d (event %d)", ev.uuid, ev.eventID)
		st, _ := parseSuppliesSearch(info)
		switch {
		case ev.uuid == 0:
			continue
		case ev.state != radarStateNotFinish:
			slog.Info(suppliesSearchFeature+": "+label+" skipped: not open on the map", "state", ev.state)
		case ev.endTime > 0 && ev.endTime <= nowMs:
			slog.Info(suppliesSearchFeature + ": " + label + " skipped: expired")
		case !st.fresh():
			slog.Info(suppliesSearchFeature+": "+label+" skipped: not at a fresh level 0", "curLevel", st.level,
				"present", st.hasLevel, "failed", st.failed)
		default:
			due = append(due, found{ev, o})
		}
	}
	if len(due) == 0 {
		slog.Info(suppliesSearchFeature + ": no fresh radar supplies search")
		return
	}
	if !TodayDuel(r.conn, in).Scores(DuelScoreRadarTask, "") {
		slog.Info(suppliesSearchFeature+": radar supplies searches held: today's Alliance Duel doesn't score radar tasks",
			"count", len(due))
		return
	}
	end := func(ev detectEvent) int64 {
		if ev.endTime <= 0 {
			return 1<<62 - 1
		}
		return ev.endTime
	}
	slices.SortStableFunc(due, func(a, b found) int { return cmp.Compare(end(a.ev), end(b.ev)) })
	for _, d := range due {
		if r.halt {
			return
		}
		label := fmt.Sprintf("supplies search radar event %d (event %d)", d.ev.uuid, d.ev.eventID)
		r.search(label, suppliesRadarNextCmd, suppliesRadarRewardCmd, "eventUuid", d.ev.uuid)
	}
}
