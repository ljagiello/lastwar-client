package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

type secretTask struct {
	uuid     int64
	done     time.Time // zero: not started
	rewarded int32
}

func secretTasksList(tasks ...secretTask) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()
	for _, task := range tasks {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", task.uuid)
		var done int64
		if !task.done.IsZero() {
			done = task.done.UnixMilli()
		}
		o.PutLong("completionTime", done)
		o.PutInt("rewarded", task.rewarded)
		arr.AddSFSObject(o)
	}
	resp.PutSFSArray("ls", arr)
	return resp
}

// secretTasksInit has an HQ of the given level and, when withActivity, DispatchTask activity
// 94101 in init activity (a map keyed by id).
func secretTasksInit(hq int32, withActivity bool) *Init {
	return rewardInitWithDay(hq, func(raw *sfs.SFSObject) {
		acts := sfs.NewSFSObject()
		other := sfs.NewSFSObject()
		other.PutLong("startTime", 1)
		acts.PutSFSObject("28", other)
		if withActivity {
			acts.PutSFSObject("94101", sfs.NewSFSObject())
		}
		raw.PutSFSObject("activity", acts)
	})
}

func secretTasksRun(t *testing.T, in *Init, list *sfs.SFSObject, claim *sfs.SFSObject) *rewardFake {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == secretTasksListCmd {
			return list
		}
		return claim
	})
	if err := runSecretTasks(conn, in); err != nil && claim.GetString("errorCode") != "999999" {
		t.Fatalf("run: %v", err)
	}
	return fake
}

func TestSecretTasksBatchClaimsFinishedUnrewarded(t *testing.T) {
	now := time.Now()
	list := secretTasksList(
		secretTask{1, now.Add(-time.Minute), 0},
		secretTask{2, now.Add(time.Hour), 0},       // still running
		secretTask{3, now.Add(-time.Hour), 1},      // already rewarded
		secretTask{4, time.Time{}, 0},              // not started
		secretTask{5, now.Add(-24 * time.Hour), 0}, // ms: read as seconds it would be in the future
	)
	batch := sfs.NewSFSObject()
	items := sfs.NewSFSArray()
	items.AddSFSObject(rewardOK())
	batch.PutSFSArray("array", items)
	fake := secretTasksRun(t, secretTasksInit(10, true), list, batch)

	reqs := fake.requests()
	if got := fake.cmds(); !slices.Equal(got, []string{secretTasksListCmd, secretTasksBatchCmd}) {
		t.Fatalf("sent %v", got)
	}
	v, _ := reqs[1].Params.Get("uuidList")
	if v.Type != sfsLongArrayTag {
		t.Errorf("uuidList type %d, want LongArray", v.Type)
	}
	if got, _ := v.Val.([]int64); !slices.Equal(got, []int64{1, 5}) {
		t.Errorf("uuidList = %v, want [1 5]", v.Val)
	}
}

func TestSecretTasksSingleUsesSingleClaim(t *testing.T) {
	fake := secretTasksRun(t, secretTasksInit(10, true), secretTasksList(secretTask{7, time.Now().Add(-time.Minute), 0}), rewardOK())
	reqs := fake.requests()
	if len(reqs) != 2 || reqs[1].Cmd != secretTasksOneCmd || reqs[1].Params.GetLong("uuid") != 7 {
		t.Errorf("sent %v, want the list then hero.dispatch.reward {uuid 7}", fake.cmds())
	}
}

func TestSecretTasksGatesNeverStartAnything(t *testing.T) {
	list := secretTasksList(secretTask{1, time.Now().Add(time.Hour), 0})
	for name, c := range map[string]struct {
		in   *Init
		want []string
	}{
		"no init":          {&Init{}, nil},
		"HQ below 5":       {secretTasksInit(4, true), nil},
		"activity missing": {secretTasksInit(10, false), nil},
		"nothing finished": {secretTasksInit(10, true), []string{secretTasksListCmd}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := secretTasksRun(t, c.in, list, rewardOK())
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

func TestSecretTasksBenignAndFailure(t *testing.T) {
	list := secretTasksList(secretTask{1, time.Now().Add(-time.Minute), 0}, secretTask{2, time.Now().Add(-time.Minute), 0})
	conn, _ := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == secretTasksListCmd {
			return list
		}
		return rewardErr("120289")
	})
	if err := runSecretTasks(conn, secretTasksInit(10, true)); err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	conn, _ = startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == secretTasksListCmd {
			return list
		}
		return rewardErr("999999")
	})
	if err := runSecretTasks(conn, secretTasksInit(10, true)); err == nil {
		t.Error("run = nil, want the claim failure")
	}
}
