package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

type allianceTask struct {
	id     int32
	state  int32
	finish time.Time // zero: not finished
}

func allianceTaskList(tasks ...allianceTask) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()
	for _, task := range tasks {
		o := sfs.NewSFSObject()
		o.PutInt("taskId", task.id)
		o.PutInt("state", task.state)
		var f int64
		if !task.finish.IsZero() {
			f = task.finish.UnixMilli()
		}
		o.PutLong("finishTime", f)
		arr.AddSFSObject(o)
	}
	resp.PutSFSArray("taskList", arr)
	return resp
}

// allianceTasksInit is an alliance init with the alliance_task switch and, when seasonEnd is set,
// an open season ending then.
func allianceTasksInit(switchOn bool, seasonEnd time.Time) *Init {
	return allianceInit(func(raw *sfs.SFSObject) {
		dc := sfs.NewSFSObject()
		if switchOn {
			dc.PutUtfString("alliance_task", "1")
		}
		raw.PutSFSObject("dataConfig", dc)
		if !seasonEnd.IsZero() {
			s := sfs.NewSFSObject()
			s.PutBool("open", true)
			s.PutLong("seasonEndTime", seasonEnd.UnixMilli())
			raw.PutSFSObject("playerServerSeasonInfo", s)
		}
	})
}

func allianceTasksRun(t *testing.T, in *Init, normal, season *sfs.SFSObject, claim func(id int32) *sfs.SFSObject) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == allianceTasksListCmd {
			if b, _ := claimBool(p, "isSeason"); b {
				return season
			}
			return normal
		}
		return claim(p.GetInt("taskId"))
	})
	return fake, runAllianceTasks(conn, in)
}

func allianceTasksClaimed(fake *rewardFake) []int32 {
	var out []int32
	for _, r := range fake.requests() {
		if r.Cmd == allianceTasksClaimCmd {
			out = append(out, r.Params.GetInt("taskId"))
		}
	}
	return out
}

func TestAllianceTasksClaimsFinishedClaimable(t *testing.T) {
	done := time.Now().Add(-time.Hour)
	normal := allianceTaskList(
		allianceTask{1, 1, done},        // claim
		allianceTask{2, 2, done},        // already claimed
		allianceTask{3, 1, time.Time{}}, // not finished
		allianceTask{4, 0, done},        // finished, not eligible
	)
	season := allianceTaskList(allianceTask{50, 1, done})
	fake, err := allianceTasksRun(t, allianceTasksInit(true, time.Now().Add(24*time.Hour)), normal, season,
		func(int32) *sfs.SFSObject { return rewardOK() })
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := allianceTasksClaimed(fake); !slices.Equal(got, []int32{1, 50}) {
		t.Errorf("claimed %v, want [1 50]", got)
	}
	reqs := fake.requests()
	if b, _ := claimBool(reqs[0].Params, "isSeason"); b {
		t.Error("first list read must be the normal one (isSeason false)")
	}
	if v, _ := reqs[0].Params.Get("isSeason"); v.Type != sfs.SFSBool {
		t.Errorf("isSeason sent as type %d, want Bool", v.Type)
	}
}

func TestAllianceTasksGates(t *testing.T) {
	list := allianceTaskList(allianceTask{1, 1, time.Now()})
	ok := func(int32) *sfs.SFSObject { return rewardOK() }
	for name, in := range map[string]*Init{
		"no init":                     {},
		"no alliance":                 rewardInitWithDay(20, func(*sfs.SFSObject) {}),
		"switch off, no season":       allianceTasksInit(false, time.Time{}),
		"switch off, season ended":    allianceTasksInit(false, time.Now().Add(-time.Hour)),
		"switch off, season end in s": allianceTasksInit(false, time.Unix(time.Now().Add(time.Hour).Unix()/1000, 0)),
	} {
		t.Run(name, func(t *testing.T) {
			fake, err := allianceTasksRun(t, in, list, list, ok)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := fake.cmds(); len(got) != 0 {
				t.Errorf("sent %v, want nothing", got)
			}
		})
	}
}

func TestAllianceTasksBenignFailureAndDeadConnection(t *testing.T) {
	done := time.Now().Add(-time.Hour)
	list := allianceTaskList(allianceTask{1, 1, done}, allianceTask{2, 1, done}, allianceTask{3, 1, done}, allianceTask{4, 1, done})
	fake, err := allianceTasksRun(t, allianceTasksInit(true, time.Time{}), list, nil, func(id int32) *sfs.SFSObject {
		switch id {
		case 1:
			return rewardErr("120289")
		case 2:
			return rewardErr("999999")
		case 3:
			return nil
		}
		return rewardOK()
	})
	if err == nil {
		t.Fatal("run = nil, want the failure and the dead connection")
	}
	if got := allianceTasksClaimed(fake); !slices.Equal(got, []int32{1, 2, 3}) {
		t.Errorf("claimed %v, want [1 2 3] (stop at the dead connection)", got)
	}
}
