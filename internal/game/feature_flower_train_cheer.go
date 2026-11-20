package game

import (
	"log/slog"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Flower Train cheer boxes in the Collect Rewards panel (OPT-IN, MASTER 1.0.370 G23; aud-dailies F6,
// cov-ui-city F2 and their verdicts). Static-only: from the 1.0.370 Lua and tables 40413, not sent
// live yet.
//
// The panel's Claim button sends gather.collect.reward with every listed uuid (the collect-rewards
// feature) and then always runs CheckClaimFlowerCheerReward (UICollectRewardView.lua:288-320), which
// sends `flower.train.cheer.box.batch {}` (no params, reply reward;
// FlowerTrainCheerBoxBatchMessage.lua:4-17) when a listed cell is a cheer box
// (IsExistFlowerTrainCheerReward, :349-367). A cheer box is an init collect_reward entry
// (CollectRewardData.lua:18-52) of type CollectRewardType.DISCOVER_SUPPLIES (21,
// Global/EnumType.lua:13098) whose contentId "configId|serverId|boxUid" names an ice_supplies row of
// type WorldSuppliesType.FlowerTrainCheerBoxType (7, Global/EnumType.lua:16476)
// (SpecialCollectCellCheck, UICollectRewardView.lua:176-197). The panel lists only entries with
// expireTime > now (GetRewardListBySort, CollectRewardDataManager.lua:115-136). The cheer cells are
// ordinary collect entries, so gather.collect.reward already carries their uuids; whether it also
// pays them is not decidable from the client, which sends both. Free, no march.
//
// The feature's name sorts after collect-rewards, so in the same run phase it runs after it, as the
// client sends the batch after the gather claim. No error code is registered as benign: the first
// reply is logged and an error stops the claim.
const (
	flowerTrainCheerFeature = "flower-train-cheer-boxes"
	flowerTrainCheerCmd     = "flower.train.cheer.box.batch"

	flowerTrainCheerCollectType = 21 // CollectRewardType.DISCOVER_SUPPLIES
	flowerTrainCheerSupplyType  = 7  // WorldSuppliesType.FlowerTrainCheerBoxType

	// flowerTrainCheerTableVersion is the table flowerTrainCheerSupplyTypes was taken from.
	flowerTrainCheerTableVersion = "40413"
)

// flowerTrainCheerSupplyTypes is table ice_supplies (tables flowerTrainCheerTableVersion, all 73
// rows): id -> type. The whole type column is kept, not only the type-7 ids, so a supply id newer
// than the table is told apart from a known non-cheer one and logged.
var flowerTrainCheerSupplyTypes = map[int64]int64{
	1: 1, 2: 1, 3: 1, 4: 1, 5: 1, 6: 1, 7: 1, 8: 1, 9: 1, 10: 1, 11: 1, 12: 1, 13: 1, 14: 1, 21: 2,
	22: 2, 23: 2, 24: 2, 25: 2, 26: 2, 27: 2, 28: 2, 29: 2, 30: 2, 31: 3, 32: 3, 33: 3, 34: 3, 35: 3,
	36: 3, 37: 3, 41: 2, 42: 2, 43: 2, 44: 2, 45: 2, 46: 2, 47: 2, 48: 2, 49: 2, 50: 2, 61: 5, 62: 4,
	64: 4, 71: 3, 121: 6, 122: 6, 123: 6, 124: 6, 125: 6, 126: 6, 127: 6, 128: 6, 129: 6, 130: 6,
	141: 6, 142: 6, 143: 6, 144: 6, 145: 6, 146: 6, 147: 6, 148: 6, 149: 6, 150: 6, 180: 7, 181: 7,
	182: 7, 183: 7, 184: 7, 185: 7, 186: 7, 187: 7,
}

func init() {
	registerFeature(Feature{
		Name: flowerTrainCheerFeature,
		Summary: "OPT-IN: after the Collect Rewards claim, claim the Flower Train cheer boxes listed there with " +
			flowerTrainCheerCmd + " (static-only, not validated live)",
		Run: runFlowerTrainCheer,
	})
}

// flowerTrainCheerBoxes returns the uuids of the unexpired init collect_reward entries that are
// cheer boxes, and the supply ids of type-21 entries that ice_supplies does not have.
func flowerTrainCheerBoxes(in *Init, nowMs int64) (boxes []int64, unknown []int64) {
	for _, e := range in.Objects("collect_reward") {
		if t, _ := evNum(e, "type"); t != flowerTrainCheerCollectType {
			continue
		}
		if exp, ok := evNum(e, "expireTime"); !ok || exp <= nowMs {
			continue
		}
		first, _, _ := strings.Cut(evString(e, "contentId"), "|")
		id, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
		if err != nil {
			continue
		}
		typ, known := flowerTrainCheerSupplyTypes[id]
		if !known {
			unknown = append(unknown, id)
			continue
		}
		if typ == flowerTrainCheerSupplyType {
			uuid, _ := evNum(e, "uuid")
			boxes = append(boxes, uuid)
		}
	}
	return boxes, unknown
}

func runFlowerTrainCheer(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(flowerTrainCheerFeature + ": no init push; skipping")
		return nil
	}
	boxes, unknown := flowerTrainCheerBoxes(in, evNow().UnixMilli())
	if len(unknown) > 0 {
		slog.Warn(flowerTrainCheerFeature+": supply collect entries whose ice_supplies id is not in the table; "+
			"they wait for a table update", "ids", unknown, "tableVersion", flowerTrainCheerTableVersion)
	}
	if len(boxes) == 0 {
		slog.Info(flowerTrainCheerFeature + ": no Flower Train cheer box in the Collect Rewards panel")
		return nil
	}
	b := &evBatch{conn: conn}
	msg := b.send("flower train cheer boxes", flowerTrainCheerCmd, sfs.NewSFSObject())
	if msg != nil {
		slog.Info(flowerTrainCheerFeature+": first "+flowerTrainCheerCmd+" reply", "boxes", len(boxes),
			"uuids", boxes, "granted", describeGrants(msg.Params), "response", msg.Params.StringRedacted())
	}
	return b.err()
}
