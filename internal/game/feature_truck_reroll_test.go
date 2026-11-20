package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// trGood is one cargo entry: an item {type 7, value {id, num}} when id != "", else a resource
// {type, value: num} (RewardType: 1 iron, 20 food, 31 coins; docs/world-map.mdx).
type trGood struct {
	typ int32
	id  string
	num int32
}

func trGoods(goods ...trGood) *sfs.SFSObject {
	full := sfs.NewSFSArray()
	for _, g := range goods {
		e := sfs.NewSFSObject()
		e.PutInt("type", g.typ)
		if g.id != "" {
			v := sfs.NewSFSObject()
			v.PutUtfString("id", g.id)
			v.PutInt("num", g.num)
			e.PutSFSObject("value", v)
		} else {
			e.PutInt("value", g.num)
		}
		full.AddSFSObject(e)
	}
	o := sfs.NewSFSObject()
	o.PutSFSArray("full", full)
	o.PutSFSArray("cur", sfs.NewSFSArray())
	return o
}

// trTruck is a parked own truck at truck station slot with cfgId, changeCountByGold count (count
// < -1 omits it), base cargo of iron and one item 500105 (below its train_para 55 count of 2) and
// no extra cargo.
func trTruck(uuid int64, cfgID int32, slot int64, count int32) *sfs.SFSObject {
	return trTruckWith(uuid, cfgID, slot, count,
		trGoods(trGood{typ: 1, num: 120000}, trGood{typ: 7, id: "500105", num: 1}), trGoods())
}

// trTruckWith is trTruck with the given baseGoods and extraGoods (nil: left out).
func trTruckWith(uuid int64, cfgID int32, slot int64, count int32, base, extra *sfs.SFSObject) *sfs.SFSObject {
	t := tdTestTruck(uuid, cfgID, slot, false, 0)
	if count >= -1 {
		t.PutInt("changeCountByGold", count)
	}
	if base != nil {
		t.PutSFSObject("baseGoods", base)
	}
	if extra != nil {
		t.PutSFSObject("extraGoods", extra)
	}
	return t
}

// trFake answers truck-reroll's requests: change returns the reply to the n-th train.change (nil
// = a myTrain with cfgId 130, a UR truck), station the train.data reply (nil = an error, so init
// is used).
type trFake struct {
	change  func(n int, uuid int64) *sfs.SFSObject
	station *sfs.SFSObject
}

func trRun(t *testing.T, in *Init, c trFake) (*rewardFake, []time.Duration, error) {
	t.Helper()
	var slept []time.Duration
	orig := trSleep
	trSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { trSleep = orig })
	n := 0
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "train.change":
			n++
			uuid, _ := claimInt(p, "uuid")
			if c.change != nil {
				return c.change(n, uuid)
			}
			r := sfs.NewSFSObject()
			mt := sfs.NewSFSObject()
			mt.PutLong("uuid", uuid+1000)
			mt.PutInt("cfgId", 130)
			mt.PutInt("changeCountByGold", 1)
			mt.PutLong("sendTime", 0)
			r.PutSFSObject("myTrain", mt)
			return r
		case "train.data":
			if c.station != nil {
				return c.station
			}
		}
		return rewardErr("E999")
	})
	err := runTruckReroll(conn, in)
	return fake, slept, err
}

// trChanged lists the truck uuids of every train.change, checking each request is exactly
// {uuid: Long} (ChangeTrainMessage.lua:5-8) and that nothing but reads and train.change was sent.
func trChanged(t *testing.T, f *rewardFake) []int64 {
	t.Helper()
	var out []int64
	for _, r := range f.requests() {
		switch r.Cmd {
		case "train.change":
		case "train.data":
			continue
		default:
			t.Errorf("sent %s; truck-reroll sends only train.data and train.change", r.Cmd)
			continue
		}
		// _id is the session layer's request id.
		if keys := slices.DeleteFunc(r.Params.Keys(), func(k string) bool { return k == "_id" }); !slices.Equal(keys, []string{"uuid"}) {
			t.Errorf("train.change keys %v, want only uuid", keys)
		}
		if v, _ := r.Params.Get("uuid"); v.Type != sfs.SFSLong {
			t.Errorf("train.change uuid is type %v, want Long", v.Type)
		}
		id, _ := claimInt(r.Params, "uuid")
		out = append(out, id)
	}
	return out
}

func trRunOK(t *testing.T, in *Init) []int64 {
	t.Helper()
	f, _, err := trRun(t, in, trFake{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return trChanged(t, f)
}

// TestTruckQualityTable pins the train_property (40413) type-1 rows at each range's ends: quality
// 1..4 are N..SSR, 5 UR, 10 the sleigh; 151-152 are alliance trains (type 2) and unknown here.
func TestTruckQualityTable(t *testing.T) {
	for cfg, want := range map[int64]int32{
		1: 1, 30: 1, 31: 2, 60: 2, 61: 3, 90: 3, 91: 4, 120: 4, 121: 5, 150: 5, 151: 0, 152: 0, 200: 0,
		201: 10, 235: 10, 236: 0, 500: 0, 501: 1, 535: 1, 536: 2, 570: 2, 571: 3, 605: 3, 606: 4, 640: 4,
		641: 5, 675: 5, 676: 10, 710: 10, 711: 0, 1030: 0, 1031: 1, 1035: 1, 1036: 0, 2031: 2, 2035: 2,
		3031: 3, 3035: 3, 4031: 4, 4035: 4, 5031: 5, 5035: 5, 0: 0,
	} {
		if got := truckQuality(cfg); got != want {
			t.Errorf("truckQuality(%d) = %d, want %d", cfg, got, want)
		}
	}
}

// TestTruckRerollOnlyFreeLowTrucks: only a parked truck below SSR in an unlocked slot is rerolled;
// SSR (the client's nudge stops below 4, UILWTrainDepartureView.lua:57), UR, the sleigh, an unknown
// cfgId and a locked slot's truck are kept.
func TestTruckRerollOnlyFreeLowTrucks(t *testing.T) {
	in := plTestInit(func(*sfs.SFSObject) {}, 0, 0,
		trTruck(11, 30, 1, 0),  // N: rerolled
		trTruck(12, 91, 2, 0),  // SSR: kept
		trTruck(13, 210, 3, 0), // sleigh: kept
		trTruck(14, 61, 4, 0),  // SR, but world squad 4 is locked: kept
	)
	if got := trRunOK(t, in); !slices.Equal(got, []int64{11}) {
		t.Errorf("rerolled %v, want only the N truck 11", got)
	}
	for name, cfg := range map[string]int32{"SR": 90, "R": 536, "N 1031": 1031} {
		if got := trRunOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trTruck(21, cfg, 1, 0))); !slices.Equal(got, []int64{21}) {
			t.Errorf("%s (cfgId %d): rerolled %v, want it", name, cfg, got)
		}
	}
	for name, cfg := range map[string]int32{"SSR": 120, "SSR 606": 606, "UR": 121, "unknown": 999, "alliance train": 151} {
		if got := trRunOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trTruck(21, cfg, 1, 0))); len(got) != 0 {
			t.Errorf("%s (cfgId %d): rerolled %v, want it kept", name, cfg, got)
		}
	}
}

// TestTruckRerollOnlyWhenFree: train_para 9 is "0,1" (40413), read at index changeCountByGold + 1
// and at its last entry past the end (TrainData.lua:666-672); without changeCountByGold the cost
// is 1.
func TestTruckRerollOnlyWhenFree(t *testing.T) {
	for _, tc := range []struct {
		count int32 // < -1: absent
		want  bool
	}{{0, true}, {1, false}, {2, false}, {7, false}, {-1, false}, {-2, false}} {
		got := trRunOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trTruck(11, 30, 1, tc.count)))
		if (len(got) == 1) != tc.want {
			t.Errorf("changeCountByGold %d: rerolled %v, want rerolled %v", tc.count, got, tc.want)
		}
	}
}

// TestTruckRerollSkipsHighOrUnreadableGoods: train_para 55 is "2270000;2|500105;2" (40413): the
// client confirms first when the full cargo (extra + base) holds 2 of either item
// (TrainData.lua:839-867), so such a truck is kept, and so is one whose goods can't be judged.
func TestTruckRerollSkipsHighOrUnreadableGoods(t *testing.T) {
	iron := trGood{typ: 1, num: 120000}
	for name, tc := range map[string]struct {
		base, extra *sfs.SFSObject // nil: removed
		want        bool
	}{
		"low goods":               {trGoods(iron, trGood{7, "2270000", 1}, trGood{7, "500105", 1}), trGoods(), true},
		"2270000 x2":              {trGoods(iron, trGood{7, "2270000", 2}), trGoods(), false},
		"500105 x3":               {trGoods(iron, trGood{7, "500105", 3}), trGoods(), false},
		"500105 split base/extra": {trGoods(iron, trGood{7, "500105", 1}), trGoods(trGood{7, "500105", 1}), false},
		"other items x9":          {trGoods(iron, trGood{7, "2270001", 9}, trGood{7, "500106", 9}), trGoods(trGood{7, "200101", 9}), true},
		"no extraGoods":           {trGoods(iron), nil, false},
		"no baseGoods":            {nil, trGoods(iron), false},
		"empty base cargo":        {trGoods(), trGoods(iron), false},
		"item without num":        {withItemNoNum(trGoods(iron)), trGoods(), false},
		"resource typed 500105":   {trGoods(iron, trGood{typ: 500105, num: 1}), trGoods(), false},
	} {
		t.Run(name, func(t *testing.T) {
			tr := trTruckWith(11, 30, 1, 0, tc.base, tc.extra)
			got := trRunOK(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, tr))
			if (len(got) == 1) != tc.want {
				t.Errorf("rerolled %v, want rerolled %v", got, tc.want)
			}
		})
	}
}

// withItemNoNum appends an item entry {type 7, value {id 200101}} with no num to g's full list.
func withItemNoNum(g *sfs.SFSObject) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutInt("type", 7)
	v := sfs.NewSFSObject()
	v.PutUtfString("id", "200101")
	e.PutSFSObject("value", v)
	arr := sfs.NewSFSArray()
	for _, o := range claimObjects(g, "full") {
		arr.AddSFSObject(o)
	}
	arr.AddSFSObject(e)
	g.PutSFSArray("full", arr)
	return g
}

// TestTruckRerollGates: the departure view opens only for a Ready truck at home
// (LWMyStationDataManager.lua:750-777; RailwayUtil.lua:66-72), and only the account's own trucks
// are touched.
func TestTruckRerollGates(t *testing.T) {
	parked := func() *sfs.SFSObject { return trTruck(11, 30, 1, 0) }
	for name, tc := range map[string]struct {
		in   *Init
		want int
	}{
		"ready":         {plTestInit(func(*sfs.SFSObject) {}, 3, 0, parked()), 1},
		"no sends left": {plTestInit(func(*sfs.SFSObject) {}, 4, 0, parked()), 0},
		"effect 90103 adds a send": {plTestInit(func(raw *sfs.SFSObject) {
			e := sfs.NewSFSObject()
			e.PutInt("90103", 1)
			raw.PutSFSObject("effect", e)
		}, 4, 0, parked()), 1},
		"still fogged": {plTestInit(func(raw *sfs.SFSObject) { railwayStage(raw, railwayFogStage-2) }, 0, 0, parked()), 0},
		"in another warzone": {plTestInit(func(raw *sfs.SFSObject) {
			u := sfs.NewSFSObject()
			u.PutInt("serverId", plTestOther)
			u.PutInt("crossFightSrcServerId", plTestHome)
			raw.PutSFSObject("user", u)
		}, 0, 0, parked()), 0},
		"out on the road": {plTestInit(func(*sfs.SFSObject) {}, 0, 0, func() *sfs.SFSObject {
			tr := parked()
			tr.PutLong("sendTime", time.Now().Add(-time.Hour).UnixMilli())
			return tr
		}()), 0},
		"own ownerId": {plTestInit(func(*sfs.SFSObject) {}, 0, 0, func() *sfs.SFSObject {
			tr := parked()
			tr.PutUtfString("ownerId", "77")
			return tr
		}()), 1},
		"another owner": {plTestInit(func(*sfs.SFSObject) {}, 0, 0, func() *sfs.SFSObject {
			tr := parked()
			tr.PutUtfString("ownerId", "78")
			return tr
		}()), 0},
		"uuid listed twice": {plTestInit(func(*sfs.SFSObject) {}, 0, 0, parked(), trTruck(11, 30, 2, 0)), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if got := trRunOK(t, tc.in); len(got) != tc.want {
				t.Errorf("rerolled %v, want %d trucks", got, tc.want)
			}
		})
	}

	// The fresh train.data reply wins over init: there the free reroll is used up.
	st := sfs.NewSFSObject()
	st.PutInt("openReward", 1)
	st.PutLong("openTime", time.Now().Add(-48*time.Hour).UnixMilli())
	st.PutInt("dailySendCount", 0)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(trTruck(11, 30, 1, 1))
	st.PutSFSArray("myTrainList", arr)
	f, _, err := trRun(t, plTestInit(func(*sfs.SFSObject) {}, 0, 0, trTruck(11, 30, 1, 0)), trFake{station: st})
	if got := trChanged(t, f); err != nil || len(got) != 0 {
		t.Errorf("run = %v, rerolled %v; want nothing once train.data shows the free reroll used", err, got)
	}
}

// TestTruckRerollOncePerTruckAndStops: each eligible truck gets one train.change, a second apart,
// and the reply's new truck is logged; an errorCode, a reply without myTrain or a dead connection
// stops the run with an error.
func TestTruckRerollOncePerTruckAndStops(t *testing.T) {
	trucks := func() *Init {
		return plTestInit(func(*sfs.SFSObject) {}, 0, 0, trTruck(11, 30, 1, 0), trTruck(12, 61, 2, 0), trTruck(13, 4031, 3, 0))
	}
	logs := captureRewardLogs(t)
	f, slept, err := trRun(t, trucks(), trFake{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := trChanged(t, f); !slices.Equal(got, []int64{11, 12}) {
		t.Errorf("rerolled %v, want 11 and 12 once each (13 is SSR)", got)
	}
	if !slices.Equal(slept, []time.Duration{time.Second}) {
		t.Errorf("slept %v between rerolls, want one 1s pause", slept)
	}
	if s := logs.String(); !strings.Contains(s, "truck rerolled") || !strings.Contains(s, "newCfgId=130") ||
		!strings.Contains(s, "newQuality=5") || !strings.Contains(s, "newUuid=1011") {
		t.Errorf("logs lack the new truck's uuid, cfgId and quality:\n%s", s)
	}

	for name, reply := range map[string]func(n int, uuid int64) *sfs.SFSObject{
		"errorCode":  func(int, int64) *sfs.SFSObject { return rewardErr("E120001") },
		"no myTrain": func(int, int64) *sfs.SFSObject { return sfs.NewSFSObject() },
		"dead":       func(int, int64) *sfs.SFSObject { return nil },
	} {
		t.Run(name, func(t *testing.T) {
			f, _, err := trRun(t, trucks(), trFake{change: reply})
			if got := trChanged(t, f); err == nil || !slices.Equal(got, []int64{11}) {
				t.Errorf("run = %v after %v; want one train.change and an error", err, got)
			}
		})
	}
}

func TestTruckRerollIsOptInAndStaticOnly(t *testing.T) {
	f, ok := featureRegistry["truck-reroll"]
	if !ok {
		t.Fatal("truck-reroll is not registered")
	}
	if f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("DefaultOn %v, summary %q; want opt-in and static-only", f.DefaultOn, f.Summary)
	}
	if len(f.Duel) != 0 || f.Hold {
		t.Errorf("duel %v, hold %v; a reroll scores in no duel row", f.Duel, f.Hold)
	}
}
