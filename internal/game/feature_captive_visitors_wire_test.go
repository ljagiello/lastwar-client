package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestCaptiveVisitorsWire: two arrived Battle Plan captives (lw_base_visitor_event 22001, behaviour
// type 40 BattlePlanCaptive, EnumType.lua:12061; queue type 9) with uids above 2^32, and a merchant
// (1001), which is left alone. The reply names no removedVisitorUids, so each captive is accepted.
//   - visitor.operate (MsgDefines.lua:1148; VisitorBattlePlanCaptive.lua:53-55 takes VisitorGift's
//     accept, sent at VisitorGift.lua:168 with operate 1) {uid: Long, operate: Int 1}
//     (VisitorOperateMessage.lua:5-9).
func TestCaptiveVisitorsWire(t *testing.T) {
	captureVisitorLog(t)
	arrived := time.Now().Add(-time.Hour)
	const a, b = int64(1)<<40 + 1, int64(1)<<40 + 2
	in := freshTestInit(5, int32(1), queuedTestVisitor(a, 22001, 9, arrived), queuedTestVisitor(7, 1001, 0, arrived),
		queuedTestVisitor(b, 22001, 9, arrived))
	client, stop := serveVisitorCmds(t, func(req visitorRequest) *sfs.SFSObject {
		if req.cmd != "visitor.operate" {
			return rewardErr("unexpected command")
		}
		return captiveReply(nil)(req)
	})
	if err := runCaptiveVisitors(client, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aVisitorSent(stop()), []aWireReq{
		{cmd: "visitor.operate", params: []aWireParam{{"uid", sfs.SFSLong, a}, {"operate", sfs.SFSInt, int32(1)}}},
		{cmd: "visitor.operate", params: []aWireParam{{"uid", sfs.SFSLong, b}, {"operate", sfs.SFSInt, int32(1)}}},
	})
}
