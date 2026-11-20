package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// dSuppliesStep answers the supplies-search commands by their literal names: each step reaches
// level 1 without failing (SuppliesSearchManager.lua:65-79 reads the reply's suppliesSearchInfo),
// each reward is a plain success; nil for everything else, so startRadarFake answers it.
func dSuppliesStep(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	switch cmd {
	case "building.supplies.search.next":
		return suppliesReply("buildingUuid", p.GetLong("buildingUuid"), 1, 0)
	case "detect.event.supplies.search.next":
		return suppliesReply("eventUuid", p.GetLong("eventUuid"), 1, 0)
	case "building.get.supplies.search.reward", "detect.event.get.supplies.search.reward":
		return evOK()
	}
	return nil
}

// TestSuppliesSearchWire pins what supplies-search sends, as literals from the 1.0.370 client:
//   - building.supplies.search.next {buildingUuid: Long} (MsgDefines.lua:2649;
//     BuildingSuppliesSearchNextMessage.lua:4-7; SuppliesSearchManager.lua:29)
//   - building.get.supplies.search.reward {buildingUuid: Long} (MsgDefines.lua:2650;
//     BuildingGetSuppliesSearchRewardMessage.lua:4-7; SuppliesSearchManager.lua:37)
//   - get.detect.info {openWnd: Bool false} (MsgDefines.lua:655; DetectInfoGetMessage.lua:5-12, where a
//     nil argument would send true; RadarCenterDataManager.lua:441)
//   - hero.event.info.get {activityId: UtfString "70000"}, the shared duel read of the radar day check
//     (MsgDefines.lua:528 ActivityEventInfoGet)
//   - detect.event.supplies.search.next {eventUuid: Long} (MsgDefines.lua:2647;
//     DetectEventSuppliesSearchNextMessage.lua:4-7; SuppliesSearchManager.lua:27)
//   - detect.event.get.supplies.search.reward {eventUuid: Long} (MsgDefines.lua:2648;
//     DetectEventGetSuppliesSearchRewardMessage.lua:4-7; SuppliesSearchManager.lua:35)
//
// Building 10150000 is LW_BUILD_SUPPLIES_SEARCH_1 (EnumType.lua:3792). Both uuids are above 2^32, so
// an Int would not hold them.
func TestSuppliesSearchWire(t *testing.T) {
	tripwireTemp(t)
	const building, event = int64(5_000_000_001), int64(6_000_000_002)
	conn, fake := startRadarFake(t, radarScoringDay(),
		[]*sfs.SFSObject{detectReply(20, 10, suppliesEv(event, 0, suppliesInfo(0, 0)))}, dSuppliesStep)
	in := digTestInit(nil)
	in.Buildings = append(in.Buildings, suppliesBuilding(building, 10150000, suppliesInfo(0, 0)))
	if err := runSuppliesSearch(conn, in); err != nil {
		t.Fatal(err)
	}
	dCheckWire(t, fake, []dWire{
		{"building.supplies.search.next", map[string]dParam{"buildingUuid": dLong(building)}},
		{"building.get.supplies.search.reward", map[string]dParam{"buildingUuid": dLong(building)}},
		{"get.detect.info", map[string]dParam{"openWnd": dBool(false)}},
		{"hero.event.info.get", map[string]dParam{"activityId": dStr("70000")}},
		{"detect.event.supplies.search.next", map[string]dParam{"eventUuid": dLong(event)}},
		{"detect.event.get.supplies.search.reward", map[string]dParam{"eventUuid": dLong(event)}},
	})
}
