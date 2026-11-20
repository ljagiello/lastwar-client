package sfs

// Request keys put on the wire by the 1.0.370 audit's season package (WP10): season-achievements
// (internal/game/feature_season_dailies.go: user.season.achievement.v2.info/.reward),
// bloody-night-tasks (internal/game/feature_season_event_tasks.go: view.blood.night.task,
// blood.night.task.get.reward), season-trends (internal/game/feature_season_trends.go:
// lw.season.trend.reward) and gold-tree-prayer (internal/game/feature_gold_tree.go:
// gold.tree.open.card). Each is a table id, a reward tier or a server timestamp; none carries a
// credential or PII.
func init() {
	for _, k := range []string{
		"achievementId", // season_achievements id (UserSesaonAchievementV2InfoMesage.lua:6)
		"grade",         // an achievement grade value from the info reply (UserSesaonAchievementV2RewardMessage.lua:7)
		"planId",        // season_blood_night_plan id (ViewBloodNightTaskMessage.lua:6)
		"trend_id",      // lw_season_trends id (LWSeasonTrendRewardMessage.lua:6)
		"weekTime",      // the gold tree week's start timestamp from the view reply (GoldTreeOpenCardMessage.lua:6)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
