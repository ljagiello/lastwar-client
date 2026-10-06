package game

import (
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func init() {
	registerFeature(Feature{
		Name:    "visitor-fresh",
		Summary: "ask the server for the next city visitor when the queue has room (visitor.fresh)",
		Run:     runVisitorFresh,
	})
}

// visitorQueue1Types are the queue types (Visitor.QueueType, Scene/CityVisitor/Const.lua:10-41)
// the client keeps in Queue1, the queue visitor.fresh refills.
var visitorQueue1Types = map[int32]bool{0: true, 1: true, 5: true, 6: true, 12: true, 18: true, 20: true}

// effectVisitorEventFunctionOpen is VISITOR_EVENT_FUNCTION_OPEN (Global/EnumType.lua:4571).
const effectVisitorEventFunctionOpen = "90004"

// runVisitorFresh sends `visitor.fresh {}`, which asks the server to schedule the next timed
// visitor, under the client's own rule (GetNewVisitorInfo, CityVisitorManager.lua:172-190, run
// on entering the game and after every visitor leaves):
//   - init.visitor.maxNum is present;
//   - fewer than maxNum visitors are in Queue1;
//   - the newest Queue1 visitor's startTime has passed (an empty queue passes);
//   - effect 90004 >= 1.
//
// The effect is the sum of init.effect and alliance, server, season and status sources init
// does not carry (EffectData.lua:196-217). So a value in init.effect below 1 skips, but a missing
// entry is logged and the other three conditions decide.
//
// in.Visitors is the init list, which predates the greets the core actions send in the same
// -collect run. The queue looks fuller and its newest startTime no older than it is, so this can
// only skip a refill the client would send, never send one it would not; the next run catches
// up. Under -run nothing was greeted and the list is current.
//
// The reply's addVisitor arrives at its own startTime (lw_base_visitor_config slots are 300 to
// 4,800 s), so it is logged, not greeted; the next run's GreetVisitors applies the allowlist to
// it. Static-only.
func runVisitorFresh(conn *session.GameConn, in *Init) error {
	return refreshVisitors(conn, in, time.Now())
}

func refreshVisitors(conn *session.GameConn, in *Init, now time.Time) error {
	if in == nil || in.Raw == nil {
		slog.Info("visitor-fresh: no init push; skipping")
		return nil
	}
	visitor := in.Object("visitor")
	m, ok := visitor.Get("maxNum")
	if !ok || !session.SFSFieldKindAccepts(session.SFSFieldKindInt, m.Val) || visitor.GetInt("maxNum") <= 0 {
		slog.Info("visitor-fresh: init has no usable visitor.maxNum; skipping (the client does the same)")
		return nil
	}
	maxNum := int(visitor.GetInt("maxNum"))
	queued := 0
	var newest int64
	for _, v := range in.Visitors {
		if visitorQueue1Types[v.QueueType()] {
			queued++
			newest = max(newest, v.StartTime())
		}
	}
	if queued >= maxNum {
		slog.Info("visitor-fresh: visitor queue is full; skipping", "queued", queued, "maxNum", maxNum)
		return nil
	}
	if queued > 0 && newest > now.Add(-visitorArrivalMargin).UnixMilli() {
		slog.Info("visitor-fresh: newest visitor has not arrived yet; skipping", "queued", queued, "newestStartTime", newest)
		return nil
	}
	if open, ok := initEffect(in, effectVisitorEventFunctionOpen); !ok {
		slog.Info("visitor-fresh: effect 90004 not in init.effect; deciding on the other conditions")
	} else if open < 1 {
		slog.Info("visitor-fresh: visitor function not open (effect 90004 < 1); skipping", "effect", open)
		return nil
	}

	resp, err := session.SendAndWait(conn, "visitor fresh", "visitor.fresh", sfs.NewSFSObject())
	if err != nil || resp == nil {
		return err
	}
	if v, ok := resp.Params.Get("addVisitor"); ok {
		if o, ok := v.Val.(*sfs.SFSObject); ok {
			nv := Visitor{Raw: o}
			t, known := visitorBehaviourType(nv.EventId())
			slog.Info("visitor-fresh: next visitor scheduled", "uid", nv.Uid(), "eventId", nv.EventId(), "type", t,
				"typeName", visitorTypeName(t), "greetable", known && greetableVisitorTypes[t], "startTime", nv.StartTime())
		}
	}
	return nil
}

// initEffect reads init.effect[id] (an object keyed by effect id, EffectData.lua:51-55,
// 254-283) as a number. ok is false when init has no effect object, no entry for id, or a
// non-numeric one.
func initEffect(in *Init, id string) (float64, bool) {
	v, ok := in.Object("effect").Get(id)
	if !ok {
		return 0, false
	}
	switch n := v.Val.(type) {
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case int16:
		return float64(n), true
	case byte:
		return float64(n), true
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case string:
		f, err := strconv.ParseFloat(n, 64)
		return f, err == nil
	}
	slog.Warn("init.effect entry is not a number", "id", id, "type", fmt.Sprintf("%T", v.Val))
	return 0, false
}
