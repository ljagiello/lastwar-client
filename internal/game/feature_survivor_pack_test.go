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
			// SurvivorVisitorReceiveScoreMessage.lua:7: PutInt index.
			seasonParamIs(t, r.Params, "index", sfs.SFSInt, r.Params.GetInt("index"))
		default:
			got = append(got, fmt.Sprintf("%s %d", r.Cmd, r.Params.GetInt("id")))
			// SurvivorVisitorReceiveFreeMessage.lua:7, SurvivorVisitorReceiveBubbleMessage.lua:7: PutInt id.
			seasonParamIs(t, r.Params, "id", sfs.SFSInt, r.Params.GetInt("id"))
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
	// tableInfoType 3: activity_survivor 40413 has rows 1 and 2 only, so the client has no config
	// (SurvivorPackManager.lua:74-86) and not even the info read is sent
	noConfig := survivorInit(48*time.Hour, nil, nil, 0)
	evObject(evObject(noConfig.Raw, "activity"), "1110002").PutInt("tableInfoType", 3)
	for name, in := range map[string]*Init{"not started": notStarted, "ended": ended, "no type 188": other, "no tomorrow": noTomorrow,
		"no activity_survivor row": noConfig} {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSurvivorPack(conn, in); err != nil {
			t.Fatal(err)
		}
		if got := fake.cmds(); len(got) != 0 {
			t.Errorf("%s: sent %v, want nothing", name, got)
		}
	}
}

// bSurvivorInit is survivorInit with the given init items in place of its score-item stack.
func bSurvivorInit(startAgo time.Duration, ids, free []int32, items ...*sfs.SFSObject) *Init {
	in := survivorInit(startAgo, ids, free, 0)
	in.Raw.PutSFSArray("items", bObjArray(items...))
	return in
}

// bSurvivorSeq is 1..n as Int32s.
func bSurvivorSeq(n int32) []int32 {
	out := make([]int32, 0, n)
	for i := int32(1); i <= n; i++ {
		out = append(out, i)
	}
	return out
}

// Refused twins of TestSurvivorPackClaimsFreeThenBubbleAndScoreStages: one input changed each, and
// only the info read goes out.
//   - Config row 2 (the entry's own subType, ActivityInfoData.lua:231) lists activity_survivor
//     list_group "2", and every activity_survivor_list row of 40413 is group "1"
//     (SurvivorPackManager.lua:74-139): no survivor is due.
//   - The score bar counts activity_survivor score_item 993113 (tables 40413, rows 1 and 2) only:
//     a bigger stack of another item reaches no stage (:157-178).
//   - The init activitySurvivorVisitor arrays hold until a reply carries new ones (ParseInfo,
//     :195-211): an info reply without them leaves survivors 1-3 claimed.
//   - activity_survivor_list 40413 ends at id 30 (day 30): with 1-30 claimed, day 32 has nothing.
func TestSurvivorPackClaimsNothingMore(t *testing.T) {
	withEvNow(t, evTestNow)
	score := func(id string, n int32) *sfs.SFSObject { return obj("itemId", id, "count", n) }
	group2 := bSurvivorInit(48*time.Hour, nil, nil, score("993113", 4))
	evObject(evObject(group2.Raw, "activity"), "1110002").PutInt("subType", 2)
	for name, in := range map[string]*Init{
		"config row 2, list group 2":      group2,
		"score of another item":           bSurvivorInit(48*time.Hour, bSurvivorSeq(3), bSurvivorSeq(3), score("993114", 50), score("993113", 4)),
		"init state, info without arrays": bSurvivorInit(48*time.Hour, bSurvivorSeq(3), bSurvivorSeq(3), score("993113", 4)),
		"day 32, list ends at 30":         bSurvivorInit(31*24*time.Hour, bSurvivorSeq(30), bSurvivorSeq(30), score("993113", 4)),
	} {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSurvivorPack(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fake.cmds(); !slices.Equal(got, []string{"survivor.visitor.info"}) {
			t.Errorf("%s: sent %v, want the info read only", name, got)
		}
	}
}

// TestSurvivorPackScoreItemCountIsTheLastField: the bar counts itemInfo.count of score_item 993113
// (SurvivorPackManager.lua:245-251, ItemData.GetItemById). ItemInfo:UpdateInfo sets count from
// count, then num, then itemLeftCnt (ItemInfo.lua:52-66), so the last present field is the count.
// Stage 0 (5) is claimed in the init state; bar_reward row 1 is 5|10|20|40|50.
func TestSurvivorPackScoreItemCountIsTheLastField(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name string
		item *sfs.SFSObject
		want []int32 // the score stages sent
	}{
		{"count 50, itemLeftCnt 0", obj("itemId", "993113", "count", 50, "itemLeftCnt", 0), nil},
		{"count 50, num 0", obj("itemId", "993113", "count", 50, "num", 0), nil},
		{"count 0, num 10", obj("itemId", "993113", "count", 0, "num", 10), []int32{1}},
		{"num 0, itemLeftCnt 20", obj("itemId", "993113", "num", 0, "itemLeftCnt", 20), []int32{1, 2}},
		{"count 20 alone", obj("itemId", "993113", "count", 20), []int32{1, 2}},
	} {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSurvivorPack(conn, bSurvivorInit(48*time.Hour, bSurvivorSeq(3), bSurvivorSeq(3), c.item)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []int32
		for _, r := range fake.only("survivor.visitor.receive.score") {
			got = append(got, r.Params.GetInt("index"))
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: score stages sent %v, want %v", c.name, got, c.want)
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

// TestSurvivorConfigIDReadsTheActivityRow: without a tableInfoType or subType on the entry, the
// activity_survivor row is the activity row's tableInfoType (ActivityInfoData.lua:231), so a
// type-188 row the old hand-copied map never listed (777188 -> 2) still finds its config.
func TestSurvivorConfigIDReadsTheActivityRow(t *testing.T) {
	withActivityRows(t, map[int32]activityRow{777188: {typ: 188, subType: 2}, 777189: {typ: 188}})
	raw := sfs.NewSFSObject()
	if got, ok := survivorConfigID(Activity{ID: 777188, Type: 188, Raw: raw}); !ok || got != 2 {
		t.Errorf("survivorConfigID(777188) = %d, %v; want the row's tableInfoType 2", got, ok)
	}
	if got, ok := survivorConfigID(Activity{ID: 1110002, Type: 188, Raw: raw}); !ok || got != 1 {
		t.Errorf("survivorConfigID(1110002) = %d, %v; want 1 (tables 40413)", got, ok)
	}
	if got, ok := survivorConfigID(Activity{ID: 777189, Type: 188, Raw: raw}); ok {
		t.Errorf("survivorConfigID(777189) = %d, true; want no config for a row without tableInfoType", got)
	}
	own := sfs.NewSFSObject()
	own.PutInt("tableInfoType", 3)
	if got, ok := survivorConfigID(Activity{ID: 777188, Type: 188, Raw: own}); !ok || got != 3 {
		t.Errorf("survivorConfigID with the entry's own tableInfoType = %d, %v; want 3", got, ok)
	}
}
