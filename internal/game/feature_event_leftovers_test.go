package game

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// skyChapter is one chapterInfo entry: its stage stars and rewardedTarget.
func skyChapter(chapter int32, rewarded []int32, stars ...int32) *sfs.SFSObject {
	c := sfs.NewSFSObject()
	c.PutInt("chapter", chapter)
	c.PutIntArray("rewardedTarget", rewarded)
	a := sfs.NewSFSArray()
	for i, s := range stars {
		st := sfs.NewSFSObject()
		st.PutInt("stageId", int32(50001+i))
		st.PutInt("star", s)
		a.AddSFSObject(st)
	}
	c.PutSFSArray("stageInfo", a)
	return c
}

func TestSkyBattleBoxesClaimUntilTheReachedBoxesAreTaken(t *testing.T) {
	claims := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		switch cmd {
		case skyBattleStageCmd:
			r.PutInt("stageType", p.GetInt("stageType"))
			a := sfs.NewSFSArray()
			if p.GetInt("stageType") == 0 {
				a.AddSFSObject(skyChapter(1, []int32{6}, 3, 3, 3, 3, 2, 0)) // 14 stars: 12 is due
				a.AddSFSObject(skyChapter(2, nil, 3, 3, 3, 3, 3, 3))        // 18 stars: 6, 12, 18 due
				a.AddSFSObject(skyChapter(3, nil, 2, 2))                    // 4 stars: none
			}
			r.PutSFSArray("chapterInfo", a)
		case skyBattleBoxCmd:
			claims++
			r.PutInt("chapter", p.GetInt("chapter"))
			switch {
			case p.GetInt("chapter") == 1:
				r.PutIntArray("rewardedTarget", []int32{6, 12})
			case claims == 2:
				r.PutIntArray("rewardedTarget", []int32{6}) // one box per claim
			default:
				r.PutIntArray("rewardedTarget", []int32{6, 12, 18})
			}
		}
		return r
	})
	if err := runSkyBattleBoxes(conn, evInit(10)); err != nil {
		t.Fatal(err)
	}
	var sent []int32
	for _, r := range fake.only(skyBattleBoxCmd) {
		for _, k := range []string{"chapter", "stageType"} {
			if v, _ := r.Params.Get(k); v.Type != sfs.SFSInt {
				t.Errorf("%s type = %d, want Int", k, v.Type)
			}
		}
		sent = append(sent, r.Params.GetInt("chapter"))
	}
	if !slices.Equal(sent, []int32{1, 2, 2}) {
		t.Errorf("claimed chapters %v, want 1 once and 2 twice", sent)
	}
	stages := fake.only(skyBattleStageCmd)
	if len(stages) != 2 || stages[0].Params.GetInt("chapter") != -1 || stages[1].Params.GetInt("stageType") != 1 {
		t.Errorf("stage reads = %+v, want chapter -1 for stageType 0 and 1", stages)
	}
}

// TestSkyBattleBoxesClaimTheGrowthChapters: a stageType-1 reply's reached chapters, chapter 4
// included, send {chapter, stageType: 1} claims until each reached box is taken.
func TestSkyBattleBoxesClaimTheGrowthChapters(t *testing.T) {
	rewarded := map[int32][]int32{1: {6, 12, 18, 24}, 3: {6}}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		switch cmd {
		case skyBattleStageCmd:
			a := sfs.NewSFSArray()
			if p.GetInt("stageType") == 1 {
				a.AddSFSObject(skyChapter(1, rewarded[1], 3, 3, 3, 3, 3, 3, 3, 3)) // 24 stars, all taken
				a.AddSFSObject(skyChapter(3, rewarded[3], 3, 3, 3, 3, 0, 0))       // 12 stars: 12 is due
				a.AddSFSObject(skyChapter(4, nil, 3, 3, 3, 3, 3, 3, 3, 3))         // 24 stars: 6, 12, 18, 24 due
			}
			r.PutSFSArray("chapterInfo", a)
		case skyBattleBoxCmd:
			ch := p.GetInt("chapter")
			for _, th := range []int32{6, 12, 18, 24} { // the server grants one box per claim
				if !slices.Contains(rewarded[ch], th) {
					rewarded[ch] = append(rewarded[ch], th)
					break
				}
			}
			r.PutInt("chapter", ch)
			r.PutIntArray("rewardedTarget", rewarded[ch])
		}
		return r
	})
	if err := runSkyBattleBoxes(conn, evInit(14)); err != nil {
		t.Fatal(err)
	}
	var sent []int32
	for _, r := range fake.only(skyBattleBoxCmd) {
		for _, k := range []string{"chapter", "stageType"} {
			if v, _ := r.Params.Get(k); v.Type != sfs.SFSInt {
				t.Errorf("%s type = %d, want Int", k, v.Type)
			}
		}
		if st := r.Params.GetInt("stageType"); st != 1 {
			t.Errorf("chapter %d claim stageType = %d, want 1", r.Params.GetInt("chapter"), st)
		}
		sent = append(sent, r.Params.GetInt("chapter"))
	}
	if !slices.Equal(sent, []int32{3, 4, 4, 4, 4}) {
		t.Errorf("claimed growth chapters %v, want 3 once and 4 four times", sent)
	}
	if !slices.Equal(rewarded[4], []int32{6, 12, 18, 24}) {
		t.Errorf("chapter 4 boxes taken %v, want all four", rewarded[4])
	}
}

// TestSkyBattleModesMatchTheChapterTables pins the embedded thresholds and HQ gates to tables
// 40413: lw_sky_battle_chapter (7 rows) and lw_sky_battle_chapter_upgrade (4 rows), column
// chapter_box "stars|reward;..." and main_building_level, which the client walks row by row
// (LWSkyBattleChapterManager.lua:99-154; LWSkyBattleGrowthChapterManager.lua:144-185).
func TestSkyBattleModesMatchTheChapterTables(t *testing.T) {
	type row struct {
		id, hq int64
		box    string
	}
	const normal = "6|212000341;12|212000342;18|212000343"
	const growth = "6|110101;12|110102;18|110103;24|110104"
	tables := map[int32][]row{
		0: {{1, 6, "6|212000301;12|212000302;18|212000303"}, {2, 7, "6|212000311;12|212000312;18|212000313"},
			{3, 8, "6|212000321;12|212000322;18|212000323"}, {4, 9, "6|212000331;12|212000332;18|212000333"},
			{5, 10, normal}, {6, 10, normal}, {7, 10, normal}},
		1: {{1, 8, growth}, {2, 10, growth}, {3, 12, growth}, {4, 14, growth}},
	}
	if len(skyBattleModes) != len(tables) {
		t.Fatalf("%d modes, want %d", len(skyBattleModes), len(tables))
	}
	for _, m := range skyBattleModes {
		rows, ok := tables[m.stageType]
		if !ok {
			t.Errorf("stageType %d has no chapter table", m.stageType)
			continue
		}
		want := map[int64][]int64{}
		minHQ := rows[0].hq
		for _, r := range rows {
			for _, box := range strings.Split(r.box, ";") {
				n, err := strconv.ParseInt(strings.SplitN(box, "|", 2)[0], 10, 64)
				if err != nil {
					t.Fatalf("chapter_box %q: %v", r.box, err)
				}
				want[r.id] = append(want[r.id], n)
			}
			minHQ = min(minHQ, r.hq)
		}
		if !maps.EqualFunc(m.thresholds, want, slices.Equal[[]int64]) {
			t.Errorf("stageType %d thresholds = %v, want %v", m.stageType, m.thresholds, want)
		}
		if int64(m.minHQ) != minHQ {
			t.Errorf("stageType %d minHQ = %d, want %d (lowest main_building_level)", m.stageType, m.minHQ, minHQ)
		}
	}
}

func TestSkyBattleBoxesStopWhenAClaimChangesNothing(t *testing.T) {
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		switch cmd {
		case skyBattleStageCmd:
			a := sfs.NewSFSArray()
			if p.GetInt("stageType") == 0 {
				a.AddSFSObject(skyChapter(4, nil, 3, 3, 3, 3, 3, 3))
			}
			r.PutSFSArray("chapterInfo", a)
		case skyBattleBoxCmd:
			r.PutIntArray("rewardedTarget", nil)
		}
		return r
	})
	if err := runSkyBattleBoxes(conn, evInit(10)); err != nil {
		t.Fatal(err)
	}
	if got := fake.only(skyBattleBoxCmd); len(got) != 1 {
		t.Errorf("claims = %d, want 1 (the reply showed no change)", len(got))
	}
	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSkyBattleBoxes(conn, evInit(5)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("HQ 5 sent %v, want nothing", got)
	}
}

func TestNewbieArenaRewardsUseTheCommandsOfEachArena(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if strings.HasSuffix(cmd, "reward.preview") {
			r.PutInt("activityId", p.GetInt("activityId"))
			a := sfs.NewSFSArray()
			for id, st := range map[int32]int32{1: 2, 2: 1, 3: 0} {
				e := sfs.NewSFSObject()
				e.PutInt("id", id)
				e.PutInt("state", st)
				e.PutInt("targetRank", 100/id)
				a.AddSFSObject(e)
			}
			r.PutSFSArray("achieveRewards", a)
		}
		return r
	})
	if err := runNewbieArenaRewards(conn, evInit(30, evActivity(100000), evActivity(100001))); err != nil {
		t.Fatal(err)
	}
	want := []string{"activity.arena.reward.preview", "activity.arena.receive", "activity.arena.v2.reward.preview", "activity.arena.v2.receive"}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	for _, r := range fake.requests() {
		if v, _ := r.Params.Get("activityId"); v.Type != sfs.SFSInt {
			t.Errorf("%s activityId type = %d, want Int", r.Cmd, v.Type)
		}
		if strings.HasSuffix(r.Cmd, "receive") && r.Params.GetInt("id") != 2 {
			t.Errorf("%s id = %d, want 2 (the only state-1 row)", r.Cmd, r.Params.GetInt("id"))
		}
	}
}

func TestBirthdaySetReward(t *testing.T) {
	for _, c := range []struct {
		name     string
		birthday string
		rewarded any
		claim    bool
	}{
		{"set, never rewarded", "5-4", nil, true}, // month-day (BirthdayDataManager.lua:246-253)
		{"set, rewarded false", "5-4", false, true},
		{"set, rewarded true", "5-4", true, false},
		{"set, rewarded 0 is truthy in Lua", "5-4", int32(0), false},
		{"not set", "", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := evInit(30)
			bf := sfs.NewSFSObject()
			bf.PutUtfString("birthday", c.birthday)
			switch v := c.rewarded.(type) {
			case bool:
				bf.PutBool("rewarded", v)
			case int32:
				bf.PutInt("rewarded", v)
			}
			in.Raw.PutSFSObject("birthday_function", bf)
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
			if err := runBirthdaySetReward(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := len(fake.only(birthdaySetRewardCmd)) == 1; got != c.claim {
				t.Errorf("claimed %v, want %v", got, c.claim)
			}
		})
	}
}

func TestMasteryNewbieReward(t *testing.T) {
	for _, c := range []struct {
		name          string
		status, level int32
		claim         bool
	}{
		{"status 1 at level 10", 1, 10, true},
		{"status 1 below level 10", 1, 9, false},
		{"already claimed (2)", 2, 15, false},
		{"no status", 0, 15, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := evInit(30)
			m := sfs.NewSFSObject()
			m.PutInt("level", c.level)
			if c.status != 0 {
				m.PutInt("newbieRewardStatus", c.status)
			}
			in.Raw.PutSFSObject("desertTalent", m)
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
			if err := runMasteryNewbieReward(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := len(fake.only(masteryNewbieClaimCmd)) == 1; got != c.claim {
				t.Errorf("claimed %v, want %v", got, c.claim)
			}
		})
	}
}

// counterAttackInit is an Init in an alliance (or not) with the Counter Attack activity 1000016
// (type 226) open.
func counterAttackInit(inAlliance bool) *Init {
	in := evInit(30, evActivity(1000016))
	if inAlliance {
		u := sfs.NewSFSObject()
		u.PutUtfString("allianceId", "fake-alliance")
		in.Raw.PutSFSObject("user", u)
	}
	return in
}

func TestCounterAttackRoundsClaimAll(t *testing.T) {
	withEvNow(t, evTestNow)
	rounds := func(states ...int32) func(string, *sfs.SFSObject) *sfs.SFSObject {
		return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			r := evOK()
			if cmd == counterAttackInfoCmd {
				a := sfs.NewSFSArray()
				for i, st := range states {
					e := sfs.NewSFSObject()
					e.PutInt("round", int32(i+1))
					e.PutInt("state", st)
					a.AddSFSObject(e)
				}
				r.PutSFSArray("allianceRoundReward", a)
			}
			return r
		}
	}
	conn, fake := startEvFake(t, rounds(2, 1, 0))
	if err := runCounterAttackRounds(conn, counterAttackInit(true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{counterAttackInfoCmd, counterAttackClaimCmd}) {
		t.Fatalf("sent %v", got)
	}
	info, claim := fake.requests()[0].Params, fake.requests()[1].Params
	if info.GetInt("aid") != 1000016 || info.GetInt("type") != 1 {
		t.Errorf("info params = %v, want {aid: 1000016, type: 1}", info)
	}
	if v, _ := claim.Get("all"); v.Type != sfs.SFSBool || v.Val != true || claim.GetInt("round") != 1 || claim.GetInt("aid") != 1000016 {
		t.Errorf("claim params = %v, want {aid: 1000016, round: 1, all: true}", claim)
	}

	conn, fake = startEvFake(t, rounds(2, 0))
	if err := runCounterAttackRounds(conn, counterAttackInit(true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.only(counterAttackClaimCmd); len(got) != 0 {
		t.Errorf("claimed %v with no round in state 1", got)
	}

	conn, fake = startEvFake(t, rounds(1))
	if err := runCounterAttackRounds(conn, counterAttackInit(false)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v outside an alliance", got)
	}
}
