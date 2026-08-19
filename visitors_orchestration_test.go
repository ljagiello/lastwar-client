package main

import (
	"bufio"
	"errors"
	"net"
	"strings"
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
// The fake connection's Read always fails with fakeNetError, so GreetVisitors' very first
// `visitor.operate` call fails immediately with a wrapped net.Error. Only that one request should
// ever be sent: if GreetVisitors didn't break early, it would go on to attempt every remaining
// visitor in the (maxNum-capped, see the Visitor doc comment in visitors.go) list in turn.
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
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only the first visitor's request -- GreetVisitors should have aborted before attempting the other two)", got)
	}
}
