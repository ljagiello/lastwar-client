package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// City upgrades (OPT-IN, feature name kept as production-upgrades): raise the Headquarters to
// productionUpgradeHQCap and every other city building to the HQ level, one upgrade per free build
// queue at a time, paid with iron, food and gold coins only. Read from the live 831 Lua bundle and
// table archive 39746:
//
//   - Request: `free.building.up.new` as the upgrade panel sends it (UIBuildUpgradeView.lua:
//     1858-1875; FreeBuildingUpNewMessage.lua:7-43): uuid as a UtfString (tostring), clientParam
//     "", gold Int BuildUpgradeUseGoldType.No (0; Yes = 2 pays diamonds, Free = 3), upLevel Int the
//     next level, truckId 0, pathTime 0, and robotUuid Long, a free build queue
//     (BuildQueueManager:IsCanUpgrade, :524-539). No workerUuid or goldCost is sent. The server
//     ignores robotUuid and takes the first unlocked free queue, or answers E100009 "building queue
//     is full!" (live, 2026-10-21).
//   - Build queues are init queue_new entries of type LWBuildQueueType.NORMAL (0). One can take an
//     upgrade when unlock == 1, it hasn't expired (expireTime <= 0 or in the future), and nothing
//     occupies it (no itemObj) or its building's upgrade has ended (BuildQueueInfo.lua:51-236).
//   - Rules: row bId+lv of the building table (building_upgrade_rules_gen.go) gives the upgrade
//     from lv to lv+1: cost_consume, time and its prerequisites (column building, bId;level pairs,
//     met by the highest instance of that type). A row needing anything else is left to the player.
//     The cap is productionUpgradeHQCap for the HQ and the HQ level (at most that cap) for the rest.
//   - Order: the HQ first, then the buildings its next level needs, then the rest, shortest first.
//     While the HQ's next level is allowed but not affordable, other upgrades only start when they
//     leave enough for it, so the city saves up for the HQ instead of spending everything on side
//     buildings.
//   - Ended upgrades (uT > 0, now >= uT) are first collected with the free
//     free.building.upgrade.finish -- the in-game "gift" -- for every city building, the HQ
//     included: Kevin chose on 2026-10-21 to go past HQ 8, which drops the newbie shield.
//   - A building type the HQ level unlocks (unlock_num) but the city doesn't have is logged: placing
//     a new building (free.building.place.new) needs a free spot the client can't see, so the
//     player places it in game.
//   - Chain: when an upgrade started this run, or one already running on a build queue at login,
//     ends within productionUpgradeChain, the run waits (still reading pushes), opens it and plans
//     again from the new levels, so a full queue at login doesn't end the run.
//   - Left out: rows with a Gate (mono_condition included: the Secret Command Post answered
//     {errorCode=E000000, errorMsg=mono condition not met}) and productionUpgradePaid types.
//   - Alliance help: right after an upgrade starts, the run sends the upgrade's free help request
//     (al.call.help) when the player is in an alliance.
//   - Never diamonds: gold is always No (0), no goldCost, no speed-up. The reply's top-level gold
//     echoes that parameter, so the spend guard doesn't read it as the balance.
//   - Every send goes through the duel spend guard: the diamond tripwire and a gold check after each
//     upgrade. A refused upgrade (errorCode) skips that building for the run; a fallen balance stops
//     it.
//
// production-upgrades-plan logs the same choices and sends nothing.
// The chain budget: how long one run keeps starting upgrades as earlier ones end (inside the
// service's TimeoutStartSec, 2 h on this host), the length of one table second and the margin added
// to each upgrade's table time before opening it. Tests shrink them.
var (
	productionUpgradeChain  = 90 * time.Minute
	productionUpgradeSecond = time.Second
	productionUpgradeSlack  = 3 * time.Second
)

// productionUpgradePaid holds the types the server refuses for a reason the table doesn't carry:
// the 4th Squad answered {errorCode=120446, errorMsg=month card is unactive} on 2026-10-21.
var productionUpgradePaid = map[int64]string{10145000: "month card"}

const (
	productionUpgradeCmd      = "free.building.up.new"
	productionUpgradeQueueBld = 0  // LWBuildQueueType.NORMAL
	productionUpgradeGoldNo   = 0  // BuildUpgradeUseGoldType.No
	productionUpgradeHQCap    = 10 // the HQ level the city is raised to (Kevin, 2026-10-21)
)

// buildingUpgradeType is one building type's rows in buildingUpgradeRules.
type buildingUpgradeType struct {
	Name   string
	Max    int64
	Unlock []buildingUnlock
	Levels map[int64]buildingUpgradeLevel
}

// buildingUnlock: the Count-th instance of a type becomes available at Headquarters level HQ.
type buildingUnlock struct{ Count, HQ int64 }

// buildingUpgradeLevel is the upgrade from a level to the next.
type buildingUpgradeLevel struct {
	Metal, Money, Wood, Seconds int64
	Needs                       []buildingNeed
	Gate                        string
	NoQueue                     bool
}

// buildingNeed: a building of type BID at level Lv or above.
type buildingNeed struct{ BID, Lv int64 }

func init() {
	spendReplyEchoesGold[productionUpgradeCmd] = true
	registerFeature(Feature{
		Name:    "production-upgrades",
		Summary: "OPT-IN: raise the HQ to 10 and every city building to the HQ level, one per free build queue, iron/food/gold only (diamond tripwire)",
		Run:     func(conn *session.GameConn, in *Init) error { return runProductionUpgrades(conn, in, false) },
	})
	registerFeature(Feature{
		Name:    "production-upgrades-plan",
		Summary: "read-only: log the upgrades production-upgrades would start",
		Run:     func(conn *session.GameConn, in *Init) error { return runProductionUpgrades(conn, in, true) },
	})
}

type productionUpgradeTarget struct {
	UUID, BID, Lv int64
	Rule          buildingUpgradeLevel
}

// productionUpgradeBuilding is one city building as the run tracks it.
type productionUpgradeBuilding struct {
	UUID, BID, Lv int64
	Busy          bool // upgrading
}

// productionUpgradeState is what init says about the city buildings and the build queues.
type productionUpgradeState struct {
	HQ        int64
	Buildings []productionUpgradeBuilding
	Queues    []int64 // free build queue uuids
	// Upgrading holds the upgrades already running on a usable build queue, ending at their uT, so
	// the run can wait for one, open it and go on instead of stopping on a full queue.
	Upgrading []productionUpgradeRun
}

// productionUpgradeStateOf reads init. opened holds the buildings whose ended upgrade was collected
// this run: init still shows them at the old level with uT set, so they count one level up and idle.
// An ended upgrade not collected (the plan, or a failed finish) still counts as busy.
func productionUpgradeStateOf(in *Init, nowMs int64, opened map[int64]bool) productionUpgradeState {
	var s productionUpgradeState
	ended := map[string]bool{} // building uuid -> its upgrade timer has ended
	running := map[string]productionUpgradeBuilding{}
	runningUT := map[string]int64{}
	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		lv, _ := claimInt(b, "lv")
		uT, _ := claimInt(b, "uT")
		if uT > 0 && nowMs >= uT {
			ended[strconv.FormatInt(uuid, 10)] = true
		}
		if opened[uuid] {
			lv, uT = lv+1, 0
		}
		if bID == claimHQBuildingID && lv > s.HQ {
			s.HQ = lv
		}
		if _, ok := buildingUpgradeRules[int32(bID)]; ok {
			b := productionUpgradeBuilding{UUID: uuid, BID: bID, Lv: lv, Busy: uT > 0}
			s.Buildings = append(s.Buildings, b)
			if uT > nowMs {
				running[strconv.FormatInt(uuid, 10)], runningUT[strconv.FormatInt(uuid, 10)] = b, uT
			}
		}
	}

	for _, q := range in.Objects("queue_new") {
		uuid, ok := claimInt(q, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		if typ, _ := claimInt(q, "type"); typ != productionUpgradeQueueBld {
			continue
		}
		if unlock, _ := claimInt(q, "unlock"); unlock != 1 {
			continue
		}
		if exp, _ := claimInt(q, "expireTime"); exp > 0 && exp < nowMs {
			continue
		}
		if occ := evString(evObject(q, "itemObj"), "itemId"); occ != "" && occ != "0" && !ended[occ] {
			if b, ok := running[occ]; ok {
				end := time.Now().Add(time.Duration(runningUT[occ]-nowMs)*time.Millisecond + productionUpgradeSlack)
				t := productionUpgradeTarget{UUID: b.UUID, BID: b.BID, Lv: b.Lv, Rule: buildingUpgradeRules[int32(b.BID)].Levels[b.Lv]}
				s.Upgrading = append(s.Upgrading, productionUpgradeRun{t, uuid, end})
			}
			continue
		}
		s.Queues = append(s.Queues, uuid)
	}
	slices.Sort(s.Queues)
	return s
}

// productionUpgradeLevels maps each building type to its highest level, the value prerequisites are
// checked against.
func productionUpgradeLevels(bs []productionUpgradeBuilding) map[int64]int64 {
	top := map[int64]int64{}
	for _, b := range bs {
		top[b.BID] = max(top[b.BID], b.Lv)
	}
	return top
}

// productionUpgradeTargets lists the idle buildings that can go one level up now, in run order:
// the HQ, then what the HQ's next level needs, then the rest, shortest first. skip holds buildings
// the server refused this run.
func productionUpgradeTargets(bs []productionUpgradeBuilding, skip map[int64]bool) []productionUpgradeTarget {
	top := productionUpgradeLevels(bs)
	hq := top[claimHQBuildingID]
	hqNeeds := map[int64]int64{}
	if r, ok := buildingUpgradeRules[claimHQBuildingID].Levels[hq]; ok && hq < productionUpgradeHQCap {
		for _, n := range r.Needs {
			hqNeeds[n.BID] = n.Lv
		}
	}
	var ts []productionUpgradeTarget
	for _, b := range bs {
		if b.Busy || skip[b.UUID] || productionUpgradePaid[b.BID] != "" {
			continue
		}
		typ := buildingUpgradeRules[int32(b.BID)]
		limit := min(hq, productionUpgradeHQCap)
		if b.BID == claimHQBuildingID {
			limit = productionUpgradeHQCap
		}
		if b.Lv >= limit || b.Lv >= typ.Max {
			continue
		}
		r, ok := typ.Levels[b.Lv]
		if !ok || r.Gate != "" || !productionUpgradeNeedsMet(r, top) {
			continue
		}
		ts = append(ts, productionUpgradeTarget{UUID: b.UUID, BID: b.BID, Lv: b.Lv, Rule: r})
	}
	rank := func(t productionUpgradeTarget) int {
		switch {
		case t.BID == claimHQBuildingID:
			return 0
		case hqNeeds[t.BID] > t.Lv:
			return 1
		}
		return 2
	}
	slices.SortFunc(ts, func(a, b productionUpgradeTarget) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Rule.Seconds, b.Rule.Seconds), cmp.Compare(a.Lv, b.Lv),
			cmp.Compare(a.BID, b.BID), cmp.Compare(a.UUID, b.UUID))
	})
	return ts
}

func productionUpgradeNeedsMet(r buildingUpgradeLevel, top map[int64]int64) bool {
	for _, n := range r.Needs {
		if top[n.BID] < n.Lv {
			return false
		}
	}
	return true
}

// productionUpgradeMissing names the buildings the player has to place in game: the types an HQ
// level up to the cap needs that the city lacks (once the current HQ unlocks them), then extra
// instances the HQ level allows of types the city has.
func productionUpgradeMissing(bs []productionUpgradeBuilding) []string {
	top := productionUpgradeLevels(bs)
	hq := top[claimHQBuildingID]
	count := map[int64]int64{}
	for _, b := range bs {
		count[b.BID]++
	}
	allowed := func(id int64) int64 {
		var n int64
		for _, u := range buildingUpgradeRules[int32(id)].Unlock {
			if u.HQ <= hq {
				n = max(n, u.Count)
			}
		}
		return n
	}
	var out []string
	listed := map[int64]bool{}
	for lv := hq; lv < productionUpgradeHQCap; lv++ {
		for _, n := range buildingUpgradeRules[claimHQBuildingID].Levels[lv].Needs {
			if count[n.BID] == 0 && !listed[n.BID] && allowed(n.BID) > 0 {
				out = append(out, fmt.Sprintf("%s (needed for HQ %d)", productionUpgradeName(n.BID), lv+1))
				listed[n.BID] = true
			}
		}
	}
	// More instances of a type the city already has (a third Iron Mine at HQ 8, say). Types it has
	// none of are decorations or optional unless the HQ needs them, listed above.
	ids := make([]int64, 0, len(count))
	for id := range count {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if have, n := count[id], allowed(id); have < n {
			out = append(out, fmt.Sprintf("%s %d/%d", productionUpgradeName(id), have, n))
		}
	}
	return out
}

func productionUpgradeParams(t productionUpgradeTarget, queue int64) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutUtfString("uuid", strconv.FormatInt(t.UUID, 10))
	p.PutUtfString("clientParam", "")
	p.PutInt("gold", productionUpgradeGoldNo)
	p.PutInt("upLevel", int32(t.Lv+1))
	p.PutInt("truckId", 0)
	p.PutInt("pathTime", 0)
	p.PutLong("robotUuid", queue)
	return p
}

// productionUpgradeLeaves reports whether paying r leaves at least reserve of each resource.
func productionUpgradeLeaves(res map[string]int64, r, reserve buildingUpgradeLevel) bool {
	return res["metal"]-r.Metal >= reserve.Metal && res["money"]-r.Money >= reserve.Money && res["wood"]-r.Wood >= reserve.Wood
}

func runProductionUpgrades(conn *session.GameConn, in *Init, plan bool) error {
	feature := "production-upgrades"
	if plan {
		feature += "-plan"
	}
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	now := evNow().UnixMilli()
	if plan {
		gold, _ := evNum(in.Object("user"), "gold")
		slog.Info(feature+": diamonds", "user.gold", gold)
		productionUpgradeLogRaw(in, feature)
	}
	opened, openErr := productionUpgradeOpenEnded(conn, in, feature, now, plan)
	if session.ContainsNonTimeoutNetError(openErr) {
		return openErr
	}
	s := productionUpgradeStateOf(in, now, opened)
	resObj := in.Object("resource")
	res := map[string]int64{}
	for _, k := range []string{"metal", "money", "wood"} {
		n, ok := evNum(resObj, k)
		if !ok {
			slog.Info(feature+": init has no resource."+k+", so costs can't be checked; not upgrading", "resource", k)
			return openErr
		}
		res[k] = n
	}
	if s.HQ == 0 {
		slog.Info(feature + ": no Headquarters in init; skipping")
		return openErr
	}
	busy := 0
	for _, b := range s.Buildings {
		if b.Busy {
			busy++
		}
	}
	slog.Info(feature+": state", "hq", s.HQ, "hqCap", productionUpgradeHQCap, "upgradable", len(productionUpgradeTargets(s.Buildings, nil)),
		"upgrading", busy, "freeQueues", len(s.Queues), "iron", res["metal"], "food", res["money"], "gold", res["wood"])
	if missing := productionUpgradeMissing(s.Buildings); len(missing) > 0 {
		slog.Warn(feature+": unlocked buildings the city doesn't have; place them in game", "missing", missing)
	}

	var g *duelSpendGuard
	if !plan && (len(s.Queues) > 0 || len(s.Upgrading) > 0) {
		var err error
		if g, err = newDuelSpendGuard(conn, in, feature); g == nil {
			return errors.Join(openErr, err)
		}
		g.res = res // the guard lowers it from pushed balances
	}

	bs, free := s.Buildings, s.Queues
	skip := map[int64]bool{}
	var running []productionUpgradeRun
	if !plan {
		running = s.Upgrading // the plan doesn't wait
	}
	deadline := time.Now().Add(productionUpgradeChain)
	errs := []error{openErr}
	for {
		for len(free) > 0 {
			targets := productionUpgradeTargets(bs, skip)
			if len(targets) == 0 {
				slog.Info(feature+": nothing left to upgrade now", "hq", productionUpgradeLevels(bs)[claimHQBuildingID])
				break
			}
			var reserve buildingUpgradeLevel
			if targets[0].BID == claimHQBuildingID {
				reserve = targets[0].Rule
			}
			i := slices.IndexFunc(targets, func(t productionUpgradeTarget) bool {
				if t.BID == claimHQBuildingID {
					return productionUpgradeLeaves(res, t.Rule, buildingUpgradeLevel{})
				}
				return productionUpgradeLeaves(res, t.Rule, reserve)
			})
			if i < 0 {
				slog.Info(feature+": nothing the resources cover now", "next", targets[0].describe(), "savingForHQ", reserve.Metal > 0,
					"iron", res["metal"], "food", res["money"], "gold", res["wood"])
				break
			}
			t, q := targets[i], free[0]
			label := fmt.Sprintf("%s: upgrade %s", feature, t.describe())
			params := productionUpgradeParams(t, q)
			bi := slices.IndexFunc(bs, func(b productionUpgradeBuilding) bool { return b.UUID == t.UUID })
			if plan {
				slog.Info(label+" (plan: not sent)", "cmd", productionUpgradeCmd, "queue", q, "seconds", t.Rule.Seconds,
					"params", params.StringRedacted())
				bs[bi].Busy = true
			} else {
				msg, err := g.spend(label, productionUpgradeCmd, params)
				if err != nil {
					if msg == nil || g.tripped != nil {
						return errors.Join(append(errs, err)...)
					}
					// Refused (errorCode): leave this building for a later run and try the next one.
					slog.Warn(label+": refused; skipping it this run", "response", msg.Params.StringRedacted())
					skip[t.UUID] = true
					continue
				}
				if err := g.check(); err != nil {
					return errors.Join(append(errs, err)...)
				}
				if err := productionUpgradeCallHelp(conn, in, feature, t); session.ContainsNonTimeoutNetError(err) {
					return errors.Join(append(errs, err)...)
				}
				bs[bi].Busy = true
				end := time.Now().Add(time.Duration(t.Rule.Seconds)*productionUpgradeSecond + productionUpgradeSlack)
				running = append(running, productionUpgradeRun{t, q, end})
			}
			if !t.Rule.NoQueue {
				free = free[1:]
			}
			res["metal"] -= t.Rule.Metal
			res["money"] -= t.Rule.Money
			res["wood"] -= t.Rule.Wood
		}
		if len(running) == 0 {
			break
		}
		// Chain: wait for the upgrade that ends first, open it, and plan again from the new levels,
		// as long as it ends within this run's budget.
		slices.SortFunc(running, func(a, b productionUpgradeRun) int { return a.End.Compare(b.End) })
		r := running[0]
		if r.End.After(deadline) {
			slog.Info(feature+": the next upgrade ends after this run's budget; leaving it for a later run",
				"upgrade", r.Target.describe(), "ends", r.End.UTC().Format(time.TimeOnly))
			break
		}
		if err := productionUpgradeIdle(conn, g, time.Until(r.End)); err != nil {
			return errors.Join(append(errs, err)...)
		}
		running = running[1:]
		p := sfs.NewSFSObject()
		p.PutLong("uuid", r.Target.UUID)
		label := fmt.Sprintf("%s: open finished upgrade %s %d lv%d->%d", feature, productionUpgradeName(r.Target.BID),
			r.Target.UUID, r.Target.Lv, r.Target.Lv+1)
		if _, err := claimAndLog(conn, label, finishedTimersUpgradeCmd, p); err != nil {
			return errors.Join(append(errs, err)...)
		}
		if bi := slices.IndexFunc(bs, func(b productionUpgradeBuilding) bool { return b.UUID == r.Target.UUID }); bi >= 0 {
			bs[bi].Lv, bs[bi].Busy = r.Target.Lv+1, false
		}
		if !r.Target.Rule.NoQueue {
			free = append(free, r.Queue)
		}
	}
	return errors.Join(errs...)
}

// productionUpgradeCallHelp presses the upgrade's alliance help button: al.call.help {uuid: Long
// building, type: Int 0 (AllianceHelpType building), qType: Int 0, itemId: ""}, the city bubble's
// request (AllianceCallHelpMessage.lua:12-15; see feature_alliance_call_help.go). Free; each member
// who helps shortens the timer. A failure is logged and the run goes on.
func productionUpgradeCallHelp(conn *session.GameConn, in *Init, feature string, t productionUpgradeTarget) error {
	if !claimInAlliance(in) {
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutLong("uuid", t.UUID)
	p.PutInt("type", 0)
	p.PutInt("qType", 0)
	p.PutUtfString("itemId", "")
	_, err := session.SendAndWait(conn, fmt.Sprintf("%s: alliance help request for %s", feature, t.describe()), callHelpCmd, p)
	if err != nil && !session.ContainsNonTimeoutNetError(err) {
		slog.Warn(feature+": alliance help request failed; the upgrade runs anyway", "uuid", t.UUID, "error", err)
	}
	return err
}

// productionUpgradeRun is an upgrade started this run, on Queue, expected to end at End.
type productionUpgradeRun struct {
	Target productionUpgradeTarget
	Queue  int64
	End    time.Time
}

// productionUpgradeIdle keeps reading the connection for d, so pushes reach the diamond tripwire and
// a terminal push (the player logging in on the phone, maintenance) ends the wait with its error.
func productionUpgradeIdle(conn *session.GameConn, g *duelSpendGuard, d time.Duration) error {
	if d > 0 {
		_, err := session.WaitFor(conn, d, func(e *session.Envelope) bool {
			if msg, ok := e.AsExtension(); ok {
				g.observe(msg, "")
			}
			return false
		})
		var ne net.Error
		if err != nil && !(errors.As(err, &ne) && ne.Timeout()) {
			return err
		}
	}
	return g.tripped
}

// productionUpgradeOpenEnded collects ("opens the gift of") every ended upgrade of a city building
// with free.building.upgrade.finish {uuid: Long}, as finished-timers does, so the new levels count
// and the build queue frees up this run. The HQ is opened too, HQ 8 included, even though finishing
// it drops the newbie shield (Kevin's choice, 2026-10-21). The plan only logs them and counts them
// as opened.
func productionUpgradeOpenEnded(conn *session.GameConn, in *Init, feature string, nowMs int64, plan bool) (map[int64]bool, error) {
	opened := map[int64]bool{}
	var errs []error
	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		if _, city := buildingUpgradeRules[int32(bID)]; !city {
			continue
		}
		uT, _ := claimInt(b, "uT")
		if uT <= 0 || nowMs < uT {
			continue
		}
		lv, _ := claimInt(b, "lv")
		label := fmt.Sprintf("%s: open finished upgrade %s %d lv%d->%d", feature, productionUpgradeName(bID), uuid, lv, lv+1)
		if plan {
			slog.Info(label+" (plan: not sent)", "cmd", finishedTimersUpgradeCmd)
			opened[uuid] = true
			continue
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		if _, err := claimAndLog(conn, label, finishedTimersUpgradeCmd, p); err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				break
			}
			continue
		}
		opened[uuid] = true
	}
	return opened, errors.Join(errs...)
}

// productionUpgradeLogRaw logs the init entries the plan reads: the city buildings and every
// queue_new entry, so a surprising plan can be checked against what the server sent.
func productionUpgradeLogRaw(in *Init, feature string) {
	for _, b := range in.Objects("building_new") {
		bID, _ := claimInt(b, "bId")
		if _, ok := buildingUpgradeRules[int32(bID)]; ok {
			slog.Info(feature+": building", "name", productionUpgradeName(bID), "raw", b.StringRedacted())
		}
	}
	for _, q := range in.Objects("queue_new") {
		slog.Info(feature+": queue", "raw", q.StringRedacted())
	}
}

// productionUpgradeName prefers the table's name for types BuildingNameOf doesn't know.
func productionUpgradeName(bID int64) string {
	if n := buildingUpgradeRules[int32(bID)].Name; n != "" {
		if _, known := buildingNames[int32(bID)]; !known {
			return n
		}
	}
	return BuildingNameOf(int32(bID))
}

func (t productionUpgradeTarget) describe() string {
	return fmt.Sprintf("%s %d lv%d->%d (iron %d, food %d, gold %d, %ds)", productionUpgradeName(t.BID), t.UUID, t.Lv,
		t.Lv+1, t.Rule.Metal, t.Rule.Money, t.Rule.Wood, t.Rule.Seconds)
}
