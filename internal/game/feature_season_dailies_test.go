package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// withSeason adds running season info for season id to in.
func evWithSeason(in *Init, id int32) *Init {
	end := evTestNow.Add(30 * 24 * time.Hour).UnixMilli()
	for _, k := range []string{"playerServerSeasonInfo", "curServerSeasonInfo"} {
		o := sfs.NewSFSObject()
		o.PutInt("seasonId", id)
		o.PutBool("open", true)
		o.PutLong("seasonEndTime", end)
		in.Raw.PutSFSObject(k, o)
	}
	return in
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

func TestSeasonDailiesClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case seasonMaxForceInfoCmd:
			r.PutLong("maxForceValue", 5000)
			l := sfs.NewSFSArray()
			for _, m := range []struct {
				score int64
				recv  bool
			}{{1000, true}, {4000, false}, {9000, false}} {
				e := sfs.NewSFSObject()
				e.PutLong("score", m.score)
				e.PutBool("receive", m.recv)
				l.AddSFSObject(e)
			}
			r.PutSFSArray("list", l)
		case seasonMilitaryInfoCmd:
			r.PutInt("dailyRewardNum", 0)
			r.PutInt("militaryLevel", 4)
		case seasonPreTaskInfoCmd:
			l := sfs.NewSFSArray()
			for id, st := range map[string]int32{"77": 1} {
				e := sfs.NewSFSObject()
				e.PutUtfString("taskId", id)
				e.PutInt("state", st)
				l.AddSFSObject(e)
			}
			r.PutSFSArray("seasonPreTaskArr", l)
		default:
			return evOK()
		}
		return r
	})
	in := evWithSeason(evInit(30, evActivity(1200101), evActivity(1200016)), 6)
	if err := runSeasonDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	want := "lw.season.user.max.force.reward.info,lw.season.user.max.force.reward.get.all,military.act.info," +
		"military.get.daily.reward,view.season.pre.task,season.pre.task.get.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if fake.only(seasonMilitaryDailyCmd)[0].Params.GetInt("viewLevel") != 4 {
		t.Error("viewLevel must be the current military level")
	}
	pre := fake.only(seasonPreTaskRewardCmd)[0].Params
	if pre.GetString("activityId") != "1200016" || pre.GetString("taskId") != "77" {
		t.Errorf("pre-task claim = %v, want UtfString activityId 1200016 and taskId 77", pre)
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
	if got := strings.Join(fake2.cmds(), ","); got != "lw.season.user.max.force.reward.info,military.act.info" {
		t.Errorf("nothing due: sent %s", got)
	}
}

func TestSeasonDailiesBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case seasonMaxForceInfoCmd:
			r := sfs.NewSFSObject()
			r.PutLong("maxForceValue", 5)
			l := sfs.NewSFSArray()
			e := sfs.NewSFSObject()
			e.PutLong("score", 1)
			l.AddSFSObject(e)
			r.PutSFSArray("list", l)
			return r
		case seasonMaxForceClaimCmd:
			return evErr(evAlreadyExecuted)
		case seasonMilitaryInfoCmd:
			return evErr("E3")
		}
		return evOK()
	})
	err := runSeasonDailies(conn, evWithSeason(evInit(30, evActivity(1200101)), 6))
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
		"day 50 of 49: end-show": {military(evTestNow.Add(-50*day), time.Time{}),
			"lw.season.user.max.force.reward.info"},
		"last hour of the normal phase": {military(evTestNow.Add(-49*day+time.Hour), time.Time{}),
			"lw.season.user.max.force.reward.info,military.act.info,military.get.daily.reward"},
		"readyTime is the show start": {military(evTestNow.Add(-50*day), evTestNow.Add(-10*day)),
			"lw.season.user.max.force.reward.info,military.act.info,military.get.daily.reward"},
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
