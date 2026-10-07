package game

import (
	"slices"
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
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == freeChestsWeekInfoCmd {
			resp := sfs.NewSFSObject()
			free := sfs.NewSFSObject()
			free.PutLong("lastRewardTime", weekCard.UnixMilli())
			resp.PutSFSObject("dailyFree", free)
			return resp
		}
		return claimReply(cmd)
	})
	return fake, func(in *Init) error { return runFreeChests(conn, in) }
}

func TestFreeChestsClaimsAllThreeWhenDue(t *testing.T) {
	yesterday := rewardDayStart(time.Now()).Add(-time.Hour)
	fake, run := freeChestsServer(t, yesterday, func(string) *sfs.SFSObject { return rewardOK() })
	if err := run(freeChestsInit(5, yesterday, yesterday)); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{freeChestsStoreCmd, freeChestsWeeklyCmd, freeChestsWeekInfoCmd, freeChestsWeekCardCmd}
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
	if err := run(freeChestsInit(5, earlier, earlier)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{freeChestsWeekInfoCmd}) {
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
			[]string{freeChestsWeeklyCmd, freeChestsWeekInfoCmd, freeChestsWeekCardCmd}},
		"no store entry or weekly package in init": {rewardInitWithDay(5, func(*sfs.SFSObject) {}),
			[]string{freeChestsWeekInfoCmd, freeChestsWeekCardCmd}},
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
	if err := run(freeChestsInit(5, yesterday, yesterday)); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}

	fake, run := freeChestsServer(t, yesterday, func(cmd string) *sfs.SFSObject {
		if cmd == freeChestsStoreCmd {
			return rewardErr("999999")
		}
		return rewardOK()
	})
	if err := run(freeChestsInit(5, yesterday, yesterday)); err == nil {
		t.Error("run = nil, want the store claim failure")
	}
	if n := len(fake.cmds()); n != 4 {
		t.Errorf("sent %d requests, want all 4: one failure must not stop the other claims", n)
	}
}
