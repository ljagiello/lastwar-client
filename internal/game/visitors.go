package game

import (
	"errors"
	"fmt"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
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
	Raw *sfs.SFSObject
	// greeted is set by GreetVisitors, in the caller's slice, once the server answered this
	// visitor's visitor.operate without an errorCode: the client then removes the visitor from
	// its queue (DeleteVisitor, CityVisitorManager.lua:505-520), and visitor-fresh, which reads
	// the same init list later in the run, no longer counts it.
	greeted bool
}

func (v Visitor) Uid() int64       { return v.Raw.GetLong("uid") }
func (v Visitor) EventId() int32   { return v.Raw.GetInt("eventId") }
func (v Visitor) VisitorId() int32 { return v.Raw.GetInt("visitorId") }

// StartTime is the visitor's arrival time in unix milliseconds. Before it, visitor.operate answers
// visitor_err_coming (see visitorArrived).
func (v Visitor) StartTime() int64 { return v.Raw.GetLong("startTime") }

// QueueType is the list entry's own `type` field: the client's queue type (Scene/CityVisitor/
// Const.lua:10-24, e.g. 0 GEN_BY_TIME), not the behaviour type GreetVisitors filters on (see
// visitorBehaviourType).
func (v Visitor) QueueType() int32 { return v.Raw.GetInt("type") }

//go:generate go run ./gen_visitor_types -table ${LASTWAR_TABLES}/lw_base_visitor_event.json -version ${LASTWAR_TABLE_VERSION} -out visitor_types_gen.go

// Visitor behaviour types: lw_base_visitor_event.type for the visitor's eventId, which the client
// picks the visitor's class by (global VisitorType, Global/EnumType.lua:11985-12007; dispatch in
// Scene/CityVisitor/CityVisitorManager.lua:108-139). visitorTypeNames has the full list.
const (
	visitorTypeGift          int32 = 2
	visitorTypeRecruitment   int32 = 3
	visitorTypeDominator     int32 = 9
	visitorTypeSeasonDayGift int32 = 10
	visitorTypeActivity      int32 = 11
	visitorTypeSystemGift    int32 = 30
)

var visitorTypeNames = map[int32]string{
	1: "MERCHANT", 2: "GIFT", 3: "RECRUITMENT", 4: "BATTLE", 5: "WORKER_LOTTERY", 6: "NOTIFY",
	7: "OPEN_PANEL", 8: "ALLIANCE_INVITE_MOVE_CITY", 9: "DOMINATOR", 10: "SeasonDayGift",
	11: "VisitorActivity", 12: "ALLIANCE_INVITE", 13: "DOMINATOR_COCKATRICE",
	14: "AllianceCongratulation", 15: "AD_REMINDER", 16: "SKY_BATTLE", 17: "PLANE_FEATURE",
	18: "S0_ALLIANCE_BOSS", 19: "SURVIVOR_PACK_GiFT", 20: "ProtectCoverVisitor", 30: "SystemGift",
}

func visitorTypeName(t int32) string {
	if n, ok := visitorTypeNames[t]; ok {
		return n
	}
	return "unknown"
}

// visitorBehaviourType looks up eventId in the generated lw_base_visitor_event map
// (visitor_types_gen.go). ok is false for an eventId the table does not have.
func visitorBehaviourType(eventId int32) (t int32, ok bool) {
	t, ok = visitorEventTypes[eventId]
	return t, ok
}

// greetableVisitorTypes are the behaviour types GreetVisitors accepts with `operate: 1`: the ones
// whose only accept path in the client grants a reward and spends nothing.
//
//   - GIFT, DOMINATOR, SeasonDayGift use VisitorGift, whose OnConfirmClick sends operate:1 with no
//     cost check (VisitorGift.lua:147-171). Their para is a reward id: GIFT "HQ;rewardId|..."
//     (2001), SeasonDayGift "1;rewardId", DOMINATOR 9001 reward 280000001 (items
//     800006-800008). GIFT and SeasonDayGift are live-validated; DOMINATOR is static-only. The
//     exception is the 11 Season-5 SeasonDayGift "choose a treasure" rows (20703 ... 20746,
//     visitorChoiceEvents): their para is empty and their choose_treasure set, so the plot ends in
//     a choice that sends visitor.season.choose {uid, chooseId} and closes the plot without
//     GF_plot_group_done, and OnConfirmClick never runs (VisitorUnit.lua:271-286, 381-428;
//     LWPlotManager.lua:120-131, 155-167). Which treasure to take is the player's pick, so
//     visitorSkipReason leaves them alone.
//   - RECRUITMENT (VisitorRecruitWorker.lua:62-71, UIWorkerDetailRecruitView.lua:27-38): both
//     buttons of the recruit window send operate:1 and the worker joins at no cost. Live-validated.
//   - SystemGift (VisitorSystemGift.lua:56-75, UICityVistorSystemGiftView.lua:111-190): the window
//     shows one gift item addressed to the player, and its only button sends operate:1.
//     Static-only.
//
// Every other type is skipped and left for the player, never declined with operate:0. Notably:
// MERCHANT, where operate:1 buys para "item;count;resourceType;price" (VisitorMerchant.lua:36-56);
// WORKER_LOTTERY, an invitation to the paid worker lottery that the client only dismisses with
// operate:1 (VisitorWorkerLottery.lua:34-53); SKY_BATTLE and PLANE_FEATURE, VisitorGift too but
// with no reward in the table, whose plot starts a feature guide (lw_guide_flow 5530); and
// SURVIVOR_PACK_GiFT, whose visitors have client-made uids and open a pack popup rather than
// sending visitor.operate (SurvivorPackManager.lua:298-345, 448-457).
var greetableVisitorTypes = map[int32]bool{
	visitorTypeGift:          true,
	visitorTypeRecruitment:   true,
	visitorTypeDominator:     true,
	visitorTypeSeasonDayGift: true,
	visitorTypeSystemGift:    true,
}

// visitorArrivalMargin is how long after its startTime GreetVisitors waits before greeting a
// visitor. init carries no server clock (the login reply's db_utc_timestamp was 0 live), so
// arrival is judged on the host clock, and the margin absorbs a host clock running slightly ahead
// of the server's.
const visitorArrivalMargin = 5 * time.Second

// visitorArrived reports whether v's startTime is at least visitorArrivalMargin before now. The
// client creates, and so lets the player tap, a visitor only once server time reaches startTime
// (VisitorUnit.lua:18-33); a missing startTime counts as arrived.
func visitorArrived(v Visitor, now time.Time) bool {
	return v.StartTime() <= now.Add(-visitorArrivalMargin).UnixMilli()
}

// visitorEventSkipReason is the part of visitorSkipReason that depends on the eventId alone: ""
// when GreetVisitors accepts a visitor of eventId once it has arrived.
func visitorEventSkipReason(eventId int32) (behaviour int32, reason string) {
	t, ok := visitorBehaviourType(eventId)
	switch {
	case !ok:
		return 0, "eventId not in lw_base_visitor_event"
	case !greetableVisitorTypes[t]:
		return t, "behaviour type not on the greet allowlist"
	case visitorChoiceEvents[eventId]:
		return t, "choice visitor, left for the player"
	}
	return t, ""
}

// visitorSkipReason returns why GreetVisitors leaves v alone, or "" when it greets it, plus v's
// behaviour type (0 when the eventId is unknown).
func visitorSkipReason(v Visitor, now time.Time) (behaviour int32, reason string) {
	t, reason := visitorEventSkipReason(v.EventId())
	if reason == "" && !visitorArrived(v, now) {
		reason = "not arrived yet (startTime in the future)"
	}
	return t, reason
}

// MaxVisitorsUpperBound caps the `visitor.list` entries ParseInitVisitors examines, and the
// visitors FetchBuildings and login.go's dedupeVisitors accumulate across pushes. The list is
// server-sent, so nothing in it, maxNum included, can bound it: an attacker-controlled count is no
// enforcement against that attacker (the reasoning buildings.go's maxCollectibleBuildingsPerRun
// gives), and this project treats a hostile -cs-ip peer as in scope. GreetVisitors sends one
// sequential visitor.operate per visitor, each of which can wait out a full defaultCmdTimeout (8s,
// conn.go) against a peer that never answers, so 300 caps that worst case at ~40 minutes instead
// of an open-ended hang. A real list is far shorter: maxNum was 5 live, plus the odd Queue2
// visitor.
const MaxVisitorsUpperBound = 300

// ParseInitVisitors extracts the current visitor list from the bare `init`
// bootstrap push's `visitor.list` field -- a sibling of `building_new` in
// the same payload, see the Visitor doc comment above.
//
// The client keeps every list entry (InitData, CityVisitorManager.lua:341-357). maxNum bounds only
// Queue1, the timed queue visitor.fresh refills (GetNewVisitorInfo, :187-205); Queue2 holds the
// queue types 2, 4, 7, 8, 11 and 19 (Const.lua:25-41), a SystemGift (8) for one, so a real list
// can be longer than maxNum. The list is therefore not cut at maxNum (visitor-fresh applies it to
// the Queue1 count), and a list longer than maxNum is logged at Info.
//
// The scan stops after MaxVisitorsUpperBound RAW items, valid or not (round 26), with a Warn: a
// malformed entry (not an *sfs.SFSObject, or a missing/wrong-typed uid) logs its own Warn, and
// sfs.MaxDecodedNodes=300,000 bounds the raw array, so counting only the valid ones would let a
// hostile peer force ~300,000 scans and warnings.
//
// uid is guarded via requireFieldType, not just requirePresentField (round 28): a present-but-
// wrong-typed uid (e.g. sent as a string) used to pass a presence-only guard and then silently
// coerce to uid=0 via GetLong's own zero-value fallback -- colliding with a genuinely-zero uid, or
// another wrong-typed one, in FetchBuildings' seenVisitorUUIDs and login.go's dedupeVisitors (the
// PRIMARY init-push path), silently dropping one of the two as a spurious "duplicate".
func ParseInitVisitors(initParams *sfs.SFSObject) []Visitor {
	var out []Visitor
	v, ok := initParams.Get("visitor")
	if !ok {
		return out
	}
	visitorObj, ok := v.Val.(*sfs.SFSObject)
	if !ok {
		// Round-39 fix: present-but-wrong-typed used to be silently indistinguishable from
		// genuinely-absent, unlike alliance.go's DonateRecommendedAllianceTech's identical-shape
		// guard on allianceScience, which already warns on this exact anomaly. Diagnostic only --
		// this function still fails safe to an empty result either way.
		slog.Warn("ParseInitVisitors: visitor field is present but not an object", "type", fmt.Sprintf("%T", v.Val))
		return out
	}
	listV, ok := visitorObj.Get("list")
	if !ok {
		// A genuinely-absent list is the normal/expected case here -- not a wrong-type anomaly,
		// so this stays silent by design.
		return out
	}
	arr, ok := listV.Val.(*sfs.SFSArray)
	if !ok {
		// See the visitor-field guard's own comment above for the round-39 rationale.
		slog.Warn("ParseInitVisitors: list field is present but not an array", "type", fmt.Sprintf("%T", listV.Val))
		return out
	}

	// GetInt reads a missing or wrong-typed maxNum as 0; refreshVisitors validates it for the
	// refill rule, so here it only labels the log line.
	if maxNum := visitorObj.GetInt("maxNum"); maxNum > 0 && len(arr.Items()) > int(maxNum) {
		slog.Info("visitor.list is longer than maxNum, which bounds only the timed queue; keeping every entry",
			"listLen", len(arr.Items()), "maxNum", maxNum)
	}
	if len(arr.Items()) > MaxVisitorsUpperBound {
		slog.Warn("visitor.list longer than cap; truncating", "listLen", len(arr.Items()), "cap", MaxVisitorsUpperBound)
	}

	for i, item := range arr.Items() {
		if i >= MaxVisitorsUpperBound {
			break
		}
		vi, ok := item.Val.(*sfs.SFSObject)
		if !ok {
			continue
		}
		if !session.RequireFieldType(vi, "uid", "visitor.list", session.SFSFieldKindLong) {
			continue
		}
		out = append(out, Visitor{Raw: vi})
	}
	return out
}

// GreetVisitors sends `visitor.operate {uid, operate: 1}`, the real client's per-tap accept (see
// the Visitor doc comment above), for each visitor whose behaviour type is on
// greetableVisitorTypes, that is not a choice visitor, and whose startTime has passed. Every other
// visitor, including one with an eventId the generated table does not know, gets one Info log line
// and is left for the player. Each visitor the server answered without an errorCode is marked
// greeted in visitors itself, the slice collectCore shares with Init.Visitors, for visitor-fresh.
func GreetVisitors(conn *session.GameConn, visitors []Visitor) error {
	return greetVisitors(conn, visitors, connNow(conn))
}

func greetVisitors(conn *session.GameConn, visitors []Visitor, now time.Time) error {
	if len(visitors) == 0 {
		slog.Info("no visitors to greet")
		return nil
	}
	var errs []error
	for i, v := range visitors {
		t, skip := visitorSkipReason(v, now)
		if skip != "" {
			slog.Info("skipping visitor", "reason", skip, "uid", v.Uid(), "eventId", v.EventId(),
				"type", t, "typeName", visitorTypeName(t), "visitorId", v.VisitorId(), "startTime", v.StartTime(),
				"queueType", v.QueueType())
			continue
		}
		slog.Info("attempting visitor greet", "uid", v.Uid(), "eventId", v.EventId(), "type", t,
			"typeName", visitorTypeName(t), "visitorId", v.VisitorId(), "queueType", v.QueueType())
		params := sfs.NewSFSObject()
		params.PutLong("uid", v.Uid())
		params.PutInt("operate", 1)
		resp, err := session.SendAndWait(conn, fmt.Sprintf("visitor greet response (uid %d)", v.Uid()), "visitor.operate", params)
		errs = append(errs, err)
		// A benign visitor_err_coming also returns a nil error, but the visitor stays.
		if err == nil && resp != nil && !resp.Params.Has("errorCode") {
			visitors[i].greeted = true
		}
		// A net.Error here means the underlying TCP connection itself is known-dead ONLY when
		// Timeout()==false -- an ordinary per-visitor sendAndWait timeout (no response within
		// defaultCmdTimeout) is ITSELF a net.Error with Timeout()==true, on an otherwise perfectly
		// healthy connection, and must NOT abort the remaining visitors: it's just this one
		// visitor's response being slow, no different from a decoded errorCode failure, so it falls
		// through and stays in errs like any other error. Only a genuine non-timeout net.Error
		// (connection reset, broken pipe, etc.) means every remaining visitor in this loop is
		// already doomed to independently burn a full defaultCmdTimeout before failing the exact
		// same way -- same root cause as CollectAll's own net.Error early-abort (buildings.go),
		// mirrored here. The blast radius is bounded either way: `visitors` comes from
		// ParseInitVisitors, which examines at most MaxVisitorsUpperBound `visitor.list` entries
		// (see that constant's doc comment), so this loop's own worst case is bounded, not an
		// open-ended list.
		var netErr net.Error
		if errors.As(err, &netErr) && !netErr.Timeout() {
			break
		}
	}
	return errors.Join(errs...)
}
