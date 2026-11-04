package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func cardInit(monthEnd, monthLast time.Time, spy int32) *Init {
	in := evInit(30, evActivity(1210000))
	mc := sfs.NewSFSObject()
	mc.PutUtfString("itemId", "800001")
	mc.PutLong("endTime", monthEnd.UnixMilli())
	mc.PutLong("time", monthLast.UnixMilli())
	in.Raw.PutSFSObject("golloesMonthCard", mc)
	g := sfs.NewSFSObject()
	g.PutInt("spyRewardNum", spy)
	g.PutInt("caravanRewardNum", 0)
	in.Raw.PutSFSObject("golloesData", g)
	priv := sfs.NewSFSObject()
	priv.PutSFSArray("freeReward", sfs.NewSFSArray())
	priv.PutSFSArray("vipReward", freebieReward())
	in.Raw.PutSFSObject("truckMonthCardPrivilege", priv)
	in.Buildings = append(in.Buildings, NewTestBuilding(2, golloesCampBuildingID, 3))
	return in
}

func cardReplies(weekLast time.Time, heroStates []int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case weekCardInfoCmd:
			r := sfs.NewSFSObject()
			cards := sfs.NewSFSArray()
			queue := sfs.NewSFSObject() // a build-queue card never has a claim
			queue.PutInt("type_function", 1)
			queue.PutLong("endTime", evTestNow.Add(time.Hour).UnixMilli())
			cards.AddSFSObject(queue)
			c := sfs.NewSFSObject()
			c.PutInt("type_function", weekCardCollectable)
			c.PutLong("endTime", evTestNow.Add(time.Hour).UnixMilli())
			c.PutLong("lastReceiveTime", weekLast.UnixMilli())
			cards.AddSFSObject(c)
			r.PutSFSArray("weekCards", cards)
			return r
		case heroMonthCardInfoCmd:
			r := sfs.NewSFSObject()
			cards := sfs.NewSFSArray()
			c := sfs.NewSFSObject()
			c.PutInt("activityId", 110001)
			c.PutInt("buy", 1)
			c.PutLong("startTime", evTestNow.Add(-36*time.Hour).UnixMilli()) // day 2 of the card
			c.PutLong("endTime", evTestNow.Add(20*24*time.Hour).UnixMilli())
			days := sfs.NewSFSArray()
			for i, st := range heroStates {
				d := sfs.NewSFSObject()
				d.PutInt("day", int32(i+1))
				d.PutInt("state", st)
				days.AddSFSObject(d)
			}
			c.PutSFSArray("rewardArr", days)
			cards.AddSFSObject(c)
			r.PutSFSArray("cards", cards)
			return r
		}
		return evOK()
	}
}

func TestCardDailiesClaimsOwnedCards(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	conn, fake := startEvFake(t, cardReplies(yesterday, []int32{1, 0, 0}))
	in := cardInit(evTestNow.Add(24*time.Hour), yesterday, 2)
	if err := runCardDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	want := "month.card.reward,get.week.card.info,receive.all.week.card.reward,get.hero.month.card.info," +
		"receive.hero.month.card.reward,receive.golloes.reward,truck.monthcard.privilege.click.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if v, _ := fake.only(monthCardRewardCmd)[0].Params.Get("itemId"); v.Type != sfs.SFSUtfString || v.Val != "800001" {
		t.Errorf("itemId = %#v, want UtfString 800001", v)
	}
	hero := fake.only(heroMonthCardRewardCmd)[0].Params
	if v, _ := hero.Get("day"); v.Type != sfs.SFSLong || hero.GetLong("day") != 2 || hero.GetInt("activityId") != 110001 {
		t.Errorf("hero card claim = %v, want {activityId:110001, day:Long 2} (day 3 not reached)", hero)
	}
	if fake.only(golloesRewardCmd)[0].Params.GetInt("type") != 1 {
		t.Error("golloes claim must be type 1 (explorer) for spyRewardNum")
	}
	if fake.only(weekCardInfoCmd)[0].Params.GetInt("isLogin") != 1 {
		t.Error("week card info must send isLogin 1")
	}
}

func TestCardDailiesSkipsClaimedExpiredAndUnowned(t *testing.T) {
	withEvNow(t, evTestNow)
	today := evTestTomorrow.Add(-time.Hour)
	conn, fake := startEvFake(t, cardReplies(today, []int32{1, 1}))
	in := cardInit(evTestNow.Add(-time.Hour), today, 0) // month card expired: no claim, no VIP truck reward
	if err := runCardDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.week.card.info,get.hero.month.card.info" {
		t.Errorf("sent %s, want only the two info reads", got)
	}
}

// TestCardDailiesMonthCardWithoutItemID: init golloesMonthCard without itemId still claims, with
// the recharge MonthCard row's para1 the client seeds it from (MonthCardNewManager.lua:33-44).
func TestCardDailiesMonthCardWithoutItemID(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	conn, fake := startEvFake(t, cardReplies(yesterday, nil))
	in := cardInit(evTestNow.Add(24*time.Hour), yesterday, 0)
	mc := sfs.NewSFSObject() // no itemId
	mc.PutLong("endTime", evTestNow.Add(24*time.Hour).UnixMilli())
	mc.PutLong("time", yesterday.UnixMilli())
	in.Raw.PutSFSObject("golloesMonthCard", mc)
	if err := runCardDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(monthCardRewardCmd)
	if len(claims) != 1 {
		t.Fatalf("sent %v, want one month.card.reward", fake.cmds())
	}
	if v, _ := claims[0].Params.Get("itemId"); v.Type != sfs.SFSUtfString || v.Val != "800001" {
		t.Errorf("itemId = %#v, want UtfString 800001 (recharge 1140000 para1)", v)
	}
}

// TestWeekCardInfoOncePerLogin: card-dailies and free-chests share one get.week.card.info per
// (conn, init), as the client sends it once at login (WeekCardManager.lua:23-25); a new init (a
// new login) asks again.
func TestWeekCardInfoOncePerLogin(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	base := cardReplies(yesterday, nil)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := base(cmd, p)
		if cmd == weekCardInfoCmd {
			free := sfs.NewSFSObject()
			free.PutLong("lastRewardTime", time.Now().Add(-72*time.Hour).UnixMilli())
			r.PutSFSObject("dailyFree", free)
		}
		return r
	})
	in := cardInit(evTestNow.Add(24*time.Hour), yesterday, 0)
	if err := runCardDailies(conn, in); err != nil {
		t.Fatal(err)
	}
	if err := runFreeChests(conn, in); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(weekCardInfoCmd)); n != 1 {
		t.Errorf("get.week.card.info sent %d times in one login, want 1", n)
	}
	if n := len(fake.only(freeChestsWeekCardCmd)); n != 1 {
		t.Errorf("week-card free gift sent %d times, want 1 from the shared reply", n)
	}
	if err := runFreeChests(conn, cardInit(evTestNow.Add(24*time.Hour), yesterday, 0)); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(weekCardInfoCmd)); n != 2 {
		t.Errorf("get.week.card.info sent %d times after a new init, want 2", n)
	}
}

// TestWeekCardInfoSharesFailure: a failed read is not resent in the same login, and card-dailies
// still reports it.
func TestWeekCardInfoSharesFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == weekCardInfoCmd {
			return evErr("E7")
		}
		return evOK()
	})
	in := cardInit(evTestNow.Add(24*time.Hour), evTestNow, 0)
	if err := runFreeChests(conn, in); err == nil || !strings.Contains(err.Error(), "E7") {
		t.Errorf("free-chests err = %v, want E7", err)
	}
	if err := runCardDailies(conn, in); err == nil || !strings.Contains(err.Error(), "E7") {
		t.Errorf("card-dailies err = %v, want the shared E7", err)
	}
	if n := len(fake.only(weekCardInfoCmd)); n != 1 {
		t.Errorf("get.week.card.info sent %d times, want 1", n)
	}
}

func TestCardDailiesBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	base := cardReplies(yesterday, nil)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case monthCardRewardCmd:
			return evErr(evAlreadyExecuted)
		case weekCardAllRewardCmd:
			return evErr("E9")
		}
		return base(cmd, p)
	})
	err := runCardDailies(conn, cardInit(evTestNow.Add(time.Hour), yesterday, 0))
	if err == nil || !strings.Contains(err.Error(), "E9") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E9 failure", err)
	}
}
