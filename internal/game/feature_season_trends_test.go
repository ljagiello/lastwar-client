package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// seasonTrendInfo is a lw.season.trend.info reply for group with trends {trend_id, cur_count,
// reward}.
func seasonTrendInfo(group int32, trends ...[3]int32) *sfs.SFSObject {
	var list []*sfs.SFSObject
	for _, t := range trends {
		list = append(list, obj("trend_id", t[0], "cur_count", int64(t[1]), "reward", t[2], "day_donate", 0))
	}
	return obj("season_group", group, "trends", list)
}

// seasonTrendS6 answers the S6 (group 60) trend list: 480 and 484 are due; 482 is short of its 20,
// 483 is claimed, 497 opens on season day 22 (evSeasonStart is 20 days before evTestNow) and 999999
// is not in the table.
func seasonTrendS6(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	if cmd == seasonTrendInfoCmd {
		return seasonTrendInfo(60, [3]int32{480, 1000, 0}, [3]int32{482, 19, 0}, [3]int32{483, 60000, 1},
			[3]int32{497, 5000, 0}, [3]int32{999999, 1 << 30, 0}, [3]int32{484, 10, 0})
	}
	return obj("trend_id", p.GetInt("trend_id"), "rewards", []*sfs.SFSObject{})
}

func TestSeasonTrendsClaimsReachedOpenTrends(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonTrendS6)
	if err := runSeasonTrends(conn, evWithSeason(evInit(30), 6)); err != nil {
		t.Fatal(err)
	}
	var claimed []int32
	for _, r := range fake.only(seasonTrendRewardCmd) {
		seasonParamIs(t, r.Params, "trend_id", sfs.SFSInt, r.Params.GetInt("trend_id"))
		claimed = append(claimed, r.Params.GetInt("trend_id"))
	}
	if !slices.Equal(claimed, []int32{480, 484}) {
		t.Errorf("claimed %v, want [480 484]", claimed)
	}
	if info := fake.requests()[0]; info.Cmd != seasonTrendInfoCmd || info.Params.Has("trend_id") {
		t.Errorf("first request %s %v, want the trend info, which takes no params", info.Cmd, info.Params)
	}
}

func TestSeasonTrendsSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	noStart := evWithSeason(evInit(30), 6)
	noStart.Object("playerServerSeasonInfo").PutLong("seasonStartTime", 0)
	offSeason := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := seasonTrendS6(cmd, p)
		r.PutInt("off_seaon", 1)
		return r
	}
	unknownGroup := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return seasonTrendInfo(61, [3]int32{480, 1000, 0})
	}
	noGroup := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		return obj("trends", []*sfs.SFSObject{obj("trend_id", 480, "cur_count", int64(1000))})
	}
	for name, c := range map[string]struct {
		in    *Init
		reply func(string, *sfs.SFSObject) *sfs.SFSObject
		want  string
	}{
		"out of season":       {evInit(30), seasonTrendS6, ""},
		"no seasonStartTime":  {noStart, seasonTrendS6, ""},
		"off-season list":     {evWithSeason(evInit(30), 6), offSeason, seasonTrendInfoCmd},
		"group not in table":  {evWithSeason(evInit(30), 6), unknownGroup, seasonTrendInfoCmd},
		"reply without group": {evWithSeason(evInit(30), 6), noGroup, seasonTrendInfoCmd},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			if err := runSeasonTrends(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

func TestSeasonTrendsErrorStops(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonTrendRewardCmd {
			return evErr("E7")
		}
		return seasonTrendS6(cmd, p)
	})
	if err := runSeasonTrends(conn, evWithSeason(evInit(30), 6)); err == nil || !strings.Contains(err.Error(), "E7") {
		t.Errorf("err = %v, want E7", err)
	}
	if n := len(fake.only(seasonTrendRewardCmd)); n != 1 {
		t.Errorf("sent %d claims, want 1: an error stops the claims", n)
	}
	f, ok := featureRegistry[seasonTrendsFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, default off, static-only", f)
	}
}
