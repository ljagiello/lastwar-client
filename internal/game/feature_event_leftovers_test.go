package game

import (
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

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

// skyBattleWindow sets init skyBattle {st, et} (ms), the normal mode's open window
// (LWSkyBattleChapterManager.lua:43-48).
func skyBattleWindow(in *Init, st, et time.Time) *Init {
	sb := sfs.NewSFSObject()
	sb.PutLong("st", st.UnixMilli())
	sb.PutLong("et", et.UnixMilli())
	in.Raw.PutSFSObject("skyBattle", sb)
	return in
}

// skyBattleGrowthTestID is a type-371 row added to the generated table for a test (tables 40413
// have none), the way TestThemeNoticeClaimsBeforeTheStart adds a type-30 row.
const skyBattleGrowthTestID int32 = 777371

// withSkyBattleGrowthRow adds the row with the literal type 371, EnumActivity.ActSkyBattleGrowth
// (EnumType.lua:6499), not skyBattleGrowthType, so the constant is checked against the client.
func withSkyBattleGrowthRow(t *testing.T) {
	t.Helper()
	activityTable[skyBattleGrowthTestID] = activityRow{typ: 371}
	t.Cleanup(func() { delete(activityTable, skyBattleGrowthTestID) })
}

// skyBattleReads lists the stageType of every lw.get.sky.battle.stage request, after checking
// each asks all chapters.
func skyBattleReads(t *testing.T, f *evFake) []int32 {
	t.Helper()
	var out []int32
	for _, r := range f.only(skyBattleStageCmd) {
		if ch := r.Params.GetInt("chapter"); ch != skyBattleAllChapters {
			t.Errorf("stage read chapter = %d, want -1", ch)
		}
		out = append(out, r.Params.GetInt("stageType"))
	}
	return out
}

func TestSkyBattleBoxesClaimUntilTheReachedBoxesAreTaken(t *testing.T) {
	withEvNow(t, evTestNow)
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
	in := skyBattleWindow(evInit(10), evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
	if err := runSkyBattleBoxes(conn, in); err != nil {
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
	if got := skyBattleReads(t, fake); !slices.Equal(got, []int32{0}) {
		t.Errorf("stage reads %v, want stageType 0 only (no type-371 activity open)", got)
	}
}

// TestSkyBattleBoxesClaimTheGrowthChapters: with a type-371 activity open, a stageType-1 reply's
// reached chapters, chapter 4 included, send {chapter, stageType: 1} claims until each reached box
// is taken.
func TestSkyBattleBoxesClaimTheGrowthChapters(t *testing.T) {
	withEvNow(t, evTestNow)
	withSkyBattleGrowthRow(t)
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
	if err := runSkyBattleBoxes(conn, evInit(14, evActivity(skyBattleGrowthTestID))); err != nil {
		t.Fatal(err)
	}
	if got := skyBattleReads(t, fake); !slices.Equal(got, []int32{1}) {
		t.Errorf("stage reads %v, want stageType 1 only (no init skyBattle window)", got)
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
	withEvNow(t, evTestNow)
	withSkyBattleGrowthRow(t)
	// The normal window alone: with the growth activity open too, the normal chapters are not
	// read (wp9-r3 #1).
	open := func(hq int32) *Init {
		return skyBattleWindow(evInit(hq), evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
	}
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
	if err := runSkyBattleBoxes(conn, open(10)); err != nil {
		t.Fatal(err)
	}
	if got := fake.only(skyBattleBoxCmd); len(got) != 1 {
		t.Errorf("claims = %d, want 1 (the reply showed no change)", len(got))
	}
	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSkyBattleBoxes(conn, open(5)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("HQ 5 with both modes open sent %v, want nothing", got)
	}
}

// TestSkyBattleBoxesReadOnlyOpenModes: a mode is read only while the client could open its chapter
// view: stageType 0 inside the init skyBattle {st, et} window (both ends inclusive, both fields
// needed), stageType 1 with an open type-371 activity (CheckIsSend), each from its lowest HQ.
func TestSkyBattleBoxesReadOnlyOpenModes(t *testing.T) {
	withEvNow(t, evTestNow)
	withSkyBattleGrowthRow(t)
	hour := time.Hour
	growth := func(end time.Time) *sfs.SFSObject {
		a := evActivity(skyBattleGrowthTestID)
		a.PutLong("endTime", end.UnixMilli())
		return a
	}
	open := growth(evTestNow.Add(48 * hour))
	for _, c := range []struct {
		name   string
		in     func() *Init
		stages []int32
	}{
		{"neither open (40413: no type-371 row in init)", func() *Init { return evInit(14) }, nil},
		{"normal window open", func() *Init { return skyBattleWindow(evInit(14), evTestNow.Add(-hour), evTestNow.Add(hour)) }, []int32{0}},
		{"growth activity open", func() *Init { return evInit(14, open) }, []int32{1}},
		// The city bubble opens the growth view whenever growth is open (BuildBubbleManager.lua:
		// 3773-3781), so the normal chapters are not read then (wp9-r3 #1).
		{"both open", func() *Init {
			return skyBattleWindow(evInit(14, open), evTestNow.Add(-hour), evTestNow.Add(hour))
		}, []int32{1}},
		{"normal window starts now", func() *Init { return skyBattleWindow(evInit(14), evTestNow, evTestNow.Add(hour)) }, []int32{0}},
		{"normal window ends now", func() *Init { return skyBattleWindow(evInit(14), evTestNow.Add(-hour), evTestNow) }, []int32{0}},
		{"normal window ended 1 ms ago", func() *Init {
			return skyBattleWindow(evInit(14), evTestNow.Add(-hour), evTestNow.Add(-time.Millisecond))
		}, nil},
		{"normal window starts in 1 ms", func() *Init {
			return skyBattleWindow(evInit(14), evTestNow.Add(time.Millisecond), evTestNow.Add(hour))
		}, nil},
		{"normal window without et", func() *Init {
			in := evInit(14)
			sb := sfs.NewSFSObject()
			sb.PutLong("st", evTestNow.Add(-hour).UnixMilli())
			in.Raw.PutSFSObject("skyBattle", sb)
			return in
		}, nil},
		{"normal window without st", func() *Init {
			in := evInit(14)
			sb := sfs.NewSFSObject()
			sb.PutLong("et", evTestNow.Add(hour).UnixMilli())
			in.Raw.PutSFSObject("skyBattle", sb)
			return in
		}, nil},
		{"growth activity ended past the tolerance", func() *Init { return evInit(14, growth(evTestNow.Add(-2*time.Second))) }, nil},
		// The bubble's growth test is the activity alone (LWSkyBattleGrowthChapterManager.lua:717-724),
		// so at HQ 7 it still opens the growth view, whose chapters start at HQ 8: nothing is read.
		{"HQ 7: growth below its lowest chapter", func() *Init {
			return skyBattleWindow(evInit(7, open), evTestNow.Add(-hour), evTestNow.Add(hour))
		}, nil},
		{"HQ 7: normal window only", func() *Init {
			return skyBattleWindow(evInit(7), evTestNow.Add(-hour), evTestNow.Add(hour))
		}, []int32{0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
			if err := runSkyBattleBoxes(conn, c.in()); err != nil {
				t.Fatal(err)
			}
			if got := skyBattleReads(t, fake); !slices.Equal(got, c.stages) {
				t.Errorf("stage reads %v, want %v", got, c.stages)
			}
			if got := fake.cmds(); len(got) != len(c.stages) {
				t.Errorf("sent %v, want only the %d stage reads", got, len(c.stages))
			}
		})
	}
}

// TestSkyBattleWindowOpenIsLuaIsOpen: st and et must both be present, and a 0 counts as present
// (Lua-truthy), like LWSkyBattleChapterManager:IsOpen (LWSkyBattleChapterManager.lua:307-313).
func TestSkyBattleWindowOpenIsLuaIsOpen(t *testing.T) {
	window := func(st, et int64) *sfs.SFSObject {
		o := sfs.NewSFSObject()
		o.PutLong("st", st)
		o.PutLong("et", et)
		return o
	}
	for _, c := range []struct {
		name string
		sb   *sfs.SFSObject
		now  int64
		want bool
	}{
		{"absent", nil, 5, false},
		{"empty", sfs.NewSFSObject(), 5, false},
		{"zero start counts", window(0, 10), 5, true},
		{"zero window", window(0, 0), 5, false},
		{"inside", window(1, 10), 5, true},
		{"at start", window(5, 10), 5, true},
		{"at end", window(1, 5), 5, true},
		{"after end", window(1, 4), 5, false},
		{"before start", window(6, 10), 5, false},
	} {
		if got := skyBattleWindowOpen(c.sb, c.now); got != c.want {
			t.Errorf("%s: open = %v, want %v", c.name, got, c.want)
		}
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

// TestCounterAttackRoundsSkipARoundWithoutARound: InitRoundAward files each entry by its round
// (CounterAttackDataManager.lua:234), which fails on an entry without one, so the client counts no
// claimable round; with no other round in state 1, no claim-all goes out.
func TestCounterAttackRoundsSkipARoundWithoutARound(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		if cmd == "dark.knight.alliance.reward.info" {
			a := sfs.NewSFSArray()
			a.AddSFSObject(obj("state", 1)) // no round
			a.AddSFSObject(obj("round", 2, "state", 0))
			r.PutSFSArray("allianceRoundReward", a)
		}
		return r
	})
	if err := runCounterAttackRounds(conn, counterAttackInit(true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.only("dark.knight.alliance.award.round"); len(got) != 0 {
		t.Errorf("claimed %+v, want nothing", got)
	}
}

// TestSkyBattleBoxesStopOnRefusal: the normal-window fixture of
// TestSkyBattleBoxesClaimUntilTheReachedBoxesAreTaken with one reply refused.
//   - The chapter 1 box claim is refused: an errorCode stops the feature's claims, so chapter 2's
//     due box is not claimed.
//   - The stage read is refused: there is nothing to read, so nothing is claimed.
func TestSkyBattleBoxesStopOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name      string
		refuse    string
		wantBoxes []int32
	}{
		{"none refused", "", []int32{1, 2}},
		{"chapter 1 box refused", "lw.sky.battle.chapter.box.reward", []int32{1}},
		{"stage read refused", "lw.get.sky.battle.stage", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == c.refuse {
					return evErr("E371001")
				}
				r := evOK()
				switch cmd {
				case "lw.get.sky.battle.stage":
					a := sfs.NewSFSArray()
					a.AddSFSObject(skyChapter(1, nil, 3, 3)) // 6 stars: the 6-star box is due
					a.AddSFSObject(skyChapter(2, nil, 3, 3))
					r.PutSFSArray("chapterInfo", a)
				case "lw.sky.battle.chapter.box.reward":
					r.PutIntArray("rewardedTarget", []int32{6})
				}
				return r
			})
			in := skyBattleWindow(evInit(10), evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
			err := runSkyBattleBoxes(conn, in)
			if (err != nil) != (c.refuse != "") {
				t.Errorf("err = %v, want a refusal reported: %v", err, c.refuse != "")
			}
			var sent []int32
			for _, r := range fake.only("lw.sky.battle.chapter.box.reward") {
				sent = append(sent, r.Params.GetInt("chapter"))
			}
			if !slices.Equal(sent, c.wantBoxes) {
				t.Errorf("claimed chapters %v, want %v", sent, c.wantBoxes)
			}
		})
	}
}

// TestNewbieArenaRewardsStopOnRefusal: two reached rows (ids 1 and 2, in that order) on both arenas.
// The V1 arena refuses row 1, which stops that activity's claims, so its row 2 is not claimed; the
// V2 arena is another activity and still claims both. A state-1 row without an id names nothing to
// claim and is skipped.
func TestNewbieArenaRewardsStopOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "activity.arena.reward.preview", "activity.arena.v2.reward.preview":
			r := evOK()
			a := sfs.NewSFSArray()
			a.AddSFSObject(obj("state", 1, "targetRank", 200)) // no id
			a.AddSFSObject(obj("id", 1, "state", 1, "targetRank", 100))
			a.AddSFSObject(obj("id", 2, "state", 1, "targetRank", 50))
			r.PutSFSArray("achieveRewards", a)
			return r
		case "activity.arena.receive":
			if p.GetInt("id") == 1 {
				return evErr("E140001")
			}
		}
		return evOK()
	})
	err := runNewbieArenaRewards(conn, evInit(30, evActivity(100000), evActivity(100001)))
	if err == nil || !strings.Contains(err.Error(), "E140001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	var sent []string
	for _, r := range fake.requests() {
		if strings.HasSuffix(r.Cmd, "receive") {
			sent = append(sent, r.Cmd+" "+strconv.Itoa(int(r.Params.GetInt("id"))))
		}
	}
	want := []string{"activity.arena.receive 1", "activity.arena.v2.receive 1", "activity.arena.v2.receive 2"}
	if !slices.Equal(sent, want) {
		t.Errorf("claimed %q, want %q", sent, want)
	}
}
