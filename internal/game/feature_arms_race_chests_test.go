package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func armsRaceTestInfo(score int64, scoreRecv []int32, medals int64, dayRecv []int32) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	info.PutLong("sc", score)
	info.PutLong("resourceItemNum", medals)
	info.PutLong("stage_end_time", evTestNow.Add(time.Hour).Unix())
	sr := sfs.NewSFSArray()
	for i, recv := range scoreRecv {
		r := sfs.NewSFSObject()
		r.PutInt("target", int32(1000*(i+1)))
		r.PutInt("receive", recv)
		sr.AddSFSObject(r)
	}
	info.PutSFSArray("score_rewards", sr)
	dr := sfs.NewSFSArray()
	for i, recv := range dayRecv {
		r := sfs.NewSFSObject()
		r.PutInt("resourceNum", int32([]int{2, 8, 18}[i]))
		r.PutInt("receive", recv)
		dr.AddSFSObject(r)
	}
	info.PutSFSArray("day_rewards", dr)
	return info
}

func TestArmsRaceClaimsScoreThenDailyChests(t *testing.T) {
	withEvNow(t, evTestNow)
	infos := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case armsRaceInfoCmd:
			infos++
			if infos == 1 { // before the score claim: 2 of 3 reached, 4 medals
				return armsRaceTestInfo(2500, []int32{1, 0, 0}, 4, []int32{1, 0, 0})
			}
			return armsRaceTestInfo(2500, []int32{1, 1, 0}, 10, []int32{1, 0, 0})
		}
		return evOK()
	})
	// 24 and 26 share group 100; 26 has the higher priority, so 24 is never queried.
	in := evInit(30, evActivity(24), evActivity(26))
	if err := runArmsRaceChests(conn, in); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := "activity.hero.get.info,activity.hero.score.reward,activity.hero.get.info,activity.hero.day.reward"
	if got != want {
		t.Fatalf("sent %s, want %s", got, want)
	}
	reqs := fake.requests()
	if reqs[0].Params.GetInt("aid") != 26 {
		t.Errorf("queried aid %d, want 26 (24 is superseded in its group)", reqs[0].Params.GetInt("aid"))
	}
	if p := reqs[1].Params; p.GetInt("aid") != 26 || p.GetInt("index") != -1 {
		t.Errorf("score claim = %v, want {aid:26, index:-1}", p)
	}
	if p := reqs[3].Params; p.GetInt("index") != 1 {
		t.Errorf("daily claim index = %d, want 1 (0-based, the 8-medal chest)", p.GetInt("index"))
	}
}

func TestArmsRaceNothingReachedOrStageOver(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name string
		info *sfs.SFSObject
	}{
		{"nothing reached", armsRaceTestInfo(500, []int32{0, 0, 0}, 1, []int32{0, 0, 0})},
		{"all claimed", armsRaceTestInfo(5000, []int32{1, 1, 1}, 20, []int32{1, 1, 1})},
		{"stage ended", func() *sfs.SFSObject {
			i := armsRaceTestInfo(5000, []int32{0, 0, 0}, 20, []int32{0, 0, 0})
			i.PutLong("stage_end_time", evTestNow.Add(-time.Second).Unix())
			return i
		}()},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return c.info })
		if err := runArmsRaceChests(conn, evInit(30, evActivity(17))); err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if got := strings.Join(fake.cmds(), ","); got != armsRaceInfoCmd {
			t.Errorf("%s: sent %s, want only the info request", c.name, got)
		}
	}
}

func TestArmsRaceAlreadyClaimedIsBenignAndFailuresAggregate(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case armsRaceInfoCmd:
			return armsRaceTestInfo(0, nil, 20, []int32{0, 0, 0})
		}
		if p.GetInt("index") == 0 {
			return evErr(evAlreadyExecuted)
		}
		if p.GetInt("index") == 1 {
			return evErr("E999")
		}
		return evOK()
	})
	err := runArmsRaceChests(conn, evInit(30, evActivity(17)))
	if err == nil || !strings.Contains(err.Error(), "E999") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E999 failure", err)
	}
	if n := len(fake.only(armsRaceDayRewardCmd)); n != 3 {
		t.Errorf("sent %d daily claims, want 3 (keep going past a failure)", n)
	}
}

func allianceDuelTestInfo(score string, target string, claimed string) *sfs.SFSObject {
	ev := sfs.NewSFSObject()
	ev.PutUtfString("actId", "duel-day-3")
	ev.PutInt("t", allianceDuelEventType)
	ev.PutUtfString("target", target)
	ev.PutLong("begintime", evTestNow.Add(-time.Hour).UnixMilli())
	ev.PutLong("endtime", evTestNow.Add(time.Hour).UnixMilli())
	us := sfs.NewSFSObject()
	us.PutUtfString("score", score)
	us.PutUtfString("newRewardFlagList", claimed)
	ev.PutSFSObject("userScore", us)
	old := sfs.NewSFSObject()
	old.PutUtfString("actId", "duel-day-2")
	old.PutInt("t", allianceDuelEventType)
	old.PutLong("begintime", evTestNow.Add(-25*time.Hour).UnixMilli())
	old.PutLong("endtime", evTestNow.Add(-time.Hour).UnixMilli())
	list := sfs.NewSFSArray()
	list.AddSFSObject(ev)
	list.AddSFSObject(old)
	info := sfs.NewSFSObject()
	info.PutUtfString("activityId", "55000")
	info.PutInt("type", allianceDuelEventType)
	info.PutSFSArray("eventList", list)
	return info
}

func TestAllianceDuelClaimsReachedUnclaimedUnlockedChests(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return allianceDuelTestInfo("450000", "100000|200000|300000|400000|500000", "1")
		}
		return evOK()
	})
	in := evInit(30, evActivity(55000))
	eff := sfs.NewSFSObject()
	eff.PutInt("92001", 1) // 6 chests unlocked
	in.Raw.PutSFSObject("effect", eff)
	if err := runArmsRaceChests(conn, in); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(allianceDuelRewardCmd)
	if len(claims) != 3 {
		t.Fatalf("sent %d duel claims, want 3 (chests 2-4: 1 claimed, 5 not reached)", len(claims))
	}
	for i, want := range []int32{200000, 300000, 400000} {
		p := claims[i].Params
		v, _ := p.Get("actId")
		if v.Type != sfs.SFSUtfString || v.Val != "duel-day-3" || p.GetInt("stage") != want || p.GetInt("eventType") != 15 {
			t.Errorf("claim %d = %v, want {actId:\"duel-day-3\", stage:%d, eventType:15}", i, p, want)
		}
	}
}

func TestAllianceDuelDefaultsToThreeChestsAndNeedsHQ10(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return allianceDuelTestInfo("900000", "100000|200000|300000|400000|500000", "")
		}
		return evOK()
	})
	if err := runArmsRaceChests(conn, evInit(30, evActivity(55000))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(allianceDuelRewardCmd)); n != 3 {
		t.Errorf("sent %d duel claims, want 3 (no unlock effect in init)", n)
	}
	conn2, fake2 := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runArmsRaceChests(conn2, evInit(9, evActivity(55000))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake2.requests()); n != 0 {
		t.Errorf("HQ 9 sent %d requests, want 0 (needMainCityLevel 10)", n)
	}
}
