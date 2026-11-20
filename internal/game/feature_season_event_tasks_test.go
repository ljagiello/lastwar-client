package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// bloodyNightInit is an S4 (Darkness, config 85) init with the Bloody Night activity 1200009 open
// and source server 812.
func bloodyNightInit() *Init {
	in := evWithSeason(evInit(30, evActivity(1200009)), 4)
	in.Raw.PutSFSObject("user", obj("serverId", 812))
	return in
}

// bloodyNightReplies answers the act view for server with plan 1002 of group 4 and the task list
// with personal task 31 and alliance task 32 claimable (state 1) beside done and claimed ones.
func bloodyNightReplies(server int32, plan int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case bloodyNightActCmd:
			return obj("serverId", server, "bloodNightPlanActDataInfo",
				obj("planId", plan, "bloodNightPlanInfo", obj("groupId", 4)))
		case bloodyNightTaskCmd:
			return obj("userBloodNightTaskArr", []*sfs.SFSObject{
				obj("taskId", 30, "num", 1, "state", 0, "dayTime", 0), obj("taskId", 31, "num", 5, "state", 1, "dayTime", int64(1)),
			}, "allianceBloodNightTaskArr", []*sfs.SFSObject{
				obj("taskId", 32, "num", 9, "state", 1, "dayTime", 0), obj("taskId", 33, "num", 9, "state", 2, "dayTime", 0),
			})
		}
		return obj("reward", []*sfs.SFSObject{})
	}
}

func TestBloodyNightTasks(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, bloodyNightReplies(812, 1002))
	if err := runBloodyNightTasks(conn, bloodyNightInit()); err != nil {
		t.Fatal(err)
	}
	want := "view.blood.night.act,view.blood.night.task,blood.night.task.get.reward,blood.night.task.get.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	seasonParamIs(t, fake.only(bloodyNightActCmd)[0].Params, "serverId", sfs.SFSInt, int32(812))
	seasonParamIs(t, fake.only(bloodyNightTaskCmd)[0].Params, "planId", sfs.SFSInt, int32(1002))
	var tasks []int32
	for _, r := range fake.only(bloodyNightRewardCmd) {
		seasonParamIs(t, r.Params, "planId", sfs.SFSInt, int32(1002))
		seasonParamIs(t, r.Params, "taskId", sfs.SFSInt, r.Params.GetInt("taskId"))
		tasks = append(tasks, r.Params.GetInt("taskId"))
	}
	if !slices.Equal(tasks, []int32{31, 32}) {
		t.Errorf("claimed %v, want [31 32]", tasks)
	}
}

func TestBloodyNightTasksSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	s6 := evWithSeason(evInit(30, evActivity(1200009)), 6)
	s6.Raw.PutSFSObject("user", obj("serverId", 812))
	noSource := evWithSeason(evInit(30, evActivity(1200009)), 4)
	for name, c := range map[string]struct {
		in    *Init
		reply func(string, *sfs.SFSObject) *sfs.SFSObject
		want  string
	}{
		"not a Darkness season": {s6, bloodyNightReplies(812, 1002), ""},
		"no Bloody Night activity": {func() *Init {
			in := evWithSeason(evInit(30), 4)
			in.Raw.PutSFSObject("user", obj("serverId", 812))
			return in
		}(), bloodyNightReplies(812, 1002), ""},
		"source server unknown": {noSource, bloodyNightReplies(812, 1002), ""},
		// The feature runs only in season (evSeasonID): the Darkness config of an ended season is not
		// enough.
		"Darkness season ended": {func() *Init {
			in := bloodyNightInit()
			for _, k := range []string{"playerServerSeasonInfo", "curServerSeasonInfo"} {
				in.Object(k).PutLong("seasonEndTime", evTestNow.Add(-time.Hour).UnixMilli())
			}
			return in
		}(), bloodyNightReplies(812, 1002), ""},
		"reply for another server": {bloodyNightInit(), bloodyNightReplies(813, 1002), bloodyNightActCmd},
		"plan not in the table":    {bloodyNightInit(), bloodyNightReplies(812, 1099), bloodyNightActCmd},
		// The stage is the season_blood_night_plan row with id planId among the rows of flag groupId
		// (BloodyNightData.lua:187-194); plan 1002 has flag 4 (tables 40413), so group 5 finds none.
		"group is not the plan's flag": {bloodyNightInit(), func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "view.blood.night.act" {
				return dBloodyNightAct(812, 1002, 5)
			}
			return bloodyNightReplies(812, 1002)(cmd, p)
		}, "view.blood.night.act"},
		// An unknown plan without a groupId matches no row either.
		"plan not in the table, no groupId": {bloodyNightInit(), func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "view.blood.night.act" {
				return dBloodyNightAct(812, 1099, -1)
			}
			return bloodyNightReplies(812, 1002)(cmd, p)
		}, "view.blood.night.act"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			if err := runBloodyNightTasks(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// TestBloodyNightTaskClaims: a state-1 entry without a taskId is no claim (the button sends the
// row's own data.taskId, BloodyNightTaskItem.lua:173-179), and the first refused claim stops the rest.
func TestBloodyNightTaskClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	tasks := obj("userBloodNightTaskArr", []*sfs.SFSObject{obj("num", 5, "state", 1), obj("taskId", 31, "num", 5, "state", 1)},
		"allianceBloodNightTaskArr", []*sfs.SFSObject{obj("taskId", 32, "num", 9, "state", 1)})
	for name, c := range map[string]struct {
		claim   *sfs.SFSObject
		want    []int32
		wantErr bool
	}{
		"no taskId, no claim":     {obj("reward", []*sfs.SFSObject{}), []int32{31, 32}, false},
		"the first refusal stops": {evErr("E5"), []int32{31}, true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case "view.blood.night.act":
					return dBloodyNightAct(812, 1002, 4)
				case "view.blood.night.task":
					return tasks
				}
				return c.claim
			})
			if err := runBloodyNightTasks(conn, bloodyNightInit()); (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			var got []int32
			for _, r := range fake.only("blood.night.task.get.reward") {
				got = append(got, r.Params.GetInt("taskId"))
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("claimed %v, want %v", got, c.want)
			}
		})
	}
}

// jungleTrialReplies answers the act view with actId 77, rewardNum earned and receiveNum received,
// each box with receiveNum + step, and the task list with task 5 claimable.
func jungleTrialReplies(earned, received, step int64) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case jungleTrialActCmd:
			return obj("actId", 77, "userSeasonMonsterActFullInfo", obj(
				"userSeasonMonsterEventActInfo", obj("receiveNum", received),
				"userSeasonMonsterEventRewardRecordInfo", obj("rewardNum", earned)))
		case jungleTrialBoxCmd:
			received += step
			return obj("reward", []*sfs.SFSObject{}, "userSeasonMonsterEventActInfo", obj("receiveNum", received))
		case jungleTrialTaskCmd:
			return obj("seasonMonsterEventTaskArr", []*sfs.SFSObject{
				obj("taskId", 4, "num", 1, "state", 2), obj("taskId", 5, "num", 3, "state", 1), obj("taskId", 6, "state", 0),
			})
		}
		return obj("reward", []*sfs.SFSObject{})
	}
}

func TestJungleTrialRewards(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, c := range map[string]struct {
		earned, received, step int64
		boxes                  int
	}{
		"two boxes earned":         {5, 3, 1, 2},
		"none earned":              {3, 3, 1, 0},
		"receiveNum does not rise": {5, 3, 0, 1},
		"bounded per run":          {500, 0, 1, 100}, // the feature's own cap of 100 boxes a run
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, jungleTrialReplies(c.earned, c.received, c.step))
			if err := runJungleTrialRewards(conn, evInit(30, evActivity(1200106))); err != nil {
				t.Fatal(err)
			}
			if n := len(fake.only(jungleTrialBoxCmd)); n != c.boxes {
				t.Errorf("opened %d boxes, want %d", n, c.boxes)
			}
			for _, r := range fake.requests()[1:] {
				seasonParamIs(t, r.Params, "actId", sfs.SFSInt, int32(77))
			}
			claims := fake.only(jungleTrialRewardCmd)
			if len(claims) != 1 {
				t.Fatalf("claimed %d tasks, want task 5 only", len(claims))
			}
			seasonParamIs(t, claims[0].Params, "taskId", sfs.SFSInt, int32(5))
			if cmds := fake.cmds(); cmds[0] != jungleTrialActCmd || cmds[len(cmds)-2] != jungleTrialTaskCmd {
				t.Errorf("order %v: act view, boxes, task view, claims", cmds)
			}
		})
	}
}

func TestJungleTrialRewardsSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, jungleTrialReplies(5, 3, 1))
	if err := runJungleTrialRewards(conn, evInit(30)); err != nil || len(fake.requests()) != 0 {
		t.Errorf("no Jungle Trial activity: err %v, sent %v", err, fake.cmds())
	}
	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return obj("x", 1) })
	if err := runJungleTrialRewards(conn2, evInit(30, evActivity(1200106))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake2.cmds(), ","); got != jungleTrialActCmd {
		t.Errorf("reply without actId sent %s", got)
	}
	conn3, fake3 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == jungleTrialBoxCmd {
			return evErr("E2")
		}
		return jungleTrialReplies(5, 3, 1)(cmd, p)
	})
	if err := runJungleTrialRewards(conn3, evInit(30, evActivity(1200106))); err == nil || !strings.Contains(err.Error(), "E2") {
		t.Errorf("err = %v, want E2", err)
	}
	if got := strings.Join(fake3.cmds(), ","); got != "season.monster.event.act.view,season.monster.event.open.box" {
		t.Errorf("a refused box sent %s", got)
	}
}

// TestJungleTrialBoxesAndTaskRefusal: GetCanOpenBoxNum is 0 without receiveNum or rewardNum
// (JungleTrialDataManager.lua:243-248), so no box is opened; and the first refused task claim stops
// the rest.
func TestJungleTrialBoxesAndTaskRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	tasks := obj("seasonMonsterEventTaskArr", []*sfs.SFSObject{obj("taskId", 5, "num", 3, "state", 1), obj("taskId", 7, "num", 1, "state", 1)})
	for name, c := range map[string]struct {
		full    *sfs.SFSObject
		claim   *sfs.SFSObject
		want    string
		wantErr bool
	}{
		"rewardNum without receiveNum": {obj("userSeasonMonsterEventActInfo", obj("startTime", int64(1)),
			"userSeasonMonsterEventRewardRecordInfo", obj("rewardNum", 5)), obj("reward", []*sfs.SFSObject{}),
			"season.monster.event.act.view,season.monster.event.task.view,season.monster.event.task.get.reward,season.monster.event.task.get.reward", false},
		"no userSeasonMonsterEventActInfo": {obj("userSeasonMonsterEventRewardRecordInfo", obj("rewardNum", 5)), obj("reward", []*sfs.SFSObject{}),
			"season.monster.event.act.view,season.monster.event.task.view,season.monster.event.task.get.reward,season.monster.event.task.get.reward", false},
		"the first refused task stops": {obj("userSeasonMonsterEventActInfo", obj("receiveNum", 5),
			"userSeasonMonsterEventRewardRecordInfo", obj("rewardNum", 5)), evErr("E6"),
			"season.monster.event.act.view,season.monster.event.task.view,season.monster.event.task.get.reward", true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case "season.monster.event.act.view":
					return obj("actId", 77, "userSeasonMonsterActFullInfo", c.full)
				case "season.monster.event.task.view":
					return tasks
				case "season.monster.event.open.box":
					return obj("reward", []*sfs.SFSObject{}, "userSeasonMonsterEventActInfo", obj("receiveNum", 9))
				}
				return c.claim
			})
			if err := runJungleTrialRewards(conn, evInit(30, evActivity(1200106))); (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %s\nwant %s", got, c.want)
			}
		})
	}
}

func TestSeasonEventTaskFeaturesRegistered(t *testing.T) {
	for _, name := range []string{bloodyNightFeature, jungleTrialFeature} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s: registered %v, %+v", name, ok, f)
		}
	}
}
