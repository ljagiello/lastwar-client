package game

import (
	"fmt"
	"log/slog"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season achievements v2 (MASTER B2's claim half), OPT-IN, static-only. season-dailies sends the
// v1 power-milestone pair only in a Desert season; every other season with a season_achievements
// group shows the v2 panel, whose two commands have never been sent by this client, so they are
// this separate feature rather than part of the ON season-dailies. No error code is registered as
// benign: the first replies are logged and an error stops that achievement's claims.
const (
	seasonAchievementsFeature = "season-achievements"
	seasonAchieveInfoCmd      = "user.season.achievement.v2.info"
	seasonAchieveRewardCmd    = "user.season.achievement.v2.reward"
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

func init() {
	registerFeature(Feature{
		Name:    seasonAchievementsFeature,
		Summary: "OPT-IN: in a season with an achievements v2 group (S1-S7), claim reached personal and alliance achievement grades (static-only, not validated live)",
		Run:     runSeasonAchievements,
	})
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
