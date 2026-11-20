package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// aRadarRescue is a FINISHED rescue event (detect_event 199, type 11) whose reward holds the given
// entries {type, value {id, num}}, each given as {type, id, num}.
func aRadarRescue(uuid int64, entries ...[3]int64) *sfs.SFSObject {
	ev := detectEv(uuid, 199, 1) // DETECT_EVENT_STATE_FINISHED (EnumType.lua:8516)
	rw := sfs.NewSFSArray()
	for _, e := range entries {
		v := sfs.NewSFSObject()
		v.PutInt("id", int32(e[1]))
		v.PutLong("num", e[2])
		o := sfs.NewSFSObject()
		o.PutInt("type", int32(e[0]))
		o.PutSFSObject("value", v)
		rw.AddSFSObject(o)
	}
	ev.PutSFSArray("reward", rw)
	return ev
}

// TestRadarRescueClaimsWire: radar-rescue-claims reads the radar and claims one finished rescue.
//   - get.detect.info (MsgDefines.lua:655) {openWnd: Bool false}: DetectInfoGetMessage.lua:5-11
//     puts openWnd, and RadarCenterDataManager.lua:437 sends it with false (:440-441).
//   - receive.detect.event.reward (MsgDefines.lua:657; sent at RadarCenterDataManager.lua:1301)
//     {uuid: Long} (DetectEventRewardReceiveMessage.lua:5-8). The uuid is above 2^32, so an Int would
//     not carry it.
func TestRadarRescueClaimsWire(t *testing.T) {
	const uuid = int64(1)<<40 + 11
	ev := aRadarRescue(uuid, [3]int64{27, 3010, 40}) // RewardType.RESOURCE_ITEM 27 (EnumType.lua:169), lw_soldier 3010
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, ev)}, nil)
	if err := runRadarRescueClaims(conn, rescueInit(1000, 500)); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(f), []aWireReq{
		{cmd: "get.detect.info", params: []aWireParam{{"openWnd", sfs.SFSBool, false}}},
		{cmd: "receive.detect.event.reward", params: []aWireParam{{"uuid", sfs.SFSLong, uuid}}},
	})
}
