package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// tacticalInit is an Init in season `season`, started startAgo before evTestNow, with the given
// init cardCollects entries.
func tacticalInit(season int32, startAgo time.Duration, collects ...*sfs.SFSObject) *Init {
	in := evInit(30)
	p := sfs.NewSFSObject()
	p.PutInt("seasonId", season)
	p.PutLong("seasonStartTime", evTestNow.Add(-startAgo).UnixMilli())
	in.Raw.PutSFSObject("playerServerSeasonInfo", p)
	if len(collects) > 0 {
		a := sfs.NewSFSArray()
		for _, c := range collects {
			a.AddSFSObject(c)
		}
		in.Raw.PutSFSArray("cardCollects", a)
	}
	return in
}

func tacticalCollect(cfgID, cardType int32, rewardStr string) *sfs.SFSObject {
	c := sfs.NewSFSObject()
	c.PutInt("cfgId", cfgID)
	c.PutInt("cardType", cardType)
	c.PutUtfString("rewardStr", rewardStr)
	return c
}

func TestTacticalCardsOpenGate(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name   string
		season int32
		ago    time.Duration
		open   bool
	}{
		{"season 3", 3, 90 * 24 * time.Hour, false},
		{"season 4 day 1", 4, 23 * time.Hour, false},
		{"season 4 day 2", 4, 25 * time.Hour, true},
		{"season 6 day 1", 6, time.Hour, true},
	} {
		if _, why := tacticalCardsOpen(tacticalInit(c.season, c.ago)); (why == "") != c.open {
			t.Errorf("%s: open = %v (%s), want %v", c.name, why == "", why, c.open)
		}
	}
	in := evInit(30)
	p := sfs.NewSFSObject()
	p.PutInt("seasonId", 4)
	in.Raw.PutSFSObject("playerServerSeasonInfo", p)
	if _, why := tacticalCardsOpen(in); why == "" {
		t.Error("season 4 without seasonStartTime must stay closed (the client's MANY_YEARS_LATER)")
	}
}

func TestTacticalBoxPointsClaimsWhenTheSeasonScoreReachesAnUnclaimedRow(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name     string
		season   int32
		score    int32
		rewarded []int32
		claim    bool
	}{
		{"2100 points, 501 claimed", 5, 2100, []int32{501}, true},
		{"2100 points, both claimed", 5, 2100, []int32{501, 502}, false},
		{"another season's rows claimed only", 6, 1500, []int32{501}, true},
		{"below the first row", 7, 999, nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				r := evOK()
				if cmd == tacticalScoreInfoCmd {
					r.PutInt("score", c.score)
					r.PutIntArray("rewardedList", c.rewarded)
				}
				return r
			})
			if err := runTacticalBoxPoints(conn, tacticalInit(c.season, 30*24*time.Hour)); err != nil {
				t.Fatal(err)
			}
			want := []string{tacticalScoreInfoCmd}
			if c.claim {
				want = append(want, tacticalScoreRewardCmd)
			}
			if got := fake.cmds(); !slices.Equal(got, want) {
				t.Errorf("sent %v, want %v", got, want)
			}
		})
	}
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runTacticalBoxPoints(conn, tacticalInit(4, time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("season 4 day 1 sent %v, want nothing", got)
	}
}

func TestTacticalStagesDueUsesTheParsedIndexAndTheActivatedCount(t *testing.T) {
	collects := []*sfs.SFSObject{
		// type 1 (cfg 50101 needs 3, 5, 7, 9, 11): stage 0 received, 1 reached at state 0, 2 not
		// reached, 3 marked available by the server
		tacticalCollect(50101, 1, "0;2|1;0|2;0|3;1|4;0"),
		// type 2 listed out of order: stage 2 (needs 12) reached with 12 cards
		tacticalCollect(50201, 2, "2;0|0;2|1;2"),
		// a cfgId newer than the table: only the server's state 1 counts
		tacticalCollect(90101, 3, "0;0|1;1"),
	}
	got := tacticalStagesDue(collects, map[int64]int64{1: 6, 2: 12, 3: 99})
	want := []tacticalStage{
		{cfgID: 50101, cardType: 1, index: 1, state: 0},
		{cfgID: 50101, cardType: 1, index: 3, state: 1},
		{cfgID: 50201, cardType: 2, index: 2, state: 0},
		{cfgID: 90101, cardType: 3, index: 1, state: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("due = %+v\nwant  %+v", got, want)
	}
}

func TestTacticalActiveCardsCountsDistinctIDs(t *testing.T) {
	r := evOK()
	a := sfs.NewSFSArray()
	for typ, cards := range map[int32]string{1: "101;102;102;103;", 2: ""} {
		e := sfs.NewSFSObject()
		e.PutInt("cardType", typ)
		e.PutUtfString("cards", cards)
		a.AddSFSObject(e)
	}
	r.PutSFSArray("collectCards", a)
	got := tacticalActiveCards(r)
	if got[1] != 3 || got[2] != 0 {
		t.Errorf("active = %v, want type 1: 3, type 2: 0", got)
	}
}

// TestTacticalActiveCardsMergesEntriesOfOneType: UpdateCardBookData puts every entry's cards into
// one set per cardType, keyed by tonumber(card) (TacticalCardDataManager.lua:156-176), so two
// entries of type 1 listing 101;102 and 102;103 make 3 active cards, and "07" is card 7.
func TestTacticalActiveCardsMergesEntriesOfOneType(t *testing.T) {
	r := evOK()
	a := sfs.NewSFSArray()
	for _, e := range []struct {
		typ   int32
		cards string
	}{{1, "101;102"}, {1, "102;103"}, {4, "7;07"}} {
		o := sfs.NewSFSObject()
		o.PutInt("cardType", e.typ)
		o.PutUtfString("cards", e.cards)
		a.AddSFSObject(o)
	}
	r.PutSFSArray("collectCards", a)
	got := tacticalActiveCards(r)
	if got[1] != 3 || got[4] != 1 {
		t.Errorf("active = %v, want type 1: 3 (merged), type 4: 1", got)
	}
}

func TestTacticalCollectionClaimsAndStopsOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	in := tacticalInit(5, 10*24*time.Hour,
		tacticalCollect(50101, 1, "0;1|1;0"), // 0 available; 1 needs 5 cards
		tacticalCollect(50301, 3, "0;0"),     // needs 1 card
	)
	cards := func() *sfs.SFSObject {
		r := evOK()
		a := sfs.NewSFSArray()
		for typ, ids := range map[int32]string{1: "1;2;3;4;5", 3: "9"} {
			e := sfs.NewSFSObject()
			e.PutInt("cardType", typ)
			e.PutUtfString("cards", ids)
			a.AddSFSObject(e)
		}
		r.PutSFSArray("collectCards", a)
		return r
	}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case tacticalCollectListCmd:
			return cards()
		case tacticalCollectRewardCmd:
			if p.GetInt("cfgId") == 50301 {
				return evErr("E200001")
			}
		}
		return evOK()
	})
	err := runTacticalCollection(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E200001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	var sent [][3]int32
	for _, r := range fake.only(tacticalCollectRewardCmd) {
		for _, k := range []string{"index", "cardType", "cfgId"} {
			if v, _ := r.Params.Get(k); v.Type != sfs.SFSInt {
				t.Errorf("%s type = %d, want Int", k, v.Type)
			}
		}
		sent = append(sent, [3]int32{r.Params.GetInt("cfgId"), r.Params.GetInt("cardType"), r.Params.GetInt("index")})
	}
	want := [][3]int32{{50101, 1, 0}, {50101, 1, 1}, {50301, 3, 0}}
	if !slices.Equal(sent, want) {
		t.Errorf("claimed %v, want %v", sent, want)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runTacticalCollection(conn, tacticalInit(5, 10*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("no cardCollects sent %v, want nothing", got)
	}
}
