package game

import (
	"fmt"
	"log/slog"
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
//     season-achievements feature claims (feature_season_achievements.go); the v2 panel's
//     claim-all button is hidden (LWSeasonPersonalRewardNew.lua:38), so no v2 season sends the v1
//     pair (MASTER B2).
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
	seasonPreTaskInfoCmd    = "view.season.pre.task"
	seasonPreTaskRewardCmd  = "season.pre.task.get.reward"
	seasonPreTaskCanReceive = 1
)

// SeasonMapType values the season features compare lw_season's type with (EnumType.lua:3884-3895).
const (
	seasonMapDesert   = 1
	seasonMapDarkness = 5
)

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
