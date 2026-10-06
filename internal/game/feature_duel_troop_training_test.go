package game

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// duelCampSFS is a Military Camp init building_new entry; busy adds a running batch.
func duelCampSFS(uuid int64, lv int32, busy bool) *sfs.SFSObject {
	b := duelObj("uuid", uuid, "bId", int(finishedTimersMilitaryCamp), "lv", int(lv))
	if busy {
		b.PutLong("prodST", evTestNow.Add(-time.Hour).UnixMilli())
		b.PutLong("prodET", evTestNow.Add(time.Hour).UnixMilli())
	}
	return b
}

// duelTrainInit is an init with the duel activity, user.gold 1000, iron and food, the camps, the
// T10/T11 science when science is set, and the given effects.
func duelTrainInit(iron, food int64, science bool, effects map[string]float64, camps ...*sfs.SFSObject) *Init {
	in := evInit(30, duelActivity())
	in.Raw.PutSFSObject("user", duelObj("gold", int64(1000)))
	in.Raw.PutSFSObject("resource", duelObj("metal", iron, "money", food, "oil", int64(5)))
	arr := sfs.NewSFSArray()
	for _, c := range camps {
		arr.AddSFSObject(c)
	}
	in.Raw.PutSFSArray("building_new", arr)
	sci := sfs.NewSFSArray()
	sci.AddSFSObject(duelObj("uuid", int64(1), "itemId", 120010100, "level", 3))
	if science {
		sci.AddSFSObject(duelObj("uuid", int64(2), "itemId", 120011100, "level", 1))
	}
	in.Raw.PutSFSArray("science_new", sci)
	eff := sfs.NewSFSObject()
	for k, v := range effects {
		eff.PutDouble(k, v)
	}
	in.Raw.PutSFSObject("effect", eff)
	return in
}

func duelTrainServer(gold int64) func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
	return func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch cmd {
		case duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(gold)}
		case duelTrainCmd:
			// The live reply shape: the batch echoed, plus the building.
			return []duelSpendMsg{duelReply(duelObj("sLevel", int(p.GetInt("sLevel")), "sNum", int(p.GetInt("sNum")),
				"buildInfo", duelObj("uuid", p.GetLong("uuid"))))}
		}
		return nil
	}
}

// trainingsOf summarises the trainings sent as "uuid:Ttier:count", and checks that every one
// carries exactly uuid, type 0, sLevel and sNum: never itemId, goldForTime or goldForResource.
func trainingsOf(t *testing.T, f *duelSpendFake) []string {
	t.Helper()
	var out []string
	for _, r := range f.only(duelTrainCmd) {
		keys := slices.Clone(r.Params.Keys())
		keys = slices.DeleteFunc(keys, func(k string) bool { return k == "_id" })
		slices.Sort(keys)
		if !slices.Equal(keys, []string{"sLevel", "sNum", "type", "uuid"}) || r.Params.GetInt("type") != 0 {
			t.Errorf("training params %v: want exactly uuid, type 0, sLevel, sNum", r.Params)
		}
		for _, k := range []string{"itemId", "goldForTime", "goldForResource", "gold", "useGold"} {
			if r.Params.Has(k) {
				t.Errorf("training sent %s: %v", k, r.Params)
			}
		}
		out = append(out, fmt.Sprintf("%d:T%d:%d", r.Params.GetLong("uuid"), r.Params.GetInt("sLevel"), r.Params.GetInt("sNum")))
	}
	for _, r := range f.requests() {
		if r.Cmd == "army.add" || r.Cmd == "soldier.up" || r.Cmd == "building.camp.accel" {
			t.Errorf("sent %s", r.Cmd)
		}
	}
	return out
}

func TestDuelTroopTrainingRegistration(t *testing.T) {
	f := featureRegistry["duel-troop-training"]
	if f.DefaultOn || !f.Hold || !strings.HasPrefix(f.Summary, "OPT-IN") {
		t.Errorf("duel-troop-training must be opt-in and duel-held: %+v", f)
	}
	want := []DuelScore{{Type: DuelScoreTrainUnit}, {Type: DuelScoreSpeedUp, Value: DuelQueueTraining}}
	if !slices.Equal(f.Duel, want) {
		t.Errorf("Duel = %v, want %v", f.Duel, want)
	}
	for _, c := range []struct {
		day  *sfs.SFSObject
		held bool
	}{
		{duelTestDay(DuelThemeMobilization, duelTestMobilize), false},
		{duelTestDay(DuelThemeEnemyBuster, duelTestEnemyBuster), false},
		{duelTestDay(DuelThemeBase, duelTestBase), true},
		{duelTestDay(DuelThemeHeroes, duelTestHeroes), true},
	} {
		conn, in := duelFake(t, c.day)
		if held, why := duelHeld(conn, in, f, nil); held != c.held {
			t.Errorf("theme %v: held = %v (%s), want %v", c.day.GetInt("eventId"), held, why, c.held)
		}
	}
}

func TestDuelTroopTrainingIdleCampsHighestTier(t *testing.T) {
	upgrading := duelCampSFS(4, 30, false)
	upgrading.PutLong("uT", evTestNow.Add(time.Hour).UnixMilli())
	finished := duelCampSFS(5, 30, false)
	finished.PutLong("prodST", evTestNow.Add(-2*time.Hour).UnixMilli())
	finished.PutLong("prodET", evTestNow.Add(-time.Hour).UnixMilli())
	in := duelTrainInit(100_000_000, 100_000_000, true, nil,
		duelCampSFS(2, 10, false), duelCampSFS(1, 30, false), duelCampSFS(3, 30, true), upgrading, finished,
		duelObj("uuid", int64(6), "bId", 10101000, "lv", 30)) // a Smith Shop
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	// Idle camps only, by uuid: level 30 with the science trains T10 at para1 570; level 10 trains
	// T4 at 370.
	want := []string{"1:T10:570", "2:T4:370"}
	if got := trainingsOf(t, f); !slices.Equal(got, want) {
		t.Errorf("trainings %v, want %v", got, want)
	}
	if got := len(f.only(duelGoldProbeCmd)); got != 3 {
		t.Errorf("sent %d gold checks, want one before and one after each training", got)
	}
}

func TestDuelTopUnit(t *testing.T) {
	for _, c := range []struct {
		lv   int64
		u    duelTrainUnlocks
		want int32
	}{
		{1, duelTrainUnlocks{}, 1},
		{2, duelTrainUnlocks{}, 1},
		{29, duelTrainUnlocks{science: true}, 9},
		{30, duelTrainUnlocks{}, 9}, // T10 needs the science
		{30, duelTrainUnlocks{science: true}, 10},
		{35, duelTrainUnlocks{science: true, t11Buff: true, season: 5}, 11},
		{35, duelTrainUnlocks{science: true, t11Buff: true, season: 4}, 10}, // season day unread
		{35, duelTrainUnlocks{science: true, season: 6}, 10},                // no UNLOCK_T11
		{34, duelTrainUnlocks{science: true, t11Buff: true, season: 6}, 10},
	} {
		got, ok := duelTopUnit(c.lv, c.u)
		if !ok || got.tier != c.want {
			t.Errorf("lv %d %+v: tier %d, want %d", c.lv, c.u, got.tier, c.want)
		}
	}
	if _, ok := duelTopUnit(0, duelTrainUnlocks{}); ok {
		t.Error("a level-0 camp trains nothing")
	}
}

func TestDuelTroopTrainingT11FromInit(t *testing.T) {
	in := duelTrainInit(100_000_000, 100_000_000, true, map[string]float64{duelTrainT11Buff: 1}, duelCampSFS(1, 35, false))
	in.Raw.PutSFSObject("playerServerSeasonInfo", duelObj("seasonId", 5))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T11:600"}) {
		t.Errorf("trainings %v, want T11 x600", got)
	}
}

func TestDuelTroopTrainingNeverTrainsWhenUnaffordable(t *testing.T) {
	// 7026 iron can't pay one T10 (7027): nothing is sent, the camp stays idle.
	in := duelTrainInit(7026, 100_000_000, true, nil, duelCampSFS(1, 30, false))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); len(got) != 0 {
		t.Errorf("trained %v with too little iron", got)
	}

	// Food binds: 100 units' worth trains exactly 100, and the second camp gets what is left.
	in = duelTrainInit(100_000_000, 7027*130+7026, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T10:130"}) {
		t.Errorf("trainings %v, want 130 in the first camp and none in the second", got)
	}

	// Two camps share the ledger: 900 units of T10 iron, 570 then 330.
	in = duelTrainInit(7027*900, 7027*900, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T10:570", "2:T10:330"}) {
		t.Errorf("trainings %v, want 570 then 330", got)
	}

	// No resource object: affordability is unknown, so nothing trains.
	in = duelTrainInit(1, 1, true, nil, duelCampSFS(1, 30, false))
	in.Raw.PutSFSObject("resource", duelObj("oil", int64(5)))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := f.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
		t.Errorf("without food/iron it sent %v", got)
	}
}

func TestDuelTroopTrainingEffects(t *testing.T) {
	// +20% capacity: floor(570 * 1.2) = 684. A 10% cost cut: 100 units cost 632,430 of each, so
	// that much trains exactly 100.
	in := duelTrainInit(100_000_000, 100_000_000, true, map[string]float64{duelTrainLimitRate: 0.2}, duelCampSFS(1, 30, false))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T10:684"}) {
		t.Errorf("trainings %v, want 684", got)
	}
	in = duelTrainInit(632430, 632430, true, map[string]float64{duelTrainCostRate: 0.1}, duelCampSFS(1, 30, false))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), duelTrainServer(1000))
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T10:100"}) {
		t.Errorf("trainings %v, want 100", got)
	}
	if c := duelTrainCost(100, 7027, duelTrainBasisPoint-1000); c != 632430 {
		t.Errorf("cost of 100 = %d, want 632430", c)
	}
	if c := duelTrainCost(3, 97, duelTrainBasisPoint-1234); c != 256 { // ceil(3*97*0.8766 = 255.09)
		t.Errorf("cost rounds up: %d, want 256", c)
	}
}

func TestDuelTroopTrainingThemeGate(t *testing.T) {
	for name, c := range map[string]struct {
		day   *sfs.SFSObject
		train bool
	}{
		"total mobilization":         {duelTestDay(DuelThemeMobilization, duelTestMobilize), true},
		"enemy buster":               {duelTestDay(DuelThemeEnemyBuster, duelTestEnemyBuster), true},
		"enemy buster, no train ids": {duelTestDay(DuelThemeEnemyBuster, "90203|90602|90000"), false},
		"base expansion":             {duelTestDay(DuelThemeBase, duelTestBase), false},
		"train heroes":               {duelTestDay(DuelThemeHeroes, duelTestHeroes), false},
		// A season variant could make another theme score training: still held.
		"radar with training": {duelTestDay(DuelThemeRadar, "90402|90502|90501"), false},
		"no duel day":         {nil, false},
	} {
		in := duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false))
		conn, f := startDuelSpendFake(t, c.day, duelTrainServer(1000))
		if err := runDuelTroopTraining(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := len(trainingsOf(t, f)); (got == 1) != c.train || got > 1 {
			t.Errorf("%s: %d trainings, want train=%v", name, got, c.train)
		}
		if !c.train && !slices.Equal(f.cmds(), []string{"hero.event.info.get"}) {
			t.Errorf("%s: sent %v on a held day", name, f.cmds())
		}
	}
}

func TestDuelTroopTrainingStopsOnInstantReplyOrTrip(t *testing.T) {
	// A reply that echoes an instant-finish field stops the feature.
	in := duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelTrainCmd {
			return []duelSpendMsg{duelReply(duelObj("goldForTime", 0, "sNum", int(p.GetInt("sNum"))))}
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	if err := runDuelTroopTraining(conn, in); err == nil || !strings.Contains(err.Error(), "goldForTime") {
		t.Errorf("err = %v, want the instant-finish stop", err)
	}
	if got := len(f.only(duelTrainCmd)); got != 1 {
		t.Errorf("%d trainings sent, want 1", got)
	}

	// A balance drop after the first camp halts the session.
	in = duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelTrainCmd {
			return []duelSpendMsg{duelReply(evOK()), duelPush(duelPushResourceInfo, duelObj("gold", int64(500)))}
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	if err := runDuelTroopTraining(conn, in); !errors.Is(err, errDuelSpendHalted) {
		t.Errorf("err = %v, want the tripwire", err)
	}
	if got := len(f.only(duelTrainCmd)); got != 1 {
		t.Errorf("%d trainings sent, want none after the drop", got)
	}

	// An errorCode stops the remaining camps.
	in = duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelTrainCmd {
			return []duelSpendMsg{duelReply(evErr("E100100"))}
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	if err := runDuelTroopTraining(conn, in); err == nil || !strings.Contains(err.Error(), "E100100") {
		t.Errorf("err = %v, want the errorCode", err)
	}
	if got := len(f.only(duelTrainCmd)); got != 1 {
		t.Errorf("%d trainings sent after an errorCode, want 1", got)
	}
}

func TestDuelTroopTrainingPushedResourcesLowerTheLedger(t *testing.T) {
	// Init says plenty, but the server pushes the real food after the first batch: the second camp
	// sizes to it.
	in := duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false), duelCampSFS(2, 30, false))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeMobilization, duelTestMobilize), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelTrainCmd {
			return []duelSpendMsg{duelReply(evOK()),
				duelPush(duelPushResourceInfo, duelObj("resources", duelObj("metal", int64(100_000_000), "money", int64(7027*7)), "gold", int64(1000)))}
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	if err := runDuelTroopTraining(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := trainingsOf(t, f); !slices.Equal(got, []string{"1:T10:570", "2:T10:7"}) {
		t.Errorf("trainings %v, want 570 then 7", got)
	}
}
