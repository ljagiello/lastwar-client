package game

import (
	"slices"
	"testing"
	"time"
)

func TestCockatriceVisitorsIsOptIn(t *testing.T) { assertCityGapFeature(t, cockatriceVisitorsFeature) }

// Only behaviour type 13 (row 9002) is accepted; a congratulation visitor (14, row 14001) waits for
// the user's ruling and a recruit (3) is the core greet's.
func TestCockatriceVisitorsAcceptsOnlyType13(t *testing.T) {
	captureVisitorLog(t)
	now := time.Now()
	arrived := now.Add(-time.Hour)
	in := freshTestInit(5, int32(1),
		queuedTestVisitor(1, 9002, 0, arrived),  // DOMINATOR_COCKATRICE
		queuedTestVisitor(2, 14001, 6, arrived), // AllianceCongratulation
		queuedTestVisitor(3, 3115, 1, arrived),  // RECRUITMENT
		queuedTestVisitor(4, 22001, 9, arrived)) // BattlePlanCaptive
	client, stop := serveVisitorCmds(t, visitorOK)
	if err := runCockatriceVisitors(client, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	var uids []int64
	for _, r := range stop() {
		if r.cmd != "visitor.operate" || r.params.GetInt("operate") != 1 {
			t.Errorf("sent %s %s, want visitor.operate operate 1", r.cmd, r.params.StringRedacted())
		}
		uids = append(uids, r.params.GetLong("uid"))
	}
	if !slices.Equal(uids, []int64{1}) {
		t.Errorf("accepted uids %v, want [1]", uids)
	}
	if !in.Visitors[0].greeted || in.Visitors[1].greeted || in.Visitors[2].greeted || in.Visitors[3].greeted {
		t.Error("greeted marks wrong: want only the type-13 visitor")
	}
}

func TestCockatriceVisitorsNoInit(t *testing.T) {
	client, stop := serveVisitorCmds(t, visitorOK)
	if err := runCockatriceVisitors(client, &Init{}); err != nil {
		t.Errorf("run = %v, want nil", err)
	}
	if got := stop(); len(got) != 0 {
		t.Errorf("sent %v, want nothing", got)
	}
}
