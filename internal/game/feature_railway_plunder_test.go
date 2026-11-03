package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// Test ids: the account is uid 77 on server 812, alliance "A1"; 640 and 700 are other servers.
const (
	plTestHome  = 812
	plTestOther = 640
)

// plTestSquad builds a formation_template entry at index with n heroes of the given power each
// (no leadership, so squad power is the hero sum) and heroes numbered from first.
func plTestSquad(index, squadNo int32, first int64, n int, power float64) *sfs.SFSObject {
	t := sfs.NewSFSObject()
	t.PutLong("uuid", int64(9000+index))
	t.PutInt("index", index)
	if squadNo > 0 {
		t.PutInt("squadNo", squadNo)
	}
	arr := sfs.NewSFSArray()
	for i := range n {
		h := sfs.NewSFSObject()
		h.PutLong("heroUuid", first+int64(i))
		h.PutInt("index", int32(i+1))
		h.PutDouble("power", power)
		h.PutDouble("effect_50079", 0)
		arr.AddSFSObject(h)
	}
	t.PutSFSArray("heroes", arr)
	return t
}

// plTestInit is an open station (stage past the fog, opening reward taken) with three unlocked
// world squads, attack squads of 10M/5M/5M, defence squads of 6M/4M/3M, trucks in myTrainList,
// and the given user fields.
func plTestInit(fill func(raw *sfs.SFSObject), dailySends, dailyRobs int32, trucks ...*sfs.SFSObject) *Init {
	return rewardInitWithDay(20, func(raw *sfs.SFSObject) {
		railwayStage(raw, railwayFogStage)
		u := sfs.NewSFSObject()
		u.PutUtfString("uid", "77")
		u.PutInt("serverId", plTestHome)
		u.PutUtfString("allianceId", "A1")
		raw.PutSFSObject("user", u)
		af := sfs.NewSFSArray()
		for i := int32(1); i <= 3; i++ {
			o := sfs.NewSFSObject()
			o.PutLong("uuid", int64(100+i))
			o.PutInt("index", i)
			af.AddSFSObject(o)
		}
		raw.PutSFSArray("army_formation", af)
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(plTestSquad(9, 1, 1000, 5, 2_000_000))
		ft.AddSFSObject(plTestSquad(10, 2, 1100, 5, 1_000_000))
		ft.AddSFSObject(plTestSquad(11, 0, 1200, 5, 1_000_000))
		ft.AddSFSObject(plTestSquad(5, 1, 2000, 3, 2_000_000))
		ft.AddSFSObject(plTestSquad(6, 2, 2100, 2, 2_000_000))
		ft.AddSFSObject(plTestSquad(7, 3, 2200, 1, 3_000_000))
		raw.PutSFSArray("formation_template", ft)
		bn := sfs.NewSFSArray()
		for i, id := range []int64{10138000, 10139000, 10140000, 10141000} {
			b := sfs.NewSFSObject()
			b.PutLong("uuid", int64(500+i+1))
			b.PutInt("bId", int32(id))
			b.PutInt("lv", 1)
			bn.AddSFSObject(b)
		}
		raw.PutSFSArray("building_new", bn)
		td := sfs.NewSFSObject()
		td.PutInt("openReward", 1)
		td.PutLong("openTime", time.Now().Add(-48*time.Hour).UnixMilli())
		td.PutInt("dailySendCount", dailySends)
		td.PutInt("dailyRobCount", dailyRobs)
		list := sfs.NewSFSArray()
		for _, tr := range trucks {
			list.AddSFSObject(tr)
		}
		td.PutSFSArray("myTrainList", list)
		raw.PutSFSObject("trainData", td)
		raw.PutUtfString("trainServers", "640,700,812,900")
		fill(raw)
	})
}

// plTestTarget is one train.list entry, travelling for another hour.
func plTestTarget(uuid int64, typ int32, cfgID int32, serverID int32, power float64, robs int32, alliance string) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutLong("uuid", uuid)
	o.PutInt("type", typ)
	o.PutInt("cfgId", cfgID)
	o.PutInt("serverId", serverID)
	o.PutUtfString("ownerId", "5"+string(rune('0'+uuid%10)))
	o.PutUtfString("allianceId", alliance)
	o.PutLong("sendTime", time.Now().Add(-time.Hour).UnixMilli())
	o.PutLong("arriveTime", time.Now().Add(time.Hour).UnixMilli())
	mi := sfs.NewSFSObject()
	mi.PutInt("robTimes", robs)
	if typ == trainTypeTrain {
		f := sfs.NewSFSObject()
		for _, k := range []string{"armyUnit1", "armyUnit2", "armyUnit3"} {
			u := sfs.NewSFSObject()
			u.PutDouble("power", power/3)
			f.PutSFSObject(k, u)
		}
		mi.PutSFSObject("formation", f)
	} else {
		mi.PutDouble("power", power)
	}
	o.PutSFSObject("marchInfo", mi)
	return o
}

// plTestRun runs railway-plunder against a fake whose train.list returns trucks and trains; the
// station and hero-detail calls fail, so init and the template powers are used.
func plTestRun(t *testing.T, in *Init, trucks, trains []*sfs.SFSObject) *rewardFake {
	t.Helper()
	robs := int32(0)
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "train.list":
			r := sfs.NewSFSObject()
			a, b := sfs.NewSFSArray(), sfs.NewSFSArray()
			for _, o := range trucks {
				a.AddSFSObject(o)
			}
			for _, o := range trains {
				b.AddSFSObject(o)
			}
			r.PutSFSArray("ls", a)
			r.PutSFSArray("allianceTrainList", b)
			r.PutUtfString("trainServers", "640,700,812,900")
			return r
		case "train.attack", "alliance.train.attack", "alliance.train.attack.kof":
			robs++
			r := rewardOK()
			r.PutInt("dailyRobCount", robs)
			return r
		case "formation.save.template", "alliance.train.formation":
			return sfs.NewSFSObject()
		}
		return rewardErr("E999")
	})
	if err := runRailwayPlunder(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	return fake
}

// plAttacked lists the uuids sent to the attack commands, in order.
func plAttacked(f *rewardFake) []int64 {
	var out []int64
	for _, r := range f.requests() {
		switch r.Cmd {
		case "train.attack", "alliance.train.attack", "alliance.train.attack.kof":
			id, _ := claimInt(r.Params, "uuid")
			out = append(out, id)
		}
	}
	return out
}

func TestRailwayPlunderHardRules(t *testing.T) {
	trucks := []*sfs.SFSObject{
		plTestTarget(1, trainTypeTruck, 130, plTestHome, 1_000_000, 0, ""),    // own server: never
		plTestTarget(2, trainTypeTruck, 100, plTestOther, 1_000_000, 0, ""),   // below UR
		plTestTarget(3, trainTypeTruck, 130, plTestOther, 10_000_000, 0, ""),  // not weaker (equal)
		plTestTarget(4, trainTypeTruck, 130, plTestOther, 3_000_000, 0, ""),   // UR, weaker: attack
		plTestTarget(5, trainTypeTruck, 210, plTestOther, 3_000_000, 0, ""),   // sleigh, weaker: attack first
		plTestTarget(6, trainTypeTruck, 210, plTestOther, 1_000_000, 1, ""),   // sleigh already hit
		plTestTarget(7, trainTypeTruck, 130, 999, 1_000_000, 0, ""),           // server not matched
		plTestTarget(8, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "A1"), // own alliance
		plTestTarget(9, trainTypeTruck, 130, plTestOther, 1_000_000, 2, ""),   // plunder cap
	}
	f := plTestRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0), trucks, nil)
	if got := plAttacked(f); !slices.Equal(got, []int64{5, 4}) {
		t.Fatalf("attacked %v, want the sleigh (5) then the UR truck (4)", got)
	}
	for _, r := range f.requests() {
		if r.Cmd == "train.attack" {
			if s, _ := claimInt(r.Params, "serverId"); s == plTestHome {
				t.Fatalf("attacked a truck on the own server: %v", r.Params)
			}
		}
	}
	if got := f.cmds(); got[slices.Index(got, "train.attack")-1] != "formation.save.template" {
		t.Errorf("sent %v: the attack squad must be saved before the first attack", got)
	}
	for _, r := range f.requests() {
		if r.Cmd == "formation.save.template" {
			idx, _ := claimInt(r.Params, "index")
			act, _ := claimInt(r.Params, "action")
			if idx != 9 || act != railwayAttackAction {
				t.Errorf("saved index %d action %d, want the strongest attack squad, 9 / 8", idx, act)
			}
		}
	}
}

func TestRailwayPlunderOwnServerFailsClosed(t *testing.T) {
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")}
	noServer := plTestInit(func(raw *sfs.SFSObject) {
		u := sfs.NewSFSObject()
		u.PutUtfString("uid", "77")
		raw.PutSFSObject("user", u)
	}, 0, 0)
	f := plTestRun(t, noServer, trucks, nil)
	if got := f.cmds(); len(got) != 0 {
		t.Fatalf("sent %v with no known own server; want nothing at all", got)
	}

	// Cross-server login with the switch on: both the login server and the home server are own.
	cross := plTestInit(func(raw *sfs.SFSObject) {
		u := sfs.NewSFSObject()
		u.PutUtfString("uid", "77")
		u.PutInt("serverId", plTestOther)
		u.PutInt("crossFightSrcServerId", plTestHome)
		raw.PutSFSObject("user", u)
		railwaySwitch(raw, 1)
	}, 0, 0)
	trucks = []*sfs.SFSObject{
		plTestTarget(1, trainTypeTruck, 130, plTestHome, 1_000_000, 0, ""),
		plTestTarget(2, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(3, trainTypeTruck, 130, 700, 1_000_000, 0, ""),
	}
	f = plTestRun(t, cross, trucks, nil)
	if got := plAttacked(f); !slices.Equal(got, []int64{3}) {
		t.Fatalf("attacked %v, want only the truck on neither own server (3)", got)
	}
}

func TestRailwayPlunderDailyCapAndTrains(t *testing.T) {
	trucks := []*sfs.SFSObject{
		plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(5, trainTypeTruck, 131, plTestOther, 1_000_000, 0, ""),
	}
	f := plTestRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 3), trucks, nil)
	if got := plAttacked(f); len(got) != 1 {
		t.Fatalf("attacked %v with 3 of 4 plunders used; want exactly one", got)
	}

	protected := plTestTarget(23, trainTypeTrain, 152, plTestOther, 9_000_000, 0, "B2")
	if mi := evObject(protected, "marchInfo"); mi != nil {
		mi.PutLong("protectTime", time.Now().Add(time.Hour).UnixMilli())
	}
	trains := []*sfs.SFSObject{
		plTestTarget(21, trainTypeTrain, 152, plTestHome, 9_000_000, 0, "B2"),   // own server
		plTestTarget(22, trainTypeTrain, 152, plTestOther, 20_000_000, 0, "B2"), // not weaker than 10M+5M+5M
		protected,
		plTestTarget(24, trainTypeTrain, 152, plTestOther, 9_000_000, 3, "B2"), // plunder cap
		plTestTarget(25, trainTypeTrain, 152, plTestOther, 9_000_000, 0, "B2"), // attack
	}
	kof := plTestInit(func(raw *sfs.SFSObject) {
		o := sfs.NewSFSObject()
		o.PutBool("isFunctionOn", true)
		raw.PutSFSObject("allianceTrainStartInfo", o)
	}, 0, 0)
	f = plTestRun(t, kof, nil, trains)
	if got := plAttacked(f); !slices.Equal(got, []int64{25}) {
		t.Fatalf("attacked trains %v, want only 25", got)
	}
	for _, r := range f.requests() {
		if r.Cmd != "alliance.train.attack.kof" {
			continue
		}
		teams := claimObjects(r.Params, "teamInfos")
		buffs := map[int64]bool{}
		for _, tm := range teams {
			n, _ := claimInt(tm, "squadNo")
			buffs[n] = true
		}
		if len(teams) != 3 || len(buffs) != 3 {
			t.Errorf("teamInfos %v: want three teams with distinct squadNo", r.Params)
		}
	}
}

func TestRailwayPowerMatchesTheSoldierTable(t *testing.T) {
	// With no game effects, CalcSoldierPower per T10 soldier (3014) is
	// floor(2514*0.5) + 20*12.5 + 4*35 = 1647, lw_soldier's own power column.
	in := rewardInit(func(*sfs.SFSObject) {})
	sq := railwaySquad{soldier: 3014, heroes: []railwayHero{
		{uuid: 1, index: 1, power: 100.9, lead: 10.7, hasPower: true, hasLead: true},
		{uuid: 2, index: railwayOverlordSlot, power: 5000, lead: 2, hasPower: true, hasLead: true},
	}}
	// Hero 1: power 100 plus 10 soldiers. The Overlord's power isn't counted (a lower bound), but
	// its 2 soldiers are, as ConscriptSoldier does.
	if got, want := railwayPower(in, sq, nil), int64(100+10*1647+2*1647); got != want {
		t.Errorf("railwayPower = %d, want %d", got, want)
	}
	stats := map[int64]railwayHeroStat{1: {power: 200, lead: 1}}
	if got, want := railwayPower(in, sq, stats), int64(200+1*1647+2*1647); got != want {
		t.Errorf("railwayPower with hero details = %d, want %d", got, want)
	}
}

func TestRailwayUR(t *testing.T) {
	for id, want := range map[int64]int32{120: 0, 121: 5, 150: 5, 151: 0, 152: 0, 201: 10, 235: 10, 641: 5, 676: 10, 710: 10, 5035: 5, 5036: 0} {
		if got := railwayTruckQuality(id); got != want {
			t.Errorf("railwayTruckQuality(%d) = %d, want %d", id, got, want)
		}
	}
}
