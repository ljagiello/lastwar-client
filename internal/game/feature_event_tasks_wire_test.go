package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// cWireParam is one expected request param: its key, SFS type tag and value, as the 1.0.370
// message class puts it.
type cWireParam struct {
	key string
	typ byte
	val any
}

// cWireReq is one expected request: the command, written as a literal, and its params in the
// message class's put order.
type cWireReq struct {
	cmd    string
	params []cWireParam
}

// cWireCheck asserts that the fake saw exactly want: each command literal, the param keys in order
// (the transport's _id aside), and each param's type tag and value. GetInt reads a Long too, so the
// type is checked by its tag.
func cWireCheck(t *testing.T, got []evReq, want []cWireReq) {
	t.Helper()
	var gotCmds, wantCmds []string
	for _, r := range got {
		gotCmds = append(gotCmds, r.Cmd)
	}
	for _, w := range want {
		wantCmds = append(wantCmds, w.cmd)
	}
	if !slices.Equal(gotCmds, wantCmds) {
		t.Fatalf("sent %v, want %v", gotCmds, wantCmds)
	}
	for i, w := range want {
		p := got[i].Params
		var keys []string
		for _, kv := range w.params {
			keys = append(keys, kv.key)
		}
		if gotKeys := session.ParamKeysWithoutID(p); !slices.Equal(gotKeys, keys) {
			t.Errorf("%s #%d keys %v, want %v", w.cmd, i, gotKeys, keys)
		}
		for _, kv := range w.params {
			v, ok := p.Get(kv.key)
			if !ok || v.Type != kv.typ || v.Val != kv.val {
				t.Errorf("%s #%d %s = %#v, want type %d value %#v", w.cmd, i, kv.key, v, kv.typ, kv.val)
			}
		}
	}
}

// cWireCase is one feature run on a fixture: the Init, the fake server's replies by command
// literal, and every request the run must send.
type cWireCase struct {
	name  string
	setup func(t *testing.T) *Init
	reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject
	run   func(*session.GameConn, *Init) error
	want  []cWireReq
}

func cWireRun(t *testing.T, cases []cWireCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withEvNow(t, evTestNow)
			in := c.setup(t)
			conn, fake := startEvFake(t, c.reply)
			if err := c.run(conn, in); err != nil {
				t.Fatal(err)
			}
			cWireCheck(t, fake.requests(), c.want)
		})
	}
}

// cTask is a task entry {idKey: UtfString id, state: Int state}.
func cTask(idKey, id string, state int32) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutUtfString(idKey, id)
	e.PutInt("state", state)
	return e
}

// cArr is an SFSArray of the given objects, in order.
func cArr(objs ...*sfs.SFSObject) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, o := range objs {
		a.AddSFSObject(o)
	}
	return a
}

// TestEventTasksWire pins, as literals, every command, param key, SFS type and sent constant of the
// eight event-task features. Commands are MsgDefines.lua entries; each request's shape is its
// message class's OnCreate and the send site's arguments:
//   - activity.doomsday.quest.info {} (MsgDefines.lua:1765; ActivityDoomsdayQuestInfoMessage.lua:4-6,
//     sent by ActivityListDataManager.lua:490) and activity.doomsday.quest.reward {uuid: UtfString
//     "all"} (MsgDefines.lua:1768; ActivityDoomsdayQuestRewardMessage.lua:4-7; the claim-all,
//     UIDoomsdayAchievePage.lua:101).
//   - hero.event.info.get {activityId: UtfString} (MsgDefines.lua:528; ActivityEventInfoGetMessage.lua:
//     5-7; ActivityListDataManager.lua:379, 500, 582, 677 send tostring(data.activityId)).
//   - multiply.task.reward {activityId: Int, taskId: UtfString "0"} (MsgDefines.lua:1808;
//     MultipleParkourTaskRewardMessage.lua:5-9; UIActivityMultipleParkourRewardsView.lua:161).
//   - activity.plane.feature.reward {activityId: Int, taskId: UtfString "0"} (MsgDefines.lua:1812;
//     FrontBreakSundayRewardMessage.lua:5-9; UIActivityFrontBreakSundayRewardsView.lua:138 sends 0,
//     tostring'd) and activity.plane.feature.get.soldier.box.reward {activityId: Int, id: Int 0}
//     (MsgDefines.lua:1817; FrontBreakSundaySaveSoliderRewardMessage.lua:5-9; FrontBreakoutSunday.lua:445).
//   - power.up.task.reward {activityId: Int, taskId: UtfString} (MsgDefines.lua:1866;
//     PowerUpTaskRewardMessage.lua:4-8; LeadingQuestItemV2.lua:158).
//   - hero.experiment.reward {activityId: Int} (MsgDefines.lua:1529; HeroExperimentRewardMessage.lua:5-8;
//     UIHeroTrial.lua:449).
//   - music.game.activity.info {activityId: Int} (MsgDefines.lua:2685; MusicGameActivityInfoMessage.lua:
//     4-7; ActivityListDataManager.lua:767) and music.receive.task.reward {activityId: Int, taskId: Int
//     0} (MsgDefines.lua:2684; MusicReceiveTaskRewardMessage.lua:4-8; ActCrazyRockTaskManager.lua:125
//     from UIActCrazyRockTaskView.lua:139's RequestReceiveTaskReward(activityId, 0)).
//   - sandworm.task.info {} (MsgDefines.lua:1887; SandWormTaskInfoMessage.lua has no OnCreate;
//     SandWormHuntDataManager.lua:145) and sandworm.task.claim.reward {taskId: UtfString}
//     (MsgDefines.lua:1888; SandWormTaskClaimRewardMessage.lua:4-7; SandWormTaskItem.lua:177).
//   - get.theme.activity.info {activityId: Int} (MsgDefines.lua:525; GetThemeActivityInfoMessage.lua:
//     5-8; ThemeActivityManager.lua:225) and receive.theme.activity.reward {activityId: Int, day: Int}
//     (MsgDefines.lua:526; ClaimThemeActNoticeRewardMessage.lua:5-9; ThemeActivityManager.lua:230).
//
// Activity ids are rows of table activity 40413 (activity_tables_gen.go): 94210 type 157, 2000001
// type 301, 2400001 type 312, 60011 type 219, 5201 type 133, 99152 type 186, 1000069 type 240. The
// theme row is added with type 30 (ActivitySummary, EnumType.lua:6319), which 40413 lacks.
func TestEventTasksWire(t *testing.T) {
	plain := func(acts ...*sfs.SFSObject) func(*testing.T) *Init {
		return func(*testing.T) *Init { return evInit(30, acts...) }
	}
	info := func(id string) cWireReq {
		return cWireReq{"hero.event.info.get", []cWireParam{{"activityId", sfs.SFSUtfString, id}}}
	}
	const themeID int32 = 777032
	cWireRun(t, []cWireCase{
		{
			name:  "doomsday-quests",
			setup: plain(evActivity(94210)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				if cmd != "activity.doomsday.quest.info" {
					return evOK()
				}
				q := sfs.NewSFSObject()
				q.PutUtfString("has_rewards", "")
				q.PutSFSArray("doomsday_quests", cArr(obj("uuid", "u1", "quest_id", 101, "state", 1)))
				r := evOK()
				r.PutSFSObject("quest_info", q)
				return r
			},
			run: runDoomsdayQuests,
			want: []cWireReq{
				{"activity.doomsday.quest.info", nil},
				{"activity.doomsday.quest.reward", []cWireParam{{"uuid", sfs.SFSUtfString, "all"}}},
			},
		},
		{
			name:  "parkour-tasks",
			setup: plain(evActivity(2000001)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				if cmd != "hero.event.info.get" {
					return evOK()
				}
				r := evInfo()
				r.PutSFSArray("taskArr", cArr(cTask("taskId", "2", 1)))
				return r
			},
			run: runParkourTasks,
			want: []cWireReq{
				info("2000001"),
				{"multiply.task.reward", []cWireParam{
					{"activityId", sfs.SFSInt, int32(2000001)}, {"taskId", sfs.SFSUtfString, "0"}}},
			},
		},
		{
			name:  "front-break-tasks",
			setup: plain(evActivity(2400001)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				if cmd != "hero.event.info.get" {
					return evOK()
				}
				r := evInfo()
				r.PutSFSArray("taskArr", cArr(cTask("taskId", "5", 1)))
				r.PutSFSArray("soldierBox", cArr(obj("id", 1, "state", 1, "target", 100))) // CanGet = 1, EnumType.lua:18304
				return r
			},
			run: runFrontBreakTasks,
			want: []cWireReq{
				info("2400001"),
				{"activity.plane.feature.reward", []cWireParam{
					{"activityId", sfs.SFSInt, int32(2400001)}, {"taskId", sfs.SFSUtfString, "0"}}},
				{"activity.plane.feature.get.soldier.box.reward", []cWireParam{
					{"activityId", sfs.SFSInt, int32(2400001)}, {"id", sfs.SFSInt, int32(0)}}},
			},
		},
		{
			name:  "leading-quest-tasks",
			setup: plain(evActivity(60011)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				if cmd != "hero.event.info.get" {
					return evOK()
				}
				r := evInfo()
				r.PutSFSArray("taskArr", cArr(cTask("taskId", "11", 1)))
				return r
			},
			run: runLeadingQuestTasks,
			want: []cWireReq{
				info("60011"),
				{"power.up.task.reward", []cWireParam{
					{"activityId", sfs.SFSInt, int32(60011)}, {"taskId", sfs.SFSUtfString, "11"}}},
			},
		},
		{
			name:  "hero-trial-stage",
			setup: plain(evActivity(5201)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				if cmd != "hero.event.info.get" {
					return evOK()
				}
				r := evInfo()
				r.PutInt("cur_stage", 2)
				r.PutSFSArray("stageArr", cArr(cTask("id", "1", 2), cTask("id", "2", 1)))
				return r
			},
			run: runHeroTrialStage,
			want: []cWireReq{
				info("5201"),
				{"hero.experiment.reward", []cWireParam{{"activityId", sfs.SFSInt, int32(5201)}}},
			},
		},
		{
			name:  "crazy-rock-tasks",
			setup: plain(evActivity(99152)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "music.game.activity.info" {
					r.PutSFSArray("taskArr", cArr(cTask("taskId", "7", 1)))
				}
				return r
			},
			run: runCrazyRockTasks,
			want: []cWireReq{
				{"music.game.activity.info", []cWireParam{{"activityId", sfs.SFSInt, int32(99152)}}},
				{"music.receive.task.reward", []cWireParam{
					{"activityId", sfs.SFSInt, int32(99152)}, {"taskId", sfs.SFSInt, int32(0)}}},
			},
		},
		{
			name:  "sand-worm-tasks",
			setup: plain(evActivity(1000069)),
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "sandworm.task.info" {
					r.PutSFSArray("achievement", cArr(cTask("id", "301", 1)))
				}
				return r
			},
			run: runSandWormTasks,
			want: []cWireReq{
				{"sandworm.task.info", nil},
				{"sandworm.task.claim.reward", []cWireParam{{"taskId", sfs.SFSUtfString, "301"}}},
			},
		},
		{
			name: "theme-prenotice",
			setup: func(t *testing.T) *Init {
				activityTable[themeID] = activityRow{typ: 30, needHQ: 10}
				t.Cleanup(func() { delete(activityTable, themeID) })
				a := evActivity(themeID)
				a.PutLong("preTime", evTestNow.Add(-50*time.Hour).UnixMilli()) // day 3 of the notice
				a.PutLong("startTime", evTestNow.Add(time.Hour).UnixMilli())
				return evInit(10, a)
			},
			reply: func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "get.theme.activity.info" {
					r.PutSFSArray("stageArr", cArr(obj("day", 1, "state", 1))) // day 1 claimed
				}
				return r
			},
			run: runThemeNotice,
			want: []cWireReq{
				{"get.theme.activity.info", []cWireParam{{"activityId", sfs.SFSInt, themeID}}},
				{"receive.theme.activity.reward", []cWireParam{
					{"activityId", sfs.SFSInt, themeID}, {"day", sfs.SFSInt, int32(2)}}},
				{"receive.theme.activity.reward", []cWireParam{
					{"activityId", sfs.SFSInt, themeID}, {"day", sfs.SFSInt, int32(3)}}},
			},
		},
	})
}
