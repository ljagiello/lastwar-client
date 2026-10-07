package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"lastwar-client/internal/session"
)

// Radar overflow guard (DUEL.md §5.2.3, §5.2.6 step 3). radar-claims holds every radar claim for a
// day whose Alliance Duel scores radar tasks, but the bank is bounded: the shown slots
// (detect_level.detect_show_num) plus the stock (detectInfo.eventNum, capped at
// detect_level.detect_max_num). Every detect_level.refresh period (360 min) N new tasks arrive,
// into empty slots first and then into stock, and none arrive once stock is at its cap: the
// client's own model, which shows "full" at maxNum <= eventNum and otherwise a time to full of
// ceil((maxNum - eventNum + max(showMax - slots, 0) - N) / N) refreshes (GetNormalEventInfo,
// UIDetectEventCtrl.lua:371-394; slots = every task the server lists, GetDetectEventCount,
// RadarCenterDataManager.lua:212-214; eventNum, GetMaxDetectNum, :182-184).
//
// So on a day that doesn't score radar tasks, the next refresh loses
// k = eventNum + N - max(show - slots, 0) - maxNum tasks. Each claim lowers that by one: it frees
// a slot and the reply's newEvent refills it from stock (RadarCenterDataManager.lua:289-327).
// This claims exactly k FINISHED tasks, oldest (lowest startTime) first, never a rescue or a
// cockatrice guide task (radarSkippedEvent) or a task whose eventId the table lacks. Their rewards
// still arrive; only their duel points are given up, and those tasks would be lost otherwise. On a
// radar-scoring day it does nothing (radar-claims claims everything). It runs after
// radar-execute (RunFeatures' name order), so tasks executed in the same run count as finished.
// When an input is missing it claims nothing. Static-only: not sent live yet.

func init() {
	registerFeature(Feature{
		Name: "radar-overflow",
		Summary: "on days the Alliance Duel doesn't score radar tasks, claim only the finished radar tasks " +
			"the next refresh would otherwise push past the bank (stock cap), oldest first",
		Run: runRadarOverflow,
	})
}

// radarOverflow is the bank arithmetic for the next refresh.
type radarOverflow struct {
	level, eventNum, show, maxNum, refreshN, slots, freeSlots, k int64
}

// radarOverflowPlan returns the overflow and the tasks to claim for it, or an error naming the
// input it lacks.
func radarOverflowPlan(snap *detectSnapshot) (radarOverflow, []detectEvent, error) {
	var o radarOverflow
	switch {
	case !snap.hasLevel:
		return o, nil, errors.New("get.detect.info has no detectInfo.level")
	case !snap.hasEventNum:
		return o, nil, errors.New("get.detect.info has no detectInfo.eventNum")
	}
	row, ok := radarLevels[snap.level]
	if !ok {
		return o, nil, fmt.Errorf("radar level %d is not in the detect_level table", snap.level)
	}
	o = radarOverflow{level: snap.level, eventNum: snap.eventNum, show: row.show, maxNum: row.max, refreshN: row.refreshN,
		slots: int64(len(snap.events))}
	o.freeSlots = max(o.show-o.slots, 0)
	o.k = o.eventNum + o.refreshN - o.freeSlots - o.maxNum
	if o.k <= 0 {
		return o, nil, nil
	}
	var finished []detectEvent
	for _, ev := range snap.events {
		if ev.uuid != 0 && ev.state == radarStateFinished && ev.typ != 0 && !radarSkippedEvent(ev.eventID) {
			finished = append(finished, ev)
		}
	}
	slices.SortStableFunc(finished, func(a, b detectEvent) int {
		return cmp.Or(cmp.Compare(a.startTime, b.startTime), cmp.Compare(a.uuid, b.uuid))
	})
	return o, finished[:min(int64(len(finished)), o.k)], nil
}

// radarDayEnd is when today's server day ends: the duel entry's et, else init tomorrow's anchor.
func radarDayEnd(in *Init, d *DuelDay) (time.Time, bool) {
	if d != nil && !d.End.IsZero() {
		return d.End, true
	}
	start, ok := in.ServerDayStart(evNow())
	return start.Add(24 * time.Hour), ok
}

// radarTomorrowScores reports whether the next server day's duel scores radar tasks. The week is
// fixed (TB:heroactivity#70000, DUEL.md §1.2) and radar task 90402 scores on Mon, Wed and Fri
// (§5.2), so the day after Base Expansion (Tue), Train Heroes (Thu) or the Sunday rest day does.
// Sunday has no duel entry; it is told by the server day's weekday (the date of the day start,
// 02:00 UTC).
func radarTomorrowScores(conn *session.GameConn, in *Init) bool {
	if d := TodayDuel(conn, in); d != nil {
		return d.Theme == DuelThemeBase || d.Theme == DuelThemeHeroes
	}
	start, ok := in.ServerDayStart(evNow())
	return ok && start.UTC().Weekday() == time.Sunday
}

func runRadarOverflow(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("radar-overflow: no init push; skipping")
		return nil
	}
	if d := TodayDuel(conn, in); d.Scores(DuelScoreRadarTask, "") {
		slog.Info("radar-overflow: today's Alliance Duel scores radar tasks; radar-claims claims them", "theme", d.ThemeName())
		return nil
	}
	snap, err := readDetect(conn)
	if err != nil {
		return err
	}
	plan, picks, err := radarOverflowPlan(snap)
	if err != nil {
		slog.Warn("radar-overflow: can't compute the bank; claiming nothing", "reason", err)
		return nil
	}
	slog.Info("radar-overflow: bank", "level", plan.level, "eventNum", plan.eventNum, "stockCap", plan.maxNum,
		"slots", plan.slots, "showSlots", plan.show, "refreshN", plan.refreshN, "overflow", max(plan.k, 0),
		"nextRefresh", time.UnixMilli(snap.nextRefresh).UTC().Format(time.RFC3339))
	if plan.k <= 0 {
		return nil
	}
	if int64(len(picks)) < plan.k {
		slog.Warn("radar-overflow: fewer finished tasks than the overflow; the rest of the refresh is lost", "overflow", plan.k, "claimable", len(picks))
	}
	errs := claimEach(picks, func(ev detectEvent) error {
		_, err := claimAndLog(conn, fmt.Sprintf("radar overflow event %d", ev.uuid), radarEventCmd, radarUUIDParams(ev.uuid))
		return err
	})
	return errors.Join(errs...)
}
