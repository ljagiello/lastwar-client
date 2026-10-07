package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

type heroPool struct {
	id         string
	poolType   int32 // 0: omitted
	limit      int32
	next       int64 // dailyFreeNextFreshTime, seconds; < 0: omitted
	start, end int64 // seconds
	item       string
}

func freeRecruitsInit(officers []int32, pools ...heroPool) *Init {
	return rewardInitWithDay(20, func(raw *sfs.SFSObject) {
		arr := sfs.NewSFSArray()
		for _, p := range pools {
			o := sfs.NewSFSObject()
			o.PutUtfString("id", p.id)
			if p.poolType != 0 {
				o.PutInt("type", p.poolType)
			}
			o.PutInt("dailyFreeLimit", p.limit)
			if p.next >= 0 {
				o.PutLong("dailyFreeNextFreshTime", p.next)
			}
			o.PutLong("startTime", p.start)
			o.PutLong("endTime", p.end)
			if p.item != "" {
				o.PutUtfString("item", p.item)
			}
			arr.AddSFSObject(o)
		}
		raw.PutSFSArray("lotteryFreeInfo", arr)
		if officers != nil {
			ids := sfs.NewSFSArray()
			for _, id := range officers {
				ids.AddInt(id)
			}
			raw.PutSFSArray("workerOfficerIds", ids)
		}
	})
}

// runFreeRecruitsFake answers worker.lottery.info with nextFreeTime (ms; < 0 omits it) and every
// pull with pull. It fails the test if any pull is not useFree 1 / isTen 0.
func runFreeRecruitsFake(t *testing.T, in *Init, nextFreeMs int64, pull *sfs.SFSObject) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == freeRecruitsWorkerInfoCmd {
			resp := sfs.NewSFSObject()
			resp.PutInt("curNum", 0)
			if nextFreeMs >= 0 {
				resp.PutLong("nextFreeTime", nextFreeMs)
			}
			return resp
		}
		return pull
	})
	err := runFreeRecruits(conn, in)
	for _, r := range fake.requests() {
		if r.Cmd == freeRecruitsHeroCmd || r.Cmd == freeRecruitsWorkerCmd {
			if r.Params.GetInt("useFree") != 1 || r.Params.GetInt("isTen") != 0 {
				t.Errorf("%s sent useFree=%d isTen=%d: only useFree 1, isTen 0 may ever be sent",
					r.Cmd, r.Params.GetInt("useFree"), r.Params.GetInt("isTen"))
			}
		}
	}
	return fake, err
}

func TestFreeRecruitsPullsOnlyFreePools(t *testing.T) {
	now := time.Now()
	in := freeRecruitsInit([]int32{401100},
		heroPool{id: "221500", limit: 1, next: now.Add(-time.Minute).Unix(), item: "230006;1|230006;10"},                        // free now
		heroPool{id: "221600", limit: 1, next: now.Add(time.Hour).Unix()},                                                       // timer running
		heroPool{id: "221700", limit: 0, next: now.Add(-time.Hour).Unix()},                                                      // no free pulls
		heroPool{id: "221701", limit: 1, next: -1},                                                                              // timer missing
		heroPool{id: "221702", limit: 1, next: now.Add(-time.Hour).UnixMilli()},                                                 // a ms value read as s: far future
		heroPool{id: "301100", poolType: 2, limit: 1, next: 1},                                                                  // type 2 pool: not listed
		heroPool{id: "301102", limit: 1, next: 1, start: now.Add(-48 * time.Hour).Unix(), end: now.Add(-24 * time.Hour).Unix()}, // closed
	)
	fake, err := runFreeRecruitsFake(t, in, now.Add(-time.Minute).UnixMilli(), rewardOK())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{freeRecruitsHeroCmd, freeRecruitsWorkerInfoCmd, freeRecruitsWorkerCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	reqs := fake.requests()
	hero := reqs[0].Params
	if hero.GetString("id") != "221500" || hero.GetString("itemId") != "230006" {
		t.Errorf("hero pull = %v, want id 221500 and itemId 230006", hero)
	}
	if v, ok := hero.Get("aiPushStatus"); !ok || len(v.Val.(*sfs.SFSObject).Keys()) != 5 {
		t.Errorf("aiPushStatus = %v, want the five default switches", v)
	}
	if reqs[1].Params.GetInt("officerId") != 401100 || reqs[2].Params.GetInt("officerId") != 401100 {
		t.Errorf("worker requests carried officerId %d / %d, want 401100", reqs[1].Params.GetInt("officerId"), reqs[2].Params.GetInt("officerId"))
	}
}

func TestFreeRecruitsSkipsWhenNotFree(t *testing.T) {
	now := time.Now()
	for name, c := range map[string]struct {
		in     *Init
		nextMs int64
		want   []string
	}{
		"no init":              {&Init{}, 0, nil},
		"no officer ids":       {freeRecruitsInit(nil), 0, nil},
		"worker timer running": {freeRecruitsInit([]int32{401100}), now.Add(time.Hour).UnixMilli(), []string{freeRecruitsWorkerInfoCmd}},
		"worker timer missing": {freeRecruitsInit([]int32{401100}), -1, []string{freeRecruitsWorkerInfoCmd}},
	} {
		t.Run(name, func(t *testing.T) {
			fake, err := runFreeRecruitsFake(t, c.in, c.nextMs, rewardOK())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestFreeRecruitsBenignAndFailure(t *testing.T) {
	now := time.Now()
	in := freeRecruitsInit([]int32{401100}, heroPool{id: "221500", limit: 1, next: now.Add(-time.Minute).Unix()})
	if _, err := runFreeRecruitsFake(t, in, now.Add(-time.Minute).UnixMilli(), rewardErr("120289")); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	fake, err := runFreeRecruitsFake(t, in, now.Add(-time.Minute).UnixMilli(), rewardErr("999999"))
	if err == nil {
		t.Error("run = nil, want the pull failures")
	}
	if n := len(fake.cmds()); n != 3 {
		t.Errorf("sent %d requests, want the hero pull, the worker info and the worker draw", n)
	}
}
