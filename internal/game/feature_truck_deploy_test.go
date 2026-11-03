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

func tdTestRun(t *testing.T, in *Init) *rewardFake {
	t.Helper()
	sends := int32(0)
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case truckDeploySendCmd:
			sends++
			r := sfs.NewSFSObject()
			r.PutInt("dailySendCount", sends)
			return r
		case "formation.save.template":
			return sfs.NewSFSObject()
		}
		return rewardErr("E999")
	})
	if err := runTruckDeploy(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	return fake
}

func TestTruckDeployOnlyURTrucks(t *testing.T) {
	in := plTestInit(func(*sfs.SFSObject) {}, 0, 0,
		tdTestTruck(11, 130, 1, false, 0), // UR, parked
		tdTestTruck(12, 100, 2, false, 0), // below UR: stays parked
		tdTestTruck(13, 210, 3, false, 0), // sleigh, parked: counts as UR
		tdTestTruck(14, 130, 4, true, 2),  // out with defence squad 2
	)
	f := tdTestRun(t, in)
	var sent []int64
	var squads []int64
	reqs := f.requests()
	for i, r := range reqs {
		if r.Cmd != truckDeploySendCmd {
			continue
		}
		id, _ := claimInt(r.Params, "uuid")
		sq, _ := claimInt(r.Params, "squadNoClient")
		sent, squads = append(sent, id), append(squads, sq)
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
	// The sleigh (quality 10) goes first; truck 12 is below UR; truck 14 is out and its squad 2
	// busy. Defence squads by power: 1 (6M), then 3 (3M); 2 is busy.
	if !slices.Equal(sent, []int64{13, 11}) {
		t.Fatalf("sent %v, want the sleigh (13) then the UR truck (11)", sent)
	}
	if !slices.Equal(squads, []int64{1, 3}) {
		t.Errorf("used squads %v, want 1 then 3 (2 is escorting)", squads)
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
		"only below UR": {plTestInit(func(*sfs.SFSObject) {}, 0, 0, tdTestTruck(12, 100, 2, false, 0)), 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := tdTestRun(t, tc.in)
			n := 0
			for _, c := range f.cmds() {
				if c == truckDeploySendCmd {
					n++
				}
			}
			if n != tc.want {
				t.Errorf("sent %d trucks (%v), want %d", n, f.cmds(), tc.want)
			}
		})
	}
}
