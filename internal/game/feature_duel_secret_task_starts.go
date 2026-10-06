package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Secret Tasks (hero dispatch), start half: start today's not-started UR tasks with idle heroes,
// because a task left unstarted is gone after the daily reset (en:456211; DUEL.md §5) and "Perform
// a UR secret task" (score 90204, type 99 value 5) is worth 75,000 duel points on Tuesday and
// Saturday (DUEL.md §2.2, §2.6, §4.2). The claim half is secret-tasks-claim. Static-only: not
// sent live yet.
//
// What the client does, and what this copies (Lua under SP/lua/src):
//   - Send: `hero.dispatch.start {uuid: Long, heroList: LongArray, marchDuration: Long}`, where the
//     serializer adds 500 to the march time it is given (DispatchStartMessage.lua:4-9). It has no
//     cost parameter. The only send site is UIFormationDispatchTaskView:OnStartClick (:420-442).
//   - OnStartClick refuses unless meetAllCondition holds (:425), the task is not started
//     (completionTime == 0, :428) and a dispatch slot is free (GetMaxMarch > GetSingleTaskIngCount,
//     :429-430). The task view also needs a map point (pointId > 0, DispatchTaskItem.lua:482).
//   - Conditions are lw_dispatch_tasks.conditions "type;value;num", parsed into
//     parsed_conditions[type] = {value, num} (ActDispatchTaskDataManager.lua:571-583). A set of at
//     most 3 heroes meets them when, for every type, at least num of the heroes have
//     heroType == value (type 1), quality >= value (2), rankLv >= value (3) or level >= value (4)
//     (RefreshSelectHero, UIFormationDispatchTaskView.lua:218-282). Type 1 needs the hero's army
//     type, which this does not carry (no task row uses it); any other type would make the client
//     itself fail, so either skips the task.
//   - Busy heroes are the heroList of every task with completionTime > 0 && rewarded == 0
//     (GetAllUsedHeroList, :584-592), finished-but-unclaimed ones included; the client checks
//     nothing else, marches included (en:456211 "Missions won't tie up your march queues").
//   - Slots: max = max_taskqueue (2 in all 116 lw_dispatch_settings rows) x the number of army
//     formations + effect LW_DISPATCH_TASK_ADD_SEQ 50212 (GetMaxMarch, :1059-1068;
//     ArmyFormationDataManager.lua:178-190, 350-357); running = tasks with completionTime > 0 &&
//     rewarded == 0 (:727-735). The effect is read from init effect only (evInitEffect), a lower
//     bound, so this can leave a slot unused but never exceeds the client's limit.
//   - Hero pick: the Quick Join auto-pick (UIFormationDispatchTaskCtrl:GetRecommendHeroList,
//     :48-90 -> FormationDispatchUtil.GetRecommendHeroList, FormationDispatchUtil.lua:106-276),
//     ported in secretTaskStartsRecommend; its result must still pass the meetAllCondition check.
//   - marchDuration: CalculateMarchTime (UIFormationDispatchTaskView.lua:412-419) is
//     ceil(dist * 1000 / armyspeed.k2) ms, dist the Euclidean distance between the main city's tile
//     and the task's tile (Vector2.Distance, Common/Tools/UnityEngine/Vector2.lua:118-120; tiles
//     from SceneUtils.IndexToTilePos, Util/SceneUtils.lua:252-286, with WorldTileCount 1000,
//     Global/ConstDefine.lua:260). The main city is init worldMainPoint (BuildManager.lua:167-168).
//
// Hard limits: never `hero.dispatch.refresh` (costType 1 is 100 diamonds), never a speed-up item,
// at most secretTaskStartsMaxPerRun start requests per run, and a diamond tripwire: the balance
// (hero.dispatch.list's gold, ActDispatchTaskDataManager.lua:363-366, else init user.gold,
// PlayerInfo.lua:612-613) is compared after every start with the start reply's gold or a fresh
// list's; any drop is an ERROR that stops the run. Not replicated, static-only: the client also
// refuses while on a Landlord center server (UIFormationDispatchTaskView.lua:421) or away from
// home (DispatchTaskItem.lua:477); a headless login is on its home server.
const (
	secretTaskStartsName      = "secret-tasks-start"
	secretTaskStartsPlanName  = "secret-tasks-start-plan"
	secretTaskStartsCmd       = "hero.dispatch.start"
	secretTaskStartsMaxPerRun = 2

	// secretTaskStartsMinColor is the lowest lw_dispatch_tasks.color started: 5 is UR, the
	// client's own "uninitiated UR task" test (DispatchTask.lua:639; en:456292). Lower it to 4 to
	// add SSR tasks and to 3 for SR as well; every row's conditions are already embedded below.
	secretTaskStartsMinColor = 5

	secretTaskStartsQueuePerFormation = 2     // lw_dispatch_settings.max_taskqueue, every row
	secretTaskStartsAddSeqEffect      = 50212 // EffectDefine.LW_DISPATCH_TASK_ADD_SEQ (EnumType.lua:4679)
	secretTaskStartsArmySpeed         = 3     // DataConfig armyspeed.k2 (table item)
	secretTaskStartsWorldTiles        = 1000  // WorldTileCount
	secretTaskStartsSerializerPad     = 500   // DispatchStartMessage.lua:8
	secretTaskStartsMaxHeroes         = 3     // the formation view's three hero slots
)

var secretTaskStartsColorNames = map[int64]string{3: "SR", 4: "SSR", 5: "UR"}

func init() {
	registerFeature(Feature{
		Name: secretTaskStartsName,
		Summary: "OPT-IN: start today's UR Secret Tasks (hero dispatch) with idle heroes before the daily reset loses them; " +
			"never refreshes or speeds up a task; stops on any diamond drop",
		// UR tasks score on Tue and Sat, but an unstarted task does not survive the daily reset,
		// so it is never held (DUEL.md §5, §7.2).
		Duel: []DuelScore{{Type: DuelScoreURSecretTask}},
		Run:  runSecretTaskStarts,
	})
	registerFeature(Feature{
		Name:    secretTaskStartsPlanName,
		Summary: "read-only preview of secret-tasks-start: logs which Secret Task it would start with which heroes; sends only hero.dispatch.list",
		Run:     runSecretTaskStartsPlan,
	})
}

func runSecretTaskStarts(conn *session.GameConn, in *Init) error {
	return secretTaskStartsRun(conn, in, false)
}

func runSecretTaskStartsPlan(conn *session.GameConn, in *Init) error {
	return secretTaskStartsRun(conn, in, true)
}

// secretTaskStartsHero is one owned hero with the fields the conditions test.
type secretTaskStartsHero struct {
	uuid, quality, level, rank int64
}

// meets reports whether the hero satisfies condition type typ with threshold value
// (RefreshSelectHero, UIFormationDispatchTaskView.lua:255-266).
func (h secretTaskStartsHero) meets(typ int, value int64) bool {
	switch typ {
	case 2:
		return h.quality >= value
	case 3:
		return h.rank >= value
	case 4:
		return h.level >= value
	}
	return false
}

// secretTaskStartsCond is one parsed_conditions entry: num heroes with at least value.
type secretTaskStartsCond struct{ value, num int64 }

// secretTaskStartsTask is one hero.dispatch.list ls[] entry.
type secretTaskStartsTask struct {
	uuid, cfgID, pointID, completion, rewarded int64
	heroes                                     []int64
}

type secretTaskStartsCfg struct {
	color int64
	conds string
}

// secretTaskStartsCfgs maps lw_dispatch_tasks id to its color and conditions.
var secretTaskStartsCfgs = func() map[int64]secretTaskStartsCfg {
	out := map[int64]secretTaskStartsCfg{}
	for key, ids := range secretTaskStartsTaskRows {
		color, conds, _ := strings.Cut(key, "/")
		c, _ := strconv.ParseInt(color, 10, 64)
		for _, id := range ids {
			out[int64(id)] = secretTaskStartsCfg{color: c, conds: conds}
		}
	}
	return out
}()

// secretTaskStartsQualityByHero maps lw_hero id to quality.
var secretTaskStartsQualityByHero = func() map[int64]int64 {
	out := map[int64]int64{}
	for q, ids := range secretTaskStartsHeroQuality {
		for _, id := range ids {
			out[int64(id)] = int64(q)
		}
	}
	return out
}()

// secretTaskStartsConditions parses a conditions cell. ok is false when a type is one this cannot
// evaluate (anything but 2, 3 and 4) or the cell is malformed.
func secretTaskStartsConditions(cell string) (map[int]secretTaskStartsCond, bool) {
	out := map[int]secretTaskStartsCond{}
	for _, part := range strings.Split(cell, "|") {
		f := strings.Split(part, ";")
		if len(f) != 3 {
			return nil, false
		}
		var n [3]int64
		for i, s := range f {
			v, err := strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, false
			}
			n[i] = v
		}
		if n[0] < 2 || n[0] > 4 {
			return nil, false
		}
		out[int(n[0])] = secretTaskStartsCond{value: n[1], num: n[2]}
	}
	return out, len(out) > 0
}

// secretTaskStartsHeroes reads init userHero (HeroDataManager.lua:121-124, 157-161; HeroInfo
// UpdateInfo, HeroInfo.lua:112-159: uuid, heroId -> lw_hero quality, lev, rankLv), sorted by
// uuid. A hero missing any of those, or whose heroId is not in lw_hero, is left out and counted in
// skipped: its conditions cannot be evaluated.
func secretTaskStartsHeroes(in *Init) (heroes []secretTaskStartsHero, skipped int) {
	for _, o := range claimObjectsOrMap(in.Raw, "userHero") {
		uuid, ok1 := claimFirstInt(o, "uuid", "heroUuid")
		id, ok2 := claimInt(o, "heroId")
		lev, ok3 := claimInt(o, "lev")
		rank, ok4 := claimInt(o, "rankLv")
		quality, ok5 := secretTaskStartsQualityByHero[id]
		if !ok1 || uuid == 0 || !ok2 || !ok3 || !ok4 || !ok5 {
			skipped++
			continue
		}
		heroes = append(heroes, secretTaskStartsHero{uuid: uuid, quality: quality, level: lev, rank: rank})
	}
	slices.SortFunc(heroes, func(a, b secretTaskStartsHero) int { return cmp.Compare(a.uuid, b.uuid) })
	return heroes, skipped
}

// secretTaskStartsMaxSlots is GetMaxMarch with the init effect only (see the doc comment). ok is
// false when init has no army_formation to count.
func secretTaskStartsMaxSlots(in *Init) (int, bool) {
	if !in.Raw.Has("army_formation") {
		return 0, false
	}
	formations := 0
	for _, f := range claimObjectsOrMap(in.Raw, "army_formation") {
		if u, ok := claimInt(f, "uuid"); ok && u != 0 {
			formations++
		}
	}
	return secretTaskStartsQueuePerFormation*formations + int(max(0, evInitEffect(in, secretTaskStartsAddSeqEffect))), true
}

func secretTaskStartsTasks(list *sfs.SFSObject) []secretTaskStartsTask {
	var out []secretTaskStartsTask
	for _, o := range claimObjects(list, "ls") {
		uuid, ok := claimInt(o, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		t := secretTaskStartsTask{uuid: uuid, heroes: claimInts(o, "heroList")}
		t.cfgID, _ = claimInt(o, "cfgId")
		t.pointID, _ = claimInt(o, "pointId")
		t.completion, _ = claimInt(o, "completionTime")
		t.rewarded, _ = claimInt(o, "rewarded")
		out = append(out, t)
	}
	return out
}

// secretTaskStartsBusy is GetSingleTaskIngCount and GetAllUsedHeroList over the list.
func secretTaskStartsBusy(tasks []secretTaskStartsTask) (running int, used map[int64]bool) {
	used = map[int64]bool{}
	for _, t := range tasks {
		if t.completion > 0 && t.rewarded == 0 {
			running++
			for _, h := range t.heroes {
				used[h] = true
			}
		}
	}
	return running, used
}

// secretTaskStartsTile is SceneUtils.IndexToTilePos for the world map; ok is false outside
// [1, 1000*1000], where the client logs an error and uses (0, 0).
func secretTaskStartsTile(index int64) (x, y float64, ok bool) {
	if index < 1 || index > secretTaskStartsWorldTiles*secretTaskStartsWorldTiles {
		return 0, 0, false
	}
	index--
	return float64(index % secretTaskStartsWorldTiles), float64(index / secretTaskStartsWorldTiles), true
}

// secretTaskStartsMarchMs is CalculateMarchTime: ceil(distance * 1000 / armyspeed.k2) ms. The
// request carries it plus secretTaskStartsSerializerPad.
func secretTaskStartsMarchMs(home, point int64) (int64, bool) {
	hx, hy, ok1 := secretTaskStartsTile(home)
	px, py, ok2 := secretTaskStartsTile(point)
	if !ok1 || !ok2 {
		return 0, false
	}
	dist := math.Sqrt((hx-px)*(hx-px) + (hy-py)*(hy-py))
	return int64(math.Ceil(dist * 1000 / secretTaskStartsArmySpeed)), true
}

// secretTaskStartsMeetsAll is the client's start gate on a hero set: 1 to 3 distinct idle heroes
// and, for every condition, at least num of them meeting it (meetAllCondition,
// UIFormationDispatchTaskView.lua:248-281).
func secretTaskStartsMeetsAll(sel []secretTaskStartsHero, used map[int64]bool, conds map[int]secretTaskStartsCond) bool {
	if len(sel) == 0 || len(sel) > secretTaskStartsMaxHeroes {
		return false
	}
	seen := map[int64]bool{}
	for _, h := range sel {
		if used[h.uuid] || seen[h.uuid] {
			return false
		}
		seen[h.uuid] = true
	}
	for typ, c := range conds {
		n := int64(0)
		for _, h := range sel {
			if h.meets(typ, c.value) {
				n++
			}
		}
		if n < c.num {
			return false
		}
	}
	return true
}

// secretTaskStartsPick is one recommendation candidate: heroWithScore in
// FormationDispatchUtil.lua:117-128.
type secretTaskStartsPick struct {
	secretTaskStartsHero
	score int
	ok    [5]bool
}

// secretTaskStartsNeed is one cond<N>Param of GetRecommendHeroList: count heroes still needed,
// heroCount candidates left.
type secretTaskStartsNeed struct {
	value, count, heroCount int64
	heroes                  map[int64]*secretTaskStartsPick
}

// secretTaskStartsNeeds holds cond1Param..cond4Param at indexes 1..4; nil once met or absent.
type secretTaskStartsNeeds [5]*secretTaskStartsNeed

func (c *secretTaskStartsNeeds) left() int {
	n := 0
	for k := 1; k <= 4; k++ {
		if c[k] != nil {
			n++
		}
	}
	return n
}

// take is CleanData2 (FormationDispatchUtil.lua:33-67).
func (c *secretTaskStartsNeeds) take(p *secretTaskStartsPick) {
	for k := 1; k <= 4; k++ {
		if p.ok[k] && c[k] != nil {
			c[k].count--
			c[k].heroCount--
			delete(c[k].heroes, p.uuid)
			if c[k].count == 0 {
				c[k] = nil
			}
		}
	}
}

// leftAfter is CleanData2Tmp (:68-94): the conditions still open if p were taken.
func (c *secretTaskStartsNeeds) leftAfter(p *secretTaskStartsPick) int {
	n := 0
	for k := 1; k <= 4; k++ {
		if c[k] != nil && (!p.ok[k] || c[k].count != 1) {
			n++
		}
	}
	return n
}

// matches is CalcMatchCount (:2-17); all is IsMatch (:18-32).
func (c *secretTaskStartsNeeds) matches(p *secretTaskStartsPick) (n int, all bool) {
	all = true
	for k := 1; k <= 4; k++ {
		if c[k] == nil {
			continue
		}
		if p.ok[k] {
			n++
		} else {
			all = false
		}
	}
	return n, all
}

// secretTaskStartsRecommend ports FormationDispatchUtil.GetRecommendHeroList (:106-276) as the
// dispatch view calls it (no special-hero check). It returns the chosen uuids sorted, or nil when
// the client's auto-pick would find none (tip 456224). Two divergences: heroes are visited in uuid
// order where the Lua walks a hash table, and the sort's last key, hero power, is computed
// client-side from hero properties that init does not carry, so level, then rank, then uuid
// (ascending, like power) stand in for it.
func secretTaskStartsRecommend(heroes []secretTaskStartsHero, used map[int64]bool, conds map[int]secretTaskStartsCond) []int64 {
	var need secretTaskStartsNeeds
	for k := 1; k <= 4; k++ {
		if c, ok := conds[k]; ok {
			need[k] = &secretTaskStartsNeed{value: c.value, count: c.num, heroes: map[int64]*secretTaskStartsPick{}}
		}
	}
	okList := map[int64]*secretTaskStartsPick{}
	for _, h := range heroes {
		if used[h.uuid] {
			continue
		}
		p := &secretTaskStartsPick{secretTaskStartsHero: h}
		for k := 1; k <= 4; k++ {
			if need[k] != nil && h.meets(k, need[k].value) {
				p.score++
				p.ok[k] = true
				need[k].heroes[h.uuid] = p
				need[k].heroCount++
			}
		}
		if p.score != 0 {
			okList[h.uuid] = p
		}
	}
	for k := 1; k <= 4; k++ {
		if need[k] != nil && need[k].heroCount < need[k].count {
			return nil
		}
	}
	ret := map[int64]*secretTaskStartsPick{}
	add := func(p *secretTaskStartsPick) {
		ret[p.uuid] = p
		delete(okList, p.uuid)
		need.take(p)
	}
	result := func() []int64 { return slices.Sorted(maps.Keys(ret)) }
	// A condition with exactly as many candidates as it needs takes them all (CleanData1, :95-105).
	for k := 1; k <= 4; k++ {
		if need[k] == nil || need[k].heroCount != need[k].count {
			continue
		}
		cands := slices.SortedFunc(maps.Values(need[k].heroes), func(a, b *secretTaskStartsPick) int { return cmp.Compare(a.uuid, b.uuid) })
		for _, p := range cands {
			if ret[p.uuid] == nil {
				add(p)
			}
		}
		need[k] = nil
	}
	if len(ret) > secretTaskStartsMaxHeroes {
		return nil
	}
	if need.left() == 0 {
		return result()
	}
	order := slices.Collect(maps.Values(okList))
	slices.SortFunc(order, func(a, b *secretTaskStartsPick) int {
		return cmp.Or(cmp.Compare(b.score, a.score), cmp.Compare(a.quality, b.quality),
			cmp.Compare(a.level, b.level), cmp.Compare(a.rank, b.rank), cmp.Compare(a.uuid, b.uuid))
	})
	for offset := 0; offset >= -4; offset-- {
		if left := need.left(); left > 0 {
			for _, p := range order {
				if ret[p.uuid] != nil || p.score+offset != left {
					continue
				}
				if _, all := need.matches(p); all {
					add(p)
					left = need.left()
				}
			}
		}
		if len(ret) > secretTaskStartsMaxHeroes {
			return nil
		}
		if need.left() == 0 {
			return result()
		}
	}
	cur := len(ret)
	for range 4 {
		var top *secretTaskStartsPick
		best := 0
		for _, p := range order {
			if ret[p.uuid] != nil {
				continue
			}
			if n, _ := need.matches(p); best < n {
				top, best = p, n
			}
		}
		if top == nil {
			continue
		}
		if need.leftAfter(top) == 0 {
			add(top)
			break
		}
		if cur < 2 {
			cur++
			add(top)
		}
	}
	if len(ret) > secretTaskStartsMaxHeroes || need.left() != 0 {
		return nil
	}
	return result()
}

// secretTaskStartsDecide plans one not-started task: the heroes and the marchDuration to send, or
// why it is skipped.
func secretTaskStartsDecide(t secretTaskStartsTask, heroes []secretTaskStartsHero, used map[int64]bool, home int64) (pick []int64, march int64, skip string) {
	cfg := secretTaskStartsCfgs[t.cfgID]
	conds, ok := secretTaskStartsConditions(cfg.conds)
	if !ok {
		return nil, 0, "its conditions (" + cfg.conds + ") include a type this cannot evaluate"
	}
	march, ok = secretTaskStartsMarchMs(home, t.pointID)
	if !ok {
		return nil, 0, fmt.Sprintf("its map point %d (or the main city's %d) is not a world tile", t.pointID, home)
	}
	pick = secretTaskStartsRecommend(heroes, used, conds)
	byUUID := map[int64]secretTaskStartsHero{}
	for _, h := range heroes {
		byUUID[h.uuid] = h
	}
	var sel []secretTaskStartsHero
	for _, u := range pick {
		sel = append(sel, byUUID[u])
	}
	if pick == nil || !secretTaskStartsMeetsAll(sel, used, conds) {
		return nil, 0, "no set of idle heroes meets its conditions (" + cfg.conds + ")"
	}
	return pick, march + secretTaskStartsSerializerPad, ""
}

// secretTaskStartsGold reads the diamond balance from a hero.dispatch.list reply, else init
// user.gold (only that field of user is read; user holds PII). src names where it came from.
func secretTaskStartsGold(list *sfs.SFSObject, in *Init) (gold int64, src string, ok bool) {
	if g, ok := claimInt(list, "gold"); ok {
		return g, "hero.dispatch.list", true
	}
	if g, ok := claimInt(in.Object("user"), "gold"); ok {
		return g, "init user.gold", true
	}
	return 0, "", false
}

// errSecretTaskStartsDiamonds is returned when the diamond balance fell after a start.
var errSecretTaskStartsDiamonds = errors.New("diamond balance fell after hero.dispatch.start")

func secretTaskStartsRun(conn *session.GameConn, in *Init, dryRun bool) error {
	name := secretTaskStartsName
	if dryRun {
		name = secretTaskStartsPlanName
	}
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	if hq := claimHQLevel(in); hq < secretTasksNeedHQ || !claimHasActivity(in, secretTasksActivities...) {
		slog.Info(name+": DispatchTask activity not open", "hqLevel", hq)
		return nil
	}
	home, ok := claimInt(in.Raw, "worldMainPoint")
	if _, _, tile := secretTaskStartsTile(home); !ok || !tile {
		slog.Info(name+": init has no usable worldMainPoint, so no march time; skipping", "worldMainPoint", home)
		return nil
	}
	heroes, unreadable := secretTaskStartsHeroes(in)
	if unreadable > 0 {
		slog.Info(name+": heroes left out because init userHero lacks heroId/lev/rankLv or lw_hero lacks the id", "count", unreadable)
	}
	if len(heroes) == 0 {
		slog.Info(name + ": no usable heroes in init userHero; skipping")
		return nil
	}
	maxSlots, ok := secretTaskStartsMaxSlots(in)
	if !ok {
		slog.Info(name + ": init has no army_formation, so the dispatch slot limit is unknown; skipping")
		return nil
	}
	list, err := session.SendAndWait(conn, "secret task list", secretTasksListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	gold, goldSrc, goldOK := secretTaskStartsGold(list.Params, in)
	tasks := secretTaskStartsTasks(list.Params)
	running, used := secretTaskStartsBusy(tasks)
	slog.Info(name+": state", "tasks", len(tasks), "running", running, "maxSlots", maxSlots, "busyHeroes", len(used),
		"heroes", len(heroes), "diamonds", gold, "diamondsFrom", goldSrc, "minQuality", secretTaskStartsColorNames[secretTaskStartsMinColor])
	if !goldOK {
		slog.Info(name + ": no diamond balance in the list reply or init, so a start could not be checked; starting nothing")
		if !dryRun {
			return nil
		}
	}
	var errs []error
	attempts := 0
	for _, t := range slices.Clone(tasks) {
		// Re-check against the latest list: OnStartClick reads the task afresh (:427-428).
		if i := slices.IndexFunc(tasks, func(x secretTaskStartsTask) bool { return x.uuid == t.uuid }); i < 0 || tasks[i].completion != 0 {
			continue
		}
		cfg, known := secretTaskStartsCfgs[t.cfgID]
		color := secretTaskStartsColorNames[cfg.color]
		if color == "" {
			color = strconv.FormatInt(cfg.color, 10)
		}
		note := func(msg string, args ...any) {
			slog.Info(name+": task "+msg, append([]any{"uuid", t.uuid, "cfgId", t.cfgID, "quality", color}, args...)...)
		}
		switch {
		case !known:
			note("skipped: cfgId is not in lw_dispatch_tasks")
			continue
		case cfg.color < secretTaskStartsMinColor:
			note("skipped: below the minimum quality", "min", secretTaskStartsColorNames[secretTaskStartsMinColor])
			continue
		case attempts >= secretTaskStartsMaxPerRun:
			note("skipped: per-run start cap reached", "cap", secretTaskStartsMaxPerRun)
			continue
		case running >= maxSlots:
			note("skipped: no free dispatch slot", "running", running, "maxSlots", maxSlots)
			continue
		}
		pick, march, why := secretTaskStartsDecide(t, heroes, used, home)
		if why != "" {
			note("skipped: " + why)
			continue
		}
		if dryRun {
			note("WOULD start", "heroList", pick, "marchDuration", march)
			attempts++
			running++
			for _, u := range pick {
				used[u] = true
			}
			continue
		}
		note("starting", "heroList", pick, "marchDuration", march, "diamondsBefore", gold)
		attempts++
		params := sfs.NewSFSObject()
		params.PutLong("uuid", t.uuid)
		params.PutValue("heroList", claimLongArray(pick))
		params.PutLong("marchDuration", march)
		label := fmt.Sprintf("secret task start %d", t.uuid)
		msg, err := session.SendAndWait(conn, label, secretTaskStartsCmd, params)
		if err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				return errors.Join(errs...)
			}
		}
		after, afterOK := int64(0), false
		if msg != nil {
			after, afterOK = claimInt(msg.Params, "gold")
		}
		relist, lerr := session.SendAndWait(conn, "secret task list", secretTasksListCmd, sfs.NewSFSObject())
		if lerr == nil {
			if g, ok := claimInt(relist.Params, "gold"); ok && (!afterOK || g < after) {
				after, afterOK = g, true
			}
		}
		if afterOK && after < gold {
			slog.Error(name+": DIAMOND BALANCE FELL after hero.dispatch.start; stopping", "uuid", t.uuid, "before", gold, "after", after)
			return errors.Join(append(errs, fmt.Errorf("%s: task %d: %w (%d -> %d)", name, t.uuid, errSecretTaskStartsDiamonds, gold, after))...)
		}
		if lerr != nil || !afterOK {
			slog.Warn(name+": could not re-read the diamond balance after a start; starting nothing more this run", "uuid", t.uuid, "listError", lerr)
			return errors.Join(append(errs, lerr)...)
		}
		gold = after
		tasks = secretTaskStartsTasks(relist.Params)
		running, used = secretTaskStartsBusy(tasks)
		if err == nil {
			started := slices.ContainsFunc(tasks, func(x secretTaskStartsTask) bool { return x.uuid == t.uuid && x.completion > 0 })
			slog.Info(label+" sent", "listedAsStarted", started, "running", running, "diamondsAfter", after)
		}
	}
	return errors.Join(errs...)
}

// secretTaskStartsTaskRows is lw_dispatch_tasks (328 rows, tables 1.0.364, byte-identical in live
// 39516) as "color/conditions" -> ids. Every color is kept so lowering secretTaskStartsMinColor
// needs no new data.
var secretTaskStartsTaskRows = map[string][]int32{
	"3/4;100;2": {
		300701, 300702, 300703, 300704, 301701, 301702, 301703, 301704, 302701, 302702, 302703, 302704,
	},
	"3/4;15;1": {
		300101, 300102, 300103, 300104,
	},
	"3/4;25;1": {
		301101, 301102, 301103, 301104, 302101, 302102, 302103, 302104,
	},
	"3/4;50;1": {
		300201, 300202, 300203, 300204, 301201, 301202, 301203, 301204, 302201, 302202, 302203, 302204,
	},
	"3/4;60;2": {
		300301, 300302, 300303, 300304, 301301, 301302, 301303, 301304, 302301, 302302, 302303, 302304,
	},
	"3/4;70;2": {
		300401, 300402, 300403, 300404, 301401, 301402, 301403, 301404, 302401, 302402, 302403, 302404,
	},
	"3/4;80;2": {
		300501, 300502, 300503, 300504, 301501, 301502, 301503, 301504, 302501, 302502, 302503, 302504,
	},
	"3/4;90;2": {
		300601, 300602, 300603, 300604, 301601, 301602, 301603, 301604, 302601, 302602, 302603, 302604,
	},
	"4/2;4;1|4;18;1": {
		400101, 400102, 400103, 400104,
	},
	"4/2;4;1|4;28;1": {
		401101, 401102, 401103, 401104, 402101, 402102, 402103, 402104,
	},
	"4/2;4;2|4;100;2": {
		400601, 400602, 400603, 400604, 401601, 401602, 401603, 401604, 402601, 402602, 402603, 402604,
	},
	"4/2;4;2|4;110;2": {
		400701, 400702, 400703, 400704, 401701, 401702, 401703, 401704, 402701, 402702, 402703, 402704,
	},
	"4/2;4;2|4;55;1": {
		400201, 400202, 400203, 400204, 401201, 401202, 401203, 401204, 402201, 402202, 402203, 402204,
	},
	"4/2;4;2|4;70;2": {
		400301, 400302, 400303, 400304, 401301, 401302, 401303, 401304, 402301, 402302, 402303, 402304,
	},
	"4/2;4;2|4;80;2": {
		400401, 400402, 400403, 400404, 401401, 401402, 401403, 401404, 402401, 402402, 402403, 402404,
	},
	"4/2;4;2|4;90;2": {
		400501, 400502, 400503, 400504, 401501, 401502, 401503, 401504, 402501, 402502, 402503, 402504,
	},
	"4/4;5;1": {
		100101, 100102, 70000403, 70000413,
	},
	"5/2;4;1|3;6;1|4;32;1": {
		500101, 500102, 500103, 500104, 5001101, 5001102, 5001103, 5001104, 5002101, 5002102, 5002103,
		5002104,
	},
	"5/2;4;1|4;22;1": {
		5000101, 5000102, 5000103, 5000104,
	},
	"5/2;4;2|3;11;1|4;80;2": {
		5000301, 5000302, 5000303, 5000304,
	},
	"5/2;4;2|3;11;2|4;80;2": {
		500301, 500302, 500303, 500304, 5001301, 5001302, 5001303, 5001304, 5002301, 5002302, 5002303,
		5002304,
	},
	"5/2;4;2|3;11;2|4;90;2": {
		50000401, 50000402, 50000403, 50000404,
	},
	"5/2;4;2|3;11;3|4;90;2": {
		500401, 500402, 500403, 500404, 5000401, 5000402, 5000403, 5000404, 50001401, 50001402, 50001403,
		50001404, 50002401, 50002402, 50002403, 50002404,
	},
	"5/2;4;2|3;6;1|4;60;1": {
		5000201, 5000202, 5000203, 5000204,
	},
	"5/2;4;2|3;6;2|4;60;1": {
		500201, 500202, 500203, 500204, 5001201, 5001202, 5001203, 5001204, 5002201, 5002202, 5002203,
		5002204,
	},
	"5/2;5;1|3;11;2|4;65;2": {
		600201, 6000201, 60001201, 60002201,
	},
	"5/2;5;1|3;16;2|4;100;3": {
		500501, 500502, 500503, 500504, 5000501, 5000502, 5000503, 5000504, 50000501, 50000502, 50000503,
		50000504, 50001501, 50001502, 50001503, 50001504, 50002501, 50002502, 50002503, 50002504,
		60001901, 60002901, 60009901,
	},
	"5/2;5;1|3;16;2|4;110;3": {
		50000601, 50000602, 50000603, 50000604, 50001601, 50001602, 50001603, 50001604, 50002601,
		50002602, 50002603, 50002604, 60001902, 60002902, 60009902,
	},
	"5/2;5;1|3;16;2|4;120;3": {
		50000701, 50000702, 50000703, 50000704, 50001701, 50001702, 50001703, 50001704, 50002701,
		50002702, 50002703, 50002704, 60001903, 60002903, 60009903,
	},
	"5/2;5;1|3;6;1|4;35;1": {
		600101, 6000101, 60001101, 60002101,
	},
	"5/2;5;1|3;6;1|4;65;2": {
		60000201,
	},
	"5/2;5;1|4;25;1": {
		60000101,
	},
	"5/2;5;2|3;11;1|4;90;2": {
		60000301,
	},
	"5/2;5;2|3;11;2|4;90;2": {
		600301, 6000301, 60001301, 60002301,
	},
	"5/2;5;2|3;16;2|4;100;3": {
		600401, 6000401, 60000401, 60001401, 60002401,
	},
	"5/2;5;2|3;21;1|4;110;3": {
		600501, 6000501, 60000501, 60001501, 60002501,
	},
	"5/2;5;2|3;21;1|4;120;3": {
		60000601, 60001601, 60002601,
	},
	"5/2;5;2|3;21;1|4;125;3": {
		60000701, 60001701, 60002701,
	},
	"5/4;5;1": {
		70000401, 70000402, 70000411, 70000412,
	},
}

// secretTaskStartsHeroQuality is lw_hero.quality (393 rows, tables 1.0.364, byte-identical in live
// 39516) as quality -> hero ids; HeroInfo takes quality from this row (HeroInfo.lua:124-128,
// HeroTemplate.lua:96).
var secretTaskStartsHeroQuality = map[int8][]int32{
	1: {
		10001, 10006, 10007, 10008, 10010, 10011, 10012, 10013, 10014, 10015, 10016, 10017, 10020, 10028,
		10029, 10030, 10031, 10032, 10033, 10052, 10053, 10101, 10102, 10103, 10104, 100101, 100102,
		100103, 100104, 100113, 100121, 100123, 100133, 100143, 100153, 100163, 100173, 100203, 110001,
		310001, 400090, 500061, 2100001, 2100002, 2100003, 3100001,
	},
	2: {
		10002, 20001, 20002, 30006, 30007, 30008, 30009, 30010, 100200, 120001, 320001,
	},
	3: {
		10003, 30002, 30003, 30004, 30005, 130002, 130003, 130004, 280006, 330002, 330003, 330004,
		2100004, 2100005, 2100006, 2100007, 2100008, 2100009, 2100010, 2100011, 2100012,
	},
	4: {
		10004, 10005, 10009, 40006, 40007, 40008, 40009, 40010, 40012, 40013, 40015, 40016, 40017, 40018,
		40019, 40020, 100050, 140006, 140007, 140008, 140009, 140010, 140012, 140013, 140015, 140016,
		140017, 140018, 140019, 140020, 340006, 340007, 340008, 340009, 340010, 340012, 340013, 340015,
		340016, 340017, 340018, 340019, 340020, 400070, 1000501,
	},
	5: {
		10051, 10060, 10061, 10062, 10063, 10064, 10065, 10066, 10067, 10068, 10069, 10070, 10071, 10072,
		10073, 10074, 10075, 10076, 10077, 40011, 40014, 50006, 50007, 50008, 50009, 50010, 50011, 50012,
		50013, 50014, 50015, 50016, 50017, 50018, 50019, 50020, 50021, 50022, 50023, 50024, 50025, 50026,
		50027, 50028, 50029, 90000, 90001, 90002, 90003, 90004, 90005, 140011, 140014, 150006, 150007,
		150008, 150009, 150010, 150011, 150012, 150013, 150014, 150015, 150016, 150017, 150018, 150019,
		150020, 150021, 150022, 150024, 150050, 150051, 150052, 150053, 150054, 150055, 150056, 150057,
		150058, 150059, 150060, 150061, 150062, 150063, 150064, 150065, 150066, 150067, 150068, 150069,
		200000, 200001, 200002, 200003, 200004, 200005, 200006, 200007, 200008, 250019, 250020, 250021,
		250022, 250023, 250024, 250025, 250026, 250027, 250028, 250029, 260019, 260020, 260021, 260022,
		260023, 260024, 260025, 260026, 260027, 260028, 260029, 270019, 270020, 270021, 270022, 270023,
		270024, 270025, 270026, 270027, 270028, 270029, 280001, 280002, 280003, 280004, 280005, 280019,
		340011, 340014, 350006, 350007, 350008, 350009, 350010, 350011, 350012, 350013, 350014, 350015,
		350016, 350017, 350018, 350019, 350020, 350021, 350022, 350024, 450019, 500160, 500170, 1000000,
		1000001, 1000002, 1000003, 1000004, 1000005, 1000006, 1000007, 1100000, 1100001, 1100002,
		1100003, 1100004, 1100005, 1100006, 1100007, 1500061, 1500071, 1500081, 1500091, 1500101,
		1500111, 1500121, 1500131, 1500141, 1500151, 1500161, 1500171, 1500181, 1500191, 1500201,
		1500211, 1500221, 3500061, 3500071, 3500081, 3500091, 3500101, 3500111, 3500121, 3500131,
		3500141, 3500151, 3500161, 3500171, 3500181, 3500191, 3500201, 3500211, 3500221, 3500241,
		3510061, 3510071, 3510081, 3510091, 3510101, 3510111, 3510121, 3510131, 3510141, 3510151,
		3510161, 3510171, 3510181, 3510191, 3510201, 3510211, 3510221, 3510241, 3520061, 3520071,
		3520081, 3520091, 3520101, 3520111, 3520121, 3520131, 3520141, 3520151, 3520161, 3520171,
		3520181, 3520191, 3520201, 3520211, 3520221, 3520241, 3530061, 3530071, 3530081, 3530091,
		3530101, 3530111, 3530121, 3530131, 3530141, 3530151, 3530161, 3530171, 3530181, 3530191,
		3530201, 3530211, 3530221, 3530241, 4500091, 4500161, 4500171,
	},
}
