package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// productionLineIDs is every building type collectCmdFor collects.
var productionLineIDs = []int32{
	BuildingFarmland, BuildingIronMine, BuildingGoldMine, BuildingSmelter, BuildingMaterialWorkshop, BuildingTrainingBase,
	BuildingOilWell, BuildingDronePartsShop, BuildingComponentFactory, BuildingTacticalInstitute, BuildingCrystalFactory,
	BuildingSporeFactory1, BuildingSporeFactory2, BuildingSporeFactory3, BuildingSporeFactory4, BuildingSporeFactoryCard,
	BuildingSeason1Farm1, BuildingSeason1Farm2, BuildingSeason1Farm3, BuildingSeason1FarmCard, BuildingSeason1Farm5,
	BuildingSeason2Farm1, BuildingSeason2Farm2, BuildingSeason2Farm3, BuildingSeason2Farm4, BuildingSeason2FarmCard,
	BuildingBlessingFountain1, BuildingBlessingFountain2, BuildingBlessingFountain3, BuildingBlessingFountain4, BuildingBlessingFountainWC,
	BuildingSeason4Quartz1, BuildingSeason4Quartz2, BuildingSeason4Quartz3, BuildingSeason4Quartz4, BuildingSeason4QuartzCard,
	BuildingSeason5City1, BuildingSeason5City2, BuildingSeason5City3, BuildingSeason5City4, BuildingSeason5CityCard,
}

// TestCollectCmdForSeasonFamilies checks every ProductLineBuilds/season_weekcard family is
// collected, has a name, and has level-1 production data in the generated table.
func TestCollectCmdForSeasonFamilies(t *testing.T) {
	for _, id := range productionLineIDs {
		if cmd, ok := collectCmdFor(id); !ok || cmd != "building.production.collect" {
			t.Errorf("collectCmdFor(%d) = (%q, %v), want building.production.collect", id, cmd, ok)
		}
		if _, ok := buildingNames[id]; !ok {
			t.Errorf("buildingNames has no entry for %d", id)
		}
		if lvl, ok := productionTable[id+1]; !ok || lvl.tickMs <= 0 {
			t.Errorf("productionTable[%d] (level 1) = %+v, %v; want a production tick", id+1, lvl, ok)
		}
	}
	for id := range seasonWeekCardBuildings {
		if !slices.Contains(productionLineIDs, id) {
			t.Errorf("week-card building %d is not collected", id)
		}
	}
}

// TestProductionTableSpotValues pins a few rows of the generated table against the values the
// tables report lists for table 39432.
func TestProductionTableSpotValues(t *testing.T) {
	cases := []struct {
		id   int32
		want productionLevel
	}{
		{BuildingFarmland + 1, productionLevel{tickMs: 5000, capS: 29280}},
		{BuildingSmelter + 1, productionLevel{tickMs: 1800000, capS: 29280}},
		{BuildingOilWell + 1, productionLevel{tickMs: 60000, capS: 29280}},
		{BuildingComponentFactory + 1, productionLevel{tickMs: 14400000, capS: 86400}},
		{BuildingDronePartsShop + 1, productionLevel{tickMs: 21360000, capS: 86400}},
		{BuildingCrystalFactory + 1, productionLevel{tickMs: 10800000, capS: 172800}},
		{BuildingSporeFactoryCard + 1, productionLevel{tickMs: 5000, capS: 86400}},
	}
	for _, c := range cases {
		if got := productionTable[c.id]; got != c.want {
			t.Errorf("productionTable[%d] = %+v, want %+v", c.id, got, c.want)
		}
	}
}

func TestProductionReady(t *testing.T) {
	now := time.UnixMilli(1_800_000_000_000)
	const tick = 30 * time.Minute // Smelter
	ms := func(d time.Duration) int64 { return now.Add(d).UnixMilli() }
	smelter := func(fields ...any) Building {
		o := obj(append([]any{"uuid", int64(1), "bId", int(BuildingSmelter), "lv", 5}, fields...)...)
		return Building{Raw: o}
	}
	cases := []struct {
		name  string
		b     Building
		ready bool
		next  time.Time
	}{
		{"unknown building type", Building{Raw: obj("bId", 99999999, "lv", 1, "prodT", ms(0), "prodET", ms(time.Hour))}, true, time.Time{}},
		{"unknown level", smelter("lv", 999, "prodT", ms(0), "prodET", ms(time.Hour)), true, time.Time{}},
		{"tick not complete", smelter("prodST", ms(-time.Minute), "prodT", ms(-time.Minute), "prodET", ms(time.Hour)), false, now.Add(tick - time.Minute)},
		{"tick complete", smelter("prodST", ms(-tick), "prodT", ms(-tick), "prodET", ms(time.Hour)), true, time.Time{}},
		// prodT 45 min after prodST sits mid-tick; aligned down to the 30-minute grid, a full tick
		// has passed since then.
		{"prodT aligned to the tick grid", smelter("prodST", ms(-time.Hour), "prodT", ms(-15*time.Minute), "prodET", ms(time.Hour)), true, time.Time{}},
		{"no prodST: no alignment", smelter("prodT", ms(-15*time.Minute), "prodET", ms(time.Hour)), false, now.Add(tick - 15*time.Minute)},
		{"stored output (Double)", smelter("prodExtend", 0.5, "prodST", ms(-time.Minute), "prodT", ms(-time.Minute), "prodET", ms(time.Hour)), true, time.Time{}},
		{"zero stored output", smelter("prodExtend", 0, "prodST", ms(-time.Minute), "prodT", ms(-time.Minute), "prodET", ms(time.Hour)), false, now.Add(tick - time.Minute)},
		{"unreadable prodExtend", smelter("prodExtend", "x", "prodST", ms(-time.Minute), "prodT", ms(-time.Minute), "prodET", ms(time.Hour)), true, time.Time{}},
		{"missing prodT", smelter("prodST", ms(-time.Minute), "prodET", ms(time.Hour)), true, time.Time{}},
		{"missing prodET", smelter("prodST", ms(-time.Minute), "prodT", ms(-time.Minute)), true, time.Time{}},
		{"line full with a partial tick left", smelter("prodST", ms(-time.Hour), "prodT", ms(-2*time.Minute), "prodET", ms(-time.Minute)), true, time.Time{}},
		{"line full and fully accounted", smelter("prodST", ms(-time.Hour), "prodT", ms(-time.Minute), "prodET", ms(-time.Minute)), false, time.Time{}},
	}
	for _, c := range cases {
		ready, next := productionReady(c.b, now)
		if ready != c.ready || !next.Equal(c.next) {
			t.Errorf("%s: productionReady = (%v, %v), want (%v, %v)", c.name, ready, next.UTC(), c.ready, c.next.UTC())
		}
	}
}

func TestReadyBuildingsWeekCardSettleGuard(t *testing.T) {
	now := time.Now()
	weekCard := productionBuilding(1, BuildingSporeFactoryCard, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	tier := productionBuilding(2, BuildingSporeFactory4, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	s1Card := productionBuilding(3, BuildingSeason1FarmCard, 1, now.Add(-time.Hour), now.Add(-time.Minute), now.Add(time.Hour))
	all := []Building{weekCard, tier, s1Card}
	season := func(settle any) *Init {
		if settle == nil {
			return &Init{Raw: obj("playerServerSeasonInfo", obj("seasonId", 6))}
		}
		return &Init{Raw: obj("playerServerSeasonInfo", obj("seasonSettleTime", settle))}
	}
	cases := []struct {
		name string
		in   *Init
		want []int64
	}{
		{"settled", season(now.Add(-time.Second).UnixMilli()), []int64{2}},
		{"not settled yet", season(now.Add(time.Hour).UnixMilli()), []int64{1, 2, 3}},
		{"no settle time", season(nil), []int64{1, 2, 3}},
		{"no season info", &Init{Raw: sfs.NewSFSObject()}, []int64{1, 2, 3}},
	}
	for _, c := range cases {
		var got []int64
		for _, b := range readyBuildings(all, c.in, now) {
			got = append(got, b.Uuid())
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: ready uuids = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSFSFieldHelpers(t *testing.T) {
	o := obj("s", "42", "i", 7, "l", int64(1<<40), "d", 1.5, "o", obj("x", 1), "a", []*sfs.SFSObject{obj("x", 1), obj("x", 2)})
	if got := idString(o, "s"); got != "42" {
		t.Errorf("idString(string) = %q", got)
	}
	if got := idString(o, "l"); got != "1099511627776" {
		t.Errorf("idString(long) = %q", got)
	}
	if got := idString(o, "d"); got != "" {
		t.Errorf("idString(double) = %q, want empty", got)
	}
	if n, ok := numberField(o, "d"); !ok || n != 1.5 {
		t.Errorf("numberField(double) = %v, %v", n, ok)
	}
	if _, ok := numberField(o, "s"); ok {
		t.Error("numberField(string) reported a number")
	}
	if objectField(o, "o").GetInt("x") != 1 || objectField(o, "i") != nil || objectField(nil, "o") != nil {
		t.Error("objectField")
	}
	if len(objectsField(o, "a")) != 2 || objectsField(o, "o") != nil || objectsField(nil, "a") != nil {
		t.Error("objectsField")
	}
}
