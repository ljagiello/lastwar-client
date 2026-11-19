package game

import (
	"fmt"
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// City garbage reward piles (MASTER G38, cov-core/F4), OPT-IN. Init cityPoints[] holds the city's
// map points {uuid, pointId, type, itemId, reward} (CityPointDataManager.lua:46-56,
// CityPointData.lua:21-35; fed from InitMessage.lua:267). A point of type 4, GarbageReward
// (CityPointType, Global/EnumType.lua:8889-8901), is the pile a picked-up garbage point leaves
// behind; tapping it with a reward list sends `receive.city.garbage.reward {uuid: Long}`
// (Util/UIUtil.lua:3088-3117; ReceiveCityGarbageRewardMessage.lua:4-8). The tap's storage check,
// ResourceItemDataManager:CheckIsStorageFull, always returns false in 1.0.370
// (ResourceItemDataManager.lua:256-258), so it gates nothing. The pile also has to sit on an
// unlocked fog tile (CityPointDataManager.lua:164, 196), which init does not say; an errorCode is
// therefore possible, and it stops the run's claims (no benign code is registered for a never-sent
// command). Type 3, MonsterReward, sends nothing itself in 1.0.370 (UIUtil.lua:3044-3087) and is
// only counted.
//
// Early-game content: a mature city normally has no piles, so every run first logs how many type-3
// and type-4 points init has. Only the piles that existed at login are seen. Static-only, not
// validated live.
const (
	cityGarbageFeature = "city-garbage"
	cityGarbageCmd     = "receive.city.garbage.reward"

	cityPointMonsterReward = 3 // CityPointType.MonsterReward
	cityPointGarbageReward = 4 // CityPointType.GarbageReward
)

// cityGarbageSpacing is the pause between two pile claims.
var cityGarbageSpacing = time.Second

func init() {
	registerFeature(Feature{
		Name:      cityGarbageFeature,
		Summary:   "OPT-IN: claim the city's garbage reward piles from init cityPoints (receive.city.garbage.reward) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runCityGarbage,
	})
}

// cityGarbagePiles returns the uuids of the type-4 points with a non-empty reward array, and logs
// the type-3 and type-4 counts.
func cityGarbagePiles(in *Init) []int64 {
	var piles []int64
	var garbage, monster, noReward int
	for _, p := range claimObjectsOrMap(in.Raw, "cityPoints") {
		switch t, _ := claimInt(p, "type"); t {
		case cityPointMonsterReward:
			monster++
		case cityPointGarbageReward:
			garbage++
			uuid, ok := claimInt(p, "uuid")
			if !ok || uuid == 0 || len(claimObjects(p, "reward")) == 0 {
				noReward++
				continue
			}
			piles = append(piles, uuid)
		}
	}
	slog.Info(cityGarbageFeature+": init city points", "garbageReward", garbage, "monsterReward", monster,
		"garbageWithoutReward", noReward, "points", len(claimObjectsOrMap(in.Raw, "cityPoints")))
	return piles
}

func runCityGarbage(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(cityGarbageFeature + ": no init push; skipping")
		return nil
	}
	for i, uuid := range cityGarbagePiles(in) {
		if i > 0 {
			time.Sleep(cityGarbageSpacing)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s %d", cityGarbageFeature, uuid), cityGarbageCmd, p); err != nil {
			return err
		}
	}
	return nil
}
