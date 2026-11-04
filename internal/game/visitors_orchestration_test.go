package game

import (
	"bytes"
	"errors"
	"io"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"log/slog"
	"math"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestVisitorStartTime pins down that Visitor.StartTime() (visitors.go) reads the "startTime" key
// via GetLong. GreetVisitors' arrival check (visitorArrived) and visitor-fresh both depend on it;
// NewTestVisitor fixtures carry no startTime, which reads as already arrived.
func TestVisitorStartTime(t *testing.T) {
	raw := sfs.NewSFSObject()
	raw.PutLong("startTime", 1234567890)
	v := Visitor{Raw: raw}

	if got := v.StartTime(); got != 1234567890 {
		t.Errorf("StartTime() = %d, want %d", got, 1234567890)
	}
}

// TestGreetVisitorsEmpty checks the len(visitors)==0 short-circuit: GreetVisitors must return nil
// without sending anything. No fake server goroutine is started here at all -- if the
// short-circuit were ever removed, GreetVisitors would block on sendAndWait's 8s timeout waiting
// for a reply nobody sends, and this test would time out instead of silently passing.
func TestGreetVisitorsEmpty(t *testing.T) {
	client, _ := session.NewPipeGameConnPair(t)

	if err := GreetVisitors(client, nil); err != nil {
		t.Errorf("GreetVisitors(nil) = %v, want nil", err)
	}
}

// TestGreetVisitorsSendsOnePerVisitor checks the success path: GreetVisitors issues one
// `visitor.operate {uid, operate: 1}` per visitor, in order, and returns nil once every one of
// them gets a real success response.
func TestGreetVisitorsSendsOnePerVisitor(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	visitors := []Visitor{
		NewTestVisitor(1001, 2001, 6),
		NewTestVisitor(1002, 2002, 6),
	}

	var gotUids []int64
	var gotOperates []int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range visitors {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			if msg.Cmd != "visitor.operate" {
				t.Errorf("Cmd = %q, want visitor.operate", msg.Cmd)
			}
			gotUids = append(gotUids, msg.Params.GetLong("uid"))
			gotOperates = append(gotOperates, msg.Params.GetInt("operate"))
			resp := sfs.NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendExtension(msg.Cmd, resp)
		}
	}()

	err := GreetVisitors(client, visitors)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading both requests")
	}

	if err != nil {
		t.Fatalf("GreetVisitors() = %v, want nil", err)
	}
	if len(gotUids) != 2 || gotUids[0] != 1001 || gotUids[1] != 1002 {
		t.Errorf("got uids %v, want [1001 1002] in order", gotUids)
	}
	for _, op := range gotOperates {
		if op != 1 {
			t.Errorf("operate = %d, want 1", op)
		}
	}
}

// TestGreetVisitorsAggregatesErrorsAndSkipsBenign exercises GreetVisitors' error handling against
// its two documented real-world response shapes (see the Visitor doc comment in visitors.go): a
// visitor that hasn't finished arriving yet answers with errorCode=visitor_err_coming, which
// classifyResponse/benignErrorCodes (conn.go) treats as a non-fatal no-op -- sendAndWait returns
// err=nil for it, same as the real client's own captured session that left that visitor
// ungreeted -- while a genuine (non-benign) errorCode does surface as an error. GreetVisitors must
// still send the request for every visitor, not stop at the first error (errs is appended to in
// the loop and only joined at the end, exactly like CollectAll's own aggregation in buildings.go),
// so the final error should reflect only the real failure, not the benign one.
func TestGreetVisitorsAggregatesErrorsAndSkipsBenign(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	visitors := []Visitor{
		NewTestVisitor(2001, 2005, 6), // gets the benign "not yet greetable" response
		NewTestVisitor(2002, 2001, 6), // gets a genuine failure
		NewTestVisitor(2003, 2002, 6), // succeeds
	}

	benign := sfs.NewSFSObject()
	benign.PutUtfString("errorCode", "visitor_err_coming")
	failure := sfs.NewSFSObject()
	failure.PutUtfString("errorCode", "999999")
	success := sfs.NewSFSObject()
	success.PutBool("success", true)
	responses := map[int64]*sfs.SFSObject{2001: benign, 2002: failure, 2003: success}

	var gotUids []int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range visitors {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			uid := msg.Params.GetLong("uid")
			gotUids = append(gotUids, uid)
			resp, ok := responses[uid]
			if !ok {
				t.Errorf("unexpected uid %d", uid)
				return
			}
			_ = server.SendExtension(msg.Cmd, resp)
		}
	}()

	err := GreetVisitors(client, visitors)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading all three requests")
	}

	if len(gotUids) != 3 {
		t.Fatalf("fake server received %d requests, want 3 (GreetVisitors must not short-circuit on the first error)", len(gotUids))
	}
	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (uid 2002 got a genuine failure errorCode)")
	}
	if strings.Contains(err.Error(), "visitor_err_coming") {
		t.Errorf("aggregated error mentions the benign errorCode, want it folded away as a non-error: %v", err)
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("aggregated error = %v, want it to mention the genuine failure's errorCode 999999", err)
	}
}

// TestGreetVisitorsAbortsRemainingVisitorsOnNetError is the round-17 regression test for
// GreetVisitors' net.Error early-abort (visitors.go), mirroring
// TestCollectAllAbortsRemainingActionsOnNetError (buildings_orchestration_test.go) -- same root
// cause (a dead connection dooms every subsequent sendAndWait call to independently burn a full
// timeout before failing the same way), just scoped to the per-visitor loop instead of CollectAll's
// batch-of-sub-actions loop. fakeNetErrConn, fakeNetError, and fakeNetAddr are all package-level
// (buildings_orchestration_test.go) and reused here as-is.
//
// Updated in round 21: fakeNetErrConn/fakeNetError now carry a `timeout` field distinguishing a
// genuine dead connection (timeout: false, the zero value used here) from sendAndWait's ordinary
// per-action timeout (timeout: true, see TestGreetVisitorsDoesNotAbortOnTimeoutNetError below) --
// only the former should still abort the remaining visitors, per GreetVisitors' corrected doc
// comment in visitors.go.
//
// The fake connection's Read always fails with a non-timeout fakeNetError, so GreetVisitors' very
// first `visitor.operate` call fails immediately with a wrapped, non-timeout net.Error. Only that
// one request should ever be sent: if GreetVisitors didn't break early, it would go on to attempt
// every remaining visitor in the (maxNum-capped, see the Visitor doc comment in visitors.go) list in
// turn.
//
// Mutation check: reverting GreetVisitors' net.Error break in visitors.go back to the old flat
// `errs = append(errs, err)`-only loop makes this test fail with writeCount() == 3 instead of 1.
func TestGreetVisitorsAbortsRemainingVisitorsOnNetError(t *testing.T) {
	fake := &fakeNetErrConn{}
	client := session.NewGameConnForTest(fake)

	visitors := []Visitor{
		NewTestVisitor(3001, 2001, 6),
		NewTestVisitor(3002, 2002, 6),
		NewTestVisitor(3003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	if netErr, ok := errors.AsType[net.Error](err); !ok {
		t.Errorf("GreetVisitors() error = %v, want it to wrap a net.Error (the failure that triggered the break)", err)
	} else if netErr.Timeout() {
		t.Errorf("GreetVisitors() error wraps a net.Error with Timeout()==true, want the non-timeout fake used by this test")
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only the first visitor's request -- GreetVisitors should have aborted before attempting the other two)", got)
	}
}

// TestGreetVisitorsDoesNotAbortOnTimeoutNetError is the round-21 regression test proving the other
// half of the fix: an ordinary per-visitor sendAndWait timeout is ITSELF a net.Error with
// Timeout()==true (see conn_wait_test.go's TestWaitForTimeout) on an otherwise healthy connection,
// and must NOT trigger GreetVisitors' early-abort -- it should fall through into errs exactly like a
// decoded errorCode failure, and the loop should continue on to every remaining visitor.
//
// It reuses fakeNetErrConn/fakeNetError (buildings_orchestration_test.go) with timeout: true, so
// every Read fails with a Timeout()==true net.Error -- the shape of an ordinary per-action timeout,
// as opposed to TestGreetVisitorsAbortsRemainingVisitorsOnNetError above's default (timeout: false)
// dead-connection case.
//
// Mutation check: reverting GreetVisitors' `!netErr.Timeout()` condition in visitors.go back to a
// bare `errors.As(err, &netErr)` makes this test fail with writeCount() == 1 instead of 3, and the
// aggregated error would no longer contain three joined net.Error failures.
func TestGreetVisitorsDoesNotAbortOnTimeoutNetError(t *testing.T) {
	fake := &fakeNetErrConn{timeout: true}
	client := session.NewGameConnForTest(fake)

	visitors := []Visitor{
		NewTestVisitor(4001, 2001, 6),
		NewTestVisitor(4002, 2002, 6),
		NewTestVisitor(4003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	if netErr, ok := errors.AsType[net.Error](err); !ok {
		t.Errorf("GreetVisitors() error = %v, want it to wrap a net.Error", err)
	} else if !netErr.Timeout() {
		t.Errorf("GreetVisitors() error wraps a net.Error with Timeout()==false, want the Timeout()==true fake used by this test")
	}
	if got := fake.writeCount(); got != 3 {
		t.Errorf("fake connection saw %d writes, want exactly 3 (a Timeout()==true net.Error must not abort the remaining visitors)", got)
	}
}

// TestFetchBuildingsKeepsVisitorsBeyondMaxNum is the B42 regression test: ParseInitVisitors used to
// cut visitor.list at maxNum, but the client keeps every entry (InitData,
// CityVisitorManager.lua:341-357) and maxNum bounds only Queue1 (GetNewVisitorInfo, :187-205).
// Queue2 holds queue types 2, 4, 7, 8, 11 and 19 (Const.lua:25-41), so a full timed queue of five
// plus a SystemGift (queue type 8) is a legitimate six-entry list with maxNum 5.
//
// It drives the real path: a fake server sends one `init` push through FetchBuildings, the sole
// call site ParseInitVisitors is reached from, and GreetVisitors then greets all six against a
// server that answers exactly six requests. Before the fix FetchBuildings returned five and the
// SystemGift visitor was never greeted.
func TestFetchBuildingsKeepsVisitorsBeyondMaxNum(t *testing.T) {
	const maxNum = 5

	initClient, initServer := session.NewPipeGameConnPair(t)
	initDone := make(chan struct{})
	go func() {
		defer close(initDone)
		list := sfs.NewSFSArray()
		for i := range maxNum {
			v := sfs.NewSFSObject()
			v.PutLong("uid", int64(5000+i))
			v.PutInt("eventId", 2001+int32(i)) // GIFT visitors in the timed queue
			v.PutInt("type", 0)                // GEN_BY_TIME, Queue1
			v.PutInt("visitorId", 6)
			list.AddSFSObject(v)
		}
		gift := sfs.NewSFSObject()
		gift.PutLong("uid", 5005)
		gift.PutInt("eventId", 21001) // SystemGift
		gift.PutInt("type", 8)        // SystemGift queue type, Queue2
		gift.PutInt("visitorId", 6)
		list.AddSFSObject(gift)
		visitor := sfs.NewSFSObject()
		visitor.PutInt("maxNum", maxNum)
		visitor.PutSFSArray("list", list)
		params := sfs.NewSFSObject()
		params.PutSFSObject("visitor", visitor)
		if err := initServer.SendExtension("init", params); err != nil {
			return
		}
		_ = initServer.RawConn().Close() // see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment (buildings_orchestration_test.go): ends the test fast instead of waiting out the post-init 3s window
	}()

	buf := captureVisitorLog(t)
	_, visitors, err := FetchBuildings(initClient, 2*time.Second)

	select {
	case <-initDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake init-push server goroutine never finished")
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact, see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment)", err)
	}
	if len(visitors) != maxNum+1 {
		t.Fatalf("FetchBuildings parsed %d visitors, want all %d (maxNum %d bounds only the timed queue)", len(visitors), maxNum+1, maxNum)
	}
	for i, v := range visitors {
		if want := int64(5000 + i); v.Uid() != want {
			t.Errorf("visitors[%d].Uid() = %d, want %d (list order)", i, v.Uid(), want)
		}
	}
	logged := buf.String()
	if !strings.Contains(logged, `msg="visitor.list is longer than maxNum, which bounds only the timed queue; keeping every entry" listLen=6 maxNum=5`) {
		t.Errorf("expected the listLen/maxNum Info line, got:\n%s", logged)
	}
	if strings.Contains(logged, "truncating") {
		t.Errorf("a six-entry list must not be truncated, got:\n%s", logged)
	}

	greetClient, stop := serveVisitorCmds(t, visitorOK)
	if err := GreetVisitors(greetClient, visitors); err != nil {
		t.Fatalf("GreetVisitors() = %v, want nil", err)
	}
	got := stop()
	if len(got) != maxNum+1 {
		t.Fatalf("server saw %d greets, want %d", len(got), maxNum+1)
	}
	if uid := got[maxNum].params.GetLong("uid"); uid != 5005 {
		t.Errorf("last greet uid = %d, want the SystemGift visitor 5005", uid)
	}
}

// newVisitorInitParams builds a bare `init` push's top-level params carrying just the
// `visitor={maxNum, list}` sibling field ParseInitVisitors reads (see the Visitor doc comment in
// visitors.go). maxNum is omitted entirely when hasMaxNum is false.
func newVisitorInitParams(hasMaxNum bool, maxNum int32, listLen int) *sfs.SFSObject {
	list := sfs.NewSFSArray()
	for i := range listLen {
		v := sfs.NewSFSObject()
		v.PutLong("uid", int64(9000+i))
		v.PutInt("eventId", 3000+int32(i))
		v.PutInt("visitorId", 6)
		list.AddSFSObject(v)
	}
	visitor := sfs.NewSFSObject()
	if hasMaxNum {
		visitor.PutInt("maxNum", maxNum)
	}
	visitor.PutSFSArray("list", list)
	params := sfs.NewSFSObject()
	params.PutSFSObject("visitor", visitor)
	return params
}

// TestParseInitVisitorsCapsAtUpperBound: the list is server-sent, so MaxVisitorsUpperBound, not
// maxNum, bounds it -- a hostile maxNum=50000 with 50000 entries, or no maxNum at all, still parses
// exactly the first 300, in order, with a truncation Warn. GreetVisitors trusts the slice it gets,
// so this is what bounds its sequential worst case.
func TestParseInitVisitorsCapsAtUpperBound(t *testing.T) {
	const listLen = 50000
	for _, hasMaxNum := range []bool{true, false} {
		buf := captureVisitorLog(t)
		out := ParseInitVisitors(newVisitorInitParams(hasMaxNum, listLen, listLen))
		if len(out) != MaxVisitorsUpperBound {
			t.Fatalf("hasMaxNum=%v: parsed %d visitors, want exactly %d", hasMaxNum, len(out), MaxVisitorsUpperBound)
		}
		for i, v := range out {
			if want := int64(9000 + i); v.Uid() != want {
				t.Errorf("hasMaxNum=%v: out[%d].Uid() = %d, want %d (the first %d visitors, in order)", hasMaxNum, i, v.Uid(), want, MaxVisitorsUpperBound)
			}
		}
		if logged := buf.String(); !strings.Contains(logged, "visitor.list longer than cap; truncating") {
			t.Errorf("hasMaxNum=%v: expected a truncation warning, got:\n%s", hasMaxNum, logged)
		}
	}
}

// TestParseInitVisitorsUpperBoundBoundary drives both sides of the cap with well-formed entries,
// so an off-by-one in either the truncation warning or the loop shows in len(out) itself.
func TestParseInitVisitorsUpperBoundBoundary(t *testing.T) {
	for _, c := range []struct {
		listLen  int
		wantWarn bool
	}{{MaxVisitorsUpperBound, false}, {MaxVisitorsUpperBound + 1, true}} {
		buf := captureVisitorLog(t)
		out := ParseInitVisitors(newVisitorInitParams(true, 5, c.listLen))
		if len(out) != MaxVisitorsUpperBound {
			t.Errorf("listLen %d: parsed %d visitors, want %d", c.listLen, len(out), MaxVisitorsUpperBound)
		}
		if warned := strings.Contains(buf.String(), "visitor.list longer than cap; truncating"); warned != c.wantWarn {
			t.Errorf("listLen %d: truncation warning = %v, want %v:\n%s", c.listLen, warned, c.wantWarn, buf.String())
		}
	}
}

// TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput is the round-26 regression test for
// ParseInitVisitors' iteration-count fix: the cap must bound the RAW visitor.list items examined,
// not just the valid ones appended. A malformed entry (no "uid") logs its own Warn and never
// reaches the append, so a loop that stopped at `len(out) >= cap` would scan, and warn on, every
// entry of a list padded with malformed ones (sfs.MaxDecodedNodes=300,000 is the only other bound).
// Counting the "missing uid" warnings, not len(out) (0 either way), is what catches it.
//
// Mutation check: changing the loop's `if i >= MaxVisitorsUpperBound { break }` to
// `if len(out) >= MaxVisitorsUpperBound { break }` makes this fail with wantMalformed warnings.
func TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput(t *testing.T) {
	const wantMalformed = MaxVisitorsUpperBound * 3

	list := sfs.NewSFSArray()
	for i := range wantMalformed {
		v := sfs.NewSFSObject()
		v.PutInt("eventId", 3000+int32(i)) // deliberately no "uid" field
		v.PutInt("visitorId", 6)
		list.AddSFSObject(v)
	}
	visitor := sfs.NewSFSObject()
	visitor.PutInt("maxNum", 3)
	visitor.PutSFSArray("list", list)
	params := sfs.NewSFSObject()
	params.PutSFSObject("visitor", visitor)

	buf := captureVisitorLog(t)
	out := ParseInitVisitors(params)

	if len(out) != 0 {
		t.Fatalf("ParseInitVisitors parsed %d visitors, want 0 (every entry in this test is malformed -- missing uid)", len(out))
	}
	if got := strings.Count(buf.String(), "skipping visitor.list entry with no uid field"); got != MaxVisitorsUpperBound {
		t.Errorf("ParseInitVisitors logged %d \"missing uid\" warnings, want exactly %d (the cap on RAW items examined) -- input had %d malformed entries",
			got, MaxVisitorsUpperBound, wantMalformed)
	}
}

// TestParseInitVisitorsWrongTypedUIDIsRejected is the round-28 regression test for requireFieldType
// (buildings.go), exercised here at ParseInitVisitors' own uid guard: before this round's fix,
// requirePresentField only checked presence, never that uid's concrete decoded SFS type actually
// matched what Visitor.Uid() (GetLong) accepts. A present-but-wrong-typed uid (e.g. the server
// sending it as a string instead of a Long) used to silently pass that presence-only guard and then
// coerce to int64(0) via GetLong's own zero-value fallback -- colliding with a genuinely-zero uid,
// or another wrong-typed one, in FetchBuildings' seenVisitorUUIDs and login.go's dedupeVisitors
// (the PRIMARY init-push path).
//
// The input here has a wrong-typed uid entry (a string) and a separate, genuinely-well-typed uid=0
// entry -- proving these two are no longer conflated: exactly one visitor must come back (the
// genuine uid=0 one), and a "wrong-typed uid" warning, not a "missing uid" one, must be logged.
//
// Mutation check: reverting ParseInitVisitors' `requireFieldType(vi, "uid", "visitor.list",
// sfsFieldKindLong)` back to `requirePresentField(vi, "uid", "visitor.list")` makes this test fail
// with 2 visitors instead of 1 (both uid=0, indistinguishable), and no "wrong-typed" warning logged.
func TestParseInitVisitorsWrongTypedUIDIsRejected(t *testing.T) {
	wrongTyped := sfs.NewSFSObject()
	wrongTyped.PutUtfString("uid", "not-a-long") // wrong SFS type: a visitor uid must be a Long
	wrongTyped.PutInt("eventId", 2005)

	genuineZero := sfs.NewSFSObject()
	genuineZero.PutLong("uid", 0) // a real, well-typed uid that happens to be zero
	genuineZero.PutInt("eventId", 2001)

	list := sfs.NewSFSArray()
	list.AddSFSObject(wrongTyped)
	list.AddSFSObject(genuineZero)
	visitor := sfs.NewSFSObject()
	visitor.PutSFSArray("list", list)
	params := sfs.NewSFSObject()
	params.PutSFSObject("visitor", visitor)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := ParseInitVisitors(params)
	slog.SetDefault(orig)

	if len(got) != 1 {
		t.Fatalf("ParseInitVisitors parsed %d visitors, want 1 (only the genuine, well-typed uid=0 entry -- the string-typed one must be rejected, not silently coerced to uid=0 too)", len(got))
	}
	if got[0].Uid() != 0 || got[0].EventId() != 2001 {
		t.Errorf("got visitor uid=%d eventId=%d, want the genuine uid=0 eventId=2001 entry", got[0].Uid(), got[0].EventId())
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping visitor.list entry with wrong-typed uid field") {
		t.Errorf("expected a wrong-typed-uid warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping visitor.list entry with no uid field") {
		t.Errorf("wrong-typed uid must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

// TestParseInitVisitorsListIgnoresMaxNum: since B42 maxNum never shapes the parsed list. Whatever
// maxNum is (absent, wrong-typed, negative, an out-of-int32-range Long, or smaller than the list),
// a 30-entry list parses in full, and only a usable maxNum below the list length logs the Info line.
// refreshVisitors is what validates maxNum, for its refill rule.
func TestParseInitVisitorsListIgnoresMaxNum(t *testing.T) {
	const listLen = 30
	cases := map[string]struct {
		set     func(*sfs.SFSObject)
		wantLog bool
	}{
		"absent":       {func(*sfs.SFSObject) {}, false},
		"wrong-typed":  {func(o *sfs.SFSObject) { o.PutUtfString("maxNum", "not-an-int") }, false},
		"negative":     {func(o *sfs.SFSObject) { o.PutInt("maxNum", -5) }, false},
		"out of range": {func(o *sfs.SFSObject) { o.PutLong("maxNum", int64(math.MaxInt32)+5000000000) }, false},
		"below list":   {func(o *sfs.SFSObject) { o.PutInt("maxNum", 5) }, true},
		"above list":   {func(o *sfs.SFSObject) { o.PutInt("maxNum", 50) }, false},
	}
	for name, c := range cases {
		params := newVisitorInitParams(false, 0, listLen)
		c.set(evObject(params, "visitor"))
		buf := captureVisitorLog(t)
		if got := ParseInitVisitors(params); len(got) != listLen {
			t.Errorf("%s: parsed %d visitors, want all %d", name, len(got), listLen)
		}
		if logged := strings.Contains(buf.String(), "visitor.list is longer than maxNum"); logged != c.wantLog {
			t.Errorf("%s: listLen/maxNum Info line = %v, want %v:\n%s", name, logged, c.wantLog, buf.String())
		}
		if strings.Contains(buf.String(), "truncating") {
			t.Errorf("%s: a %d-entry list must not be truncated:\n%s", name, listLen, buf.String())
		}
	}
}

// TestParseInitVisitorsMalformedTopLevelShapes is the round-36 regression test for the MINOR
// finding that ParseInitVisitors' four top-level malformed-shape guards (wrong-typed visitor
// object, absent list, wrong-typed list array, non-object list entry) had zero direct/isolated
// test coverage -- each was only ever exercised indirectly, end to end, by happy-path tests that
// build a valid visitor/list/sfs.SFSArray/sfs.SFSObject chain. ParseInitVisitors parses the bare `init`
// bootstrap push, which this codebase's own threat-model comments (e.g.
// maxVisitorsDefensiveCeiling's doc comment) explicitly treat as attacker-influenceable via a
// hostile/misbehaving -cs-ip peer, so these four malformed shapes are squarely in scope.
func TestParseInitVisitorsMalformedTopLevelShapes(t *testing.T) {
	t.Run("visitor field present but wrong-typed (not an sfs.SFSObject)", func(t *testing.T) {
		params := sfs.NewSFSObject()
		params.PutUtfString("visitor", "not-an-object")

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := ParseInitVisitors(params)
		slog.SetDefault(orig)

		if len(got) != 0 {
			t.Fatalf("got %d visitors, want 0 (wrong-typed visitor field must yield no visitors, not a panic)", len(got))
		}
		// Round-39 regression: this used to be silently indistinguishable from a genuinely-absent
		// visitor field, with zero diagnostic.
		if logged := buf.String(); !strings.Contains(logged, "visitor field is present but not an object") {
			t.Errorf("expected a Warn naming the wrong-typed visitor field, got:\n%s", logged)
		}
	})
	t.Run("list field absent from an otherwise well-formed visitor object", func(t *testing.T) {
		visitor := sfs.NewSFSObject()
		visitor.PutInt("maxNum", 5) // present, but deliberately no "list" field
		params := sfs.NewSFSObject()
		params.PutSFSObject("visitor", visitor)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := ParseInitVisitors(params)
		slog.SetDefault(orig)

		if len(got) != 0 {
			t.Fatalf("got %d visitors, want 0 (absent list field must yield no visitors, not a panic)", len(got))
		}
		// A genuinely-absent list is the expected case, distinct from a present-but-wrong-typed
		// one (covered above) -- must stay silent, not warn.
		if logged := buf.String(); logged != "" {
			t.Errorf("expected no log output for a genuinely-absent list field, got:\n%s", logged)
		}
	})
	t.Run("list field present but wrong-typed (not an sfs.SFSArray)", func(t *testing.T) {
		visitor := sfs.NewSFSObject()
		visitor.PutUtfString("list", "not-an-array")
		params := sfs.NewSFSObject()
		params.PutSFSObject("visitor", visitor)

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got := ParseInitVisitors(params)
		slog.SetDefault(orig)

		if len(got) != 0 {
			t.Fatalf("got %d visitors, want 0 (wrong-typed list field must yield no visitors, not a panic)", len(got))
		}
		// Round-39 regression: this used to be silently indistinguishable from a genuinely-absent
		// list field, with zero diagnostic.
		if logged := buf.String(); !strings.Contains(logged, "list field is present but not an array") {
			t.Errorf("expected a Warn naming the wrong-typed list field, got:\n%s", logged)
		}
	})
	t.Run("list entry present but non-object -- skipped, sibling well-formed entries still kept", func(t *testing.T) {
		good := sfs.NewSFSObject()
		good.PutLong("uid", 555)
		list := sfs.NewSFSArray()
		list.AddInt(12345) // a bare non-object entry, not a *sfs.SFSObject
		list.AddSFSObject(good)
		visitor := sfs.NewSFSObject()
		visitor.PutSFSArray("list", list)
		params := sfs.NewSFSObject()
		params.PutSFSObject("visitor", visitor)

		got := ParseInitVisitors(params)
		if len(got) != 1 {
			t.Fatalf("got %d visitors, want exactly 1 (the non-object entry must be skipped, not panic or corrupt the sibling entry)", len(got))
		}
		if got[0].Uid() != 555 {
			t.Errorf("got uid %d, want 555", got[0].Uid())
		}
	})
}

// eofConnWithWrites is a minimal net.Conn whose every Read returns bare io.EOF -- like
// conn_wait_test.go's eofConn, simulating a peer's graceful close at the live-connection level -- but
// unlike eofConn (which embeds a nil net.Conn and would panic if any other method were called), Write
// here succeeds and is counted. That's required for a GreetVisitors end-to-end test: unlike
// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError (which calls ReadEnvelope directly, never
// touching Write), GreetVisitors' sendAndWait always writes the `visitor.operate` request first, so a
// nil-embed Write would panic before the Read path -- the one actually under test here -- was ever
// reached.
type eofConnWithWrites struct {
	mu     sync.Mutex
	writes int
}

func (c *eofConnWithWrites) Read([]byte) (int, error) { return 0, io.EOF }

func (c *eofConnWithWrites) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}

func (c *eofConnWithWrites) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *eofConnWithWrites) Close() error                     { return nil }
func (c *eofConnWithWrites) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *eofConnWithWrites) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *eofConnWithWrites) SetDeadline(time.Time) error      { return nil }
func (c *eofConnWithWrites) SetReadDeadline(time.Time) error  { return nil }
func (c *eofConnWithWrites) SetWriteDeadline(time.Time) error { return nil }

// TestGreetVisitorsAbortsOnRealGracefulCloseEndToEnd is the round-25 regression test proving
// GreetVisitors' net.Error early-abort (visitors.go) actually fires for a REAL graceful close, not
// just for the already-a-net.Error fakeNetErrConn used by
// TestGreetVisitorsAbortsRemainingVisitorsOnNetError above. That existing test injects a fake whose
// Read already returns something satisfying net.Error directly -- it never exercises packet.go's
// sfs.DeadConnError conversion (round 24), the piece of production code actually responsible for turning
// a real closed connection's bare io.EOF into a net.Error with Timeout()==false in the first place
// (see sfs.DeadConnError's and sfs.WrapIfClosed's doc comments in packet.go, and
// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError in conn_wait_test.go, which proves the same
// conversion at the ReadEnvelope level but never drives it through GreetVisitors itself).
//
// eofConnWithWrites' Read returns bare io.EOF -- the shape a real net.Conn produces for a peer's
// graceful close between packets -- fed through an actual GameConn, so GreetVisitors' first
// `visitor.operate` call runs the real sfs.ReadPacket -> sfs.ReadFrameField/sfs.WrapIfClosed -> sfs.DeadConnError
// chain end to end. Only that one request should ever be sent: if GreetVisitors didn't break early,
// it would go on to attempt every remaining visitor in turn.
//
// Mutation check: reverting GreetVisitors' net.Error break in visitors.go back to the old flat
// `errs = append(errs, err)`-only loop makes this test fail with writeCount() == 3 instead of 1.
func TestGreetVisitorsAbortsOnRealGracefulCloseEndToEnd(t *testing.T) {
	fake := &eofConnWithWrites{}
	client := session.NewGameConnForTest(fake)

	visitors := []Visitor{
		NewTestVisitor(6001, 2001, 6),
		NewTestVisitor(6002, 2002, 6),
		NewTestVisitor(6003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read returns bare io.EOF)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("GreetVisitors() error = %v, want it to wrap a net.Error (packet.go's sfs.DeadConnError, converted from the real bare io.EOF)", err)
	}
	if netErr.Timeout() {
		t.Errorf("GreetVisitors() error wraps a net.Error with Timeout()==true, want false -- a real graceful close must read as a genuine dead connection, not an ordinary per-call timeout")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("GreetVisitors() error = %v, want it to still satisfy errors.Is(err, io.EOF) (sfs.DeadConnError wraps, never replaces, the original io.EOF) -- proving this test actually exercised the real bare-io.EOF conversion path rather than a synthetic already-a-net.Error fake", err)
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only the first visitor's request -- GreetVisitors should have aborted before attempting the other two)", got)
	}
}
