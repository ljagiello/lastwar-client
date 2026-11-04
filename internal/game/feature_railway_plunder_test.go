package game

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// Test ids: the account is uid 77 on server 812, alliance "A1"; 640 and 700 are other servers.
const (
	plTestHome  = 812
	plTestOther = 640
)

// plTestSquad builds a formation_template entry at index with n heroes numbered from first, each
// with a template power (no leadership, so squad power is the hero sum).
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

// plTestStats are the hero.init.detail.get powers matching plTestInit's templates: attack squads of
// 10M (heroes 1000-1004), 5M (1100-1104) and 5M (1200-1204); defence squads of 6M (2000-2002), 4M
// (2100-2101) and 3M (2200).
func plTestStats() map[int64]float64 {
	m := map[int64]float64{}
	for i := int64(0); i < 5; i++ {
		m[1000+i], m[1100+i], m[1200+i] = 2_000_000, 1_000_000, 1_000_000
	}
	for i := int64(0); i < 3; i++ {
		m[2000+i] = 2_000_000
	}
	m[2100], m[2101], m[2200] = 2_000_000, 2_000_000, 3_000_000
	return m
}

// plTestTemplates are the default truck squad templates (see plTestStats).
func plTestTemplates() []*sfs.SFSObject {
	return []*sfs.SFSObject{
		plTestSquad(9, 1, 1000, 5, 2_000_000), plTestSquad(10, 2, 1100, 5, 1_000_000),
		plTestSquad(11, 0, 1200, 5, 1_000_000), plTestSquad(5, 1, 2000, 3, 2_000_000),
		plTestSquad(6, 2, 2100, 2, 2_000_000), plTestSquad(7, 3, 2200, 1, 3_000_000),
	}
}

// plTestInit is an open station (stage past the fog, opening reward taken) with world squads 1-3
// unlocked, plTestTemplates, trucks in myTrainList, and a user on server 812. fill runs last.
func plTestInit(fill func(raw *sfs.SFSObject), dailySends, dailyRobs int32, trucks ...*sfs.SFSObject) *Init {
	return rewardInitWithDay(20, func(raw *sfs.SFSObject) {
		railwayStage(raw, railwayFogStage)
		u := sfs.NewSFSObject()
		u.PutUtfString("uid", "77")
		u.PutInt("serverId", plTestHome)
		u.PutUtfString("allianceId", "A1")
		raw.PutSFSObject("user", u)
		plTestWorldSquads(raw, 3)
		ft := sfs.NewSFSArray()
		for _, t := range plTestTemplates() {
			ft.AddSFSObject(t)
		}
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

// plTestWorldSquads unlocks world squads 1..n in army_formation, each with one hero 3000+i.
func plTestWorldSquads(raw *sfs.SFSObject, n int32) {
	af := sfs.NewSFSArray()
	for i := int32(1); i <= n; i++ {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", int64(100+i))
		o.PutInt("index", i)
		h := sfs.NewSFSObject()
		h.PutLong("heroUuid", int64(3000+i))
		h.PutInt("index", 1)
		hs := sfs.NewSFSArray()
		hs.AddSFSObject(h)
		o.PutSFSArray("heroes", hs)
		af.AddSFSObject(o)
	}
	raw.PutSFSArray("army_formation", af)
}

// plTestTarget is one train.list entry, travelling for another hour.
func plTestTarget(uuid int64, typ int32, cfgID int32, serverID int32, power float64, robs int32, alliance string) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutLong("uuid", uuid)
	o.PutInt("type", typ)
	o.PutInt("cfgId", cfgID)
	if serverID != 0 {
		o.PutInt("serverId", serverID)
	}
	o.PutUtfString("ownerId", "5"+strconv.FormatInt(uuid, 10))
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
	} else if power > 0 {
		mi.PutDouble("power", power)
	}
	o.PutSFSObject("marchInfo", mi)
	return o
}

// plClose, returned by a plTestFake.attack hook, makes the fake close the connection.
var plClose = sfs.NewSFSObject()

// plTestFake is the fake server's behaviour for one railway-plunder run.
type plTestFake struct {
	stats   map[int64]float64          // hero.init.detail.get powers; nil answers with an error
	station *sfs.SFSObject             // train.data reply; nil answers with an error
	trucks  []*sfs.SFSObject           // train.list ls
	trains  []*sfs.SFSObject           // train.list allianceTrainList
	lineup  *sfs.SFSObject             // alliance.train.formation reply; nil sends an empty one
	attack  func(n int) *sfs.SFSObject // reply to the n-th attack (1-based); nil = success
	save    *sfs.SFSObject             // formation.save.template reply; nil = success
}

func (c plTestFake) reply(robs *int32) func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
	attacks := 0
	return func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "hero.init.detail.get":
			if c.stats == nil {
				return rewardErr("E999")
			}
			r := sfs.NewSFSObject()
			arr := sfs.NewSFSArray()
			for uuid, p := range c.stats {
				h := sfs.NewSFSObject()
				h.PutLong("uuid", uuid)
				eff := sfs.NewSFSObject()
				eff.PutDouble("100000", p)
				eff.PutDouble("50079", 0)
				h.PutSFSObject("effect", eff)
				arr.AddSFSObject(h)
			}
			r.PutSFSArray("userHero", arr)
			return r
		case "train.data":
			if c.station == nil {
				return rewardErr("E999")
			}
			return c.station
		case "train.list":
			r := sfs.NewSFSObject()
			a, b := sfs.NewSFSArray(), sfs.NewSFSArray()
			for _, o := range c.trucks {
				a.AddSFSObject(o)
			}
			for _, o := range c.trains {
				b.AddSFSObject(o)
			}
			r.PutSFSArray("ls", a)
			r.PutSFSArray("allianceTrainList", b)
			r.PutUtfString("trainServers", "640,700,812,900")
			return r
		case "train.attack", "alliance.train.attack", "alliance.train.attack.kof":
			attacks++
			if c.attack != nil {
				if r := c.attack(attacks); r == plClose {
					return nil
				} else if r != nil {
					return r
				}
			}
			*robs++
			r := rewardOK()
			r.PutInt("dailyRobCount", *robs)
			return r
		case "formation.save.template":
			if c.save != nil {
				return c.save
			}
			return sfs.NewSFSObject()
		case "alliance.train.formation":
			if c.lineup != nil {
				return c.lineup
			}
			return sfs.NewSFSObject()
		}
		return rewardErr("E999")
	}
}

// plRun runs railway-plunder against c and returns the fake and the run's error.
func plRun(t *testing.T, in *Init, c plTestFake) (*rewardFake, error) {
	t.Helper()
	var robs int32
	conn, fake := startRewardFake(t, c.reply(&robs))
	return fake, runRailwayPlunder(conn, in)
}

// plOK is plRun with the default hero stats, failing the test on a run error.
func plOK(t *testing.T, in *Init, c plTestFake) *rewardFake {
	t.Helper()
	if c.stats == nil {
		c.stats = plTestStats()
	}
	f, err := plRun(t, in, c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return f
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

func plInit() *Init { return plTestInit(func(*sfs.SFSObject) {}, 0, 0) }

func TestRailwayPlunderHardRules(t *testing.T) {
	heroOnly := plTestTarget(10, trainTypeTruck, 130, plTestOther, 0, 0, "")
	hi := sfs.NewSFSObject()
	h := sfs.NewSFSObject()
	h.PutDouble("power", 1_000)
	hi.PutSFSObject("1", h)
	evObject(heroOnly, "marchInfo").PutSFSObject("heroInfo", hi)
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
		heroOnly, // no marchInfo.power: the hero-only sum would undercount, so power is unknown
		plTestTarget(11, trainTypeTruck, 130, plTestOther, 0, 0, ""), // no power at all
		plTestTarget(12, trainTypeTruck, 130, 0, 1_000_000, 0, ""),   // no serverId
	}
	f := plOK(t, plInit(), plTestFake{trucks: trucks})
	if got := plAttacked(f); !slices.Equal(got, []int64{5, 4}) {
		t.Fatalf("attacked %v, want the sleigh (5) then the UR truck (4)", got)
	}
	reqs := f.requests()
	first := slices.IndexFunc(reqs, func(r rewardFakeReq) bool { return r.Cmd == "train.attack" })
	if first < 1 || reqs[first-1].Cmd != "formation.save.template" {
		t.Fatalf("sent %v: the attack squad must be saved before the first attack", f.cmds())
	}
	if idx, _ := claimInt(reqs[first-1].Params, "index"); idx != 9 {
		t.Errorf("saved template %d, want the strongest attack squad, 9", idx)
	}
	for _, r := range reqs {
		if r.Cmd != "train.attack" {
			continue
		}
		if s, _ := claimInt(r.Params, "serverId"); s == plTestHome {
			t.Fatalf("attacked a truck on the own server: %v", r.Params)
		}
		if n, _ := claimInt(r.Params, "squadNo"); n != 1 {
			t.Errorf("train.attack squadNo %d, want 1", n)
		}
		for _, h := range claimObjects(r.Params, "heroInfo") {
			if v, _ := h.Get("heroUuid"); v.Type != sfs.SFSLong {
				t.Errorf("heroUuid is type %v, want Long", v.Type)
			}
		}
	}
}

func TestRailwayPlunderOwnServerFailsClosed(t *testing.T) {
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")}
	user := func(fields func(u *sfs.SFSObject)) *Init {
		return plTestInit(func(raw *sfs.SFSObject) {
			u := sfs.NewSFSObject()
			u.PutUtfString("uid", "77")
			fields(u)
			raw.PutSFSObject("user", u)
			railwaySwitch(raw, 1)
		}, 0, 0)
	}
	for name, in := range map[string]*Init{
		"no server id":        user(func(*sfs.SFSObject) {}),
		"server id 0":         user(func(u *sfs.SFSObject) { u.PutInt("serverId", 0) }),
		"only the source one": user(func(u *sfs.SFSObject) { u.PutInt("crossFightSrcServerId", plTestHome) }),
	} {
		t.Run(name, func(t *testing.T) {
			if got := plOK(t, in, plTestFake{trucks: trucks}).cmds(); len(got) != 0 {
				t.Fatalf("sent %v with no known own server; want nothing at all", got)
			}
		})
	}

	// Cross-server login with the switch on: both the login server and the home server are own.
	cross := user(func(u *sfs.SFSObject) {
		u.PutInt("serverId", plTestOther)
		u.PutInt("crossFightSrcServerId", plTestHome)
	})
	trucks = []*sfs.SFSObject{
		plTestTarget(1, trainTypeTruck, 130, plTestHome, 1_000_000, 0, ""),
		plTestTarget(2, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(3, trainTypeTruck, 130, 700, 1_000_000, 0, ""),
	}
	if got := plAttacked(plOK(t, cross, plTestFake{trucks: trucks})); !slices.Equal(got, []int64{3}) {
		t.Fatalf("attacked %v, want only the truck on neither own server (3)", got)
	}
}

func TestRailwayPlunderAttackPowerComesFromFreshHeroStats(t *testing.T) {
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 3_000_000, 0, "")}
	// The templates say squad 9 is 10M, but the fresh stats say 5 x 0.5M = 2.5M: a 3M truck isn't weaker.
	low := plTestStats()
	for i := int64(0); i < 5; i++ {
		low[1000+i], low[1100+i], low[1200+i] = 500_000, 500_000, 500_000
	}
	if got := plAttacked(plOK(t, plInit(), plTestFake{stats: low, trucks: trucks})); len(got) != 0 {
		t.Errorf("attacked %v although the fresh stats put every squad below the truck", got)
	}
	// A main hero missing from the reply makes that squad's power unknown, not its template value.
	partial := plTestStats()
	delete(partial, 1000)
	for i := int64(0); i < 5; i++ {
		partial[1100+i], partial[1200+i] = 100_000, 100_000
	}
	if got := plAttacked(plOK(t, plInit(), plTestFake{stats: partial, trucks: trucks})); len(got) != 0 {
		t.Errorf("attacked %v using a squad with a hero the stats don't cover", got)
	}
	// No stats at all: nothing is plundered, and the list isn't even asked for.
	f, err := plRun(t, plInit(), plTestFake{trucks: trucks})
	if err != nil || slices.Contains(f.cmds(), "train.list") {
		t.Errorf("run = %v, sent %v; want no plunder without hero stats", err, f.cmds())
	}
}

func TestRailwayPlunderGates(t *testing.T) {
	arrived := plTestTarget(20, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")
	arrived.PutLong("arriveTime", time.Now().Add(-time.Minute).UnixMilli())
	parked := plTestTarget(21, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")
	parked.PutLong("sendTime", 0)
	mine := plTestTarget(22, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")
	mine.PutUtfString("ownerId", "77")
	noOwner := plTestTarget(23, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")
	noOwner.PutUtfString("ownerId", "")
	if got := plAttacked(plOK(t, plInit(), plTestFake{trucks: []*sfs.SFSObject{arrived, parked, mine, noOwner}})); len(got) != 0 {
		t.Errorf("attacked %v: arrived, parked, own and ownerless trucks must be skipped", got)
	}

	ok := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")}
	td := func(fn func(td *sfs.SFSObject)) func(raw *sfs.SFSObject) {
		return func(raw *sfs.SFSObject) { fn(evObject(raw, "trainData")) }
	}
	for name, fill := range map[string]func(raw *sfs.SFSObject){
		"another warzone": func(raw *sfs.SFSObject) {
			u := evObject(raw, "user")
			u.PutInt("crossFightSrcServerId", 900) // not the target's server, so only the gate holds it
		},
		"opening reward not taken": td(func(td *sfs.SFSObject) { td.PutInt("openReward", 0) }),
		"not started yet":          td(func(td *sfs.SFSObject) { td.PutLong("openTime", time.Now().Add(time.Hour).UnixMilli()) }),
		"daily limit used":         td(func(td *sfs.SFSObject) { td.PutInt("dailyRobCount", railwayDailyRobs) }),
		"daily count unknown": func(raw *sfs.SFSObject) {
			old := evObject(raw, "trainData")
			n := sfs.NewSFSObject()
			for _, k := range []string{"openReward", "openTime", "myTrainList"} {
				v, _ := old.Get(k)
				n.PutValue(k, v)
			}
			raw.PutSFSObject("trainData", n)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := plAttacked(plOK(t, plTestInit(fill, 0, 0), plTestFake{trucks: ok})); len(got) != 0 {
				t.Errorf("attacked %v; the gate must hold every attack", got)
			}
		})
	}

	// The fresh train.data reply wins over init: it says the daily limit is used up.
	st := sfs.NewSFSObject()
	st.PutInt("openReward", 1)
	st.PutLong("openTime", time.Now().Add(-48*time.Hour).UnixMilli())
	st.PutInt("dailyRobCount", railwayDailyRobs)
	st.PutSFSArray("myTrainList", sfs.NewSFSArray())
	if got := plAttacked(plOK(t, plInit(), plTestFake{station: st, trucks: ok})); len(got) != 0 {
		t.Errorf("attacked %v although train.data says today's plunders are used up", got)
	}
}

func TestRailwayPlunderRefusals(t *testing.T) {
	trucks := []*sfs.SFSObject{
		plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, ""),
		plTestTarget(5, trainTypeTruck, 131, plTestOther, 2_000_000, 0, ""),
		plTestTarget(6, trainTypeTruck, 132, plTestOther, 3_000_000, 0, ""),
	}
	// E400207 (looted by someone else): go on; the count isn't raised by the refusal.
	taken := func(n int) *sfs.SFSObject {
		if n == 1 {
			return rewardErr(railwayTakenCode)
		}
		return nil
	}
	in := plTestInit(func(*sfs.SFSObject) {}, 0, 2)
	if got := plAttacked(plOK(t, in, plTestFake{trucks: trucks, attack: taken})); !slices.Equal(got, []int64{4, 5, 6}) {
		t.Errorf("attacked %v, want all three: the taken one costs no plunder, so two more fit", got)
	}
	// Any other refusal stops the run with an error.
	other := func(int) *sfs.SFSObject { return rewardErr("E400999") }
	f, err := plRun(t, plInit(), plTestFake{stats: plTestStats(), trucks: trucks, attack: other})
	if err == nil || len(plAttacked(f)) != 1 {
		t.Errorf("run = %v, attacked %v; want one attack and an error", err, plAttacked(f))
	}
	// A dead connection stops it too.
	f, err = plRun(t, plInit(), plTestFake{stats: plTestStats(), trucks: trucks, attack: func(int) *sfs.SFSObject { return plClose }})
	if err == nil || len(plAttacked(f)) != 1 {
		t.Error("run = nil after the connection closed on an attack; want an error")
	}
	// A failed squad save sends no attack.
	f, err = plRun(t, plInit(), plTestFake{stats: plTestStats(), trucks: trucks, save: rewardErr("E400208")})
	if err == nil || len(plAttacked(f)) != 0 {
		t.Errorf("run = %v, attacked %v; want an error and no attack after a failed save", err, plAttacked(f))
	}
}

// plLineup is an alliance.train.formation reply with three teams of the given hero uuids.
func plLineup(teams ...[]int64) *sfs.SFSObject {
	f := sfs.NewSFSObject()
	for i, ids := range teams {
		u := sfs.NewSFSObject()
		u.PutInt("squadNo", int32(i+1))
		arr := sfs.NewSFSArray()
		for j, id := range ids {
			h := sfs.NewSFSObject()
			h.PutLong("heroUuid", id)
			h.PutInt("index", int32(j+1))
			arr.AddSFSObject(h)
		}
		u.PutSFSArray("heroes", arr)
		f.PutSFSObject("armyUnit"+strconv.Itoa(i+1), u)
	}
	r := sfs.NewSFSObject()
	r.PutInt("type", 1)
	r.PutSFSObject("formation", f)
	return r
}

func TestRailwayPlunderTrains(t *testing.T) {
	kof := func(on bool) *Init {
		return plTestInit(func(raw *sfs.SFSObject) {
			o := sfs.NewSFSObject()
			o.PutBool("isFunctionOn", on)
			raw.PutSFSObject("allianceTrainStartInfo", o)
		}, 0, 0)
	}
	protected := plTestTarget(23, trainTypeTrain, 152, plTestOther, 9_000_000, 0, "B2")
	evObject(protected, "marchInfo").PutLong("protectTime", time.Now().Add(time.Hour).UnixMilli())
	vip := plTestTarget(26, trainTypeTrain, 152, plTestOther, 9_000_000, 2, "B2")
	evObject(vip, "marchInfo").PutBool("vipOn", true)
	trains := []*sfs.SFSObject{
		plTestTarget(21, trainTypeTrain, 152, plTestHome, 9_000_000, 0, "B2"),   // own server
		plTestTarget(22, trainTypeTrain, 152, plTestOther, 20_000_000, 0, "B2"), // not weaker than 10M+5M+5M
		protected,
		plTestTarget(24, trainTypeTrain, 152, plTestOther, 9_000_000, 3, "B2"), // plunder cap
		vip, // a VIP aboard lowers the cap to 2
		plTestTarget(25, trainTypeTrain, 152, plTestOther, 9_000_000, 0, "B2"), // attack
	}
	for _, on := range []bool{true, false} {
		want := map[bool]string{true: "alliance.train.attack.kof", false: "alliance.train.attack"}[on]
		f := plOK(t, kof(on), plTestFake{trains: trains})
		if got := plAttacked(f); !slices.Equal(got, []int64{25}) {
			t.Fatalf("isFunctionOn %v: attacked trains %v, want only 25", on, got)
		}
		reqs := f.requests()
		i := slices.IndexFunc(reqs, func(r rewardFakeReq) bool { return r.Cmd == want })
		if i < 1 || reqs[i-1].Cmd != "alliance.train.formation" {
			t.Fatalf("isFunctionOn %v: sent %v, want alliance.train.formation then %s", on, f.cmds(), want)
		}
		var buffs []int64
		for _, tm := range claimObjects(reqs[i].Params, "teamInfos") {
			n, _ := claimInt(tm, "squadNo")
			buffs = append(buffs, n)
		}
		if slices.Sort(buffs); !slices.Equal(buffs, []int64{1, 2, 3}) {
			t.Errorf("teamInfos squadNo %v, want 1, 2 and 3", buffs)
		}
	}

	// The lineup the server returns is what's sent, and its power must beat the train.
	lineup := plLineup([]int64{1000, 1001}, []int64{1100}, []int64{1200})
	f := plOK(t, kof(true), plTestFake{trains: trains, lineup: lineup})
	if got := plAttacked(f); len(got) != 0 {
		t.Errorf("attacked %v with a 6M lineup against 9M trains", got)
	}
	strong := plLineup([]int64{1000, 1001, 1002, 1003, 1004}, []int64{1100, 1101, 1102}, []int64{1200, 1201})
	f = plOK(t, kof(true), plTestFake{trains: trains, lineup: strong})
	if got := plAttacked(f); !slices.Equal(got, []int64{25}) {
		t.Fatalf("attacked %v with a 15M lineup, want 25", got)
	}
	for _, r := range f.requests() {
		if r.Cmd == "alliance.train.attack.kof" {
			first := claimObjects(r.Params, "teamInfos")[0]
			if id, _ := claimInt(claimObjects(first, "heroInfos")[0], "heroUuid"); id != 1000 {
				t.Errorf("team 1 starts with hero %d, want the lineup's 1000", id)
			}
		}
	}

	// Without three attack squads and without a lineup, trains are left alone.
	two := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(plTestSquad(9, 1, 1000, 5, 2_000_000))
		ft.AddSFSObject(plTestSquad(10, 2, 1100, 5, 1_000_000))
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0)
	if got := plAttacked(plOK(t, two, plTestFake{trains: trains})); len(got) != 0 {
		t.Errorf("attacked %v with only two attack squads", got)
	}
}

// The client's local item table overrides init dataConfig for alliance_train (DataConfig.lua:36-74),
// so a larger k10 in init must not raise the per-train cap above the table's 3.
func TestRailwayPlunderTrainCapIgnoresInitDataConfig(t *testing.T) {
	in := plTestInit(func(raw *sfs.SFSObject) {
		train := sfs.NewSFSObject()
		train.PutInt("k10", 5)
		dc := sfs.NewSFSObject()
		dc.PutSFSObject("alliance_train", train)
		raw.PutSFSObject("dataConfig", dc)
	}, 0, 0)
	vip := plTestTarget(26, trainTypeTrain, 152, plTestOther, 9_000_000, 2, "B2")
	evObject(vip, "marchInfo").PutBool("vipOn", true)
	trains := []*sfs.SFSObject{
		plTestTarget(24, trainTypeTrain, 152, plTestOther, 9_000_000, 3, "B2"), // at the table's cap
		vip, // at the table's cap less one
		plTestTarget(25, trainTypeTrain, 152, plTestOther, 9_000_000, 2, "B2"), // below it
	}
	if got := plAttacked(plOK(t, in, plTestFake{trains: trains})); !slices.Equal(got, []int64{25}) {
		t.Fatalf("with init k10 = 5, attacked trains %v, want only 25", got)
	}
}

func TestRailwayPlunderSharedDailyCap(t *testing.T) {
	trains := []*sfs.SFSObject{
		plTestTarget(31, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B2"),
		plTestTarget(32, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B3"),
		plTestTarget(33, trainTypeTrain, 152, plTestOther, 3_000_000, 0, "B4"),
	}
	if got := plAttacked(plOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 2), plTestFake{trains: trains})); len(got) != 2 {
		t.Errorf("attacked trains %v with 2 of 4 plunders used; want exactly 2", got)
	}
	trucks := []*sfs.SFSObject{plTestTarget(4, trainTypeTruck, 130, plTestOther, 1_000_000, 0, "")}
	got := plAttacked(plOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 3), plTestFake{trucks: trucks, trains: trains}))
	if !slices.Equal(got, []int64{4}) {
		t.Errorf("attacked %v with 3 of 4 used; want only the truck, since trucks and trains share the count", got)
	}
}

func TestRailwaySquadsParsing(t *testing.T) {
	dupe := plTestSquad(10, 1, 1100, 2, 1) // shares squadNo 1 with 9, and its second hero with 9
	heroes := claimObjects(dupe, "heroes")
	heroes[1].PutLong("heroUuid", 1000)
	ov := sfs.NewSFSObject()
	ov.PutLong("heroUuid", 7000)
	ov.PutInt("index", railwayOverlordSlot)
	arr := sfs.NewSFSArray()
	for _, h := range heroes {
		arr.AddSFSObject(h)
	}
	arr.AddSFSObject(ov)
	dupe.PutSFSArray("heroes", arr)
	first := plTestSquad(9, 1, 1000, 2, 1)
	fa := claimObjects(first, "heroes")
	fa = append(fa, ov)
	fArr := sfs.NewSFSArray()
	for _, h := range fa {
		fArr.AddSFSObject(h)
	}
	first.PutSFSArray("heroes", fArr)
	first.PutInt("chipEquipGroup", 3)
	third := plTestSquad(11, 0, 1200, 1, 1)
	third.PutInt("chipEquipGroup", 0)
	locked := plTestSquad(12, 4, 1300, 1, 1) // world squad 4 is locked
	in := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		for _, t := range []*sfs.SFSObject{first, dupe, third, locked, plTestSquad(5, 1, 2000, 1, 1)} {
			ft.AddSFSObject(t)
		}
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0)
	got := railwaySquads(in, railwayAttackAction)
	if len(got) != 3 {
		t.Fatalf("parsed %d attack squads, want 3 (slot 4 is locked)", len(got))
	}
	// InitTeamUsingBuff keeps a template's own squadNo even when another uses it, and fills only
	// a missing one: own slot if free.
	if got[0].squadNo != 1 || got[1].squadNo != 1 || got[2].squadNo != 3 {
		t.Errorf("squadNo %d/%d/%d, want 1/1/3", got[0].squadNo, got[1].squadNo, got[2].squadNo)
	}
	if got[0].chip != 3 || got[1].chip != 1 || got[2].chip != 0 {
		t.Errorf("chip %d/%d/%d, want 3, the default 1, and none for 0", got[0].chip, got[1].chip, got[2].chip)
	}
	var second []int64
	for _, h := range got[1].heroes {
		second = append(second, h.uuid)
	}
	if !slices.Equal(second, []int64{1100, 7000}) {
		t.Errorf("squad 2 heroes %v, want 1100 and the Overlord 7000 (1000 stays in squad 1 only)", second)
	}

	// Defence templates only: the attack set copies the world squads, squadNo and chip = slot.
	defOnly := plTestInit(func(raw *sfs.SFSObject) {
		ft := sfs.NewSFSArray()
		ft.AddSFSObject(plTestSquad(5, 1, 2000, 1, 1))
		raw.PutSFSArray("formation_template", ft)
	}, 0, 0)
	att := railwaySquads(defOnly, railwayAttackAction)
	if len(att) != 3 || att[2].slot != 3 || att[2].squadNo != 3 || att[2].chip != 3 || att[2].heroes[0].uuid != 3003 {
		t.Errorf("attack squads copied from the world squads = %+v", att)
	}
}

func TestRailwayPowerMatchesTheSoldierTable(t *testing.T) {
	// With no game effects, CalcSoldierPower per T10 soldier (3014) is
	// floor(2514*0.5) + 20*12.5 + 4*35 = 1647, lw_soldier's own power column.
	in := rewardInit(func(*sfs.SFSObject) {})
	sq := railwaySquad{soldier: 3014, heroes: []railwayHero{
		{uuid: 1, index: 1, power: 100.9, lead: 10.7},
		{uuid: 2, index: railwayOverlordSlot, power: 5000, lead: 2},
	}}
	// Hero 1: power 100 plus 10 soldiers. The Overlord's power isn't counted (a lower bound), but
	// its 2 soldiers are, as ConscriptSoldier does.
	if got, want := railwayPower(in, sq, nil, false), int64(100+10*1647+2*1647); got != want {
		t.Errorf("railwayPower = %d, want %d", got, want)
	}
	// Strict: only the fresh stats count, and the Overlord (not in them) adds nothing.
	stats := map[int64]railwayHeroStat{1: {power: 200, lead: 1}}
	if got, want := railwayPower(in, sq, stats, true), int64(200+1*1647); got != want {
		t.Errorf("strict railwayPower = %d, want %d", got, want)
	}
	if got := railwayPower(in, sq, map[int64]railwayHeroStat{}, true); got != 0 {
		t.Errorf("strict railwayPower without the hero's stats = %d, want 0 (unknown)", got)
	}
	// A soldierType the table doesn't know adds no soldiers rather than the strongest unit.
	sq.soldier = 12005
	if got := railwayPower(in, sq, stats, true); got != 200 {
		t.Errorf("railwayPower with an unknown soldierType = %d, want 200", got)
	}
}

func TestRailwayUR(t *testing.T) {
	for id, want := range map[int64]int32{
		120: 0, 121: 5, 150: 5, 151: 0, 152: 0, 200: 0, 201: 10, 235: 10, 236: 0,
		640: 0, 641: 5, 675: 5, 676: 10, 710: 10, 711: 0, 5030: 0, 5031: 5, 5035: 5, 5036: 0,
	} {
		if got := railwayTruckQuality(id); got != want {
			t.Errorf("railwayTruckQuality(%d) = %d, want %d", id, got, want)
		}
	}
}
