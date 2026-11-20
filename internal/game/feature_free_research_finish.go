package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync/atomic"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Free research finish (MASTER G37; cov-dc-social-city/F10, confirmed). The client flags a working
// research queue (type 6, NewQueueType.Science, EnumType.lua:3221) once its remaining time is within
// the research free time: `v.endTime > 0 and v.endTime ~= LongMaxValue and v.endTime - curTime <=
// effectTime * 1000`, effectTime = GetGameEffect(RESEARCH_TIME_REDUCE 30100) (QueueDataManager.lua:
// 572-590; EnumType.lua:4355). An end of LongMaxValue (ConstDefine.lua:147), which the client skips,
// is never inside the window here either. The Tech Center bubble
// HeroFreeScienceAddTime then shows (BuildBubbleManager.lua:1298-1313, once the building is in
// view, :5435-5443), and its tap sends `queue.ccd.m.new {qUUID: Long, itemIDs: ""}`
// (BuildBubbleManager.lua:4123-4128). QueueCcdMNewMessage.lua:7-10 puts qUUID and itemIDs only:
// isGold is never put, and useGold only when set.
//
// GetGameEffect(30100) is a sum of which init effect carries only the base, so init
// effect["30100"] can only under-state the window. This sends nothing when it is absent, not a
// finite number, or not above 0. It also keeps freeResearchFinishMarginMs inside the window: the session
// clock is only as exact as the pong it was synced from. The real client has no such margin; it is
// a choice made here. The free finish only ends the timer: the research is completed by
// queue.finish, which finished-timers sends on a later run.
//
// Held like finished-timers' research (the user's decision of 2026-11-19, "and the free research
// finish"). While the account is in a running duel cycle whose day doesn't score research power
// (type 6), nothing is sent. With no duel, or a duel that can't be read, it goes ahead
// (completionHold).
//
// The request carries no cost, but whether an empty itemIDs can be charged in diamonds is an open
// question (docs/alliance-duel.mdx). So init user.gold must be present, and every reply's gold
// balance (remainGold / gold, top level or under queue; QueueCcsMNewHandle, QueueDataManager.lua:
// 495-525) must not fall below it. A fall is logged at Error, persisted (tripwire.go) and stops
// the feature. It refuses to run while the tripwire file exists. Any errorCode is logged and stops
// the run. That includes E100173 and 120021, which duel-speedups registers as benign for this
// command; this feature registers none. Each reply is logged at Info. Static-only, not validated
// live.
const (
	freeResearchFinishName     = "free-research-finish"
	freeResearchFinishCmd      = "queue.ccd.m.new"
	freeResearchFinishEffect   = "30100" // RESEARCH_TIME_REDUCE (EnumType.lua:4355), seconds
	freeResearchFinishMarginMs = 30_000
)

// freeResearchFinishTripped stops the feature for the rest of the process after a gold drop.
var freeResearchFinishTripped atomic.Bool

func init() {
	registerFeature(Feature{
		Name: freeResearchFinishName,
		Summary: "OPT-IN: queue.ccd.m.new {itemIDs: \"\"} on a running research whose remaining time is inside the " +
			"free research time (init effect 30100); held like finished-timers' research for an Alliance Duel day " +
			"that scores research power; a gold drop stops it (static-only, not validated live)",
		DefaultOn: false,
		Run:       runFreeResearchFinish,
	})
}

// freeResearchFinishWindowMs is init effect["30100"] in ms; ok is false when it is absent, not a
// number, or not above 0.
func freeResearchFinishWindowMs(in *Init) (float64, bool) {
	v, has := in.Object("effect").Get(freeResearchFinishEffect)
	if !has {
		return 0, false
	}
	var sec float64
	switch n := v.Val.(type) {
	case float64:
		sec = n
	case float32:
		sec = float64(n)
	default:
		i, ok := claimIntValue(n)
		if !ok {
			return 0, false
		}
		sec = float64(i)
	}
	if math.IsNaN(sec) || math.IsInf(sec, 0) || sec <= 0 {
		return 0, false
	}
	return sec * 1000, true
}

func runFreeResearchFinish(conn *session.GameConn, in *Init) error {
	if freeResearchFinishTripped.Load() {
		return errors.New(freeResearchFinishName + ": stopped for this process after a gold drop")
	}
	if err := tripwireErr(freeResearchFinishName); err != nil {
		return err
	}
	if in == nil || in.Raw == nil {
		slog.Info(freeResearchFinishName + ": no init push; skipping")
		return nil
	}
	window, ok := freeResearchFinishWindowMs(in)
	if !ok {
		slog.Info(freeResearchFinishName+": no research free time (init effect 30100 absent or not above 0); nothing to do",
			"effectPresent", in.Object("effect").Has(freeResearchFinishEffect))
		return nil
	}
	now := claimNow(in).UnixMilli()
	var due []finishedTimersQueue
	for _, q := range finishedTimersQueues(in) {
		if q.Type != finishedTimersQueueResearch || !q.Working(now) {
			continue
		}
		left := q.End - now
		if float64(left) > window-freeResearchFinishMarginMs {
			slog.Info(freeResearchFinishName+": research not yet inside the free time", "queue", q.UUID,
				"remainingSec", left/1000, "freeSec", int64(window)/1000)
			continue
		}
		due = append(due, q)
	}
	if len(due) == 0 {
		slog.Info(freeResearchFinishName + ": no running research inside the free time")
		return nil
	}
	hold := newCompletionHold(freeResearchFinishName, conn, in)
	if hold.held(finishedTimersDuelSciencePower) {
		for _, q := range due {
			slog.Info(freeResearchFinishName+": holding the free research finish for an Alliance Duel day that scores research power",
				"queue", q.UUID, "today", hold.today(), "waitsFor", completionHoldWaitsFor(finishedTimersDuelSciencePower))
		}
		return nil
	}
	gold, ok := claimInt(in.Object("user"), "gold")
	if !ok {
		slog.Warn(freeResearchFinishName + ": init has no user.gold, so the gold check cannot be armed; nothing sent")
		return nil
	}
	for _, q := range due {
		p := sfs.NewSFSObject()
		p.PutLong("qUUID", q.UUID)
		p.PutUtfString("itemIDs", "")
		label := fmt.Sprintf("free research finish queue %d", q.UUID)
		msg, err := session.SendAndWait(conn, label, freeResearchFinishCmd, p)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if msg.Params.Has("errorCode") {
			code, _ := msg.Params.Get("errorCode")
			slog.Warn(freeResearchFinishName+": the server refused the free finish; stopping", "queue", q.UUID, "errorCode", code.Val)
			return fmt.Errorf("%s: errorCode=%v", label, code.Val)
		}
		queue := evObject(msg.Params, "queue")
		end, endOK := claimFirstInt(queue, "updateTime", "uT")
		slog.Info(freeResearchFinishName+": reply", "queue", q.UUID, "replyQueue", queue != nil, "uT", end, "uTPresent", endOK,
			"endedNow", endOK && end <= claimNow(in).UnixMilli(), "itemCostArr", queue.Has("itemCostArr"), "keys", msg.Params.Keys())
		if err := freeResearchFinishCheckGold(msg.Params, gold); err != nil {
			freeResearchFinishTripped.Store(true)
			slog.Error("DIAMOND TRIPWIRE: "+freeResearchFinishName+" saw the diamond balance fall; stopping",
				"queue", q.UUID, "error", err)
			tripwireTrip(freeResearchFinishName, freeResearchFinishCmd, err.Error())
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	return nil
}

// freeResearchFinishCheckGold fails when a gold balance in the reply (remainGold or gold, at the
// top level or under queue) is below the init balance.
func freeResearchFinishCheckGold(reply *sfs.SFSObject, gold int64) error {
	for _, h := range []*sfs.SFSObject{reply, evObject(reply, "queue")} {
		if h == nil {
			continue
		}
		for _, key := range []string{"remainGold", "gold"} {
			v, ok := h.Get(key)
			if !ok {
				continue
			}
			g, ok := claimIntValue(v.Val)
			if !ok {
				return fmt.Errorf("reply %s is not a number (%v)", key, v.Val)
			}
			if g < gold {
				return fmt.Errorf("diamonds dropped from %d to %d (reply %s)", gold, g, key)
			}
		}
	}
	return nil
}
