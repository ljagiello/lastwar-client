package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// goldTreeView is a gold.tree.act.view reply: the tree charged at finish, the player's tree week
// [start, end] and the days already prayed.
func goldTreeView(finish, start, end time.Time, prayed ...int32) *sfs.SFSObject {
	var cards []*sfs.SFSObject
	for _, d := range prayed {
		cards = append(cards, obj("day", d, "cardId", 3))
	}
	week := obj("uuid", int64(1), "weekTime", int64(1746979200000), "goldTreeConfigId", 10001)
	if !start.IsZero() {
		week.PutLong("startTime", start.UnixMilli())
		week.PutLong("endTime", end.UnixMilli())
	}
	tree := obj("treeId", 1)
	if !finish.IsZero() {
		tree.PutLong("finishTime", finish.UnixMilli())
	}
	return obj("goldTreeInfo", tree, "userGoldTreeDataInfo",
		obj("weekTime", int64(1746979200000), "userGoldTeeInfo", week, "userGoldTreeCardArr", cards))
}

// goldTreeInit is an S4 (Darkness, config 85, golden tree 10001 "2|6") init with the Gold Tree
// activity 1200021 open.
func goldTreeInit() *Init { return evWithSeason(evInit(30, evActivity(1200021)), 4) }

func TestGoldTreeWeekdayIsMondayOne(t *testing.T) {
	dayStart := evTestTomorrow.Add(-24 * time.Hour) // 02:00 UTC: server time is UTC-2
	for _, c := range []struct {
		now  time.Time
		want int32
	}{
		{time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), 1},  // Monday
		{time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC), 3},  // Wednesday
		{time.Date(2026, 10, 7, 1, 0, 0, 0, time.UTC), 2},   // Tuesday 23:00 server time
		{time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), 7}, // Sunday
		{time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC), 4}, // Thursday
	} {
		if got := goldTreeWeekday(c.now, dayStart); got != c.want {
			t.Errorf("%v: weekday %d, want %d", c.now, got, c.want)
		}
	}
}

func TestGoldTreeDays(t *testing.T) {
	for _, c := range []struct {
		today  int32
		prayed []int64
		want   []int32
	}{
		{3, nil, []int32{2, 3}},
		{3, []int64{2}, []int32{3}},
		{6, []int64{2, 4}, []int32{3, 5, 6}},
		{7, nil, []int32{2, 3, 4, 5, 6}},
		{1, []int64{2, 3}, []int32{4, 5, 6}}, // Monday counts as day 8: the whole cycle
	} {
		if got := goldTreeDays(2, 6, c.today, c.prayed); !slices.Equal(got, c.want) {
			t.Errorf("today %d prayed %v: %v, want %v", c.today, c.prayed, got, c.want)
		}
	}
}

func TestGoldTreePrayer(t *testing.T) {
	wednesday := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	withEvNow(t, wednesday)
	charged := wednesday.Add(-time.Hour)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == goldTreeViewCmd {
			return goldTreeView(charged, wednesday.Add(-30*time.Hour), wednesday.Add(100*time.Hour))
		}
		return obj("day", p.GetInt("day"), "cardReward", []*sfs.SFSObject{})
	})
	act := evActivity(1200021)
	act.PutLong("endTime", wednesday.Add(72*time.Hour).UnixMilli())
	in := evWithSeason(evInit(30, act), 4)
	in.Raw.PutLong("tomorrow", time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC).Unix())
	if err := runGoldTreePrayer(conn, in); err != nil {
		t.Fatal(err)
	}
	var days []int32
	for _, r := range fake.only(goldTreeCardCmd) {
		seasonParamIs(t, r.Params, "weekTime", sfs.SFSLong, int64(1746979200000))
		seasonParamIs(t, r.Params, "day", sfs.SFSInt, r.Params.GetInt("day"))
		days = append(days, r.Params.GetInt("day"))
	}
	if !slices.Equal(days, []int32{2, 3}) {
		t.Errorf("prayed days %v, want [2 3] (Tuesday and Wednesday)", days)
	}
}

func TestGoldTreePrayerSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	week := func(f func(start, end time.Time) *sfs.SFSObject) func(string, *sfs.SFSObject) *sfs.SFSObject {
		return func(string, *sfs.SFSObject) *sfs.SFSObject {
			return f(evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
		}
	}
	for name, c := range map[string]struct {
		in    *Init
		reply func(string, *sfs.SFSObject) *sfs.SFSObject
		want  string
	}{
		"out of season": {evInit(30, evActivity(1200021)), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(s, s, e)
		}), ""},
		"no Gold Tree activity": {evWithSeason(evInit(30), 4), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(s, s, e)
		}), ""},
		"tree still charging": {goldTreeInit(), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(e, s, e)
		}), goldTreeViewCmd},
		"tree never charged": {goldTreeInit(), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(time.Time{}, s, e)
		}), goldTreeViewCmd},
		"tree week over": {goldTreeInit(), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(s, s.Add(-48*time.Hour), s.Add(-24*time.Hour))
		}), goldTreeViewCmd},
		"no tree week": {goldTreeInit(), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(s, time.Time{}, time.Time{})
		}), goldTreeViewCmd},
		"all prayed": {goldTreeInit(), week(func(s, e time.Time) *sfs.SFSObject {
			return goldTreeView(s, s, e, 2, 3, 4, 5, 6)
		}), goldTreeViewCmd},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			if err := runGoldTreePrayer(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
	f, ok := featureRegistry[goldTreeFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, default off, static-only", f)
	}
}

func TestGoldTreeCycle(t *testing.T) {
	withEvNow(t, evTestNow)
	if first, last := goldTreeCycle(goldTreeInit()); first != 2 || last != 6 {
		t.Errorf("S4 cycle %d|%d, want 2|6 (season_golden_tree 10001)", first, last)
	}
	if first, last := goldTreeCycle(evWithSeason(evInit(30), 6)); first != 2 || last != 6 {
		t.Errorf("a config without a golden tree: %d|%d, want the defaults 2|6", first, last)
	}
}
