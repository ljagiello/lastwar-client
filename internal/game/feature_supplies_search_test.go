package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// suppliesInfo is a suppliesSearchInfo {cfgId, curLevel, isFail}.
func suppliesInfo(level, fail int32) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutInt("cfgId", 1001)
	o.PutInt("curLevel", level)
	o.PutInt("isFail", fail)
	return o
}

// suppliesEv is a radar event on the map (state 0) carrying a supplies search.
func suppliesEv(uuid int64, state int64, info *sfs.SFSObject) *sfs.SFSObject {
	ev := detectEv(uuid, 100, state)
	ev.PutSFSObject("suppliesSearchInfo", info)
	return ev
}

// suppliesReply answers the step with the level it reached, as SuppliesSearchManager reads it.
func suppliesReply(key string, uuid int64, level, fail int32) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutLong(key, uuid)
	r.PutSFSObject("suppliesSearchInfo", suppliesInfo(level, fail))
	return r
}

// suppliesSent lists "cmd uuid" for every supplies-search request, in order.
func suppliesSent(f *evFake) []string {
	var out []string
	for _, r := range f.requests() {
		switch r.Cmd {
		case suppliesRadarNextCmd, suppliesRadarRewardCmd:
			out = append(out, fmt.Sprintf("%s %d", r.Cmd, r.Params.GetLong("eventUuid")))
		case suppliesBuildNextCmd, suppliesBuildRewardCmd:
			out = append(out, fmt.Sprintf("%s %d", r.Cmd, r.Params.GetLong("buildingUuid")))
		}
	}
	return out
}

func suppliesBuilding(uuid int64, bid int32, info *sfs.SFSObject) Building {
	b := NewTestBuilding(uuid, bid, 1)
	if info != nil {
		b.Raw.PutSFSObject("suppliesSearchInfo", info)
	}
	return b
}

// TestSuppliesSearchTakesOnlyTheRiskFreeStep: one step from level 0 (level 1's rate is 100 % in
// every monopoly_search row), then the reward; never a second step, never a search already
// started or failed, never an event off the map.
func TestSuppliesSearchTakesOnlyTheRiskFreeStep(t *testing.T) {
	events := []*sfs.SFSObject{
		suppliesEv(201, radarStateNotFinish, suppliesInfo(0, 0)),  // fresh: step and reward
		suppliesEv(202, radarStateNotInWorld, suppliesInfo(0, 0)), // not on the map: no placement
		suppliesEv(203, radarStateNotFinish, suppliesInfo(2, 0)),  // the player went further: theirs
		suppliesEv(204, radarStateNotFinish, suppliesInfo(1, 1)),  // failed: theirs
		suppliesEv(205, radarStateNotFinish, suppliesInfo(0, 0)),  // the step comes back failed
		detectEv(206, 100, radarStateNotFinish),                   // no search at all
	}
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, events...)},
		func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			switch cmd {
			case suppliesRadarNextCmd:
				if p.GetLong("eventUuid") == 205 {
					return suppliesReply("eventUuid", 205, 1, 1)
				}
				return suppliesReply("eventUuid", p.GetLong("eventUuid"), 1, 0)
			case suppliesBuildNextCmd:
				return suppliesReply("buildingUuid", p.GetLong("buildingUuid"), 1, 0)
			}
			return nil
		})
	in := digTestInit(nil)
	in.Buildings = append(in.Buildings,
		suppliesBuilding(301, 10150000, suppliesInfo(0, 0)),
		suppliesBuilding(302, 10152000, suppliesInfo(1, 0)), // already at level 1: the player's call
		suppliesBuilding(303, 10226000, suppliesInfo(0, 0)), // not a supplies-search building
	)
	if err := runSuppliesSearch(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{
		suppliesBuildNextCmd + " 301", suppliesBuildRewardCmd + " 301",
		suppliesRadarNextCmd + " 201", suppliesRadarRewardCmd + " 201",
		suppliesRadarNextCmd + " 205",
	}
	if got := suppliesSent(f); !slices.Equal(got, want) {
		t.Errorf("sent\n%v\nwant\n%v", got, want)
	}
	// Each request carries its uuid as an SFS Long, as the client puts it
	// (Net/Msgs/SuppliesSearch/*.lua:6, PutLong eventUuid / buildingUuid).
	for _, r := range f.requests() {
		switch r.Cmd {
		case "detect.event.supplies.search.next", "detect.event.get.supplies.search.reward":
			seasonParamIs(t, r.Params, "eventUuid", sfs.SFSLong, r.Params.GetLong("eventUuid"))
		case "building.supplies.search.next", "building.get.supplies.search.reward":
			seasonParamIs(t, r.Params, "buildingUuid", sfs.SFSLong, r.Params.GetLong("buildingUuid"))
		}
	}
}

// TestSuppliesSearchRadarHeldOffDuelRadarDays: a radar search completes a radar task (score 82),
// so it waits for a radar-scoring day; the building form does not.
func TestSuppliesSearchRadarHeldOffDuelRadarDays(t *testing.T) {
	conn, f := startRadarFake(t, radarOffDay(),
		[]*sfs.SFSObject{detectReply(20, 10, suppliesEv(201, radarStateNotFinish, suppliesInfo(0, 0)))},
		func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == suppliesBuildNextCmd {
				return suppliesReply("buildingUuid", p.GetLong("buildingUuid"), 1, 0)
			}
			return nil
		})
	in := digTestInit(nil)
	in.Buildings = append(in.Buildings, suppliesBuilding(301, 10151000, suppliesInfo(0, 0)))
	if err := runSuppliesSearch(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{suppliesBuildNextCmd + " 301", suppliesBuildRewardCmd + " 301"}
	if got := suppliesSent(f); !slices.Equal(got, want) {
		t.Errorf("sent %v, want only the building's step and reward", got)
	}
}

// TestSuppliesSearchDiamondDropTrips: the meaning of monopoly_search's win_diamond / fail_diamond
// is untraced, so a reply whose gold falls below init user.gold trips the tripwire and stops.
func TestSuppliesSearchDiamondDropTrips(t *testing.T) {
	path := tripwireTemp(t)
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10)},
		func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == suppliesBuildNextCmd {
				r := suppliesReply("buildingUuid", p.GetLong("buildingUuid"), 1, 0)
				r.PutLong("gold", 400)
				return r
			}
			return nil
		})
	in := digTestInit(nil)
	user := sfs.NewSFSObject()
	user.PutLong("gold", 500)
	in.Raw.PutSFSObject("user", user)
	in.Buildings = append(in.Buildings, suppliesBuilding(301, 10150000, suppliesInfo(0, 0)),
		suppliesBuilding(302, 10150000, suppliesInfo(0, 0)))
	if err := runSuppliesSearch(conn, in); err == nil {
		t.Fatal("want the diamond drop reported")
	}
	if got := suppliesSent(f); !slices.Equal(got, []string{suppliesBuildNextCmd + " 301"}) {
		t.Errorf("sent %v, want nothing after the drop", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("tripwire file: %v", err)
	}
	var rec TripwireRecord
	if err := json.Unmarshal(data, &rec); err != nil || rec.Feature != suppliesSearchFeature {
		t.Errorf("tripwire record = %+v (%v), want feature %s", rec, err, suppliesSearchFeature)
	}
}

// TestSuppliesSearchHonoursASetTripwire: with the tripwire file set (by this feature or any other
// spend, in an earlier run), nothing is sent, not even the risk-free step, and the run reports it.
func TestSuppliesSearchHonoursASetTripwire(t *testing.T) {
	path := tripwireTemp(t)
	tripwireTrip("duel-recruit-tickets", "lottery.hero.card", "an earlier run's diamond drop")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("tripwire file not written: %v", err)
	}
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10)},
		func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == suppliesBuildNextCmd {
				return suppliesReply("buildingUuid", p.GetLong("buildingUuid"), 1, 0)
			}
			return nil
		})
	in := digTestInit(nil)
	in.Buildings = append(in.Buildings, suppliesBuilding(301, 10150000, suppliesInfo(0, 0)))
	err := runSuppliesSearch(conn, in)
	if !errors.Is(err, errTripwire) {
		t.Errorf("run = %v, want the tripwire refusal", err)
	}
	if got := suppliesSent(f); len(got) != 0 {
		t.Errorf("sent %v while the tripwire file exists, want nothing", got)
	}
}

// TestSuppliesSearchRefusals: inputs whose search is not the risk-free step from a fresh level 0
// send no step, and a step that did not land on level 1 (or was refused) sends no reward. Each
// run's whole command list is checked, so a stray radar or duel read fails too.
func TestSuppliesSearchRefusals(t *testing.T) {
	expiresAt := func(uuid, end int64) *sfs.SFSObject {
		ev := suppliesEv(uuid, 0, suppliesInfo(0, 0))
		ev.PutLong("endTime", end)
		return ev
	}
	noLevel := sfs.NewSFSObject()
	noLevel.PutInt("cfgId", 1001)
	noLevel.PutInt("isFail", 0)
	for name, c := range map[string]struct {
		building *sfs.SFSObject // suppliesSearchInfo of building 301 (10150000), nil for none
		events   []*sfs.SFSObject
		step     *sfs.SFSObject // the building step's reply, nil for level 1
		want     string
		wantErr  bool
	}{
		// isFail 1 ends the search (UISuppliesSearchView.lua:125-200): not fresh even at level 0.
		"failed at level 0": {building: suppliesInfo(0, 1), want: "get.detect.info"},
		// A search without curLevel is not known to be at level 0.
		"no curLevel": {building: noLevel, want: "get.detect.info"},
		// A radar event is acted on by its uuid; 0 is no event.
		"radar event uuid 0": {events: []*sfs.SFSObject{suppliesEv(0, 0, suppliesInfo(0, 0))}, want: "get.detect.info"},
		// An event whose endTime has come is off the map.
		"radar event ending now":     {events: []*sfs.SFSObject{expiresAt(201, evTestNow.UnixMilli())}, want: "get.detect.info"},
		"radar event ended long ago": {events: []*sfs.SFSObject{expiresAt(201, 1)}, want: "get.detect.info"},
		// The reward is only for a step that reached level 1 (the next reply's curLevel,
		// SuppliesSearchManager.lua:65-79).
		"the step stays at level 0": {building: suppliesInfo(0, 0), step: suppliesReply("buildingUuid", 301, 0, 0),
			want: "building.supplies.search.next,get.detect.info"},
		// An errorCode (none is benign for these commands) is a refusal whatever else the reply
		// carries; the run reports it.
		"a refused step": {building: suppliesInfo(0, 0), step: func() *sfs.SFSObject {
			r := suppliesReply("buildingUuid", 301, 1, 0)
			r.PutUtfString("errorCode", "E8")
			return r
		}(), want: "building.supplies.search.next,get.detect.info", wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, c.events...)},
				func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
					if cmd == "building.supplies.search.next" && c.step != nil {
						return c.step
					}
					return dSuppliesStep(cmd, p)
				})
			in := digTestInit(nil)
			if c.building != nil {
				in.Buildings = append(in.Buildings, suppliesBuilding(301, 10150000, c.building))
			}
			if err := runSuppliesSearch(conn, in); (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(f.cmds(), ","); got != c.want {
				t.Errorf("sent %s\nwant %s", got, c.want)
			}
		})
	}
	t.Run("no init push", func(t *testing.T) {
		conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10)}, dSuppliesStep)
		for _, in := range []*Init{nil, {}} {
			if err := runSuppliesSearch(conn, in); err != nil {
				t.Fatal(err)
			}
		}
		if got := f.cmds(); len(got) != 0 {
			t.Errorf("sent %v without an init push", got)
		}
	})
}

// TestSuppliesSearchCapStopsTheRun: 21 fresh buildings get 20 searches (the feature's own cap of 20
// a run), and the cap ends the run before the radar is read.
func TestSuppliesSearchCapStopsTheRun(t *testing.T) {
	tripwireTemp(t)
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10)}, dSuppliesStep)
	in := digTestInit(nil)
	for i := range int64(21) {
		in.Buildings = append(in.Buildings, suppliesBuilding(400+i, 10150000, suppliesInfo(0, 0)))
	}
	if err := runSuppliesSearch(conn, in); err != nil {
		t.Fatal(err)
	}
	if n := len(f.only("building.supplies.search.next")); n != 20 {
		t.Errorf("stepped %d searches, want 20", n)
	}
	if n := len(f.requests()); n != 40 {
		t.Errorf("sent %d requests (%v), want the 20 steps and their 20 rewards only", n, f.cmds())
	}
}

// TestSuppliesSearchDiamondChecks: the tripwire compares every reply's gold and remainGold with the
// balance seen so far, init user.gold (PlayerInfo.lua:630-632) or the last reply's, and a drop ends
// the run: no further search and no radar read.
func TestSuppliesSearchDiamondChecks(t *testing.T) {
	for name, c := range map[string]struct {
		initGold    int64 // 0: init carries no gold
		step, prize func(*sfs.SFSObject)
		want        string
	}{
		"remainGold below init gold": {initGold: 500, step: func(r *sfs.SFSObject) { r.PutLong("remainGold", 400) },
			want: "building.supplies.search.next"},
		"a drop between replies, no init gold": {step: func(r *sfs.SFSObject) { r.PutLong("gold", 500) },
			prize: func(r *sfs.SFSObject) { r.PutLong("gold", 400) },
			want:  "building.supplies.search.next,building.get.supplies.search.reward"},
	} {
		t.Run(name, func(t *testing.T) {
			tripwireTemp(t)
			conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10)},
				func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
					r := dSuppliesStep(cmd, p)
					switch {
					case cmd == "building.supplies.search.next" && c.step != nil:
						c.step(r)
					case cmd == "building.get.supplies.search.reward" && c.prize != nil:
						r = evOK()
						c.prize(r)
					}
					return r
				})
			in := digTestInit(nil)
			if c.initGold > 0 {
				in.Raw.PutSFSObject("user", obj("gold", c.initGold))
			}
			in.Buildings = append(in.Buildings, suppliesBuilding(301, 10150000, suppliesInfo(0, 0)),
				suppliesBuilding(302, 10150000, suppliesInfo(0, 0)))
			if err := runSuppliesSearch(conn, in); err == nil {
				t.Error("want the diamond drop reported")
			}
			if got := strings.Join(f.cmds(), ","); got != c.want {
				t.Errorf("sent %s\nwant %s", got, c.want)
			}
		})
	}
}

// TestSuppliesSearchRadarSoonestFirst: radar searches run soonest-ending first, an event without an
// endTime last.
func TestSuppliesSearchRadarSoonestFirst(t *testing.T) {
	noEnd := suppliesEv(201, 0, suppliesInfo(0, 0))
	noEnd.PutLong("endTime", 0)
	soon := suppliesEv(202, 0, suppliesInfo(0, 0))
	soon.PutLong("endTime", evTestNow.Add(3600e9).UnixMilli())
	conn, f := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{detectReply(20, 10, noEnd, soon)}, dSuppliesStep)
	if err := runSuppliesSearch(conn, digTestInit(nil)); err != nil {
		t.Fatal(err)
	}
	want := []string{"detect.event.supplies.search.next 202", "detect.event.get.supplies.search.reward 202",
		"detect.event.supplies.search.next 201", "detect.event.get.supplies.search.reward 201"}
	if got := suppliesSent(f); !slices.Equal(got, want) {
		t.Errorf("sent %v\nwant %v", got, want)
	}
}

func TestSuppliesSearchFeatureIsOptIn(t *testing.T) {
	f, ok := featureRegistry[suppliesSearchFeature]
	if !ok {
		t.Fatal("supplies-search not registered")
	}
	if f.DefaultOn || f.Hold || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("DefaultOn=%v Hold=%v Summary=%q", f.DefaultOn, f.Hold, f.Summary)
	}
}

// TestSuppliesSearchLevelOneIsCertain pins the premise of the feature against the table dump: the
// first level of every monopoly_search row succeeds at 10000 (100 %). Skipped without
// LASTWAR_TABLES.
func TestSuppliesSearchLevelOneIsCertain(t *testing.T) {
	dir := os.Getenv("LASTWAR_TABLES")
	if dir == "" {
		t.Skip("LASTWAR_TABLES is not set")
	}
	data, err := os.ReadFile(filepath.Join(dir, "monopoly_search.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		Index map[string][]json.RawMessage `json:"index"`
		Rows  [][]json.RawMessage          `json:"rows"`
	}
	if err := json.Unmarshal(data, &tab); err != nil {
		t.Fatal(err)
	}
	spec, ok := tab.Index["level_rate"]
	if !ok || len(spec) == 0 {
		t.Fatal("no level_rate column")
	}
	var pos int
	if err := json.Unmarshal(spec[0], &pos); err != nil || pos < 1 {
		t.Fatalf("level_rate position %s", spec[0])
	}
	if len(tab.Rows) == 0 {
		t.Fatal("no rows")
	}
	for _, row := range tab.Rows {
		var vals []json.RawMessage
		if len(row) != 2 || json.Unmarshal(row[1], &vals) != nil || len(vals) < pos {
			t.Fatalf("bad row %s", row)
		}
		var rate string
		if err := json.Unmarshal(vals[pos-1], &rate); err != nil {
			t.Fatalf("row %s: level_rate %s", row[0], vals[pos-1])
		}
		if first, _, _ := strings.Cut(rate, "|"); first != "1;10000" {
			t.Errorf("monopoly_search %s: level 1 is %q, not 1;10000: the first step can fail", row[0], first)
		}
	}
}
