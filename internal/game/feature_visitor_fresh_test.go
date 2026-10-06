package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// queuedTestVisitor is an init visitor.list entry with a queue type and startTime.
func queuedTestVisitor(uid int64, eventId, queueType int32, startTime time.Time) Visitor {
	v := newTimedTestVisitor(uid, eventId, startTime)
	v.Raw.PutInt("type", queueType)
	return v
}

// freshTestInit builds an init carrying visitor.maxNum (omitted when maxNum < 0), the given
// visitors, and, when effect is non-nil, init.effect["90004"] = effect.
func freshTestInit(maxNum int32, effect any, visitors ...Visitor) *Init {
	raw := sfs.NewSFSObject()
	vo := sfs.NewSFSObject()
	if maxNum >= 0 {
		vo.PutInt("maxNum", maxNum)
	}
	raw.PutSFSObject("visitor", vo)
	eff := sfs.NewSFSObject()
	eff.PutInt("90001", 1)
	switch e := effect.(type) {
	case int32:
		eff.PutInt("90004", e)
	case float64:
		eff.PutDouble("90004", e)
	case string:
		eff.PutUtfString("90004", e)
	}
	raw.PutSFSObject("effect", eff)
	return &Init{Raw: raw, Visitors: visitors}
}

func freshReply(visitorRequest) *sfs.SFSObject {
	nv := sfs.NewSFSObject()
	nv.PutLong("uid", 424242)
	nv.PutInt("eventId", 2003)
	nv.PutLong("startTime", time.Now().Add(5*time.Minute).UnixMilli())
	nv.PutInt("type", 0)
	nv.PutInt("visitorId", 6)
	resp := sfs.NewSFSObject()
	resp.PutSFSObject("addVisitor", nv)
	return resp
}

func TestVisitorFreshIsOptIn(t *testing.T) { assertOptInFeature(t, "visitor-fresh") }

func TestVisitorFreshSendsWhenQueueHasRoom(t *testing.T) {
	now := time.Now()
	arrived := now.Add(-time.Hour)
	cases := map[string]*Init{
		"effect open":        freshTestInit(5, int32(1), queuedTestVisitor(1, 1001, 0, arrived)),
		"effect as double":   freshTestInit(5, 1.0, queuedTestVisitor(1, 1001, 0, arrived)),
		"effect as string":   freshTestInit(5, "2", queuedTestVisitor(1, 1001, 0, arrived)),
		"effect not in init": freshTestInit(5, nil, queuedTestVisitor(1, 1001, 0, arrived)),
		"empty queue":        freshTestInit(5, int32(1)),
		// Queue2 visitors (STAGE 2, SystemGift 8, WORKER_LOTTERY 4) do not take Queue1 slots.
		"queue2 not counted": freshTestInit(2, int32(1),
			queuedTestVisitor(1, 2001, 0, arrived), queuedTestVisitor(2, 21001, 8, now.Add(time.Hour)),
			queuedTestVisitor(3, 5001, 4, arrived)),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			buf := captureVisitorLog(t)
			client, stop := serveVisitorCmds(t, freshReply)
			err := refreshVisitors(client, in, now)
			got := stop()
			if err != nil {
				t.Fatalf("refreshVisitors() = %v, want nil", err)
			}
			if len(got) != 1 || got[0].cmd != "visitor.fresh" || len(session.ParamKeysWithoutID(got[0].params)) != 0 {
				t.Fatalf("server saw %v, want one visitor.fresh with no params", got)
			}
			if want := `msg="visitor-fresh: next visitor scheduled" uid=424242 eventId=2003 type=2 typeName=GIFT greetable=true`; !strings.Contains(buf.String(), want) {
				t.Errorf("log missing %q:\n%s", want, buf.String())
			}
			gap := strings.Contains(buf.String(), "effect 90004 not in init.effect")
			if wantGap := name == "effect not in init"; gap != wantGap {
				t.Errorf("effect-gap log present = %v, want %v:\n%s", gap, wantGap, buf.String())
			}
		})
	}
}

func TestVisitorFreshSkips(t *testing.T) {
	now := time.Now()
	arrived := now.Add(-time.Hour)
	cases := map[string]*Init{
		"nil init":     nil,
		"no init push": {},
		"no maxNum":    freshTestInit(-1, int32(1)),
		"maxNum zero":  freshTestInit(0, int32(1)),
		"queue full": freshTestInit(2, int32(1),
			queuedTestVisitor(1, 2001, 0, arrived), queuedTestVisitor(2, 1001, 1, arrived)),
		"newest still arriving": freshTestInit(5, int32(1),
			queuedTestVisitor(1, 2001, 0, arrived), queuedTestVisitor(2, 2002, 0, now.Add(time.Minute))),
		"newest inside the margin": freshTestInit(5, int32(1),
			queuedTestVisitor(1, 2001, 0, now.Add(-visitorArrivalMargin/2))),
		"function not open": freshTestInit(5, int32(0), queuedTestVisitor(1, 2001, 0, arrived)),
	}
	for name, in := range cases {
		fake := &fakeNetErrConn{}
		if err := refreshVisitors(session.NewGameConnForTest(fake), in, now); err != nil {
			t.Errorf("%s: refreshVisitors() = %v, want nil", name, err)
		}
		if fake.writeCount() != 0 {
			t.Errorf("%s: sent %d requests, want none", name, fake.writeCount())
		}
	}
}

func TestVisitorFreshReportsFailure(t *testing.T) {
	client, stop := serveVisitorCmds(t, func(visitorRequest) *sfs.SFSObject {
		resp := sfs.NewSFSObject()
		resp.PutUtfString("errorCode", "E_FAKE_FRESH")
		return resp
	})
	err := runVisitorFresh(client, freshTestInit(5, int32(1)))
	if got := stop(); len(got) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(got))
	}
	if err == nil || !strings.Contains(err.Error(), "E_FAKE_FRESH") {
		t.Errorf("runVisitorFresh() = %v, want the E_FAKE_FRESH failure reported", err)
	}
}

func TestInitEffect(t *testing.T) {
	in := freshTestInit(5, 1.5)
	if v, ok := initEffect(in, "90004"); !ok || v != 1.5 {
		t.Errorf("initEffect(90004) = %v, %v; want 1.5, true", v, ok)
	}
	if v, ok := initEffect(in, "90001"); !ok || v != 1 {
		t.Errorf("initEffect(90001) = %v, %v; want 1, true", v, ok)
	}
	if _, ok := initEffect(in, "12345"); ok {
		t.Error("initEffect(absent id) ok = true, want false")
	}
	if _, ok := initEffect(&Init{Raw: sfs.NewSFSObject()}, "90004"); ok {
		t.Error("initEffect without init.effect ok = true, want false")
	}
	bad := freshTestInit(5, nil)
	bad.Object("effect").PutBool("90004", true)
	if _, ok := initEffect(bad, "90004"); ok {
		t.Error("initEffect(bool entry) ok = true, want false")
	}
}
