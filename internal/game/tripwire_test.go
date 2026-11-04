package game

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// tripwireTemp points the tripwire at <dir>/session.json.tripwire.json in a fresh temp dir and
// clears every in-process trip around the test, as a new process starts with none. It returns
// the tripwire file's path.
func tripwireTemp(t *testing.T) string {
	t.Helper()
	path := TripwireFile(filepath.Join(t.TempDir(), "session.json"))
	SetTripwirePath(path)
	tripwireRestart()
	t.Cleanup(func() {
		SetTripwirePath("")
		tripwireRestart()
	})
	return path
}

// tripwireRestart clears the in-process trips: what a new cron run starts with.
func tripwireRestart() {
	duelSpendHalted.Store(false)
	duelSpeedupsTrip.Lock()
	duelSpeedupsTrip.reason = ""
	duelSpeedupsTrip.Unlock()
	digsTrip.Lock()
	digsTrip.reason = ""
	digsTrip.Unlock()
}

func readTripwire(t *testing.T, path string) TripwireRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("tripwire file: %v", err)
	}
	var rec TripwireRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("tripwire file %q: %v", data, err)
	}
	return rec
}

func TestTripwirePersistsAcrossRunsUntilCleared(t *testing.T) {
	path := tripwireTemp(t)
	if filepath.Base(path) != "session.json.tripwire.json" {
		t.Fatalf("tripwire file %s, want its own file next to the session config", path)
	}

	// Run 1: a pushed balance below init gold trips duel-recruit-tickets.
	in := duelRecruitInit(1000, map[string]int{"230006": 40}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, _ := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelRecruitHeroCmd {
			return []duelSpendMsg{duelPush(duelPushResourceInfo, duelObj("gold", int64(940))), duelReply(evOK())}
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	if err := runDuelRecruitTickets(conn, in); !errors.Is(err, errDuelSpendHalted) {
		t.Fatalf("run 1: err = %v, want the tripwire", err)
	}
	rec := readTripwire(t, path)
	if rec.Feature != "duel-recruit-tickets" || rec.Command != duelPushResourceInfo ||
		!strings.Contains(rec.Reason, "1000 to 940") || rec.Time.IsZero() {
		t.Errorf("record %+v, want the feature, the push it was seen in, the drop and a time", rec)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("tripwire file mode: %v, %v; want 0600", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Errorf("dir holds %d entries, want only the tripwire file (no temp file, no session config)", len(entries))
	}

	// Run 2, a new process: nothing in memory, but every duel and dig spend refuses and sends
	// nothing, not even the duel read.
	tripwireRestart()
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); !errors.Is(err, errTripwire) || !strings.Contains(err.Error(), path) {
		t.Errorf("duel-recruit-tickets: err = %v, want the tripwire file named", err)
	}
	if err := runDuelTroopTraining(conn, duelTrainInit(100_000_000, 100_000_000, true, nil, duelCampSFS(1, 30, false))); !errors.Is(err, errTripwire) {
		t.Errorf("duel-troop-training: err = %v, want the tripwire", err)
	}
	if err := runSecretTaskStarts(conn, stInit(2, 1000, stHeroRow{1, stHeroUR, 40, 7})); !errors.Is(err, errTripwire) {
		t.Errorf("secret-tasks-start: err = %v, want the tripwire", err)
	}
	if got := f.cmds(); len(got) != 0 {
		t.Errorf("refusing features sent %v", got)
	}
	c := speedInit{jobs: allJobs(), items: allItems(), gold: 1000}
	if sent, err := runSpeed(t, newSpeedFake(speedTue(), c), c, false); !errors.Is(err, errTripwire) || len(sent) != 0 {
		t.Errorf("duel-speedups: sent %v, err %v; want nothing and the tripwire", sentStrings(sent), err)
	}
	// The read-only plan still runs.
	if _, err := runSpeed(t, newSpeedFake(speedTue(), c), c, true); err != nil {
		t.Errorf("duel-speedups-plan: %v", err)
	}
	digConn, digFake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	r := newDigsRun(digConn, digsInit(0), "season-dig", false)
	open := sfs.NewSFSObject()
	open.PutLong("uuid", 1)
	open.PutInt("pos", 3)
	if _, res := r.write("open", seasonDigBlockCmd, open); res != digsFailed || !errors.Is(r.err(), errTripwire) || !r.halt {
		t.Errorf("dig write: %v, %v; want it refused by the tripwire", res, r.err())
	}
	if got := digFake.cmds(); len(got) != 0 {
		t.Errorf("dig write sent %v", got)
	}

	// The operator clears it: the next run spends again.
	if removed, err := ClearTripwire(path); !removed || err != nil {
		t.Fatalf("clear: %v, %v", removed, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tripwire file still there: %v", err)
	}
	tripwireRestart()
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil || len(pullsOf(t, f)) == 0 {
		t.Errorf("after the clear: err %v, pulls %v; want the tickets spent", err, pullsOf(t, f))
	}
	if removed, err := ClearTripwire(path); removed || err != nil {
		t.Errorf("clearing again: %v, %v; want nothing to clear", removed, err)
	}
}

func TestTripwireRecordsEachTripSource(t *testing.T) {
	// duel-speedups: the first trip is kept; a later one doesn't overwrite it.
	path := tripwireTemp(t)
	c := speedInit{jobs: []speedJob{{kind: duelSpeedupBuild, uuid: 1, left: 2 * time.Hour}}, items: allItems(), gold: 1000}
	f := newSpeedFake(speedTue(), c)
	f.goldDrop = 1
	if _, err := runSpeed(t, f, c, false); err == nil {
		t.Fatal("want the tripwire")
	}
	if rec := readTripwire(t, path); rec.Feature != "duel-speedups" || rec.Command != duelSpeedupsBuildCmd ||
		!strings.Contains(rec.Reason, "dropped from 1000 to 999") {
		t.Errorf("record %+v", rec)
	}
	tripwireTrip("season-dig", seasonDigBlockCmd, "later")
	if rec := readTripwire(t, path); rec.Feature != "duel-speedups" {
		t.Errorf("a later trip replaced the first: %+v", rec)
	}

	// A dig write whose reply carries a lower balance.
	path = tripwireTemp(t)
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		r.PutLong("gold", 400) // below the 500 in init
		return r
	})
	r := newDigsRun(conn, digsInit(0), "season-dig", false)
	open := sfs.NewSFSObject()
	open.PutLong("uuid", 1)
	open.PutInt("pos", 3)
	r.write("open", seasonDigBlockCmd, open)
	if rec := readTripwire(t, path); rec.Feature != "season-dig" || rec.Command != seasonDigBlockCmd ||
		!strings.Contains(rec.Reason, "500 to 400") {
		t.Errorf("dig record %+v", rec)
	}

	// secret-tasks-start: a re-listed balance below the first.
	path = tripwireTemp(t)
	heroes := []stHeroRow{{1, stHeroUR, 40, 7}, {2, stHeroUR, 40, 7}}
	s := &stServer{tasks: []stTask{{uuid: 11, cfgID: stTaskUR, point: stNear}, {uuid: 12, cfgID: stTaskUR, point: stNear}},
		listGold: []int64{1000, 900}, startGold: -1}
	if _, err := stRun(t, stInit(2, 1000, heroes...), s, false); !errors.Is(err, errSecretTaskStartsDiamonds) {
		t.Fatalf("err = %v", err)
	}
	if rec := readTripwire(t, path); rec.Feature != secretTaskStartsName || rec.Command != secretTaskStartsCmd ||
		!strings.Contains(rec.Reason, "1000 to 900") {
		t.Errorf("secret task record %+v", rec)
	}
}

func TestTripwireFailsClosedAndStaysInMemoryWithoutAPath(t *testing.T) {
	// A file that can't be parsed still counts as a trip.
	path := tripwireTemp(t)
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := tripwireErr("duel-speedups"); !errors.Is(err, errTripwire) {
		t.Errorf("malformed file: err = %v, want the tripwire", err)
	}

	// No path (tests, a run without a session config): a trip writes nothing and nothing refuses.
	SetTripwirePath("")
	dir := filepath.Dir(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	tripwireTrip("duel-speedups", duelSpeedupsBuildCmd, "in memory only")
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("a trip without a path wrote %d files", len(entries))
	}
	if err := tripwireErr("duel-speedups"); err != nil {
		t.Errorf("no path: err = %v", err)
	}
	if TripwireFile("") != "" {
		t.Error("no session config must mean no tripwire file")
	}
	if _, err := ClearTripwire(""); err == nil {
		t.Error("clearing an empty path must fail")
	}
}
