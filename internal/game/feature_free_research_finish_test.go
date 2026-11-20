package game

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// freeResearchSetup describes one free-research-finish run: the queues, init effect["30100"]
// (nil: absent) and user.gold (nil: absent); duel, when set, adds an alliance and an open duel
// activity whose hero.event.info.get {activityId:"70000"} reply it returns.
type freeResearchSetup struct {
	queues []timerQueue
	effect any
	gold   any
	duel   func(now time.Time) *sfs.SFSObject
	reply  func(p *sfs.SFSObject) *sfs.SFSObject // queue.ccd.m.new; nil: freeResearchOK
}

// freeResearchOK is a queue.ccd.m.new reply whose queue now ends at once and whose remainGold is
// unchanged (QueueCcsMNewHandle reads queue and queue.remainGold, QueueDataManager.lua:495-513).
func freeResearchOK(p *sfs.SFSObject) *sfs.SFSObject {
	q := sfs.NewSFSObject()
	q.PutLong("uuid", p.GetLong("qUUID"))
	q.PutInt("type", 6)
	q.PutLong("uT", time.Now().UnixMilli())
	q.PutLong("remainGold", 1000)
	r := sfs.NewSFSObject()
	r.PutSFSObject("queue", q)
	return r
}

func runFreeResearch(t *testing.T, now time.Time, c freeResearchSetup) (sent []*sfs.SFSObject, duelReads int, err error) {
	t.Helper()
	freeResearchFinishTripped.Store(false)
	t.Cleanup(func() { freeResearchFinishTripped.Store(false) })
	in := timerInit{queues: c.queues, soldiers: -1}.build()
	if c.effect != nil {
		eff := sfs.NewSFSObject()
		switch v := c.effect.(type) {
		case float64:
			eff.PutDouble("30100", v)
		case int32:
			eff.PutInt("30100", v)
		case string:
			eff.PutUtfString("30100", v)
		}
		in.Raw.PutSFSObject("effect", eff)
	}
	if c.duel != nil {
		in = duelHoldInit(t, in, now, 25, true, now.Add(-12*time.Hour), now.Add(12*time.Hour))
	} else {
		withEvNow(t, now)
	}
	user := sfs.NewSFSObject()
	if c.duel != nil {
		user.PutUtfString("allianceId", "7f3a")
	}
	if g, ok := c.gold.(int64); ok {
		user.PutLong("gold", g)
	}
	in.Raw.PutSFSObject("user", user)
	reply := c.reply
	if reply == nil {
		reply = freeResearchOK
	}
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.event.info.get":
			if p.GetString("activityId") == "70000" && c.duel != nil {
				return c.duel(now)
			}
			return evOK()
		case "queue.ccd.m.new":
			return reply(p)
		}
		return rewardOK()
	})
	err = runFreeResearchFinish(conn, in)
	for _, r := range fake.requests() {
		switch r.Cmd {
		case "hero.event.info.get":
			duelReads++
		case "queue.ccd.m.new":
			sent = append(sent, r.Params)
		default:
			t.Errorf("unexpected request %s %v", r.Cmd, r.Params)
		}
	}
	return sent, duelReads, err
}

func sentQueues(sent []*sfs.SFSObject) []int64 {
	var out []int64
	for _, p := range sent {
		out = append(out, p.GetLong("qUUID"))
	}
	return out
}

// G37: a working research queue (type 6) whose remaining time is within effect 30100 seconds gets
// queue.ccd.m.new {qUUID: Long, itemIDs: ""} (BuildBubbleManager.lua:4123-4128;
// QueueCcdMNewMessage.lua:7-10); nothing else does (QueueDataManager.lua:572-590).
func TestFreeResearchFinishSendsInsideTheFreeTime(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Hour)
	queues := []timerQueue{
		{uuid: 21, qType: 6, start: start, end: now.Add(2 * time.Minute)},                // inside 300 s: send
		{uuid: 22, qType: 6, start: start, end: now.Add(10 * time.Minute)},               // outside
		{uuid: 23, qType: 3, start: start, end: now.Add(time.Minute)},                    // hospital: never
		{uuid: 24, qType: 6, start: start, end: now.Add(-time.Minute)},                   // finished: finished-timers' job
		{uuid: 26, qType: 6, start: start, end: now.Add(280 * time.Second)},              // within 300 s but not 30 s inside
		{uuid: 27, qType: 6, start: start, end: now.Add(4*time.Minute + 10*time.Second)}, // 250 s: send
	}
	sent, reads, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := sentQueues(sent); !slices.Equal(got, []int64{21, 27}) {
		t.Errorf("free finishes for queues %v, want [21 27]", got)
	}
	if reads != 0 {
		t.Errorf("duel reads = %d, want none outside an alliance", reads)
	}
	if len(sent) > 0 {
		p := sent[0]
		q, _ := p.Get("qUUID")
		items, _ := p.Get("itemIDs")
		// _id is the request id the session adds to every request.
		keys := slices.DeleteFunc(slices.Clone(p.Keys()), func(k string) bool { return k == "_id" })
		if q.Type != sfs.SFSLong || items.Type != sfs.SFSUtfString || p.GetString("itemIDs") != "" ||
			!slices.Equal(keys, []string{"qUUID", "itemIDs"}) {
			t.Errorf("request = %v (keys %v), want exactly {qUUID: Long, itemIDs: UtfString \"\"}", p, p.Keys())
		}
	}
}

// Never without the effect: init effect["30100"] absent, 0, negative, not a number or not finite
// sends nothing and says so; an Int is read.
func TestFreeResearchFinishNeedsTheEffect(t *testing.T) {
	now := time.Now()
	queues := []timerQueue{{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: now.Add(time.Minute)}}
	for name, c := range map[string]struct {
		effect any
		want   []int64
	}{
		"absent":       {nil, nil},
		"zero":         {0.0, nil},
		"negative":     {-60.0, nil},
		"not a number": {"300s", nil},
		"NaN":          {math.NaN(), nil},
		"infinite":     {math.Inf(1), nil},
		"Int seconds":  {int32(300), []int64{21}},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureRewardLogs(t)
			sent, _, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: c.effect, gold: int64(1000)})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := sentQueues(sent); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
			if c.want == nil && !strings.Contains(logs.String(), "no research free time") {
				t.Errorf("a missing free time must be logged as such:\n%s", logs.String())
			}
		})
	}
}

// The user's ruling covers the free research finish too: held like finished-timers' research on a
// day that doesn't score research power (type 6), sent on Age of Science and Total Mobilization,
// and sent when there is no duel or the duel can't be read.
func TestFreeResearchFinishDuelHold(t *testing.T) {
	day := func(theme int32) func(now time.Time) *sfs.SFSObject {
		return func(now time.Time) *sfs.SFSObject {
			return duelInfo(duelEntry(theme, heroEventDuelScores[theme], now.Add(-time.Hour), now.Add(time.Hour)))
		}
	}
	for name, c := range map[string]struct {
		duel func(now time.Time) *sfs.SFSObject
		want []int64
	}{
		"Mon radar":          {day(110000), nil},
		"Tue base expansion": {day(110001), nil},
		"Wed science":        {day(110002), []int64{21}},
		"Thu heroes":         {day(110003), nil},
		"Fri mobilization":   {day(110004), []int64{21}},
		"Sat enemy buster":   {day(110005), nil},
		"Sun rest day": {func(now time.Time) *sfs.SFSObject {
			return duelInfo(duelEntry(110005, heroEventDuelScores[110005], now.Add(-25*time.Hour), now.Add(-time.Hour)))
		}, nil},
		"duel unreadable": {func(time.Time) *sfs.SFSObject { return evOK() }, []int64{21}},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			queues := []timerQueue{{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: now.Add(time.Minute)}}
			logs := captureRewardLogs(t)
			sent, reads, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), duel: c.duel})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if got := sentQueues(sent); !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
			if reads != 1 {
				t.Errorf("duel reads = %d, want 1", reads)
			}
			if c.want == nil && !strings.Contains(logs.String(), "Age of Science or Total Mobilization") {
				t.Errorf("a held finish must log the days it waits for:\n%s", logs.String())
			}
		})
	}
}

// The gold check: no init user.gold, nothing sent; a reply whose remainGold falls below it stops the
// feature with an error (and in-process, the next run too); an errorCode stops the run.
func TestFreeResearchFinishGoldCheckAndErrors(t *testing.T) {
	now := time.Now()
	start := now.Add(-time.Hour)
	queues := []timerQueue{
		{uuid: 21, qType: 6, start: start, end: now.Add(time.Minute)},
		{uuid: 22, qType: 6, start: start, end: now.Add(2 * time.Minute)},
	}
	sent, _, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0})
	if err != nil || len(sent) != 0 {
		t.Errorf("no user.gold: err %v, sent %v, want nothing sent", err, sentQueues(sent))
	}

	drop := func(p *sfs.SFSObject) *sfs.SFSObject {
		r := freeResearchOK(p)
		evObject(r, "queue").PutLong("remainGold", 990)
		return r
	}
	sent, _, err = runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), reply: drop})
	if err == nil || !strings.Contains(err.Error(), "diamonds dropped from 1000 to 990") {
		t.Errorf("gold drop: err = %v, want the drop", err)
	}
	if got := sentQueues(sent); !slices.Equal(got, []int64{21}) {
		t.Errorf("gold drop: sent %v, want only the first", got)
	}
	if !freeResearchFinishTripped.Load() {
		t.Error("gold drop: the feature must stay stopped for the rest of the process")
	} else if err := runFreeResearchFinish(nil, &Init{}); err == nil {
		t.Error("a run after the gold drop must refuse")
	}

	topGold := func(p *sfs.SFSObject) *sfs.SFSObject {
		r := freeResearchOK(p)
		r.PutLong("gold", 999)
		return r
	}
	if _, _, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), reply: topGold}); err == nil {
		t.Error("a top-level gold below init user.gold must stop the feature")
	}

	// E100173 ("Completed", tip 170008, QueueDataManager.lua:530-531) is benign for queue.ccd.m.new
	// because duel-speedups registers it; E000001 is not. Both stop the run.
	for _, code := range []string{"E100173", "E000001"} {
		refused := func(*sfs.SFSObject) *sfs.SFSObject { return rewardErr(code) }
		sent, _, err = runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), reply: refused})
		if err == nil || !strings.Contains(err.Error(), code) {
			t.Errorf("%s reply: err = %v, want an error naming it", code, err)
		}
		if got := sentQueues(sent); !slices.Equal(got, []int64{21}) {
			t.Errorf("%s reply: sent %v, want only the first", code, got)
		}
	}

	sent, _, err = runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000)})
	if err != nil || !slices.Equal(sentQueues(sent), []int64{21, 22}) {
		t.Errorf("unchanged gold: err %v, sent %v, want both", err, sentQueues(sent))
	}
}

// A gold drop is a trip source of the shared tripwire file (tripwire.go): it is persisted to
// <config>.tripwire.json with the feature, the command and the drop. While the file exists a
// later run, a new cron process with nothing in memory, refuses and sends nothing, not even the
// duel read. Once the operator clears the file, the next run sends again.
func TestFreeResearchFinishTripwirePersistsAcrossRuns(t *testing.T) {
	path := tripwireTemp(t)
	now := time.Now()
	queues := []timerQueue{{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: now.Add(time.Minute)}}
	drop := func(p *sfs.SFSObject) *sfs.SFSObject {
		r := freeResearchOK(p)
		evObject(r, "queue").PutLong("remainGold", 990)
		return r
	}

	// Run 1: the reply's queue.remainGold (QueueDataManager.lua:495-513) is below init user.gold.
	sent, _, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), reply: drop})
	if err == nil || errors.Is(err, errTripwire) || !slices.Equal(sentQueues(sent), []int64{21}) {
		t.Fatalf("run 1: err %v, sent %v; want the drop to stop the feature after queue 21", err, sentQueues(sent))
	}
	if rec := readTripwire(t, path); rec.Feature != "free-research-finish" || rec.Command != "queue.ccd.m.new" ||
		!strings.Contains(rec.Reason, "dropped from 1000 to 990") || rec.Time.IsZero() {
		t.Errorf("record %+v, want the feature, its command, the drop and a time", rec)
	}

	// Run 2, a new process. tripwireRestart doesn't know this feature's in-process flag, so it is
	// cleared here too: only the file stops the run. Wednesday (110002) scores research power, so
	// without the file the run would read the duel and send.
	tripwireRestart()
	freeResearchFinishTripped.Store(false)
	wed := func(now time.Time) *sfs.SFSObject {
		return duelInfo(duelEntry(110002, heroEventDuelScores[110002], now.Add(-time.Hour), now.Add(time.Hour)))
	}
	logs := captureRewardLogs(t)
	sent, reads, err := runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), duel: wed})
	if !errors.Is(err, errTripwire) || !strings.Contains(err.Error(), path) {
		t.Errorf("run 2: err = %v, want the tripwire file named", err)
	}
	if len(sent) != 0 || reads != 0 {
		t.Errorf("run 2: sent %v and %d duel reads while the tripwire file exists, want nothing", sentQueues(sent), reads)
	}
	if !strings.Contains(logs.String(), "free-research-finish: not spending while the diamond tripwire file exists") {
		t.Errorf("run 2 must Warn about the tripwire file:\n%s", logs.String())
	}

	// The operator clears it: the next run reads the duel and sends again.
	if removed, err := ClearTripwire(path); !removed || err != nil {
		t.Fatalf("clear: %v, %v", removed, err)
	}
	tripwireRestart()
	sent, reads, err = runFreeResearch(t, now, freeResearchSetup{queues: queues, effect: 300.0, gold: int64(1000), duel: wed})
	if err != nil || !slices.Equal(sentQueues(sent), []int64{21}) || reads != 1 {
		t.Errorf("after the clear: err %v, sent %v, %d duel reads; want queue 21 after one duel read", err, sentQueues(sent), reads)
	}
}

func TestFreeResearchFinishRegistration(t *testing.T) {
	f, ok := featureRegistry["free-research-finish"]
	if !ok || f.DefaultOn || f.Hold || len(f.Duel) != 0 ||
		!strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("free-research-finish: registered %v, DefaultOn %v, Hold %v, Duel %v, Summary %q",
			ok, f.DefaultOn, f.Hold, f.Duel, f.Summary)
	}
}
