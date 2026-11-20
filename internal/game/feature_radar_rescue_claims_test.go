package game

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// rescueEv is a FINISHED rescue event (detect_event 199, type 11) whose reward carries RESOURCE_ITEM
// entries {id: soldier item, num: count}, as ReturnRewardParamForView reads them.
func rescueEv(uuid int64, soldiers map[int32]int64) *sfs.SFSObject {
	ev := detectEv(uuid, 199, radarStateFinished)
	rw := sfs.NewSFSArray()
	for _, id := range slices.Sorted(func(yield func(int32) bool) {
		for k := range soldiers {
			if !yield(k) {
				return
			}
		}
	}) {
		v := sfs.NewSFSObject()
		v.PutInt("id", id)
		v.PutLong("num", soldiers[id])
		e := sfs.NewSFSObject()
		e.PutInt("type", rewardTypeResourceItem)
		e.PutSFSObject("value", v)
		rw.AddSFSObject(e)
	}
	ev.PutSFSArray("reward", rw)
	return ev
}

// rescueInit is an init with the soldier stock cap (effect 30122) and held type-1 soldiers.
func rescueInit(stockCap float64, held int64) *Init {
	in := evInit(30)
	eff := sfs.NewSFSObject()
	eff.PutDouble("30122", stockCap)
	in.Raw.PutSFSObject("effect", eff)
	items := sfs.NewSFSArray()
	s := sfs.NewSFSObject()
	s.PutInt("itemId", 3010)
	s.PutLong("number", held)
	items.AddSFSObject(s)
	in.Raw.PutSFSArray("resource_items", items)
	return in
}

func TestRadarRescueSoldiers(t *testing.T) {
	if n, ok := radarRescueSoldiers(rescueEv(1, map[int32]int64{3010: 40, 4005: 20, 12011: 5})); !ok || n != 65 {
		t.Errorf("sum = %d, %v; want 65 over three lw_soldier ids", n, ok)
	}
	notSoldier := rescueEv(1, map[int32]int64{4003: 10}) // no lw_soldier row 4003
	if _, ok := radarRescueSoldiers(notSoldier); ok {
		t.Error("a reward with no soldier entry must not read as claimable")
	}
	noCount := detectEv(1, 199, radarStateFinished)
	v := sfs.NewSFSObject()
	v.PutInt("itemId", 3010)
	e := sfs.NewSFSObject()
	e.PutInt("type", rewardTypeResourceItem)
	e.PutSFSObject("value", v)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(e)
	noCount.PutSFSArray("reward", arr)
	if _, ok := radarRescueSoldiers(noCount); ok {
		t.Error("a soldier entry without num or add must not read as claimable")
	}
	if _, ok := radarRescueSoldiers(detectEv(1, 199, radarStateFinished)); ok {
		t.Error("an event without reward must not read as claimable")
	}
	if len(radarSoldierIDs) != 46 {
		t.Errorf("lw_soldier ids = %d, want 46 (tables 40413)", len(radarSoldierIDs))
	}
}

// TestRadarRescueClaimsWhatFits: the client claims a finished rescue when its soldiers fit
// LW_SOLDIER_MAX_STOCK minus the held Player soldiers (RadarCenterDataManager.lua:1281-1313) and
// asks first otherwise; each claim uses up room.
func TestRadarRescueClaimsWhatFits(t *testing.T) {
	events := []*sfs.SFSObject{
		rescueEv(11, map[int32]int64{3010: 300}),           // fits: room 500
		rescueEv(12, map[int32]int64{3010: 150, 4005: 60}), // 210 > the 200 left: the client asks
		rescueEv(13, map[int32]int64{3010: 200}),           // fits the 200 left
		detectEv(14, 100, radarStateFinished),              // not a rescue: radar-claims' job
		rescueEv(15, map[int32]int64{3010: 1}),             // not FINISHED: skipped
	}
	events[4].PutInt("state", radarStateNotFinish)
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, events...)}, nil)
	if err := runRadarRescueClaims(conn, rescueInit(1000, 500)); err != nil {
		t.Fatal(err)
	}
	if got := radarUUIDs(f, radarEventCmd); !slices.Equal(got, []int64{11, 13}) {
		t.Errorf("claimed %v, want 11 and 13", got)
	}
	for _, c := range f.cmds() {
		if c != radarInfoCmd && c != radarEventCmd {
			t.Errorf("sent %s", c)
		}
	}
}

// TestRadarRescueCountsWhatFinishedTimersCollectsFirst: finished-timers runs earlier in the run
// and adds ready Military Camp output and healed troops to the stock, which init does not show.
func TestRadarRescueCountsWhatFinishedTimersCollectsFirst(t *testing.T) {
	past := evTestNow.Add(-time.Minute).UnixMilli()
	camp := func(prodBase int32) *sfs.SFSObject {
		b := sfs.NewSFSObject()
		b.PutLong("uuid", 90)
		b.PutInt("bId", finishedTimersMilitaryCamp)
		b.PutInt("lv", 3)
		b.PutLong("prodST", evTestNow.Add(-time.Hour).UnixMilli())
		b.PutLong("prodET", past)
		b.PutInt("prodBase", prodBase)
		return b
	}
	hospital := sfs.NewSFSObject()
	hospital.PutLong("uuid", 91)
	hospital.PutInt("type", finishedTimersQueueHospital)
	hospital.PutLong("startTime", evTestNow.Add(-time.Hour).UnixMilli())
	hospital.PutLong("updateTime", past)
	for _, tc := range []struct {
		name     string
		building *sfs.SFSObject
		queue    *sfs.SFSObject
		want     []int64
	}{
		{"nothing pending", nil, nil, []int64{11}},
		{"camp output ready", camp(300), nil, nil}, // 400 > 500 - 300
		{"small camp output", camp(50), nil, []int64{11}},
		{"hospital queue finished", nil, hospital, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn, f := startRadarFake(t, radarScoringDay(),
				[]*sfs.SFSObject{detectReply(20, 10, rescueEv(11, map[int32]int64{3010: 400}))}, nil)
			in := rescueInit(1000, 500)
			if tc.building != nil {
				arr := sfs.NewSFSArray()
				arr.AddSFSObject(tc.building)
				in.Raw.PutSFSArray("building_new", arr)
			}
			if tc.queue != nil {
				arr := sfs.NewSFSArray()
				arr.AddSFSObject(tc.queue)
				in.Raw.PutSFSArray("queue_new", arr)
			}
			if err := runRadarRescueClaims(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := radarUUIDs(f, radarEventCmd); !slices.Equal(got, tc.want) {
				t.Errorf("claimed %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRadarSoldierIDsMatchTheTable pins radarSoldierIDs against lw_soldier in the table dump.
// Skipped without LASTWAR_TABLES.
func TestRadarSoldierIDsMatchTheTable(t *testing.T) {
	dir := os.Getenv("LASTWAR_TABLES")
	if dir == "" {
		t.Skip("LASTWAR_TABLES is not set")
	}
	data, err := os.ReadFile(filepath.Join(dir, "lw_soldier.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		Rows [][]json.RawMessage `json:"rows"`
	}
	if err := json.Unmarshal(data, &tab); err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, row := range tab.Rows {
		var id int64
		if len(row) == 0 || json.Unmarshal(row[0], &id) != nil {
			t.Fatalf("bad row key in %s", row)
		}
		got[id] = true
	}
	if !maps.Equal(got, radarSoldierIDs) {
		t.Errorf("lw_soldier ids %v, radarSoldierIDs %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(radarSoldierIDs)))
	}
}

func TestRadarRescueNeedsTheStock(t *testing.T) {
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, rescueEv(11, map[int32]int64{3010: 1}))}, nil)
	if err := runRadarRescueClaims(conn, evInit(30)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.only(radarEventCmd)); n != 0 {
		t.Errorf("claimed %d rescues without a known stock cap", n)
	}
}

func TestRadarRescueFeatureIsOptInAndHeld(t *testing.T) {
	f, ok := featureRegistry[radarRescueFeature]
	if !ok {
		t.Fatal("radar-rescue-claims not registered")
	}
	if f.DefaultOn || !f.Hold || len(f.Duel) != 1 || f.Duel[0].Type != DuelScoreRadarTask {
		t.Errorf("DefaultOn=%v Hold=%v Duel=%v, want opt-in and held for radar-task days", f.DefaultOn, f.Hold, f.Duel)
	}
}
