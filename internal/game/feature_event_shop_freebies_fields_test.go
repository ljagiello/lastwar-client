package game

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// B97: a Lucky Roll reply without rollInfo.lastReceiveFreeTime (or without rollInfo) and a Bargain
// Shop reply without lastReceiveFreeTime are not claimed (ActLuckyRollInfo.lua:24-33,
// UILuckyRollShopItem.lua:42-47; ActBargainShopInfo.lua:64-66, 138), and each skip logs a Warn
// naming the activity and the field. A present 0 is claimed. Monopoly, Slots and Blue Store keep
// "absent = never claimed" (their data classes default the field to 0).
func TestEventShopFreebiesLastFreeTimeMustBePresent(t *testing.T) {
	withEvNow(t, evTestNow)
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(saved) })

	luckyRoll := map[int32]*sfs.SFSObject{}
	for id, rollInfo := range map[int32]*sfs.SFSObject{
		14:   nil, // no rollInfo at all
		15:   sfs.NewSFSObject(),
		1002: func() *sfs.SFSObject { o := sfs.NewSFSObject(); o.PutLong("lastReceiveFreeTime", 0); return o }(),
	} {
		r := sfs.NewSFSObject()
		r.PutSFSArray("Lucky_roulette_reward", freebieReward())
		if rollInfo != nil {
			r.PutSFSObject("rollInfo", rollInfo)
		}
		luckyRoll[id] = r
	}
	noTime := func() *sfs.SFSObject {
		o := sfs.NewSFSObject()
		o.PutSFSArray("dayReward", freebieReward())
		return o
	}
	events := map[string]*sfs.SFSObject{
		"99020": noTime(),         // Bargain Shop without the field: skip
		"99040": dayRewardInfo(0), // Bargain Shop, present 0: claim
		"99011": noTime(),         // Monopoly without the field: claim (defaults to 0)
		"99047": noTime(),         // Slots without the field: claim
	}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "get.lucky.roll.info":
			return luckyRoll[p.GetInt("activityId")]
		case "hero.event.info.get":
			if r, ok := events[p.GetString("activityId")]; ok {
				return r
			}
			return sfs.NewSFSObject()
		}
		return evOK()
	})
	in := evInit(30, evActivity(14), evActivity(15), evActivity(1002), evActivity(99020), evActivity(99040),
		evActivity(99011), evActivity(99047))
	if err := runEventShopFreebies(conn, in); err != nil {
		t.Fatal(err)
	}
	var claims []string
	for _, r := range fake.requests() {
		if r.Cmd != "hero.event.info.get" && r.Cmd != "get.lucky.roll.info" {
			claims = append(claims, r.Cmd+":"+evString(r.Params, "activityId"))
		}
	}
	want := "roll.receive.free.reward:1002,rich.man.day.reward:99011,bargain.day.reward:99040,slots.daily.reward:99047"
	if got := strings.Join(claims, ","); got != want {
		t.Errorf("claims = %s\nwant     %s", got, want)
	}
	out := logs.String()
	for _, w := range []string{"activity=14 ", "activity=15 ", "activity=99020 ", "missing=rollInfo.lastReceiveFreeTime", "missing=lastReceiveFreeTime"} {
		if !strings.Contains(out, w) {
			t.Errorf("warn log lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "activity=99011 ") || strings.Contains(out, "activity=1002 ") {
		t.Errorf("warned for a type or reply that keeps absent = never claimed:\n%s", out)
	}
}

// B97: with the field present, Lucky Roll and Bargain Shop keep the day test.
func TestEventShopFreebiesLastFreeTimeToday(t *testing.T) {
	withEvNow(t, evTestNow)
	today := evTestTomorrow.Add(-23 * time.Hour)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "get.lucky.roll.info":
			r := sfs.NewSFSObject()
			r.PutSFSArray("Lucky_roulette_reward", freebieReward())
			ri := sfs.NewSFSObject()
			ri.PutLong("lastReceiveFreeTime", today.UnixMilli())
			r.PutSFSObject("rollInfo", ri)
			return r
		case "hero.event.info.get":
			return dayRewardInfo(today.Unix())
		}
		return evOK()
	})
	if err := runEventShopFreebies(conn, evInit(30, evActivity(14), evActivity(99020))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.lucky.roll.info,hero.event.info.get" {
		t.Errorf("sent %s, want only the two reads (both claimed today)", got)
	}
}

// B62: survival.vip.gift.get.info's isShow is hidden as Bool false, as the strings "0"/"false" in
// any case, and as an integer 0 (VipGiftActDataManager.lua:610-616).
func TestSurvivalVipGiftIsShowShapes(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name string
		set  func(o *sfs.SFSObject)
		want bool
	}{
		{"absent", func(*sfs.SFSObject) {}, true},
		{"bool true", func(o *sfs.SFSObject) { o.PutBool("isShow", true) }, true},
		{"bool false", func(o *sfs.SFSObject) { o.PutBool("isShow", false) }, false},
		{"string 0", func(o *sfs.SFSObject) { o.PutUtfString("isShow", "0") }, false},
		{"string FALSE", func(o *sfs.SFSObject) { o.PutUtfString("isShow", "FALSE") }, false},
		{"string 1", func(o *sfs.SFSObject) { o.PutUtfString("isShow", "1") }, true},
		{"int 0", func(o *sfs.SFSObject) { o.PutInt("isShow", 0) }, false},
		{"long 0", func(o *sfs.SFSObject) { o.PutLong("isShow", 0) }, false},
		{"int 1", func(o *sfs.SFSObject) { o.PutInt("isShow", 1) }, true},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "survival.vip.gift.get.info" {
				r := sfs.NewSFSObject()
				c.set(r)
				r.PutBool("freeReceived", false)
				return r
			}
			return evOK()
		})
		if err := runEventShopFreebies(conn, evInit(30, evActivity(98801))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := len(fake.only("survival.vip.gift.receive.free")) == 1; got != c.want {
			t.Errorf("%s: claimed %v, want %v", c.name, got, c.want)
		}
	}
}
