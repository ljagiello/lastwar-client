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
	if got := len(foodLevelClaims(a, banquetEntry(500, "", 0), banquetLevelCmd, "banquet")); got != foodMaxLevelClaims {
		t.Errorf("level 500 without level_reward: %d claims, want the bound %d", got, foodMaxLevelClaims)
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
