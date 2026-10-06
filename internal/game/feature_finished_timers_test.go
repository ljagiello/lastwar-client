package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// timerBuilding is one building_new entry; zero times are omitted.
type timerBuilding struct {
	uuid           int64
	bID, lv        int32
	uT, dEndT      time.Time
	prodST, prodET time.Time
	prodBase       int32
	help           int32
}

type timerQueue struct {
	uuid, qType int64
	start, end  time.Time
	isHelped    int32
	itemID      string
}

type timerInit struct {
	buildings []timerBuilding
	queues    []timerQueue
	shieldEnd *time.Time // nil: no defend_wall
	stockCap  float64    // 0: no effect["30122"]
	soldiers  int64      // < 0: no resource_items
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (c timerInit) build() *Init {
	return rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		v, _ := raw.Get("building_new")
		builds := v.Val.(*sfs.SFSArray)
		for _, b := range c.buildings {
			o := sfs.NewSFSObject()
			o.PutLong("uuid", b.uuid)
			o.PutInt("bId", b.bID)
			o.PutInt("lv", b.lv)
			o.PutLong("uT", ms(b.uT))
			if !b.dEndT.IsZero() {
				o.PutLong("dEndT", ms(b.dEndT))
			}
			if !b.prodST.IsZero() {
				o.PutLong("prodST", ms(b.prodST))
				o.PutLong("prodET", ms(b.prodET))
				o.PutInt("prodBase", b.prodBase)
			}
			o.PutInt("help", b.help)
			builds.AddSFSObject(o)
		}
		qs := sfs.NewSFSArray()
		for _, q := range c.queues {
			o := sfs.NewSFSObject()
			o.PutLong("uuid", q.uuid)
			o.PutInt("type", int32(q.qType))
			o.PutLong("sT", ms(q.start))
			o.PutLong("uT", ms(q.end))
			o.PutInt("isHelped", q.isHelped)
			if q.itemID != "" {
				item := sfs.NewSFSObject()
				item.PutUtfString("itemId", q.itemID)
				o.PutSFSObject("itemObj", item)
			}
			qs.AddSFSObject(o)
		}
		raw.PutSFSArray("queue_new", qs)
		if c.shieldEnd != nil {
			dw := sfs.NewSFSObject()
			dw.PutLong("protectEndTime", c.shieldEnd.UnixMilli())
			raw.PutSFSObject("defend_wall", dw)
		}
		if c.stockCap > 0 {
			eff := sfs.NewSFSObject()
			eff.PutDouble("30122", c.stockCap)
			raw.PutSFSObject("effect", eff)
		}
		if c.soldiers >= 0 {
			items := sfs.NewSFSArray()
			s := sfs.NewSFSObject()
			s.PutInt("itemId", 3010)
			s.PutLong("number", c.soldiers)
			items.AddSFSObject(s)
			other := sfs.NewSFSObject() // not a soldier item
			other.PutInt("itemId", 630011)
			other.PutLong("number", 1_000_000)
			items.AddSFSObject(other)
			raw.PutSFSArray("resource_items", items)
		}
	})
}

type timerSent struct {
	cmd  string
	uuid int64
}

func runTimers(t *testing.T, in *Init, reply func(cmd string, uuid int64) *sfs.SFSObject) ([]timerSent, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return reply(cmd, p.GetLong("uuid")) })
	err := runFinishedTimers(conn, in)
	var out []timerSent
	for _, r := range fake.requests() {
		out = append(out, timerSent{r.Cmd, r.Params.GetLong("uuid")})
	}
	return out, err
}

func okTimers(string, int64) *sfs.SFSObject { return rewardOK() }

func TestFinishedTimersCollectsOnlyEndedTimers(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	shieldDown := now.Add(-time.Hour)
	in := timerInit{
		buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: past},                                                     // finished upgrade
			{uuid: 2, bID: 10202000, lv: 5, uT: future},                                                   // still upgrading
			{uuid: 3, bID: 10100000, lv: 7, uT: past},                                                     // HQ 7, shield down: finish
			{uuid: 4, bID: 10107000, lv: 5, dEndT: past},                                                  // finished repair
			{uuid: 5, bID: 10107000, lv: 5, dEndT: future},                                                // repairing
			{uuid: 6, bID: 792000, lv: 0, uT: past},                                                       // wormhole sub at lv 0: skip
			{uuid: 7, bID: 10101000, lv: 3, prodST: now.Add(-2 * time.Hour), prodET: past},                // smith done
			{uuid: 8, bID: 10103000, lv: 3, prodST: now.Add(-2 * time.Hour), prodET: past, prodBase: 100}, // camp done
			{uuid: 9, bID: 10103000, lv: 3, prodST: now.Add(-time.Hour), prodET: future},                  // camp training
			{uuid: 10, bID: 10210000, lv: 3, prodST: now.Add(-2 * time.Hour), prodET: past},               // production line: never camp.collect
		},
		queues: []timerQueue{
			{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: past},   // research done
			{uuid: 22, qType: 6, start: now.Add(-time.Hour), end: future}, // research running
			{uuid: 23, qType: 3, start: now.Add(-time.Hour), end: past},   // healing done
			{uuid: 24, qType: 117, start: past, end: past},                // rebirth done (sT == uT)
			{uuid: 25, qType: 39, start: now.Add(-time.Hour), end: past},  // pasture: batch command, skip
			{uuid: 26, qType: 116, start: now.Add(-time.Hour), end: past}, // dragon hospital: skip
		},
		shieldEnd: &shieldDown,
		stockCap:  5000.7,
		soldiers:  1000,
	}
	sent, err := runTimers(t, in.build(), okTimers)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []timerSent{
		{finishedTimersUpgradeCmd, 1}, {finishedTimersUpgradeCmd, 3}, {finishedTimersRepairCmd, 4},
		{finishedTimersCampCmd, 7}, {finishedTimersCampCmd, 8},
		{finishedTimersQueueCmd, 21}, {finishedTimersQueueCmd, 23}, {finishedTimersQueueCmd, 24},
	}
	if !slices.Equal(sent, want) {
		t.Errorf("sent %v\nwant %v", sent, want)
	}
}

func TestFinishedTimersHQShieldAndStockCap(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	shieldUp := now.Add(24 * time.Hour)
	base := timerInit{
		buildings: []timerBuilding{
			{uuid: 3, bID: 10100000, lv: 7, uT: past},
			{uuid: 8, bID: 10103000, lv: 3, prodST: now.Add(-time.Hour), prodET: past, prodBase: 100},
		},
		queues: []timerQueue{{uuid: 23, qType: 3, start: now.Add(-time.Hour), end: past}},
	}
	for name, c := range map[string]struct {
		mod  func(*timerInit)
		want []timerSent
	}{
		"shield up, stock full":         {func(c *timerInit) { c.shieldEnd, c.stockCap, c.soldiers = &shieldUp, 1000, 1000 }, nil},
		"no defend_wall, stock unknown": {func(c *timerInit) { c.soldiers = -1 }, nil},
		"camp output above the cap": {func(c *timerInit) { c.shieldEnd, c.stockCap, c.soldiers = &shieldUp, 99, 0 },
			[]timerSent{{finishedTimersQueueCmd, 23}}},
	} {
		t.Run(name, func(t *testing.T) {
			ti := base
			c.mod(&ti)
			sent, err := runTimers(t, ti.build(), okTimers)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, c.want) {
				t.Errorf("sent %v, want %v", sent, c.want)
			}
		})
	}
}

func TestFinishedTimersChipFactoryFallsBackToChipCollect(t *testing.T) {
	now := time.Now()
	in := timerInit{buildings: []timerBuilding{
		{uuid: 31, bID: 10232000, lv: 2, prodST: now.Add(-time.Hour), prodET: now.Add(-time.Minute)},
	}, soldiers: -1}
	sent, err := runTimers(t, in.build(), func(cmd string, _ int64) *sfs.SFSObject {
		if cmd == finishedTimersCampCmd {
			return rewardErr("E000001")
		}
		return rewardOK()
	})
	if err != nil {
		t.Fatalf("run: %v (the chip.collect fallback succeeded)", err)
	}
	if want := []timerSent{{finishedTimersCampCmd, 31}, {finishedTimersChipCmd, 0}}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
}

func TestFinishedTimersNothingDueBenignAndFailure(t *testing.T) {
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runFinishedTimers(conn, &Init{}); err != nil || len(fake.cmds()) != 0 {
		t.Errorf("no init: err %v, sent %v", err, fake.cmds())
	}
	now := time.Now()
	idle := timerInit{buildings: []timerBuilding{{uuid: 1, bID: 10202000, lv: 5, uT: now.Add(time.Hour)}}}
	if sent, err := runTimers(t, idle.build(), okTimers); err != nil || len(sent) != 0 {
		t.Errorf("nothing ended: err %v, sent %v", err, sent)
	}

	two := timerInit{buildings: []timerBuilding{
		{uuid: 1, bID: 10202000, lv: 5, uT: now.Add(-time.Minute)},
		{uuid: 2, bID: 10202000, lv: 5, uT: now.Add(-time.Minute)},
		{uuid: 3, bID: 10202000, lv: 5, uT: now.Add(-time.Minute)},
	}}
	sent, err := runTimers(t, two.build(), func(_ string, uuid int64) *sfs.SFSObject {
		switch uuid {
		case 1:
			return rewardErr("120289")
		case 2:
			return rewardErr("999999")
		}
		return nil // drop the connection
	})
	if err == nil {
		t.Fatal("run = nil, want the failure and the dead connection")
	}
	if len(sent) != 3 {
		t.Errorf("sent %v, want all three tried", sent)
	}
}
