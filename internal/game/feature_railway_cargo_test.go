package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

type railwayTruck struct {
	sent, arrive time.Time // zero sent: parked
}

// railwayInit builds init trainData; openReward < 0 omits the field.
func railwayInit(openReward int32, openTime time.Time, trucks ...railwayTruck) *Init {
	return rewardInitWithDay(20, func(raw *sfs.SFSObject) {
		td := sfs.NewSFSObject()
		if openReward >= 0 {
			td.PutInt("openReward", openReward)
		}
		td.PutLong("openTime", openTime.UnixMilli())
		arr := sfs.NewSFSArray()
		for _, tr := range trucks {
			o := sfs.NewSFSObject()
			var sent, arrive int64
			if !tr.sent.IsZero() {
				sent, arrive = tr.sent.UnixMilli(), tr.arrive.UnixMilli()
			}
			o.PutLong("sendTime", sent)
			o.PutLong("arriveTime", arrive)
			arr.AddSFSObject(o)
		}
		td.PutSFSArray("myTrainList", arr)
		raw.PutSFSObject("trainData", td)
	})
}

func railwayRun(t *testing.T, in *Init, reply func(cmd string) *sfs.SFSObject) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject { return reply(cmd) })
	return fake, runRailwayCargo(conn, in)
}

func TestRailwayCargoClaimsArrivedTrucksOnce(t *testing.T) {
	now := time.Now()
	in := railwayInit(1, now.Add(-30*24*time.Hour),
		railwayTruck{now.Add(-3 * time.Hour), now.Add(-time.Hour)}, // arrived
		railwayTruck{now.Add(-3 * time.Hour), now.Add(-time.Minute)},
		railwayTruck{now.Add(-time.Hour), now.Add(time.Hour)}, // travelling
		railwayTruck{},
	)
	fake, err := railwayRun(t, in, func(string) *sfs.SFSObject {
		resp := sfs.NewSFSObject()
		list := sfs.NewSFSArray()
		list.AddSFSObject(rewardOK())
		resp.PutSFSArray("trainRewardList", list)
		return resp
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{railwayBatchCmd}) {
		t.Fatalf("sent %v, want one train.batch.reward", got)
	}
	if n := len(session.ParamKeysWithoutID(fake.requests()[0].Params)); n != 0 {
		t.Errorf("train.batch.reward carried %d params, want none", n)
	}
}

func TestRailwayCargoOpeningReward(t *testing.T) {
	now := time.Now()
	ok := func(string) *sfs.SFSObject { return rewardOK() }
	fake, err := railwayRun(t, railwayInit(0, now.Add(-time.Hour)), ok)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{railwayFirstOpenCmd}) {
		t.Errorf("sent %v, want the opening reward only", got)
	}
	for name, in := range map[string]*Init{
		"not open yet":    railwayInit(0, now.Add(time.Hour)),
		"already taken":   railwayInit(1, now.Add(-time.Hour)),
		"field missing":   railwayInit(-1, now.Add(-time.Hour)),
		"no trainData":    rewardInitWithDay(20, func(*sfs.SFSObject) {}),
		"no init":         {},
		"nothing arrived": railwayInit(1, now.Add(-time.Hour), railwayTruck{now, now.Add(time.Hour)}),
	} {
		t.Run(name, func(t *testing.T) {
			fake, err := railwayRun(t, in, ok)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
		})
	}
}

func TestRailwayCargoBenignAndFailure(t *testing.T) {
	now := time.Now()
	in := railwayInit(0, now.Add(-time.Hour), railwayTruck{now.Add(-2 * time.Hour), now.Add(-time.Hour)})
	if _, err := railwayRun(t, in, func(string) *sfs.SFSObject { return rewardErr("120289") }); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	fake, err := railwayRun(t, in, func(cmd string) *sfs.SFSObject {
		if cmd == railwayFirstOpenCmd {
			return rewardErr("999999") // e.g. a fogged-in station
		}
		return rewardOK()
	})
	if err == nil {
		t.Error("run = nil, want the opening-reward failure")
	}
	if got := fake.cmds(); !slices.Equal(got, []string{railwayFirstOpenCmd, railwayBatchCmd}) {
		t.Errorf("sent %v: the cargo claim must still go out after the opening reward fails", got)
	}
}
