package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// duelGachaDrawReply is an al.duel.lottery.lottery reply as OnGachaMessageCallback reads it
// (AllyDuelScoreGachaManager.lua:182-205): the new score and protectNum, and rewardArr[] {pos,
// reward[]}.
func duelGachaDrawReply(score, protectNum int64) *sfs.SFSObject {
	r := duelObj("score", score, "protectNum", protectNum)
	arr := sfs.NewSFSArray()
	e := duelObj("pos", 2)
	rw := sfs.NewSFSArray()
	rw.AddSFSObject(duelObj("type", 7, "itemId", "200001", "num", 1))
	e.PutSFSArray("reward", rw)
	arr.AddSFSObject(e)
	r.PutSFSArray("rewardArr", arr)
	return r
}

// runDuelGachaDrawFake runs duel-gacha-draw against a fake server that answers the info read with
// info and the n-th draw (from 0) with draw(n). It records the pauses between draws.
func runDuelGachaDrawFake(t *testing.T, in *Init, info *sfs.SFSObject, draw func(n int) *sfs.SFSObject) ([]evReq, []time.Duration, error) {
	t.Helper()
	withEvNow(t, evTestNow)
	var slept []time.Duration
	saved := duelGachaDrawSleep
	duelGachaDrawSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { duelGachaDrawSleep = saved })
	n := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case duelGachaInfoCmd:
			return info
		case duelGachaDrawCmd:
			r := draw(n)
			n++
			return r
		}
		return evErr("999999")
	})
	err := runDuelGachaDraw(conn, in)
	for _, r := range fake.requests() {
		if r.Cmd != duelGachaInfoCmd && r.Cmd != duelGachaDrawCmd {
			t.Errorf("sent %s: only the gacha read and the draw may be sent", r.Cmd)
		}
	}
	return fake.requests(), slept, err
}

// duelGachaDrawSpending answers each draw with the balance 10,000,000 lower (DataConfig
// alliance_duel_league k6, table item 40413), starting from start, and the pity counter one up.
func duelGachaDrawSpending(start int64) func(n int) *sfs.SFSObject {
	return func(n int) *sfs.SFSObject {
		return duelGachaDrawReply(start-int64(n+1)*10_000_000, int64(n+1))
	}
}

func duelGachaDrawCount(reqs []evReq) int {
	n := 0
	for _, r := range reqs {
		if r.Cmd == "al.duel.lottery.lottery" {
			n++
		}
	}
	return n
}

func TestDuelGachaDrawRegistration(t *testing.T) {
	f, ok := featureRegistry["duel-gacha-draw"]
	if !ok || f.DefaultOn || f.Hold || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("duel-gacha-draw registered as %+v; want opt-in, not duel-held, static-only", f)
	}
}

// TestDuelGachaDrawSendsOnlyConfigIdAndNum: the request is AlDuelLotteryLotteryMessage's two Ints
// (AlDuelLotteryLotteryMessage.lua:6-7) and nothing else, num 1 (the one-draw button), and the
// draws stop when the score no longer pays 10,000,000.
func TestDuelGachaDrawSendsOnlyConfigIdAndNum(t *testing.T) {
	reqs, slept, err := runDuelGachaDrawFake(t, duelGachaInit(nil), duelGachaInfo(25_000_000, 50, 0, 1), duelGachaDrawSpending(25_000_000))
	if err != nil {
		t.Fatal(err)
	}
	var cmds []string
	for _, r := range reqs {
		cmds = append(cmds, r.Cmd)
	}
	want := []string{"al.duel.lottery.get.info", "al.duel.lottery.lottery", "al.duel.lottery.lottery"}
	if !slices.Equal(cmds, want) {
		t.Fatalf("sent %v, want %v (25,000,000 score pays two draws)", cmds, want)
	}
	for _, r := range reqs[1:] {
		var keys []string
		for _, k := range r.Params.Keys() {
			if k != "_id" { // the conn's request id
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"configId", "num"}) {
			t.Errorf("draw params %v, want exactly configId and num (no currency or item)", keys)
		}
		for k, v := range map[string]int32{"configId": 3, "num": 1} {
			got, _ := r.Params.Get(k)
			if got.Type != sfs.SFSInt || got.Val != v {
				t.Errorf("draw %s = %v (type %d), want Int %d", k, got.Val, got.Type, v)
			}
		}
	}
	if !slices.Equal(slept, []time.Duration{time.Second}) {
		t.Errorf("paused %v, want one 1 s pause between the two draws", slept)
	}
}

func TestDuelGachaDrawPaysOnlyWithTheScore(t *testing.T) {
	for name, c := range map[string]struct {
		score int64
		draws int
	}{
		"9,999,999 pays none":       {9_999_999, 0},
		"10,000,000 pays one":       {10_000_000, 1},
		"19,999,999 pays one":       {19_999_999, 1},
		"35,000,000 pays three":     {35_000_000, 3},
		"zero":                      {0, 0},
		"1,000,000,000 is capped":   {1_000_000_000, 50},
		"500,000,000 is exactly 50": {500_000_000, 50},
	} {
		reqs, _, err := runDuelGachaDrawFake(t, duelGachaInit(nil), duelGachaInfo(c.score, 50, 0, 1), duelGachaDrawSpending(c.score))
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := duelGachaDrawCount(reqs); got != c.draws {
			t.Errorf("%s: %d draws, want %d", name, got, c.draws)
		}
	}
}

// TestDuelGachaDrawFollowsTheReplyBalance: each reply's score is the balance the next draw is
// checked against, not the info score minus what the run thinks it spent.
func TestDuelGachaDrawFollowsTheReplyBalance(t *testing.T) {
	// 40,000,000 would pay four draws, but the first reply leaves 9,000,000.
	reqs, _, err := runDuelGachaDrawFake(t, duelGachaInit(nil), duelGachaInfo(40_000_000, 50, 0, 1), func(n int) *sfs.SFSObject {
		return duelGachaDrawReply(9_000_000, 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := duelGachaDrawCount(reqs); got != 1 {
		t.Errorf("%d draws, want 1: the reply's 9,000,000 pays no second draw", got)
	}
	// A reply balance above the info read's never adds draws beyond floor(info score / 10,000,000).
	reqs, _, err = runDuelGachaDrawFake(t, duelGachaInit(nil), duelGachaInfo(20_000_000, 50, 0, 1), func(n int) *sfs.SFSObject {
		return duelGachaDrawReply(int64(19_000_000-n), 1)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := duelGachaDrawCount(reqs); got != 2 {
		t.Errorf("%d draws, want 2 (what the info read's 20,000,000 pays)", got)
	}
}

func TestDuelGachaDrawStops(t *testing.T) {
	for name, c := range map[string]struct {
		draw    func(n int) *sfs.SFSObject
		draws   int
		wantErr string
	}{
		"errorCode on the second draw": {func(n int) *sfs.SFSObject {
			if n == 1 {
				return evErr("E100")
			}
			return duelGachaDrawReply(20_000_000, 1)
		}, 2, "E100"},
		"no score in the reply": {func(int) *sfs.SFSObject {
			return duelWithoutKey(duelGachaDrawReply(20_000_000, 1), "score")
		}, 1, ""},
		"balance did not fall": {func(int) *sfs.SFSObject { return duelGachaDrawReply(30_000_000, 1) }, 1, ""},
		"balance rose":         {func(int) *sfs.SFSObject { return duelGachaDrawReply(31_000_000, 1) }, 1, ""},
	} {
		reqs, _, err := runDuelGachaDrawFake(t, duelGachaInit(nil), duelGachaInfo(30_000_000, 50, 0, 1), c.draw)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: err %v, want none", name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err %v, want the %s failure", name, err, c.wantErr)
		}
		if got := duelGachaDrawCount(reqs); got != c.draws {
			t.Errorf("%s: %d draws, want %d", name, got, c.draws)
		}
	}
}

func TestDuelGachaDrawGates(t *testing.T) {
	never := func(int) *sfs.SFSObject { return evErr("999999") }
	for name, c := range map[string]struct {
		in   *Init
		info *sfs.SFSObject
		sent []string
	}{
		"no configId": {duelGachaInit(nil), duelWithoutKey(duelGachaInfo(30_000_000, 50, 0, 1), "configId"), []string{"al.duel.lottery.get.info"}},
		"configId 0":  {duelGachaInit(nil), duelGachaInfoWith(duelGachaInfo(30_000_000, 50, 0, 1), "configId", 0), []string{"al.duel.lottery.get.info"}},
		"no score":    {duelGachaInit(nil), duelWithoutKey(duelGachaInfo(30_000_000, 50, 0, 1), "score"), []string{"al.duel.lottery.get.info"}},
		"score not a number": {duelGachaInit(nil), duelGachaInfoWith(duelGachaInfo(30_000_000, 50, 0, 1), "score", "lots"),
			[]string{"al.duel.lottery.get.info"}},
		"effect 92019 0":  {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("effect", duelObj("92019", 0.0)) }), duelGachaInfo(30_000_000, 50, 0, 1), []string{"al.duel.lottery.get.info"}},
		"no effect":       {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("effect", sfs.NewSFSObject()) }), duelGachaInfo(30_000_000, 50, 0, 1), []string{"al.duel.lottery.get.info"}},
		"no alliance":     {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("alliance", duelObj("uid", "")) }), duelGachaInfo(30_000_000, 50, 0, 1), nil},
		"switch off":      {duelGachaInit(func(raw *sfs.SFSObject) { raw.PutSFSObject("dataConfig", duelObj("alliance_duel_zhuanpan", 0)) }), duelGachaInfo(30_000_000, 50, 0, 1), nil},
		"season 1 day 56": {duelGachaInit(duelGachaSeason1(55)), duelGachaInfo(30_000_000, 50, 0, 1), nil},
	} {
		reqs, _, err := runDuelGachaDrawFake(t, c.in, c.info, never)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		var got []string
		for _, r := range reqs {
			got = append(got, r.Cmd)
		}
		if !slices.Equal(got, c.sent) {
			t.Errorf("%s: sent %v, want %v", name, got, c.sent)
		}
	}
}

// TestDuelGachaDrawReasons: the logged reason names what the live reply lacked; a reply with no
// score is told apart from a score of 0.
func TestDuelGachaDrawReasons(t *testing.T) {
	in := duelGachaInit(nil)
	for name, c := range map[string]struct {
		info *sfs.SFSObject
		want string
	}{
		"no score":    {duelWithoutKey(duelGachaInfo(30_000_000, 50, 0, 1), "score"), "no score"},
		"score 0":     {duelGachaInfo(0, 50, 0, 1), "the score 0 pays for no draw (10000000 a draw)"},
		"no configId": {duelWithoutKey(duelGachaInfo(30_000_000, 50, 0, 1), "configId"), "no usable configId"},
		"draws":       {duelGachaInfo(10_000_000, 50, 0, 1), ""},
	} {
		_, hasScore := claimInt(c.info, "score")
		got := duelGachaDrawDue(in, duelGachaStateOf(c.info), hasScore)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: reason %q, want %q", name, got, c.want)
		}
	}
}

// TestDuelGachaDrawStopReasons: a draw reply without a score stops the run as unknown, not as a
// balance of 0, and a balance that did not fall stops it too.
func TestDuelGachaDrawStopReasons(t *testing.T) {
	for name, c := range map[string]struct {
		before, after int64
		hasAfter      bool
		want          string
	}{
		"no score":      {30_000_000, 0, false, "carries no score"},
		"did not fall":  {30_000_000, 30_000_000, true, "did not fall after a draw (30000000 -> 30000000)"},
		"rose":          {30_000_000, 31_000_000, true, "did not fall"},
		"fell one draw": {30_000_000, 20_000_000, true, ""},
		"fell to 0":     {10_000_000, 0, true, ""},
	} {
		got := duelGachaDrawStop(c.before, c.after, c.hasAfter)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("%s: reason %q, want %q", name, got, c.want)
		}
	}
}

func TestDuelGachaDrawInfoErrorStops(t *testing.T) {
	reqs, _, err := runDuelGachaDrawFake(t, duelGachaInit(nil), evErr("E100"), duelGachaDrawSpending(30_000_000))
	if err == nil || !strings.Contains(err.Error(), "E100") {
		t.Errorf("err = %v, want the E100 failure", err)
	}
	if len(reqs) != 1 {
		t.Errorf("sent %v, want only the info read", reqs)
	}
}

func TestDuelGachaDrawNoInit(t *testing.T) {
	if err := runDuelGachaDraw(nil, nil); err != nil {
		t.Errorf("nil init: %v", err)
	}
}

// duelGachaInfoWith returns o with key replaced by v (an int or a string).
func duelGachaInfoWith(o *sfs.SFSObject, key string, v any) *sfs.SFSObject {
	r := duelWithoutKey(o, key)
	switch x := v.(type) {
	case int:
		r.PutInt(key, int32(x))
	case string:
		r.PutUtfString(key, x)
	}
	return r
}
