package game

import (
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func init() {
	registerFeature(Feature{
		Name:    "activity-visitors",
		Summary: "claim event visitors' once-per-period reward (visitor.receive.reward)",
		Run:     runActivityVisitors,
	})
}

// maxActivityVisitorsPerRun bounds the claims one run sends, since each can wait out
// session.DefaultCmdTimeout against a peer that never answers. Event visitors come one per
// running event, so a real init stays far below it.
const maxActivityVisitorsPerRun = 20

// runActivityVisitors claims the reward of each event ("activity") visitor. These visitors are not
// in init.visitor.list, so GreetVisitors never sees them: init.activityVisitor maps an activityId
// to {sTime, eTime, rewardState, lpTime, eventId, nextResetTime} (InitMessage.lua:471,
// ActivityVisitorData.lua:23-32), and push.activity.visitor refreshes it.
//
// The client shows the visitor while rewardState == 0 and server time (s) < eTime
// (ActivityVisitorData.lua:64-70), and tapping it sends visitor.receive.reward {activityId:Int}
// (VisitorActivity.lua:40-52). The reply sets rewardState = 1 until nextResetTime, when the
// visitor comes back (ActivityVisitorData.lua:33-53). The client only gets there through the VisitorActivity class, so an entry
// whose eventId's behaviour type is not VisitorActivity (11) is skipped. Static-only: no live
// activityVisitor sample and no already-claimed errorCode have been seen.
func runActivityVisitors(conn *session.GameConn, in *Init) error {
	return claimActivityVisitors(conn, in, time.Now())
}

func claimActivityVisitors(conn *session.GameConn, in *Init, now time.Time) error {
	if in == nil || in.Raw == nil {
		slog.Info("activity-visitors: no init push; skipping")
		return nil
	}
	visitors := in.Object("activityVisitor")
	if visitors == nil {
		slog.Info("activity-visitors: init has no activityVisitor; nothing to claim")
		return nil
	}
	var errs []error
	sent := 0
	for _, key := range visitors.Keys() {
		v, _ := visitors.Get(key)
		entry, ok := v.Val.(*sfs.SFSObject)
		if !ok {
			slog.Warn("activity-visitors: entry is not an object; skipping", "activityId", key, "type", fmt.Sprintf("%T", v.Val))
			continue
		}
		activityId, err := strconv.ParseInt(key, 10, 32)
		if err != nil || activityId <= 0 {
			slog.Warn("activity-visitors: key is not an activity id; skipping", "key", key)
			continue
		}
		if !session.RequireFieldType(entry, "rewardState", "activityVisitor", session.SFSFieldKindInt) ||
			!session.RequireFieldType(entry, "eTime", "activityVisitor", session.SFSFieldKindLong) {
			continue
		}
		eventId := entry.GetInt("eventId")
		t, known := visitorBehaviourType(eventId)
		switch {
		case entry.GetInt("rewardState") != 0:
			slog.Info("activity visitor already claimed this period", "activityId", activityId, "eventId", eventId,
				"nextResetTime", entry.GetLong("nextResetTime"))
			continue
		case now.Unix() >= entry.GetLong("eTime"):
			slog.Info("activity visitor's event has ended", "activityId", activityId, "eventId", eventId, "eTime", entry.GetLong("eTime"))
			continue
		case !known || t != visitorTypeActivity:
			slog.Info("skipping activity visitor: eventId is not a VisitorActivity visitor", "activityId", activityId,
				"eventId", eventId, "type", t, "typeName", visitorTypeName(t))
			continue
		}
		if sent == maxActivityVisitorsPerRun {
			slog.Warn("activity-visitors: per-run cap reached; leaving the rest for the next run", "cap", maxActivityVisitorsPerRun)
			break
		}
		sent++
		params := sfs.NewSFSObject()
		params.PutInt("activityId", int32(activityId))
		_, err = session.SendAndWait(conn, fmt.Sprintf("activity visitor reward (activityId %d)", activityId), "visitor.receive.reward", params)
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			break
		}
	}
	return errors.Join(errs...)
}
