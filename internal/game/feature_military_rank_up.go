package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Military rank-up (MASTER G2), OPT-IN, static-only; see runSeasonMilitaryRankUp. No error code is
// registered as benign: the first replies are logged and an error stops the rank-ups.
const (
	// seasonMilitaryRankFeature sorts before season-dailies, so in their shared run phase
	// (featuresInRunOrder: phase, then name) the rank-up runs first and season-dailies claims the
	// daily salary at the new rank, the order the client's red dots offer them in.
	seasonMilitaryRankFeature = "military-rank-up"
	seasonMilitaryUpCmd       = "military.up.level"
)

func init() {
	registerFeature(Feature{
		Name:    seasonMilitaryRankFeature,
		Summary: "OPT-IN: in season, take the free manual military rank-ups the merit score unlocks, before season-dailies claims the daily salary (static-only, not validated live)",
		Run:     runSeasonMilitaryRankUp,
	})
}

// runSeasonMilitaryRankUp is the free manual military rank-up (MASTER G2). The client's rank-up red
// dot and button (SeasonMilitaryManager.lua:80-110, 337-341, 379-383; UILWSeasonMilitaryUpgradeCompS7
// .lua:152-168) need: no daily settlement in progress, the activity in its normal phase, the level
// below the group's highest manual level (the row with standard_level 1) with a next row, that row's
// need_rank 0, totalMilitaryScore >= its unlock_score and, when it has an unlock_condition_type,
// that condition's progress (militaryConditionArr) >= unlock_condition_para. The request is
// military.up.level {viewLevel: Int current level} (MilitaryUpLevelMessage.lua:6); its reply carries
// militaryLevel, totalMilitaryScore and dailyRewardNum (SeasonMilitaryInfoData.lua:53-64). The
// thresholds are cumulative, so the client shows no cost; that the request spends nothing is still
// not provable statically, so each rank-up logs the score before and after and the loop stops if
// the score drops, the level does not rise, or anything fails.
func runSeasonMilitaryRankUp(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || evSeasonID(in) <= 0 {
		return nil
	}
	mil := Activities(conn, in).Open(actTypeSeasonMilitary)
	if len(mil) == 0 {
		return nil
	}
	a := mil[0]
	if !seasonMilitaryNormal(a) {
		slog.Info(seasonMilitaryRankFeature+": the military activity is past its normal phase; no rank-up", "activity", a.ID)
		return nil
	}
	group, ok := seasonMilitaryActivityGroups[a.ID]
	levels := seasonMilitaryLevels[group]
	if !ok || len(levels) == 0 {
		slog.Info(seasonMilitaryRankFeature+": the activity's para_2 group is not in the generated tables; skipping",
			"activity", a.ID, "group", group, "tableVersion", seasonTablesVersion)
		return nil
	}
	b := &evBatch{conn: conn}
	msg := b.send(seasonMilitaryRankFeature+": military info", seasonMilitaryInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return b.err()
	}
	if evBool(msg.Params, "isDailySettleIng") {
		slog.Info(seasonMilitaryRankFeature + ": daily settlement in progress; no rank-up")
		return nil
	}
	progress := map[int64]int64{}
	for _, c := range evObjects(msg.Params, "militaryConditionArr") {
		t, _ := evNum(c, "conditionType")
		progress[t], _ = evNum(c, "conditionNum")
	}
	level, _ := evNum(msg.Params, "militaryLevel")
	level = max(1, level)
	score, _ := evNum(msg.Params, "totalMilitaryScore")
	for range levels {
		if b.dead || !seasonMilitaryCanRankUp(levels, level, score, progress) {
			break
		}
		p := sfs.NewSFSObject()
		p.PutInt("viewLevel", int32(level))
		r := b.send(fmt.Sprintf("%s: rank up from %d", seasonMilitaryRankFeature, level), seasonMilitaryUpCmd, p)
		if r == nil || r.Params.Has("errorCode") {
			break
		}
		newLevel, _ := evNum(r.Params, "militaryLevel")
		newLevel = max(1, newLevel)
		newScore, hasScore := evNum(r.Params, "totalMilitaryScore")
		slog.Info(seasonMilitaryRankFeature+": ranked up", "activity", a.ID, "levelBefore", level, "levelAfter", newLevel,
			"scoreBefore", score, "scoreAfter", newScore, "dailyRewardNum", evString(r.Params, "dailyRewardNum"))
		if hasScore && newScore < score {
			slog.Warn(seasonMilitaryRankFeature+": totalMilitaryScore dropped after a rank-up, so it is not free; stopping",
				"scoreBefore", score, "scoreAfter", newScore)
			break
		}
		if newLevel <= level {
			break
		}
		level = newLevel
		if hasScore {
			score = newScore
		}
	}
	return b.err()
}

// seasonMilitaryCanRankUp is ShowUpgradeRed's level test for level (the settle and phase tests are
// the caller's): CanManualUpgrade(level) (a next row and level < the standard_level row's level,
// none = 0) and the next row's need_rank, unlock_score and unlock condition
// (SeasonMilitaryManager.lua:86-110, 316-341; SeasonMilitaryInfoData.lua:98-100).
func seasonMilitaryCanRankUp(levels []seasonMilitaryLevelRow, level, score int64, progress map[int64]int64) bool {
	var maxManual int64
	var next *seasonMilitaryLevelRow
	for i, r := range levels {
		if r.standardLevel == 1 && maxManual == 0 {
			maxManual = int64(r.level)
		}
		if int64(r.level) == level+1 {
			next = &levels[i]
		}
	}
	if next == nil || level >= maxManual || next.needRank != 0 || score < int64(next.unlockScore) {
		return false
	}
	return next.conditionType <= 0 || progress[int64(next.conditionType)] >= int64(next.conditionPara)
}
