package game

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// Hero templates by lw_hero quality, and dispatch task rows by color (lw_dispatch_tasks).
const (
	stHeroUR  = 50006 // quality 5
	stHeroSSR = 40006 // quality 4
	stHeroSR  = 30002 // quality 3

	stTaskUR     = 500101 // UR: 2;4;1|3;6;1|4;32;1
	stTaskURHard = 500301 // UR: 2;4;2|3;11;2|4;80;2
	stTaskSSR    = 400101 // SSR: 2;4;1|4;18;1
	stTaskSR     = 300101 // SR: 4;15;1

	stHome = 200101 // tile (100, 200)
	stNear = 204104 // tile (103, 204): 5 tiles from home
)

type stHeroRow struct{ uuid, heroID, lev, rank int64 }

type stTask struct {
	uuid, cfgID, point, completion int64
	heroes                         []int64
}

// stInitOpts shapes stInit: formations army formations, init user.gold (omitted when < 0), the
// main city tile index, and whether activity and army_formation are present at all.
type stInitOpts struct {
	formations int
	gold, home int64
	noActivity bool
	noForms    bool
}

// stInit is an HQ-10 init with DispatchTask activity 94101, the main city at stHome, the given
// number of army formations, init user.gold (omitted when gold < 0) and heroes.
func stInit(formations int, gold int64, heroes ...stHeroRow) *Init {
	return stInitWith(stInitOpts{formations: formations, gold: gold, home: stHome}, heroes...)
}

func stInitWith(o stInitOpts, heroes ...stHeroRow) *Init {
	return rewardInitWithDay(10, func(raw *sfs.SFSObject) {
		if !o.noActivity {
			acts := sfs.NewSFSObject()
			acts.PutSFSObject("94101", sfs.NewSFSObject())
			raw.PutSFSObject("activity", acts)
		}
		raw.PutInt("worldMainPoint", int32(o.home))
		if !o.noForms {
			forms := sfs.NewSFSArray()
			for i := range o.formations {
				f := sfs.NewSFSObject()
				f.PutLong("uuid", int64(700+i))
				f.PutInt("index", int32(i+1))
				forms.AddSFSObject(f)
			}
			raw.PutSFSArray("army_formation", forms)
		}
		if o.gold >= 0 {
			user := sfs.NewSFSObject()
			user.PutLong("gold", o.gold)
			raw.PutSFSObject("user", user)
		}
		arr := sfs.NewSFSArray()
		for _, h := range heroes {
			e := sfs.NewSFSObject()
			e.PutLong("uuid", h.uuid)
			e.PutInt("heroId", int32(h.heroID))
			e.PutInt("lev", int32(h.lev))
			e.PutInt("rankLv", int32(h.rank))
			arr.AddSFSObject(e)
		}
		raw.PutSFSArray("userHero", arr)
	})
}

// stServer answers hero.dispatch.list from its task list and hero.dispatch.start by marking the
// task started. listGold[n] is the gold of the n-th list reply (the last value repeats; none
// omits it); startGold, when >= 0, is put on the start reply; startErr makes the n-th start fail.
type stServer struct {
	tasks     []stTask
	listGold  []int64
	startGold int64
	startErr  map[int]string
	lists     int
	starts    int
}

func (s *stServer) reply(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	switch cmd {
	case secretTasksListCmd:
		resp := sfs.NewSFSObject()
		if len(s.listGold) > 0 {
			resp.PutLong("gold", s.listGold[min(s.lists, len(s.listGold)-1)])
		}
		s.lists++
		arr := sfs.NewSFSArray()
		for _, task := range s.tasks {
			o := sfs.NewSFSObject()
			o.PutLong("uuid", task.uuid)
			o.PutInt("cfgId", int32(task.cfgID))
			o.PutInt("pointId", int32(task.point))
			o.PutLong("completionTime", task.completion)
			o.PutInt("rewarded", 0)
			o.PutValue("heroList", claimLongArray(task.heroes))
			arr.AddSFSObject(o)
		}
		resp.PutSFSArray("ls", arr)
		return resp
	case secretTaskStartsCmd:
		s.starts++
		if code := s.startErr[s.starts]; code != "" {
			return rewardErr(code)
		}
		uuid := p.GetLong("uuid")
		for i := range s.tasks {
			if s.tasks[i].uuid == uuid {
				s.tasks[i].completion = time.Now().Add(time.Hour).UnixMilli()
				s.tasks[i].heroes = claimInts(p, "heroList")
			}
		}
		resp := sfs.NewSFSObject()
		data := sfs.NewSFSObject()
		data.PutLong("uuid", uuid)
		resp.PutSFSObject("data", data)
		if s.startGold >= 0 {
			resp.PutLong("gold", s.startGold)
		}
		return resp
	}
	return rewardErr("999999")
}

func stRun(t *testing.T, in *Init, s *stServer, dryRun bool) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, s.reply)
	err := secretTaskStartsRun(conn, in, dryRun)
	for _, c := range fake.cmds() {
		if c != secretTasksListCmd && c != secretTaskStartsCmd {
			t.Errorf("sent %s; only hero.dispatch.list and hero.dispatch.start are allowed", c)
		}
	}
	return fake, err
}

func stStarts(fake *rewardFake) []*sfs.SFSObject {
	var out []*sfs.SFSObject
	for _, r := range fake.requests() {
		if r.Cmd == secretTaskStartsCmd {
			out = append(out, r.Params)
		}
	}
	return out
}

func stRunning(uuid int64, heroes ...int64) stTask {
	return stTask{uuid: uuid, cfgID: stTaskUR, point: stNear, completion: time.Now().Add(time.Hour).UnixMilli(), heroes: heroes}
}

func TestSecretTaskStartsStartsURWithIdleHeroes(t *testing.T) {
	in := stInit(2, 1000,
		stHeroRow{1, stHeroUR, 40, 7},   // meets all three conditions of stTaskUR alone
		stHeroRow{2, stHeroSR, 10, 1},   // meets none
		stHeroRow{3, stHeroUR, 100, 20}, // stronger, but busy on task 12
	)
	s := &stServer{
		tasks: []stTask{
			{uuid: 11, cfgID: stTaskUR, point: stNear},
			stRunning(12, 3),
			{uuid: 13, cfgID: stTaskSSR, point: stNear},
			{uuid: 14, cfgID: stTaskSR, point: stNear},
		},
		listGold:  []int64{1000},
		startGold: -1,
	}
	fake, err := stRun(t, in, s, false)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{secretTasksListCmd, secretTaskStartsCmd, secretTasksListCmd}) {
		t.Fatalf("sent %v, want list, one start, list", got)
	}
	p := stStarts(fake)[0]
	if p.GetLong("uuid") != 11 {
		t.Errorf("started task %d, want the UR task 11", p.GetLong("uuid"))
	}
	v, _ := p.Get("heroList")
	if v.Type != sfsLongArrayTag {
		t.Errorf("heroList type %d, want LongArray", v.Type)
	}
	if got, _ := v.Val.([]int64); !slices.Equal(got, []int64{1}) {
		t.Errorf("heroList = %v, want [1] (hero 3 is on task 12)", v.Val)
	}
	// 5 tiles at armyspeed 3: ceil(5000 / 3) = 1667 ms, plus the serializer's 500.
	if got := p.GetLong("marchDuration"); got != 2167 {
		t.Errorf("marchDuration = %d, want 2167", got)
	}
}

func TestSecretTaskStartsSkipsNonUR(t *testing.T) {
	in := stInit(2, 1000, stHeroRow{1, stHeroUR, 120, 25}, stHeroRow{2, stHeroUR, 120, 25})
	s := &stServer{tasks: []stTask{{uuid: 13, cfgID: stTaskSSR, point: stNear}, {uuid: 14, cfgID: stTaskSR, point: stNear}, {uuid: 15, cfgID: 999, point: stNear}}, listGold: []int64{1000}, startGold: -1}
	fake, err := stRun(t, in, s, false)
	if err != nil || !slices.Equal(fake.cmds(), []string{secretTasksListCmd}) {
		t.Errorf("run = %v, sent %v; want only the list (SSR, SR and unknown tasks are skipped)", err, fake.cmds())
	}
}

func TestSecretTaskStartsSkipsUnmetConditions(t *testing.T) {
	// stTaskURHard needs two heroes of quality >= 4, rank >= 11 and level >= 80; one qualifies and
	// the other strong hero is busy.
	in := stInit(2, 1000,
		stHeroRow{1, stHeroUR, 90, 12},
		stHeroRow{2, stHeroSSR, 79, 12},
		stHeroRow{3, stHeroUR, 90, 12},
	)
	s := &stServer{tasks: []stTask{{uuid: 11, cfgID: stTaskURHard, point: stNear}, stRunning(12, 3)}, listGold: []int64{1000}, startGold: -1}
	fake, err := stRun(t, in, s, false)
	if err != nil || len(stStarts(fake)) != 0 {
		t.Errorf("run = %v, sent %v; want no start", err, fake.cmds())
	}
}

func TestSecretTaskStartsNeedsAFreeSlot(t *testing.T) {
	heroes := []stHeroRow{{1, stHeroUR, 40, 7}, {2, stHeroUR, 40, 7}, {3, stHeroUR, 40, 7}}
	tasks := func() []stTask {
		return []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, stRunning(12, 2), stRunning(13, 3)}
	}
	// One formation: 2 slots, both running.
	fake, err := stRun(t, stInit(1, 1000, heroes...), &stServer{tasks: tasks(), listGold: []int64{1000}, startGold: -1}, false)
	if err != nil || len(stStarts(fake)) != 0 {
		t.Errorf("full slots: run = %v, sent %v; want no start", err, fake.cmds())
	}
	// LW_DISPATCH_TASK_ADD_SEQ in init effect adds a slot.
	in := stInit(1, 1000, heroes...)
	eff := sfs.NewSFSObject()
	eff.PutInt("50212", 1)
	in.Raw.PutSFSObject("effect", eff)
	fake, err = stRun(t, in, &stServer{tasks: tasks(), listGold: []int64{1000}, startGold: -1}, false)
	if err != nil || len(stStarts(fake)) != 1 {
		t.Errorf("effect slot: run = %v, sent %v; want one start", err, fake.cmds())
	}
}

func TestSecretTaskStartsCapsStartsPerRun(t *testing.T) {
	var heroes []stHeroRow
	var tasks []stTask
	for i := range int64(4) {
		heroes = append(heroes, stHeroRow{1 + i, stHeroUR, 40, 7})
		tasks = append(tasks, stTask{uuid: 11 + i, cfgID: stTaskUR, point: stNear})
	}
	fake, err := stRun(t, stInit(4, 1000, heroes...), &stServer{tasks: tasks, listGold: []int64{1000}, startGold: -1}, false)
	starts := stStarts(fake)
	if err != nil || len(starts) != secretTaskStartsMaxPerRun {
		t.Fatalf("run = %v, %d starts; want %d", err, len(starts), secretTaskStartsMaxPerRun)
	}
	a, _ := starts[0].Get("heroList")
	b, _ := starts[1].Get("heroList")
	if slices.Equal(a.Val.([]int64), b.Val.([]int64)) {
		t.Errorf("both starts used heroes %v; the second must see the first's heroes as busy", a.Val)
	}
}

func TestSecretTaskStartsDiamondTripwire(t *testing.T) {
	heroes := []stHeroRow{{1, stHeroUR, 40, 7}, {2, stHeroUR, 40, 7}}
	tasks := func() []stTask {
		return []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, {uuid: 12, cfgID: stTaskUR, point: stNear}}
	}
	for name, s := range map[string]*stServer{
		"list after the start": {tasks: tasks(), listGold: []int64{1000, 900}, startGold: -1},
		"start reply":          {tasks: tasks(), listGold: []int64{1000}, startGold: 999},
	} {
		t.Run(name, func(t *testing.T) {
			fake, err := stRun(t, stInit(2, 1000, heroes...), s, false)
			if !errors.Is(err, errSecretTaskStartsDiamonds) {
				t.Errorf("run = %v, want the diamond tripwire", err)
			}
			if n := len(stStarts(fake)); n != 1 {
				t.Errorf("%d starts, want the run to stop after the first", n)
			}
		})
	}
}

func TestSecretTaskStartsBalanceChecks(t *testing.T) {
	heroes := []stHeroRow{{1, stHeroUR, 40, 7}, {2, stHeroUR, 40, 7}}
	tasks := func() []stTask {
		return []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, {uuid: 12, cfgID: stTaskUR, point: stNear}}
	}
	// No balance in the list or init: nothing to compare against, so nothing starts.
	fake, err := stRun(t, stInit(2, -1, heroes...), &stServer{tasks: tasks(), startGold: -1}, false)
	if err != nil || len(stStarts(fake)) != 0 {
		t.Errorf("no balance: run = %v, sent %v; want no start", err, fake.cmds())
	}
	// Init has a balance but no reply carries one: the first start can't be verified, so the run
	// stops there.
	fake, err = stRun(t, stInit(2, 1000, heroes...), &stServer{tasks: tasks(), startGold: -1}, false)
	if err != nil || len(stStarts(fake)) != 1 {
		t.Errorf("unverifiable: run = %v, %d starts; want one start and no error", err, len(stStarts(fake)))
	}
	// A rising balance is fine.
	fake, err = stRun(t, stInit(2, 1000, heroes...), &stServer{tasks: tasks(), listGold: []int64{1000, 1050}, startGold: -1}, false)
	if err != nil || len(stStarts(fake)) != 2 {
		t.Errorf("rising: run = %v, %d starts; want two", err, len(stStarts(fake)))
	}
}

func TestSecretTaskStartsFailedStartKeepsGoing(t *testing.T) {
	heroes := []stHeroRow{{1, stHeroUR, 40, 7}, {2, stHeroUR, 40, 7}}
	s := &stServer{
		tasks:     []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, {uuid: 12, cfgID: stTaskUR, point: stNear}},
		listGold:  []int64{1000},
		startGold: -1,
		startErr:  map[int]string{1: "999999"},
	}
	fake, err := stRun(t, stInit(2, 1000, heroes...), s, false)
	starts := stStarts(fake)
	if err == nil || len(starts) != 2 || starts[1].GetLong("uuid") != 12 {
		t.Errorf("run = %v, sent %v; want the failure reported and task 12 still started", err, fake.cmds())
	}
}

func TestSecretTaskStartsPlanIsReadOnly(t *testing.T) {
	in := stInit(2, 1000, stHeroRow{1, stHeroUR, 40, 7}, stHeroRow{2, stHeroUR, 40, 7})
	s := &stServer{tasks: []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, {uuid: 12, cfgID: stTaskUR, point: stNear}}, startGold: -1}
	buf := captureRewardLogs(t)
	fake, err := stRun(t, in, s, true)
	if err != nil || !slices.Equal(fake.cmds(), []string{secretTasksListCmd}) {
		t.Errorf("plan: run = %v, sent %v; want only the list", err, fake.cmds())
	}
	if got := strings.Count(buf.String(), "WOULD start"); got != 2 {
		t.Errorf("logged %d WOULD-start lines, want 2:\n%s", got, buf.String())
	}
}

func TestSecretTaskStartsGates(t *testing.T) {
	hero := stHeroRow{1, stHeroUR, 40, 7}
	base := stInitOpts{formations: 2, gold: 1000, home: stHome}
	noHome, noForms, noAct := base, base, base
	noHome.home = 0
	noForms.noForms = true
	noAct.noActivity = true
	for name, in := range map[string]*Init{
		"no init":           {},
		"no activity":       stInitWith(noAct, hero),
		"no main city":      stInitWith(noHome, hero),
		"no army formation": stInitWith(noForms, hero),
		"no heroes":         stInit(2, 1000),
		"unknown hero id":   stInit(2, 1000, stHeroRow{1, 1, 40, 7}),
	} {
		t.Run(name, func(t *testing.T) {
			s := &stServer{tasks: []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}}, listGold: []int64{1000}, startGold: -1}
			fake, err := stRun(t, in, s, false)
			if err != nil || len(fake.cmds()) != 0 {
				t.Errorf("run = %v, sent %v; want nothing", err, fake.cmds())
			}
		})
	}
}

func TestSecretTaskStartsMarchMs(t *testing.T) {
	for _, c := range []struct {
		home, point, want int64
		ok                bool
	}{
		{stHome, stNear, 1667, true},  // 5 tiles
		{stHome, stHome, 0, true},     // same tile
		{1, 1002, 472, true},          // (0,0) -> (1,1): ceil(1414.21 / 3)
		{1, 1_000_000, 470934, true},  // corner to corner: ceil(999*sqrt(2)*1000 / 3)
		{0, stNear, 0, false},         // below the map
		{stHome, 1_000_001, 0, false}, // past the map
		{-5, -5, 0, false},
	} {
		got, ok := secretTaskStartsMarchMs(c.home, c.point)
		if got != c.want || ok != c.ok {
			t.Errorf("marchMs(%d, %d) = %d, %v; want %d, %v", c.home, c.point, got, ok, c.want, c.ok)
		}
	}
}

func TestSecretTaskStartsConditionsAndTables(t *testing.T) {
	conds, ok := secretTaskStartsConditions(secretTaskStartsCfgs[stTaskURHard].conds)
	want := map[int]secretTaskStartsCond{2: {4, 2}, 3: {11, 2}, 4: {80, 2}}
	if !ok || len(conds) != 3 || conds[2] != want[2] || conds[3] != want[3] || conds[4] != want[4] {
		t.Errorf("conditions = %v, %v; want %v", conds, ok, want)
	}
	for _, bad := range []string{"", "1;2;1", "5;1;1", "2;4", "2;x;1"} {
		if _, ok := secretTaskStartsConditions(bad); ok {
			t.Errorf("conditions(%q) ok; want it refused", bad)
		}
	}
	if len(secretTaskStartsCfgs) != 328 || len(secretTaskStartsQualityByHero) != 393 {
		t.Errorf("tables: %d tasks, %d heroes; want 328 and 393", len(secretTaskStartsCfgs), len(secretTaskStartsQualityByHero))
	}
	for id, color := range map[int64]int64{stTaskUR: 5, stTaskURHard: 5, stTaskSSR: 4, stTaskSR: 3} {
		if secretTaskStartsCfgs[id].color != color {
			t.Errorf("task %d color %d, want %d", id, secretTaskStartsCfgs[id].color, color)
		}
	}
	for id, q := range map[int64]int64{stHeroUR: 5, stHeroSSR: 4, stHeroSR: 3} {
		if secretTaskStartsQualityByHero[id] != q {
			t.Errorf("hero %d quality %d, want %d", id, secretTaskStartsQualityByHero[id], q)
		}
	}
}

func TestSecretTaskStartsRecommend(t *testing.T) {
	h := func(uuid, q, lev, rank int64) secretTaskStartsHero {
		return secretTaskStartsHero{uuid: uuid, quality: q, level: lev, rank: rank}
	}
	hard := map[int]secretTaskStartsCond{2: {4, 2}, 3: {11, 2}, 4: {80, 2}}
	for name, c := range map[string]struct {
		heroes []secretTaskStartsHero
		used   map[int64]bool
		conds  map[int]secretTaskStartsCond
		want   []int64
	}{
		// Each condition has exactly the heroes it needs, so all are taken.
		"exact fit": {[]secretTaskStartsHero{h(1, 5, 90, 12), h(2, 4, 80, 11), h(3, 3, 10, 1)}, nil, hard, []int64{1, 2}},
		// The weakest full match is preferred (score, then quality, then the power stand-in).
		"weakest first": {[]secretTaskStartsHero{h(1, 5, 120, 25), h(2, 4, 40, 7), h(3, 5, 33, 6)}, nil,
			map[int]secretTaskStartsCond{2: {4, 1}, 3: {6, 1}, 4: {32, 1}}, []int64{2}},
		"busy hero excluded": {[]secretTaskStartsHero{h(1, 5, 90, 12), h(2, 4, 80, 11), h(3, 5, 90, 12)}, map[int64]bool{1: true}, hard, []int64{2, 3}},
		"impossible":         {[]secretTaskStartsHero{h(1, 5, 90, 12), h(2, 4, 79, 11)}, nil, hard, nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := secretTaskStartsRecommend(c.heroes, c.used, c.conds)
			if !slices.Equal(got, c.want) {
				t.Errorf("recommend = %v, want %v", got, c.want)
			}
		})
	}
}

func TestSecretTaskStartsMeetsAll(t *testing.T) {
	conds := map[int]secretTaskStartsCond{2: {4, 1}, 4: {32, 2}}
	a := secretTaskStartsHero{uuid: 1, quality: 5, level: 40}
	b := secretTaskStartsHero{uuid: 2, quality: 3, level: 32}
	c := secretTaskStartsHero{uuid: 3, quality: 3, level: 31}
	for _, tc := range []struct {
		sel  []secretTaskStartsHero
		used map[int64]bool
		want bool
	}{
		{[]secretTaskStartsHero{a, b}, nil, true},
		{[]secretTaskStartsHero{a, c}, nil, false},                     // only one hero at level 32+
		{[]secretTaskStartsHero{b, b}, nil, false},                     // duplicate
		{[]secretTaskStartsHero{a, b}, map[int64]bool{2: true}, false}, // busy
		{[]secretTaskStartsHero{a, b, c, {uuid: 4}}, nil, false},       // four heroes
		{nil, nil, false},
	} {
		if got := secretTaskStartsMeetsAll(tc.sel, tc.used, conds); got != tc.want {
			t.Errorf("meetsAll(%v, used %v) = %v, want %v", tc.sel, tc.used, got, tc.want)
		}
	}
}

func TestSecretTaskStartsRegistration(t *testing.T) {
	f, ok := featureRegistry[secretTaskStartsName]
	if !ok || f.DefaultOn || f.Hold || !strings.HasPrefix(f.Summary, "OPT-IN") ||
		!slices.Equal(f.Duel, []DuelScore{{Type: DuelScoreURSecretTask}}) {
		t.Errorf("%s registered as %+v; want opt-in, off by default, not held, scoring type 99", secretTaskStartsName, f)
	}
	if held, _ := duelHeld(nil, &Init{}, f, nil); held {
		t.Error("an unstarted task does not survive the reset, so it must never be held")
	}
	if p, ok := featureRegistry[secretTaskStartsPlanName]; !ok || p.DefaultOn || len(p.Duel) != 0 {
		t.Errorf("%s registered as %+v; want an off-by-default read-only preview", secretTaskStartsPlanName, p)
	}
}
