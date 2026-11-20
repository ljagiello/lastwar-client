package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// crystalBossFake answers the crystal boss reads: the march reply names marchID (none when ""),
// the progress reply is for progressSeason with one claimable segment, and the achievement reply
// carries tasks and names replyID (none when "").
func crystalBossFake(t *testing.T, marchID string, progressSeason int32, replyID string, tasks *sfs.SFSArray) (*evFake, func(*Init) error) {
	t.Helper()
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case redBossMarchCmd:
			if marchID != "" {
				r.PutUtfString("activityId", marchID)
			}
		case redBossProgressCmd:
			r.PutInt("claimableCount", 1)
			r.PutInt("season", progressSeason)
		case redBossAchievementCmd:
			if replyID != "" {
				r.PutUtfString("activityId", replyID)
			}
			if tasks != nil {
				r.PutSFSArray("tasks", tasks)
			}
		case arenaInfoCmd:
		default:
			return evOK()
		}
		return r
	})
	return fake, func(in *Init) error { return runEventClaims(conn, in) }
}

// The client's achievement types, CrystalBossAchievementDefine.Type = {Damage = 1, AttackCount = 2}
// (CrystalBossAchievementDefine.lua:2). Literals, so a wrong redBossAchievement* constant fails.
const (
	crystalTypeDamage int32 = 1
	crystalTypeAttack int32 = 2
)

func crystalBossTask(typ, id, state, season int32) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutInt("type", typ)
	o.PutInt("id", id)
	o.PutInt("state", state)
	if season >= 0 {
		o.PutInt("season", season)
	}
	return o
}

func crystalBossInit(hq int32, act *sfs.SFSObject, season int32) *Init {
	in := evInit(hq, act)
	if season > 0 {
		s := sfs.NewSFSObject()
		s.PutInt("seasonId", season)
		in.Raw.PutSFSObject("playerServerSeasonInfo", s)
	}
	return in
}

func crystalBossCmds(fake *evFake) string {
	var out []string
	for _, c := range fake.cmds() {
		if strings.HasPrefix(c, "red.boss.") {
			out = append(out, c)
		}
	}
	return strings.Join(out, ",")
}

// B12: the client asks for nothing below the condition's HQ (80152 is "1|20"), or outside
// [readyTime or startTime, endViewTime or rewardTime or endTime] (CrystalBossDataManager.lua:
// 386-416), and it sends the march read first.
func TestCrystalBossHQAndWindowGates(t *testing.T) {
	withEvNow(t, evTestNow)
	at := func(start, end, view, reward, ready time.Duration) *sfs.SFSObject {
		a := sfs.NewSFSObject()
		a.PutInt("id", 80152)
		a.PutLong("startTime", evTestNow.Add(start).UnixMilli())
		a.PutLong("endTime", evTestNow.Add(end).UnixMilli())
		if view != 0 {
			a.PutLong("endViewTime", evTestNow.Add(view).UnixMilli())
		}
		if reward != 0 {
			a.PutLong("rewardTime", evTestNow.Add(reward).UnixMilli())
		}
		if ready != 0 {
			a.PutLong("readyTime", evTestNow.Add(ready).UnixMilli())
		}
		return a
	}
	all := "red.boss.get.march,red.boss.get.progress,red.boss.claim.all,red.boss.get.achievement.info"
	for _, c := range []struct {
		name string
		hq   int32
		act  *sfs.SFSObject
		want string
	}{
		{"HQ 19 is below the condition", 19, evActivity(80152), ""},
		{"HQ 20 meets it", 20, evActivity(80152), all},
		{"after endTime, before endViewTime", 30, at(-48*time.Hour, -time.Hour, time.Hour, 0, 0), all},
		{"after endTime, before rewardTime", 30, at(-48*time.Hour, -time.Hour, 0, time.Hour, 0), all},
		{"endViewTime wins over rewardTime", 30, at(-48*time.Hour, -2*time.Hour, -time.Hour, time.Hour, 0), ""},
		{"after the show window", 30, at(-48*time.Hour, -2*time.Hour, -time.Hour, 0, 0), ""},
		{"readyTime opens it before startTime", 30, at(time.Hour, 48*time.Hour, 0, 0, -time.Hour), all},
		{"before startTime without readyTime", 30, at(time.Hour, 48*time.Hour, 0, 0, 0), ""},
	} {
		fake, run := crystalBossFake(t, "80152", 0, "", nil)
		if err := run(crystalBossInit(c.hq, c.act, 0)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := crystalBossCmds(fake); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCrystalBossConditionParse(t *testing.T) {
	saved, had := activityTable[80152]
	if !had || saved.condition != "1|20" {
		t.Fatalf("activity 80152 row = %+v (present %v), want condition 1|20 (tables 40413)", saved, had)
	}
	t.Cleanup(func() { activityTable[80152] = saved })
	a := Activity{ID: 80152, Type: actTypeCrystalBoss}
	for _, c := range []struct {
		cond string
		hq   int32
		want bool
	}{
		{"", 1, true}, {"1|20", 20, true}, {" 1 | 20 ", 20, true}, {"01|20", 20, true}, {"1|20", 19, false},
		{"2|20", 30, false}, {"1|20,8|9", 30, false}, {"x", 30, false},
	} {
		row := saved
		row.condition = c.cond
		activityTable[80152] = row
		if got, why := redBossUnlocked(a, c.hq); got != c.want {
			t.Errorf("condition %q at HQ %d: unlocked %v (%s), want %v", c.cond, c.hq, got, why, c.want)
		}
	}
}

// B12: progress and achievements are asked only with the march reply's activityId, and the
// achievement request carries that id as a UtfString (CrystalBossDataManager.lua:756-760, 589-598).
func TestCrystalBossUsesTheMarchReplysActivityID(t *testing.T) {
	withEvNow(t, evTestNow)
	fake, run := crystalBossFake(t, "", 0, "", nil)
	if err := run(crystalBossInit(30, evActivity(80152), 0)); err != nil {
		t.Fatal(err)
	}
	if got := crystalBossCmds(fake); got != "red.boss.get.march" {
		t.Errorf("no activityId in the march reply: sent %q, want only the march read", got)
	}

	tasks := sfs.NewSFSArray()
	tasks.AddSFSObject(crystalBossTask(crystalTypeDamage, 1, 1, -1))
	fake, run = crystalBossFake(t, "777", 0, "", tasks)
	if err := run(crystalBossInit(30, evActivity(80152), 0)); err != nil {
		t.Fatal(err)
	}
	ach := fake.only(redBossAchievementCmd)
	if len(ach) != 1 {
		t.Fatalf("achievement reads = %d, want 1", len(ach))
	}
	if v, _ := ach[0].Params.Get("activityId"); v.Type != sfs.SFSUtfString || v.Val != "777" {
		t.Errorf("achievement activityId = %#v, want UtfString 777 from the march reply", v)
	}
	if got := crystalBossCmds(fake); !strings.HasPrefix(got, "red.boss.get.march,red.boss.get.progress") {
		t.Errorf("order %q: the march read must come first", got)
	}
}

// B12 (cov-dc-events F8): a progress reply without the account's season, an achievement reply for
// another activity, and attack-count tasks of another season claim nothing.
func TestCrystalBossSeasonAndActivityFilters(t *testing.T) {
	withEvNow(t, evTestNow)
	claims := func(fake *evFake) []int32 {
		var out []int32
		for _, r := range fake.only(redBossClaimAllCmd) {
			out = append(out, r.Params.GetInt("type"))
		}
		return out
	}
	one := func(tasks ...*sfs.SFSObject) *sfs.SFSArray {
		a := sfs.NewSFSArray()
		for _, t := range tasks {
			a.AddSFSObject(t)
		}
		return a
	}
	for _, c := range []struct {
		name     string
		progress int32
		replyID  string
		tasks    *sfs.SFSArray
		want     []int32
	}{
		{"progress of another season", 6, "", nil, nil},
		{"progress of the account's season", 7, "", nil, []int32{1}},
		{"attack task of another season", 6, "", one(crystalBossTask(crystalTypeAttack, 3, 1, 6)), nil},
		{"attack task of this season", 6, "", one(crystalBossTask(crystalTypeAttack, 3, 1, 7)), []int32{2}},
		{"damage task, any season", 6, "", one(crystalBossTask(crystalTypeDamage, 3, 1, -1)), []int32{2}},
		{"unknown task type", 6, "", one(crystalBossTask(5, 3, 1, 7)), nil},
		{"task without an id", 6, "", one(crystalBossTask(crystalTypeDamage, 0, 1, -1)), nil},
		{"reply for another activity", 6, "999", one(crystalBossTask(crystalTypeDamage, 3, 1, -1)), nil},
		{"reply naming this activity", 6, "80152", one(crystalBossTask(crystalTypeDamage, 3, 1, -1)), []int32{2}},
	} {
		fake, run := crystalBossFake(t, "80152", c.progress, c.replyID, c.tasks)
		if err := run(crystalBossInit(30, evActivity(80152), 7)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := claims(fake); !slices.Equal(got, c.want) {
			t.Errorf("%s: claim types %v, want %v", c.name, got, c.want)
		}
	}
}

// B58: a type-34 event shown by ActSevenDay (showUiType 0 or 2) claims a box only on its free flag
// (ActSevenDayBoxItem.lua:51, 93-96); the DoomVanguard view (showUiType 3) keeps the VIP term.
func TestSevenDayPerBoxIgnoresTheVIPFlag(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case sevenDayInfoCmd:
			// box 1: free claimed, VIP unclaimed, VIP level reached (needVipLevel 10)
			return eventScoreBoxes(500, [3]int32{100, 1, 0})
		case arenaInfoCmd:
			return sfs.NewSFSObject()
		}
		return evOK()
	})
	in := evInit(30, evActivity(3013), evActivity(99024), evActivity(95003))
	vip := sfs.NewSFSObject()
	vi := sfs.NewSFSObject()
	vi.PutInt("level", 12)
	vip.PutSFSObject("vipInfo", vi)
	in.Raw.PutSFSObject("vip", vip)
	if err := runEventClaims(conn, in); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(sevenDayOneCmd)); n != 0 {
		t.Errorf("per-box claims = %d, want 0 (3013 and 99024 have only the VIP reward left)", n)
	}
	all := fake.only(sevenDayAllCmd)
	if len(all) != 1 || all[0].Params.GetInt("activityId") != 95003 {
		t.Errorf("claim-all = %v, want one for 95003 (DoomVanguard keeps the VIP term)", all)
	}
}

func TestSevenDayClaimAllFollowsShowUIType(t *testing.T) {
	for id, want := range map[int32]int32{95003: 3, 95005: 3, 99024: 2, 99116: 2, 3013: 0} {
		if got := (Activity{ID: id}).ShowUIType(); got != want {
			t.Errorf("activity %d showUiType = %d, want %d (tables 40413)", id, got, want)
		}
	}
}
