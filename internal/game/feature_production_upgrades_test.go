package game

import (
	"slices"
	"strconv"
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
		{uuid: 1, bID: int(BuildingFarmland), lv: 3},
		{uuid: 2, bID: int(BuildingIronMine), lv: 1},
		{uuid: 3, bID: int(BuildingGoldMine), lv: 5},                       // at the HQ level
		{uuid: 4, bID: int(BuildingTrainingBase), lv: 2, uT: time.Hour},    // upgrading
		{uuid: 5, bID: int(BuildingTrainingBase), lv: 2, uT: -time.Minute}, // ended, not collected yet
		{uuid: 6, bID: 10103000, lv: 1},                                    // Military Camp: not a target
		{uuid: 7, bID: int(BuildingIronMine), lv: 0},                       // not built
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
	if s.HQ != 5 || s.Busy != 2 {
		t.Errorf("hq %d busy %d, want 5 and 2", s.HQ, s.Busy)
	}
	var got []int64
	for _, tg := range s.Targets {
		got = append(got, tg.UUID)
	}
	if !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("targets %v, want [2 1] (cheapest level first)", got)
	}
	if !slices.Equal(s.Queues, []int64{11, 13, 16}) {
		t.Errorf("queues %v, want [11 13 16]", s.Queues)
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

	in = upgradeInit(5, 1_000_000, 1_000_000, 0, lines, queues)
	conn, f = startDuelSpendFake(t, nil, func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == productionUpgradeCmd {
			return []duelSpendMsg{duelReply(duelObj("errorCode", "E100"))}
		}
		return upgradeServer(1000)(cmd, p)
	})
	if err := runProductionUpgrades(conn, in, false); err == nil {
		t.Fatal("want the errorCode error")
	}
	if n := len(upgradesOf(t, f)); n != 1 {
		t.Errorf("%d upgrades sent, want 1 before stopping", n)
	}
}

func TestProductionUpgradeCostsTable(t *testing.T) {
	for _, bID := range []int32{BuildingFarmland, BuildingIronMine, BuildingGoldMine, BuildingTrainingBase} {
		levels := productionUpgradeCosts[bID]
		if len(levels) != productionUpgradeMaxLevel-1 {
			t.Errorf("%s: %d levels, want %d", BuildingNameOf(bID), len(levels), productionUpgradeMaxLevel-1)
		}
		for lv := int64(1); lv < productionUpgradeMaxLevel; lv++ {
			if c := levels[lv]; c.Metal <= 0 || c.Seconds <= 0 {
				t.Errorf("%s lv%d: %+v", BuildingNameOf(bID), lv, c)
			}
		}
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

func TestProductionUpgradesKeepsHQ8WhileShielded(t *testing.T) {
	in := upgradeInit(7, 0, 0, 0, nil, nil)
	in.Objects("building_new")[0].PutLong("uT", evTestNow.Add(-time.Minute).UnixMilli())
	in.Raw.PutSFSObject("defend_wall", duelObj("protectEndTime", evTestNow.Add(time.Hour).UnixMilli()))
	withUpgradeChain(t, 0)
	conn, f := startDuelSpendFake(t, nil, upgradeServer(1000))
	if err := runProductionUpgrades(conn, in, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := len(f.only(finishedTimersUpgradeCmd)); n != 0 {
		t.Errorf("opened the HQ 8 upgrade under the shield")
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
