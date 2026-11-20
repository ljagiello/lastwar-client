package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// aCityRewardRun answers world.all.city.reward.info with cities and each claim with fwClaimOK, and
// returns the claimed cityIds.
func aCityRewardRun(t *testing.T, cities ...*sfs.SFSObject) []int32 {
	t.Helper()
	withFwPace(t)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "world.all.city.reward.info":
			return obj("cityArr", cities)
		case "world.alliance.city.first.occupied.reward":
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
	if err := runCityFirstOccupation(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	var ids []int32
	for _, r := range fake.only("world.alliance.city.first.occupied.reward") {
		ids = append(ids, r.Params.GetInt("cityId"))
	}
	return ids
}

// TestCityFirstOccupationWire: cityArrReply lists 2001 (unclaimed alliance reward) and 2002
// (unclaimed member reward) among cities with nothing to claim.
//   - world.all.city.reward.info (MsgDefines.lua:959; sent at WorldAllianceCityDataManager.lua:160),
//     no params (WorldAllCityRewardInfoMessage.lua:4-6).
//   - world.alliance.city.first.occupied.reward (MsgDefines.lua:962; sent at UISiegeSuccessView.lua:301)
//     {cityId: Int} (WorldAllianceCityFirstOccupiedRewardMessage.lua:4-7).
func TestCityFirstOccupationWire(t *testing.T) {
	withFwPace(t)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "world.all.city.reward.info":
			return cityArrReply()
		case "world.alliance.city.first.occupied.reward":
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
	if err := runCityFirstOccupation(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "world.all.city.reward.info"},
		{cmd: "world.alliance.city.first.occupied.reward", params: []aWireParam{{"cityId", sfs.SFSInt, int32(2001)}}},
		{cmd: "world.alliance.city.first.occupied.reward", params: []aWireParam{{"cityId", sfs.SFSInt, int32(2002)}}},
	})
}
