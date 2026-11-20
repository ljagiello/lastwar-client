package game

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestSeasonAchievementsWire: S6 (config 1075, season_achievements group 8000) outside an alliance
// reads the personal row 8001; gradeValue 60000 reaches 50000, 25000 is claimed and 75000 is not
// reached.
//   - user.season.achievement.v2.info (MsgDefines.lua:1249; sent at SeasonRewardDataManager.lua:88)
//     {achievementId: Int} (UserSesaonAchievementV2InfoMesage.lua:4-7).
//   - user.season.achievement.v2.reward (MsgDefines.lua:1251; sent at LWSeasonPersonalRewardItem.lua:78)
//     {achievementId: Int, grade: Int} (UserSesaonAchievementV2RewardMessage.lua:4-8).
func TestSeasonAchievementsWire(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "user.season.achievement.v2.info":
			r := obj("achievementId", p.GetInt("achievementId"), "gradeValue", int64(60000), "gradesInfo", []*sfs.SFSObject{
				obj("grade", 25000), obj("grade", 50000), obj("grade", 75000),
			})
			r.PutIntArray("userRewards", []int32{25000})
			return r
		case "user.season.achievement.v2.reward":
			return obj("achievementId", p.GetInt("achievementId"), "grade", p.GetInt("grade"))
		}
		return evErr("unexpected command")
	})
	if err := runSeasonAchievements(conn, evWithSeason(evInit(30), 6)); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "user.season.achievement.v2.info", params: []aWireParam{{"achievementId", sfs.SFSInt, int32(8001)}}},
		{cmd: "user.season.achievement.v2.reward", params: []aWireParam{
			{"achievementId", sfs.SFSInt, int32(8001)}, {"grade", sfs.SFSInt, int32(50000)}}},
	})
}
