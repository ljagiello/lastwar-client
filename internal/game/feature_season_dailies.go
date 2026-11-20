package game

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season dailies (MASTER.md §4 #18), static-only, sent only while the account is in a season.
//
//   - Desert-season power milestones (achievements v1): lw.season.user.max.force.reward.info {} ->
//     maxForceValue, list[{score, receive}]; lw.season.user.max.force.reward.get.all {} when a
//     milestone at or below maxForceValue is unclaimed (SeasonRewardDataManager.lua:245-266,
//     296-330). The 1.0.370 client sends the v1 info only from the panel it registers under
//     SeasonUtil.IsInSeasonDesertMode() (LWSeasonRewardView.lua:24-26, LWSeason5RewardView.lua:
//     20-21, LWS6RewardView.lua:16-17, LWSeason7RewardView.lua:24-25), so it is sent only when the
//     season config (lw_season[seasonConfigId]) is type 1, Desert (SeasonUtil.lua:184-186). Every
//     other season with a season_achievements group uses achievements v2, which the opt-in
//     season-achievements feature below claims; the v2 panel's claim-all button is hidden
//     (LWSeasonPersonalRewardNew.lua:38), so no v2 season sends the v1 pair (MASTER B2).
//   - Military daily: military.act.info {} -> dailyRewardNum, militaryLevel, isDailySettleIng;
//     military.get.daily.reward {viewLevel: Int max(1, militaryLevel)} while dailyRewardNum < 1,
//     the SeasonMilitary activity (389) runs in its normal phase (seasonMilitaryNormal) and no
//     daily settlement is in progress (SeasonMilitaryManager.lua:51-78, 343-354;
//     SeasonMilitaryInfoData.lua:22-35, 72-74; MilitaryGetDailyRewardMessage.lua:6). The manager
//     has no season-number test: Season 6 (1200101) and Season 7 (1200224, the S7 screen
//     UILWSeasonMilitaryDailyRewardCompS7.lua:103-104) send the same pair (MASTER B7).
//   - Pre-season tasks: view.season.pre.task {activityId: UtfString} -> seasonPreTaskArr[]; a task
//     in state 1 is claimed with season.pre.task.get.reward {activityId: UtfString, taskId:
//     UtfString} (SeasonPreviewManager.lua:82-96, 139-148). These run in the season-preview activity
//     (248) before a season starts, so they are gated on that activity, not on the season.
//
// Not sent: the season week card's free chest and daily (the opt-in season-week-card feature,
// feature_season_week_card.go); receive.season.week.buff.reward
// and season.force.get.all.reward (no sender in 1.0.364); get.chicken.daily.reward (an S5 tap-the-
// chicken claim the client paces by an animation, up to 50 a day: needs a live check of server
// pacing); season.tower.reward / season.tower.box.award (their gates need season_tower tables;
// listed for a follow-up).
const (
	seasonMaxForceInfoCmd   = "lw.season.user.max.force.reward.info"
	seasonMaxForceClaimCmd  = "lw.season.user.max.force.reward.get.all"
	seasonMilitaryInfoCmd   = "military.act.info"
	seasonMilitaryDailyCmd  = "military.get.daily.reward"
	seasonMilitaryUpCmd     = "military.up.level"
	seasonPreTaskInfoCmd    = "view.season.pre.task"
	seasonPreTaskRewardCmd  = "season.pre.task.get.reward"
	seasonAchieveInfoCmd    = "user.season.achievement.v2.info"
	seasonAchieveRewardCmd  = "user.season.achievement.v2.reward"
	seasonPreTaskCanReceive = 1

	seasonAchievementsFeature = "season-achievements"
	// seasonMilitaryRankFeature sorts before season-dailies, so in their shared run phase
	// (featuresInRunOrder: phase, then name) the rank-up runs first and season-dailies claims the
	// daily salary at the new rank, the order the client's red dots offer them in.
	seasonMilitaryRankFeature = "military-rank-up"
)

// SeasonMapType values the season features compare lw_season's type with (EnumType.lua:3884-3895).
const (
	seasonMapDesert   = 1
	seasonMapDarkness = 5
)

// seasonAchievementFlag* is SeasonAchivementFlag (EnumType.lua:15413-15417).
const (
	seasonAchievementFlagPersonal = 0
	seasonAchievementFlagAlliance = 1
)

// seasonAchievementBuilderGroups are the builder-alliance achievement groups. The container uses
// them only for its "farmer" panel, whose claim asks for two confirmations
// (UILWSingleActivityContainerView.lua:97-102; LWSeasonPersonalRewardItem.lua:88-108); they are
// never claimed.
var seasonAchievementBuilderGroups = []int32{3000, 6000}

//go:generate go run ./genseason -season ${LASTWAR_TABLES}/lw_season.json -achievements ${LASTWAR_TABLES}/season_achievements.json -military ${LASTWAR_TABLES}/lw_season_military_level.json -trends ${LASTWAR_TABLES}/lw_season_trends.json -activity ${LASTWAR_TABLES}/activity.json -bloodplan ${LASTWAR_TABLES}/season_blood_night_plan.json -goldtree ${LASTWAR_TABLES}/season_golden_tree.json -version ${LASTWAR_TABLE_VERSION} -out season_tables_gen.go

// seasonMilitaryNormal is SeasonMilitaryManager:GetCurActState() == Normal: now before the show
// start (readyTime when above 0, else startTime) plus para_1 days of the activity's table row
// (SeasonMilitaryManager.lua:51-57, 120-130; ActivityInfoData.lua:358, 447-453). After it the
// activity is in end-show: the client shows no daily panel (UILWSeasonMilitaryMain.lua:360-366)
// and no daily or rank-up red dot (SeasonMilitaryManager.lua:70-101). para_1 is checknumber'd, so a
// cell that is no number counts as 0 days and the activity is never normal once it started.
func seasonMilitaryNormal(a Activity) bool {
	days, err := strconv.ParseFloat(strings.TrimSpace(a.Para1()), 64)
	if err != nil {
		slog.Info("season military: activity para_1 is no number; its normal phase ended at its start",
			"id", a.ID, "para_1", a.Para1())
		days = 0
	}
	start := a.StartTime()
	if ready, _ := evNum(a.Raw, "readyTime"); ready > 0 {
		start = ready
	}
	return float64(evNow().UnixMilli()) < float64(start)+days*86400000
}

func init() {
	registerFeature(Feature{
		Name:    "season-dailies",
		Summary: "in season: Desert-season power-milestone claim-all, military daily (S6/S7); pre-season task rewards; static-only",
		Run:     runSeasonDailies,
	})
	registerFeature(Feature{
		Name:    seasonAchievementsFeature,
		Summary: "OPT-IN: in a season with an achievements v2 group (S1-S7), claim reached personal and alliance achievement grades (static-only, not validated live)",
		Run:     runSeasonAchievements,
	})
	registerFeature(Feature{
		Name:    seasonMilitaryRankFeature,
		Summary: "OPT-IN: in season, take the free manual military rank-ups the merit score unlocks, before season-dailies claims the daily salary (static-only, not validated live)",
		Run:     runSeasonMilitaryRankUp,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, seasonMaxForceClaimCmd, seasonMilitaryDailyCmd, seasonPreTaskRewardCmd)
}

// evSeasonID is the season the account is playing, or 0 outside one. It is SeasonUtil.IsInSeason(true)
// without the lw_season table checks (SeasonUtil.lua:62-84; SeasonInfoTemplate.lua:13-63, 267-281):
// init playerServerSeasonInfo (the source server's season) is open with now before its
// seasonEndTime (ms), and curServerSeasonInfo (the login server's) is open with the same end.
func evSeasonID(in *Init) int64 {
	p, s := in.Object("playerServerSeasonInfo"), in.Object("curServerSeasonInfo")
	if p == nil || s == nil {
		return 0
	}
	id, _ := evNum(p, "seasonId")
	pEnd, ok1 := evNum(p, "seasonEndTime")
	sEnd, ok2 := evNum(s, "seasonEndTime")
	if id <= 0 || !ok1 || !ok2 || pEnd != sEnd || evNow().UnixMilli() >= pEnd || !evBool(p, "open") || !evBool(s, "open") {
		return 0
	}
	return id
}

// evSeasonConfig returns the lw_season row of init playerServerSeasonInfo.seasonConfigId, the
// config a running season reads (SeasonInfoTemplate.lua:18, 26-35, 69-72; SeasonDataManager:
// GetSeasonConfig, SeasonDataManager.lua:1030-1035). The client's SeasonUtil.GetSeasonType() reads
// the login server's config instead (SeasonUtil.lua:351-369); evSeasonID already requires both
// servers to run the same season. id is the config id read (0 when absent); ok is false when
// it is absent or table lw_season lacks it.
func evSeasonConfig(in *Init) (row seasonConfigRow, id int64, ok bool) {
	id, ok = evNum(in.Object("playerServerSeasonInfo"), "seasonConfigId")
	if !ok || id <= 0 {
		return seasonConfigRow{}, id, false
	}
	row, ok = seasonConfigTable[int32(id)]
	return row, id, ok
}

// evBool reads a Bool, or a 0/1 number, as the Lua's truthiness of `== true`/`== 1`.
func evBool(o *sfs.SFSObject, key string) bool {
	v, ok := o.Get(key)
	if !ok {
		return false
	}
	if b, ok := v.Val.(bool); ok {
		return b
	}
	n, ok := evNum(o, key)
	return ok && n == 1
}

func runSeasonDailies(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	b := &evBatch{conn: conn}
	s := Activities(conn, in)
	if season := evSeasonID(in); season > 0 {
		claimSeasonPowerMilestones(b, in, season)
		if mil := s.Open(actTypeSeasonMilitary); len(mil) > 0 && !b.dead && seasonMilitaryNormal(mil[0]) {
			claimSeasonMilitary(b)
		}
	}
	for _, a := range s.Open(actTypeSeasonPreview) {
		if b.dead {
			break
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("activityId", a.IDString())
		info, err := s.Info("season pre-task info response", seasonPreTaskInfoCmd, a, p)
		if err != nil {
			b.add(err)
			continue
		}
		for _, t := range evObjects(info, "seasonPreTaskArr") {
			if st, _ := evNum(t, "state"); st != seasonPreTaskCanReceive || b.dead {
				continue
			}
			taskID := evString(t, "taskId")
			if taskID == "" {
				continue
			}
			p := sfs.NewSFSObject()
			p.PutUtfString("activityId", a.IDString())
			p.PutUtfString("taskId", taskID)
			b.send(fmt.Sprintf("season pre-task %s", taskID), seasonPreTaskRewardCmd, p)
		}
	}
	return b.err()
}

// claimSeasonPowerMilestones sends the v1 achievement pair in a Desert season only. Each run logs
// the season config it went by, so a new season shows up in the logs.
func claimSeasonPowerMilestones(b *evBatch, in *Init, season int64) {
	cfg, cfgID, ok := evSeasonConfig(in)
	if !ok {
		slog.Warn("season-dailies: init playerServerSeasonInfo.seasonConfigId is missing or not in table lw_season; "+
			"the season type is unknown, so no power-milestone claim", "season", season, "seasonConfigId", cfgID,
			"tableVersion", seasonTablesVersion)
		return
	}
	slog.Info("season-dailies: season config", "season", season, "seasonConfigId", cfgID, "type", cfg.typ,
		"type1", cfg.type1, "achievementsGroup", cfg.achievements)
	if cfg.typ != seasonMapDesert {
		return // achievements v2 (season-achievements) or the untraced S1/S2 two-tab panel: never v1
	}
	msg := b.send("season power milestones", seasonMaxForceInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	maxForce, _ := evNum(msg.Params, "maxForceValue")
	for _, m := range evObjects(msg.Params, "list") {
		score, ok := evNum(m, "score")
		if ok && score <= maxForce && !evBool(m, "receive") {
			b.send("season power milestone rewards", seasonMaxForceClaimCmd, sfs.NewSFSObject())
			return
		}
	}
}

func claimSeasonMilitary(b *evBatch) {
	msg := b.send("season military info", seasonMilitaryInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	n, ok := evNum(msg.Params, "dailyRewardNum")
	if !ok || n >= 1 || evBool(msg.Params, "isDailySettleIng") {
		return
	}
	level, _ := evNum(msg.Params, "militaryLevel")
	p := sfs.NewSFSObject()
	p.PutInt("viewLevel", int32(max(1, level)))
	b.send("season military daily", seasonMilitaryDailyCmd, p)
}

// runSeasonAchievements claims the achievements v2 grades of the running season (MASTER B2's
// claim half, opt-in because both commands are new). The client asks for every achievement of
// the season's group at login once HQ is level 3 (SeasonRewardDataManager.lua:45-93):
// user.season.achievement.v2.info {achievementId: Int} (UserSesaonAchievementV2InfoMesage.lua:4-7)
// -> achievementId, gradeValue, gradesInfo[{grade, reward}], userRewards[grade...] (:1197-1236).
// A grade is claimable when grade <= gradeValue and it is not in userRewards (:1220-1226); the
// panel's row then sends user.season.achievement.v2.reward {achievementId: Int, grade: Int}
// (LWSeasonPersonalRewardItem.lua:77-78; UserSesaonAchievementV2RewardMessage.lua:4-8). The group
// is lw_season[seasonConfigId].season_achievements while the season runs (SeasonUtil.lua:
// 1166-1174); the rows are the panel's: not_show != 1 (LWSeasonPersonalRewardNew.lua:126-136),
// flag personal, or alliance while in one (SeasonRewardDataManager.lua:84-89). Camp rows and the
// not_show rows (7003, 8003) belong to other screens and are left to the player.
func runSeasonAchievements(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	season := evSeasonID(in)
	if season <= 0 {
		return nil
	}
	if lv := activityHQLevel(in); lv < 3 {
		slog.Info(seasonAchievementsFeature+": HQ below level 3; the client loads no achievements", "hqLevel", lv)
		return nil
	}
	cfg, cfgID, ok := evSeasonConfig(in)
	if !ok {
		slog.Warn(seasonAchievementsFeature+": init playerServerSeasonInfo.seasonConfigId is missing or not in table lw_season; skipping",
			"season", season, "seasonConfigId", cfgID, "tableVersion", seasonTablesVersion)
		return nil
	}
	group := cfg.achievements
	if group == 0 || slices.Contains(seasonAchievementBuilderGroups, group) {
		slog.Info(seasonAchievementsFeature+": the season config has no achievements v2 group; nothing to claim",
			"season", season, "seasonConfigId", cfgID, "group", group)
		return nil
	}
	inAlliance := claimInAlliance(in)
	b := &evBatch{conn: conn}
	for _, r := range seasonAchievementTable {
		if b.dead {
			break
		}
		if r.group != group || r.notShow == 1 ||
			(r.flag != seasonAchievementFlagPersonal && (r.flag != seasonAchievementFlagAlliance || !inAlliance)) {
			continue
		}
		claimSeasonAchievement(b, r.id)
	}
	return b.err()
}

// claimSeasonAchievement reads one achievement and claims its reached, unclaimed grades in order;
// an error stops that achievement.
func claimSeasonAchievement(b *evBatch, id int32) {
	p := sfs.NewSFSObject()
	p.PutInt("achievementId", id)
	msg := b.send(fmt.Sprintf("%s: achievement %d info", seasonAchievementsFeature, id), seasonAchieveInfoCmd, p)
	if msg == nil {
		return
	}
	value, ok := evNum(msg.Params, "gradeValue")
	claimed := map[int64]bool{}
	for _, g := range evNums(msg.Params, "userRewards") {
		claimed[g] = true
	}
	var due []int64
	grades := evObjects(msg.Params, "gradesInfo")
	for _, g := range grades {
		if grade, has := evNum(g, "grade"); has && ok && grade <= value && !claimed[grade] {
			due = append(due, grade)
		}
	}
	slog.Info(seasonAchievementsFeature+": achievement", "achievementId", id, "gradeValue", value, "hasGradeValue", ok,
		"grades", len(grades), "claimed", len(claimed), "due", due)
	for _, grade := range due {
		if b.dead {
			return
		}
		p := sfs.NewSFSObject()
		p.PutInt("achievementId", id)
		p.PutInt("grade", int32(grade))
		_, err := claimAndLog(b.conn, fmt.Sprintf("%s: achievement %d grade %d", seasonAchievementsFeature, id, grade),
			seasonAchieveRewardCmd, p)
		if err != nil {
			b.add(err)
			return
		}
	}
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
