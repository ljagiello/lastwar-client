package main

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestVisitor builds a Visitor whose Raw carries just the fields GreetVisitors and its own
// logging touch: uid, eventId, visitorId -- mirroring the shape ParseInitVisitors produces from a
// real `init` push (see the Visitor doc comment in visitors.go).
func newTestVisitor(uid int64, eventId, visitorId int32) Visitor {
	raw := NewSFSObject()
	raw.PutLong("uid", uid)
	raw.PutInt("eventId", eventId)
	raw.PutInt("visitorId", visitorId)
	return Visitor{Raw: raw}
}

// TestGreetVisitorsEmpty checks the len(visitors)==0 short-circuit: GreetVisitors must return nil
// without sending anything. No fake server goroutine is started here at all -- if the
// short-circuit were ever removed, GreetVisitors would block on sendAndWait's 8s timeout waiting
// for a reply nobody sends, and this test would time out instead of silently passing.
func TestGreetVisitorsEmpty(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	if err := GreetVisitors(client, nil); err != nil {
		t.Errorf("GreetVisitors(nil) = %v, want nil", err)
	}
}

// TestGreetVisitorsSendsOnePerVisitor checks the success path: GreetVisitors issues one
// `visitor.operate {uid, operate: 1}` per visitor, in order, and returns nil once every one of
// them gets a real success response.
func TestGreetVisitorsSendsOnePerVisitor(t *testing.T) {
	client, server := newPipeGameConnPair(t)
	visitors := []Visitor{
		newTestVisitor(1001, 2001, 6),
		newTestVisitor(1002, 2002, 6),
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
			resp := NewSFSObject()
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
	client, server := newPipeGameConnPair(t)
	visitors := []Visitor{
		newTestVisitor(2001, 2005, 6), // gets the benign "not yet greetable" response
		newTestVisitor(2002, 2001, 6), // gets a genuine failure
		newTestVisitor(2003, 2002, 6), // succeeds
	}

	benign := NewSFSObject()
	benign.PutUtfString("errorCode", "visitor_err_coming")
	failure := NewSFSObject()
	failure.PutUtfString("errorCode", "999999")
	success := NewSFSObject()
	success.PutBool("success", true)
	responses := map[int64]*SFSObject{2001: benign, 2002: failure, 2003: success}

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
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	visitors := []Visitor{
		newTestVisitor(3001, 2001, 6),
		newTestVisitor(3002, 2002, 6),
		newTestVisitor(3003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
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
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	visitors := []Visitor{
		newTestVisitor(4001, 2001, 6),
		newTestVisitor(4002, 2002, 6),
		newTestVisitor(4003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("GreetVisitors() error = %v, want it to wrap a net.Error", err)
	} else if !netErr.Timeout() {
		t.Errorf("GreetVisitors() error wraps a net.Error with Timeout()==false, want the Timeout()==true fake used by this test")
	}
	if got := fake.writeCount(); got != 3 {
		t.Errorf("fake connection saw %d writes, want exactly 3 (a Timeout()==true net.Error must not abort the remaining visitors)", got)
	}
}

// TestGreetVisitorsOnlyGreetsUpToInitPushMaxNumAndLogsTruncation is the round-24 regression test for
// ParseInitVisitors' maxNum enforcement (visitors.go): the init push's own `visitor.maxNum` sibling
// field (see the Visitor doc comment) is now read and used to cap `visitor.list` before GreetVisitors
// ever sees the parsed slice -- closing the unbounded-hang threat model the round-24 audit reopened:
// a hostile or misbehaving peer could otherwise pad visitor.list arbitrarily, and each greet costs up
// to a full defaultCmdTimeout (conn.go) against a peer that never responds.
//
// This covers the full real-world path in two stages. First, a fake server sends a single `init`
// push whose visitor.list carries wantVisitors entries (well over a deliberately small maxNum=3)
// through FetchBuildings (buildings.go) -- the sole call site ParseInitVisitors is actually reached
// from -- and asserts the returned slice has exactly maxNum entries plus the truncation warning was
// logged. Second, GreetVisitors is run on that already-capped slice against its own fake server that
// only answers maxNum requests before returning: if GreetVisitors (or ParseInitVisitors before it)
// ever regressed to processing the full wantVisitors list, GreetVisitors' 4th request would hang with
// nobody left to answer it, and this test's own done-channel wait would time out.
//
// Mutation check: reverting ParseInitVisitors' cap loop in visitors.go back to appending every
// visitor.list entry unconditionally makes this test fail at the FetchBuildings stage already (len
// (visitors) == wantVisitors instead of maxNum, no truncation warning logged) -- the second stage
// would then also time out waiting for a 4th request nobody in the greet-server loop is there to
// answer.
func TestGreetVisitorsOnlyGreetsUpToInitPushMaxNumAndLogsTruncation(t *testing.T) {
	const (
		maxNum       = 3
		wantVisitors = 8 // well over maxNum
	)

	initClient, initServer := newPipeGameConnPair(t)

	initDone := make(chan struct{})
	go func() {
		defer close(initDone)
		list := NewSFSArray()
		for i := 0; i < wantVisitors; i++ {
			v := NewSFSObject()
			v.PutLong("uid", int64(5000+i))
			v.PutInt("eventId", 2000+int32(i))
			v.PutInt("visitorId", 6)
			list.AddSFSObject(v)
		}
		visitor := NewSFSObject()
		visitor.PutInt("maxNum", maxNum)
		visitor.PutSFSArray("list", list)
		params := NewSFSObject()
		params.PutSFSObject("visitor", visitor)
		if err := initServer.SendExtension("init", params); err != nil {
			return
		}
		initServer.conn.Close() // see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment (buildings_orchestration_test.go): ends the test fast instead of waiting out the post-init 3s window
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))

	_, visitors, err := FetchBuildings(initClient, 2*time.Second)
	slog.SetDefault(orig)

	select {
	case <-initDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake init-push server goroutine never finished")
	}

	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact, see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment)", err)
	}
	if len(visitors) != maxNum {
		t.Fatalf("FetchBuildings parsed %d visitors, want exactly %d (the init push's own maxNum=%d field, not the full %d-entry visitor.list)", len(visitors), maxNum, maxNum, wantVisitors)
	}
	for i, v := range visitors {
		if want := int64(5000 + i); v.Uid() != want {
			t.Errorf("visitors[%d].Uid() = %d, want %d (the first %d visitors, in order)", i, v.Uid(), want, maxNum)
		}
	}

	if logged := buf.String(); !strings.Contains(logged, "visitor.list longer than cap") {
		t.Errorf("expected a truncation warning log from ParseInitVisitors, got:\n%s", logged)
	}

	// Second stage: prove GreetVisitors itself only ever attempts a greet for the already-capped
	// slice FetchBuildings returned, not the original wantVisitors-sized list -- a fake server that
	// answers only maxNum requests before its goroutine returns.
	greetClient, greetServer := newPipeGameConnPair(t)

	var gotUids []int64
	greetDone := make(chan struct{})
	go func() {
		defer close(greetDone)
		for i := 0; i < maxNum; i++ {
			env, err := greetServer.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			gotUids = append(gotUids, msg.Params.GetLong("uid"))
			resp := NewSFSObject()
			resp.PutBool("success", true)
			_ = greetServer.SendExtension(msg.Cmd, resp)
		}
	}()

	if err := GreetVisitors(greetClient, visitors); err != nil {
		t.Fatalf("GreetVisitors() = %v, want nil", err)
	}

	select {
	case <-greetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake greet server never finished reading the expected requests (a broken cap would leave GreetVisitors sending more requests than the fake server answers, or a broken slice would send fewer)")
	}

	if len(gotUids) != maxNum {
		t.Fatalf("fake greet server received %d requests, want exactly %d", len(gotUids), maxNum)
	}
	for i, uid := range gotUids {
		if want := int64(5000 + i); uid != want {
			t.Errorf("gotUids[%d] = %d, want %d (the first %d visitors, in order)", i, uid, want, maxNum)
		}
	}
}

// newVisitorInitParams builds a bare `init` push's top-level params carrying just the
// `visitor={maxNum, list}` sibling field ParseInitVisitors reads (see the Visitor doc comment in
// visitors.go). maxNum is omitted entirely when hasMaxNum is false, exercising the
// maxVisitorsDefensiveCeiling fallback path exactly like a real push that never sends the field.
func newVisitorInitParams(hasMaxNum bool, maxNum int32, listLen int) *SFSObject {
	list := NewSFSArray()
	for i := 0; i < listLen; i++ {
		v := NewSFSObject()
		v.PutLong("uid", int64(9000+i))
		v.PutInt("eventId", 3000+int32(i))
		v.PutInt("visitorId", 6)
		list.AddSFSObject(v)
	}
	visitor := NewSFSObject()
	if hasMaxNum {
		visitor.PutInt("maxNum", maxNum)
	}
	visitor.PutSFSArray("list", list)
	params := NewSFSObject()
	params.PutSFSObject("visitor", visitor)
	return params
}

// TestParseInitVisitorsFallbackCeilingTruncatesWithoutMaxNum is the round-25 regression test proving
// maxVisitorsDefensiveCeiling's own FALLBACK branch (visitors.go) -- used when the init push's
// `visitor.maxNum` field is absent, unparseable, or <= 0 -- actually truncates. Every other test in
// this file that drives ParseInitVisitors through a real push (see
// TestGreetVisitorsOnlyGreetsUpToInitPushMaxNumAndLogsTruncation above) always supplies an explicit
// positive maxNum well under 25, so the fallback ceiling itself had never actually been exercised: a
// regression breaking the fallback branch (e.g. defaulting `limit` to 0, or to an unbounded value)
// could have shipped undetected.
//
// The init push here omits `maxNum` entirely and supplies a visitor.list well over
// maxVisitorsDefensiveCeiling (25) entries -- asserting ParseInitVisitors truncates to exactly 25, in
// list order, and logs the truncation warning.
//
// Mutation check: reverting the `limit := maxVisitorsDefensiveCeiling` fallback initializer in
// visitors.go's ParseInitVisitors to `limit := len(arr.items)` (i.e. no cap at all when maxNum is
// absent) makes this test fail with len(out) == wantListLen instead of
// maxVisitorsDefensiveCeiling, and no truncation warning logged.
func TestParseInitVisitorsFallbackCeilingTruncatesWithoutMaxNum(t *testing.T) {
	const wantListLen = maxVisitorsDefensiveCeiling + 5 // well over the fallback ceiling

	params := newVisitorInitParams(false, 0, wantListLen)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := ParseInitVisitors(params)
	slog.SetDefault(orig)

	if len(out) != maxVisitorsDefensiveCeiling {
		t.Fatalf("ParseInitVisitors parsed %d visitors, want exactly %d (the fallback ceiling -- no maxNum field was present)", len(out), maxVisitorsDefensiveCeiling)
	}
	for i, v := range out {
		if want := int64(9000 + i); v.Uid() != want {
			t.Errorf("out[%d].Uid() = %d, want %d (the first %d visitors, in order)", i, v.Uid(), want, maxVisitorsDefensiveCeiling)
		}
	}
	if logged := buf.String(); !strings.Contains(logged, "visitor.list longer than cap") {
		t.Errorf("expected a truncation warning log from ParseInitVisitors' fallback ceiling, got:\n%s", logged)
	}
}

// TestParseInitVisitorsClampsInflatedMaxNumToUpperBound is the round-25 regression test for the fix
// closing the finding that ParseInitVisitors' cap was fully bypassable: it used to defer entirely to
// the server-supplied `maxNum` field with only a `> 0` sanity check and no upper bound, so a hostile
// or misbehaving -cs-ip peer could set maxNum to an enormous value (paired with a matching number of
// visitor.list entries, well within sfsobject.go's maxDecodedNodes=300,000 decode budget) and
// completely defeat maxVisitorsDefensiveCeiling's own protection -- GreetVisitors trusts whatever
// slice ParseInitVisitors hands it entirely, so this directly reopened the unbounded sequential
// wall-clock cost maxVisitorsDefensiveCeiling (round 24) was introduced to bound.
//
// The init push here sets an obviously-hostile maxNum=50000 alongside a matching 50000-entry
// visitor.list -- proving ParseInitVisitors now clamps to maxVisitorsUpperBound (300), not to the
// attacker-supplied maxNum, and logs the clamp-triggered warning.
//
// Mutation check: reverting ParseInitVisitors' `if maxNum := ...; maxNum > 0 { limit =
// int(maxNum) }` in visitors.go back to omitting the maxVisitorsUpperBound clamp entirely makes this
// test fail with len(out) == maxVisitorsUpperBound-worth-exceeding count (up to the full 50000-entry
// list) instead of 300, and no clamp warning logged.
func TestParseInitVisitorsClampsInflatedMaxNumToUpperBound(t *testing.T) {
	const hostileMaxNum = 50000

	params := newVisitorInitParams(true, hostileMaxNum, hostileMaxNum)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := ParseInitVisitors(params)
	slog.SetDefault(orig)

	if len(out) != maxVisitorsUpperBound {
		t.Fatalf("ParseInitVisitors parsed %d visitors, want exactly %d (the hardcoded upper bound -- the server-supplied maxNum=%d must not be trusted verbatim)", len(out), maxVisitorsUpperBound, hostileMaxNum)
	}
	for i, v := range out {
		if want := int64(9000 + i); v.Uid() != want {
			t.Errorf("out[%d].Uid() = %d, want %d (the first %d visitors, in order)", i, v.Uid(), want, maxVisitorsUpperBound)
		}
	}
	if logged := buf.String(); !strings.Contains(logged, "visitor.maxNum exceeds upper sanity bound; clamping") {
		t.Errorf("expected a clamp warning log from ParseInitVisitors, got:\n%s", logged)
	}
}

// TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput is the round-26 regression test for
// ParseInitVisitors' iteration-count fix (visitors.go): the round-24/25 cap (maxVisitorsDefensiveCeiling
// / maxVisitorsUpperBound) only ever bounded the number of VALID entries appended to the returned
// slice -- the loop's break condition was `len(out) >= limit`, checked only after a successful
// requirePresentField pass. A malformed entry (missing the required "uid" field) hit a `continue`
// that didn't advance len(out) at all, so it never counted against the cap, even though
// requirePresentField itself logs a Warn for every single one of them. Since the raw `visitor.list`
// array is bounded only by sfsobject.go's much larger maxDecodedNodes=300,000 decode budget, a
// hostile peer could pad visitor.list with malformed entries and force ParseInitVisitors to scan (and
// log-warn on) all of them regardless of how small the configured cap was.
//
// The init push here supplies a small maxNum well under maxVisitorsDefensiveCeiling, and a
// visitor.list made entirely of malformed entries (no "uid" field) numbering far more than that cap.
// Since every entry is malformed, len(out) stays 0 throughout the whole scan -- so the old
// `len(out) >= limit` break condition would never fire, and the loop would examine (and
// requirePresentField would log a warning for) every one of the wantMalformed entries instead of
// stopping at maxNum. Counting the "skipping visitor.list entry with no uid field" warnings actually
// logged, rather than just asserting len(out), is what makes this test capable of catching the
// unbounded-scan regression at all: len(out) would be 0 either way, fixed or broken.
//
// Mutation check: reverting the loop's `for i, item := range arr.items { if i >= limit { break }
// ...}` in visitors.go back to `for _, item := range arr.items { if len(out) >= limit { break } ...}`
// makes this test fail with a logged-warning count of wantMalformed instead of maxNum.
func TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput(t *testing.T) {
	const (
		maxNum        = 3
		wantMalformed = maxNum * 50 // far more malformed entries than the cap
	)

	list := NewSFSArray()
	for i := 0; i < wantMalformed; i++ {
		v := NewSFSObject()
		v.PutInt("eventId", 3000+int32(i)) // deliberately no "uid" field
		v.PutInt("visitorId", 6)
		list.AddSFSObject(v)
	}
	visitor := NewSFSObject()
	visitor.PutInt("maxNum", maxNum)
	visitor.PutSFSArray("list", list)
	params := NewSFSObject()
	params.PutSFSObject("visitor", visitor)

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	out := ParseInitVisitors(params)
	slog.SetDefault(orig)

	if len(out) != 0 {
		t.Fatalf("ParseInitVisitors parsed %d visitors, want 0 (every entry in this test is malformed -- missing uid)", len(out))
	}

	gotWarnings := strings.Count(buf.String(), "skipping visitor.list entry with no uid field")
	if gotWarnings != maxNum {
		t.Errorf("ParseInitVisitors logged %d \"missing uid\" warnings, want exactly %d (the cap on RAW items examined, not just valid ones appended) -- input had %d malformed entries; the loop must stop scanning after the first %d regardless of how many turned out valid", gotWarnings, maxNum, wantMalformed, maxNum)
	}
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
// deadConnError conversion (round 24), the piece of production code actually responsible for turning
// a real closed connection's bare io.EOF into a net.Error with Timeout()==false in the first place
// (see deadConnError's and wrapIfClosed's doc comments in packet.go, and
// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError in conn_wait_test.go, which proves the same
// conversion at the ReadEnvelope level but never drives it through GreetVisitors itself).
//
// eofConnWithWrites' Read returns bare io.EOF -- the shape a real net.Conn produces for a peer's
// graceful close between packets -- fed through an actual GameConn, so GreetVisitors' first
// `visitor.operate` call runs the real ReadPacket -> readFrameField/wrapIfClosed -> deadConnError
// chain end to end. Only that one request should ever be sent: if GreetVisitors didn't break early,
// it would go on to attempt every remaining visitor in turn.
//
// Mutation check: reverting GreetVisitors' net.Error break in visitors.go back to the old flat
// `errs = append(errs, err)`-only loop makes this test fail with writeCount() == 3 instead of 1.
func TestGreetVisitorsAbortsOnRealGracefulCloseEndToEnd(t *testing.T) {
	fake := &eofConnWithWrites{}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	visitors := []Visitor{
		newTestVisitor(6001, 2001, 6),
		newTestVisitor(6002, 2002, 6),
		newTestVisitor(6003, 2003, 6),
	}

	err := GreetVisitors(client, visitors)

	if err == nil {
		t.Fatal("GreetVisitors() = nil, want a non-nil error (the fake connection's every Read returns bare io.EOF)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("GreetVisitors() error = %v, want it to wrap a net.Error (packet.go's deadConnError, converted from the real bare io.EOF)", err)
	}
	if netErr.Timeout() {
		t.Errorf("GreetVisitors() error wraps a net.Error with Timeout()==true, want false -- a real graceful close must read as a genuine dead connection, not an ordinary per-call timeout")
	}
	if !errors.Is(err, io.EOF) {
		t.Errorf("GreetVisitors() error = %v, want it to still satisfy errors.Is(err, io.EOF) (deadConnError wraps, never replaces, the original io.EOF) -- proving this test actually exercised the real bare-io.EOF conversion path rather than a synthetic already-a-net.Error fake", err)
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only the first visitor's request -- GreetVisitors should have aborted before attempting the other two)", got)
	}
}
