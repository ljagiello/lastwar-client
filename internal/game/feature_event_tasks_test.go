package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// wp9Features are the opt-in claims added for the 1.0.370 audit's work package 9.
var wp9Features = []string{
	doomsdayQuestsFeature, parkourTasksFeature, frontBreakTasksFeature, leadingQuestFeature, heroTrialFeature,
	crazyRockFeature, themeNoticeFeature, offSeasonTasksFeature, tacticalBoxPointsFeature, tacticalCollectionFeature,
	lettersFeature, survivorPackFeature, skyBattleBoxesFeature, newbieArenaFeature,
	birthdaySetFeature, masteryNewbieFeature, counterAttackFeature, sandWormFeature,
}

// TestWP9LeavesTheExpiredItemExchangeUnregistered: MASTER G45's activity.goods.recovery {goodsId,
// num} converts (uses up) the whole owned stack (ItemExchangeManager.lua:42-53), so it is no claim
// (IMPLEMENT rule 9) and stays unregistered until the user rules on converting expired items.
func TestWP9LeavesTheExpiredItemExchangeUnregistered(t *testing.T) {
	if _, ok := featureRegistry["expired-items"]; ok {
		t.Error("expired-items is registered; it spends items and waits for the user's decision")
	}
}

func TestSandWormTasksClaimEachDoneTask(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if cmd == sandWormTaskInfoCmd {
			r.PutSFSArray("achievement", evTasks("id", map[string]int32{"301": 1, "302": 2}))
			r.PutSFSArray("daily", evTasks("id", map[string]int32{"401": 0, "402": 1}))
		}
		return r
	})
	if err := runSandWormTasks(conn, evInit(30, evActivity(1000069))); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, r := range fake.only(sandWormTaskRewardCmd) {
		if v, _ := r.Params.Get("taskId"); v.Type != sfs.SFSUtfString {
			t.Errorf("taskId type = %d, want UtfString", v.Type)
		}
		sent = append(sent, r.Params.GetString("taskId"))
	}
	if !slices.Equal(sent, []string{"301", "402"}) {
		t.Errorf("claimed %v, want 301 and 402", sent)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSandWormTasks(conn, evInit(30, evActivity(99152))); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v without a Sand Worm Hunt", got)
	}
}

// TestWP9FeaturesAreOptInAndStaticOnly: every new claim is registered, off by default, and says it
// was not validated live.
func TestWP9FeaturesAreOptInAndStaticOnly(t *testing.T) {
	for _, name := range wp9Features {
		f, ok := featureRegistry[name]
		if !ok {
			t.Errorf("%s is not registered", name)
			continue
		}
		if f.DefaultOn {
			t.Errorf("%s is on by default; new claims stay opt-in until validated live", name)
		}
		if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s summary %q must end with (static-only, not validated live)", name, f.Summary)
		}
		if featurePhase(name) == phaseCapacity || len(f.Duel) > 0 || f.Hold {
			t.Errorf("%s: phase %d, duel %v, hold %v; want a plain claim", name, featurePhase(name), f.Duel, f.Hold)
		}
	}
}

// evTasks builds an array of {idKey, state} entries.
func evTasks(idKey string, states map[string]int32) *sfs.SFSArray {
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	a := sfs.NewSFSArray()
	for _, id := range ids {
		e := sfs.NewSFSObject()
		e.PutUtfString(idKey, id)
		e.PutInt("state", states[id])
		a.AddSFSObject(e)
	}
	return a
}

func TestDoomsdayQuestsClaimAllWhenAQuestIsDone(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name   string
		has    string
		states map[int32]int32
		claim  bool
	}{
		{"done and unclaimed", "101,", map[int32]int32{101: 1, 102: 1, 103: 0}, true},
		{"done but in has_rewards", "101,102", map[int32]int32{101: 1, 102: 1, 103: 0}, false},
		{"nothing done", "", map[int32]int32{101: 0}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd != doomsdayQuestInfoCmd {
					return evOK()
				}
				q := sfs.NewSFSObject()
				q.PutUtfString("has_rewards", c.has)
				a := sfs.NewSFSArray()
				for _, id := range []int32{101, 102, 103} {
					if st, ok := c.states[id]; ok {
						e := sfs.NewSFSObject()
						e.PutUtfString("uuid", "u"+string(rune('0'+id%10)))
						e.PutInt("quest_id", id)
						e.PutInt("state", st)
						a.AddSFSObject(e)
					}
				}
				q.PutSFSArray("doomsday_quests", a)
				r := evOK()
				r.PutSFSObject("quest_info", q)
				return r
			})
			if err := runDoomsdayQuests(conn, evInit(30, evActivity(94210))); err != nil {
				t.Fatal(err)
			}
			if got := fake.only(doomsdayQuestInfoCmd); len(got) != 1 || !slices.Equal(got[0].Params.Keys(), []string{"_id"}) {
				t.Errorf("info requests = %+v, want one with no params (the transport's _id only)", got)
			}
			claims := fake.only(doomsdayQuestRewardCmd)
			if !c.claim {
				if len(claims) != 0 {
					t.Errorf("claimed %+v, want nothing", claims)
				}
				return
			}
			if len(claims) != 1 {
				t.Fatalf("claims = %+v, want one claim-all", claims)
			}
			if v, _ := claims[0].Params.Get("uuid"); v.Type != sfs.SFSUtfString || v.Val != "all" {
				t.Errorf("uuid = %#v, want UtfString all", v)
			}
		})
	}
}

func TestDoomsdayQuestsNeedAnOpenEvent(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runDoomsdayQuests(conn, evInit(30, evActivity(99152))); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v without a Doomsday activity", got)
	}
}

// TestDoomsdayQuestsSkipAQuestWithoutAQuestID: OnGetQuestInfo sorts the quests by quest_id
// (LWDoomsdayManager.lua:113-115), which fails on a quest without one, so the client shows nothing
// claimable; here the only other quest is not done. No claim-all goes out.
func TestDoomsdayQuestsSkipAQuestWithoutAQuestID(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd != "activity.doomsday.quest.info" {
			return evOK()
		}
		q := sfs.NewSFSObject()
		q.PutUtfString("has_rewards", "")
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj("uuid", "u0", "state", 1)) // no quest_id
		a.AddSFSObject(obj("uuid", "u1", "quest_id", 101, "state", 0))
		q.PutSFSArray("doomsday_quests", a)
		r := evOK()
		r.PutSFSObject("quest_info", q)
		return r
	})
	if err := runDoomsdayQuests(conn, evInit(30, evActivity(94210))); err != nil {
		t.Fatal(err)
	}
	if got := fake.only("activity.doomsday.quest.reward"); len(got) != 0 {
		t.Errorf("claimed %+v, want nothing", got)
	}
}

// TestSandWormTasksStopOnRefusal: the first refused claim stops the rest (achievement 301 is
// refused, so achievement 302 and daily 402 are not claimed), and a state-1 task without an id is
// never claimed.
func TestSandWormTasksStopOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name        string
		achievement *sfs.SFSArray
		refuse      string
		want        []string
	}{
		{"301 refused", evTasks("id", map[string]int32{"301": 1, "302": 1}), "301", []string{"301"}},
		{"none refused", evTasks("id", map[string]int32{"301": 1, "302": 1}), "", []string{"301", "302", "402"}},
		{"a done task without an id", func() *sfs.SFSArray {
			a := sfs.NewSFSArray()
			a.AddSFSObject(obj("state", 1))
			a.AddSFSObject(obj("id", "303", "state", 1))
			return a
		}(), "", []string{"303", "402"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case "sandworm.task.info":
					r := evOK()
					r.PutSFSArray("achievement", c.achievement)
					r.PutSFSArray("daily", evTasks("id", map[string]int32{"402": 1}))
					return r
				case "sandworm.task.claim.reward":
					if p.GetString("taskId") == c.refuse {
						return evErr("E240001")
					}
				}
				return evOK()
			})
			err := runSandWormTasks(conn, evInit(30, evActivity(1000069)))
			if (err != nil) != (c.refuse != "") {
				t.Errorf("err = %v, want a refusal only when one is sent", err)
			}
			var sent []string
			for _, r := range fake.only("sandworm.task.claim.reward") {
				sent = append(sent, r.Params.GetString("taskId"))
			}
			if !slices.Equal(sent, c.want) {
				t.Errorf("claimed %q, want %q", sent, c.want)
			}
		})
	}
}

// evInfo starts a hero.event.info.get data reply. The client drops a reply that carries success
// (Net/Msgs/Activity/ActivityEventInfoGetMessage.lua:26-29), and so does EventInfo
// (errEventInfoNoData), so an info fixture must not start from evOK.
func evInfo() *sfs.SFSObject { return sfs.NewSFSObject() }

func TestParkourTasksClaimAll(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if cmd == "hero.event.info.get" {
			r = evInfo()
			switch p.GetString("activityId") {
			case "2000001":
				r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"1": 2, "2": 1, "3": 0}))
				// A soldier box in state 1 (CanGet, EnumType.lua:18304): only Front Break (type 312)
				// claims boxes, so Parkour must not.
				r.PutSFSArray("soldierBox", evTasks("id", map[string]int32{"1": 1}))
			case "2000002":
				r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"1": 2, "2": 0}))
			}
		}
		return r
	})
	if err := runParkourTasks(conn, evInit(30, evActivity(2000001), evActivity(2000002))); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(parkourTaskRewardCmd)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v, want one (only 2000001 has a claimable task)", claims)
	}
	p := claims[0].Params
	if v, _ := p.Get("activityId"); v.Type != sfs.SFSInt || p.GetInt("activityId") != 2000001 {
		t.Errorf("activityId = %#v, want Int 2000001", v)
	}
	if v, _ := p.Get("taskId"); v.Type != sfs.SFSUtfString || v.Val != "0" {
		t.Errorf("taskId = %#v, want UtfString \"0\" (claim all)", v)
	}
	if got := fake.only(frontBreakSoldierBoxCmd); len(got) != 0 {
		t.Errorf("parkour sent the Front Break soldier-box claim: %+v", got)
	}
}

func TestFrontBreakTasksAndSoldierBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	boxes := func(states ...int32) *sfs.SFSArray {
		a := sfs.NewSFSArray()
		for i, st := range states {
			e := sfs.NewSFSObject()
			e.PutInt("id", int32(i+1))
			e.PutInt("state", st)
			e.PutInt("target", int32(100*(i+1)))
			a.AddSFSObject(e)
		}
		return a
	}
	for _, c := range []struct {
		name  string
		tasks map[string]int32
		boxes []int32
		want  []string
	}{
		{"tasks and a box", map[string]int32{"5": 1}, []int32{2, 1, 0}, []string{"hero.event.info.get", frontBreakTaskRewardCmd, frontBreakSoldierBoxCmd}},
		{"box only", map[string]int32{"5": 2}, []int32{1, 0}, []string{"hero.event.info.get", frontBreakSoldierBoxCmd}},
		{"nothing reached", map[string]int32{"5": 0}, []int32{2, 0}, []string{"hero.event.info.get"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == "hero.event.info.get" {
					r = evInfo()
					r.PutSFSArray("taskArr", evTasks("taskId", c.tasks))
					r.PutSFSArray("soldierBox", boxes(c.boxes...))
				}
				return r
			})
			if err := runFrontBreakTasks(conn, evInit(30, evActivity(2400001))); err != nil {
				t.Fatal(err)
			}
			if got := fake.cmds(); !slices.Equal(got, c.want) {
				t.Fatalf("sent %v, want %v", got, c.want)
			}
			for _, r := range fake.only(frontBreakSoldierBoxCmd) {
				if v, _ := r.Params.Get("id"); v.Type != sfs.SFSInt || r.Params.GetInt("id") != 0 || r.Params.GetInt("activityId") != 2400001 {
					t.Errorf("soldier box params = %v, want {activityId: 2400001, id: Int 0}", r.Params)
				}
			}
		})
	}
}

// TestFrontBreakTaskRefusalStopsTheSoldierBoxes: the "tasks and a box" fixture with the task
// claim refused. An errorCode stops that event's claims, so the reached box is not claimed.
func TestFrontBreakTaskRefusalStopsTheSoldierBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			r := evInfo()
			r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"5": 1}))
			r.PutSFSArray("soldierBox", evTasks("id", map[string]int32{"1": 2, "2": 1}))
			return r
		case "activity.plane.feature.reward":
			return evErr("E300001")
		}
		return evOK()
	})
	err := runFrontBreakTasks(conn, evInit(30, evActivity(2400001)))
	if err == nil || !strings.Contains(err.Error(), "E300001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	if got, want := fake.cmds(), []string{"hero.event.info.get", "activity.plane.feature.reward"}; !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v (the refused task claim stops the soldier box)", got, want)
	}
}

// TestLeadingQuestTasksSkipATaskWithoutAnID: a state-1 task with no taskId names nothing to claim,
// so only the task with an id is claimed and no claim carries an empty taskId.
func TestLeadingQuestTasksSkipATaskWithoutAnID(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			r := evInfo()
			a := sfs.NewSFSArray()
			a.AddSFSObject(obj("state", 1)) // no taskId
			a.AddSFSObject(obj("taskId", "11", "state", 1))
			r.PutSFSArray("taskArr", a)
			return r
		}
		return evOK()
	})
	if err := runLeadingQuestTasks(conn, evInit(30, evActivity(60011))); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, r := range fake.only("power.up.task.reward") {
		sent = append(sent, r.Params.GetString("taskId"))
	}
	if !slices.Equal(sent, []string{"11"}) {
		t.Errorf("claimed %q, want only 11", sent)
	}
}

func TestLeadingQuestTasksClaimsEachDoneTaskAndStopsOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			r := evInfo()
			r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"11": 1, "12": 2, "13": 1, "14": 1}))
			return r
		case leadingQuestRewardCmd:
			if p.GetString("taskId") == "13" {
				return evErr("E000001")
			}
		}
		return evOK()
	})
	err := runLeadingQuestTasks(conn, evInit(30, evActivity(60011)))
	if err == nil || !strings.Contains(err.Error(), "E000001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	var sent []string
	for _, r := range fake.only(leadingQuestRewardCmd) {
		if v, _ := r.Params.Get("taskId"); v.Type != sfs.SFSUtfString {
			t.Errorf("taskId type = %d, want UtfString", v.Type)
		}
		sent = append(sent, r.Params.GetString("taskId"))
	}
	if !slices.Equal(sent, []string{"11", "13"}) {
		t.Errorf("claimed %v, want 11 then 13 (the refusal of 13 stops 14)", sent)
	}
}

func TestHeroTrialStageClaimsTheCurrentStageOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	stages := func(cur int32, states ...int32) *sfs.SFSObject {
		r := evInfo()
		r.PutInt("cur_stage", cur)
		a := sfs.NewSFSArray()
		for i, st := range states {
			e := sfs.NewSFSObject()
			e.PutUtfString("id", string(rune('1'+i))) // the client tonumber()s it
			e.PutInt("state", st)
			a.AddSFSObject(e)
		}
		r.PutSFSArray("stageArr", a)
		return r
	}
	for _, c := range []struct {
		name  string
		info  *sfs.SFSObject
		claim bool
	}{
		{"current stage done", stages(2, 2, 1, 0), true},
		{"an older stage in state 1 is not the current one", stages(3, 1, 2, 0), false},
		{"current stage claimed", stages(2, 2, 2, 0), false},
		// GetEventCanRewardCount counts only the stageArr entry whose id is cur_stage
		// (UIHeroTrial.lua:517-522): no entry matches, so stages in state 1 do not count.
		{"cur_stage matches no stage", stages(4, 1, 1, 1), false},
		{"no cur_stage", func() *sfs.SFSObject {
			r := evInfo()
			v, _ := stages(1, 1).Get("stageArr")
			r.PutValue("stageArr", v)
			return r
		}(), false},
		// tonumber(v.id) == nil never holds, so a stage "0" is not the current one without a cur_stage.
		{"no cur_stage, a stage 0 in state 1", func() *sfs.SFSObject {
			r := evInfo()
			r.PutSFSArray("stageArr", evTasks("id", map[string]int32{"0": 1}))
			return r
		}(), false},
		// tonumber(nil) == 0 never holds either, so a stage without an id is not stage 0.
		{"cur_stage 0, a stage without an id in state 1", func() *sfs.SFSObject {
			r := evInfo()
			r.PutInt("cur_stage", 0)
			a := sfs.NewSFSArray()
			a.AddSFSObject(obj("state", 1))
			r.PutSFSArray("stageArr", a)
			return r
		}(), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == "hero.event.info.get" {
					return c.info
				}
				return evOK()
			})
			if err := runHeroTrialStage(conn, evInit(30, evActivity(5201))); err != nil {
				t.Fatal(err)
			}
			claims := fake.only(heroTrialRewardCmd)
			if c.claim != (len(claims) == 1) || len(claims) > 1 {
				t.Fatalf("claims = %+v, want claim=%v", claims, c.claim)
			}
			if c.claim {
				if v, _ := claims[0].Params.Get("activityId"); v.Type != sfs.SFSInt || claims[0].Params.GetInt("activityId") != 5201 {
					t.Errorf("activityId = %#v, want Int 5201", v)
				}
			}
		})
	}
}

func TestCrazyRockTasksReadTaskArrAndClaimAll(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name  string
		info  func() *sfs.SFSObject
		claim bool
	}{
		{"taskId keyed", func() *sfs.SFSObject {
			r := evOK()
			r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"7": 1}))
			return r
		}, true},
		{"id keyed", func() *sfs.SFSObject {
			r := evOK()
			r.PutSFSArray("taskArr", evTasks("id", map[string]int32{"7": 1}))
			return r
		}, true},
		{"only the push key a_task", func() *sfs.SFSObject {
			r := evOK()
			r.PutSFSArray("a_task", evTasks("taskId", map[string]int32{"7": 1}))
			return r
		}, false},
		{"nothing claimable", func() *sfs.SFSObject {
			r := evOK()
			r.PutSFSArray("taskArr", evTasks("taskId", map[string]int32{"7": 2, "8": 0}))
			return r
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == crazyRockInfoCmd {
					return c.info()
				}
				return evOK()
			})
			if err := runCrazyRockTasks(conn, evInit(30, evActivity(99152))); err != nil {
				t.Fatal(err)
			}
			infos := fake.only(crazyRockInfoCmd)
			if len(infos) != 1 {
				t.Fatalf("info requests = %d, want 1", len(infos))
			}
			if v, _ := infos[0].Params.Get("activityId"); v.Type != sfs.SFSInt || infos[0].Params.GetInt("activityId") != 99152 {
				t.Errorf("info activityId = %#v, want Int 99152", v)
			}
			claims := fake.only(crazyRockRewardCmd)
			if c.claim != (len(claims) == 1) {
				t.Fatalf("claims = %+v, want claim=%v", claims, c.claim)
			}
			if c.claim {
				if v, _ := claims[0].Params.Get("taskId"); v.Type != sfs.SFSInt || claims[0].Params.GetInt("taskId") != 0 {
					t.Errorf("taskId = %#v, want Int 0 (all)", v)
				}
			}
		})
	}
}

func TestThemeNoticeDueDays(t *testing.T) {
	day := int64(themeNoticeDayMs)
	pre := int64(1_000_000)
	for _, c := range []struct {
		name    string
		now     int64
		claimed map[int64]bool
		want    []int64
	}{
		{"at preTime", pre, nil, nil},
		{"first hour", pre + 3600_000, nil, []int64{1}},
		{"exactly one day", pre + day, nil, []int64{1}},
		{"into day 2, day 1 claimed", pre + day + 1, map[int64]bool{1: true}, []int64{2}},
		{"day 9 caps at 3", pre + 9*day, map[int64]bool{2: true}, []int64{1, 3}},
	} {
		if got := themeNoticeDueDays(pre, c.now, c.claimed); !slices.Equal(got, c.want) {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
	if themeNoticeWindow(0, 10, 5) || themeNoticeWindow(6, 10, 5) || themeNoticeWindow(1, 10, 10) || !themeNoticeWindow(1, 10, 9) {
		t.Error("themeNoticeWindow must be preTime > 0 && preTime <= now < startTime")
	}
}

// TestThemeNoticeClaimsBeforeTheStart runs the feature against a type-30 row added to the
// generated table for the test (tables 40413 have none).
func TestThemeNoticeClaimsBeforeTheStart(t *testing.T) {
	withEvNow(t, evTestNow)
	const id int32 = 777030
	activityTable[id] = activityRow{typ: themeNoticeActType, needHQ: 10}
	t.Cleanup(func() { delete(activityTable, id) })
	theme := func(hq int32, preAgo time.Duration) *Init {
		a := evActivity(id)
		a.PutLong("preTime", evTestNow.Add(-preAgo).UnixMilli())
		a.PutLong("startTime", evTestNow.Add(time.Hour).UnixMilli())
		a.PutLong("endTime", evTestNow.Add(7*24*time.Hour).UnixMilli())
		return evInit(hq, a)
	}
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if cmd == themeNoticeInfoCmd {
			r.PutInt("activityId", p.GetInt("activityId"))
			a := sfs.NewSFSArray()
			for day, st := range map[int32]int32{1: 1} { // day 1 already claimed
				e := sfs.NewSFSObject()
				e.PutInt("day", day)
				e.PutInt("state", st)
				a.AddSFSObject(e)
			}
			r.PutSFSArray("stageArr", a)
		}
		return r
	}
	conn, fake := startEvFake(t, reply)
	if err := runThemeNotice(conn, theme(10, 50*time.Hour)); err != nil { // day 3 of the notice
		t.Fatal(err)
	}
	var days []int32
	for _, r := range fake.only(themeNoticeRewardCmd) {
		if v, _ := r.Params.Get("day"); v.Type != sfs.SFSInt || r.Params.GetInt("activityId") != id {
			t.Errorf("claim params = %v", r.Params)
		}
		days = append(days, r.Params.GetInt("day"))
	}
	if !slices.Equal(days, []int32{2, 3}) {
		t.Errorf("claimed days %v, want 2 and 3", days)
	}

	conn, fake = startEvFake(t, reply)
	if err := runThemeNotice(conn, theme(9, 50*time.Hour)); err != nil { // below needMainCityLevel
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v below the HQ level", got)
	}
}

// TestThemeNoticeEntryWithoutStateIsClaimed: the client keys stageArr by day and counts a day as
// claimable only with no entry or an entry whose state is 0 (ThemeActivityManager.lua:43-60,
// 96-104; UIActivitySummaryPreviewView.lua:161-168: state ~= 0 is claimed). On day 3 of the notice,
// day 1 has state 1, day 2 an entry with no state, day 3 state 0: only day 3 is claimed.
func TestThemeNoticeEntryWithoutStateIsClaimed(t *testing.T) {
	withEvNow(t, evTestNow)
	const id int32 = 777031
	activityTable[id] = activityRow{typ: 30, needHQ: 10} // EnumActivity.ActivitySummary 30 (EnumType.lua:6319)
	t.Cleanup(func() { delete(activityTable, id) })
	a := evActivity(id)
	a.PutLong("preTime", evTestNow.Add(-50*time.Hour).UnixMilli())
	a.PutLong("startTime", evTestNow.Add(time.Hour).UnixMilli())
	a.PutLong("endTime", evTestNow.Add(7*24*time.Hour).UnixMilli())
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if cmd == "get.theme.activity.info" {
			r.PutInt("activityId", p.GetInt("activityId"))
			arr := sfs.NewSFSArray()
			arr.AddSFSObject(obj("day", 1, "state", 1))
			arr.AddSFSObject(obj("day", 2, "reward", "box"))
			arr.AddSFSObject(obj("day", 3, "state", 0))
			r.PutSFSArray("stageArr", arr)
		}
		return r
	})
	if err := runThemeNotice(conn, evInit(10, a)); err != nil {
		t.Fatal(err)
	}
	var days []int32
	for _, r := range fake.only("receive.theme.activity.reward") {
		days = append(days, r.Params.GetInt("day"))
	}
	if !slices.Equal(days, []int32{3}) {
		t.Errorf("claimed days %v, want only 3 (day 2's entry has no state: claimed, as the client reads it)", days)
	}
}

// TestThemeNoticeWindowAndRefusals runs the TestThemeNoticeClaimsBeforeTheStart fixture (day 3 of
// the notice, day 1 claimed, so days 2 and 3 are due) with one field changed each time:
//   - the theme already started, or its preTime is still ahead: CheckIfShowEntrance hides the
//     entrance outside preTime <= now < startTime (ThemeActivityManager.lua:79-81), so nothing is
//     read or claimed;
//   - the info read is refused: nothing tells which days are claimed, so nothing is claimed;
//   - the day 2 claim is refused: an errorCode stops that activity's claims, so day 3 is not sent.
func TestThemeNoticeWindowAndRefusals(t *testing.T) {
	withEvNow(t, evTestNow)
	const id int32 = 777033
	activityTable[id] = activityRow{typ: 30, needHQ: 10} // EnumActivity.ActivitySummary 30 (EnumType.lua:6319)
	t.Cleanup(func() { delete(activityTable, id) })
	theme := func(preAgo, startIn time.Duration) *Init {
		a := evActivity(id)
		a.PutLong("preTime", evTestNow.Add(-preAgo).UnixMilli())
		a.PutLong("startTime", evTestNow.Add(startIn).UnixMilli())
		a.PutLong("endTime", evTestNow.Add(7*24*time.Hour).UnixMilli())
		return evInit(10, a)
	}
	for _, c := range []struct {
		name       string
		in         *Init
		infoErr    bool
		refuseDay  int32
		wantInfo   int
		wantDays   []int32
		wantRefuse bool
	}{
		{"before the start", theme(50*time.Hour, time.Hour), false, 0, 1, []int32{2, 3}, false},
		{"started an hour ago", theme(50*time.Hour, -time.Hour), false, 0, 0, nil, false},
		{"preTime an hour ahead", theme(-time.Hour, 2*time.Hour), false, 0, 0, nil, false},
		{"info refused", theme(50*time.Hour, time.Hour), true, 0, 1, nil, true},
		{"day 2 refused", theme(50*time.Hour, time.Hour), false, 2, 1, []int32{2}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case "get.theme.activity.info":
					if c.infoErr {
						return evErr("E030001")
					}
					r := evOK()
					r.PutInt("activityId", p.GetInt("activityId"))
					arr := sfs.NewSFSArray()
					arr.AddSFSObject(obj("day", 1, "state", 1))
					r.PutSFSArray("stageArr", arr)
					return r
				case "receive.theme.activity.reward":
					if p.GetInt("day") == c.refuseDay {
						return evErr("E030002")
					}
				}
				return evOK()
			})
			err := runThemeNotice(conn, c.in)
			if (err != nil) != c.wantRefuse {
				t.Errorf("err = %v, want a refusal reported: %v", err, c.wantRefuse)
			}
			if n := len(fake.only("get.theme.activity.info")); n != c.wantInfo {
				t.Errorf("info reads %d, want %d", n, c.wantInfo)
			}
			var days []int32
			for _, r := range fake.only("receive.theme.activity.reward") {
				days = append(days, r.Params.GetInt("day"))
			}
			if !slices.Equal(days, c.wantDays) {
				t.Errorf("claimed days %v, want %v", days, c.wantDays)
			}
		})
	}
}
