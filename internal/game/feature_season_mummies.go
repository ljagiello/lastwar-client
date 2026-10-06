package game

import (
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season mummies (S3-S6), opt-in and static-only: lw.season.mummy.get {} while the season's mummy
// yard is built and init mummyWaitRecInfo[].armyNum sums above 0 (BuildBubbleManager.lua:3218-3266,
// 4440-4448; SeasonMummyDataManager.lua:29-42). It turns waiting fallen troops into mummy soldiers,
// which changes the army's composition, so unlike the other season collectors (season-output) it
// is a player's choice and stays off unless the session config enables it. The server enforces the
// stock cap (effect 94064), which init alone can't give.
const seasonMummyCmd = "lw.season.mummy.get"

// seasonMummyYards maps a season to its mummy-yard building (SeasonUtil.lua:456-480).
var seasonMummyYards = map[int64]int32{3: 787000, 4: 806000, 5: 831000, 6: 851000}

func init() {
	registerFeature(Feature{
		Name:    "season-mummies",
		Summary: "OPT-IN: in season (S3-S6), turn waiting fallen troops into mummy soldiers (changes the army); static-only",
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
