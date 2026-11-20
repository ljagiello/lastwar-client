package game

import (
	"slices"
	"strings"
	"testing"

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
	unknown := evWithSeason(evInit(30), 6)
	unknown.Object("playerServerSeasonInfo").PutInt("seasonConfigId", 999999)
	for name, in := range map[string]*Init{
		"out of season":           evInit(30),
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
