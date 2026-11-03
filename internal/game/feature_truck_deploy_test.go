package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// tdTestTruck is one of the account's own trucks at truck-station building uuid 500+slot.
func tdTestTruck(uuid int64, cfgID int32, slot int64, out bool, squad int32) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutLong("uuid", uuid)
	o.PutInt("cfgId", cfgID)
	o.PutLong("relationId", 500+slot)
	if out {
		o.PutLong("sendTime", time.Now().Add(-time.Hour).UnixMilli())
		o.PutLong("arriveTime", time.Now().Add(time.Hour).UnixMilli())
	} else {
		o.PutLong("sendTime", 0)
	}
	mi := sfs.NewSFSObject()
	mi.PutInt("squadNoClient", squad)
	o.PutSFSObject("marchInfo", mi)
	return o
}

// tdTestFake answers truck-deploy's requests; send returns the reply to the n-th train.send
// (nil = success), station the train.data reply (nil = an error, so init is used).
type tdTestFake struct {
	send    func(n int) *sfs.SFSObject
	station *sfs.SFSObject
}

func tdRun(t *testing.T, in *Init, c tdTestFake) (*rewardFake, error) {
	t.Helper()
	sends := int32(0)
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case truckDeploySendCmd:
			if c.send != nil {
				if r := c.send(int(sends) + 1); r != nil {
					return r
				}
			}
			sends++
			r := sfs.NewSFSObject()
			r.PutInt("dailySendCount", sends)
			return r
		case "formation.save.template":
			return sfs.NewSFSObject()
		case "train.data":
			if c.station != nil {
				return c.station
			}
		}
		return rewardErr("E999")
	})
	return fake, runTruckDeploy(conn, in)
}

func tdTestRun(t *testing.T, in *Init) *rewardFake {
	t.Helper()
	f, err := tdRun(t, in, tdTestFake{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return f
}

// tdSends lists (truck uuid, squadNoClient) for every train.send, checking each was preceded by
// the matching formation.save.template.
func tdSends(t *testing.T, f *rewardFake) (trucks, squads []int64) {
	t.Helper()
	reqs := f.requests()
	for i, r := range reqs {
		if r.Cmd != truckDeploySendCmd {
			continue
		}
		id, _ := claimInt(r.Params, "uuid")
		sq, _ := claimInt(r.Params, "squadNoClient")
		trucks, squads = append(trucks, id), append(squads, sq)
		if i == 0 || reqs[i-1].Cmd != "formation.save.template" {
			t.Errorf("train.send for %d wasn't preceded by formation.save.template", id)
		} else if idx, _ := claimInt(reqs[i-1].Params, "index"); idx != sq+railwayDefenceAction {
			t.Errorf("saved template %d for squad %d, want %d", idx, sq, sq+railwayDefenceAction)
		}
		for _, h := range claimObjects(r.Params, "heroInfo") {
			if v, _ := h.Get("index"); v.Type != sfs.SFSInt {
				t.Errorf("heroInfo index is type %v, want Int", v.Type)
			}
		}
	}
	return trucks, squads
}

// tdFourSquads unlocks world squad 4 and adds a fourth defence squad, so a free squad is always
// left over.
func tdFourSquads(raw *sfs.SFSObject) {
	plTestWorldSquads(raw, 4)
	ft := sfs.NewSFSArray()
	for _, t := range plTestTemplates() {
		ft.AddSFSObject(t)
	}
	ft.AddSFSObject(plTestSquad(8, 4, 2300, 1, 1_000_000))
	raw.PutSFSArray("formation_template", ft)
}

func TestTruckDeployOnlyURTrucks(t *testing.T) {
	in := plTestInit(tdFourSquads, 0, 0,
		tdTestTruck(11, 130, 1, false, 0), // UR, parked
		tdTestTruck(12, 100, 2, false, 0), // below UR: stays parked though a squad is free
		tdTestTruck(13, 210, 3, false, 0), // sleigh, parked: counts as UR
		tdTestTruck(14, 130, 4, true, 2),  // out with defence squad 2
	)
	trucks, squads := tdSends(t, tdTestRun(t, in))
	// The sleigh (quality 10) goes first; truck 14 is out and its squad 2 busy. Defence squads by
	// power: 1 (6M), 3 (3M), 4 (1M); 2 is busy.
	if !slices.Equal(trucks, []int64{13, 11}) {
		t.Fatalf("sent %v, want the sleigh (13) then the UR truck (11), and never 12", trucks)
	}
	if !slices.Equal(squads, []int64{1, 3}) {
		t.Errorf("used squads %v, want 1 then 3 (2 is escorting)", squads)
	}
}

func TestTruckDeployRefusals(t *testing.T) {
	trucks := []*sfs.SFSObject{tdTestTruck(11, 130, 1, false, 0), tdTestTruck(13, 210, 3, false, 0)}
	// The server says the first squad is escorting (an errorCode with the truck uuid): try the next.
	escorting := func(n int) *sfs.SFSObject {
		if n == 1 {
			r := rewardErr("E120014")
			r.PutLong("uuid", 13)
			return r
		}
		return nil
	}
	f, err := tdRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trucks...), tdTestFake{send: escorting})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, squads := tdSends(t, f); !slices.Equal(squads, []int64{1, 2, 3}) {
		t.Errorf("squads tried %v, want 1 (refused) then 2 for the sleigh, and 3 for the UR truck", squads)
	}
	// Any other refusal stops the run with an error.
	stop := func(int) *sfs.SFSObject { return rewardErr("E999999") }
	f, err = tdRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trucks...), tdTestFake{send: stop})
	if got, _ := tdSends(t, f); err == nil || len(got) != 1 {
		t.Errorf("run = %v after %v; want one send and an error", err, got)
	}
}

func TestTruckDeploySkipsSquadsWithHeroesOut(t *testing.T) {
	out := tdTestTruck(14, 130, 4, true, 0) // out, squad unknown, but carrying hero id 77001
	hi := sfs.NewSFSObject()
	h := sfs.NewSFSObject()
	h.PutInt("id", 77001)
	hi.PutSFSObject("1", h)
	evObject(out, "marchInfo").PutSFSObject("heroInfo", hi)
	in := plTestInit(func(raw *sfs.SFSObject) {
		uh := sfs.NewSFSArray()
		o := sfs.NewSFSObject()
		o.PutLong("uuid", 2000) // a hero of defence squad 1
		o.PutInt("heroId", 77001)
		uh.AddSFSObject(o)
		raw.PutSFSArray("userHero", uh)
	}, 0, 0, tdTestTruck(11, 130, 1, false, 0), out)
	if _, squads := tdSends(t, tdTestRun(t, in)); !slices.Equal(squads, []int64{2}) {
		t.Errorf("used squads %v, want 2: squad 1 (the strongest) has a hero out with a truck", squads)
	}
}

func TestTruckDeployGates(t *testing.T) {
	parked := tdTestTruck(11, 130, 1, false, 0)
	for name, tc := range map[string]struct {
		in   *Init
		want int
	}{
		"one send left": {plTestInit(func(*sfs.SFSObject) {}, 3, 0, parked, tdTestTruck(13, 210, 3, false, 0)), 1},
		"no sends left": {plTestInit(func(*sfs.SFSObject) {}, 4, 0, parked), 0},
		"still fogged":  {plTestInit(func(raw *sfs.SFSObject) { railwayStage(raw, railwayFogStage-2) }, 0, 0, parked), 0},
		"in another warzone": {plTestInit(func(raw *sfs.SFSObject) {
			u := sfs.NewSFSObject()
			u.PutInt("serverId", plTestOther)
			u.PutInt("crossFightSrcServerId", plTestHome)
			raw.PutSFSObject("user", u)
		}, 0, 0, parked), 0},
		"only below UR": {plTestInit(tdFourSquads, 0, 0, tdTestTruck(12, 100, 2, false, 0)), 0},
		"send count unknown": {plTestInit(func(raw *sfs.SFSObject) {
			old := evObject(raw, "trainData")
			n := sfs.NewSFSObject()
			for _, k := range []string{"openReward", "openTime", "myTrainList"} {
				v, _ := old.Get(k)
				n.PutValue(k, v)
			}
			raw.PutSFSObject("trainData", n)
		}, 0, 0, parked), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got, _ := tdSends(t, tdTestRun(t, tc.in)); len(got) != tc.want {
				t.Errorf("sent %v, want %d trucks", got, tc.want)
			}
		})
	}

	// The fresh train.data reply wins over init: it says today's sends are used up.
	st := sfs.NewSFSObject()
	st.PutInt("openReward", 1)
	st.PutLong("openTime", time.Now().Add(-48*time.Hour).UnixMilli())
	st.PutInt("dailySendCount", truckDeployBaseSends)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(parked)
	st.PutSFSArray("myTrainList", arr)
	f, err := tdRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, parked), tdTestFake{station: st})
	if got, _ := tdSends(t, f); err != nil || len(got) != 0 {
		t.Errorf("run = %v, sent %v; want nothing once train.data shows the sends used up", err, got)
	}
}
