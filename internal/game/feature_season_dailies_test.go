package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// evSeasonConfigIDs are lw_season rows (tables 40413) for each season number, the seasonConfigId
// evWithSeason puts in init: 3 Desert (type 1), 19 S2 (type 3, achievements 2000), 59 S3 Mummy
// (4000), 85 S4 Darkness (5000, golden tree 10001), 759 S5 NineNation (7000), 1075 S6
// NineNationRainforest (8000), 1477 S7 MummyV2 (9000).
var evSeasonConfigIDs = map[int32]int32{1: 3, 2: 19, 3: 59, 4: 85, 5: 759, 6: 1075, 7: 1477}

// evSeasonStart is the seasonStartTime evWithSeason puts in init: 20 days before evTestNow.
var evSeasonStart = evTestNow.Add(-20 * 24 * time.Hour)

// evWithSeason adds running season info for season id to in, with that season's config id
// (evSeasonConfigIDs) and evSeasonStart.
func evWithSeason(in *Init, id int32) *Init {
	end := evTestNow.Add(30 * 24 * time.Hour).UnixMilli()
	for _, k := range []string{"playerServerSeasonInfo", "curServerSeasonInfo"} {
		o := sfs.NewSFSObject()
		o.PutInt("seasonId", id)
		o.PutBool("open", true)
		o.PutLong("seasonStartTime", evSeasonStart.UnixMilli())
		o.PutLong("seasonEndTime", end)
		if cfg, ok := evSeasonConfigIDs[id]; ok {
			o.PutInt("seasonConfigId", cfg)
		}
		in.Raw.PutSFSObject(k, o)
	}
	return in
}

// seasonParamIs checks that p carries key with SFS type typ and value val.
func seasonParamIs(t *testing.T, p *sfs.SFSObject, key string, typ byte, val any) {
	t.Helper()
	v, ok := p.Get(key)
	if !ok || v.Type != typ || v.Val != val {
		t.Errorf("%s = %#v (type %d, present %v), want %#v of type %d in %v", key, v.Val, v.Type, ok, val, typ, p)
	}
}

func TestEvSeasonID(t *testing.T) {
	withEvNow(t, evTestNow)
	if got := evSeasonID(evWithSeason(evInit(1), 6)); got != 6 {
		t.Errorf("running season 6: got %d", got)
	}
	ended := evWithSeason(evInit(1), 3)
	ended.Object("playerServerSeasonInfo").PutLong("seasonEndTime", evTestNow.Add(-time.Hour).UnixMilli())
	if got := evSeasonID(ended); got != 0 {
		t.Errorf("ended season: got %d", got)
	}
	closed := evWithSeason(evInit(1), 3)
	closed.Object("curServerSeasonInfo").PutBool("open", false)
	if got := evSeasonID(closed); got != 0 {
		t.Errorf("login server not open: got %d", got)
	}
	if got := evSeasonID(evInit(1)); got != 0 {
		t.Errorf("no season info: got %d", got)
	}
}

func TestEvSeasonConfig(t *testing.T) {
	withEvNow(t, evTestNow)
	cfg, id, ok := evSeasonConfig(evWithSeason(evInit(1), 7))
	if !ok || id != 1477 || cfg != (seasonConfigRow{season: 7, typ: 4, type1: 1, achievements: 9000, trends: 70}) {
		t.Errorf("S7 config = %+v, %d, %v", cfg, id, ok)
	}
	unknown := evWithSeason(evInit(1), 6)
	unknown.Object("playerServerSeasonInfo").PutInt("seasonConfigId", 999999)
	if _, id, ok := evSeasonConfig(unknown); ok || id != 999999 {
		t.Errorf("a config id table lw_season lacks: id %d, ok %v", id, ok)
	}
	if _, _, ok := evSeasonConfig(evWithSeason(evInit(1), 9)); ok {
		t.Error("no seasonConfigId must not resolve")
	}
}

// seasonDailiesReplies answers the season-dailies commands: an unclaimed power milestone, a military
// daily not yet claimed at level 4, and one claimable pre-season task.
func seasonDailiesReplies(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
	switch cmd {
	case seasonMaxForceInfoCmd:
		return obj("maxForceValue", int64(5000), "list", []*sfs.SFSObject{
			obj("score", int64(1000), "receive", 1), obj("score", int64(4000)), obj("score", int64(9000)),
		})
	case seasonMilitaryInfoCmd:
		return obj("dailyRewardNum", 0, "militaryLevel", 4)
	case seasonPreTaskInfoCmd:
		return obj("seasonPreTaskArr", []*sfs.SFSObject{obj("taskId", "77", "state", 1)})
	}
	return evOK()
}

// TestSeasonDailiesClaims: in Season 6 (lw_season type 6, achievements group 8000) the client has
// no path to the v1 power milestones (MASTER B2), so only the military daily and the pre-season
// task are claimed.
func TestSeasonDailiesClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonDailiesReplies)
	in := evWithSeason(evInit(30, evActivity(1200101), evActivity(1200016)), 6)
	if err := runSeasonDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	want := "military.act.info,military.get.daily.reward,view.season.pre.task,season.pre.task.get.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	seasonParamIs(t, fake.only(seasonMilitaryDailyCmd)[0].Params, "viewLevel", sfs.SFSInt, int32(4))
	pre := fake.only(seasonPreTaskRewardCmd)[0].Params
	if pre.GetString("activityId") != "1200016" || pre.GetString("taskId") != "77" {
		t.Errorf("pre-task claim = %v, want UtfString activityId 1200016 and taskId 77", pre)
	}
}

// TestSeasonDailiesPowerMilestonesDesertOnly pins B2: the v1 pair only in a Desert (type 1)
// season, never in an achievements v2 season, and nothing when the season config is unknown.
func TestSeasonDailiesPowerMilestonesDesertOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	unknown := evWithSeason(evInit(30), 6)
	unknown.Object("playerServerSeasonInfo").PutInt("seasonConfigId", 999999)
	noConfig := evWithSeason(evInit(30), 8) // no evSeasonConfigIDs entry: no seasonConfigId
	for name, c := range map[string]struct {
		in   *Init
		want string
	}{
		"Desert":                  {evWithSeason(evInit(30), 1), "lw.season.user.max.force.reward.info,lw.season.user.max.force.reward.get.all"},
		"S2 type 3, group 2000":   {evWithSeason(evInit(30), 2), ""},
		"S6 type 6, group 8000":   {evWithSeason(evInit(30), 6), ""},
		"S7 type 4, group 9000":   {evWithSeason(evInit(30), 7), ""},
		"config not in lw_season": {unknown, ""},
		"no seasonConfigId":       {noConfig, ""},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, seasonDailiesReplies)
			if err := runSeasonDailies(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// TestSeasonDailiesMilitarySeason7 pins B7: Season 7's SeasonMilitary activity (1200224, para_1 49)
// gets the same daily claim as Season 6's; the manager has no season-number test.
func TestSeasonDailiesMilitarySeason7(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonDailiesReplies)
	if err := runSeasonDailies(conn, evWithSeason(evInit(30, evActivity(1200224)), 7)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "military.act.info,military.get.daily.reward" {
		t.Errorf("S7 sent %s", got)
	}
}

func TestSeasonDailiesOutOfSeasonOrNothingDue(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSeasonDailies(conn, evInit(30, evActivity(1200101))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("out of season sent %v", fake.cmds())
	}
	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		if cmd == seasonMilitaryInfoCmd {
			r.PutInt("dailyRewardNum", 1)
		}
		return r
	})
	if err := runSeasonDailies(conn2, evWithSeason(evInit(30, evActivity(1200101)), 6)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake2.cmds(), ","); got != "military.act.info" {
		t.Errorf("nothing due: sent %s", got)
	}
}

func TestSeasonDailiesBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case seasonMaxForceInfoCmd:
			return obj("maxForceValue", int64(5), "list", []*sfs.SFSObject{obj("score", int64(1))})
		case seasonMaxForceClaimCmd:
			return evErr(evAlreadyExecuted)
		case seasonMilitaryInfoCmd:
			return evErr("E3")
		}
		return evOK()
	})
	err := runSeasonDailies(conn, evWithSeason(evInit(30, evActivity(1200101)), 1))
	if err == nil || !strings.Contains(err.Error(), "E3") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E3 failure", err)
	}
}

// TestSeasonDailiesMilitaryEndShow: past the show start plus para_1 (49) days activity 1200101 is
// in end-show, where the client hides the daily (SeasonMilitaryManager.lua:51-57, 120-130).
func TestSeasonDailiesMilitaryEndShow(t *testing.T) {
	withEvNow(t, evTestNow)
	military := func(start, ready time.Time) *sfs.SFSObject {
		a := evActivity(1200101)
		a.PutLong("startTime", start.UnixMilli())
		if !ready.IsZero() {
			a.PutLong("readyTime", ready.UnixMilli())
		}
		return a
	}
	day := 24 * time.Hour
	for name, c := range map[string]struct {
		act  *sfs.SFSObject
		want string
	}{
		"day 50 of 49: end-show":        {military(evTestNow.Add(-50*day), time.Time{}), ""},
		"last hour of the normal phase": {military(evTestNow.Add(-49*day+time.Hour), time.Time{}), "military.act.info,military.get.daily.reward"},
		"readyTime is the show start":   {military(evTestNow.Add(-50*day), evTestNow.Add(-10*day)), "military.act.info,military.get.daily.reward"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				r := sfs.NewSFSObject()
				if cmd == seasonMilitaryInfoCmd {
					r.PutInt("dailyRewardNum", 0)
				}
				return r
			})
			if err := runSeasonDailies(conn, evWithSeason(evInit(30, c.act), 6)); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %s\nwant %s", got, c.want)
			}
		})
	}
	if seasonMilitaryNormal(Activity{ID: 1, Raw: military(evTestNow, time.Time{})}) {
		t.Error("an id without para_1 must not count as the normal phase")
	}
}

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
	// No gradeValue: nothing is reached, so nothing is claimed; an error stops that achievement.
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

// seasonMilitaryRankReplies answers military.act.info with level and score, and each rank-up with
// the next level and the same score.
func seasonMilitaryRankReplies(level int32, score int64) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case seasonMilitaryInfoCmd:
			return obj("militaryLevel", level, "totalMilitaryScore", score, "dailyRewardNum", 0)
		case seasonMilitaryUpCmd:
			return obj("militaryLevel", p.GetInt("viewLevel")+1, "totalMilitaryScore", score, "dailyRewardNum", 0)
		}
		return evOK()
	}
}

// TestSeasonMilitaryRankUp: S7 group 70 thresholds 60000 (level 4) and 112000 (level 5): a score
// of 100000 at level 2 ranks up twice (2 -> 3 -> 4), each time sending the current level.
func TestSeasonMilitaryRankUp(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonMilitaryRankReplies(2, 100000))
	if err := runSeasonMilitaryRankUp(conn, evWithSeason(evInit(30, evActivity(1200224)), 7)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "military.act.info,military.up.level,military.up.level" {
		t.Fatalf("sent %s", got)
	}
	ups := fake.only(seasonMilitaryUpCmd)
	seasonParamIs(t, ups[0].Params, "viewLevel", sfs.SFSInt, int32(2))
	seasonParamIs(t, ups[1].Params, "viewLevel", sfs.SFSInt, int32(3))
}

func TestSeasonMilitaryRankUpStops(t *testing.T) {
	withEvNow(t, evTestNow)
	settling := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000), "isDailySettleIng", 1)
	}
	scoreDrops := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return obj("militaryLevel", 3, "totalMilitaryScore", int64(10))
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	sameLevel := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	refused := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonMilitaryUpCmd {
			return evErr("E9")
		}
		return obj("militaryLevel", 2, "totalMilitaryScore", int64(900000))
	}
	s6 := evWithSeason(evInit(30, evActivity(1200101)), 6)
	ended := evActivity(1200101)
	ended.PutLong("startTime", evTestNow.Add(-50*24*time.Hour).UnixMilli())
	for name, c := range map[string]struct {
		reply   func(string, *sfs.SFSObject) *sfs.SFSObject
		in      *Init
		want    string
		wantErr bool
	}{
		"below the next threshold":     {seasonMilitaryRankReplies(3, 28999), s6, "military.act.info", false},
		"at the highest manual level":  {seasonMilitaryRankReplies(15, 9000000), s6, "military.act.info", false},
		"daily settlement in progress": {settling, s6, "military.act.info", false},
		"score drops after a rank-up":  {scoreDrops, s6, "military.act.info,military.up.level", false},
		"level does not rise":          {sameLevel, s6, "military.act.info,military.up.level", false},
		"refused":                      {refused, s6, "military.act.info,military.up.level", true},
		"end-show phase":               {seasonMilitaryRankReplies(2, 900000), evWithSeason(evInit(30, ended), 6), "", false},
		"out of season":                {seasonMilitaryRankReplies(2, 900000), evInit(30, evActivity(1200101)), "", false},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			err := runSeasonMilitaryRankUp(conn, c.in)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

func TestSeasonMilitaryCanRankUp(t *testing.T) {
	levels := []seasonMilitaryLevelRow{
		{level: 1}, {level: 2, unlockScore: 10}, {level: 3, unlockScore: 20, conditionType: 7, conditionPara: 5},
		{level: 4, unlockScore: 30, standardLevel: 1}, {level: 5, needRank: 1},
	}
	for _, c := range []struct {
		level, score int64
		progress     map[int64]int64
		want         bool
	}{
		{1, 10, nil, true},
		{1, 9, nil, false},
		{2, 20, map[int64]int64{7: 4}, false}, // the unlock condition is short
		{2, 20, map[int64]int64{7: 5}, true},
		{3, 99, nil, true},
		{4, 99, nil, false}, // standard_level: the last manual rank
		{5, 99, nil, false}, // no next row
	} {
		if got := seasonMilitaryCanRankUp(levels, c.level, c.score, c.progress); got != c.want {
			t.Errorf("level %d score %d progress %v: got %v", c.level, c.score, c.progress, got)
		}
	}
	if seasonMilitaryCanRankUp([]seasonMilitaryLevelRow{{level: 1}, {level: 2}}, 1, 99, nil) {
		t.Error("a group without a standard_level row has no manual rank-up")
	}
}

// TestSeasonFeaturesRegistered: the two new features are opt-in and static-only, and the rank-up
// runs before season-dailies claims the daily salary.
func TestSeasonFeaturesRegistered(t *testing.T) {
	for _, name := range []string{seasonAchievementsFeature, seasonMilitaryRankFeature} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s: registered %v, %+v", name, ok, f)
		}
	}
	var order []string
	for _, f := range featuresInRunOrder() {
		order = append(order, f.Name)
	}
	if rank, daily := slices.Index(order, seasonMilitaryRankFeature), slices.Index(order, "season-dailies"); rank < 0 || rank > daily {
		t.Errorf("run order has %s at %d and season-dailies at %d", seasonMilitaryRankFeature, rank, daily)
	}
}
