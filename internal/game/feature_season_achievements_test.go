package game

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// seasonAchievementReplies answers user.season.achievement.v2.info for each achievement: gradeValue
// 60000 with grades 25000, 50000 and 75000, of which 25000 is claimed.
func seasonAchievementReplies(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	switch cmd {
	case seasonAchieveInfoCmd:
		r := obj("achievementId", p.GetInt("achievementId"), "gradeValue", int64(60000), "gradesInfo", []*sfs.SFSObject{
			obj("grade", 25000), obj("grade", 50000), obj("grade", 75000),
		})
		r.PutIntArray("userRewards", []int32{25000})
		return r
	case seasonAchieveRewardCmd:
		return obj("achievementId", p.GetInt("achievementId"), "grade", p.GetInt("grade"))
	}
	return evOK()
}

// TestSeasonAchievementsClaimsReachedGrades: in Season 6 (group 8000) the personal row 8001 and,
// in an alliance, the alliance row 8002 are read and their reached, unclaimed grades claimed; the
// camp row 8003 (not_show 1) is never touched.
func TestSeasonAchievementsClaimsReachedGrades(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, c := range map[string]struct {
		alliance string
		want     []int32
	}{
		"in an alliance": {"A1", []int32{8001, 8002}},
		"no alliance":    {"", []int32{8001}},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, seasonAchievementReplies)
			in := evWithSeason(evInit(30), 6)
			in.Raw.PutSFSObject("user", obj("allianceId", c.alliance))
			if err := runSeasonAchievements(conn, in); err != nil {
				t.Fatal(err)
			}
			var read, claimed []int32
			for _, r := range fake.requests() {
				seasonParamIs(t, r.Params, "achievementId", sfs.SFSInt, r.Params.GetInt("achievementId"))
				switch r.Cmd {
				case seasonAchieveInfoCmd:
					read = append(read, r.Params.GetInt("achievementId"))
				case seasonAchieveRewardCmd:
					seasonParamIs(t, r.Params, "grade", sfs.SFSInt, int32(50000))
					claimed = append(claimed, r.Params.GetInt("achievementId"))
				default:
					t.Errorf("unexpected %s", r.Cmd)
				}
			}
			if !slices.Equal(read, c.want) || !slices.Equal(claimed, c.want) {
				t.Errorf("read %v, claimed grade 50000 of %v; want %v", read, claimed, c.want)
			}
		})
	}
}

func TestSeasonAchievementsGates(t *testing.T) {
	withEvNow(t, evTestNow)
	desert := evWithSeason(evInit(30), 1)
	lowHQ := evWithSeason(evInit(2), 6)
	// S6 with its config (1075, group 8000) still in init, but past seasonEndTime.
	ended := evWithSeason(evInit(30), 6)
	for _, k := range []string{"playerServerSeasonInfo", "curServerSeasonInfo"} {
		ended.Object(k).PutLong("seasonEndTime", evTestNow.Add(-time.Hour).UnixMilli())
	}
	unknown := evWithSeason(evInit(30), 6)
	unknown.Object("playerServerSeasonInfo").PutInt("seasonConfigId", 999999)
	for name, in := range map[string]*Init{
		"out of season":           evInit(30),
		"season ended":            ended,
		"Desert has no v2 group":  desert,
		"HQ below level 3":        lowHQ,
		"config not in lw_season": unknown,
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, seasonAchievementReplies)
			if err := runSeasonAchievements(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v", got)
			}
		})
	}
	// No gradeValue: nothing is reached, so nothing is claimed (a refused grade claim is pinned by
	// TestSeasonClaimsStopOnTheFirstRefusal).
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonAchieveInfoCmd {
			return obj("gradesInfo", []*sfs.SFSObject{obj("grade", 1)})
		}
		return evErr("E1")
	})
	if err := runSeasonAchievements(conn, evWithSeason(evInit(30), 7)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "user.season.achievement.v2.info" {
		t.Errorf("S7 without gradeValue or alliance sent %s", got)
	}
}

// TestSeasonAchievementsSkipsHiddenRows: in S5 (config 759, group 7000) in an alliance the panel
// lists 7001 (personal) and 7002 (alliance); 7003 is an alliance row with not_show 1, which
// LWSeasonPersonalRewardNew.lua:126-136 hides, so it is never read or claimed.
func TestSeasonAchievementsSkipsHiddenRows(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonAchievementReplies)
	in := evWithSeason(evInit(30), 5)
	in.Raw.PutSFSObject("user", obj("allianceId", "A1"))
	if err := runSeasonAchievements(conn, in); err != nil {
		t.Fatal(err)
	}
	var read []int32
	for _, r := range fake.only("user.season.achievement.v2.info") {
		read = append(read, r.Params.GetInt("achievementId"))
	}
	if !slices.Equal(read, []int32{7001, 7002}) {
		t.Errorf("read %v, want [7001 7002]", read)
	}
}

// TestSeasonAchievementsBuilderGroupsAreNeverClaimed: a season config whose season_achievements is
// a builder-alliance group (3000 and 6000: season_achievements rows 3001-3002 and 6001-6002, the
// farmer panel's groups from season_builders_alliance_group.season_achievements, 3000 in its row
// 10001; claimed there behind two confirmations, UILWSingleActivityContainerView.lua:97-102,
// LWSeasonPersonalRewardItem.lua:88-108) sends nothing. The 40413 lw_season names neither, so the
// test adds a config row for the run.
func TestSeasonAchievementsBuilderGroupsAreNeverClaimed(t *testing.T) {
	withEvNow(t, evTestNow)
	const cfgID = 990001
	for _, c := range []struct {
		group int32
		want  string
	}{
		{8000, "user.season.achievement.v2.info,user.season.achievement.v2.reward"}, // the S6 group: 8001
		{3000, ""},
		{6000, ""},
	} {
		t.Run(strconv.Itoa(int(c.group)), func(t *testing.T) {
			seasonConfigTable[cfgID] = seasonConfigRow{season: 6, typ: 6, achievements: c.group}
			t.Cleanup(func() { delete(seasonConfigTable, cfgID) })
			in := evWithSeason(evInit(30), 6)
			for _, k := range []string{"playerServerSeasonInfo", "curServerSeasonInfo"} {
				in.Object(k).PutInt("seasonConfigId", cfgID)
			}
			conn, fake := startEvFake(t, seasonAchievementReplies)
			if err := runSeasonAchievements(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// TestSeasonAchievementsGradeEntries: a grade is claimable when grade <= gradeValue
// (SeasonRewardDataManager.lua:1220-1226). A gradesInfo entry without grade, or a reply without
// gradeValue, reaches nothing. S7 (group 9000) outside an alliance reads 9001 only.
func TestSeasonAchievementsGradeEntries(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name string
		info *sfs.SFSObject
		want []int32
	}{
		{"an entry without grade", obj("achievementId", 9001, "gradeValue", int64(60000),
			"gradesInfo", []*sfs.SFSObject{obj("reward", "x"), obj("grade", 50000)}), []int32{50000}},
		{"no gradeValue", obj("achievementId", 9001, "gradesInfo", []*sfs.SFSObject{obj("grade", 0)}), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == "user.season.achievement.v2.info" {
					return c.info
				}
				return obj("achievementId", p.GetInt("achievementId"), "grade", p.GetInt("grade"))
			})
			if err := runSeasonAchievements(conn, evWithSeason(evInit(30), 7)); err != nil {
				t.Fatal(err)
			}
			var grades []int32
			for _, r := range fake.only("user.season.achievement.v2.reward") {
				grades = append(grades, r.Params.GetInt("grade"))
			}
			if !slices.Equal(grades, c.want) {
				t.Errorf("claimed grades %v, want %v", grades, c.want)
			}
		})
	}
}
