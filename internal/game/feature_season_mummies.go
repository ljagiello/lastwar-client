package game

import (
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season mummies (S3-S7), opt-in and static-only: lw.season.mummy.get {} while the season's mummy
// yard is built and init mummyWaitRecInfo[].armyNum sums above 0 (BuildBubbleManager.lua:3218-3266,
// 4440-4448; SeasonMummyDataManager.lua:29-42). It turns waiting fallen troops into mummy soldiers,
// which changes the army's composition, so unlike the other season collectors (season-output) it
// is a player's choice and stays off unless the session config enables it. The server enforces the
// stock cap (effect 94064), which init alone can't give.
const seasonMummyCmd = "lw.season.mummy.get"

// seasonMummyYards maps a season to its mummy-yard building. The client picks the yard by the
// season's map type (GetMummyYardBuildingId, SeasonUtil.lua:527-552; IsMummyYardBuilding,
// :394-419): Mummy (S3) 787000, Darkness (S4) 806000, NineNation (S5) 831000,
// NineNationRainforest (S6) 851000 and MummyV2 (S7) LW_BUILD_ARMY_YARD_MUMMY_S7 858000
// (EnumType.lua:3744-3747, 3775). Table lw_season 40413 gives seasons 3 to 7 one type each, so
// the season number keys the same yard.
var seasonMummyYards = map[int64]int32{3: 787000, 4: 806000, 5: 831000, 6: 851000, 7: 858000}

func init() {
	registerFeature(Feature{
		Name:    "season-mummies",
		Summary: "OPT-IN: in season (S3-S7), turn waiting fallen troops into mummy soldiers (changes the army); static-only",
		Run:     runSeasonMummies,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, seasonMummyCmd)
}

func runSeasonMummies(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	yard, ok := seasonMummyYards[evSeasonID(in)]
	if !ok || !evHasBuilding(in, yard) {
		return nil
	}
	var waiting int64
	for _, w := range in.Objects("mummyWaitRecInfo") {
		n, _ := evNum(w, "armyNum")
		waiting += n
	}
	if waiting <= 0 {
		return nil
	}
	b := &evBatch{conn: conn}
	b.send("season mummies", seasonMummyCmd, sfs.NewSFSObject())
	return b.err()
}
