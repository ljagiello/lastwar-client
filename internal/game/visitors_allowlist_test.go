package game

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

type visitorRequest struct {
	cmd    string
	params *sfs.SFSObject
}

// serveVisitorCmds answers every extension request on an in-memory connection with reply(req).
// stop closes the client end and returns the requests the server saw, in order.
func serveVisitorCmds(t *testing.T, reply func(req visitorRequest) *sfs.SFSObject) (client *session.GameConn, stop func() []visitorRequest) {
	t.Helper()
	client, server := session.NewPipeGameConnPair(t)
	var got []visitorRequest
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			req := visitorRequest{cmd: msg.Cmd, params: msg.Params}
			got = append(got, req)
			if err := server.SendExtension(msg.Cmd, reply(req)); err != nil {
				return
			}
		}
	}()
	return client, func() []visitorRequest {
		_ = client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("fake server did not stop after the client closed")
		}
		return got
	}
}

func visitorOK(visitorRequest) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	resp.PutBool("success", true)
	return resp
}

// captureVisitorLog sends the default logger to a buffer for the rest of the test.
func captureVisitorLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func newTimedTestVisitor(uid int64, eventId int32, startTime time.Time) Visitor {
	v := NewTestVisitor(uid, eventId, 6)
	v.Raw.PutLong("startTime", startTime.UnixMilli())
	return v
}

func TestVisitorEventTypesMatchKnownPairs(t *testing.T) {
	if len(visitorEventTypes) != 398 {
		t.Errorf("visitorEventTypes has %d rows, want the table's 398", len(visitorEventTypes))
	}
	want := map[int32]int32{1001: 1, 1002: 1, 3115: 3, 3116: 3, 9001: 9, 21001: 30, 16001: 16, 16101: 17, 20950: 19, 4006: 11}
	for id := int32(2001); id <= 2006; id++ {
		want[id] = 2
	}
	for id := int32(20840); id <= 20849; id++ {
		want[id] = 10
	}
	for id, typ := range want {
		if got, ok := visitorEventTypes[id]; !ok || got != typ {
			t.Errorf("visitorEventTypes[%d] = %d (present %v), want %d", id, got, ok, typ)
		}
	}
}

// TestGreetAllowlistCoversOnlyRewardTypes walks the whole generated table: an eventId is greeted
// exactly when its behaviour type is GIFT, RECRUITMENT, DOMINATOR, SeasonDayGift or SystemGift.
func TestGreetAllowlistCoversOnlyRewardTypes(t *testing.T) {
	accept := map[int32]bool{2: true, 3: true, 9: true, 10: true, 30: true}
	past := time.Unix(1_700_000_000, 0)
	now := past.Add(time.Hour)
	for id, typ := range visitorEventTypes {
		_, reason := visitorSkipReason(newTimedTestVisitor(1, id, past), now)
		if greeted := reason == ""; greeted != accept[typ] {
			t.Errorf("eventId %d (type %d %s): greeted = %v, want %v", id, typ, visitorTypeName(typ), greeted, accept[typ])
		}
	}
}

func TestVisitorSkipReason(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name    string
		eventId int32
		start   time.Time
		want    string
		wantTyp int32
	}{
		{"gift arrived", 2001, now.Add(-time.Minute), "", 2},
		{"recruit arrived", 3115, now.Add(-time.Minute), "", 3},
		{"season gift arrived", 20840, now.Add(-time.Minute), "", 10},
		{"dominator arrived", 9001, now.Add(-time.Minute), "", 9},
		{"system gift arrived", 21001, now.Add(-time.Minute), "", 30},
		{"merchant", 1001, now.Add(-time.Minute), "behaviour type not on the greet allowlist", 1},
		{"worker lottery", 5001, now.Add(-time.Minute), "behaviour type not on the greet allowlist", 5},
		{"sky battle", 16001, now.Add(-time.Minute), "behaviour type not on the greet allowlist", 16},
		{"survivor pack", 20950, now.Add(-time.Minute), "behaviour type not on the greet allowlist", 19},
		{"unknown eventId", 999999, now.Add(-time.Minute), "eventId not in lw_base_visitor_event", 0},
		{"gift still arriving", 2001, now.Add(time.Minute), "not arrived yet (startTime in the future)", 2},
		{"gift inside the margin", 2001, now.Add(-visitorArrivalMargin / 2), "not arrived yet (startTime in the future)", 2},
		{"gift exactly at the margin", 2001, now.Add(-visitorArrivalMargin), "", 2},
	}
	for _, c := range cases {
		typ, reason := visitorSkipReason(newTimedTestVisitor(1, c.eventId, c.start), now)
		if reason != c.want || typ != c.wantTyp {
			t.Errorf("%s: visitorSkipReason = (%d, %q), want (%d, %q)", c.name, typ, reason, c.wantTyp, c.want)
		}
	}
	// No startTime field at all reads as arrived.
	if _, reason := visitorSkipReason(NewTestVisitor(1, 2001, 6), now); reason != "" {
		t.Errorf("visitor without startTime: skip reason %q, want it greeted", reason)
	}
}

// TestGreetVisitorsNeverOperatesOnSkippedVisitors is the safety property of the allowlist: a
// merchant (whose operate:1 buys an item), an unknown eventId, and an allowlisted visitor that
// has not arrived get no request at all -- not operate:1 and not a declining operate:0.
func TestGreetVisitorsNeverOperatesOnSkippedVisitors(t *testing.T) {
	buf := captureVisitorLog(t)
	now := time.Now()
	visitors := []Visitor{
		newTimedTestVisitor(101, 1001, now.Add(-time.Hour)),   // MERCHANT
		newTimedTestVisitor(102, 1002, now.Add(-time.Hour)),   // MERCHANT
		newTimedTestVisitor(103, 999999, now.Add(-time.Hour)), // not in the table
		newTimedTestVisitor(104, 5001, now.Add(-time.Hour)),   // WORKER_LOTTERY
		newTimedTestVisitor(105, 2001, now.Add(time.Hour)),    // GIFT, still arriving
		newTimedTestVisitor(106, 2002, now.Add(-time.Hour)),   // GIFT
		newTimedTestVisitor(107, 3115, now.Add(-time.Hour)),   // RECRUITMENT
	}
	client, stop := serveVisitorCmds(t, visitorOK)

	err := greetVisitors(client, visitors, now)
	got := stop()

	if err != nil {
		t.Fatalf("greetVisitors() = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2 (uids 106 and 107 only): %v", len(got), got)
	}
	for i, wantUid := range []int64{106, 107} {
		if got[i].cmd != "visitor.operate" || got[i].params.GetLong("uid") != wantUid || got[i].params.GetInt("operate") != 1 {
			t.Errorf("request %d = %s uid=%d operate=%d, want visitor.operate uid=%d operate=1",
				i, got[i].cmd, got[i].params.GetLong("uid"), got[i].params.GetInt("operate"), wantUid)
		}
	}
	logged := buf.String()
	for _, want := range []string{
		`msg="skipping visitor" reason="behaviour type not on the greet allowlist" uid=101 eventId=1001 type=1 typeName=MERCHANT`,
		`msg="skipping visitor" reason="eventId not in lw_base_visitor_event" uid=103 eventId=999999 type=0 typeName=unknown`,
		`uid=104 eventId=5001 type=5 typeName=WORKER_LOTTERY`,
		`msg="skipping visitor" reason="not arrived yet (startTime in the future)" uid=105 eventId=2001`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

// TestGreetVisitorsAllSkippedSendsNothing: with only skipped visitors, GreetVisitors returns nil
// without touching the connection (a send would block on the unanswered pipe).
func TestGreetVisitorsAllSkippedSendsNothing(t *testing.T) {
	client, _ := session.NewPipeGameConnPair(t)
	visitors := []Visitor{NewTestVisitor(1, 1001, 6), NewTestVisitor(2, 4001, 6), NewTestVisitor(3, 12001, 6)}
	if err := GreetVisitors(client, visitors); err != nil {
		t.Errorf("GreetVisitors(merchant, battle, alliance invite) = %v, want nil", err)
	}
}
