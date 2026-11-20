package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// zoneTaskInfo is a get.zone.mobilization.task.info {action: 1} reply with the client's field
// names (LWZoneMobilizationDailyTaskInfo.lua:18-26): 9001 and 9003 claimable (one id an Int, one a
// string), 9002 received, 9004 not done, an entry without a taskId.
func zoneTaskInfo() *sfs.SFSObject {
	l := sfs.NewSFSArray()
	l.AddSFSObject(obj("taskId", 9001, "num", 5, "state", 1))
	l.AddSFSObject(obj("taskId", 9002, "num", 5, "state", 2))
	l.AddSFSObject(obj("taskId", "9003", "num", 1, "state", 1))
	l.AddSFSObject(obj("taskId", 9004, "num", 0, "state", 0))
	l.AddSFSObject(obj("num", 1, "state", 1))
	r := sfs.NewSFSObject()
	r.PutSFSArray("taskArr", l)
	return r
}

func startZoneTaskFake(t *testing.T, running bool, claim func(p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case zoneMobInfoCmd:
			return zoneInfo(running)
		case zoneMobTaskInfoCmd:
			return zoneTaskInfo()
		case questRewardCmd:
			return claim(p)
		}
		return evErr("unexpected command")
	})
}

func TestZoneMobilizationTasksClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	logs := captureFwLogs(t)
	conn, fake := startZoneTaskFake(t, true, func(*sfs.SFSObject) *sfs.SFSObject { return evOK() })
	in := zoneInit(10, true)
	in.Raw.PutSFSArray("task", func() *sfs.SFSArray {
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj("id", "9001", "state", 1))
		return a
	}())
	if err := runZoneMobilizationTasks(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{zoneMobInfoCmd, zoneMobTaskInfoCmd, questRewardCmd, questRewardCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if v, _ := fake.only(zoneMobTaskInfoCmd)[0].Params.Get("action"); v.Type != sfs.SFSInt || v.Val != int32(1) {
		t.Errorf("task info action = %#v, want Int 1", v)
	}
	var ids []string
	for _, c := range fake.only(questRewardCmd) {
		v, _ := c.Params.Get("id")
		if v.Type != sfs.SFSUtfString {
			t.Errorf("id type %v, want UtfString", v.Type)
		}
		ids = append(ids, c.Params.GetString("id"))
	}
	if !slices.Equal(ids, []string{"9001", "9003"}) {
		t.Errorf("claimed %v", ids)
	}
	if out := logs.String(); !strings.Contains(out, "taskId=9001 state=1 num=5 inInitTasks=true") ||
		!strings.Contains(out, "taskId=9003 state=1 num=1 inInitTasks=false") {
		t.Errorf("log lacks the task lines:\n%s", out)
	}
}

func TestZoneMobilizationTasksGates(t *testing.T) {
	withEvNow(t, evTestNow)
	ok := func(*sfs.SFSObject) *sfs.SFSObject { return evOK() }
	conn, fake := startZoneTaskFake(t, false, ok)
	if err := runZoneMobilizationTasks(conn, zoneInit(10, true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{zoneMobInfoCmd}) {
		t.Errorf("sent %v while the event is not running", got)
	}
	conn, fake = startZoneTaskFake(t, true, ok)
	for _, in := range []*Init{nil, {}, zoneInit(5, true)} {
		if err := runZoneMobilizationTasks(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v below HQ 6 or without init", fake.cmds())
	}
	conn, fake = startZoneTaskFake(t, true, func(*sfs.SFSObject) *sfs.SFSObject { return evErr("E_TASK") })
	if err := runZoneMobilizationTasks(conn, zoneInit(10, true)); err == nil || !strings.Contains(err.Error(), "E_TASK") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.only(questRewardCmd)); n != 1 {
		t.Errorf("sent %d claims; the first refusal ends the run", n)
	}
	f, registered := featureRegistry[zoneMobTasksFeature]
	if !registered || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("registered %v, DefaultOn %v, summary %q", registered, f.DefaultOn, f.Summary)
	}
}

// TestZoneMobilizationTasksTaskIDZeroAndCap: a state-1 row whose taskId is 0 is no task (the row is
// the quest template of its taskId, LWUIZoneMobilizationDailyTaskItemRender.lua:71) and is not
// claimed; 31 claimable tasks get 30 claims, the feature's own cap of 30 a run.
func TestZoneMobilizationTasksTaskIDZeroAndCap(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, c := range map[string]struct {
		tasks []*sfs.SFSObject
		want  int
	}{
		"taskId 0": {[]*sfs.SFSObject{obj("taskId", 0, "num", 5, "state", 1), obj("taskId", "0", "num", 5, "state", 1)}, 0},
		"31 claimable": {func() []*sfs.SFSObject {
			var l []*sfs.SFSObject
			for i := range 31 {
				l = append(l, obj("taskId", 9100+i, "num", 1, "state", 1))
			}
			return l
		}(), 30},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case "get.zone.mobilization.info":
					return zoneInfo(true)
				case "get.zone.mobilization.task.info":
					return obj("taskArr", c.tasks)
				}
				return evOK()
			})
			if err := runZoneMobilizationTasks(conn, zoneInit(10, true)); err != nil {
				t.Fatal(err)
			}
			if n := len(fake.only("task.reward.get")); n != c.want {
				t.Errorf("sent %d claims, want %d", n, c.want)
			}
		})
	}
}
