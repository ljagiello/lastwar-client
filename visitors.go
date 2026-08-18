package main

import (
	"log/slog"
	"time"
)

// Visitor is a city visitor NPC ("greet visitors") -- confirmed live via a
// real packet capture of the actual game client (not a guess). Visitors
// arrive in your city on their own (up to `maxNum`, seen live as 5) and
// show up as `?`-bubble characters standing near the base. They're
// delivered as part of the same bare `init` bootstrap push that carries
// `building_new` -- no separate "list visitors" request exists or is
// needed -- under a sibling top-level field:
//
//	visitor={addNum=0, maxNum=5, list=[
//	  {uid=1389862050553073568, eventId=2005, startTime=..., type=0, extendInfo=, visitorId=6},
//	  {uid=1389862047206018852, eventId=2002, startTime=..., type=0, extendInfo=, visitorId=6},
//	  {uid=1389862047533174570, eventId=2001, startTime=..., type=0, extendInfo=, visitorId=6},
//	]}
//
// Greeting one is `visitor.operate {uid, operate: 1}` -- confirmed live via
// the real client's own captured traffic (two calls, one per visitor
// tapped, `operate: 1` in both). The response to those two specific calls
// fell outside the successfully-decoded portion of that capture (a stream
// reassembly artifact past a certain point, same class of issue seen
// before with the Truck Rewards capture), so a successful-greet reward
// payload has still not been directly observed.
//
// What IS confirmed live, through this Go client: sending the identical
// request shape against the one visitor the real client's own session
// left ungreeted (the newest-arrived of the three, `eventId=2005`) got a
// real, well-formed business-logic response --
// `errorCode=visitor_err_coming, errorMsg="visitor not started"` -- not a
// protocol error or an "unknown command." That both confirms
// `visitor.operate` end to end (parse from `init`, build the request,
// round-trip it, decode a real typed response) and explains why the real
// user greeted only 2 of the 3 present visitors: this one's `startTime`
// was the most recent of the three, and it was apparently still "coming"
// (an arrival delay/animation window) rather than actually greetable yet.
// Still open: a genuinely successful greet's reward payload shape.
type Visitor struct {
	Raw *SFSObject
}

func (v Visitor) Uid() int64       { return v.Raw.GetLong("uid") }
func (v Visitor) EventId() int32   { return v.Raw.GetInt("eventId") }
func (v Visitor) VisitorId() int32 { return v.Raw.GetInt("visitorId") }
func (v Visitor) StartTime() int64 { return v.Raw.GetLong("startTime") }

// ParseInitVisitors extracts the current visitor list from the bare `init`
// bootstrap push's `visitor.list` field -- a sibling of `building_new` in
// the same payload, see the Visitor doc comment above.
func ParseInitVisitors(initParams *SFSObject) []Visitor {
	var out []Visitor
	v, ok := initParams.Get("visitor")
	if !ok {
		return out
	}
	visitorObj, ok := v.Val.(*SFSObject)
	if !ok {
		return out
	}
	listV, ok := visitorObj.Get("list")
	if !ok {
		return out
	}
	arr, ok := listV.Val.(*SFSArray)
	if !ok {
		return out
	}
	for _, item := range arr.items {
		if vi, ok := item.Val.(*SFSObject); ok {
			out = append(out, Visitor{Raw: vi})
		}
	}
	return out
}

// GreetVisitors sends `visitor.operate {uid, operate: 1}` for every
// currently-present visitor, matching the real client's own captured
// per-tap behavior (see the Visitor doc comment above).
func GreetVisitors(conn *GameConn, visitors []Visitor) {
	if len(visitors) == 0 {
		slog.Info("no visitors to greet")
		return
	}
	for _, v := range visitors {
		slog.Info("attempting visitor greet", "uid", v.Uid(), "eventId", v.EventId(), "visitorId", v.VisitorId())
		params := NewSFSObject()
		params.PutLong("uid", v.Uid())
		params.PutInt("operate", 1)
		if err := conn.SendExtension("visitor.operate", params); err != nil {
			slog.Error("visitor greet send failed", "error", err)
			continue
		}
		msg, err := waitForCmd(conn, 8*time.Second, "visitor.operate")
		if err != nil {
			slog.Error("visitor greet no response", "error", err)
			continue
		}
		slog.Info("visitor greet response", "response", msg.Params.String())
	}
}
