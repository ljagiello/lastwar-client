package game

import (
	"encoding/json"
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
