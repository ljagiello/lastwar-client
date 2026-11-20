package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season trend rewards (MASTER G3), OPT-IN, static-only.
//
// In season the client asks for the trends at login: lw.season.trend.info {}
// (SeasonDataManager.lua:246-248; LwSeasonTrendInfoMessage.lua:4-14) -> season_group,
// trends[{trend_id, cur_count, reward, day_donate}] (LWSeasonTrendsManager.lua:101-125;
// LWSeasonTrendsData.lua:21-38). A trend's window is not on the wire: it opens at the season start
// (init playerServerSeasonInfo.seasonStartTime, SeasonInfoTemplate.lua:23, 660-662) plus
// (unlock_time - 1) days of its lw_season_trends row. The season tab's red dot counts a trend
// claimable when now is past that start, reward != 1 and cur_count >= the row's para2
// (LWSeasonTrendsManager.lua:491-514); the row's button then sends lw.season.trend.reward
// {trend_id: Int} (LWSeasonTrendsItem.lua:229-230, 368-380; LWSeasonTrendRewardMessage.lua:4-7).
// An expired trend stays claimable (only NotOpen is refused, :369-372). A trend id the table lacks
// has no window or target in the client, so it is skipped. Progress is a server or alliance
// counter (lw_season_trends user_type), so a passive account qualifies.
//
// Never sent: lw.season.trend.donate (type 1 spends resources, type 2 diamonds,
// LWSeasonTrendsItem.lua:520-524). No error code is registered as benign: the first replies are
// logged and an error stops the claims.
const (
	seasonTrendsFeature  = "season-trends"
	seasonTrendInfoCmd   = "lw.season.trend.info"
	seasonTrendRewardCmd = "lw.season.trend.reward"
	seasonTrendClaimed   = 1
)

func init() {
	registerFeature(Feature{
		Name:    seasonTrendsFeature,
		Summary: "OPT-IN: in season, claim the season trend rewards whose server or alliance counter reached its target (static-only, not validated live)",
		Run:     runSeasonTrends,
	})
}

func runSeasonTrends(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	season := evSeasonID(in)
	if season <= 0 {
		return nil
	}
	start, ok := evNum(in.Object("playerServerSeasonInfo"), "seasonStartTime")
	if !ok || start <= 0 {
		slog.Info(seasonTrendsFeature+": init playerServerSeasonInfo has no seasonStartTime, so no trend window is known; skipping",
			"season", season)
		return nil
	}
	msg, err := session.SendAndWait(conn, seasonTrendsFeature+": trend info", seasonTrendInfoCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	info := msg.Params
	group, hasGroup := evNum(info, "season_group")
	trends := evObjects(info, "trends")
	if off, _ := evNum(info, "off_seaon"); off > 0 || !hasGroup || !seasonTrendGroupKnown(int32(group)) {
		slog.Info(seasonTrendsFeature+": the reply is no season trend list the table knows; claiming nothing",
			"season", season, "season_group", group, "hasGroup", hasGroup, "off_seaon", off, "trends", len(trends),
			"tableVersion", seasonTablesVersion)
		return nil
	}
	now := evNow().UnixMilli()
	var due []int32
	for _, t := range trends {
		if id, ok := evNum(t, "trend_id"); ok && seasonTrendDue(t, int32(id), start, now) {
			due = append(due, int32(id))
		}
	}
	slog.Info(seasonTrendsFeature+": trend info", "season", season, "season_group", group, "seasonStartTime", start,
		"trends", len(trends), "due", due)
	for _, id := range due {
		p := sfs.NewSFSObject()
		p.PutInt("trend_id", id)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s: trend %d", seasonTrendsFeature, id), seasonTrendRewardCmd, p, "rewards"); err != nil {
			return err
		}
	}
	return nil
}

// seasonTrendDue is the red dot's test for trend t (id) with the season start and now in ms
// (LWSeasonTrendsManager.lua:491-514; LWSeasonTrendsData.lua:21-38).
func seasonTrendDue(t *sfs.SFSObject, id int32, seasonStart, now int64) bool {
	row, ok := seasonTrendTable[id]
	if !ok {
		return false
	}
	opens := seasonStart + 86400000*int64(row.unlockTime-1)
	count, _ := evNum(t, "cur_count")
	claimed, _ := evNum(t, "reward")
	return opens != 0 && now > opens && claimed != seasonTrendClaimed && count >= int64(row.target)
}

// seasonTrendGroupKnown reports whether table lw_season_trends has rows of group: without them the
// client drops the reply (InitTrendData, LWSeasonTrendsManager.lua:109-112).
func seasonTrendGroupKnown(group int32) bool {
	for _, r := range seasonTrendTable {
		if r.group == group {
			return true
		}
	}
	return false
}
