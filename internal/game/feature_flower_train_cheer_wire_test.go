package game

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestFlowerTrainCheerWire pins flower-train-cheer-boxes' wire shape as literals from the 1.0.370
// client: flower.train.cheer.box.batch (MsgDefines.lua:2844) with no params
// (FlowerTrainCheerBoxBatchMessage.lua puts none), sent by UICollectRewardView.lua:353, once for
// all the listed cheer boxes. Entry type 21 is CollectRewardType.DISCOVER_SUPPLIES
// (Global/EnumType.lua:13098), and ice_supplies 183 is a type-7 (FlowerTrainCheerBoxType) row of
// tables 40413.
func TestFlowerTrainCheerWire(t *testing.T) {
	withEvNow(t, evTestNow)
	later := evTestNow.Add(time.Hour)
	in := cheerInit(cheerEntry(2, 21, "183|1203|9001", later), cheerEntry(3, 21, "187|1203|9002", later))
	got := bWireRun(t, "flower-train-cheer-boxes", in, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	bWireCheck(t, got, []bWireReq{bReq("flower.train.cheer.box.batch")})
}
