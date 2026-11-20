package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// citySoldiersInit is an HQ 25 init with the given building_new entries ({uuid, bId, lv,
// prodBase}; a Military Camp's training has ended), the troop stock (cap 0: no effect["30122"]; soldiers < 0: no resource_items), init
// sparkUpgrade (< 0: absent) and init items {itemId: count}.
func citySoldiersInit(builds [][4]int64, stockCap float64, soldiers, sparkLevel int64, items map[int64]int64) *Init {
	return rewardInitWithDay(25, func(raw *sfs.SFSObject) {
		v, _ := raw.Get("building_new")
		arr := v.Val.(*sfs.SFSArray)
		for _, b := range builds {
			o := NewTestBuildingSFS(b[0], int32(b[1]), int32(b[2]))
			o.PutInt("prodBase", int32(b[3]))
			if b[1] == finishedTimersMilitaryCamp { // training finished a minute ago
				o.PutLong("prodET", time.Now().Add(-time.Minute).UnixMilli())
			}
			arr.AddSFSObject(o)
		}
		if stockCap > 0 {
			eff := sfs.NewSFSObject()
			eff.PutDouble("30122", stockCap)
			raw.PutSFSObject("effect", eff)
		}
		if soldiers >= 0 {
			ri := sfs.NewSFSArray()
			ri.AddSFSObject(obj("itemId", 3005, "number", soldiers))
			raw.PutSFSArray("resource_items", ri)
		}
		if sparkLevel >= 0 {
			raw.PutInt("sparkUpgrade", int32(sparkLevel))
		}
		its := sfs.NewSFSArray()
		for id, n := range items {
			its.AddSFSObject(obj("uuid", id*10, "itemId", int(id), "count", int(n)))
		}
		raw.PutSFSArray("items", its)
	})
}

func citySoldiersCmds(t *testing.T, in *Init, reply func(string, *sfs.SFSObject) *sfs.SFSObject) ([]string, error) {
	t.Helper()
	conn, fake := startRewardFake(t, reply)
	err := runCitySoldiers(conn, in)
	for _, r := range fake.requests() {
		if keys := session.ParamKeysWithoutID(r.Params); len(keys) != 0 {
			t.Errorf("%s sent params %v, want none", r.Cmd, keys)
		}
	}
	return fake.cmds(), err
}

func citySoldiersOK(string, *sfs.SFSObject) *sfs.SFSObject {
	return obj("soldierItemId", "3010", "soldierNum", 120)
}

func TestCitySoldiersIsOptIn(t *testing.T) { assertCityGapFeature(t, citySoldiersFeature) }

func TestCitySoldiersClaims(t *testing.T) {
	stage := [4]int64{1, citySoldiersCountBattle, 3, 120}
	spark := [4]int64{2, citySoldiersSpark, 1, 80}
	yard := [4]int64{3, citySoldiersArmyYard, 5, 0}
	both := []string{citySoldiersStageCmd, citySoldiersSparkCmd}
	cases := []struct {
		name       string
		builds     [][4]int64
		cap        float64
		soldiers   int64
		sparkLevel int64
		items      map[int64]int64
		want       []string
	}{
		{"both waiting", [][4]int64{stage, spark, yard}, 10_000, 1_000, -1, nil, both},
		{"nothing produced", [][4]int64{{1, citySoldiersCountBattle, 3, 0}, {2, citySoldiersSpark, 1, 0}, yard}, 10_000, 1_000, -1, nil, nil},
		{"count battle at level 0", [][4]int64{{1, citySoldiersCountBattle, 0, 120}}, 10_000, 1_000, -1, nil, nil},
		{"no Drill Ground", [][4]int64{spark}, 10_000, 1_000, -1, nil, nil},
		{"Drill Ground at level 0", [][4]int64{spark, {3, citySoldiersArmyYard, 0, 0}}, 10_000, 1_000, -1, nil, nil},
		{"spark upgradable: item held", [][4]int64{stage, spark, yard}, 10_000, 1_000, 2, map[int64]int64{666702: 1}, []string{citySoldiersStageCmd}},
		{"spark level 2, other level's item", [][4]int64{spark, yard}, 10_000, 1_000, 2, map[int64]int64{666701: 3}, []string{citySoldiersSparkCmd}},
		{"spark at max level", [][4]int64{spark, yard}, 10_000, 1_000, 6, map[int64]int64{666706: 1}, []string{citySoldiersSparkCmd}},
		{"stock unknown", [][4]int64{stage, spark, yard}, 0, 1_000, -1, nil, nil},
		{"stock full", [][4]int64{stage, spark, yard}, 1_000, 1_000, -1, nil, nil},
		// The stage claim takes the stock to the cap; the spark claim is left.
		{"room for one", [][4]int64{stage, spark, yard}, 1_120, 1_000, -1, nil, []string{citySoldiersStageCmd}},
		// The stock is below its cap, but the 1500 waiting are more than the whole cap of 1000.
		{"more than the whole stock", [][4]int64{{1, citySoldiersCountBattle, 3, 1_500}}, 1_000, 0, -1, nil, nil},
		// A finished Military Camp counts as taken first (finished-timers runs earlier).
		{"camp output fills the room", [][4]int64{stage, yard, {4, finishedTimersMilitaryCamp, 10, 500}}, 1_400, 1_000, -1, nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := citySoldiersInit(c.builds, c.cap, c.soldiers, c.sparkLevel, c.items)
			got, err := citySoldiersCmds(t, in, citySoldiersOK)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("sent %v, want %v", got, c.want)
			}
		})
	}
}

// TestCitySoldiersSparkItemFieldNames: CheckCanUpgrade compares the upgrade item's
// ItemData:GetItemCount, the stack's ItemInfo.count (LWCivilizationSparkManager.lua:165-177;
// ItemData.lua:596-599). ItemInfo:UpdateInfo names the item by itemId or goodsId and sets count
// from count, then num, then itemLeftCnt, so the last of them present wins (ItemInfo.lua:43-66). A
// spark at level 2 whose upgrade item 666702 is held can be upgraded: the client shows the upgrade
// bubble, not the soldiers, and the soldiers are left.
func TestCitySoldiersSparkItemFieldNames(t *testing.T) {
	for _, c := range []struct {
		name  string
		item  *sfs.SFSObject
		claim bool
	}{
		{"itemId, count 1", obj("uuid", int64(1), "itemId", 666702, "count", 1), false},
		{"goodsId, num 1", obj("uuid", int64(1), "goodsId", 666702, "num", 1), false},
		{"itemId, itemLeftCnt 1", obj("uuid", int64(1), "itemId", 666702, "itemLeftCnt", 1), false},
		{"count 0, itemLeftCnt 1", obj("uuid", int64(1), "itemId", 666702, "count", 0, "itemLeftCnt", 1), false},
		{"count 0: none held", obj("uuid", int64(1), "itemId", 666702, "count", 0), true},
		{"count 1, itemLeftCnt 0: none left", obj("uuid", int64(1), "itemId", 666702, "count", 1, "itemLeftCnt", 0), true},
	} {
		in := citySoldiersInit([][4]int64{{2, 10230000, 1, 80}, {3, 10104000, 5, 0}}, 10_000, 1_000, 2, nil)
		arr := sfs.NewSFSArray()
		arr.AddSFSObject(c.item)
		in.Raw.PutSFSArray("items", arr)
		got, err := citySoldiersCmds(t, in, citySoldiersOK)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		var want []string
		if c.claim {
			want = []string{"lw.claim.spark.soldier"}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: sent %v, want %v", c.name, got, want)
		}
	}
}

// An errorCode from either never-sent claim is a failure (no benign code is registered); the other
// claim still goes out.
func TestCitySoldiersErrorIsReported(t *testing.T) {
	in := citySoldiersInit([][4]int64{{1, citySoldiersCountBattle, 3, 120}, {2, citySoldiersSpark, 1, 80}, {3, citySoldiersArmyYard, 5, 0}},
		10_000, 1_000, -1, nil)
	got, err := citySoldiersCmds(t, in, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == citySoldiersStageCmd {
			return rewardErr("E000000")
		}
		return citySoldiersOK(cmd, p)
	})
	if err == nil {
		t.Error("run = nil, want the stage claim's errorCode")
	}
	if want := []string{citySoldiersStageCmd, citySoldiersSparkCmd}; !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}
