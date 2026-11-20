package game

import (
	"slices"
	"strings"
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
	hq        int32      // 0: HQ 25
	settings  *sfs.SFSObject
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func (c timerInit) build() *Init {
	hq := c.hq
	if hq == 0 {
		hq = 25
	}
	return rewardInitWithDay(hq, func(raw *sfs.SFSObject) {
		if c.settings != nil {
			raw.PutSFSObject("userSetting", c.settings)
		}
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

// The camps share the stock room with each other and with healed troops. Checking each against the
// init total alone sent a second camp after the first had filled the stock, and the server answered
// it E000000 "addNum error" (live, 2026-10-24 13:48, these prodBase values and cap).
func TestFinishedTimersCountsEachCampAgainstTheStock(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	base := timerInit{
		buildings: []timerBuilding{
			{uuid: 845, bID: 10103000, lv: 27, prodST: now.Add(-time.Hour), prodET: past, prodBase: 132},
			{uuid: 507, bID: 10103000, lv: 27, prodST: now.Add(-time.Hour), prodET: past, prodBase: 658},
		},
		queues:   []timerQueue{{uuid: 23, qType: 3, start: now.Add(-time.Hour), end: past}},
		stockCap: 44390,
	}
	for name, c := range map[string]struct {
		soldiers int64
		want     []timerSent
	}{
		"room for both camps and the heal": {43_500,
			[]timerSent{{finishedTimersCampCmd, 845}, {finishedTimersCampCmd, 507}, {finishedTimersQueueCmd, 23}}},
		"both camps reach the cap, no heal": {43_600,
			[]timerSent{{finishedTimersCampCmd, 845}, {finishedTimersCampCmd, 507}}},
		"the first camp fills the stock": {44_300, []timerSent{{finishedTimersCampCmd, 845}}},
	} {
		t.Run(name, func(t *testing.T) {
			ti := base
			ti.soldiers = c.soldiers
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

// B13: with user setting 19 ("confirm finished building") on and the HQ at building_finish_check.k1
// (16) or above, the client asks before every upgrade finish (BuildManager.lua:3089-3113), so
// finished-timers leaves upgrades to the player; repairs, queues and camp output are not behind
// that dialog. The client compares the value with the string "1".
func TestFinishedTimersLeavesUpgradesWhenConfirmSettingIsOn(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Minute)
	setting := func(v any) *sfs.SFSObject {
		o := sfs.NewSFSObject()
		o.PutUtfString("18", "1") // DIAMOND_USAGE_REMINDER: unrelated
		switch v := v.(type) {
		case string:
			o.PutUtfString("19", v)
		case int32:
			o.PutInt("19", v)
		}
		return o
	}
	base := timerInit{
		buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: past},
			{uuid: 4, bID: 10107000, lv: 5, dEndT: past},
			{uuid: 7, bID: 10101000, lv: 3, prodST: now.Add(-2 * time.Hour), prodET: past},
		},
		queues:   []timerQueue{{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: past}},
		soldiers: -1,
	}
	held := []timerSent{{finishedTimersRepairCmd, 4}, {finishedTimersCampCmd, 7}, {finishedTimersQueueCmd, 21}}
	all := append([]timerSent{{finishedTimersUpgradeCmd, 1}}, held...)
	for name, c := range map[string]struct {
		settings *sfs.SFSObject
		hq       int32
		want     []timerSent
	}{
		"on, HQ 25":                   {setting("1"), 25, held},
		"on, HQ 16":                   {setting("1"), 16, held},
		"on, HQ 15":                   {setting("1"), 15, all},
		"off":                         {setting("0"), 25, all},
		"integer 1 is not the string": {setting(int32(1)), 25, all},
		"key missing":                 {setting(nil), 25, all},
		"no userSetting":              {nil, 25, all},
	} {
		t.Run(name, func(t *testing.T) {
			ti := base
			ti.settings, ti.hq = c.settings, c.hq
			sent, err := runTimers(t, ti.build(), okTimers)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, c.want) {
				t.Errorf("sent %v\nwant %v", sent, c.want)
			}
		})
	}
}

// B72: the client overrides a building's uT with the uT of the build queue (type 0 or 110) whose
// itemObj.itemId names it (BuildQueueManager.lua:18-55), so finished-timers decides on that value.
func TestFinishedTimersUsesTheBuildQueueUpgradeTime(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	in := timerInit{
		buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: future}, // queue says done: finish
			{uuid: 2, bID: 10202000, lv: 5, uT: past},   // queue says running: leave
			{uuid: 3, bID: 10202000, lv: 5, uT: future}, // only a research queue names it: leave
			{uuid: 4, bID: 10202000, lv: 5, uT: past},   // no queue names it: finish on its own uT
		},
		queues: []timerQueue{
			{uuid: 31, qType: 0, start: now.Add(-time.Hour), end: past, itemID: "1"},
			{uuid: 32, qType: 110, start: now.Add(-time.Hour), end: future, itemID: "2"},
			{uuid: 33, qType: 6, start: now.Add(-time.Hour), end: future, itemID: "3"},
		},
		soldiers: -1,
	}
	sent, err := runTimers(t, in.build(), okTimers)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if want := []timerSent{{finishedTimersUpgradeCmd, 1}, {finishedTimersUpgradeCmd, 4}}; !slices.Equal(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
}

// heroEventDuelScores are the six duel days' scoring lists, the `score` column of heroevent rows
// 110000-110005 verbatim (tables 40413). 90202 (score type 23, BuildPowerUp) is on 110001 and
// 110004; 90302 (type 6, SciencePowerUp) is on 110002 and 110004 (EnumType.lua:15799-15809).
var heroEventDuelScores = map[int32]string{
	110000: "90401|90402|90102|90004|90005|90000|90001|90002|90003",
	110001: "90201|90202|90203|90204|90000|100910",
	110002: "90301|90302|90303|90402|90000|100921|100922|100923|100924|100925|100926|100927",
	110003: "90101|90102|90103|90104|90105|90106|90000",
	110004: "90402|90201|90202|90301|90302|90501|90502|90503|90504|90505|90506|90507|90508|90509|90510|90511|90000",
	110005: "90203|90204|90201|90301|90501|90601|90602|90603|90604|90605|90606|90607|90608|90609|90610|90629|" +
		"90611|90612|90613|90614|90615|90616|90617|90618|90619|90630|90620|90621|90622|90623|90624|90625|90626|" +
		"90627|90628|90631|90000",
}

// duelHoldInit adds the duel side to a test init: user.allianceId (unless noAlliance), the HQ in
// Buildings (the activity sweep reads it there) and activity 55000 with activityid "70000", open
// from start to end. It pins evNow to now.
func duelHoldInit(t *testing.T, in *Init, now time.Time, hq int32, noAlliance bool, start, end time.Time) *Init {
	t.Helper()
	withEvNow(t, now)
	if !noAlliance {
		user := sfs.NewSFSObject()
		user.PutUtfString("allianceId", "7f3a")
		in.Raw.PutSFSObject("user", user)
	}
	act := sfs.NewSFSObject()
	act.PutInt("id", 55000)
	act.PutUtfString("activityid", "70000")
	act.PutLong("startTime", start.UnixMilli())
	act.PutLong("endTime", end.UnixMilli())
	acts := sfs.NewSFSObject()
	acts.PutSFSObject("55000", act)
	in.Raw.PutSFSObject("activity", acts)
	in.Buildings = []Building{NewTestBuilding(9001, claimHQBuildingID, hq)}
	return in
}

// runTimersDuel runs finished-timers against a server whose hero.event.info.get {activityId:
// "70000"} reply is info (nil drops the connection); it returns the other requests and how many
// duel reads were sent.
func runTimersDuel(t *testing.T, in *Init, info *sfs.SFSObject) (sent []timerSent, duelReads int, err error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			if p.GetString("activityId") != "70000" {
				return evOK()
			}
			return info
		}
		return rewardOK()
	})
	err = runFinishedTimers(conn, in)
	for _, r := range fake.requests() {
		if r.Cmd == "hero.event.info.get" {
			duelReads++
			continue
		}
		sent = append(sent, timerSent{r.Cmd, r.Params.GetLong("uuid")})
	}
	return sent, duelReads, err
}

// duelHoldTimers is a city with one of each finished timer: upgrade 1, repair 4, smith 7, research
// queue 21 and hospital queue 23.
func duelHoldTimers(now time.Time) timerInit {
	past := now.Add(-time.Minute)
	return timerInit{
		buildings: []timerBuilding{
			{uuid: 1, bID: 10202000, lv: 5, uT: past},
			{uuid: 4, bID: 10107000, lv: 5, dEndT: past},
			{uuid: 7, bID: 10101000, lv: 3, prodST: now.Add(-2 * time.Hour), prodET: past},
		},
		queues: []timerQueue{
			{uuid: 21, qType: 6, start: now.Add(-time.Hour), end: past},
			{uuid: 23, qType: 3, start: now.Add(-time.Hour), end: past},
		},
		stockCap: 5000,
		soldiers: 1000,
	}
}

// The user's ruling (2026-11-19): a finished upgrade waits for a day that scores building power and
// a finished research for one that scores research power; repairs, the smith and the hospital are
// not held. The rest day (only yesterday's entry) scores neither.
func TestFinishedTimersHoldsCompletionsForTheirDuelDay(t *testing.T) {
	var (
		upgrade  = timerSent{finishedTimersUpgradeCmd, 1}
		repair   = timerSent{finishedTimersRepairCmd, 4}
		smith    = timerSent{finishedTimersCampCmd, 7}
		research = timerSent{finishedTimersQueueCmd, 21}
		hospital = timerSent{finishedTimersQueueCmd, 23}
	)
	neither := []timerSent{repair, smith, hospital}
	for name, c := range map[string]struct {
		theme  int32
		today  bool // false: only yesterday's entry
		want   []timerSent
		logged []string
	}{
		"Mon radar":          {110000, true, neither, []string{"Base Expansion or Total Mobilization", "Age of Science or Total Mobilization"}},
		"Tue base expansion": {110001, true, []timerSent{upgrade, repair, smith, hospital}, []string{"Age of Science or Total Mobilization"}},
		"Wed science":        {110002, true, []timerSent{repair, smith, research, hospital}, []string{"Base Expansion or Total Mobilization"}},
		"Thu heroes":         {110003, true, neither, nil},
		"Fri mobilization":   {110004, true, []timerSent{upgrade, repair, smith, research, hospital}, nil},
		"Sat enemy buster":   {110005, true, neither, nil},
		"Sun rest day":       {110005, false, neither, []string{"rest day"}},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureRewardLogs(t)
			now := time.Now()
			in := duelHoldInit(t, duelHoldTimers(now).build(), now, 25, false, now.Add(-12*time.Hour), now.Add(12*time.Hour))
			begin, end := now.Add(-time.Hour), now.Add(time.Hour)
			if !c.today {
				begin, end = now.Add(-25*time.Hour), now.Add(-time.Hour)
			}
			sent, reads, err := runTimersDuel(t, in, duelInfo(duelEntry(c.theme, heroEventDuelScores[c.theme], begin, end)))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, c.want) {
				t.Errorf("sent %v\nwant %v", sent, c.want)
			}
			if reads != 1 {
				t.Errorf("duel reads = %d, want 1", reads)
			}
			for _, s := range c.logged {
				if !strings.Contains(logs.String(), s) {
					t.Errorf("log lacks %q:\n%s", s, logs.String())
				}
			}
		})
	}
}

// With no duel to hold for, finished-timers finishes as before, and it reads the duel only when an
// alliance member has a finished upgrade or research.
func TestFinishedTimersFinishesWithoutADuel(t *testing.T) {
	all := []timerSent{{finishedTimersUpgradeCmd, 1}, {finishedTimersRepairCmd, 4}, {finishedTimersCampCmd, 7},
		{finishedTimersQueueCmd, 21}, {finishedTimersQueueCmd, 23}}
	monday := func(now time.Time) *sfs.SFSObject {
		return duelInfo(duelEntry(110000, heroEventDuelScores[110000], now.Add(-time.Hour), now.Add(time.Hour)))
	}
	for name, c := range map[string]struct {
		hq         int32
		noAlliance bool
		closed     bool
		info       func(now time.Time) *sfs.SFSObject
		reads      int
	}{
		"not in an alliance":                           {25, true, false, monday, 0},
		"duel activity ended":                          {25, false, true, monday, 0},
		"HQ below the activity's needMainCityLevel 10": {9, false, false, monday, 0},
		"no duel entry at all":                         {25, false, false, func(time.Time) *sfs.SFSObject { return duelInfo() }, 1},
	} {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			ti := duelHoldTimers(now)
			ti.hq = c.hq
			start, end := now.Add(-12*time.Hour), now.Add(12*time.Hour)
			if c.closed {
				start, end = now.Add(-36*time.Hour), now.Add(-12*time.Hour)
			}
			in := duelHoldInit(t, ti.build(), now, c.hq, c.noAlliance, start, end)
			sent, reads, err := runTimersDuel(t, in, c.info(now))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, all) {
				t.Errorf("sent %v\nwant %v", sent, all)
			}
			if reads != c.reads {
				t.Errorf("duel reads = %d, want %d", reads, c.reads)
			}
		})
	}

	// Nothing finished that the duel scores: no read at all, even on a non-scoring day.
	now := time.Now()
	ti := duelHoldTimers(now)
	ti.buildings[0].uT = now.Add(time.Hour)
	ti.queues = ti.queues[1:]
	in := duelHoldInit(t, ti.build(), now, 25, false, now.Add(-12*time.Hour), now.Add(12*time.Hour))
	sent, reads, err := runTimersDuel(t, in, monday(now))
	want := []timerSent{{finishedTimersRepairCmd, 4}, {finishedTimersCampCmd, 7}, {finishedTimersQueueCmd, 23}}
	if err != nil || reads != 0 || !slices.Equal(sent, want) {
		t.Errorf("nothing to hold: err %v, duel reads %d, sent %v, want %v and no read", err, reads, sent, want)
	}
}

// A duel that can't be read never stalls the queues: finished-timers finishes everything with a Warn.
func TestFinishedTimersDuelHoldFailsOpen(t *testing.T) {
	all := []timerSent{{finishedTimersUpgradeCmd, 1}, {finishedTimersRepairCmd, 4}, {finishedTimersCampCmd, 7},
		{finishedTimersQueueCmd, 21}, {finishedTimersQueueCmd, 23}}
	noWindow := func(time.Time) *sfs.SFSObject {
		e := sfs.NewSFSObject() // a type-15 entry with neither st/et nor begintime/endtime
		e.PutInt("t", 15)
		e.PutInt("eventId", 110000)
		e.PutUtfString("score", heroEventDuelScores[110000])
		return duelInfo(e)
	}
	for name, info := range map[string]func(now time.Time) *sfs.SFSObject{
		"error reply":        func(time.Time) *sfs.SFSObject { return rewardErr("E000001") },
		"success-only reply": func(time.Time) *sfs.SFSObject { return evOK() },
		"no eventList": func(time.Time) *sfs.SFSObject {
			r := sfs.NewSFSObject()
			r.PutUtfString("activityId", "70000")
			return r
		},
		"entries without a day window": noWindow,
		"score ids the table lacks": func(now time.Time) *sfs.SFSObject {
			return duelInfo(duelEntry(110000, "1234567|7654321", now.Add(-time.Hour), now.Add(time.Hour)))
		},
		"empty score list": func(now time.Time) *sfs.SFSObject {
			return duelInfo(duelEntry(110000, "", now.Add(-time.Hour), now.Add(time.Hour)))
		},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureRewardLogs(t)
			now := time.Now()
			in := duelHoldInit(t, duelHoldTimers(now).build(), now, 25, false, now.Add(-12*time.Hour), now.Add(12*time.Hour))
			sent, reads, err := runTimersDuel(t, in, info(now))
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, all) {
				t.Errorf("sent %v\nwant %v", sent, all)
			}
			if reads != 1 {
				t.Errorf("duel reads = %d, want 1", reads)
			}
			if !strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("no Warn logged:\n%s", logs.String())
			}
		})
	}
}
