package game

import (
	"strconv"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// giftBoxReply is a get.activity.gift.box.info reply: useFreeTimes (absent when < 0), boxes slots
// filled, score and scoreArr entries {index, targetScore, state}.
func giftBoxReply(useFree int32, boxes int, score int32, entries ...[3]int32) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	if useFree >= 0 {
		r.PutInt("useFreeTimes", useFree)
	}
	gb := sfs.NewSFSArray()
	for i := range boxes {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", int64(900+i))
		o.PutInt("itemId", 210158)
		gb.AddSFSObject(o)
	}
	r.PutSFSArray("giftBoxs", gb)
	r.PutInt("score", score)
	sa := sfs.NewSFSArray()
	for _, e := range entries {
		o := sfs.NewSFSObject()
		o.PutInt("index", e[0])
		o.PutInt("targetScore", e[1])
		o.PutInt("state", e[2])
		sa.AddSFSObject(o)
	}
	r.PutSFSArray("scoreArr", sa)
	r.PutInt("free", 1) // the daily free pack is claimed: event-shop-freebies has nothing to send
	return r
}

// G4: the free single draw is activity.gift.box.lottery {activityId: Int, fiveLottery: Int 0,
// useFree: Int 1}, sent only while useFreeTimes (absent = 0) is below cost_1_free and a slot is
// free (UIGiftBoxNew.lua:393-410, 489-531), at most once a run.
func TestGiftBoxFreeDrawGates(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name    string
		useFree int32
		boxes   int
		want    bool
	}{
		{"unused, absent count", -1, 0, true},
		{"unused, three boxes", 0, 3, true},
		{"free draw used", 1, 0, false},
		{"all four slots full", 0, 4, false},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == giftBoxInfoCmd {
				return giftBoxReply(c.useFree, c.boxes, 0)
			}
			return evOK()
		})
		if err := runGiftBoxFreeDraw(conn, evInit(30, evActivity(98001))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		draws := fake.only(giftBoxLotteryCmd)
		if (len(draws) == 1) != c.want {
			t.Errorf("%s: draws %d, want drawn=%v", c.name, len(draws), c.want)
			continue
		}
		if !c.want {
			continue
		}
		p := draws[0].Params
		for key, want := range map[string]int32{"activityId": 98001, "fiveLottery": 0, "useFree": 1} {
			if v, _ := p.Get(key); v.Type != sfs.SFSInt || p.GetInt(key) != want {
				t.Errorf("%s: %s = %#v, want Int %d (never a paid or five-draw)", c.name, key, v, want)
			}
		}
	}
}

// G4: one draw a run across Gift Box events; an activity without an activity_boxopen row is never
// drawn (its draw is never free).
func TestGiftBoxFreeDrawOncePerRun(t *testing.T) {
	withEvNow(t, evTestNow)
	saved := giftBoxOpenActivities[98005]
	delete(giftBoxOpenActivities, 98005)
	t.Cleanup(func() { giftBoxOpenActivities[98005] = saved })
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == giftBoxInfoCmd {
			return giftBoxReply(0, 0, 0)
		}
		return evOK()
	})
	if err := runGiftBoxFreeDraw(conn, evInit(30, evActivity(98001), evActivity(98005), evActivity(98008))); err != nil {
		t.Fatal(err)
	}
	draws := fake.only(giftBoxLotteryCmd)
	if len(draws) != 1 || draws[0].Params.GetInt("activityId") != 98001 {
		t.Errorf("draws = %v, want one for 98001", draws)
	}
	for _, r := range fake.only(giftBoxInfoCmd) {
		if r.Params.GetInt("activityId") == 98005 {
			t.Error("98005 has no activity_boxopen row here, so its info must not be read for a draw")
		}
	}
}

// G4, refused twins of the 98001 draw in TestGiftBoxFreeDrawGates, each run alone so no earlier
// draw ends the run first: a type-132 activity without an activity_boxopen row never has a free
// single draw (RefreshCostOne reads the row's cost_1_free, UIGiftBoxNew.lua:393-410, and leaves
// isFree 0 without one), so nothing is read or drawn; and a refused info read gives no
// useFreeTimes to check, so no draw follows it.
func TestGiftBoxFreeDrawNeedsTheRowAndTheRead(t *testing.T) {
	withEvNow(t, evTestNow)
	withActivityRows(t, map[int32]activityRow{777132: {typ: 132}})
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "get.activity.gift.box.info" {
			return giftBoxReply(0, 0, 0)
		}
		return evOK()
	})
	if err := runGiftBoxFreeDraw(conn, evInit(30, evActivity(777132))); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("no activity_boxopen row: sent %v, want nothing", got)
	}

	conn, fake = startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return evErr("E_GIFT_INFO") })
	if err := runGiftBoxFreeDraw(conn, evInit(30, evActivity(98001))); err == nil || !strings.Contains(err.Error(), "E_GIFT_INFO") {
		t.Errorf("refused read: err = %v, want the E_GIFT_INFO failure", err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.activity.gift.box.info" {
		t.Errorf("refused read: sent %s, want the read only", got)
	}
}

// G4: a scoreArr entry is a box only with its state, targetScore and index (ActGiftBoxScoreInfo.
// lua:19-31): entries missing one of them are not claimed; the complete one next to them is.
func TestGiftBoxScoreBoxesNeedEveryField(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "get.activity.gift.box.info" {
			r := giftBoxReply(1, 0, 500, [3]int32{4, 100, 0})
			arr, _ := r.Get("scoreArr")
			for _, e := range []*sfs.SFSObject{
				obj("index", 1, "targetScore", 50), // no state
				obj("index", 2, "state", 0),        // no targetScore
				obj("targetScore", 50, "state", 0), // no index
			} {
				arr.Val.(*sfs.SFSArray).AddSFSObject(e)
			}
			return r
		}
		return evOK()
	})
	if err := runGiftBoxScoreBoxes(conn, evInit(30, evActivity(98001))); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.only("gift.box.receive") {
		got = append(got, evString(r.Params, "index"))
	}
	if strings.Join(got, ",") != "4" {
		t.Errorf("claimed boxes %v, want only 4", got)
	}
}

// G4: score boxes with state 0 and score >= targetScore send gift.box.receive {activityId: Int,
// index: Int <entry index>} (GiftBoxRewardBoxItemNew.lua:82-86); a draw first refreshes the reply.
func TestGiftBoxScoreBoxesAfterTheDraw(t *testing.T) {
	withEvNow(t, evTestNow)
	infos := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case giftBoxInfoCmd:
			infos++
			if infos == 1 {
				return giftBoxReply(0, 0, 90, [3]int32{1, 50, 1}, [3]int32{2, 100, 0}, [3]int32{3, 200, 0})
			}
			// the draw added 10 score
			return giftBoxReply(1, 1, 100, [3]int32{1, 50, 1}, [3]int32{2, 100, 0}, [3]int32{3, 200, 0})
		}
		return evOK()
	})
	in := evInit(30, evActivity(98001))
	if err := runGiftBoxFreeDraw(conn, in); err != nil {
		t.Fatal(err)
	}
	if err := runGiftBoxScoreBoxes(conn, in); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{giftBoxInfoCmd, giftBoxLotteryCmd, giftBoxInfoCmd, giftBoxReceiveCmd}, ",")
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	p := fake.only(giftBoxReceiveCmd)[0].Params
	if v, _ := p.Get("index"); v.Type != sfs.SFSInt || p.GetInt("index") != 2 {
		t.Errorf("score box index %#v, want Int 2 (box 1 claimed, box 3 needs 200)", v)
	}
}

// G4: neither command is benign; an errorCode stops the feature.
func TestGiftBoxErrorsStop(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == giftBoxInfoCmd {
			return giftBoxReply(0, 0, 500, [3]int32{1, 50, 0}, [3]int32{2, 100, 0})
		}
		return evErr(evAlreadyExecuted)
	})
	in := evInit(30, evActivity(98001), evActivity(98005))
	if err := runGiftBoxScoreBoxes(conn, in); err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("score boxes: err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake.only(giftBoxReceiveCmd)); n != 1 {
		t.Errorf("score box claims = %d, want 1 (the first error stops)", n)
	}
	if err := runGiftBoxFreeDraw(conn, in); err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("draw: err = %v, want the %s failure", err, evAlreadyExecuted)
	}
}

// TestGiftBoxOpenRowsMatchTheActivityTable: giftBoxOpenActivities is the type-132 rows of the
// generated activity table.
func TestGiftBoxOpenRowsMatchTheActivityTable(t *testing.T) {
	n := 0
	for id, r := range activityTable {
		if r.typ == actTypeGiftBox {
			n++
			if !giftBoxOpenActivities[id] {
				t.Errorf("type-132 activity %d is not in giftBoxOpenActivities", id)
			}
		}
	}
	if n != len(giftBoxOpenActivities) {
		t.Errorf("giftBoxOpenActivities has %d ids, the activity table %d type-132 rows", len(giftBoxOpenActivities), n)
	}
}

// TestGiftBoxOpenRowsMatchTheDump pins giftBoxOpenActivities and the cost_1_free / box_num
// constants to table activity_boxopen.
func TestGiftBoxOpenRowsMatchTheDump(t *testing.T) {
	rows := eventTableDump(t, "activity_boxopen")
	acts := map[int32]bool{}
	for key, r := range rows {
		id, err := strconv.ParseInt(eventDumpScalar(r["activity"]), 10, 32)
		if err != nil {
			t.Fatalf("row %s activity %v", key, r["activity"])
		}
		acts[int32(id)] = true
		if f := eventDumpScalar(r["cost_1_free"]); f != strconv.Itoa(giftBoxFreeDraws) {
			t.Errorf("row %s cost_1_free %s, want %d", key, f, giftBoxFreeDraws)
		}
		if b := eventDumpScalar(r["box_num"]); b != strconv.Itoa(giftBoxSlots) {
			t.Errorf("row %s box_num %s, want %d", key, b, giftBoxSlots)
		}
	}
	if len(acts) != len(giftBoxOpenActivities) {
		t.Errorf("dump covers %d activities, giftBoxOpenActivities %d", len(acts), len(giftBoxOpenActivities))
	}
	for id := range acts {
		if !giftBoxOpenActivities[id] {
			t.Errorf("activity %d has an activity_boxopen row but is not in giftBoxOpenActivities", id)
		}
	}
}
