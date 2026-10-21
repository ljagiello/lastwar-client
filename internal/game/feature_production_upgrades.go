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

// Production-line upgrades (OPT-IN): raise the Farmlands, Iron Mines, Gold Mines and Training
// Bases to the level the Headquarters allows, one upgrade per free build queue per run, paid with
// iron, food and gold coins only. Read from the live 831 Lua bundle and table archive 39746:
//
//   - Request: `free.building.up.new` as the upgrade panel sends it (UIBuildUpgradeView.lua:
//     1858-1875; FreeBuildingUpNewMessage.lua:7-43): uuid as a UtfString (tostring), clientParam
//     "", gold Int BuildUpgradeUseGoldType.No (0; Yes = 2 pays diamonds, Free = 3), upLevel Int the
//     next level, truckId 0, pathTime 0 (only buildings with scan = BuildScanAnim.Play fly a drone;
//     these four have scan 2, NoFly), and robotUuid Long, the free build queue
//     (BuildQueueManager:IsCanUpgrade, :524-539). No workerUuid or goldCost is sent.
//   - Build queues are init queue_new entries of type LWBuildQueueType.NORMAL (0). One can take an
//     upgrade when unlock == 1, it hasn't expired (expireTime <= 0 or in the future), and nothing
//     occupies it (no itemObj) or its building's upgrade has ended (BuildQueueInfo.lua:51-236).
//   - Gates: the table's only prerequisite for each level is the Headquarters at lv+1, so the target
//     is min(HQ level, 35). An ended upgrade (uT > 0, now >= uT) is first collected with the free
//     free.building.upgrade.finish -- the in-game "gift" -- for the HQ and the four line types, which
//     raises the level, frees the queue and lets the next upgrade start the same run. A building
//     still upgrading is skipped.
//   - Cost: cost_consume of row bId+lv (production_upgrade_costs_gen.go) against init resource.metal,
//     money and wood. The client may apply cost reductions; the full cost is the safe side.
//   - Chain: when an upgrade started this run ends within productionUpgradeChain, the run waits
//     (still reading pushes), opens it and starts the next level, so short low-level upgrades
//     don't wait for the next scheduled run.
//   - Every send goes through the duel spend guard: the diamond tripwire and a gold check after each
//     upgrade.
//
// production-upgrades-plan logs the same choices and sends nothing.
// The chain budget: how long one run keeps starting upgrades as earlier ones end (well inside the
// service's 15 min TimeoutStartSec), the length of one table second and the margin added to each
// upgrade's table time before opening it. Tests shrink them.
var (
	productionUpgradeChain  = 10 * time.Minute
	productionUpgradeSecond = time.Second
	productionUpgradeSlack  = 3 * time.Second
)

const (
	productionUpgradeCmd      = "free.building.up.new"
	productionUpgradeQueueBld = 0  // LWBuildQueueType.NORMAL
	productionUpgradeGoldNo   = 0  // BuildUpgradeUseGoldType.No
	productionUpgradeMaxLevel = 35 // building max_level for all four types
)

type productionUpgradeCost struct {
	Metal, Money, Wood, Seconds int64
}

func init() {
	registerFeature(Feature{
		Name:    "production-upgrades",
		Summary: "OPT-IN: upgrade Farmlands, Iron/Gold Mines and Training Bases up to the HQ level, one per free build queue, iron/food/gold only (diamond tripwire)",
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
	Cost          productionUpgradeCost
}

// productionUpgradeState is what init says about the HQ, the production lines and the build queues.
type productionUpgradeState struct {
	HQ      int64
	Targets []productionUpgradeTarget // below the cap and idle, cheapest level first
	Queues  []int64                   // free build queue uuids
	Busy    int                       // production lines skipped because they are upgrading
}

// productionUpgradeStateOf reads init. opened holds the buildings whose ended upgrade was collected
// this run: init still shows them at the old level with uT set, so they count one level up and idle.
func productionUpgradeStateOf(in *Init, nowMs int64, opened map[int64]bool) productionUpgradeState {
	var s productionUpgradeState
	ended := map[string]bool{} // building uuid -> its upgrade timer has ended
	type line struct{ uuid, bID, lv, uT int64 }
	var lines []line
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
		if _, ok := productionUpgradeCosts[int32(bID)]; ok {
			lines = append(lines, line{uuid, bID, lv, uT})
		}
	}
	limit := min(s.HQ, productionUpgradeMaxLevel)
	for _, l := range lines {
		if l.lv < 1 || l.lv >= limit {
			continue
		}
		if l.uT > 0 {
			s.Busy++
			continue
		}
		cost, ok := productionUpgradeCosts[int32(l.bID)][l.lv]
		if !ok {
			continue
		}
		s.Targets = append(s.Targets, productionUpgradeTarget{UUID: l.uuid, BID: l.bID, Lv: l.lv, Cost: cost})
	}
	productionUpgradeSort(s.Targets)

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
			continue
		}
		s.Queues = append(s.Queues, uuid)
	}
	slices.Sort(s.Queues)
	return s
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

func (c productionUpgradeCost) coveredBy(res map[string]int64) bool {
	return res["metal"] >= c.Metal && res["money"] >= c.Money && res["wood"] >= c.Wood
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
	slog.Info(feature+": state", "hq", s.HQ, "targetLevel", min(s.HQ, productionUpgradeMaxLevel),
		"belowTarget", len(s.Targets), "upgrading", s.Busy, "freeQueues", len(s.Queues),
		"iron", res["metal"], "food", res["money"], "gold", res["wood"])
	if s.HQ == 0 {
		slog.Info(feature + ": no Headquarters in init; skipping")
		return openErr
	}
	if len(s.Targets) == 0 {
		slog.Info(feature + ": every idle production line is at the HQ level")
		return openErr
	}
	if len(s.Queues) == 0 {
		slog.Info(feature + ": no free build queue")
		return openErr
	}

	var g *duelSpendGuard
	if !plan {
		var err error
		if g, err = newDuelSpendGuard(conn, in, feature); g == nil {
			return errors.Join(openErr, err)
		}
		g.res = res // the guard lowers it from pushed balances
	}

	limit := min(s.HQ, productionUpgradeMaxLevel)
	targets, free := s.Targets, s.Queues
	var running []productionUpgradeRun
	deadline := time.Now().Add(productionUpgradeChain)
	errs := []error{openErr}
	for {
		for len(free) > 0 && len(targets) > 0 {
			i := slices.IndexFunc(targets, func(t productionUpgradeTarget) bool { return t.Cost.coveredBy(res) })
			if i < 0 {
				slog.Info(feature+": nothing left that the resources cover", "next", targets[0].describe(), "iron", res["metal"],
					"food", res["money"], "gold", res["wood"])
				break
			}
			t, q := targets[i], free[0]
			targets, free = slices.Delete(targets, i, i+1), free[1:]
			label := fmt.Sprintf("%s: upgrade %s", feature, t.describe())
			params := productionUpgradeParams(t, q)
			if plan {
				slog.Info(label+" (plan: not sent)", "cmd", productionUpgradeCmd, "queue", q, "seconds", t.Cost.Seconds,
					"params", params.StringRedacted())
			} else {
				if _, err := g.spend(label, productionUpgradeCmd, params); err != nil {
					return errors.Join(append(errs, err)...)
				}
				if err := g.check(); err != nil {
					return errors.Join(append(errs, err)...)
				}
				end := time.Now().Add(time.Duration(t.Cost.Seconds)*productionUpgradeSecond + productionUpgradeSlack)
				running = append(running, productionUpgradeRun{t, q, end})
			}
			res["metal"] -= t.Cost.Metal
			res["money"] -= t.Cost.Money
			res["wood"] -= t.Cost.Wood
		}
		if len(running) == 0 {
			break
		}
		// Chain: wait for the upgrade that ends first, open it, and start the next one on its queue,
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
		label := fmt.Sprintf("%s: open finished upgrade %s %d lv%d->%d", feature, BuildingNameOf(int32(r.Target.BID)),
			r.Target.UUID, r.Target.Lv, r.Target.Lv+1)
		if _, err := claimAndLog(conn, label, finishedTimersUpgradeCmd, p); err != nil {
			return errors.Join(append(errs, err)...)
		}
		free = append(free, r.Queue)
		if next := r.Target.Lv + 1; next < limit {
			if cost, ok := productionUpgradeCosts[int32(r.Target.BID)][next]; ok {
				targets = append(targets, productionUpgradeTarget{UUID: r.Target.UUID, BID: r.Target.BID, Lv: next, Cost: cost})
				productionUpgradeSort(targets)
			}
		}
	}
	return errors.Join(errs...)
}

// productionUpgradeSort puts the cheapest level first.
func productionUpgradeSort(ts []productionUpgradeTarget) {
	slices.SortFunc(ts, func(a, b productionUpgradeTarget) int {
		return cmp.Or(cmp.Compare(a.Lv, b.Lv), cmp.Compare(a.BID, b.BID), cmp.Compare(a.UUID, b.UUID))
	})
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

// productionUpgradeOpenEnded collects ("opens the gift of") every ended upgrade of the HQ and the
// production lines with free.building.upgrade.finish {uuid: Long}, as finished-timers does, so the
// new levels count and the build queue frees up this run. HQ 7 is left alone while the newbie
// shield is up (finishing HQ 8 drops it). The plan only logs them and counts them as opened.
func productionUpgradeOpenEnded(conn *session.GameConn, in *Init, feature string, nowMs int64, plan bool) (map[int64]bool, error) {
	opened := map[int64]bool{}
	var errs []error
	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		_, line := productionUpgradeCosts[int32(bID)]
		if !line && bID != claimHQBuildingID {
			continue
		}
		uT, _ := claimInt(b, "uT")
		if uT <= 0 || nowMs < uT {
			continue
		}
		lv, _ := claimInt(b, "lv")
		if bID == claimHQBuildingID && lv == finishedTimersShieldHQLevel && finishedTimersShieldUp(in, nowMs) {
			slog.Info(feature+": leaving the HQ 8 upgrade to the player while the newbie shield is up", "uuid", uuid)
			continue
		}
		label := fmt.Sprintf("%s: open finished upgrade %s %d lv%d->%d", feature, BuildingNameOf(int32(bID)), uuid, lv, lv+1)
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

// productionUpgradeLogRaw logs the init entries the plan reads: the HQ, the production lines and
// every queue_new entry, so a surprising plan can be checked against what the server sent.
func productionUpgradeLogRaw(in *Init, feature string) {
	for _, b := range in.Objects("building_new") {
		bID, _ := claimInt(b, "bId")
		if _, ok := productionUpgradeCosts[int32(bID)]; ok || bID == claimHQBuildingID {
			slog.Info(feature+": building", "name", BuildingNameOf(int32(bID)), "raw", b.StringRedacted())
		}
	}
	for _, q := range in.Objects("queue_new") {
		slog.Info(feature+": queue", "raw", q.StringRedacted())
	}
}

func (t productionUpgradeTarget) describe() string {
	return fmt.Sprintf("%s %d lv%d->%d (iron %d, food %d, gold %d, %ds)", BuildingNameOf(int32(t.BID)), t.UUID, t.Lv,
		t.Lv+1, t.Cost.Metal, t.Cost.Money, t.Cost.Wood, t.Cost.Seconds)
}
