package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// torchTask is one taskArr entry as InitServerData reads it: id (any numeric shape) and state.
func torchTask(id any, state int32) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	switch v := id.(type) {
	case string:
		o.PutUtfString("id", v)
	case int:
		o.PutInt("id", int32(v))
	}
	o.PutInt("num", 1)
	o.PutInt("state", state)
	return o
}

// torchInfo is a type-309 hero.event.info.get reply: taskArr, totalScore and rewardProcess at the
// top level (ActivityTorchRelayTaskManager.lua:24-29).
func torchInfo(score int64, claimed []int32, tasks ...*sfs.SFSObject) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("activityType", actTypeTorchRelay)
	r.PutLong("totalScore", score)
	rp := sfs.NewSFSArray()
	for _, i := range claimed {
		rp.AddInt(i)
	}
	r.PutSFSArray("rewardProcess", rp)
	arr := sfs.NewSFSArray()
	for _, t := range tasks {
		arr.AddSFSObject(t)
	}
	r.PutSFSArray("taskArr", arr)
	return r
}

func TestTorchRelayIsOptIn(t *testing.T) {
	f, ok := featureRegistry[torchRelayFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("torch-relay = %+v, want registered, off by default, static-only summary", f)
	}
}

func TestTorchRelayClaimsListedTasksThenMilestones(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			return torchInfo(7, []int32{0},
				torchTask("7025100", 1), // daily, claimable, id as a string
				torchTask(7025205, 1),   // game task, claimable
				torchTask(7025206, 2),   // already received
				torchTask(7025201, 0),   // not done
				torchTask(7099999, 1),   // not in row 2's lists: the client never shows it
			)
		case torchRelayTaskCmd:
			r := sfs.NewSFSObject()
			r.PutInt("activityId", p.GetInt("activityId"))
			r.PutUtfString("id", p.GetString("taskId"))
			if p.GetString("taskId") == "7025205" {
				r.PutLong("totalScore", 9) // the game task's score lifts the progress past 8
			}
			return r
		case torchRelayMilesCmd:
			r := sfs.NewSFSObject()
			rp := sfs.NewSFSArray()
			rp.AddInt(0)
			rp.AddInt(p.GetInt("index"))
			r.PutSFSArray("rewardProcess", rp)
			return r
		}
		return evOK()
	})
	if err := runTorchRelay(conn, evInit(30, evActivity(99113))); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := "hero.event.info.get,torch.relay.task.reward,torch.relay.task.reward,torch.relay.process.reward"
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if p := fake.only("hero.event.info.get")[0].Params; p.GetString("activityId") != "99113" {
		t.Errorf("info request = %v, want {activityId: \"99113\"}", p)
	}
	tasks := fake.only(torchRelayTaskCmd)
	for i, id := range []string{"7025100", "7025205"} {
		p := tasks[i].Params
		v, _ := p.Get("taskId")
		a, _ := p.Get("activityId")
		if p.GetString("taskId") != id || v.Type != sfs.SFSUtfString || a.Type != sfs.SFSInt || p.GetInt("activityId") != 99113 {
			t.Errorf("task claim %d = %v, want {activityId: Int 99113, taskId: UtfString %s}", i, p, id)
		}
	}
	m := fake.only(torchRelayMilesCmd)[0].Params
	if v, _ := m.Get("index"); v.Type != sfs.SFSInt || m.GetInt("index") != 1 || m.GetInt("activityId") != 99113 {
		t.Errorf("milestone claim = %v, want {activityId: 99113, index: Int 1} (0 claimed, 12 not reached)", m)
	}
}

func TestTorchRelayErrorStopsThatCommand(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			return torchInfo(12, nil, torchTask(7024100, 1), torchTask(7024101, 1))
		case torchRelayTaskCmd:
			return evErr("E77")
		case torchRelayMilesCmd:
			if p.GetInt("index") == 0 {
				return evErr("E78")
			}
		}
		return evOK()
	})
	err := runTorchRelay(conn, evInit(30, evActivity(99086)))
	if err == nil || !strings.Contains(err.Error(), "E77") || !strings.Contains(err.Error(), "E78") {
		t.Fatalf("err = %v, want the E77 task and E78 milestone failures", err)
	}
	got := strings.Join(fake.cmds(), ",")
	if want := "hero.event.info.get,torch.relay.task.reward,torch.relay.process.reward"; got != want {
		t.Errorf("sent %s\nwant %s (one failure stops that command; milestones still use the info score)", got, want)
	}
}

func TestTorchRelaySkipsUnknownRowAndOtherType(t *testing.T) {
	withEvNow(t, evTestNow)
	saved := torchRelayActivityRows
	torchRelayActivityRows = map[int32]int32{99101: 1} // 99086 has no embedded row
	t.Cleanup(func() { torchRelayActivityRows = saved })
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := torchInfo(20, nil, torchTask(7024100, 1))
		r.PutInt("activityType", actTypeActTask)
		return r
	})
	if err := runTorchRelay(conn, evInit(30, evActivity(99086), evActivity(99101))); err != nil {
		t.Fatal(err)
	}
	got := fake.requests()
	if len(got) != 1 || got[0].Cmd != "hero.event.info.get" || got[0].Params.GetString("activityId") != "99101" {
		t.Errorf("sent %v, want only 99101's info (99086 has no row, and the reply is not a torch relay)", fake.cmds())
	}
}
