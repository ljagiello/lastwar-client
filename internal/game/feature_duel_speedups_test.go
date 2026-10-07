package game

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Item ids used below (goods type 2, 1.0.364).
const (
	uni1m, uni5m, uni1h, uni3h         = 200200, 200201, 200203, 200204
	build1m, build5m, build1h, build3h = 200210, 200211, 200213, 200214
	res5m, res1h                       = 200221, 200223
	train15m, train1h                  = 200232, 200233
	heal5m, heal1h                     = 200241, 200243

	speedWall = 10107000 // any non-HQ building
)

// Duel days at evTestNow, with the 1.0.364 score lists.
func speedDay(theme int32, score string) *sfs.SFSObject {
	return duelEntry(theme, score, evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
}

var (
	speedMon = func() *sfs.SFSObject { return speedDay(DuelThemeRadar, "90402|90401|90102|90004|90005|90000") }
	speedTue = func() *sfs.SFSObject {
		return speedDay(DuelThemeBase, "90201|90202|90203|90204|90000|100910|110301|110302")
	}
	speedWed = func() *sfs.SFSObject { return speedDay(DuelThemeScience, "90301|90302|90303|90402|90000") }
	speedThu = func() *sfs.SFSObject { return speedDay(DuelThemeHeroes, "90101|90102|90103|90104|90105|90106|90000") }
	speedFri = func() *sfs.SFSObject {
		return speedDay(DuelThemeMobilization, "90402|90201|90301|90501|90202|90302|90502|90000")
	}
	speedSat = func() *sfs.SFSObject {
		return speedDay(DuelThemeEnemyBuster, "90203|90204|90201|90301|90501|90601|90602|90000")
	}
)

// speedJob is one running job in the test init and on the fake server.
type speedJob struct {
	kind   duelSpeedupKind
	uuid   int64
	bID    int64 // buildings; default speedWall (or the camp for training)
	lv     int64
	left   time.Duration // remaining at evTestNow; <= 0 is not running
	state  int64
	qStart time.Time // queues: sT (default an hour ago)
}

type speedInit struct {
	jobs     []speedJob
	items    map[int64]int64
	gold     int64
	noGold   bool
	shield   *time.Time // defend_wall.protectEndTime; nil: no defend_wall
	extraRaw func(raw *sfs.SFSObject)
}

func (c speedInit) build() *Init {
	in := evInit(30, duelActivity())
	raw := in.Raw
	builds := sfs.NewSFSArray()
	queues := sfs.NewSFSArray()
	for _, j := range c.jobs {
		end := evTestNow.Add(j.left).UnixMilli()
		switch j.kind {
		case duelSpeedupBuild, duelSpeedupTraining:
			o := sfs.NewSFSObject()
			o.PutLong("uuid", j.uuid)
			bID, lv := j.bID, j.lv
			if bID == 0 {
				bID = speedWall
				if j.kind == duelSpeedupTraining {
					bID = finishedTimersMilitaryCamp
				}
			}
			if lv == 0 {
				lv = 20
			}
			o.PutInt("bId", int32(bID))
			o.PutInt("lv", int32(lv))
			if j.state != 0 {
				o.PutInt("state", int32(j.state))
			}
			if j.kind == duelSpeedupBuild {
				o.PutLong("uT", end)
			} else {
				o.PutLong("prodST", evTestNow.Add(-time.Hour).UnixMilli())
				o.PutLong("prodET", end)
			}
			builds.AddSFSObject(o)
		default:
			o := sfs.NewSFSObject()
			o.PutLong("uuid", j.uuid)
			qType := int32(finishedTimersQueueResearch)
			if j.kind == duelSpeedupHeal {
				qType = finishedTimersQueueHospital
			}
			o.PutInt("type", qType)
			start := j.qStart
			if start.IsZero() {
				start = evTestNow.Add(-time.Hour)
			}
			o.PutLong("sT", start.UnixMilli())
			o.PutLong("uT", end)
			queues.AddSFSObject(o)
		}
	}
	raw.PutSFSArray("building_new", builds)
	raw.PutSFSArray("queue_new", queues)
	items := sfs.NewSFSArray()
	other := sfs.NewSFSObject() // not a speed-up
	other.PutLong("uuid", 1)
	other.PutUtfString("itemId", "630011")
	other.PutInt("count", 99)
	items.AddSFSObject(other)
	for _, id := range slices.Sorted(maps.Keys(c.items)) {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", 5000+id)
		o.PutUtfString("itemId", strconv.FormatInt(id, 10))
		o.PutInt("count", int32(c.items[id]))
		items.AddSFSObject(o)
	}
	raw.PutSFSArray("items", items)
	if !c.noGold {
		user := sfs.NewSFSObject()
		user.PutLong("gold", c.gold)
		user.PutUtfString("name", "not-logged")
		raw.PutSFSObject("user", user)
	}
	if c.shield != nil {
		dw := sfs.NewSFSObject()
		dw.PutLong("protectEndTime", c.shield.UnixMilli())
		raw.PutSFSObject("defend_wall", dw)
	}
	if c.extraRaw != nil {
		c.extraRaw(raw)
	}
	return in
}

// speedFake is a game server for duel-speedups: it serves today's duel, keeps the jobs, the bag
// and the diamonds, answers the three speed-up commands the way the 1.0.364 handlers read them,
// and records every request that breaks a rule.
type speedFake struct {
	mu         sync.Mutex
	day        *sfs.SFSObject
	gold       int64
	items      map[int64]int64
	ends       map[int64]int64 // job uuid -> end ms
	qTypes     map[int64]int32
	violations []string

	goldDrop     int64                // taken off every gold balance replied
	topGoldDrop  bool                 // queue: put the dropped balance at the top level, not under queue
	noItemCost   bool                 // omit itemCostArr
	noCampGold   bool                 // omit gold from camp replies
	noEnd        bool                 // omit the end time
	noProgress   bool                 // leave the end time unchanged
	takeOff      func(ms int64) int64 // server-side time taken off for ms of items (default ms)
	errFor       map[int64]string     // job uuid -> errorCode
	closeOnSpeed bool                 // drop the connection on the first speed-up
}

func newSpeedFake(day *sfs.SFSObject, c speedInit) *speedFake {
	f := &speedFake{day: day, gold: c.gold, items: maps.Clone(c.items), ends: map[int64]int64{}, qTypes: map[int64]int32{}, errFor: map[int64]string{}}
	if f.items == nil {
		f.items = map[int64]int64{}
	}
	for _, j := range c.jobs {
		f.ends[j.uuid] = evTestNow.Add(j.left).UnixMilli()
		if j.kind == duelSpeedupHeal {
			f.qTypes[j.uuid] = finishedTimersQueueHospital
		} else {
			f.qTypes[j.uuid] = finishedTimersQueueResearch
		}
	}
	return f
}

func (f *speedFake) violate(format string, a ...any) {
	f.violations = append(f.violations, fmt.Sprintf(format, a...))
}

func (f *speedFake) reply(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd {
	case "hero.event.info.get":
		if p.GetString("activityId") != "70000" || f.day == nil {
			return duelInfo()
		}
		return duelInfo(f.day)
	case duelSpeedupsBuildCmd, duelSpeedupsQueueCmd, duelSpeedupsCampCmd:
	default:
		return evOK()
	}
	if f.closeOnSpeed {
		return nil
	}
	for _, k := range []string{"useGold", "golloesSpeedTime", "gold", "isGold"} {
		if p.Has(k) {
			f.violate("%s sent %s", cmd, k)
		}
	}
	uuidKey, itemKey := "qUUID", "itemIDs"
	switch cmd {
	case duelSpeedupsBuildCmd:
		uuidKey = "bUUID"
	case duelSpeedupsCampCmd:
		uuidKey, itemKey = "uuid", "itemId"
	}
	uuid := p.GetLong(uuidKey)
	if cmd == duelSpeedupsBuildCmd {
		if v, ok := p.Get("isFixRuins"); !ok || v.Val != false {
			f.violate("build.ccd.m.new without isFixRuins:false")
		}
	}
	end, ok := f.ends[uuid]
	if !ok {
		f.violate("%s for unknown job %d", cmd, uuid)
		return evErr("E000001")
	}
	if code := f.errFor[uuid]; code != "" {
		return evErr(code)
	}
	nowMs := evTestNow.UnixMilli()
	if end <= nowMs {
		f.violate("%s on job %d that is not running", cmd, uuid)
	}
	items := p.GetString(itemKey)
	if items == "" {
		f.violate("%s with an empty item list", cmd)
	}
	costs := sfs.NewSFSArray()
	var ms int64
	for _, part := range strings.Split(items, "|") {
		idStr, nStr, _ := strings.Cut(part, ";")
		id, _ := strconv.ParseInt(idStr, 10, 64)
		n, _ := strconv.ParseInt(nStr, 10, 64)
		if n <= 0 || n > f.items[id] {
			f.violate("%s asks for %d of item %d, owned %d", cmd, n, id, f.items[id])
		}
		f.items[id] -= n
		ms += n * duelSpeedupItemSecs[id] * 1000
		c := sfs.NewSFSObject()
		c.PutLong("uuid", 5000+id)
		c.PutUtfString("itemId", idStr)
		c.PutInt("count", int32(f.items[id]))
		costs.AddSFSObject(c)
	}
	if f.takeOff != nil {
		ms = f.takeOff(ms)
	}
	if !f.noProgress {
		end -= ms
	}
	f.ends[uuid] = end
	finished := end <= nowMs
	gold := f.gold - f.goldDrop
	f.gold = gold

	r := sfs.NewSFSObject()
	switch cmd {
	case duelSpeedupsBuildCmd:
		r.PutLong("remainGold", gold)
		if !f.noItemCost {
			r.PutSFSArray("itemCostArr", costs)
		}
		b := sfs.NewSFSObject()
		b.PutLong("uuid", uuid)
		if !f.noEnd {
			b.PutLong("uT", end)
		}
		r.PutSFSObject("buildInfo", b)
		r.PutBool("finished", finished)
	case duelSpeedupsQueueCmd:
		q := sfs.NewSFSObject()
		q.PutLong("uuid", uuid)
		q.PutInt("type", f.qTypes[uuid])
		q.PutLong("sT", evTestNow.Add(-time.Hour).UnixMilli())
		if !f.noEnd {
			q.PutLong("uT", end)
		}
		if !f.noItemCost {
			q.PutSFSArray("itemCostArr", costs)
		}
		if f.topGoldDrop {
			r.PutLong("remainGold", gold)
		} else {
			q.PutLong("remainGold", gold)
		}
		r.PutSFSObject("queue", q)
	case duelSpeedupsCampCmd:
		if !f.noCampGold {
			r.PutLong("gold", gold)
		}
		b := sfs.NewSFSObject()
		b.PutLong("uuid", uuid)
		b.PutLong("prodST", evTestNow.Add(-time.Hour).UnixMilli())
		if !f.noEnd {
			b.PutLong("prodET", end)
		}
		r.PutSFSObject("buildInfo", b)
	}
	return r
}

// speedSent is one speed-up request as sent.
type speedSent struct {
	cmd   string
	uuid  int64
	items string
}

func (s speedSent) String() string { return fmt.Sprintf("%s %d %s", s.cmd, s.uuid, s.items) }

// clearSpeedTrip resets the session tripwire around a test.
func clearSpeedTrip(t *testing.T) {
	t.Helper()
	reset := func() {
		duelSpeedupsTrip.Lock()
		duelSpeedupsTrip.reason = ""
		duelSpeedupsTrip.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// runSpeed runs duel-speedups (or the plan, dry) against f and returns the speed-up requests.
// Every run is checked for the hard rules: no diamond keys, no empty item list, never more than
// owned (cumulative across the run), only items of a queue's own class or universal.
func runSpeed(t *testing.T, f *speedFake, c speedInit, dry bool) ([]speedSent, error) {
	t.Helper()
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, f.reply)
	var err error
	if dry {
		err = runDuelSpeedupsPlan(conn, c.build())
	} else {
		err = runDuelSpeedups(conn, c.build())
	}
	var out []speedSent
	used := map[int64]int64{}
	for _, r := range fake.requests() {
		keys, speed := duelSpeedupsAllowedKeys[r.Cmd]
		if !speed {
			if r.Cmd != "hero.event.info.get" {
				t.Errorf("unexpected command %s", r.Cmd)
			}
			continue
		}
		for _, k := range r.Params.Keys() {
			if !slices.Contains(keys, k) && k != "_id" { // _id: the session layer's request id
				t.Errorf("%s sent key %q (only %v allowed)", r.Cmd, k, keys)
			}
		}
		itemKey := keys[len(keys)-1]
		items := r.Params.GetString(itemKey)
		if items == "" {
			t.Errorf("%s sent an empty %s", r.Cmd, itemKey)
		}
		for _, part := range strings.Split(items, "|") {
			idStr, nStr, _ := strings.Cut(part, ";")
			id, _ := strconv.ParseInt(idStr, 10, 64)
			n, _ := strconv.ParseInt(nStr, 10, 64)
			used[id] += n
			if used[id] > c.items[id] {
				t.Errorf("run used %d of item %d, owned %d", used[id], id, c.items[id])
			}
		}
		out = append(out, speedSent{r.Cmd, r.Params.GetLong(keys[0]), items})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.violations {
		t.Errorf("server saw a rule break: %s", v)
	}
	return out, err
}

func sentStrings(s []speedSent) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.String()
	}
	return out
}

func wantSent(t *testing.T, got []speedSent, want ...string) {
	t.Helper()
	if g := sentStrings(got); !slices.Equal(g, want) {
		t.Errorf("sent:\n  %s\nwant:\n  %s", strings.Join(g, "\n  "), strings.Join(want, "\n  "))
	}
}

// allJobs is a city with every kind of job running: construction 2h10m, research 2h, training
// 1h, healing 3h.
func allJobs() []speedJob {
	return []speedJob{
		{kind: duelSpeedupBuild, uuid: 101, left: 2*time.Hour + 10*time.Minute},
		{kind: duelSpeedupResearch, uuid: 201, left: 2 * time.Hour},
		{kind: duelSpeedupTraining, uuid: 301, left: time.Hour},
		{kind: duelSpeedupHeal, uuid: 401, left: 3 * time.Hour},
	}
}

// allItems owns some of every class.
func allItems() map[int64]int64 {
	return map[int64]int64{
		uni1h: 3, uni3h: 1, uni5m: 4,
		build1h: 3, build5m: 20,
		res1h: 5, res5m: 10,
		train15m: 2,
		heal1h:   2, heal5m: 5,
	}
}

func TestDuelSpeedupsPickIsTheClientPicker(t *testing.T) {
	ids := append(slices.Clone(duelSpeedupSpecific[duelSpeedupBuild]), duelSpeedupUniversal...)
	cases := []struct {
		name   string
		remain time.Duration
		owned  map[int64]int64
		want   string
	}{
		{"largest first, then smaller", 2*time.Hour + 30*time.Minute, map[int64]int64{build1h: 5, build5m: 10}, "200213;2|200211;6"},
		{"queue items before universal", 2 * time.Hour, map[int64]int64{build1h: 1, uni1h: 5}, "200213;1|200203;1"},
		{"never more than owned", 10 * time.Hour, map[int64]int64{build1h: 1, build5m: 2}, "200213;1|200211;2"},
		{"remaining + 59 s takes the 1m", 30 * time.Second, map[int64]int64{build1m: 3}, "200210;1"},
		{"too big an item is left", 3 * time.Minute, map[int64]int64{build5m: 3}, ""},
		{"3h item left for a 2h59m job", 2*time.Hour + 59*time.Minute, map[int64]int64{build3h: 1, build1h: 2}, "200213;2"},
		{"gap filler under 60 s", time.Second, map[int64]int64{build1m: 2}, "200210;1"},
		{"nothing owned", time.Hour, map[int64]int64{}, ""},
		{"not running", 0, map[int64]int64{build1h: 1}, ""},
	}
	for _, c := range cases {
		plan := duelSpeedupsPick(c.remain.Milliseconds(), c.owned, ids)
		if got := duelSpeedupsItemString(plan); got != c.want {
			t.Errorf("%s: plan %q, want %q", c.name, got, c.want)
		}
	}

	// Over a sweep of jobs and bags: within owned, under remaining + 60 s, and something is used
	// whenever an owned item fits within remaining + 59 s.
	for rem := int64(500); rem < 30*3600*1000; rem = rem*3/2 + 777 {
		for bag := range 12 {
			owned := map[int64]int64{}
			for i, id := range ids {
				owned[id] = int64((bag*7 + i*3) % 4)
			}
			plan := duelSpeedupsPick(rem, owned, ids)
			for _, u := range plan {
				if u.count <= 0 || u.count > owned[u.id] {
					t.Fatalf("rem %d bag %d: %d of item %d, owned %d", rem, bag, u.count, u.id, owned[u.id])
				}
			}
			if used := duelSpeedupsPlanMs(plan); used >= rem+duelSpeedupsGapLimitMs {
				t.Fatalf("rem %d bag %d: overshoot, %d ms of items", rem, bag, used)
			}
			fits := false
			for _, id := range ids {
				fits = fits || (owned[id] > 0 && duelSpeedupItemSecs[id]*1000 < rem+duelSpeedupsToleranceMs)
			}
			if fits && len(plan) == 0 {
				t.Fatalf("rem %d bag %d: an owned item fits but nothing was picked", rem, bag)
			}
		}
	}
}

func TestDuelSpeedupsCheckParamsRefusesUnsafeRequests(t *testing.T) {
	owned := map[int64]int64{build1h: 2}
	ok := func() *sfs.SFSObject {
		p := sfs.NewSFSObject()
		p.PutLong("bUUID", 1)
		p.PutBool("isFixRuins", false)
		p.PutUtfString("itemIDs", "200213;2")
		return p
	}
	if err := duelSpeedupsCheckParams(duelSpeedupsBuildCmd, ok(), owned); err != nil {
		t.Fatalf("a valid request was refused: %v", err)
	}
	bad := map[string]func(p *sfs.SFSObject){
		"useGold":          func(p *sfs.SFSObject) { p.PutInt("useGold", 0) },
		"golloesSpeedTime": func(p *sfs.SFSObject) { p.PutInt("golloesSpeedTime", 60) },
		"empty itemIDs":    func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "") },
		"more than owned":  func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "200213;3") },
		"zero count":       func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "200213;0") },
		"not a speed-up":   func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "630011;1") },
		"malformed":        func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "200213") },
		"repeated item":    func(p *sfs.SFSObject) { p.PutUtfString("itemIDs", "200213;1|200213;1") },
		"ruin repair":      func(p *sfs.SFSObject) { p.PutBool("isFixRuins", true) },
	}
	for name, mutate := range bad {
		p := ok()
		mutate(p)
		if err := duelSpeedupsCheckParams(duelSpeedupsBuildCmd, p, owned); err == nil {
			t.Errorf("%s: request was not refused", name)
		}
	}
	camp := sfs.NewSFSObject()
	camp.PutLong("uuid", 1)
	camp.PutUtfString("itemId", "200213;1")
	camp.PutInt("useGold", 1)
	if err := duelSpeedupsCheckParams(duelSpeedupsCampCmd, camp, owned); err == nil {
		t.Error("building.camp.accel with useGold was not refused")
	}
	if err := duelSpeedupsCheckParams("item.buyAndUse", ok(), owned); err == nil {
		t.Error("a non-speed-up command was not refused")
	}
}

func TestDuelSpeedupsBaseExpansionUsesConstructionItemsOnly(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	f := newSpeedFake(speedTue(), c)
	sent, err := runSpeed(t, f, c, false)
	if err != nil {
		t.Fatal(err)
	}
	// 2h10m: 1h x2 (2h), then 5m x2 (10m). No universal, nothing on the other queues.
	wantSent(t, sent, "build.ccd.m.new 101 200213;2|200211;2")
	if f.ends[101] > evTestNow.UnixMilli() {
		t.Error("construction should be finished")
	}
}

func TestDuelSpeedupsAgeOfScienceUsesResearchItemsOnly(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedWed(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, "queue.ccd.m.new 201 200223;2")
}

func TestDuelSpeedupsTotalMobilization(t *testing.T) {
	clearSpeedTrip(t)
	jobs := []speedJob{
		{kind: duelSpeedupBuild, uuid: 101, left: 5 * time.Hour},
		{kind: duelSpeedupResearch, uuid: 201, left: 2 * time.Hour},
		{kind: duelSpeedupTraining, uuid: 301, left: time.Hour},
		{kind: duelSpeedupHeal, uuid: 401, left: 9 * time.Hour}, // healing doesn't score Friday
	}
	c := speedInit{jobs: jobs, items: map[int64]int64{
		build1h: 5, res1h: 5, heal1h: 5, // held for Tue / Wed / Sat
		train15m: 2,
		uni3h:    1, uni1h: 3,
	}, gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedFri(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent,
		"building.camp.accel 301 200232;2",      // training items on training
		"build.ccd.m.new 101 200204;1|200203;2", // universal onto the longest scoring job (5h)
		"queue.ccd.m.new 201 200203;1",          // then the next longest (2h research)
	)
}

func TestDuelSpeedupsEnemyBusterTrainingAndHealing(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedSat(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	// Healing 3h: 1h x2 + 5m x5; training 1h: 15m x2. Construction and research score on
	// Saturday too, but their items are held for Tue / Wed, and universal items for Friday.
	wantSent(t, sent,
		"queue.ccd.m.new 401 200243;2|200241;5",
		"building.camp.accel 301 200232;2",
	)
}

func TestDuelSpeedupsUniversalOnlyOnFriday(t *testing.T) {
	for name, day := range map[string]func() *sfs.SFSObject{"tue": speedTue, "wed": speedWed, "sat": speedSat} {
		t.Run(name, func(t *testing.T) {
			clearSpeedTrip(t)
			c := speedInit{jobs: allJobs(), items: map[int64]int64{uni1m: 50, uni1h: 9, uni3h: 9}, gold: 1000}
			sent, err := runSpeed(t, newSpeedFake(day(), c), c, false)
			if err != nil || len(sent) != 0 {
				t.Errorf("universal items used on %s: %v (err %v)", name, sentStrings(sent), err)
			}
		})
	}
}

func TestDuelSpeedupsThemeAndScoreGate(t *testing.T) {
	cases := map[string]*sfs.SFSObject{
		"radar day":                   speedMon(),
		"heroes day":                  speedThu(),
		"no duel day":                 nil,
		"Tue without 51/7 in score":   speedDay(DuelThemeBase, "90202|90203|90204|90000"),
		"Wed without 51/6 in score":   speedDay(DuelThemeScience, "90302|90303|90402"),
		"51/7 scoring on another day": speedDay(DuelThemeHeroes, "90201|90301|90501|90601"),
	}
	for name, day := range cases {
		t.Run(name, func(t *testing.T) {
			clearSpeedTrip(t)
			c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
			sent, err := runSpeed(t, newSpeedFake(day, c), c, false)
			if err != nil || len(sent) != 0 {
				t.Errorf("sent %v (err %v), want nothing", sentStrings(sent), err)
			}
		})
	}
	// Friday without 51/4: no training items; universal still goes to the scoring jobs.
	clearSpeedTrip(t)
	c := speedInit{jobs: []speedJob{{kind: duelSpeedupTraining, uuid: 301, left: time.Hour}}, items: map[int64]int64{train15m: 2, uni1h: 1}, gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedDay(DuelThemeMobilization, "90201|90301|90402"), c), c, false)
	if err != nil || len(sent) != 0 {
		t.Errorf("training sped up on a Friday that doesn't score it: %v (err %v)", sentStrings(sent), err)
	}
}

func TestDuelSpeedupsHeldByTheSchedulerOffDays(t *testing.T) {
	clearSpeedTrip(t)
	withEvNow(t, evTestNow)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	f := newSpeedFake(speedThu(), c)
	conn, fake := startEvFake(t, f.reply)
	err := RunFeatures(conn, c.build(), FeatureConfig{}, "duel-speedups")
	if err == nil || !strings.Contains(err.Error(), "held") {
		t.Errorf("-run duel-speedups on Train Heroes must be refused as held, got %v", err)
	}
	if got := fake.only(duelSpeedupsBuildCmd); len(got) != 0 {
		t.Errorf("a held run sent %d speed-ups", len(got))
	}
}

func TestDuelSpeedupsNeverMoreThanOwnedAcrossJobs(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: []speedJob{
		{kind: duelSpeedupBuild, uuid: 101, left: 2 * time.Hour},
		{kind: duelSpeedupBuild, uuid: 102, left: 2 * time.Hour},
		{kind: duelSpeedupBuild, uuid: 103, left: 2 * time.Hour},
	}, items: map[int64]int64{build1h: 3}, gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedTue(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, "build.ccd.m.new 101 200213;2", "build.ccd.m.new 102 200213;1")
}

func TestDuelSpeedupsTripwireOnDiamondDrop(t *testing.T) {
	cases := map[string]struct {
		day func() *sfs.SFSObject
		job duelSpeedupKind
	}{
		"build": {speedTue, duelSpeedupBuild},
		"queue": {speedWed, duelSpeedupResearch},
		"camp":  {speedSat, duelSpeedupTraining},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearSpeedTrip(t)
			// Two jobs of the kind: the second must never be sent once the first trips.
			c := speedInit{jobs: []speedJob{{kind: tc.job, uuid: 1, left: 2 * time.Hour}, {kind: tc.job, uuid: 2, left: time.Hour}},
				items: allItems(), gold: 1000}
			c.items[train1h] = 4
			day := tc.day
			f := newSpeedFake(day(), c)
			f.goldDrop = 1
			sent, err := runSpeed(t, f, c, false)
			if err == nil || !strings.Contains(err.Error(), "tripwire") || !strings.Contains(err.Error(), "dropped from 1000 to 999") {
				t.Fatalf("a diamond drop must trip: err %v", err)
			}
			if len(sent) != 1 {
				t.Errorf("sent %d speed-ups after the drop, want only the one that dropped", len(sent))
			}
			// The rest of the session sends nothing, not even the duel lookup.
			f2 := newSpeedFake(day(), c)
			again, err := runSpeed(t, f2, c, false)
			if err == nil || len(again) != 0 {
				t.Errorf("after a trip the feature must stay off: sent %v, err %v", sentStrings(again), err)
			}
		})
	}
}

func TestDuelSpeedupsTripwireOnMissingProof(t *testing.T) {
	cases := map[string]struct {
		day   func() *sfs.SFSObject
		knob  func(f *speedFake)
		cause string
	}{
		"build without itemCostArr": {speedTue, func(f *speedFake) { f.noItemCost = true }, "no itemCostArr"},
		"queue without itemCostArr": {speedWed, func(f *speedFake) { f.noItemCost = true }, "no itemCostArr"},
		"camp without gold":         {speedSat, func(f *speedFake) { f.noCampGold = true }, "no gold balance"},
		"queue top-level drop":      {speedWed, func(f *speedFake) { f.goldDrop, f.topGoldDrop = 5, true }, "dropped"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			clearSpeedTrip(t)
			c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
			if name == "camp without gold" {
				c.jobs = []speedJob{{kind: duelSpeedupTraining, uuid: 301, left: time.Hour}}
			}
			f := newSpeedFake(tc.day(), c)
			tc.knob(f)
			sent, err := runSpeed(t, f, c, false)
			if err == nil || !strings.Contains(err.Error(), tc.cause) {
				t.Errorf("err %v, want a tripwire naming %q", err, tc.cause)
			}
			if len(sent) != 1 || duelSpeedupsTripped() == "" {
				t.Errorf("sent %d, tripped %q: want one call and a session trip", len(sent), duelSpeedupsTripped())
			}
		})
	}
}

func TestDuelSpeedupsNeedsInitGold(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), noGold: true}
	sent, err := runSpeed(t, newSpeedFake(speedTue(), c), c, false)
	if err == nil || !strings.Contains(err.Error(), "user.gold") || len(sent) != 0 {
		t.Errorf("without init user.gold nothing may be spent: sent %v, err %v", sentStrings(sent), err)
	}
}

func TestDuelSpeedupsBenignStopsEndOnlyThatJob(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: []speedJob{
		{kind: duelSpeedupBuild, uuid: 101, left: 3 * time.Hour},
		{kind: duelSpeedupBuild, uuid: 102, left: 2 * time.Hour},
		{kind: duelSpeedupBuild, uuid: 103, left: time.Hour},
	}, items: map[int64]int64{build1h: 9}, gold: 1000}
	f := newSpeedFake(speedTue(), c)
	f.errFor[101] = duelSpeedupsDoneCode
	f.errFor[102] = duelSpeedupsNoItemsCode
	sent, err := runSpeed(t, f, c, false)
	if err != nil {
		t.Fatalf("benign stops must not fail the run: %v", err)
	}
	wantSent(t, sent, "build.ccd.m.new 101 200213;3", "build.ccd.m.new 102 200213;2", "build.ccd.m.new 103 200213;1")

	// A real failure stops that job too and is reported, the rest still run.
	clearSpeedTrip(t)
	f = newSpeedFake(speedTue(), c)
	f.errFor[101] = "E999999"
	sent, err = runSpeed(t, f, c, false)
	if err == nil || len(sent) != 3 {
		t.Errorf("a failed job must be reported and the others still sped up: sent %v, err %v", sentStrings(sent), err)
	}
}

func TestDuelSpeedupsDoesNotStopAtTheLastChest(t *testing.T) {
	clearSpeedTrip(t)
	day := speedTue()
	us := sfs.NewSFSObject()
	us.PutLong("score", 9_000_000) // past every chest (target 540000)
	day.PutSFSObject("userScore", us)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(day, c), c, false)
	if err != nil || len(sent) != 1 {
		t.Errorf("speed-ups must continue past the chests: sent %v, err %v", sentStrings(sent), err)
	}
}

func TestDuelSpeedupsRereadsRemainingFromEachReply(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: []speedJob{{kind: duelSpeedupBuild, uuid: 101, left: 2 * time.Hour}}, items: map[int64]int64{build1h: 5}, gold: 1000}
	f := newSpeedFake(speedTue(), c)
	first := true
	f.takeOff = func(ms int64) int64 { // the first call takes off only half
		if first {
			first = false
			return ms / 2
		}
		return ms
	}
	sent, err := runSpeed(t, f, c, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, "build.ccd.m.new 101 200213;2", "build.ccd.m.new 101 200213;1")

	for name, knob := range map[string]func(f *speedFake){
		"no end time": func(f *speedFake) { f.noEnd = true },
		"no progress": func(f *speedFake) { f.noProgress = true },
	} {
		t.Run(name, func(t *testing.T) {
			clearSpeedTrip(t)
			f := newSpeedFake(speedTue(), c)
			f.takeOff = func(ms int64) int64 { return ms / 2 }
			knob(f)
			sent, err := runSpeed(t, f, c, false)
			if err != nil || len(sent) != 1 {
				t.Errorf("a reply without usable time must end the job after one call: sent %v, err %v", sentStrings(sent), err)
			}
		})
	}
}

func TestDuelSpeedupsCallCap(t *testing.T) {
	clearSpeedTrip(t)
	var jobs []speedJob
	for i := range duelSpeedupsMaxCalls + 5 {
		jobs = append(jobs, speedJob{kind: duelSpeedupBuild, uuid: int64(1000 + i), left: 30 * time.Second})
	}
	c := speedInit{jobs: jobs, items: map[int64]int64{build1m: 1000}, gold: 1000}
	sent, err := runSpeed(t, newSpeedFake(speedTue(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(sent) != duelSpeedupsMaxCalls {
		t.Errorf("sent %d speed-ups, want the cap %d", len(sent), duelSpeedupsMaxCalls)
	}
}

func TestDuelSpeedupsOnlyRunningJobs(t *testing.T) {
	clearSpeedTrip(t)
	shieldEnd := evTestNow.Add(24 * time.Hour)
	c := speedInit{jobs: []speedJob{
		{kind: duelSpeedupBuild, uuid: 101, left: -time.Minute},                                   // finished
		{kind: duelSpeedupBuild, uuid: 102, left: time.Hour, state: duelBuildingFoldUp},           // folded up
		{kind: duelSpeedupBuild, uuid: 103, left: time.Hour, bID: claimHQBuildingID, lv: 7},       // HQ 7 -> 8 under shield
		{kind: duelSpeedupResearch, uuid: 201, left: time.Hour, qStart: evTestNow.Add(time.Hour)}, // sT == uT: done
		{kind: duelSpeedupHeal, uuid: 401, left: -time.Second},                                    // finished
		{kind: duelSpeedupTraining, uuid: 301, left: 0},                                           // not training
	}, items: map[int64]int64{build1h: 5, res1h: 5, heal1h: 5, train1h: 5, uni1h: 5}, gold: 1000, shield: &shieldEnd}
	for _, day := range []func() *sfs.SFSObject{speedTue, speedWed, speedFri, speedSat} {
		sent, err := runSpeed(t, newSpeedFake(day(), c), c, false)
		if err != nil || len(sent) != 0 {
			t.Errorf("a job that isn't running was sped up: %v (err %v)", sentStrings(sent), err)
		}
	}
	// Without defend_wall the HQ 7 upgrade is also left alone; above HQ 7 it is sped up.
	c.shield = nil
	c.jobs = []speedJob{
		{kind: duelSpeedupBuild, uuid: 103, left: time.Hour, bID: claimHQBuildingID, lv: 7},
		{kind: duelSpeedupBuild, uuid: 104, left: time.Hour, bID: claimHQBuildingID, lv: 20},
	}
	sent, err := runSpeed(t, newSpeedFake(speedTue(), c), c, false)
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, sent, "build.ccd.m.new 104 200213;1")
}

func TestDuelSpeedupsDeadConnectionStops(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	f := newSpeedFake(speedSat(), c)
	f.closeOnSpeed = true
	sent, err := runSpeed(t, f, c, false)
	if err == nil || !session.ContainsNonTimeoutNetError(err) {
		t.Errorf("a dropped connection must be reported as such: %v", err)
	}
	if len(sent) != 1 {
		t.Errorf("sent %d speed-ups on a dead connection, want 1", len(sent))
	}
}

func TestDuelSpeedupsPlanSendsNothing(t *testing.T) {
	clearSpeedTrip(t)
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	f := newSpeedFake(speedFri(), c)
	sent, err := runSpeed(t, f, c, true)
	if err != nil || len(sent) != 0 {
		t.Errorf("the plan must send no speed-up: %v (err %v)", sentStrings(sent), err)
	}
	if !maps.Equal(f.items, c.items) {
		t.Error("the plan changed the bag")
	}
}

func TestDuelSpeedupsRegistration(t *testing.T) {
	f := featureRegistry["duel-speedups"]
	if f.DefaultOn || !f.Hold || !strings.HasPrefix(f.Summary, "OPT-IN") {
		t.Errorf("duel-speedups must be opt-in, off by default and held: %+v", f)
	}
	want := []DuelScore{{DuelScoreSpeedUp, DuelQueueBuild}, {DuelScoreSpeedUp, DuelQueueResearch},
		{DuelScoreSpeedUp, DuelQueueTraining}, {DuelScoreSpeedUp, DuelQueueHeal}}
	if !slices.Equal(f.Duel, want) {
		t.Errorf("Duel = %v, want %v", f.Duel, want)
	}
	plan := featureRegistry["duel-speedups-plan"]
	if plan.DefaultOn || plan.Hold || len(plan.Duel) != 0 {
		t.Errorf("duel-speedups-plan must be off by default and never held: %+v", plan)
	}
}
