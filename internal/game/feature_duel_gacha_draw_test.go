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

// duelGachaDrawSundayNow is a Sunday clock: 2026-10-11 12:00 UTC, 10:00 server time on the server
// day that init tomorrow (evTestTomorrow, 02:00 UTC) anchors, so GetNowWeekdayIndex is 7 and the
// Sunday tab opens the draw view. evTestNow is the Monday after the previous Sunday (2026-10-05).
var duelGachaDrawSundayNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// runDuelGachaDrawFake runs duel-gacha-draw on a Sunday (duelGachaDrawSundayNow) with no duel
// event read; see runDuelGachaDrawFakeAt.
func runDuelGachaDrawFake(t *testing.T, in *Init, info *sfs.SFSObject, draw func(n int) *sfs.SFSObject) ([]evReq, []time.Duration, error) {
	t.Helper()
	return runDuelGachaDrawFakeAt(t, duelGachaDrawSundayNow, in, info, nil, draw)
}

// runDuelGachaDrawFakeAt runs duel-gacha-draw at now against a fake server that answers the info
// read with info, hero.event.info.get {activityId: "70000"} (the duel's) with event, and the n-th
// draw (from 0) with draw(n). A nil event means no event read may be sent. It records the pauses
// between draws.
func runDuelGachaDrawFakeAt(t *testing.T, now time.Time, in *Init, info, event *sfs.SFSObject, draw func(n int) *sfs.SFSObject) ([]evReq, []time.Duration, error) {
	t.Helper()
	withEvNow(t, now)
	var slept []time.Duration
	saved := duelGachaDrawSleep
	duelGachaDrawSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { duelGachaDrawSleep = saved })
	n := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch {
		case cmd == duelGachaInfoCmd:
			return info
		case cmd == duelGachaDrawCmd:
			r := draw(n)
			n++
			return r
		case cmd == "hero.event.info.get" && p.GetString("activityId") == "70000" && event != nil:
			return event
		}
		return evErr("999999")
	})
	err := runDuelGachaDraw(conn, in)
	for _, r := range fake.requests() {
		switch {
		case r.Cmd == duelGachaInfoCmd, r.Cmd == duelGachaDrawCmd:
		case r.Cmd == "hero.event.info.get" && event != nil:
		default:
			t.Errorf("sent %s %v: only the gacha read, the draw and (on the Activity tab path) the duel's event read may be sent", r.Cmd, r.Params)
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
		"season 1 day 56": {duelGachaInit(duelGachaDrawSeason1Day(56)), duelGachaInfo(30_000_000, 50, 0, 1), nil},
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

// duelGachaDrawSeason1Day puts season 1 on its given day at duelGachaDrawSundayNow
// ((now - seasonStartTime) // 86400000 + 1, SeasonUtil.lua:308-316).
func duelGachaDrawSeason1Day(day int) func(raw *sfs.SFSObject) {
	return func(raw *sfs.SFSObject) {
		start := duelGachaDrawSundayNow.Add(-time.Duration(day-1) * 24 * time.Hour).UnixMilli()
		raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 1, "seasonStartTime", start))
	}
}

// duelGachaDrawDuel puts the Alliance Duel's init entry into init: id 55000 (ActId "55000",
// EnumType.lua:6301-6305) with activityid "70000", open from two days before to two days after
// evTestNow, edited by edit.
func duelGachaDrawDuel(edit func(entry *sfs.SFSObject)) func(raw *sfs.SFSObject) {
	return func(raw *sfs.SFSObject) {
		a := duelActivity()
		if edit != nil {
			edit(a)
		}
		evObject(raw, "activity").PutSFSObject("55000", a)
	}
}

// duelGachaDrawEvent is the duel's hero.event.info.get {activityId: "70000"} reply with the top
// level confirmed live (docs/alliance-duel.mdx, "The reply"): activityId "70000", type 15 and an
// eventList with today's entry, edited by edit.
func duelGachaDrawEvent(edit func(r *sfs.SFSObject)) *sfs.SFSObject {
	r := duelInfo(duelEntry(110000, "90402", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour)))
	r.PutUtfString("activityId", "70000")
	r.PutInt("type", 15)
	if edit != nil {
		edit(r)
	}
	return r
}

// TestDuelGachaDrawView: a draw needs a draw view the 1.0.370 client can open. The Sunday tab
// needs server weekday 7 (UIAllyDuelCtrl.lua:21-26; the date of UTC plus the server offset, here
// UTC-2 from init tomorrow at 02:00 UTC); the Activity tab needs the duel activity (55000) not
// finished and its event info filed (UIAllyDuelCtrl.lua:15-20). Monday to Saturday outside a
// running duel only the info read is sent. A failed event read stops with no draw.
func TestDuelGachaDrawView(t *testing.T) {
	const (
		infoCmd  = "al.duel.lottery.get.info"
		eventCmd = "hero.event.info.get"
		drawCmd  = "al.duel.lottery.lottery"
	)
	monday := evTestNow // 2026-10-05 12:00 UTC, 10:00 Monday server time
	sunday := duelGachaDrawSundayNow
	duel := func(edit func(entry *sfs.SFSObject)) *Init { return duelGachaInit(duelGachaDrawDuel(edit)) }
	noTomorrow := func(in *Init) *Init {
		in.Raw = duelWithoutKey(in.Raw, "tomorrow")
		return in
	}
	hq9 := func(in *Init) *Init {
		in.Buildings = []Building{NewTestBuilding(1, activityHQBuildingID, 9)} // the duel needs HQ 10
		return in
	}
	event := duelGachaDrawEvent(nil)
	for name, c := range map[string]struct {
		now     time.Time
		in      *Init
		event   *sfs.SFSObject
		sent    []string
		wantErr string
	}{
		"Monday, no duel activity: only the info read": {monday, duelGachaInit(nil), nil, []string{infoCmd}, ""},
		"Sunday, no duel activity: the Sunday tab":     {sunday, duelGachaInit(nil), nil, []string{infoCmd, drawCmd}, ""},
		"Monday, the duel with event info: the Activity tab": {monday, duel(nil), event,
			[]string{infoCmd, eventCmd, drawCmd}, ""},
		"Sunday with the duel open: no event read": {sunday, duel(func(e *sfs.SFSObject) {
			e.PutLong("endTime", sunday.Add(48*time.Hour).UnixMilli())
		}), event, []string{infoCmd, drawCmd}, ""},
		// The server day starts at 02:00 UTC: the weekday is the date of UTC-2.
		"Sunday 01:00 UTC is Saturday 23:00 server time": {time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC), duelGachaInit(nil), nil,
			[]string{infoCmd}, ""},
		"Monday 01:00 UTC is Sunday 23:00 server time": {time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC), duelGachaInit(nil), nil,
			[]string{infoCmd, drawCmd}, ""},
		// CheckIsSubmitting (weekday 7, first server hour) hides only the Activity tab.
		"Sunday 00:30 server time, submitting": {time.Date(2026, 10, 11, 2, 30, 0, 0, time.UTC), duelGachaInit(nil), nil,
			[]string{infoCmd, drawCmd}, ""},
		"no init tomorrow on a Sunday: weekday unknown": {sunday, noTomorrow(duelGachaInit(nil)), nil, []string{infoCmd}, ""},
		"no init tomorrow, the duel with event info":    {monday, noTomorrow(duel(nil)), event, []string{infoCmd, eventCmd, drawCmd}, ""},
		"the duel below its HQ level is not open":       {monday, hq9(duel(nil)), event, []string{infoCmd}, ""},
		"init finish true": {monday, duel(func(e *sfs.SFSObject) { e.PutBool("finish", true) }), event,
			[]string{infoCmd, eventCmd}, ""},
		"init finish 0, true in Lua": {monday, duel(func(e *sfs.SFSObject) { e.PutInt("finish", 0) }), event,
			[]string{infoCmd, eventCmd}, ""},
		"init finish false": {monday, duel(func(e *sfs.SFSObject) { e.PutBool("finish", false) }), event,
			[]string{infoCmd, eventCmd, drawCmd}, ""},
		// An SFS null is nil in Lua: no finish, and no replacement of the init entry's.
		"init finish null": {monday, duel(func(e *sfs.SFSObject) { e.PutValue("finish", sfs.SFSValue{Type: sfs.SFSNull}) }), event,
			[]string{infoCmd, eventCmd, drawCmd}, ""},
		"reply finish null keeps init finish true": {monday, duel(func(e *sfs.SFSObject) { e.PutBool("finish", true) }),
			duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutValue("finish", sfs.SFSValue{Type: sfs.SFSNull}) }), []string{infoCmd, eventCmd}, ""},
		"reply finish true": {monday, duel(nil), duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutBool("finish", true) }),
			[]string{infoCmd, eventCmd}, ""},
		"reply finish false replaces init finish true": {monday, duel(func(e *sfs.SFSObject) { e.PutBool("finish", true) }),
			duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutBool("finish", false) }), []string{infoCmd, eventCmd, drawCmd}, ""},
		"empty eventList": {monday, duel(nil), duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutSFSArray("eventList", sfs.NewSFSArray()) }),
			[]string{infoCmd, eventCmd}, ""},
		"no eventList":       {monday, duel(nil), duelWithoutKey(event, "eventList"), []string{infoCmd, eventCmd}, ""},
		"reply type 14":      {monday, duel(nil), duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutInt("type", 14) }), []string{infoCmd, eventCmd}, ""},
		"reply without type": {monday, duel(nil), duelWithoutKey(event, "type"), []string{infoCmd, eventCmd}, ""},
		"reply for activity 55000": {monday, duel(nil), duelGachaDrawEvent(func(r *sfs.SFSObject) { r.PutUtfString("activityId", "55000") }),
			[]string{infoCmd, eventCmd}, ""},
		"reply success, no data": {monday, duel(nil), evOK(), []string{infoCmd, eventCmd}, ""},
		"reply errorCode":        {monday, duel(nil), evErr("E200"), []string{infoCmd, eventCmd}, "E200"},
	} {
		reqs, _, err := runDuelGachaDrawFakeAt(t, c.now, c.in, duelGachaInfo(10_000_000, 50, 0, 1), c.event, duelGachaDrawSpending(10_000_000))
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: err %v, want none", name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err %v, want the %s failure", name, err, c.wantErr)
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

// TestDuelGachaDrawViewIsTheDuelActivity: the Activity tab reads the activity the client looks up
// by ActId "55000" (UIAllyDuelCtrl.lua:16; EnumType.lua:6301-6305), not any type-14 entry. Table
// activity has one type-14 row today, so the test adds a second for its run.
func TestDuelGachaDrawViewIsTheDuelActivity(t *testing.T) {
	const other = 55001
	if _, ok := activityTable[other]; ok {
		t.Fatalf("table activity has a row %d; pick another id", other)
	}
	activityTable[other] = activityRow{typ: 14, needHQ: 10}
	t.Cleanup(func() { delete(activityTable, other) })
	entry := evActivity(other)
	entry.PutUtfString("activityid", "70000")
	in := duelGachaInit(func(raw *sfs.SFSObject) { evObject(raw, "activity").PutSFSObject("55001", entry) })
	reqs, _, err := runDuelGachaDrawFakeAt(t, evTestNow, in, duelGachaInfo(10_000_000, 50, 0, 1), duelGachaDrawEvent(nil), duelGachaDrawSpending(10_000_000))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range reqs {
		got = append(got, r.Cmd)
	}
	if want := []string{"al.duel.lottery.get.info"}; !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v: a type-14 activity other than 55000 opens no Activity tab", got, want)
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
