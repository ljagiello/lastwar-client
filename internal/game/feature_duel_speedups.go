package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel speed-ups (DUEL.md §4.3, §7.2-7.3): apply OWNED speed-up items to RUNNING jobs on
// the duel days that score them, 50 points per item minute (score type 51; the queue sped up is
// the score value: 7 build, 6 research, 4 training, 9 heal, EnumType.lua:15739-15744, 15811-15816).
// Which items go where, by today's theme (hero.event.info.get eventId) AND today's score list:
//
//   - construction items 200210-200217: Base Expansion (110001) scoring 51/7;
//   - research items 200220-200227: Age of Science (110002) scoring 51/6;
//   - training items 200230-200235: Total Mobilization (110004) or Enemy Buster (110005) scoring 51/4;
//   - healing items 200240-200245: Enemy Buster (110005) scoring 51/9;
//   - universal items 200200-200207: Total Mobilization only, each time onto the running job that
//     scores today and has the longest remaining time.
//
// There is no stop at the last chest: points beyond it still count for the alliance total.
//
// Running jobs, found the way the client does:
//   - construction: init building_new with uT > now (BuildingData:IsUpgrading, BuildingDate.lua:
//     454-457), skipping state FoldUp/Paused/Crossing (EnumType.lua:3299-3305), and HQ 7 while the
//     newbie shield is up, since finishing HQ 8 drops it (see finished-timers);
//   - research and healing: init queue_new type 6 / 3 in the Work state (QueueInfo.lua:34-124;
//     LWUIHospitalView.lua:619, 659-662);
//   - training: the Military Camp (10103000) with prodST > 0 and now < prodET (BuildingUtils.lua:
//     1813-1818, 1836-1853; UILWMilitaryCampPanelView.lua:873-876).
//
// Commands, in the items-only form the speed panel sends on close (UISpeedView.lua:1540-1586):
// `build.ccd.m.new {bUUID: Long, isFixRuins: false, itemIDs: "tmpl;n|tmpl;n"}`, `queue.ccd.m.new
// {qUUID: Long, itemIDs}` (research and the hospital queue), `building.camp.accel {uuid: Long,
// itemId}` (the camp's uuid; the same item string). useGold and golloesSpeedTime are the diamond
// and free-time forms: never sent, and duelSpeedupsCheckParams refuses any key outside these.
//
// Items come from init items (InitMessage.lua:70 -> ItemData:ParseItemData; per stack uuid, itemId
// or goodsId, count/num/itemLeftCnt, ItemInfo.lua:35-64), summed per template and decremented
// locally after every call, so no request ever asks for more than is owned. The picker is the
// client's (LWResourceLackUtil.lua:897-990): queue items before universal ones, largest first, as
// many as fit in remaining + 59 s; then, if time is left, one of the smallest owned item when that
// overshoots by less than speedup_config k1 = 60 s. The client's wider gap-filler (60-600 s
// overshoot, then giving items back, :955-1012) is left out, so a call never wastes a minute.
//
// Diamond tripwire: the balance starts at init user.gold (PlayerInfo.lua:612-613) and every reply
// is checked before anything else happens: remainGold / gold, top level or under queue
// (BuildManager.lua:403-413; QueueDataManager.lua:483-492; BuildingCampAccelMessage.lua:15-22),
// must not drop; build and queue replies must carry an itemCostArr listing every item sent (proof
// the items paid); a camp reply, which has no itemCostArr, must carry gold. Otherwise the feature
// stops for the rest of the process with an ERROR naming the call. E100173 (already completed,
// QueueDataManager.lua:507-508) and 120021 ("Not enough items.") stop just that job. Remaining
// time is re-read from every reply (buildInfo.uT / finished, queue uT, buildInfo.prodET); a
// reply without it ends that job. Remaining time is end minus the host clock (evNow, as TodayDuel
// uses); the client uses server time, so a host clock skewed by more than the 59 s tolerance would
// skew the pick. At most duelSpeedupsMaxCalls spend calls per run.
//
// duel-speedups-plan runs the same selection with every spend replaced by a log line, the read-only
// pre-check before a live run. Static-only: no speed-up has been sent live yet.
const (
	duelSpeedupsBuildCmd = "build.ccd.m.new"
	duelSpeedupsQueueCmd = "queue.ccd.m.new"
	duelSpeedupsCampCmd  = "building.camp.accel"

	duelSpeedupsDoneCode     = "E100173" // queue already completed (tip 170008 "Completed")
	duelSpeedupsNoItemsCode  = "120021"  // "Not enough items."
	duelSpeedupsMaxCalls     = 200
	duelSpeedupsToleranceMs  = 59_000 // LWResourceLackUtil.lua:922: remaining + 59 s
	duelSpeedupsGapLimitMs   = 60_000 // DataConfig speedup_config k1
	duelSpeedupsPointsPerMin = 50     // score 90201/90301/90501/90601, before tech bonuses

	duelBuildingFoldUp   = 2 // BuildingStateType (EnumType.lua:3299-3305)
	duelBuildingPaused   = 3
	duelBuildingCrossing = 5
)

// duelSpeedupKind is the job a speed-up applies to.
type duelSpeedupKind int

const (
	duelSpeedupBuild duelSpeedupKind = iota
	duelSpeedupResearch
	duelSpeedupTraining
	duelSpeedupHeal
)

var duelSpeedupKinds = []duelSpeedupKind{duelSpeedupBuild, duelSpeedupResearch, duelSpeedupTraining, duelSpeedupHeal}

func (k duelSpeedupKind) String() string {
	return [...]string{"construction", "research", "training", "healing"}[k]
}

// scoreValue is the type-51 score value of the queue.
func (k duelSpeedupKind) scoreValue() string {
	return [...]string{DuelQueueBuild, DuelQueueResearch, DuelQueueTraining, DuelQueueHeal}[k]
}

func (k duelSpeedupKind) cmd() string {
	switch k {
	case duelSpeedupBuild:
		return duelSpeedupsBuildCmd
	case duelSpeedupTraining:
		return duelSpeedupsCampCmd
	}
	return duelSpeedupsQueueCmd
}

// duelSpeedupItemSecs is goods type 2 rows 200200-200245: template id -> seconds (para3; none has
// overdue, so they never expire). Universal 2002000x, then 7 build, 6 research, 3 training and 4
// heal menus (type2).
var (
	duelSpeedupItemSecs  = map[int64]int64{}
	duelSpeedupUniversal []int64
	duelSpeedupSpecific  = map[duelSpeedupKind][]int64{}
)

func init() {
	secs := []int64{60, 300, 900, 3600, 10800, 28800, 86400, 432000} // 1m 5m 15m 1h 3h 8h 1d 5d
	add := func(base int64, n int) []int64 {
		var ids []int64
		for i := n - 1; i >= 0; i-- { // largest first
			duelSpeedupItemSecs[base+int64(i)] = secs[i]
			ids = append(ids, base+int64(i))
		}
		return ids
	}
	duelSpeedupUniversal = add(200200, 8)
	duelSpeedupSpecific[duelSpeedupBuild] = add(200210, 8)
	duelSpeedupSpecific[duelSpeedupResearch] = add(200220, 8)
	duelSpeedupSpecific[duelSpeedupTraining] = add(200230, 6)
	duelSpeedupSpecific[duelSpeedupHeal] = add(200240, 6)

	registerFeature(Feature{
		Name: "duel-speedups",
		Summary: "OPT-IN, SPENDS ITEMS: on Alliance Duel speed-up days use owned speed-up items (never diamonds) " +
			"on running construction/research/training/healing jobs; diamond tripwire stops it",
		Duel: []DuelScore{
			{Type: DuelScoreSpeedUp, Value: DuelQueueBuild},
			{Type: DuelScoreSpeedUp, Value: DuelQueueResearch},
			{Type: DuelScoreSpeedUp, Value: DuelQueueTraining},
			{Type: DuelScoreSpeedUp, Value: DuelQueueHeal},
		},
		Hold: true,
		Run:  runDuelSpeedups,
	})
	registerFeature(Feature{
		Name:    "duel-speedups-plan",
		Summary: "read-only: log the speed-up items duel-speedups would use today, per running job; spends nothing",
		Run:     runDuelSpeedupsPlan,
	})
	for _, code := range []string{duelSpeedupsDoneCode, duelSpeedupsNoItemsCode} {
		session.RegisterBenignErrorCode(code, duelSpeedupsBuildCmd, duelSpeedupsQueueCmd, duelSpeedupsCampCmd)
	}
}

// duelSpeedupsTrip disables duel-speedups for the rest of the process once the tripwire fires.
var duelSpeedupsTrip struct {
	sync.Mutex
	reason string
}

func duelSpeedupsTripped() string {
	duelSpeedupsTrip.Lock()
	defer duelSpeedupsTrip.Unlock()
	return duelSpeedupsTrip.reason
}

func duelSpeedupsSetTrip(reason string) {
	duelSpeedupsTrip.Lock()
	defer duelSpeedupsTrip.Unlock()
	if duelSpeedupsTrip.reason == "" {
		duelSpeedupsTrip.reason = reason
	}
}

// duelSpeedupsAllowed returns, per job kind, whether today allows its queue items and whether it
// takes universal items.
func duelSpeedupsAllowed(d *DuelDay) (specific, universal map[duelSpeedupKind]bool) {
	specific, universal = map[duelSpeedupKind]bool{}, map[duelSpeedupKind]bool{}
	if d == nil {
		return specific, universal
	}
	scores := func(k duelSpeedupKind) bool { return d.Scores(DuelScoreSpeedUp, k.scoreValue()) }
	switch d.Theme {
	case DuelThemeBase:
		specific[duelSpeedupBuild] = scores(duelSpeedupBuild)
	case DuelThemeScience:
		specific[duelSpeedupResearch] = scores(duelSpeedupResearch)
	case DuelThemeMobilization:
		specific[duelSpeedupTraining] = scores(duelSpeedupTraining)
		for _, k := range duelSpeedupKinds {
			universal[k] = scores(k)
		}
	case DuelThemeEnemyBuster:
		specific[duelSpeedupTraining] = scores(duelSpeedupTraining)
		specific[duelSpeedupHeal] = scores(duelSpeedupHeal)
	}
	return specific, universal
}

// duelSpeedupJob is one running job; endMs is when it finishes.
type duelSpeedupJob struct {
	kind  duelSpeedupKind
	uuid  int64
	name  string
	endMs int64
	done  bool // no more calls: finished, stopped by the server, or its reply was unusable
}

func (j *duelSpeedupJob) remainingMs(nowMs int64) int64 { return max(0, j.endMs-nowMs) }

func (j *duelSpeedupJob) label() string {
	return fmt.Sprintf("duel speed-up %s %s", j.kind, j.name)
}

// duelSpeedupsJobs lists the running jobs a speed-up applies to.
func duelSpeedupsJobs(in *Init, nowMs int64) []*duelSpeedupJob {
	var jobs []*duelSpeedupJob
	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		lv, _ := claimInt(b, "lv")
		name := fmt.Sprintf("%s %d", BuildingNameOf(int32(bID)), uuid)
		state, _ := claimInt(b, "state")
		if uT, _ := claimInt(b, "uT"); uT > nowMs {
			switch {
			case state == duelBuildingFoldUp || state == duelBuildingPaused || state == duelBuildingCrossing:
			case bID == claimHQBuildingID && lv == finishedTimersShieldHQLevel && finishedTimersShieldUp(in, nowMs):
				slog.Info("duel-speedups: leaving the HQ 8 upgrade alone while the newbie shield is up", "uuid", uuid)
			default:
				jobs = append(jobs, &duelSpeedupJob{kind: duelSpeedupBuild, uuid: uuid, name: name, endMs: uT})
			}
		}
		if bID == finishedTimersMilitaryCamp && lv > 0 {
			st, _ := claimInt(b, "prodST")
			et, ok := claimInt(b, "prodET")
			if st > 0 && ok && et > nowMs {
				jobs = append(jobs, &duelSpeedupJob{kind: duelSpeedupTraining, uuid: uuid, name: name, endMs: et})
			}
		}
	}
	for _, q := range finishedTimersQueues(in) {
		if !q.Working(nowMs) {
			continue
		}
		name := fmt.Sprintf("queue %d", q.UUID)
		switch q.Type {
		case finishedTimersQueueResearch:
			jobs = append(jobs, &duelSpeedupJob{kind: duelSpeedupResearch, uuid: q.UUID, name: name, endMs: q.End})
		case finishedTimersQueueHospital:
			jobs = append(jobs, &duelSpeedupJob{kind: duelSpeedupHeal, uuid: q.UUID, name: name, endMs: q.End})
		}
	}
	return jobs
}

// duelSpeedupsInventory sums the owned speed-up items from init items per template id.
func duelSpeedupsInventory(in *Init) map[int64]int64 {
	owned := map[int64]int64{}
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		id, ok := claimFirstInt(it, "goodsId", "itemId")
		if _, speedup := duelSpeedupItemSecs[id]; !ok || !speedup {
			continue
		}
		if n, ok := claimFirstInt(it, "itemLeftCnt", "num", "count"); ok && n > 0 {
			owned[id] += n
		}
	}
	return owned
}

// duelSpeedupUse is one entry of an itemIDs string.
type duelSpeedupUse struct{ id, count int64 }

// duelSpeedupsPick is LWResourceLackUtil:GetSpeedUpGoods (LWResourceLackUtil.lua:897-990) over the
// owned items in ids, which are already in the client's order (queue items, then universal, each
// largest first). It never takes more than owned and never overshoots remainingMs by 60 s or more.
func duelSpeedupsPick(remainingMs int64, owned map[int64]int64, ids []int64) []duelSpeedupUse {
	if remainingMs <= 0 {
		return nil
	}
	left := remainingMs
	surplus := remainingMs + duelSpeedupsToleranceMs
	avail := map[int64]int64{}
	var plan []duelSpeedupUse
	for _, id := range ids {
		avail[id] = owned[id]
		ms := duelSpeedupItemSecs[id] * 1000
		if left <= 0 || avail[id] <= 0 || ms <= 0 || surplus <= ms {
			continue
		}
		n := min(surplus/ms, avail[id])
		left -= n * ms
		surplus -= n * ms
		avail[id] -= n
		plan = append(plan, duelSpeedupUse{id, n})
	}
	if left > 0 {
		// The smallest item still owned (ties: queue items first), as the client re-sorts.
		var gap int64
		for _, id := range ids {
			if avail[id] > 0 && (gap == 0 || duelSpeedupItemSecs[id] < duelSpeedupItemSecs[gap]) {
				gap = id
			}
		}
		if gap != 0 {
			ms := duelSpeedupItemSecs[gap] * 1000
			if dev := ms - left; dev > -duelSpeedupsGapLimitMs && dev < duelSpeedupsGapLimitMs {
				i := slices.IndexFunc(plan, func(u duelSpeedupUse) bool { return u.id == gap })
				if i < 0 {
					plan = append(plan, duelSpeedupUse{gap, 1})
				} else {
					plan[i].count++
				}
			}
		}
	}
	return plan
}

// duelSpeedupsItemString is the itemIDs value: "tmpl;n|tmpl;n" (UISpeedView.lua:1544-1557).
func duelSpeedupsItemString(plan []duelSpeedupUse) string {
	parts := make([]string, 0, len(plan))
	for _, u := range plan {
		parts = append(parts, strconv.FormatInt(u.id, 10)+";"+strconv.FormatInt(u.count, 10))
	}
	return strings.Join(parts, "|")
}

// duelSpeedupsPlanMs is the item time a plan applies.
func duelSpeedupsPlanMs(plan []duelSpeedupUse) int64 {
	var ms int64
	for _, u := range plan {
		ms += u.count * duelSpeedupItemSecs[u.id] * 1000
	}
	return ms
}

// duelSpeedupsParams builds the request for j and plan.
func duelSpeedupsParams(j *duelSpeedupJob, plan []duelSpeedupUse) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	items := duelSpeedupsItemString(plan)
	switch j.kind {
	case duelSpeedupBuild:
		p.PutLong("bUUID", j.uuid)
		p.PutBool("isFixRuins", false)
		p.PutUtfString("itemIDs", items)
	case duelSpeedupTraining:
		p.PutLong("uuid", j.uuid)
		p.PutUtfString("itemId", items)
	default:
		p.PutLong("qUUID", j.uuid)
		p.PutUtfString("itemIDs", items)
	}
	return p
}

// duelSpeedupsAllowedKeys is the whole key set each speed-up request may carry. useGold and
// golloesSpeedTime are not in it.
var duelSpeedupsAllowedKeys = map[string][]string{
	duelSpeedupsBuildCmd: {"bUUID", "isFixRuins", "itemIDs"},
	duelSpeedupsQueueCmd: {"qUUID", "itemIDs"},
	duelSpeedupsCampCmd:  {"uuid", "itemId"},
}

// duelSpeedupsCheckParams is the last check before a speed-up goes on the wire: only the
// items-only keys, isFixRuins false, and a non-empty item string of known speed-up items whose
// counts are positive and within owned.
func duelSpeedupsCheckParams(cmd string, p *sfs.SFSObject, owned map[int64]int64) error {
	keys, ok := duelSpeedupsAllowedKeys[cmd]
	if !ok {
		return fmt.Errorf("%s is not a speed-up command", cmd)
	}
	for _, k := range p.Keys() {
		if !slices.Contains(keys, k) {
			return fmt.Errorf("%s: refusing to send key %q", cmd, k)
		}
	}
	for _, k := range keys {
		if !p.Has(k) {
			return fmt.Errorf("%s: missing %q", cmd, k)
		}
	}
	if v, _ := p.Get("isFixRuins"); cmd == duelSpeedupsBuildCmd && v.Val != false {
		return fmt.Errorf("%s: isFixRuins must be false", cmd)
	}
	itemKey := keys[len(keys)-1]
	items := p.GetString(itemKey)
	if items == "" {
		return fmt.Errorf("%s: refusing an empty %s", cmd, itemKey)
	}
	seen := map[int64]bool{}
	for _, part := range strings.Split(items, "|") {
		idStr, nStr, ok := strings.Cut(part, ";")
		id, err1 := strconv.ParseInt(idStr, 10, 64)
		n, err2 := strconv.ParseInt(nStr, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			return fmt.Errorf("%s: malformed item entry %q", cmd, part)
		}
		if _, speedup := duelSpeedupItemSecs[id]; !speedup || seen[id] {
			return fmt.Errorf("%s: item %d is not a speed-up item or repeats", cmd, id)
		}
		if n <= 0 || n > owned[id] {
			return fmt.Errorf("%s: item %d count %d is outside 1..%d owned", cmd, id, n, owned[id])
		}
		seen[id] = true
	}
	return nil
}

// duelSpeedupsCheckReply is the diamond tripwire for one successful reply: every gold balance it
// carries must be at least goldBefore, a build or queue reply must list each item sent in
// itemCostArr, and a camp reply must carry gold. It returns the balance to check the next reply
// against.
func duelSpeedupsCheckReply(cmd string, reply *sfs.SFSObject, goldBefore int64, plan []duelSpeedupUse) (int64, error) {
	holders := []*sfs.SFSObject{reply}
	costs := reply
	if cmd == duelSpeedupsQueueCmd {
		costs = evObject(reply, "queue")
		if costs == nil {
			return goldBefore, errors.New("reply has no queue object")
		}
		holders = append(holders, costs)
	}
	goldAfter, sawGold := goldBefore, false
	for _, h := range holders {
		for _, key := range []string{"remainGold", "gold"} {
			v, ok := h.Get(key)
			if !ok {
				continue
			}
			g, ok := claimIntValue(v.Val)
			if !ok {
				return goldBefore, fmt.Errorf("reply %s is not a number (%v)", key, v.Val)
			}
			if g < goldBefore {
				return goldBefore, fmt.Errorf("diamonds dropped from %d to %d (reply %s)", goldBefore, g, key)
			}
			if !sawGold || g < goldAfter {
				goldAfter = g
			}
			sawGold = true
		}
	}
	if cmd == duelSpeedupsCampCmd {
		if !sawGold {
			return goldBefore, errors.New("reply has no gold balance to check")
		}
		return goldAfter, nil
	}
	if !costs.Has("itemCostArr") {
		return goldBefore, errors.New("reply has no itemCostArr: nothing shows the items paid")
	}
	paid := map[int64]bool{}
	for _, e := range claimObjectsOrMap(costs, "itemCostArr") {
		if id, ok := claimFirstInt(e, "goodsId", "itemId"); ok {
			paid[id] = true
		}
	}
	for _, u := range plan {
		if !paid[u.id] {
			return goldBefore, fmt.Errorf("itemCostArr does not list item %d", u.id)
		}
	}
	return goldAfter, nil
}

// duelSpeedupsEnd reads the job's new end time from a reply; ok is false when the reply has none.
func duelSpeedupsEnd(j *duelSpeedupJob, reply *sfs.SFSObject) (endMs int64, finished, ok bool) {
	switch j.kind {
	case duelSpeedupBuild:
		if f, has := claimBool(reply, "finished"); has && f {
			return 0, true, true
		}
		endMs, ok = claimInt(evObject(reply, "buildInfo"), "uT")
	case duelSpeedupTraining:
		endMs, ok = claimInt(evObject(reply, "buildInfo"), "prodET")
	default:
		endMs, ok = claimFirstInt(evObject(reply, "queue"), "updateTime", "uT")
	}
	return endMs, false, ok
}

// duelSpeedupsRunner is one run's state.
type duelSpeedupsRunner struct {
	conn  *session.GameConn
	dry   bool
	owned map[int64]int64
	gold  int64
	calls int
	used  map[duelSpeedupKind]int64 // item ms applied per job kind
	errs  []error
	halt  bool // stop the run: tripwire, dead connection, no reply, call cap
}

func runDuelSpeedups(conn *session.GameConn, in *Init) error { return duelSpeedupsRun(conn, in, false) }

func runDuelSpeedupsPlan(conn *session.GameConn, in *Init) error {
	return duelSpeedupsRun(conn, in, true)
}

func duelSpeedupsRun(conn *session.GameConn, in *Init, dry bool) error {
	if reason := duelSpeedupsTripped(); reason != "" && !dry {
		return fmt.Errorf("duel-speedups is disabled for this session by the diamond tripwire: %s", reason)
	}
	if in == nil || in.Raw == nil {
		slog.Info("duel-speedups: no init push; skipping")
		return nil
	}
	d := TodayDuel(conn, in)
	specific, universal := duelSpeedupsAllowed(d)
	if !slices.Contains(slices.Collect(maps.Values(specific)), true) && !slices.Contains(slices.Collect(maps.Values(universal)), true) {
		slog.Info("duel-speedups: today's duel takes no speed-up items", "theme", d.ThemeName())
		return nil
	}
	gold, ok := claimInt(in.Object("user"), "gold")
	if !ok {
		err := errors.New("duel-speedups: init has no user.gold, so the diamond tripwire cannot be armed; nothing sent")
		slog.Error(err.Error())
		return err
	}
	r := &duelSpeedupsRunner{conn: conn, dry: dry, owned: duelSpeedupsInventory(in), gold: gold, used: map[duelSpeedupKind]int64{}}
	jobs := duelSpeedupsJobs(in, evNow().UnixMilli())
	slog.Info("duel-speedups: start", "theme", d.ThemeName(), "dryRun", dry, "runningJobs", len(jobs),
		"ownedItemMinutes", duelSpeedupsOwnedMinutes(r.owned), "diamonds", gold)

	byRemaining := func() {
		now := evNow().UnixMilli()
		slices.SortStableFunc(jobs, func(a, b *duelSpeedupJob) int {
			return cmp.Compare(b.remainingMs(now), a.remainingMs(now))
		})
	}
	// Queue items, longest job first so the large items find a home.
	byRemaining()
	for _, j := range jobs {
		if !specific[j.kind] {
			continue
		}
		for !r.halt && !j.done {
			plan := duelSpeedupsPick(j.remainingMs(evNow().UnixMilli()), r.owned, duelSpeedupSpecific[j.kind])
			if len(plan) == 0 {
				break
			}
			r.apply(j, plan)
		}
	}
	// Universal items (Total Mobilization): always onto the scoring job with the most time left.
	for !r.halt {
		var target *duelSpeedupJob
		byRemaining()
		for _, j := range jobs {
			if universal[j.kind] && !j.done && j.remainingMs(evNow().UnixMilli()) > 0 {
				target = j
				break
			}
		}
		if target == nil {
			break
		}
		plan := duelSpeedupsPick(target.remainingMs(evNow().UnixMilli()), r.owned, duelSpeedupUniversal)
		if len(plan) == 0 {
			break
		}
		r.apply(target, plan)
	}
	for _, k := range duelSpeedupKinds {
		if ms := r.used[k]; ms > 0 {
			slog.Info("duel-speedups: item time used", "dryRun", dry, "queue", fmt.Sprint(k), "minutes", ms/60000,
				"expectedPoints", ms/60000*duelSpeedupsPointsPerMin)
		}
	}
	if r.calls == 0 {
		slog.Info("duel-speedups: nothing to speed up (no running job today's duel scores, or no matching items)",
			"theme", d.ThemeName())
	}
	return errors.Join(r.errs...)
}

func duelSpeedupsOwnedMinutes(owned map[int64]int64) map[string]int64 {
	out := map[string]int64{}
	for id, n := range owned {
		kind := "universal"
		for k, ids := range duelSpeedupSpecific {
			if slices.Contains(ids, id) {
				kind = k.String()
			}
		}
		out[kind] += n * duelSpeedupItemSecs[id] / 60
	}
	return out
}

// apply sends one speed-up (or, dry, logs it) and updates the job, the inventory and the gold
// snapshot from the reply.
func (r *duelSpeedupsRunner) apply(j *duelSpeedupJob, plan []duelSpeedupUse) {
	if r.calls >= duelSpeedupsMaxCalls {
		slog.Warn("duel-speedups: call cap reached; stopping this run", "cap", duelSpeedupsMaxCalls)
		r.halt = true
		return
	}
	cmd := j.kind.cmd()
	params := duelSpeedupsParams(j, plan)
	if err := duelSpeedupsCheckParams(cmd, params, r.owned); err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s: %w", j.label(), err))
		r.halt = true
		return
	}
	nowMs := evNow().UnixMilli()
	applyMs := duelSpeedupsPlanMs(plan)
	r.calls++
	if r.dry {
		slog.Info("duel-speedups plan: would send", "job", j.label(), "cmd", cmd,
			"remainingMin", j.remainingMs(nowMs)/60000, "items", duelSpeedupsItemString(plan), "itemMinutes", applyMs/60000)
		r.spend(j, plan, applyMs)
		j.endMs -= applyMs
		j.done = j.endMs <= nowMs
		return
	}
	label := j.label()
	msg, err := session.SendAndWait(r.conn, label, cmd, params)
	if msg == nil {
		// No reply: whether the items were spent is unknown, so nothing more is sent this run.
		r.errs = append(r.errs, err)
		r.halt = true
		return
	}
	if msg.Params.Has("errorCode") {
		j.done = true // benign (already finished / not enough items) or a real failure: next job
		if err != nil {
			r.errs = append(r.errs, err)
		}
		return
	}
	gold, err := duelSpeedupsCheckReply(cmd, msg.Params, r.gold, plan)
	if err != nil {
		reason := fmt.Sprintf("%s (%s %s): %v", label, cmd, duelSpeedupsItemString(plan), err)
		duelSpeedupsSetTrip(reason)
		slog.Error("duel-speedups DIAMOND TRIPWIRE: disabled for this session", "call", cmd, "job", label, "reason", err,
			"diamondsBefore", r.gold)
		r.errs = append(r.errs, errors.New("duel-speedups diamond tripwire: "+reason))
		r.halt = true
		return
	}
	r.gold = gold
	r.spend(j, plan, applyMs)
	end, finished, ok := duelSpeedupsEnd(j, msg.Params)
	switch {
	case finished:
		j.done = true
	case !ok:
		slog.Warn("duel-speedups: reply carries no end time; leaving this job", "job", label)
		j.done = true
	case end >= j.endMs:
		slog.Warn("duel-speedups: reply shows no time taken off; leaving this job", "job", label, "endMs", end)
		j.done = true
	default:
		j.endMs = end
		j.done = end <= evNow().UnixMilli()
	}
}

// spend takes plan out of the local inventory.
func (r *duelSpeedupsRunner) spend(j *duelSpeedupJob, plan []duelSpeedupUse, applyMs int64) {
	for _, u := range plan {
		r.owned[u.id] -= u.count
	}
	r.used[j.kind] += applyMs
}
