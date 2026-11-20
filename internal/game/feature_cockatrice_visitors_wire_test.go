package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestCockatriceVisitorsWire: an arrived DOMINATOR_COCKATRICE visitor (lw_base_visitor_event 9002,
// behaviour type 13, EnumType.lua:12052) with a uid above 2^32, beside a congratulation visitor
// (14001, type 14), which is left alone.
//   - visitor.operate (MsgDefines.lua:1148; sent at VisitorRecruitWorker.lua:65, the class type 13
//     shares with RECRUITMENT, CityVisitorManager.lua:111-112) {uid: Long, operate: Int 1}
//     (VisitorOperateMessage.lua:5-9).
func TestCockatriceVisitorsWire(t *testing.T) {
	captureVisitorLog(t)
	arrived := time.Now().Add(-time.Hour)
	const uid = int64(1)<<40 + 1
	in := freshTestInit(5, int32(1), queuedTestVisitor(uid, 9002, 0, arrived), queuedTestVisitor(2, 14001, 6, arrived))
	client, stop := serveVisitorCmds(t, func(req visitorRequest) *sfs.SFSObject {
		if req.cmd != "visitor.operate" {
			return rewardErr("unexpected command")
		}
		return visitorOK(req)
	})
	if err := runCockatriceVisitors(client, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aVisitorSent(stop()), []aWireReq{
		{cmd: "visitor.operate", params: []aWireParam{{"uid", sfs.SFSLong, uid}, {"operate", sfs.SFSInt, int32(1)}}},
	})
}
