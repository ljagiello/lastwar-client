package game

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// radarNoReply, returned by a startRadarFake reply func, leaves the request unanswered.
var radarNoReply = sfs.NewSFSObject()

// detectEv builds one get.detect.info event, unstarted-expiry 4 h from now.
func detectEv(uuid, eventID, state int64) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutLong("uuid", uuid)
	o.PutInt("eventId", int32(eventID))
	o.PutInt("state", int32(state))
	o.PutLong("startTime", time.Now().Add(-4*time.Hour).UnixMilli())
	o.PutLong("endTime", time.Now().Add(4*time.Hour).UnixMilli())
	return o
}

// detectReply builds a get.detect.info reply.
func detectReply(level, eventNum int64, events ...*sfs.SFSObject) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	d := sfs.NewSFSObject()
	d.PutInt("level", int32(level))
	d.PutInt("eventNum", int32(eventNum))
	d.PutLong("nextRefreshTime", evTestNow.Add(30*time.Minute).UnixMilli()) // within the fakes' duel day
	r.PutSFSObject("detectInfo", d)
	arr := sfs.NewSFSArray()
	for _, e := range events {
		arr.AddSFSObject(e)
	}
	r.PutSFSArray("events", arr)
	return r
}

// placedReply answers a batch placement with every listed uuid in state 0, except skip.
func placedReply(p *sfs.SFSObject, skip ...int64) *sfs.SFSObject {
	arr := sfs.NewSFSArray()
	for _, s := range strings.Split(p.GetString("uuidList"), "|") {
		uuid, _ := claimIntValue(s)
		if slices.Contains(skip, uuid) {
			continue
		}
		e := sfs.NewSFSObject()
		e.PutLong("uuid", uuid)
		e.PutInt("state", radarStateNotFinish)
		e.PutInt("pointId", 123456)
		arr.AddSFSObject(e)
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("array", arr)
	return r
}

// radarTestInit is an init with an open duel, Quick Execute on, an alliance and stamina
// (lastStaminaTime now, so no regeneration); stamina < 0 leaves playerInfo out.
func radarTestInit(stamina int64, inAlliance bool) *Init {
	in := evInit(30, duelActivity())
	dc := sfs.NewSFSObject()
	dc.PutUtfString(radarQuickSwitch, "1")
	in.Raw.PutSFSObject("dataConfig", dc)
	if stamina >= 0 {
		p := sfs.NewSFSObject()
		p.PutInt("stamina", int32(stamina))
		p.PutLong("lastStaminaTime", time.Now().UnixMilli())
		in.Raw.PutSFSObject("playerInfo", p)
	}
	if inAlliance {
		a := sfs.NewSFSObject()
		a.PutUtfString("uid", "alliance-1")
		in.Raw.PutSFSObject("alliance", a)
	}
	return in
}

// startRadarFake serves today's duel entries and the radar commands: get.detect.info answers
// infos in turn (the last one repeats), a batch placement places everything, and reply (when it
// returns non-nil) overrides any other answer; everything else gets a plain success.
func startRadarFake(t *testing.T, duel []*sfs.SFSObject, infos []*sfs.SFSObject, reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	withEvNow(t, evTestNow)
	client, server := session.NewPipeGameConnPair(t)
	f := &evFake{}
	reads := 0
	go func() {
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, evReq{msg.Cmd, msg.Params})
			f.mu.Unlock()
			var r *sfs.SFSObject
			if reply != nil {
				r = reply(msg.Cmd, msg.Params)
			}
			if r == nil {
				switch {
				case msg.Cmd == "hero.event.info.get" && msg.Params.GetString("activityId") == "70000":
					r = duelInfo(duel...)
				case msg.Cmd == radarInfoCmd:
					r = infos[min(reads, len(infos)-1)]
					reads++
				case msg.Cmd == radarPlaceCmd:
					r = placedReply(msg.Params)
				default:
					r = evOK()
				}
			}
			if r == radarNoReply {
				continue
			}
			if err := server.SendExtension(msg.Cmd, r); err != nil {
				return
			}
		}
	}()
	return client, f
}

// withRadarSleep records each fake-march wait and how many requests the server had seen by then.
func withRadarSleep(t *testing.T, f **evFake) *[][2]int64 {
	t.Helper()
	saved := radarSleep
	var mu sync.Mutex
	var got [][2]int64
	radarSleep = func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, [2]int64{int64(d), int64(len((*f).requests()))})
	}
	t.Cleanup(func() { radarSleep = saved })
	return &got
}

func radarOffDay() []*sfs.SFSObject {
	return []*sfs.SFSObject{duelEntry(DuelThemeBase, "90201", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))}
}

func radarScoringDay() []*sfs.SFSObject {
	return []*sfs.SFSObject{duelEntry(DuelThemeRadar, "90402", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))}
}

// radarUUIDs lists the uuid param of every request with cmd, in order.
func radarUUIDs(f *evFake, cmd string) []int64 {
	var out []int64
	for _, r := range f.requests() {
		if r.Cmd == cmd {
			out = append(out, r.Params.GetLong("uuid"))
		}
	}
	return out
}

// radarExecuteAllowed is everything radar-execute may send on an off day; a radar-scoring day adds
// radarDayAllowed.
var (
	radarExecuteAllowed = []string{"hero.event.info.get", radarInfoCmd, radarPlaceCmd, radarPickStartCmd, radarSamplingEndCmd,
		radarVisitorEndCmd, radarHelpStartCmd, radarHelpEndCmd}
	radarDayAllowed = append(slices.Clone(radarExecuteAllowed), radarTalkStartCmd, radarTalkEndCmd, radarEventCmd)
)

// checkRadarSent fails on any command outside allowed (so never reset.detect.event, a battle
// result, a march or an item use).
func checkRadarSent(t *testing.T, f *evFake, allowed []string) {
	t.Helper()
	for _, c := range f.cmds() {
		if !slices.Contains(allowed, c) {
			t.Errorf("sent %q, which radar-execute must never send here", c)
		}
	}
}

func TestRadarExecuteOffDayRunsOnlyMarchFreeTasks(t *testing.T) {
	mismatch := detectEv(12, 100, 0)
	mismatch.PutInt("type", 2) // the event's own type disagrees with the table
	expired := detectEv(13, 100, 0)
	expired.PutLong("endTime", time.Now().Add(-time.Minute).UnixMilli())
	info := detectReply(16, 10,
		detectEv(1, 100, radarStateNotInWorld), // sampling, placed first
		detectEv(2, 601001, 0),                 // season visitor
		detectEv(3, 24000, 0),                  // help teammates
		detectEv(4, 20000, 0),                  // talk: radar days only
		detectEv(5, 205, 0),                    // kill zombies: march
		detectEv(6, 21001, 0),                  // collect resources: march
		detectEv(7, 15000, 0),                  // doom elite: rally
		detectEv(8, 199, 0),                    // rescue: left to the player
		detectEv(9, 100, radarStateFinished),   // already banked: no claim on an off day
		detectEv(10, 999999, 0),                // unknown id
		detectEv(11, 400000, 0),                // dominator treatment
		mismatch, expired,
		detectEv(14, 22011, 0), detectEv(15, 26074, 0), detectEv(16, 505001, 0), detectEv(17, 505351, 0), // types 17, 20, 40, 46
		detectEv(18, 18001, 0),                    // type 10 Spec Ops (client-simulated battle)
		detectEv(19, 101, radarStateNotInWorld),   // sampling the placement reply leaves out
		detectEv(20, 25001, radarStateNotInWorld), // type 19 treasure (mini-game): not placed
	)
	var f *evFake
	waits := withRadarSleep(t, &f)
	conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{info}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == radarPlaceCmd {
			return placedReply(p, 19)
		}
		return nil
	})
	f = fake
	if err := runRadarExecute(conn, radarTestInit(110, true)); err != nil {
		t.Fatalf("run: %v", err)
	}

	checkRadarSent(t, fake, radarExecuteAllowed)
	var placed []string
	for _, r := range fake.requests() {
		if r.Cmd == radarPlaceCmd {
			placed = append(placed, r.Params.GetString("uuidList"))
		}
	}
	if !slices.Equal(placed, []string{"1|19"}) {
		t.Errorf("placed %q, want one batch of the two state-3 sampling tasks [1|19]", placed)
	}
	starts := radarUUIDs(fake, radarPickStartCmd)
	slices.Sort(starts)
	if !slices.Equal(starts, []int64{1, 2, 11}) {
		t.Errorf("start.pick.garbage %v, want [1 2 11]", starts)
	}
	ends := radarUUIDs(fake, radarSamplingEndCmd)
	slices.Sort(ends)
	if !slices.Equal(ends, []int64{1, 2, 11}) {
		t.Errorf("finish.sampling %v, want [1 2 11]", ends)
	}
	if got := radarUUIDs(fake, radarVisitorEndCmd); !slices.Equal(got, []int64{2}) {
		t.Errorf("finish.visitor %v, want [2] (season visitor only)", got)
	}
	for _, cmd := range []string{radarHelpStartCmd, radarHelpEndCmd} {
		var n int
		for _, r := range fake.requests() {
			if r.Cmd == cmd {
				n++
				if r.Params.GetLong("uuid") != 3 || r.Params.GetInt("eventType") != 18 {
					t.Errorf("%s params %s, want {uuid:3, eventType:18}", cmd, r.Params.String())
				}
			}
		}
		if n != 1 {
			t.Errorf("%s sent %d times, want 1", cmd, n)
		}
	}

	// One fake-march wait of at least 3 s, after every start and before every end.
	if len(*waits) != 1 || time.Duration((*waits)[0][0]) < 3*time.Second {
		t.Fatalf("waits %v, want one of at least 3s", *waits)
	}
	at := int((*waits)[0][1])
	for i, r := range fake.requests() {
		start := r.Cmd == radarPickStartCmd || r.Cmd == radarHelpStartCmd
		end := r.Cmd == radarSamplingEndCmd || r.Cmd == radarVisitorEndCmd || r.Cmd == radarHelpEndCmd
		if (start && i >= at) || (end && i < at) {
			t.Errorf("request %d %s is on the wrong side of the fake-march wait (after request %d)", i, r.Cmd, at)
		}
	}
}

func TestRadarExecuteHelpStaminaReserve(t *testing.T) {
	free := detectEv(3, 24002, 0)
	free.PutInt("cost", 1) // a free help task (DetectEventCompleteOneClickBtnView.lua:143)
	free.PutLong("endTime", time.Now().Add(6*time.Hour).UnixMilli())
	soon := detectEv(1, 24000, 0)
	soon.PutLong("endTime", time.Now().Add(1*time.Hour).UnixMilli())
	later := detectEv(2, 24001, 0)
	later.PutLong("endTime", time.Now().Add(2*time.Hour).UnixMilli())
	info := detectReply(16, 10, soon, later, free)

	cases := []struct {
		name string
		in   *Init
		want []int64
	}{
		// 75 - 10 = 65 keeps the 60 reserve; a second paid task would leave 55. The free one runs.
		{"reserve", radarTestInit(75, true), []int64{1, 3}},
		{"plenty", radarTestInit(110, true), []int64{1, 2, 3}},
		{"no stamina in init", radarTestInit(-1, true), nil},
		{"no alliance", radarTestInit(110, false), nil},
	}
	for _, c := range cases {
		var f *evFake
		withRadarSleep(t, &f)
		conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{info}, nil)
		f = fake
		if err := runRadarExecute(conn, c.in); err != nil {
			t.Fatalf("%s: run: %v", c.name, err)
		}
		if got := radarUUIDs(fake, radarHelpStartCmd); !slices.Equal(got, c.want) {
			t.Errorf("%s: helped %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRadarStaminaFloor(t *testing.T) {
	now := time.Now()
	in := func(stamina int64, last time.Time) *Init {
		raw := sfs.NewSFSObject()
		p := sfs.NewSFSObject()
		p.PutInt("stamina", int32(stamina))
		p.PutLong("lastStaminaTime", last.UnixMilli())
		raw.PutSFSObject("playerInfo", p)
		return &Init{Raw: raw}
	}
	for _, c := range []struct {
		in   *Init
		want int64
		ok   bool
	}{
		{in(50, now.Add(-2*time.Hour)), 74, true},    // +1 per 300 s
		{in(100, now.Add(-10*time.Hour)), 110, true}, // capped at the base cap
		{in(130, now.Add(-10*time.Hour)), 130, true}, // above the cap (items): no regeneration
		{&Init{Raw: sfs.NewSFSObject()}, 0, false},
	} {
		if got, ok := radarStaminaFloor(c.in, now); got != c.want || ok != c.ok {
			t.Errorf("radarStaminaFloor = %d, %v; want %d, %v", got, ok, c.want, c.ok)
		}
	}
}

func TestRadarExecuteTalkOnlyOnRadarDays(t *testing.T) {
	info := detectReply(16, 10, detectEv(4, 20000, 0))
	for _, radarDay := range []bool{false, true} {
		duel := radarOffDay()
		if radarDay {
			duel = radarScoringDay()
		}
		var f *evFake
		withRadarSleep(t, &f)
		conn, fake := startRadarFake(t, duel, []*sfs.SFSObject{info}, nil)
		f = fake
		if err := runRadarExecute(conn, radarTestInit(110, true)); err != nil {
			t.Fatalf("radarDay=%v: run: %v", radarDay, err)
		}
		var want []int64
		if radarDay {
			want = []int64{4}
		}
		if got := radarUUIDs(fake, radarTalkStartCmd); !slices.Equal(got, want) {
			t.Errorf("radarDay=%v: talk starts %v, want %v", radarDay, got, want)
		}
		if got := radarUUIDs(fake, radarTalkEndCmd); !slices.Equal(got, want) {
			t.Errorf("radarDay=%v: talk ends %v, want %v", radarDay, got, want)
		}
		if got := radarUUIDs(fake, radarEventCmd); len(got) != 0 {
			t.Errorf("radarDay=%v: claimed %v; a talk's end claims it, nothing else may", radarDay, got)
		}
		checkRadarSent(t, fake, radarDayAllowed)
	}
}

func TestRadarExecuteNeedsQuickExecute(t *testing.T) {
	in := radarTestInit(110, true)
	in.Raw.PutSFSObject("dataConfig", sfs.NewSFSObject())
	conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{detectReply(16, 10, detectEv(1, 100, 0))}, nil)
	if err := runRadarExecute(conn, in); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("Quick Execute off: err %v, sent %v; want nothing", err, fake.cmds())
	}
}

func TestRadarExecuteClaimsWhatItFinishesOnRadarDays(t *testing.T) {
	// Tasks it must leave alone on a radar day too: a banked one from earlier (radar-claims' job),
	// a finished rescue, and march, rally and battle tasks.
	untouched := func() []*sfs.SFSObject {
		return []*sfs.SFSObject{detectEv(9, 100, radarStateFinished), detectEv(10, 199, radarStateFinished),
			detectEv(30, 205, 0), detectEv(31, 15000, 0), detectEv(32, 18001, 0), detectEv(33, 21001, radarStateNotInWorld)}
	}
	infos := []*sfs.SFSObject{
		// Round 1: one task to run.
		detectReply(16, 10, append(untouched(), detectEv(1, 100, 0))...),
		// After round 1's ends: task 1 finished.
		detectReply(16, 10, append(untouched(), detectEv(1, 100, radarStateFinished))...),
		// After the claim: its newEvent, unplaced.
		detectReply(16, 9, append(untouched(), detectEv(21, 102, radarStateNotInWorld))...),
		// After round 2's ends.
		detectReply(16, 9, append(untouched(), detectEv(21, 102, radarStateFinished))...),
		// After the second claim: nothing march-free left.
		detectReply(16, 8, untouched()...),
	}
	var f *evFake
	waits := withRadarSleep(t, &f)
	conn, fake := startRadarFake(t, radarScoringDay(), infos, nil)
	f = fake
	if err := runRadarExecute(conn, radarTestInit(110, true)); err != nil {
		t.Fatalf("run: %v", err)
	}
	checkRadarSent(t, fake, radarDayAllowed)
	if got := radarUUIDs(fake, radarPickStartCmd); !slices.Equal(got, []int64{1, 21}) {
		t.Errorf("started %v, want [1 21]", got)
	}
	if got := radarUUIDs(fake, radarEventCmd); !slices.Equal(got, []int64{1, 21}) {
		t.Errorf("claimed %v, want [1 21]: only the tasks this run finished", got)
	}
	if got := radarUUIDs(fake, radarSamplingEndCmd); !slices.Equal(got, []int64{1, 21}) {
		t.Errorf("finished %v, want [1 21]", got)
	}
	if len(*waits) != 2 {
		t.Errorf("waited %d times, want one per round (2)", len(*waits))
	}

	// The same tasks on an off day: executed, never claimed.
	withRadarSleep(t, &f)
	conn, fake = startRadarFake(t, radarOffDay(), infos, nil)
	f = fake
	if err := runRadarExecute(conn, radarTestInit(110, true)); err != nil {
		t.Fatalf("off day run: %v", err)
	}
	if got := radarUUIDs(fake, radarEventCmd); len(got) != 0 {
		t.Errorf("off day: claimed %v, want nothing", got)
	}
}

func TestRadarExecuteCapsTasksPerRun(t *testing.T) {
	var events []*sfs.SFSObject
	for i := range radarExecuteMaxEvents + 5 {
		events = append(events, detectEv(int64(100+i), 100, 0))
	}
	var f *evFake
	withRadarSleep(t, &f)
	conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{detectReply(16, 10, events...)}, nil)
	f = fake
	if err := runRadarExecute(conn, radarTestInit(110, true)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(radarUUIDs(fake, radarPickStartCmd)); n != radarExecuteMaxEvents {
		t.Errorf("started %d tasks, want the cap %d", n, radarExecuteMaxEvents)
	}
}

func TestRadarExecuteFailuresAndNoReply(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out one command timeout")
	}
	info := detectReply(16, 10, detectEv(1, 100, 0), detectEv(2, 101, 0), detectEv(3, 102, 0))
	var f *evFake
	withRadarSleep(t, &f)
	conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{info}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch {
		case cmd == radarPickStartCmd && p.GetLong("uuid") == 2:
			return evErr("999999") // refused: never ended
		case cmd == radarSamplingEndCmd && p.GetLong("uuid") == 1:
			return radarNoReply // the client ignores this reply; the feature must too
		}
		return nil
	})
	f = fake
	err := runRadarExecute(conn, radarTestInit(110, true))
	if err == nil || !strings.Contains(err.Error(), "999999") {
		t.Errorf("run = %v, want the refused start's error", err)
	}
	if got := radarUUIDs(fake, radarSamplingEndCmd); !slices.Equal(got, []int64{1, 3}) {
		t.Errorf("finish.sampling %v, want [1 3]: a refused start is not ended, an unanswered end doesn't stop the next", got)
	}
}

func TestRadarExecuteNoInit(t *testing.T) {
	conn, fake := startRadarFake(t, nil, []*sfs.SFSObject{detectReply(16, 10)}, nil)
	if err := runRadarExecute(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v; want nothing", err, fake.cmds())
	}
}

func TestRadarInventoryIsReadOnly(t *testing.T) {
	info := detectReply(16, 38, detectEv(1, 100, 0), detectEv(2, 100, radarStateFinished), detectEv(3, 205, 0),
		detectEv(4, 20000, radarStateNotInWorld))
	for _, duel := range [][]*sfs.SFSObject{radarOffDay(), radarScoringDay()} {
		conn, fake := startRadarFake(t, duel, []*sfs.SFSObject{info}, nil)
		if err := runRadarInventory(conn, radarTestInit(110, true)); err != nil {
			t.Fatalf("run: %v", err)
		}
		for _, c := range fake.cmds() {
			if c != radarInfoCmd && c != "hero.event.info.get" {
				t.Errorf("radar-inventory sent %q; it may only read", c)
			}
		}
	}
}

func TestRadarEventType(t *testing.T) {
	for id, want := range map[int64]int32{100: 6, 130: 6, 199: 11, 205: 2, 20000: 14, 24000: 18, 410004: 37, 601001: 44} {
		if got, ok := radarEventType(id); !ok || got != want {
			t.Errorf("radarEventType(%d) = %d, %v; want %d", id, got, ok, want)
		}
	}
	for _, id := range []int64{0, 99, 131, 999999, 1 << 40} {
		if got, ok := radarEventType(id); ok {
			t.Errorf("radarEventType(%d) = %d, want unknown", id, got)
		}
	}
	// radarSkippedEvent's rescue and cockatrice ids are exactly the table's types 11 and 35-37.
	for _, r := range radarEventTypeRanges {
		for id := r.lo; id <= r.hi; id++ {
			want := r.typ == radarTypeRescue || (r.typ >= 35 && r.typ <= 37)
			if radarSkippedEvent(id) != want {
				t.Errorf("radarSkippedEvent(%d) = %v, but its type is %d", id, !want, r.typ)
			}
		}
	}
}

func TestRadarFeaturesRegistered(t *testing.T) {
	for _, name := range []string{"radar-execute", "radar-overflow", "radar-inventory"} {
		f, ok := featureRegistry[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if f.DefaultOn || f.Hold {
			t.Errorf("%s: DefaultOn=%v Hold=%v, want both false (it gates on the duel itself)", name, f.DefaultOn, f.Hold)
		}
	}
}
