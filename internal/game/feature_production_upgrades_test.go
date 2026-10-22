package game

import (
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// upgradeLine is one building_new entry; uT is relative to evTestNow (0: not upgrading).
type upgradeLine struct {
	uuid    int64
	bID, lv int
	uT      time.Duration
}

// upgradeQueue is one queue_new entry; occupant is itemObj.itemId ("" for none).
type upgradeQueue struct {
	uuid, typ, unlock int64
	expire            time.Duration // relative to evTestNow; 0: never
	occupant          string
}

func upgradeInit(hq int, iron, food, gold int64, lines []upgradeLine, queues []upgradeQueue) *Init {
	in := evInit(int32(hq))
	in.Raw.PutSFSObject("user", duelObj("gold", int64(1000)))
	in.Raw.PutSFSObject("resource", duelObj("metal", iron, "money", food, "wood", gold))
	builds := sfs.NewSFSArray()
	builds.AddSFSObject(duelObj("uuid", int64(9000), "bId", int(claimHQBuildingID), "lv", hq))
	for _, l := range lines {
		b := duelObj("uuid", l.uuid, "bId", l.bID, "lv", l.lv)
		if l.uT != 0 {
			b.PutLong("uT", evTestNow.Add(l.uT).UnixMilli())
		}
		builds.AddSFSObject(b)
	}
	in.Raw.PutSFSArray("building_new", builds)
	qs := sfs.NewSFSArray()
	for _, q := range queues {
		o := duelObj("uuid", q.uuid, "qid", int64(1), "type", int(q.typ), "unlock", int(q.unlock))
		if q.expire != 0 {
			o.PutLong("expireTime", evTestNow.Add(q.expire).UnixMilli())
		}
		if q.occupant != "" {
			o.PutSFSObject("itemObj", duelObj("itemId", q.occupant))
		}
		qs.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("queue_new", qs)
	return in
}

// withUpgradeChain sets the chain budget and shrinks a table second to a millisecond.
func withUpgradeChain(t *testing.T, budget time.Duration) {
	t.Helper()
	chain, sec, slack := productionUpgradeChain, productionUpgradeSecond, productionUpgradeSlack
	productionUpgradeChain, productionUpgradeSecond, productionUpgradeSlack = budget, time.Millisecond, 0
	t.Cleanup(func() { productionUpgradeChain, productionUpgradeSecond, productionUpgradeSlack = chain, sec, slack })
}

func upgradeServer(gold int64) func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
	return func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch cmd {
		case duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(gold)}
		case productionUpgradeCmd:
			return []duelSpendMsg{duelReply(duelObj("uuid", p.GetString("uuid")))}
		}
		return nil
	}
}

// upgradesOf summarises the upgrades sent as "uuid>upLevel@queue" and checks each carries exactly
// the panel's keys with gold 0.
func upgradesOf(t *testing.T, f *duelSpendFake) []string {
	t.Helper()
	var out []string
	for _, r := range f.only(productionUpgradeCmd) {
		keys := slices.DeleteFunc(slices.Clone(r.Params.Keys()), func(k string) bool { return k == "_id" })
		slices.Sort(keys)
		want := []string{"clientParam", "gold", "pathTime", "robotUuid", "truckId", "upLevel", "uuid"}
		if !slices.Equal(keys, want) {
			t.Errorf("upgrade keys %v, want %v", keys, want)
		}
		if r.Params.GetInt("gold") != 0 || r.Params.Has("goldCost") {
			t.Errorf("upgrade spends diamonds: %v", r.Params)
		}
		out = append(out, r.Params.GetString("uuid")+">"+strconv.Itoa(int(r.Params.GetInt("upLevel")))+"@"+strconv.FormatInt(r.Params.GetLong("robotUuid"), 10))
	}
	return out
}

func TestProductionUpgradesState(t *testing.T) {
	withEvNow(t, evTestNow)
	lines := []upgradeLine{
		{uuid: 1, bID: int(BuildingFarmland), lv: 3},                       // 117 s
		{uuid: 2, bID: int(BuildingIronMine), lv: 1},                       // 12 s
		{uuid: 3, bID: int(BuildingGoldMine), lv: 5},                       // at the HQ level
		{uuid: 4, bID: int(BuildingTrainingBase), lv: 2, uT: time.Hour},    // upgrading
		{uuid: 5, bID: int(BuildingTrainingBase), lv: 2, uT: -time.Minute}, // ended, not collected yet
		{uuid: 6, bID: 10103000, lv: 1},                                    // Military Camp, 3 s
		{uuid: 7, bID: int(BuildingIronMine), lv: 0},                       // not built: its row needs PvE
	}
	queues := []upgradeQueue{
		{uuid: 11, typ: 0, unlock: 1},                     // free
		{uuid: 12, typ: 0, unlock: 1, occupant: "4"},      // busy
		{uuid: 13, typ: 0, unlock: 1, occupant: "5"},      // occupant ended: free
		{uuid: 14, typ: 0, unlock: 0},                     // locked
		{uuid: 15, typ: 0, unlock: 1, expire: -time.Hour}, // rented, expired
		{uuid: 16, typ: 0, unlock: 1, expire: time.Hour},  // rented, running
		{uuid: 17, typ: 110, unlock: 1},                   // special queue
		{uuid: 18, typ: 6, unlock: 1},                     // research queue
	}
	s := productionUpgradeStateOf(upgradeInit(5, 0, 0, 0, lines, queues), evTestNow.UnixMilli(), nil)
	busy := 0
	for _, b := range s.Buildings {
		if b.Busy {
			busy++
		}
	}
	if s.HQ != 5 || busy != 2 {
		t.Errorf("hq %d busy %d, want 5 and 2", s.HQ, busy)
	}
	// The HQ 5 -> 6 needs a Wall and a Drill Ground at 5, which this city lacks.
	if got := targetUUIDs(productionUpgradeTargets(s.Buildings, nil)); !slices.Equal(got, []int64{6, 2, 1}) {
		t.Errorf("targets %v, want [6 2 1] (shortest first)", got)
	}
	if got := targetUUIDs(productionUpgradeTargets(s.Buildings, map[int64]bool{6: true})); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("targets skipping 6: %v, want [2 1]", got)
	}
	if !slices.Equal(s.Queues, []int64{11, 13, 16}) {
		t.Errorf("queues %v, want [11 13 16]", s.Queues)
	}
}

func targetUUIDs(ts []productionUpgradeTarget) []int64 {
	var out []int64
	for _, tg := range ts {
		out = append(out, tg.UUID)
	}
	return out
}

// The HQ goes first once its prerequisites are met; before that, the buildings it needs do.
func TestProductionUpgradesHQAndItsNeedsFirst(t *testing.T) {
	bs := []productionUpgradeBuilding{
		{UUID: 9000, BID: claimHQBuildingID, Lv: 4},
		{UUID: 1, BID: int64(BuildingFarmland), Lv: 1}, // 12 s
		{UUID: 2, BID: 10107000, Lv: 3},                // Wall, 117 s: HQ 4 -> 5 needs it at 4
		{UUID: 3, BID: 10103000, Lv: 4},                // Military Camp at 4: met
	}
	if got := targetUUIDs(productionUpgradeTargets(bs, nil)); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("targets %v, want [2 1] (the HQ's prerequisite first)", got)
	}
	bs[2].Lv = 4
	if got := targetUUIDs(productionUpgradeTargets(bs, nil)); !slices.Equal(got, []int64{9000, 1}) {
		t.Errorf("targets %v, want [9000 1] (the HQ first)", got)
	}
	bs[0].Lv = productionUpgradeHQCap
	for _, tg := range productionUpgradeTargets(bs, nil) {
		if tg.BID == claimHQBuildingID {
			t.Errorf("HQ %d is past the cap", productionUpgradeHQCap)
		}
	}
}

// While the HQ is allowed but not affordable, nothing else spends what it needs.
func TestProductionUpgradesSavesForTheHQ(t *testing.T) {
	lines := []upgradeLine{
		{uuid: 2, bID: 10107000, lv: 5}, // Wall
		{uuid: 3, bID: 10104000, lv: 5}, // Drill Ground
		{uuid: 1, bID: int(BuildingFarmland), lv: 1},
	}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}, {uuid: 12, typ: 0, unlock: 1}}
	withUpgradeChain(t, 0)

	// HQ 5 -> 6 costs 91000 iron and food: affordable, then the Farmland fits in what is left.
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, upgradeInit(5, 100_000, 100_000, 0, lines, queues), false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := upgradesOf(t, f); !slices.Equal(got, []string{"9000>6@11", "1>2@12"}) {
		t.Errorf("sent %v, want [9000>6@11 1>2@12]", got)
	}

	// Not affordable: the Farmland would eat into the HQ's savings, so nothing starts.
	conn, f = startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, upgradeInit(5, 91_100, 50_000, 0, lines, queues), false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := upgradesOf(t, f); len(got) != 0 {
		t.Errorf("sent %v while saving for the HQ", got)
	}
}

func TestProductionUpgradesListsMissingBuildings(t *testing.T) {
	bs := []productionUpgradeBuilding{
		{UUID: 9000, BID: claimHQBuildingID, Lv: 6},
		{UUID: 2, BID: 10107000, Lv: 6},
		{UUID: 6, BID: 10106000, Lv: 1}, // Alliance Center, needed for HQ 8
		{UUID: 7, BID: 10124000, Lv: 1}, // Hospital, needed for HQ 10
	}
	missing := productionUpgradeMissing(bs)
	if len(missing) == 0 || missing[0] != "Tank Center (needed for HQ 7)" {
		t.Errorf("missing %v, want the Tank Center first", missing)
	}
	if slices.ContainsFunc(missing[1:], func(m string) bool { return strings.HasPrefix(m, "Tank Center") }) {
		t.Errorf("Tank Center listed twice: %v", missing)
	}
	if len(missing) != 1 {
		t.Errorf("missing %v, want only the Tank Center (the Tech Center unlocks at HQ 7)", missing)
	}
	// HQ 8 unlocks a third Iron Mine; the Tech Center the HQ needs is now unlocked too.
	bs = append(bs, productionUpgradeBuilding{UUID: 3, BID: int64(BuildingIronMine), Lv: 8},
		productionUpgradeBuilding{UUID: 4, BID: int64(BuildingIronMine), Lv: 8},
		productionUpgradeBuilding{UUID: 5, BID: 10116000, Lv: 8})
	bs[0].Lv = 8
	missing = productionUpgradeMissing(bs)
	if !slices.Contains(missing, "1st Tech Center (needed for HQ 9)") || !slices.Contains(missing, "Iron Mine 2/3") {
		t.Errorf("missing %v, want the Tech Center and a third Iron Mine", missing)
	}
}

func TestProductionUpgradesSendsOnePerQueueWithinResources(t *testing.T) {
	lines := []upgradeLine{
		{uuid: 1, bID: int(BuildingFarmland), lv: 3}, // 2500 iron, 840 food
		{uuid: 2, bID: int(BuildingIronMine), lv: 1}, // cheapest
		{uuid: 3, bID: int(BuildingGoldMine), lv: 2},
	}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}, {uuid: 12, typ: 0, unlock: 1}}
	in := upgradeInit(6, 1500, 1000, 0, lines, queues) // covers lv1 and one lv2, not lv3
	withUpgradeChain(t, 0)
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	got := upgradesOf(t, f)
	want := []string{"2>2@11", "3>3@12"}
	if !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
	if n := len(f.only(duelGoldProbeCmd)); n != 3 {
		t.Errorf("%d gold checks, want 3 (before and after each upgrade)", n)
	}
}

func TestProductionUpgradesPlanSendsNothing(t *testing.T) {
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingFarmland), lv: 1}}
	in := upgradeInit(5, 1_000_000, 1_000_000, 1_000_000, lines, []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}})
	withUpgradeChain(t, 0)
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, true); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if reqs := f.requests(); len(reqs) != 0 {
		t.Errorf("plan sent %d requests", len(reqs))
	}
}

func TestProductionUpgradesStopsOnDiamondDropAndError(t *testing.T) {
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingFarmland), lv: 1}, {uuid: 2, bID: int(BuildingIronMine), lv: 1}}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}, {uuid: 12, typ: 0, unlock: 1}}
	in := upgradeInit(5, 1_000_000, 1_000_000, 0, lines, queues)

	withUpgradeChain(t, 0)
	probes := 0
	conn, f := startDuelSpendFake(t, nil, func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelGoldProbeCmd {
			probes++
			if probes > 1 {
				return []duelSpendMsg{duelGoldReply(900)} // fell after the first upgrade
			}
		}
		return upgradeServer(1000)(cmd, p)
	})
	if err := runProductionUpgrades(conn, in, false); err == nil {
		t.Fatal("want the tripwire error")
	}
	if n := len(upgradesOf(t, f)); n != 1 {
		t.Errorf("%d upgrades sent, want 1 before the tripwire", n)
	}

	// A refused upgrade skips that building and the run tries the next one.
	in = upgradeInit(5, 1_000_000, 1_000_000, 0, lines, queues)
	conn, f = startDuelSpendFake(t, nil, func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == productionUpgradeCmd && p.GetString("uuid") == "1" {
			return []duelSpendMsg{duelReply(duelObj("errorCode", "E100009"))}
		}
		return upgradeServer(1000)(cmd, p)
	})
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := upgradesOf(t, f); !slices.Equal(got, []string{"1>2@11", "2>2@11"}) {
		t.Errorf("sent %v, want [1>2@11 2>2@11] (the refused one, then the next)", got)
	}
}

func TestBuildingUpgradeRulesTable(t *testing.T) {
	hq := buildingUpgradeRules[claimHQBuildingID]
	for lv := int64(1); lv < productionUpgradeHQCap; lv++ {
		r, ok := hq.Levels[lv]
		if !ok || r.Metal <= 0 || r.Seconds <= 0 || len(r.Needs) == 0 || r.Gate != "" {
			t.Errorf("HQ lv%d: %+v", lv, r)
		}
	}
	for _, bID := range []int32{BuildingFarmland, BuildingIronMine, BuildingGoldMine, BuildingTrainingBase} {
		for lv := int64(1); lv < productionUpgradeHQCap; lv++ {
			r := buildingUpgradeRules[bID].Levels[lv]
			if r.Metal <= 0 || r.Seconds <= 0 || !slices.Equal(r.Needs, []buildingNeed{{int64(claimHQBuildingID), lv + 1}}) {
				t.Errorf("%s lv%d: %+v", BuildingNameOf(bID), lv, r)
			}
		}
	}
	// Live 2026-10-21: the Wall 5 -> 6 cost 91000 iron, 30000 food and took 2106 s.
	if r := buildingUpgradeRules[10107000].Levels[5]; r.Metal != 91000 || r.Money != 30000 || r.Seconds != 2106 {
		t.Errorf("Wall lv5: %+v", r)
	}
}

// The live state of 2026-10-21: every line and the HQ ended an upgrade the player never opened, and
// the HQ holds the only build queue. The run opens all of them, then upgrades from the new levels.
func TestProductionUpgradesOpensEndedUpgradesFirst(t *testing.T) {
	lines := []upgradeLine{
		{uuid: 1, bID: int(BuildingFarmland), lv: 3, uT: -20 * time.Minute}, // becomes 4
		{uuid: 2, bID: int(BuildingGoldMine), lv: 1, uT: -15 * time.Minute}, // becomes 2
		{uuid: 3, bID: int(BuildingIronMine), lv: 2, uT: time.Minute},       // still running
	}
	in := upgradeInit(5, 1_000_000, 1_000_000, 1_000_000, lines,
		[]upgradeQueue{{uuid: 11, typ: 0, unlock: 1, occupant: "9000"}})
	hq := in.Objects("building_new")[0]
	hq.PutLong("uT", evTestNow.Add(-time.Minute).UnixMilli()) // HQ 5 -> 6 ended

	withUpgradeChain(t, 0)
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	var finished []int64
	for _, r := range f.only(finishedTimersUpgradeCmd) {
		finished = append(finished, r.Params.GetLong("uuid"))
	}
	if !slices.Equal(finished, []int64{9000, 1, 2}) {
		t.Errorf("opened %v, want [9000 1 2]", finished)
	}
	// HQ 6 now: the Gold Mine at 2 is the cheapest, through the queue the HQ left.
	if got := upgradesOf(t, f); !slices.Equal(got, []string{"2>3@11"}) {
		t.Errorf("sent %v, want [2>3@11]", got)
	}
}

// Kevin chose to go past HQ 8: its ended upgrade is opened even while the newbie shield is up.
func TestProductionUpgradesOpensHQ8UnderTheShield(t *testing.T) {
	in := upgradeInit(7, 0, 0, 0, nil, nil)
	in.Objects("building_new")[0].PutLong("uT", evTestNow.Add(-time.Minute).UnixMilli())
	in.Raw.PutSFSObject("defend_wall", duelObj("protectEndTime", evTestNow.Add(time.Hour).UnixMilli()))
	withUpgradeChain(t, 0)
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(f.only(finishedTimersUpgradeCmd)); n != 1 {
		t.Errorf("opened %d upgrades, want the HQ 8 one", n)
	}
}

// With a budget, the run waits for each upgrade, opens it and starts the next level on the same
// queue until the line reaches the HQ level.
func TestProductionUpgradesChainsWithinBudget(t *testing.T) {
	withUpgradeChain(t, 5*time.Second)
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingIronMine), lv: 1}} // 12 and 47 table seconds
	in := upgradeInit(3, 1_000_000, 1_000_000, 0, lines, []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}})
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := upgradesOf(t, f); !slices.Equal(got, []string{"1>2@11", "1>3@11"}) {
		t.Errorf("sent %v, want [1>2@11 1>3@11]", got)
	}
	var order []string
	for _, r := range f.requests() {
		if r.Cmd == productionUpgradeCmd || r.Cmd == finishedTimersUpgradeCmd {
			order = append(order, r.Cmd)
		}
	}
	want := []string{productionUpgradeCmd, finishedTimersUpgradeCmd, productionUpgradeCmd, finishedTimersUpgradeCmd}
	if !slices.Equal(order, want) {
		t.Errorf("order %v, want %v", order, want)
	}
}

// The live reply of 2026-10-21 carried gold=0, the echo of the request's payment mode, while the
// account had diamonds: it must not trip the guard.
func TestProductionUpgradesReplyGoldEchoIsNotABalance(t *testing.T) {
	withUpgradeChain(t, 0)
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingGoldMine), lv: 2}, {uuid: 2, bID: int(BuildingIronMine), lv: 2}}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}, {uuid: 12, typ: 0, unlock: 1}}
	in := upgradeInit(6, 1_000_000, 1_000_000, 0, lines, queues)
	conn, f := startDuelSpendFake(t, nil, func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == productionUpgradeCmd {
			return []duelSpendMsg{duelReply(duelObj("gold", 0, "uuid", p.GetString("uuid")))}
		}
		return nil // the gold check gets no balance, as live
	})
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(upgradesOf(t, f)); n != 2 {
		t.Errorf("%d upgrades sent, want 2", n)
	}
	if duelSpendHalted.Load() {
		t.Error("the echoed gold=0 tripped the guard")
	}
}

// A pushed balance lower than init still trips it.
func TestProductionUpgradesPushedDiamondDropTrips(t *testing.T) {
	withUpgradeChain(t, 0)
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingGoldMine), lv: 2}, {uuid: 2, bID: int(BuildingIronMine), lv: 2}}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}, {uuid: 12, typ: 0, unlock: 1}}
	in := upgradeInit(6, 1_000_000, 1_000_000, 0, lines, queues)
	conn, f := startDuelSpendFake(t, nil, func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == productionUpgradeCmd {
			return []duelSpendMsg{duelPush(duelPushRemainGold, duelObj("gold", int64(10))), duelReply(duelObj("gold", 0))}
		}
		return nil
	})
	if err := runProductionUpgrades(conn, in, false); err == nil {
		t.Fatal("want the tripwire error")
	}
	if n := len(upgradesOf(t, f)); n != 1 {
		t.Errorf("%d upgrades sent, want 1", n)
	}
}

func TestProductionUpgradesAsksAllianceHelp(t *testing.T) {
	withUpgradeChain(t, 0)
	lines := []upgradeLine{{uuid: 1, bID: int(BuildingGoldMine), lv: 2}}
	queues := []upgradeQueue{{uuid: 11, typ: 0, unlock: 1}}
	for _, inAlliance := range []bool{true, false} {
		in := upgradeInit(6, 1_000_000, 1_000_000, 0, lines, queues)
		if inAlliance {
			in.Raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}
		conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
		if err := runProductionUpgrades(conn, in, false); err != nil {
			t.Fatalf("run: %v", err)
		}
		helps := f.only(callHelpCmd)
		if !inAlliance {
			if len(helps) != 0 {
				t.Errorf("help asked outside an alliance")
			}
			continue
		}
		if len(helps) != 1 {
			t.Fatalf("%d help requests, want 1", len(helps))
		}
		h := helps[0].Params
		if h.GetLong("uuid") != 1 || h.GetInt("type") != 0 || h.GetInt("qType") != 0 || h.GetString("itemId") != "" {
			t.Errorf("help params %v", h)
		}
	}
}

// A full queue at login no longer ends the run: it waits for the running upgrade, opens it and
// starts the next one.
func TestProductionUpgradesWaitsForARunningUpgrade(t *testing.T) {
	withEvNow(t, evTestNow)
	withUpgradeChain(t, 5*time.Second)
	lines := []upgradeLine{
		{uuid: 1, bID: int(BuildingIronMine), lv: 1, uT: 200 * time.Millisecond}, // ends soon, on queue 11
		{uuid: 2, bID: int(BuildingGoldMine), lv: 1},
	}
	in := upgradeInit(3, 1_000_000, 1_000_000, 0, lines, []upgradeQueue{{uuid: 11, typ: 0, unlock: 1, occupant: "1"}})
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	var order []string
	for _, r := range f.requests() {
		switch r.Cmd {
		case finishedTimersUpgradeCmd:
			order = append(order, "open "+strconv.FormatInt(r.Params.GetLong("uuid"), 10))
		case productionUpgradeCmd:
			order = append(order, "up "+r.Params.GetString("uuid")+">"+strconv.Itoa(int(r.Params.GetInt("upLevel"))))
		}
	}
	// The Iron Mine opens at 2; then the shortest of Gold Mine 1->2 (12 s) and Iron Mine 2->3 (47 s).
	if len(order) < 2 || order[0] != "open 1" || order[1] != "up 2>2" {
		t.Errorf("order %v, want [open 1, up 2>2, ...]", order)
	}
}

func TestProductionUpgradesLeavesGatedAndPaidTypes(t *testing.T) {
	bs := []productionUpgradeBuilding{
		{UUID: 9000, BID: claimHQBuildingID, Lv: 6},
		{UUID: 1, BID: 10144000, Lv: 0}, // Secret Command Post: mono_condition
		{UUID: 2, BID: 10145000, Lv: 0}, // 4th Squad: month card
		{UUID: 3, BID: 10120000, Lv: 1}, // Tavern
	}
	if got := targetUUIDs(productionUpgradeTargets(bs, nil)); !slices.Equal(got, []int64{3}) {
		t.Errorf("targets %v, want only the Tavern", got)
	}
}
