package game

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// foodInfo wraps one info[0] entry the way the info replies send it.
func foodInfo(e *sfs.SFSObject) *sfs.SFSObject {
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(e)
	r := sfs.NewSFSObject()
	r.PutSFSArray("info", arr)
	return r
}

func cookingEntry(total int32, claimed string, scores ...int32) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutInt("id", 7)
	e.PutInt("totalScore", total)
	e.PutInt("freeReward", 1) // the daily pack is claimed
	l := sfs.NewSFSArray()
	for _, s := range scores {
		o := sfs.NewSFSObject()
		o.PutInt("score", s)
		o.PutSFSArray("reward", freebieReward())
		l.AddSFSObject(o)
	}
	e.PutSFSArray("score_reward", l)
	if claimed != "" {
		e.PutUtfString("rewards", claimed)
	}
	return e
}

func banquetEntry(level int32, claimed string, rewards int) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutInt("id", 31)
	e.PutInt("level", level)
	e.PutInt("freeReward", 1)
	l := sfs.NewSFSArray()
	for range rewards {
		o := sfs.NewSFSObject()
		o.PutSFSArray("rewards", freebieReward())
		l.AddSFSObject(o)
	}
	e.PutSFSArray("level_reward", l)
	if claimed != "" {
		e.PutUtfString("levels", claimed)
	}
	return e
}

// G15: Cooking claims each score box reached and not in the rewards CSV with {aid, id: info[0].id,
// score} (CookingRewardBoxItem.lua:57-65); Banquet each level box 1..level not in the levels CSV
// with {aid, id, level} (UIActChristmasTree.lua:467-477), all Int.
func TestFoodEventBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case cookingInfoCmd:
			return foodInfo(cookingEntry(450, "100", 100, 300, 500))
		case banquetInfoCmd:
			return foodInfo(banquetEntry(3, "1,3", 5))
		}
		return evOK()
	})
	if err := runFoodBoxes(conn, evInit(30, evActivity(99003), evActivity(99004)), false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{cookingInfoCmd, cookingScoreCmd, banquetInfoCmd, banquetLevelCmd}, ",")
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	c := fake.only(cookingScoreCmd)[0].Params
	b := fake.only(banquetLevelCmd)[0].Params
	for _, k := range []struct {
		p    *sfs.SFSObject
		key  string
		want int32
	}{{c, "aid", 99003}, {c, "id", 7}, {c, "score", 300}, {b, "aid", 99004}, {b, "id", 31}, {b, "level", 2}} {
		if v, _ := k.p.Get(k.key); v.Type != sfs.SFSInt || k.p.GetInt(k.key) != k.want {
			t.Errorf("%s = %#v, want Int %d", k.key, v, k.want)
		}
	}
}

// G15: the level boxes stop at level_reward's length when it is shorter than the level.
func TestFoodLevelClaimsBound(t *testing.T) {
	a := Activity{ID: 99004, Type: actTypeBanquet}
	if got := len(foodLevelClaims(a, banquetEntry(9, "", 4), banquetLevelCmd, "banquet")); got != 4 {
		t.Errorf("level 9 with 4 level rewards: %d claims, want 4", got)
	}
	if got := len(foodLevelClaims(a, banquetEntry(2, "", 4), banquetLevelCmd, "banquet")); got != 2 {
		t.Errorf("level 2 with 4 level rewards: %d claims, want 2", got)
	}
	// a single level_reward entry bounds too (GetCanGetScoreBoxCount, ActBanquetData.lua:347-355,
	// counts over levelReward)
	if got := len(foodLevelClaims(a, banquetEntry(3, "", 1), banquetLevelCmd, "banquet")); got != 1 {
		t.Errorf("level 3 with 1 level reward: %d claims, want 1", got)
	}
	// 60 is this client's own bound per activity and run (no client line: the feature's cap)
	if got := len(foodLevelClaims(a, banquetEntry(500, "", 0), banquetLevelCmd, "banquet")); got != 60 {
		t.Errorf("level 500 without level_reward: %d claims, want the bound 60", got)
	}
}

// G15: Banquet v2 reads activity.food.party.v2.info {aid: Int}, then claims the state-1 tasks
// {activityId: Int, taskId: UtfString}, the level boxes and a non-empty stash {aid: Int}
// (ActBanquetV2Data.lua:69-141; cov-ui-city F4's corrected sequence).
func TestBanquetV2Claims(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == banquetV2InfoCmd {
			e := banquetEntry(2, "1", 5)
			e.PutSFSArray("extraReward", freebieReward())
			r := foodInfo(e)
			r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"801": 1}))
			return r
		}
		return evOK()
	})
	if err := runBanquetV2(conn, evInit(30, evActivity(99065)), false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{banquetV2InfoCmd, banquetV2TaskCmd, banquetV2LevelCmd, banquetV2ExtraCmd}, ",")
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if v, _ := fake.only(banquetV2InfoCmd)[0].Params.Get("aid"); v.Type != sfs.SFSInt || v.Val != int32(99065) {
		t.Errorf("info aid %#v, want Int 99065", v)
	}
	task := fake.only(banquetV2TaskCmd)[0].Params
	if v, _ := task.Get("taskId"); v.Type != sfs.SFSUtfString || v.Val != "801" {
		t.Errorf("taskId %#v, want UtfString 801", v)
	}
	if task.GetInt("activityId") != 99065 {
		t.Errorf("task activityId %d", task.GetInt("activityId"))
	}
	if lv := fake.only(banquetV2LevelCmd)[0].Params; lv.GetInt("level") != 2 || lv.GetInt("id") != 31 || lv.GetInt("aid") != 99065 {
		t.Errorf("level claim %v, want {aid 99065, id 31, level 2}", lv)
	}
	if ex := fake.only(banquetV2ExtraCmd)[0].Params; ex.GetInt("aid") != 99065 {
		t.Errorf("stash claim %v", ex)
	}
}

// G15: the -plan twins send only the info reads and log every claim they would send.
func TestFoodEventPlansSendNoClaim(t *testing.T) {
	withEvNow(t, evTestNow)
	var logs bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(saved) })
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case cookingInfoCmd:
			return foodInfo(cookingEntry(450, "", 100))
		case banquetInfoCmd:
			return foodInfo(banquetEntry(1, "", 5))
		case banquetV2InfoCmd:
			r := foodInfo(banquetEntry(1, "", 5))
			r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"801": 1}))
			return r
		}
		return evErr("E_SHOULD_NOT_BE_SENT")
	})
	in := evInit(30, evActivity(99003), evActivity(99004), evActivity(99065))
	for _, name := range []string{foodBoxesFeature + foodPlanSuffix, banquetV2Feature + foodPlanSuffix} {
		if err := featureRegistry[name].Run(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if got := strings.Join(fake.cmds(), ","); got != cookingInfoCmd+","+banquetInfoCmd+","+banquetV2InfoCmd {
		t.Errorf("plans sent %s, want only the three info reads", got)
	}
	out := logs.String()
	for _, cmd := range []string{cookingScoreCmd, banquetLevelCmd, banquetV2TaskCmd, banquetV2LevelCmd} {
		if !strings.Contains(out, "would claim") || !strings.Contains(out, "cmd="+cmd) {
			t.Errorf("plan log lacks a would-claim line for %s:\n%s", cmd, out)
		}
	}
}

// G15: no claim command is benign; an errorCode stops the feature.
func TestFoodEventClaimErrorsStop(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case cookingInfoCmd:
			return foodInfo(cookingEntry(450, "", 100, 300))
		case banquetInfoCmd:
			return foodInfo(banquetEntry(3, "", 5))
		}
		return evErr(evAlreadyExecuted)
	})
	err := runFoodBoxes(conn, evInit(30, evActivity(99003), evActivity(99004)), false)
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if got := strings.Join(fake.cmds(), ","); got != cookingInfoCmd+","+cookingScoreCmd {
		t.Errorf("sent %s, want the first claim only", got)
	}
}

// G15, refused twin of TestBanquetV2Claims: the same reply with the stash left out and tasks that
// are not claimable. The client shows a task's receive button only in state 1 (TaskState.CanReceive,
// BanquetAttackMonsterTaskItem.lua:149-152; OnReceiveClick :166-167), and claims the stash only
// when extraReward holds one (ActBanquetV2Data.lua:113-114; UIBanquetFinRewardGetNewCtrl.lua:37-43).
func TestBanquetV2SkipsUnclaimableTasksAndAnEmptyStash(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "activity.food.party.v2.info" {
			r := foodInfo(banquetEntry(2, "1,2", 5)) // every level box claimed; no extraReward
			// 801 claimable; 802 not finished (0); 803 claimed (2); a state-1 task with no taskId
			r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"801": 1, "802": 0, "803": 2, "": 1}))
			return r
		}
		return evOK()
	})
	if err := runBanquetV2(conn, evInit(30, evActivity(99065)), false); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	if want := "activity.food.party.v2.info,activity.food.party.v2.task.reward"; got != want {
		t.Fatalf("sent %s\nwant %s (task 801 only; no stash claim)", got, want)
	}
	if id := fake.only("activity.food.party.v2.task.reward")[0].Params.GetString("taskId"); id != "801" {
		t.Errorf("claimed task %q, want 801", id)
	}
}

// G15: an errorCode stops banquet-v2-claims, across activities too: neither a refused info read
// nor a refused claim moves on to the next type-160 activity (99065 and 99141 are both table
// type-160 rows).
func TestBanquetV2StopsAtTheFirstRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	in := evInit(30, evActivity(99065), evActivity(99141))
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return evErr("E_V2_INFO") })
	if err := runBanquetV2(conn, in, false); err == nil || !strings.Contains(err.Error(), "E_V2_INFO") {
		t.Errorf("refused info: err = %v, want the E_V2_INFO failure", err)
	}
	if got := fake.cmds(); len(got) != 1 {
		t.Errorf("refused info: sent %v, want one info read and nothing after it", got)
	}

	conn, fake = startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "activity.food.party.v2.info" {
			r := foodInfo(banquetEntry(1, "", 5))
			r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"801": 1}))
			return r
		}
		return evErr("E_V2_CLAIM")
	})
	if err := runBanquetV2(conn, in, false); err == nil || !strings.Contains(err.Error(), "E_V2_CLAIM") {
		t.Errorf("refused claim: err = %v, want the E_V2_CLAIM failure", err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "activity.food.party.v2.info,activity.food.party.v2.task.reward" {
		t.Errorf("refused claim: sent %s, want one info read and the refused claim only", got)
	}
}

// G15: a claimed list sent as an int array instead of the CSV string still counts as claimed
// (evCSVNums' fallback): Cooking's rewards [100] and Banquet's levels [1, 3] leave only score box
// 300 and level box 2, as the CSV form does in TestFoodEventBoxes.
func TestFoodEventClaimedListAsIntArray(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "activity.mak.food.info":
			e := cookingEntry(450, "", 100, 300, 500)
			e.PutIntArray("rewards", []int32{100})
			return foodInfo(e)
		case "activity.food.party.info":
			e := banquetEntry(3, "", 5)
			e.PutIntArray("levels", []int32{1, 3})
			return foodInfo(e)
		}
		return evOK()
	})
	if err := runFoodBoxes(conn, evInit(30, evActivity(99003), evActivity(99004)), false); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.only("activity.mak.food.score.reward") {
		got = append(got, "score "+evString(r.Params, "score"))
	}
	for _, r := range fake.only("activity.food.party.level.reward") {
		got = append(got, "level "+evString(r.Params, "level"))
	}
	if s := strings.Join(got, ","); s != "score 300,level 2" {
		t.Errorf("claimed %s, want score 300,level 2", s)
	}
}

// G15: a box needs the reply's ids. info[0] without id sends no Cooking or Banquet claim (the claim
// carries info[0].id: ActCookingData.lua:27-28, CookingRewardBoxItem.lua:57-65,
// UIActChristmasTree.lua:467-477), and a score_reward entry without its score is no box
// (GetActScoreList, ActCookingData.lua:66-77).
func TestFoodEventEntriesWithoutIDOrScore(t *testing.T) {
	withEvNow(t, evTestNow)
	unreached := sfs.NewSFSObject()
	unreached.PutInt("score", 999)
	unreached.PutSFSArray("reward", freebieReward())
	scoreless := sfs.NewSFSObject()
	scoreless.PutSFSArray("reward", freebieReward())
	boxes := sfs.NewSFSArray()
	boxes.AddSFSObject(unreached)
	boxes.AddSFSObject(scoreless)
	noScore := cookingEntry(450, "")
	noScore.PutSFSArray("score_reward", boxes)
	for name, c := range map[string]struct{ cooking, banquet *sfs.SFSObject }{
		"no info[0].id": {duelWithoutKey(cookingEntry(450, "", 100), "id"), duelWithoutKey(banquetEntry(3, "", 5), "id")},
		"no score":      {noScore, banquetEntry(3, "1,2,3", 5)},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			switch cmd {
			case "activity.mak.food.info":
				return foodInfo(c.cooking)
			case "activity.food.party.info":
				return foodInfo(c.banquet)
			}
			return evOK()
		})
		if err := runFoodBoxes(conn, evInit(30, evActivity(99003), evActivity(99004)), false); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.Join(fake.cmds(), ","); got != "activity.mak.food.info,activity.food.party.info" {
			t.Errorf("%s: sent %s, want the two info reads only", name, got)
		}
	}
}
