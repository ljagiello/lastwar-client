package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestCityGarbageWire: init cityPoints holds two garbage reward piles (CityPointType.GarbageReward 4,
// EnumType.lua:8894) with uuids above 2^32 and a monster reward point (MonsterReward 3, :8893), which
// sends nothing.
//   - receive.city.garbage.reward (MsgDefines.lua:672; sent at UIUtil.lua:3116) {uuid: Long}
//     (ReceiveCityGarbageRewardMessage.lua:4-9).
func TestCityGarbageWire(t *testing.T) {
	noCityGarbageSpacing(t)
	captureVisitorLog(t)
	const a, b = int64(1)<<40 + 101, int64(1)<<40 + 106
	in := cityGarbageInit(cityPoint(a, 4, 1), cityPoint(1<<40+103, 3, 1), cityPoint(b, 4, 2))
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "receive.city.garbage.reward" {
			return rewardOK()
		}
		return rewardErr("unexpected command")
	})
	if err := runCityGarbage(conn, in); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aRewardSent(fake), []aWireReq{
		{cmd: "receive.city.garbage.reward", params: []aWireParam{{"uuid", sfs.SFSLong, a}}},
		{cmd: "receive.city.garbage.reward", params: []aWireParam{{"uuid", sfs.SFSLong, b}}},
	})
}
