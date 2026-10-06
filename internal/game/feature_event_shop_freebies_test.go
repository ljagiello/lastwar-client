package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// freebieReward is a non-empty reward list.
func freebieReward() *sfs.SFSArray {
	a := sfs.NewSFSArray()
	r := sfs.NewSFSObject()
	r.PutInt("type", 7)
	a.AddSFSObject(r)
	return a
}

func dayRewardInfo(lastFree int64) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	info.PutSFSArray("dayReward", freebieReward())
	info.PutLong("lastReceiveFreeTime", lastFree)
	return info
}

func TestEventShopFreebiesPerTypeUnitsAndGates(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	today := evTestTomorrow.Add(-23 * time.Hour)
	replies := map[string]*sfs.SFSObject{
		"99011": dayRewardInfo(yesterday.Unix()),      // Monopoly, seconds, yesterday: claim
		"99020": dayRewardInfo(today.Unix()),          // Bargain, seconds, today: skip
		"99047": dayRewardInfo(yesterday.UnixMilli()), // Slots, ms, yesterday: claim
		"99050": dayRewardInfo(today.UnixMilli()),     // Slots, ms, today: skip
		"5301": func() *sfs.SFSObject { // Blue Store with no pack: skip
			i := sfs.NewSFSObject()
			i.PutLong("lastReceiveFreeTime", 0)
			return i
		}(),
	}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return replies[p.GetString("activityId")]
		}
		return evOK()
	})
	in := evInit(30, evActivity(99011), evActivity(99020), evActivity(99047), evActivity(99050), evActivity(5301))
	if err := runEventShopFreebies(conn, in); err != nil {
		t.Fatal(err)
	}
	var claims []string
	for _, r := range fake.requests() {
		if r.Cmd != "hero.event.info.get" {
			v, _ := r.Params.Get("activityId")
			if v.Type != sfs.SFSInt {
				t.Errorf("%s activityId type %d, want Int", r.Cmd, v.Type)
			}
			claims = append(claims, r.Cmd+":"+evString(r.Params, "activityId"))
		}
	}
	if got := strings.Join(claims, ","); got != "rich.man.day.reward:99011,slots.daily.reward:99047" {
		t.Errorf("claims = %s", got)
	}
}

func TestEventShopFreebiesOwnInfoCommands(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "get.lucky.roll.info":
			r := sfs.NewSFSObject()
			r.PutSFSArray("Lucky_roulette_reward", freebieReward())
			ri := sfs.NewSFSObject()
			ri.PutLong("lastReceiveFreeTime", yesterday.UnixMilli())
			r.PutSFSObject("rollInfo", ri)
			return r
		case "activity.mak.food.info":
			e := sfs.NewSFSObject()
			e.PutInt("id", 7)
			e.PutInt("freeReward", 0)
			e.PutSFSArray("box_reward", freebieReward())
			arr := sfs.NewSFSArray()
			arr.AddSFSObject(e)
			r := sfs.NewSFSObject()
			r.PutSFSArray("info", arr)
			return r
		case "get.activity.gift.box.info":
			r := sfs.NewSFSObject()
			r.PutInt("free", 1) // claimed
			r.PutSFSArray("boxReward", freebieReward())
			return r
		case "survival.vip.gift.get.info":
			r := sfs.NewSFSObject()
			r.PutBool("isShow", true)
			r.PutBool("freeReceived", false)
			return r
		}
		return evOK()
	})
	in := evInit(30, evActivity(14), evActivity(99003), evActivity(98001), evActivity(98801))
	if err := runEventShopFreebies(conn, in); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := "get.lucky.roll.info,roll.receive.free.reward,get.activity.gift.box.info," +
		"activity.mak.food.info,activity.mak.food.free.reward,survival.vip.gift.get.info,survival.vip.gift.receive.free"
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	food := fake.only("activity.mak.food.free.reward")[0].Params
	if food.GetInt("aid") != 99003 || food.GetInt("id") != 7 {
		t.Errorf("cooking claim = %v, want {aid:99003, id:7}", food)
	}
	if fake.only("get.lucky.roll.info")[0].Params.GetInt("activityId") != 14 {
		t.Error("lucky roll info must take the activity id as an Int")
	}
}

func TestEvFreeFlagDue(t *testing.T) {
	mk := func(kv map[string]any) *sfs.SFSObject {
		o := sfs.NewSFSObject()
		for k, v := range kv {
			switch x := v.(type) {
			case int:
				o.PutInt(k, int32(x))
			case *sfs.SFSArray:
				o.PutSFSArray(k, x)
			}
		}
		return o
	}
	cases := []struct {
		name      string
		o         *sfs.SFSObject
		rewardKey bool
		want      bool
	}{
		{"flag 0 with box reward", mk(map[string]any{"free": 0, "boxReward": freebieReward()}), false, true},
		{"flag 1", mk(map[string]any{"free": 1, "boxReward": freebieReward()}), false, false},
		{"no flag", mk(map[string]any{"boxReward": freebieReward()}), false, false},
		{"free_reward is the list, freeReward the flag", mk(map[string]any{"freeReward": 0, "free_reward": freebieReward()}), false, true},
		{"reward key ignored for gift box", mk(map[string]any{"free": 0, "reward": freebieReward()}), false, false},
		{"reward key used elsewhere", mk(map[string]any{"free": 0, "reward": freebieReward()}), true, true},
		{"last flag wins", mk(map[string]any{"free_reward": 0, "free": 1, "boxReward": freebieReward()}), false, false},
	}
	for _, c := range cases {
		if got := evFreeFlagDue(c.o, c.rewardKey); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestEventShopFreebiesBenignFailureAndNoTomorrow(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour).Unix()
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			return dayRewardInfo(yesterday)
		case "rich.man.day.reward":
			return evErr(evAlreadyExecuted)
		}
		return evErr("E555")
	})
	err := runEventShopFreebies(conn, evInit(30, evActivity(99011), evActivity(99020)))
	if err == nil || !strings.Contains(err.Error(), "E555") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the bargain E555 failure", err)
	}
	if n := len(fake.requests()); n != 4 {
		t.Errorf("sent %d requests, want 4 (keep going past the first claim)", n)
	}

	in := evInit(30, evActivity(99011))
	in.Raw = sfs.NewSFSObject() // the same activity, no tomorrow
	in.Raw.PutSFSObject("activity", evInit(30, evActivity(99011)).Object("activity"))
	conn2, fake2 := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runEventShopFreebies(conn2, in); err != nil || len(fake2.requests()) != 0 {
		t.Errorf("no tomorrow: err %v, %d requests; want a silent skip", err, len(fake2.requests()))
	}
}
