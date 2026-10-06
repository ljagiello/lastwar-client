package game

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func activityVisitorEntry(rewardState int32, eTime time.Time, eventId int32) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutLong("sTime", eTime.Add(-7*24*time.Hour).Unix())
	e.PutLong("eTime", eTime.Unix())
	e.PutInt("rewardState", rewardState)
	e.PutLong("lpTime", 0)
	e.PutInt("eventId", eventId)
	e.PutLong("nextResetTime", eTime.Add(-time.Hour).Unix())
	return e
}

func initWithActivityVisitors(entries map[string]*sfs.SFSObject, order ...string) *Init {
	av := sfs.NewSFSObject()
	for _, k := range order {
		av.PutSFSObject(k, entries[k])
	}
	raw := sfs.NewSFSObject()
	raw.PutSFSObject("activityVisitor", av)
	return &Init{Raw: raw}
}

// assertOptInFeature checks name is registered with DefaultOn false: new features stay off until
// validated live.
func assertOptInFeature(t *testing.T, name string) {
	t.Helper()
	f, ok := featureRegistry[name]
	if !ok {
		t.Fatalf("feature %q is not registered", name)
	}
	if f.DefaultOn {
		t.Errorf("feature %q is DefaultOn, want off until validated live", name)
	}
}

func TestActivityVisitorsIsOptIn(t *testing.T) { assertOptInFeature(t, "activity-visitors") }

// TestActivityVisitorsClaimsOnlyEligible: one claimable entry among every skip reason; only it is
// sent, as visitor.receive.reward with an Int activityId.
func TestActivityVisitorsClaimsOnlyEligible(t *testing.T) {
	buf := captureVisitorLog(t)
	now := time.Unix(1_800_000_000, 0)
	future, past := now.Add(24*time.Hour), now.Add(-time.Minute)
	missingState := sfs.NewSFSObject()
	missingState.PutLong("eTime", future.Unix())
	missingState.PutInt("eventId", 4006)
	entries := map[string]*sfs.SFSObject{
		"94309": activityVisitorEntry(0, future, 4006), // claimable
		"94310": activityVisitorEntry(1, future, 4006), // claimed this period
		"94311": activityVisitorEntry(0, past, 4006),   // event over
		"94312": activityVisitorEntry(0, future, 2001), // a GIFT eventId: not an activity visitor
		"94313": activityVisitorEntry(0, future, 777),  // unknown eventId
		"x9431": activityVisitorEntry(0, future, 4006), // key is not an activity id
		"94314": missingState,
	}
	in := initWithActivityVisitors(entries, "94310", "94311", "94312", "94313", "x9431", "94314", "94309")
	in.Object("activityVisitor").PutInt("94315", 3) // not an object

	client, stop := serveVisitorCmds(t, visitorOK)
	err := claimActivityVisitors(client, in, now)
	got := stop()

	if err != nil {
		t.Fatalf("claimActivityVisitors() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].cmd != "visitor.receive.reward" {
		t.Fatalf("server saw %v, want exactly one visitor.receive.reward", got)
	}
	v, _ := got[0].params.Get("activityId")
	if id, ok := v.Val.(int32); !ok || id != 94309 {
		t.Errorf("activityId = %#v, want Int 94309", v.Val)
	}
	logged := buf.String()
	for _, want := range []string{
		`msg="activity visitor already claimed this period" activityId=94310`,
		`msg="activity visitor's event has ended" activityId=94311`,
		`activityId=94312 eventId=2001 type=2 typeName=GIFT`,
		`activityId=94313 eventId=777 type=0 typeName=unknown`,
		`key=x9431`,
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log missing %q:\n%s", want, logged)
		}
	}
}

func TestActivityVisitorsKeepsGoingPastFailure(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	future := now.Add(time.Hour)
	in := initWithActivityVisitors(map[string]*sfs.SFSObject{
		"1": activityVisitorEntry(0, future, 4006),
		"2": activityVisitorEntry(0, future, 4006),
	}, "1", "2")
	client, stop := serveVisitorCmds(t, func(req visitorRequest) *sfs.SFSObject {
		if req.params.GetInt("activityId") == 1 {
			resp := sfs.NewSFSObject()
			resp.PutUtfString("errorCode", "E_FAKE_1")
			return resp
		}
		return visitorOK(req)
	})
	err := claimActivityVisitors(client, in, now)
	got := stop()

	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2 (a failed claim must not stop the next)", len(got))
	}
	if err == nil || !strings.Contains(err.Error(), "E_FAKE_1") {
		t.Errorf("claimActivityVisitors() = %v, want the E_FAKE_1 failure reported", err)
	}
}

func TestActivityVisitorsStopsOnDeadConnection(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	in := initWithActivityVisitors(map[string]*sfs.SFSObject{
		"1": activityVisitorEntry(0, now.Add(time.Hour), 4006),
		"2": activityVisitorEntry(0, now.Add(time.Hour), 4006),
	}, "1", "2")
	fake := &fakeNetErrConn{}
	err := claimActivityVisitors(session.NewGameConnForTest(fake), in, now)
	if !session.ContainsNonTimeoutNetError(err) {
		t.Errorf("claimActivityVisitors() = %v, want the dead-connection error", err)
	}
	if fake.writeCount() != 1 {
		t.Errorf("sent %d requests, want 1 (stop after the connection died)", fake.writeCount())
	}
}

func TestActivityVisitorsNothingToDo(t *testing.T) {
	for name, in := range map[string]*Init{
		"nil init":            nil,
		"no init push":        {},
		"no activityVisitor":  {Raw: sfs.NewSFSObject()},
		"empty activityVisit": initWithActivityVisitors(nil),
	} {
		fake := &fakeNetErrConn{}
		if err := runActivityVisitors(session.NewGameConnForTest(fake), in); err != nil {
			t.Errorf("%s: runActivityVisitors() = %v, want nil", name, err)
		}
		if fake.writeCount() != 0 {
			t.Errorf("%s: sent %d requests, want none", name, fake.writeCount())
		}
	}
}

func TestActivityVisitorsCapsClaimsPerRun(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	entries := map[string]*sfs.SFSObject{}
	var order []string
	for i := range maxActivityVisitorsPerRun + 5 {
		key := strconv.Itoa(1000 + i)
		entries[key] = activityVisitorEntry(0, now.Add(time.Hour), 4006)
		order = append(order, key)
	}
	in := initWithActivityVisitors(entries, order...)
	client, stop := serveVisitorCmds(t, visitorOK)
	if err := claimActivityVisitors(client, in, now); err != nil {
		t.Fatalf("claimActivityVisitors() = %v, want nil", err)
	}
	if got := stop(); len(got) != maxActivityVisitorsPerRun {
		t.Errorf("server saw %d claims, want the per-run cap %d", len(got), maxActivityVisitorsPerRun)
	}
}
