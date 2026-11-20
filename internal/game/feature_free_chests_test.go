package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// rewardDayStart returns the start of the server day containing now for a 02:00 UTC reset, the
// live value (init tomorrow is the next one).
func rewardDayStart(now time.Time) time.Time {
	start := now.UTC().Truncate(24 * time.Hour).Add(2 * time.Hour)
	if now.Before(start) {
		start = start.Add(-24 * time.Hour)
	}
	return start
}

// rewardInitWithDay is rewardInit plus init tomorrow and an HQ of the given level.
func rewardInitWithDay(hq int32, fill func(raw *sfs.SFSObject)) *Init {
	return rewardInit(func(raw *sfs.SFSObject) {
		raw.PutLong("tomorrow", rewardDayStart(time.Now()).Add(24*time.Hour).Unix())
		builds := sfs.NewSFSArray()
		builds.AddSFSObject(NewTestBuildingSFS(9001, claimHQBuildingID, hq))
		raw.PutSFSArray("building_new", builds)
		fill(raw)
	})
}

// freeChestsInit sets the store entry's lastTime (seconds) and the weekly package's
// lastRewardTime (ms) to the given instants.
func freeChestsInit(hq int32, store, weekly time.Time) *Init {
	return rewardInitWithDay(hq, func(raw *sfs.SFSObject) {
		arr := sfs.NewSFSArray()
		e := sfs.NewSFSObject()
		e.PutInt("type", 1)
		e.PutLong("lastTime", store.Unix())
		arr.AddSFSObject(e)
		raw.PutSFSArray("freeRewardArr", arr)
		w := sfs.NewSFSObject()
		w.PutLong("lastRewardTime", weekly.UnixMilli())
		raw.PutSFSObject("weekFreeReward", w)
	})
}

func freeChestsServer(t *testing.T, weekCard time.Time, claimReply func(cmd string) *sfs.SFSObject) (*rewardFake, func(*Init) error) {
	t.Helper()
	return freeChestsServerInfo(t, func() *sfs.SFSObject {
		resp := sfs.NewSFSObject()
		free := sfs.NewSFSObject()
		free.PutLong("lastRewardTime", weekCard.UnixMilli())
		resp.PutSFSObject("dailyFree", free)
		return resp
	}, claimReply)
}

// freeChestsServerInfo is freeChestsServer with the whole get.week.card.info reply given.
func freeChestsServerInfo(t *testing.T, info func() *sfs.SFSObject, claimReply func(cmd string) *sfs.SFSObject) (*rewardFake, func(*Init) error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == weekCardInfoCmd {
			return info()
		}
		return claimReply(cmd)
	})
	return fake, func(in *Init) error { return runFreeChests(conn, in) }
}

// freeChestsHQ is above every free-chests unlock (store HQ 2, weekly package HQ 8).
const freeChestsHQ = 20

func TestFreeChestsClaimsAllThreeWhenDue(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	fake, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardOK() })
	if err := run(freeChestsInit(freeChestsHQ, yesterday, yesterday)); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{freeChestsStoreCmd, freeChestsWeeklyCmd, weekCardInfoCmd, freeChestsWeekCardCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	reqs := fake.requests()
	if p := reqs[0].Params; p.GetInt("type") != 1 || p.GetInt("isAutoClaim") != 1 {
		t.Errorf("store claim params = %v, want type 1, isAutoClaim 1", p)
	}
	if p := reqs[2].Params; p.GetInt("isLogin") != 1 {
		t.Errorf("week card info params = %v, want isLogin 1", p)
	}
	if len(session.ParamKeysWithoutID(reqs[1].Params))+len(session.ParamKeysWithoutID(reqs[3].Params)) != 0 {
		t.Error("the weekly and week-card claims must carry no params")
	}
}

func TestFreeChestsSkipsWhatWasClaimedToday(t *testing.T) {
	// lastTime is in seconds: read as ms it would date from 1970 and look claimable.
	earlier := rewardDayStart(time.Now()).Add(time.Minute)
	if earlier.After(time.Now()) {
		earlier = time.Now()
	}
	fake, run := freeChestsServer(t, earlier, func(string) *sfs.SFSObject { return rewardOK() })
	if err := run(freeChestsInit(freeChestsHQ, earlier, earlier)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{weekCardInfoCmd}) {
		t.Errorf("sent %v, want only the week card info read", got)
	}
}

func TestFreeChestsGates(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	for name, c := range map[string]struct {
		in   *Init
		want []string
	}{
		"no init": {&Init{}, nil},
		"no server day anchor": {rewardInit(func(raw *sfs.SFSObject) {
			raw.PutSFSObject("weekFreeReward", sfs.NewSFSObject())
		}), nil},
		"HQ below the store unlock": {freeChestsInit(1, yesterday, yesterday),
			[]string{weekCardInfoCmd, freeChestsWeekCardCmd}},
		"HQ below the weekly package unlock (8)": {freeChestsInit(7, yesterday, yesterday),
			[]string{freeChestsStoreCmd, weekCardInfoCmd, freeChestsWeekCardCmd}},
		"HQ at the weekly package unlock": {freeChestsInit(8, yesterday, yesterday),
			[]string{freeChestsStoreCmd, freeChestsWeeklyCmd, weekCardInfoCmd, freeChestsWeekCardCmd}},
		"no store entry or weekly package in init": {rewardInitWithDay(freeChestsHQ, func(*sfs.SFSObject) {}),
			[]string{weekCardInfoCmd, freeChestsWeekCardCmd}},
	} {
		t.Run(name, func(t *testing.T) {
			fake, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardOK() })
			if err := run(c.in); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestFreeChestsBenignAndFailure(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	_, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardErr("120289") })
	if err := run(freeChestsInit(freeChestsHQ, yesterday, yesterday)); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}

	fake, run := freeChestsServer(t, yesterday, func(cmd string) *sfs.SFSObject {
		if cmd == freeChestsStoreCmd {
			return rewardErr("999999")
		}
		return rewardOK()
	})
	if err := run(freeChestsInit(freeChestsHQ, yesterday, yesterday)); err == nil {
		t.Error("run = nil, want the store claim failure")
	}
	if n := len(fake.cmds()); n != 4 {
		t.Errorf("sent %d requests, want all 4: one failure must not stop the other claims", n)
	}
}

// TestFreeChestsSwitchGate: init dataConfig free_chest_receive set to anything but 1 drops the
// weekly package and the week-card free gift, which the client builds only behind it
// (UISubscriptionListPanelView.lua:27-59); the store free reward is outside it, and an absent
// switch keeps every claim.
func TestFreeChestsSwitchGate(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	withSwitch := func(v *sfs.SFSValue) *Init {
		in := freeChestsInit(freeChestsHQ, yesterday, yesterday)
		if v != nil {
			dc := sfs.NewSFSObject()
			dc.PutValue("free_chest_receive", *v) // a literal, so a wrong freeChestSwitch fails
			in.Raw.PutSFSObject("dataConfig", dc)
		}
		return in
	}
	all := []string{freeChestsStoreCmd, freeChestsWeeklyCmd, weekCardInfoCmd, freeChestsWeekCardCmd}
	for name, c := range map[string]struct {
		v    *sfs.SFSValue
		want []string
	}{
		"absent":        {nil, all},
		"on (Int 1)":    {&sfs.SFSValue{Type: sfs.SFSInt, Val: int32(1)}, all},
		"on (\"1\")":    {&sfs.SFSValue{Type: sfs.SFSUtfString, Val: "1"}, all},
		"off (Int 0)":   {&sfs.SFSValue{Type: sfs.SFSInt, Val: int32(0)}, []string{freeChestsStoreCmd}},
		"off (\"0\")":   {&sfs.SFSValue{Type: sfs.SFSUtfString, Val: "0"}, []string{freeChestsStoreCmd}},
		"off (garbage)": {&sfs.SFSValue{Type: sfs.SFSUtfString, Val: "x"}, []string{freeChestsStoreCmd}},
	} {
		t.Run(name, func(t *testing.T) {
			fake, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardOK() })
			if err := run(withSwitch(c.v)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestFreeChestsWeeklyPackageSwitch is MASTER 1.0.370 B49 (aud-dailies F8, cov-ui-store F10): the
// new weekly package, the only 1.0.370 screen offering receive.week.free.reward, opens only with
// switch weekgift_new_switch on (CheckIfNewWeeklyPackageOpen, GiftPackManager.lua:406-417). An init
// dataConfig carrying it off drops that claim alone; an absent key keeps it.
func TestFreeChestsWeeklyPackageSwitch(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	withSwitch := func(v *sfs.SFSValue) *Init {
		in := freeChestsInit(freeChestsHQ, yesterday, yesterday)
		if v != nil {
			dc := sfs.NewSFSObject()
			dc.PutValue("weekgift_new_switch", *v) // a literal, so a wrong freeChestsWeeklySwitch fails
			in.Raw.PutSFSObject("dataConfig", dc)
		}
		return in
	}
	all := []string{freeChestsStoreCmd, freeChestsWeeklyCmd, weekCardInfoCmd, freeChestsWeekCardCmd}
	noWeekly := []string{freeChestsStoreCmd, weekCardInfoCmd, freeChestsWeekCardCmd}
	for name, c := range map[string]struct {
		v    *sfs.SFSValue
		want []string
	}{
		"absent":      {nil, all},
		"on (\"1\")":  {&sfs.SFSValue{Type: sfs.SFSUtfString, Val: "1"}, all},
		"off (\"0\")": {&sfs.SFSValue{Type: sfs.SFSUtfString, Val: "0"}, noWeekly},
		"off (Int 0)": {&sfs.SFSValue{Type: sfs.SFSInt, Val: int32(0)}, noWeekly},
	} {
		t.Run(name, func(t *testing.T) {
			fake, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardOK() })
			if err := run(withSwitch(c.v)); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestFreeChestsWeekCardGiftWithoutDailyFree is MASTER 1.0.370 B47 (aud-dailies F4, cov-dc-world
// F2): the client's lastRecvFreeTime starts at 0 and get.week.card.info updates it only when the
// reply carries dailyFree (WeekCardManager.lua:7; GetWeekCardListMessage.lua:26-28), so a reply
// without it shows the gift as claimable and Go claims it. A dailyFree that is not an object, or a
// refused info read, still skips the gift. A missing init weekFreeReward stays a skip, at Warn.
func TestFreeChestsWeekCardGiftWithoutDailyFree(t *testing.T) {
	for name, c := range map[string]struct {
		info    func() *sfs.SFSObject
		want    []string
		wantErr bool
	}{
		"no dailyFree": {sfs.NewSFSObject, []string{weekCardInfoCmd, freeChestsWeekCardCmd}, false},
		"dailyFree without lastRewardTime": {func() *sfs.SFSObject {
			r := sfs.NewSFSObject()
			r.PutSFSObject("dailyFree", sfs.NewSFSObject())
			return r
		}, []string{weekCardInfoCmd, freeChestsWeekCardCmd}, false},
		"dailyFree not an object": {func() *sfs.SFSObject {
			r := sfs.NewSFSObject()
			r.PutLong("dailyFree", 1)
			return r
		}, []string{weekCardInfoCmd}, false},
		"info refused": {func() *sfs.SFSObject { return rewardErr("E42") }, []string{weekCardInfoCmd}, true},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureRewardLogs(t)
			fake, run := freeChestsServerInfo(t, c.info, func(string) *sfs.SFSObject { return rewardOK() })
			// No store entry and no weekFreeReward in init: only the week-card gift is left.
			if err := run(rewardInitWithDay(freeChestsHQ, func(*sfs.SFSObject) {})); (err != nil) != c.wantErr {
				t.Fatalf("run = %v, want error %v", err, c.wantErr)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
			if !strings.Contains(logs.String(), `level=WARN msg="free-chests: init has no weekFreeReward`) {
				t.Errorf("a missing weekFreeReward is not logged at Warn:\n%s", logs.String())
			}
		})
	}
}
