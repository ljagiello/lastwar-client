package game

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// survivorInit is an Init with the Survivor Pack activity 1110002 started startAgo before
// evTestNow, init activitySurvivorVisitor {receiveIdArr: ids, receiveFreeArr: free} and scoreItems
// of item 993113.
func survivorInit(startAgo time.Duration, ids, free []int32, scoreItems int32) *Init {
	a := evActivity(1110002)
	a.PutLong("startTime", evTestNow.Add(-startAgo).UnixMilli())
	in := evInit(30, a)
	v := sfs.NewSFSObject()
	v.PutIntArray("receiveIdArr", ids)
	v.PutIntArray("receiveFreeArr", free)
	v.PutIntArray("receivePayArr", nil)
	v.PutIntArray("receiveScoreArr", []int32{0})
	in.Raw.PutSFSObject("activitySurvivorVisitor", v)
	items := sfs.NewSFSArray()
	it := sfs.NewSFSObject()
	it.PutUtfString("itemId", survivorScoreItem)
	it.PutInt("count", scoreItems)
	items.AddSFSObject(it)
	in.Raw.PutSFSArray("items", items)
	return in
}

func TestSurvivorOpenDayCountsServerDays(t *testing.T) {
	in := evInit(30) // the server day starts at 02:00 UTC
	for _, c := range []struct {
		start time.Time
		want  int64
	}{
		{evTestNow, 1},
		{evTestNow.Add(-9 * time.Hour), 1},                 // 03:00, same server day
		{evTestNow.Add(-11 * time.Hour), 2},                // 01:00, the previous server day
		{evTestNow.Add(-48 * time.Hour), 3},                // two server days back
		{evTestNow.Add(48 * time.Hour), 1},                 // not started: max(1, ...)
		{time.Date(2026, 9, 6, 2, 0, 0, 0, time.UTC), 30},  // 29 days back at the reset
		{time.Date(2026, 9, 6, 1, 59, 0, 0, time.UTC), 31}, // a minute before that reset
	} {
		got, ok := survivorOpenDay(in, c.start.UnixMilli(), evTestNow)
		if !ok || got != c.want {
			t.Errorf("start %v: day %d (%v), want %d", c.start, got, ok, c.want)
		}
	}
}

func TestSurvivorPackClaimsFreeThenBubbleAndScoreStages(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		switch cmd {
		case survivorInfoCmd:
			// fresher than init: survivor 2 came as a visitor since
			r.PutIntArray("receiveIdArr", []int32{1, 2})
			r.PutIntArray("receiveFreeArr", []int32{1})
			r.PutIntArray("receiveScoreArr", []int32{0})
		case survivorFreeCmd:
			r.PutIntArray("receiveFreeArr", []int32{1, p.GetInt("id")})
		}
		return r
	})
	// day 3: survivors 1-3 are due; 1 claimed, 2 listed, 3 new; 12 score items reach stages 0
	// and 1, and 0 is claimed
	if err := runSurvivorPack(conn, survivorInit(48*time.Hour, []int32{1}, []int32{1}, 12)); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests() {
		if v, _ := r.Params.Get("activityId"); v.Type != sfs.SFSInt || r.Params.GetInt("activityId") != 1110002 {
			t.Errorf("%s activityId = %#v, want Int 1110002", r.Cmd, v)
		}
		switch r.Cmd {
		case survivorInfoCmd:
			got = append(got, r.Cmd)
		case survivorScoreCmd:
			got = append(got, fmt.Sprintf("%s %d", r.Cmd, r.Params.GetInt("index")))
		default:
			got = append(got, fmt.Sprintf("%s %d", r.Cmd, r.Params.GetInt("id")))
		}
	}
	want := []string{
		survivorInfoCmd,
		survivorFreeCmd + " 2",
		survivorFreeCmd + " 3", survivorBubbleCmd + " 3",
		survivorScoreCmd + " 1",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sent %v\nwant %v", got, want)
	}
}

func TestSurvivorPackGates(t *testing.T) {
	withEvNow(t, evTestNow)
	notStarted := survivorInit(-time.Hour, nil, nil, 0)
	ended := survivorInit(48*time.Hour, nil, nil, 0)
	evObject(evObject(ended.Raw, "activity"), "1110002").PutLong("endTime", evTestNow.UnixMilli()) // endTime is exclusive
	other := evInit(30, evActivity(99152))
	noTomorrow := survivorInit(48*time.Hour, nil, nil, 0)
	noTomorrow.Raw.PutLong("tomorrow", 0)
	for name, in := range map[string]*Init{"not started": notStarted, "ended": ended, "no type 188": other, "no tomorrow": noTomorrow} {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSurvivorPack(conn, in); err != nil {
			t.Fatal(err)
		}
		if got := fake.cmds(); len(got) != 0 {
			t.Errorf("%s: sent %v, want nothing", name, got)
		}
	}
}

func TestSurvivorPackStopsOnRefusal(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == survivorFreeCmd {
			return evErr("E400001")
		}
		return evOK()
	})
	err := runSurvivorPack(conn, survivorInit(24*time.Hour, nil, nil, 50))
	if err == nil || !strings.Contains(err.Error(), "E400001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{survivorInfoCmd, survivorFreeCmd}) {
		t.Errorf("sent %v, want the info and the first free claim only", got)
	}
}

func TestSurvivorScoresDue(t *testing.T) {
	scores := survivorConfigs[1].scores
	if got := survivorScoresDue(scores, 4, nil); len(got) != 0 {
		t.Errorf("4 items: %v, want none", got)
	}
	if got := survivorScoresDue(scores, 40, []int64{0, 2}); !slices.Equal(got, []int64{1, 3}) {
		t.Errorf("40 items, 0 and 2 claimed: %v, want [1 3]", got)
	}
}
