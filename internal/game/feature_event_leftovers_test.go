package game

import (
	"slices"
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
		{"set, never rewarded", "1990-05-04", nil, true},
		{"set, rewarded false", "1990-05-04", false, true},
		{"set, rewarded true", "1990-05-04", true, false},
		{"set, rewarded 0 is truthy in Lua", "1990-05-04", int32(0), false},
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
