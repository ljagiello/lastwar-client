package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// offSeasonInfoReply is a city.battle.activity.gain.task.info reply as
// OffSeason1TaskDataManager:ParseTaskServerData reads it: group, score, taskRequests[{configId,
// hasReward}] (configId -> hasReward).
func offSeasonInfoReply(group int32, rows map[int32]int32) *sfs.SFSObject {
	r := evOK()
	r.PutInt("group", group)
	r.PutInt("score", 1200)
	a := sfs.NewSFSArray()
	ids := make([]int32, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		t := sfs.NewSFSObject()
		t.PutInt("configId", id)
		t.PutInt("hasReward", rows[id])
		t.PutInt("curNum", 1)
		t.PutInt("totalNum", 1)
		a.AddSFSObject(t)
	}
	r.PutSFSArray("taskRequests", a)
	return r
}

func TestOffSeasonTasksClaimsTasksThenTheChestFromAFreshInfo(t *testing.T) {
	withEvNow(t, evTestNow)
	infos := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case offSeasonTaskInfoCmd:
			infos++
			if infos == 1 {
				// a task is claimable; the first chest is not reached yet
				return offSeasonInfoReply(1, map[int32]int32{1001: 1, 1002: 0, 10001: 0, 10002: 0})
			}
			// the task claim raised the score: the chest is claimable now
			return offSeasonInfoReply(1, map[int32]int32{1001: 2, 1002: 0, 10001: 1, 10002: 0})
		}
		return evOK()
	})
	err := runOffSeasonTasks(conn, evInit(30, evActivity(1000109))) // Recapture, type 347
	if err != nil {
		t.Fatal(err)
	}
	want := []string{offSeasonTaskInfoCmd, offSeasonTaskRewardCmd, offSeasonTaskInfoCmd, offSeasonTaskRewardCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	reqs := fake.requests()
	for _, r := range reqs {
		if v, _ := r.Params.Get("group"); v.Type != sfs.SFSInt || r.Params.GetInt("group") != 1 {
			t.Errorf("%s group = %#v, want Int 1", r.Cmd, v)
		}
	}
	if got := reqs[1].Params.GetInt("configId"); got != offSeasonClaimAllTasks {
		t.Errorf("first claim configId = %d, want -1 (all tasks)", got)
	}
	if got := reqs[3].Params.GetInt("configId"); got != offSeasonClaimChest {
		t.Errorf("second claim configId = %d, want -2 (score chest)", got)
	}
	if v, _ := reqs[3].Params.Get("configId"); v.Type != sfs.SFSInt {
		t.Errorf("configId type = %d, want Int", v.Type)
	}
}

func TestOffSeasonTasksQueenOfBloodChestOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == offSeasonTaskInfoCmd {
			// 10001 is a Type -1 (group 1) goal row: in group 2 the client files it as a task, but
			// it is claimed already; the group-2 chest 11002 is due.
			return offSeasonInfoReply(2, map[int32]int32{2001: 2, 10001: 2, 11001: 2, 11002: 1})
		}
		return evOK()
	})
	if err := runOffSeasonTasks(conn, evInit(30, evActivity(1000110))); err != nil { // Queen of Blood, type 348
		t.Fatal(err)
	}
	claims := fake.only(offSeasonTaskRewardCmd)
	if len(claims) != 1 || claims[0].Params.GetInt("group") != 2 || claims[0].Params.GetInt("configId") != offSeasonClaimChest {
		t.Fatalf("claims = %+v, want one {group 2, configId -2}", claims)
	}
	if n := len(fake.only(offSeasonTaskInfoCmd)); n != 1 {
		t.Errorf("info sent %d times, want once (no task claim, so no re-read)", n)
	}
}

func TestOffSeasonTasksGates(t *testing.T) {
	withEvNow(t, evTestNow)
	ended := evActivity(1000109)
	ended.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	for _, c := range []struct {
		name string
		in   *Init
		rows map[int32]int32
	}{
		{"no off-season activity", evInit(30, evActivity(99152)), map[int32]int32{1001: 1}},
		{"activity ended", evInit(30, ended), map[int32]int32{1001: 1}},
		{"nothing claimable", evInit(30, evActivity(1000109)), map[int32]int32{1001: 0, 1002: 2, 10001: 0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == offSeasonTaskInfoCmd {
					return offSeasonInfoReply(1, c.rows)
				}
				return evOK()
			})
			if err := runOffSeasonTasks(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := fake.only(offSeasonTaskRewardCmd); len(got) != 0 {
				t.Errorf("claimed %+v, want nothing", got)
			}
		})
	}
}

func TestOffSeasonTasksErrorStopsTheGroup(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == offSeasonTaskInfoCmd {
			return offSeasonInfoReply(1, map[int32]int32{1001: 1, 10001: 1})
		}
		return evErr(evAlreadyExecuted)
	})
	err := runOffSeasonTasks(conn, evInit(30, evActivity(1000109)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the refusal reported (no benign codes)", err)
	}
	if got := fake.only(offSeasonTaskRewardCmd); len(got) != 1 {
		t.Errorf("claims = %d, want the task claim only (its refusal stops the chest)", len(got))
	}
}
