package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

// Tests for the edges of the owner's plunder rules and the stop-on-trouble policy.

// rulesKOF is plTestInit with the KOF train command on and the given attack templates (nil keeps
// the defaults).
func rulesKOF(robs int32, attack ...*sfs.SFSObject) *Init {
	return plTestInit(func(raw *sfs.SFSObject) {
		o := sfs.NewSFSObject()
		o.PutBool("isFunctionOn", true)
		raw.PutSFSObject("allianceTrainStartInfo", o)
		if attack != nil {
			ft := sfs.NewSFSArray()
			for _, t := range attack {
				ft.AddSFSObject(t)
			}
			raw.PutSFSArray("formation_template", ft)
		}
	}, 0, robs)
}

func TestRailwayTrainWithAnUnknownTeamIsNotAttacked(t *testing.T) {
	weak := func(uuid int64, edit func(f *sfs.SFSObject)) *sfs.SFSObject {
		o := plTestTarget(uuid, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B2")
		edit(evObject(evObject(o, "marchInfo"), "formation"))
		return o
	}
	noPower := sfs.NewSFSObject()
	zero := sfs.NewSFSObject()
	zero.PutDouble("power", 0)
	trains := []*sfs.SFSObject{
		weak(41, func(f *sfs.SFSObject) { f.PutSFSObject("armyUnit2", noPower) }),
		weak(42, func(f *sfs.SFSObject) { f.PutSFSObject("armyUnit2", zero) }),
		weak(43, func(f *sfs.SFSObject) {
			n := sfs.NewSFSObject()
			for _, k := range []string{"armyUnit1", "armyUnit3"} {
				v, _ := f.Get(k)
				n.PutValue(k, v)
			}
			*f = *n
		}),
	}
	if got := plAttacked(plOK(t, rulesKOF(0), plTestFake{trains: trains})); len(got) != 0 {
		t.Errorf("attacked %v: a train with a team of unknown power must count as unknown, not weaker", got)
	}
}

func TestRailwayPlunderTruckLoopDailyCap(t *testing.T) {
	trucks := []*sfs.SFSObject{
		plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(5, trainTypeTruck, 131, plTestOther, 1_000_000, 0, ""),
	}
	if got := plAttacked(plOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 3), plTestFake{trucks: trucks})); len(got) != 1 {
		t.Errorf("attacked %v with 3 of 4 plunders used; want exactly one", got)
	}
}

func TestRailwayPlunderAwayFromHomeSendsNothing(t *testing.T) {
	// The source server (900) differs from the login server and the switch is off: the cross-server
	// gate holds the run before anything is sent, though the target (640) is on neither.
	in := plTestInit(func(raw *sfs.SFSObject) { evObject(raw, "user").PutInt("crossFightSrcServerId", 900) }, 0, 0)
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")}
	if got := plOK(t, in, plTestFake{trucks: trucks}).cmds(); len(got) != 0 {
		t.Errorf("sent %v from another warzone; want nothing", got)
	}
}

func TestRailwayPlunderReasonRejectsBadServerIDs(t *testing.T) {
	homes := map[int64]bool{plTestHome: true}
	matched := map[int64]bool{0: true, 1 << 31: true, plTestOther: true}
	now := int64(1_000)
	base := railwayTarget{typ: trainTypeTruck, cfgID: 130, ownerID: "9", sendTime: 1, arriveTime: 2_000, power: 1}
	for _, id := range []int64{0, -5, 1 << 31, plTestHome} {
		tg := base
		tg.serverID = id
		if got := railwayPlunderReason(tg, homes, matched, "77", "A1", 10, now, 3); got != "own or unknown server" {
			t.Errorf("serverId %d: reason %q, want own or unknown server", id, got)
		}
	}
	tg := base
	tg.serverID = plTestOther
	if got := railwayPlunderReason(tg, homes, matched, "77", "A1", 10, now, 3); got != "" {
		t.Errorf("a valid target was rejected: %q", got)
	}
}

func TestRailwayHeroCountedOnce(t *testing.T) {
	// A template listing the same hero twice keeps it once, in its last slot.
	tpl := plTestSquad(9, 1, 1000, 2, 1)
	hs := claimObjects(tpl, "heroes")
	hs[1].PutLong("heroUuid", 1000)
	arr := sfs.NewSFSArray()
	for _, h := range hs {
		arr.AddSFSObject(h)
	}
	tpl.PutSFSArray("heroes", arr)
	in := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(tpl)
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0)
	sq := railwaySquads(in, railwayAttackAction)[0]
	if len(sq.heroes) != 1 || sq.heroes[0].uuid != 1000 || sq.heroes[0].index != 2 {
		t.Fatalf("heroes %+v, want hero 1000 once, in slot 2", sq.heroes)
	}
	if got := railwayPower(in, sq, map[int64]railwayHeroStat{1000: {power: 2_000_000}}, true); got != 2_000_000 {
		t.Errorf("power %d, want 2000000 (the hero once)", got)
	}

	// A lineup putting one hero in two teams is refused; with no fallback, no train is attacked.
	two := rulesKOF(0, plTestSquad(9, 1, 1000, 5, 1), plTestSquad(10, 2, 1100, 5, 1))
	lineup := plLineup([]int64{1000, 1001, 1002, 1003, 1004}, []int64{1000, 1100}, []int64{1200})
	trains := []*sfs.SFSObject{plTestTarget(25, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B2")}
	if got := plAttacked(plOK(t, two, plTestFake{trains: trains, lineup: lineup})); len(got) != 0 {
		t.Errorf("attacked %v with a lineup that counts hero 1000 twice", got)
	}
}

func TestRailwayTrainTeamsNeedDistinctBuffs(t *testing.T) {
	trains := []*sfs.SFSObject{plTestTarget(25, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B2")}
	// Fallback: the three attack squads share team buff 1, so the client would refuse (500262).
	dup := rulesKOF(0, plTestSquad(9, 1, 1000, 5, 1), plTestSquad(10, 1, 1100, 5, 1), plTestSquad(11, 2, 1200, 5, 1))
	if got := plAttacked(plOK(t, dup, plTestFake{trains: trains})); len(got) != 0 {
		t.Errorf("attacked %v with fallback teams sharing a team buff", got)
	}
	// Lineup: the same within the server's lineup.
	lineup := plLineup([]int64{1000, 1001, 1002, 1003, 1004}, []int64{1100, 1101, 1102, 1103, 1104}, []int64{1200})
	evObject(evObject(lineup, "formation"), "armyUnit2").PutInt("squadNo", 1)
	two := rulesKOF(0, plTestSquad(9, 1, 1000, 5, 1), plTestSquad(10, 2, 1100, 5, 1))
	if got := plAttacked(plOK(t, two, plTestFake{trains: trains, lineup: lineup})); len(got) != 0 {
		t.Errorf("attacked %v with a lineup sharing a team buff", got)
	}
}

func TestRailwayPlunderTrainRefusals(t *testing.T) {
	trains := []*sfs.SFSObject{
		plTestTarget(31, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B2"),
		plTestTarget(32, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B3"),
	}
	first := func(code string) func(n int) *sfs.SFSObject {
		return func(n int) *sfs.SFSObject {
			if n == 1 {
				return rewardErr(code)
			}
			return nil
		}
	}
	f, err := plRun(t, rulesKOF(0), plTestFake{stats: plTestStats(), trains: trains, attack: first("E400999")})
	if err == nil || len(plAttacked(f)) != 1 {
		t.Errorf("run = %v, attacked %v; a refused train attack must stop the run", err, plAttacked(f))
	}
	f, err = plRun(t, rulesKOF(0), plTestFake{stats: plTestStats(), trains: trains, attack: first(railwayTakenCode)})
	if err != nil || len(plAttacked(f)) != 2 {
		t.Errorf("run = %v, attacked %v; after E400207 the next train is tried", err, plAttacked(f))
	}
}

func TestRailwayTimeoutStopsTheRun(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the 8s command timeout twice")
	}
	trucks := []*sfs.SFSObject{
		plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(5, trainTypeTruck, 131, plTestOther, 1_000_000, 0, ""),
	}
	silent := func(int) *sfs.SFSObject { return rewardNoReply }
	f, err := plRun(t, plInit(), plTestFake{stats: plTestStats(), trucks: trucks, attack: silent})
	if err == nil || len(plAttacked(f)) != 1 {
		t.Errorf("plunder: run = %v, attacked %v; a timeout must stop after one attack", err, plAttacked(f))
	}

	in := plTestInit(func(*sfs.SFSObject) {}, 0, 0, tdTestTruck(11, 130, 1, false, 0), tdTestTruck(13, 210, 3, false, 0))
	d, err := tdRun(t, in, tdTestFake{send: func(int) *sfs.SFSObject { return rewardNoReply }})
	if got, _ := tdSends(t, d); err == nil || len(got) != 1 {
		t.Errorf("deploy: run = %v, sent %v; a timeout must stop after one send", err, got)
	}
}

func TestRailwaySquadsRankByPowerNotSlot(t *testing.T) {
	// Deploy: defence slot 3 (6M) outranks slots 1 (1M) and 2 (2M), so it escorts the UR truck.
	in := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(plTestSquad(5, 1, 2000, 1, 1_000_000))
		ft.AddSFSObject(plTestSquad(6, 2, 2100, 1, 2_000_000))
		ft.AddSFSObject(plTestSquad(7, 3, 2200, 3, 2_000_000))
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0, tdTestTruck(11, 130, 1, false, 0))
	if _, squads := tdSends(t, tdTestRun(t, in)); !slices.Equal(squads, []int64{3}) {
		t.Errorf("deploy used squads %v, want the strongest, 3", squads)
	}

	// Plunder: attack slot 2 (10M) is the strongest and is the one saved and sent.
	att := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(plTestSquad(9, 1, 1200, 1, 1))
		ft.AddSFSObject(plTestSquad(10, 2, 1000, 5, 1))
		ft.AddSFSObject(plTestSquad(11, 3, 1100, 5, 1))
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0)
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 3_000_000, 0, "")}
	f := plOK(t, att, plTestFake{trucks: trucks})
	for _, r := range f.requests() {
		if r.Cmd == "formation.save.template" {
			if idx, _ := claimInt(r.Params, "index"); idx != 10 {
				t.Errorf("saved template %d, want the strongest attack squad, 10", idx)
			}
		}
	}
	if got := plAttacked(f); !slices.Equal(got, []int64{4}) {
		t.Errorf("attacked %v, want 4 with the 10M squad", got)
	}
}
