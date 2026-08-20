package main

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
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

// StartTime has no caller today, but it's kept: it's the field that
// distinguishes a still-arriving visitor (visitor_err_coming, see the
// Visitor doc comment above) from one actually greetable, so a future
// GreetVisitors could use it to skip a doomed operate call instead of
// discovering "not started yet" from the server's error response.
func (v Visitor) StartTime() int64 { return v.Raw.GetLong("startTime") }

// maxVisitorsDefensiveCeiling is the fallback cap ParseInitVisitors applies to `visitor.list` when
// the init push's own sibling `maxNum` field (see the Visitor doc comment above -- the real push
// carries `visitor={addNum, maxNum, list}`) is absent, unparseable, or nonsensical (<= 0). This
// closes the same unbounded-hang threat model buildings.go's capDeadline (round 20) closed for wait
// DURATION, just for item COUNT instead: GreetVisitors issues one sequential `visitor.operate`
// network call per parsed visitor, and each can cost up to a full defaultCmdTimeout (8s, conn.go)
// against a peer that simply never responds -- this project's own threat model already treats an
// arbitrary/hostile -cs-ip peer as in-scope (see capDeadline's own doc comment, round 16's
// redactSFSValue fail-open fix). Set well above the live-confirmed maxNum=5 (see the Visitor doc
// comment above) as a defensive margin, not hardcoded to exactly 5, since the real live value could
// legitimately vary a bit across accounts/game versions -- but still small enough to keep
// GreetVisitors' worst-case sequential wall-clock cost bounded to a couple minutes, not an
// open-ended hang.
const maxVisitorsDefensiveCeiling = 25

// maxVisitorsUpperBound is a hardcoded, non-server-trusting upper sanity bound on the init push's
// own `visitor.maxNum` field (see the Visitor doc comment above). Before this existed,
// ParseInitVisitors trusted maxNum verbatim as long as it was > 0, which let a hostile or
// misbehaving -cs-ip peer simply set maxNum to an enormous value -- paired with a matching number of
// visitor.list entries, well within sfsobject.go's maxDecodedNodes=300,000 decode budget -- and
// completely defeat maxVisitorsDefensiveCeiling's own protection. That was the wrong model to begin
// with: an attacker-controlled field is not itself an enforcement mechanism against that same
// attacker, precisely the reasoning buildings.go's maxCollectibleBuildingsPerRun doc comment gives
// for never deferring to a server-sent count at all. This bound closes that gap while still trusting
// maxNum for its intended purpose: set well above both the live-confirmed maxNum=5 and
// maxVisitorsDefensiveCeiling=25 -- generously large, in the same spirit as
// maxCollectibleBuildingsPerRun=300, so a legitimately bigger real maxNum from a future game update
// wouldn't be needlessly clamped -- while still finite, so GreetVisitors' worst-case sequential
// runtime against a peer that never responds stays bounded (300 * defaultCmdTimeout (8s, conn.go)
// caps the worst case at ~40 minutes instead of an open-ended hang).
const maxVisitorsUpperBound = 300

// ParseInitVisitors extracts the current visitor list from the bare `init`
// bootstrap push's `visitor.list` field -- a sibling of `building_new` in
// the same payload, see the Visitor doc comment above.
//
// The list is capped at the init push's own `maxNum` field when present and sane (> 0), itself
// clamped to maxVisitorsUpperBound (see that constant's doc comment -- maxNum is server-supplied and
// therefore not trustworthy as an enforcement mechanism on its own), falling back to
// maxVisitorsDefensiveCeiling when maxNum is absent, unparseable, or <= 0. A maxNum exceeding
// maxVisitorsUpperBound is itself logged as a Warn before being clamped, and a server-sent list
// longer than the cap actually applied is separately logged as a Warn too: both are signals worth
// knowing about (a misbehaving/hostile peer, or this client's live-confirmed maxNum assumption
// having drifted), not something to silently truncate away.
//
// The cap bounds the number of RAW `visitor.list` items examined (round 26), not merely the number
// of valid ones appended to the returned slice. Before round 26, the loop below only stopped once
// len(out) reached limit -- so a malformed entry (not an *SFSObject, or missing/wrong-typed the
// required "uid" field via requireFieldType) hit a `continue` that didn't count against the cap at
// all, since it never reached the append. requireFieldType itself logs a Warn per malformed entry,
// and the raw `arr.items` slice is bounded only by sfsobject.go's much larger
// maxDecodedNodes=300,000 decode budget -- so a hostile peer could pad visitor.list with up to
// ~300,000 minimal malformed entries (e.g. objects with no "uid" field) and force this function to
// scan and log-warn on every single one regardless of how small `limit` was configured, even though
// the returned slice (and therefore GreetVisitors' own worst-case cost, see
// maxVisitorsDefensiveCeiling's doc comment above) really was still capped. There's no legitimate
// reason to look at more raw items than the output cap already enforces, so the loop below now stops
// after examining `limit` items total, valid or not.
//
// uid is guarded via requireFieldType, not just requirePresentField (round 28): a present-but-
// wrong-typed uid (e.g. sent as a string) used to pass a presence-only guard and then silently
// coerce to uid=0 via GetLong's own zero-value fallback -- colliding with a genuinely-zero uid, or
// another wrong-typed one, in FetchBuildings' seenVisitorUUIDs and login.go's dedupeVisitors (the
// PRIMARY init-push path), silently dropping one of the two as a spurious "duplicate".
func ParseInitVisitors(initParams *SFSObject) []Visitor {
	var out []Visitor
	v, ok := initParams.Get("visitor")
	if !ok {
		return out
	}
	visitorObj, ok := v.Val.(*SFSObject)
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
		// A genuinely-absent list is the normal/expected case here (mirrors maxNum's own
		// presence-checked-separately-from-type reasoning just below) -- not a wrong-type anomaly,
		// so this stays silent by design.
		return out
	}
	arr, ok := listV.Val.(*SFSArray)
	if !ok {
		// See the visitor-field guard's own comment above for the round-39 rationale.
		slog.Warn("ParseInitVisitors: list field is present but not an array", "type", fmt.Sprintf("%T", listV.Val))
		return out
	}

	limit := maxVisitorsDefensiveCeiling
	// maxNum's presence is checked separately from its type (round 29 audit), unlike the sibling
	// uid guard below (requireFieldType, which conflates the two): maxNum being genuinely ABSENT is
	// this function's own documented, expected fallback path (see this function's doc comment
	// above) -- not a malformed-entry signal worth a Warn -- so only a PRESENT-but-wrong-typed
	// maxNum (e.g. sent as a string) gets a diagnostic Warn here. Before this guard, a wrong-typed
	// maxNum silently coerced to maxNum=0 via GetInt's own zero-value fallback, which is <= 0 and so
	// was already indistinguishable from -- and fell back exactly like -- a genuinely-absent field:
	// fail-safe (the more conservative maxVisitorsDefensiveCeiling always wins over trusting a
	// wrong-typed value), but with zero diagnostic signal that the field was actually malformed
	// rather than simply omitted. See TestParseInitVisitorsWrongTypedMaxNumFallsBackWithWarning.
	//
	// Round 33 fix: a present, CORRECTLY-typed maxNum that's <= 0 (most notably negative -- 0
	// could at least plausibly mean a feature-locked account, but a visitor-capacity field can
	// never legitimately be negative) used to fall through this whole if/else-if chain with zero
	// diagnostic, identical treatment to the genuinely-and-expectedly-absent case that's silent by
	// design -- the exact same "still-uncovered anomaly branch" bug shape flagged repeatedly for
	// gsl.go's getIntFlexible in rounds 30-32. Still fails safe either way (limit stays at
	// maxVisitorsDefensiveCeiling), so this is diagnostic-only, but a negative maxNum is a genuine
	// malformed/hostile-peer signal now worth surfacing like its wrong-typed and too-large
	// siblings already are.
	if maxNumV, ok := visitorObj.Get("maxNum"); ok && maxNumV.Val != nil {
		// Round-35 fix: a present, CORRECTLY-typed int64 Long whose value is out of int32's range
		// (e.g. maxNum=5000000000) used to pass the sfsFieldKindAccepts type check below (a pure
		// Go-type check, by design -- see GetInt's own doc comment for why value-range awareness
		// was deliberately kept out of it), then silently degrade to 0 via GetInt's own int64
		// range-clamp -- landing in the "not positive" branch below with the real value already
		// lost, actively MISCHARACTERIZING a huge out-of-range Long as merely non-positive rather
		// than reporting what it actually was. This is the exact fourth anomaly shape gsl.go's
		// getIntFlexible was hardened against in round 33 (checking v.Val.(int64) directly against
		// math.MinInt32/MaxInt32 before ever calling GetInt), backported here. Checked before the
		// GetInt call below so the real n64 value is still available to log.
		if n64, isInt64 := maxNumV.Val.(int64); isInt64 && (n64 < math.MinInt32 || n64 > math.MaxInt32) {
			slog.Warn("visitor.maxNum field is present as an out-of-int32-range Long; falling back to defensive ceiling",
				"maxNum", n64, "cap", maxVisitorsDefensiveCeiling)
		} else if !sfsFieldKindAccepts(sfsFieldKindInt, maxNumV.Val) {
			slog.Warn("visitor.maxNum field is present but wrong-typed; falling back to defensive ceiling",
				"raw", visitorObj.StringRedacted(), "goType", fmt.Sprintf("%T", maxNumV.Val), "cap", maxVisitorsDefensiveCeiling)
		} else if maxNum := visitorObj.GetInt("maxNum"); maxNum > 0 {
			limit = int(maxNum)
			if limit > maxVisitorsUpperBound {
				slog.Warn("visitor.maxNum exceeds upper sanity bound; clamping",
					"maxNum", maxNum, "cap", maxVisitorsUpperBound)
				limit = maxVisitorsUpperBound
			}
		} else {
			slog.Warn("visitor.maxNum field is present and correctly-typed but not positive; falling back to defensive ceiling",
				"maxNum", maxNum, "cap", maxVisitorsDefensiveCeiling)
		}
	}
	if len(arr.items) > limit {
		slog.Warn("visitor.list longer than cap; truncating", "listLen", len(arr.items), "cap", limit)
	}

	for i, item := range arr.items {
		if i >= limit {
			break
		}
		vi, ok := item.Val.(*SFSObject)
		if !ok {
			continue
		}
		if !requireFieldType(vi, "uid", "visitor.list", sfsFieldKindLong) {
			continue
		}
		out = append(out, Visitor{Raw: vi})
	}
	return out
}

// GreetVisitors sends `visitor.operate {uid, operate: 1}` for every
// currently-present visitor, matching the real client's own captured
// per-tap behavior (see the Visitor doc comment above).
func GreetVisitors(conn *GameConn, visitors []Visitor) error {
	if len(visitors) == 0 {
		slog.Info("no visitors to greet")
		return nil
	}
	var errs []error
	for _, v := range visitors {
		slog.Info("attempting visitor greet", "uid", v.Uid(), "eventId", v.EventId(), "visitorId", v.VisitorId())
		params := NewSFSObject()
		params.PutLong("uid", v.Uid())
		params.PutInt("operate", 1)
		_, err := sendAndWait(conn, fmt.Sprintf("visitor greet response (uid %d)", v.Uid()), "visitor.operate", params)
		errs = append(errs, err)
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
		// ParseInitVisitors, which caps `visitor.list` at the init push's own `maxNum` field (seen
		// live as 5) or maxVisitorsDefensiveCeiling otherwise (see ParseInitVisitors' doc comment),
		// so this loop's own worst case is bounded, not an open-ended list.
		var netErr net.Error
		if errors.As(err, &netErr) && !netErr.Timeout() {
			break
		}
	}
	return errors.Join(errs...)
}
