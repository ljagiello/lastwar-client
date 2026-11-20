package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// assertCityGapFeature checks the opt-in contract of a feature added for a 1.0.370 city gap: off by
// default, no Alliance Duel scores, and a static-only summary.
func assertCityGapFeature(t *testing.T, name string) {
	t.Helper()
	f, ok := featureRegistry[name]
	if !ok {
		t.Fatalf("feature %q is not registered", name)
	}
	if f.DefaultOn || len(f.Duel) != 0 || f.Hold || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %q = {DefaultOn %v, Duel %v, Hold %v, Summary %q}, want opt-in, no duel scores, static-only summary",
			name, f.DefaultOn, f.Duel, f.Hold, f.Summary)
	}
}

// captiveTestVisitor is an init visitor.list entry for the Battle Plan captive row 22001 (behaviour
// type 40) in queue type 9.
func captiveTestVisitor(uid int64, start time.Time) Visitor {
	return queuedTestVisitor(uid, 22001, 9, start)
}

// captiveReply answers visitor.operate like CityVisitorManager.lua:435-445 expects: visitorType 9,
// captiveVisitors and removedVisitorUids (nil leaves the field out), and visitor.fresh with
// freshReply.
func captiveReply(removed *sfs.SFSArray) func(visitorRequest) *sfs.SFSObject {
	return func(req visitorRequest) *sfs.SFSObject {
		if req.cmd == "visitor.fresh" {
			return freshReply(req)
		}
		resp := sfs.NewSFSObject()
		resp.PutLong("uid", req.params.GetLong("uid"))
		resp.PutInt("operate", 1)
		resp.PutInt("visitorType", 9)
		captives := sfs.NewSFSArray()
		captives.AddSFSObject(obj("heroId", 1001, "num", 1))
		resp.PutSFSArray("captiveVisitors", captives)
		if removed != nil {
			resp.PutSFSArray("removedVisitorUids", removed)
		}
		return resp
	}
}

func TestCaptiveVisitorsIsOptIn(t *testing.T) { assertCityGapFeature(t, captiveVisitorsFeature) }

// One accept can clear several captives (removedVisitorUids): the cleared ones are not sent again
// and visitor-fresh, which runs after this feature, no longer counts them.
func TestCaptiveVisitorsMarksEveryRemovedUid(t *testing.T) {
	captureVisitorLog(t)
	now := time.Now()
	arrived := now.Add(-time.Hour)
	in := freshTestInit(3, int32(1),
		captiveTestVisitor(1, arrived), captiveTestVisitor(2, arrived), captiveTestVisitor(3, arrived),
		queuedTestVisitor(4, 1001, 0, arrived)) // a merchant: never touched
	client, stop := serveVisitorCmds(t, captiveReply(claimLongSFSArray([]int64{1, 2, 3})))
	if err := greetOptInVisitors(client, in, captiveVisitorsFeature, visitorTypeBattlePlanCaptive, now, captiveVisitorsCleared); err != nil {
		t.Fatalf("greet: %v", err)
	}
	if err := refreshVisitors(client, in, now); err != nil {
		t.Fatalf("refreshVisitors: %v", err)
	}
	var cmds []string
	var uids []int64
	for _, r := range stop() {
		cmds = append(cmds, r.cmd)
		if r.cmd == "visitor.operate" {
			uids = append(uids, r.params.GetLong("uid"))
			if op := r.params.GetInt("operate"); op != 1 {
				t.Errorf("operate = %d, want 1", op)
			}
		}
	}
	if want := []string{"visitor.operate", "visitor.fresh"}; !slices.Equal(cmds, want) {
		t.Errorf("server saw %v, want %v (one accept, then the refill of the emptied queue)", cmds, want)
	}
	if !slices.Equal(uids, []int64{1}) {
		t.Errorf("accepted uids %v, want [1]", uids)
	}
	for i, v := range in.Visitors {
		if want := i < 3; v.greeted != want {
			t.Errorf("visitor %d greeted = %v, want %v", v.Uid(), v.greeted, want)
		}
	}
}

func TestCaptiveVisitorsWithoutRemovedUids(t *testing.T) {
	captureVisitorLog(t)
	now := time.Now()
	arrived := now.Add(-time.Hour)
	in := freshTestInit(5, int32(1),
		captiveTestVisitor(1, arrived),
		captiveTestVisitor(2, now.Add(time.Minute)), // not arrived
		captiveTestVisitor(3, arrived))
	in.Visitors[2].greeted = true // greeted earlier in the run
	client, stop := serveVisitorCmds(t, captiveReply(nil))
	if err := greetOptInVisitors(client, in, captiveVisitorsFeature, visitorTypeBattlePlanCaptive, now, captiveVisitorsCleared); err != nil {
		t.Fatalf("greet: %v", err)
	}
	var uids []int64
	for _, r := range stop() {
		uids = append(uids, r.params.GetLong("uid"))
	}
	if !slices.Equal(uids, []int64{1}) {
		t.Errorf("accepted uids %v, want [1] (2 has not arrived, 3 was already greeted)", uids)
	}
	if !in.Visitors[0].greeted || in.Visitors[1].greeted {
		t.Errorf("greeted = %v %v, want true false", in.Visitors[0].greeted, in.Visitors[1].greeted)
	}
}

// visitor_err_coming is benign for visitor.operate: the captive stays and the next one is tried.
// Any other errorCode stops the feature (no benign code is registered for a never-sent use).
func TestCaptiveVisitorsErrors(t *testing.T) {
	captureVisitorLog(t)
	now := time.Now()
	arrived := now.Add(-time.Hour)
	for name, c := range map[string]struct {
		code    string
		wantErr bool
		want    []int64
	}{
		"not arrived on the server": {"visitor_err_coming", false, []int64{1, 2}},
		"refused":                   {"E_FAKE_CAPTIVE", true, []int64{1}},
	} {
		t.Run(name, func(t *testing.T) {
			in := freshTestInit(5, int32(1), captiveTestVisitor(1, arrived), captiveTestVisitor(2, arrived))
			client, stop := serveVisitorCmds(t, func(visitorRequest) *sfs.SFSObject { return rewardErr(c.code) })
			err := runCaptiveVisitors(client, in)
			var uids []int64
			for _, r := range stop() {
				uids = append(uids, r.params.GetLong("uid"))
			}
			if (err != nil) != c.wantErr {
				t.Errorf("run = %v, want error %v", err, c.wantErr)
			}
			if !slices.Equal(uids, c.want) {
				t.Errorf("accepted uids %v, want %v", uids, c.want)
			}
			if in.Visitors[0].greeted || in.Visitors[1].greeted {
				t.Error("a visitor answered with an errorCode was marked greeted")
			}
		})
	}
}

// The visitor features' greets reach visitor-fresh only because they run first: features run by
// (phase, name) and none of them is in featurePhases.
func TestOptInVisitorFeaturesRunBeforeVisitorFresh(t *testing.T) {
	order := map[string]int{}
	for i, f := range featuresInRunOrder() {
		order[f.Name] = i
	}
	for _, name := range []string{captiveVisitorsFeature, cockatriceVisitorsFeature} {
		if order[name] >= order["visitor-fresh"] {
			t.Errorf("%s runs at %d, after visitor-fresh at %d", name, order[name], order["visitor-fresh"])
		}
	}
}
