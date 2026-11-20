package game

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func eventBoxObjs(fields []string, rows ...[]int32) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, r := range rows {
		o := sfs.NewSFSObject()
		for i, f := range fields {
			o.PutInt(f, r[i])
		}
		a.AddSFSObject(o)
	}
	return a
}

func eventBoxInts(v ...int32) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, n := range v {
		a.AddInt(n)
	}
	return a
}

// G26: Lucky Roll stages (state 0, totalLotteryCount >= needLotteryNum; UILuckyRollBox.lua:90-98),
// Blue Store boxes (totalScore >= target, index not in boxReceive; LWTitaniumBlueStoreManager.lua:
// 287-305) and slot machine boxes (activity_slots_info thresholds, 0-based index not in
// rewardProcess; UIActSlotProgressItem.lua:86-90) and state-1 tasks.
func TestEventMilestoneBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case luckyRollInfoCmd:
			ri := sfs.NewSFSObject()
			ri.PutInt("totalLotteryCount", 25)
			r.PutSFSObject("rollInfo", ri)
			r.PutSFSArray("stageArr", eventBoxObjs([]string{"stage", "needLotteryNum", "state"},
				[]int32{1, 10, 1}, []int32{2, 20, 0}, []int32{3, 30, 0}))
		case "hero.event.info.get":
			switch p.GetString("activityId") {
			case "5301": // Blue Store
				r.PutInt("totalScore", 600)
				r.PutSFSArray("boxRewards", eventBoxObjs([]string{"index", "target"}, []int32{1, 100}, []int32{2, 500}, []int32{3, 900}))
				r.PutSFSArray("boxReceive", eventBoxInts(1))
			case "99050": // Slots, tableInfoType 1
				r.PutInt("totalScore", 150)
				r.PutSFSArray("rewardProcess", eventBoxInts(0))
				r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"901": 1}))
			}
		default:
			return evOK()
		}
		return r
	})
	in := evInit(30, evActivity(14), evActivity(5301), evActivity(99050))
	if err := runEventMilestones(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests() {
		switch r.Cmd {
		case luckyRollStageCmd:
			got = append(got, "roll:"+strconv.Itoa(int(r.Params.GetInt("stage"))))
		case blueStoreBoxCmd, slotsProcessCmd:
			got = append(got, r.Cmd+":"+strconv.Itoa(int(r.Params.GetInt("index"))))
		case slotsTaskCmd:
			v, _ := r.Params.Get("taskId")
			if v.Type != sfs.SFSUtfString {
				t.Errorf("slots taskId %#v, want UtfString", v)
			}
			got = append(got, "task:"+r.Params.GetString("taskId"))
		default:
			continue
		}
		if v, _ := r.Params.Get("activityId"); v.Type != sfs.SFSInt {
			t.Errorf("%s activityId %#v, want Int", r.Cmd, v)
		}
	}
	want := []string{"roll:2", blueStoreBoxCmd + ":2", slotsProcessCmd + ":1", "task:901"}
	if !slices.Equal(got, want) {
		t.Errorf("claims %v\nwant   %v", got, want)
	}
}

// G26, refused twin of TestEventMilestoneBoxes: the same three panels with one claimable entry each
// and entries the client would not claim. A Lucky Roll stage needs its stage, needLotteryNum and
// state 0 (UILuckyRollBox.lua:96-97); a Blue Store box its index and target
// (LWTitaniumBlueBoxRewardInfo.lua:15-25); a slots task state 1 (TaskState.CanReceive,
// UIActSlotTaskItem.lua:171-174) and the taskId its claim sends (:188-190).
func TestEventMilestoneEntriesNotClaimable(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case "get.lucky.roll.info":
			ri := sfs.NewSFSObject()
			ri.PutInt("totalLotteryCount", 25)
			r.PutSFSObject("rollInfo", ri)
			r.PutSFSArray("stageArr", bObjArray(
				obj("stage", 2, "needLotteryNum", 20, "state", 0), // claimable
				obj("needLotteryNum", 10, "state", 0),             // no stage
				obj("stage", 4, "state", 0),                       // no needLotteryNum
				obj("stage", 5, "needLotteryNum", 10),             // no state
			))
		case "hero.event.info.get":
			switch p.GetString("activityId") {
			case "5301": // Blue Store
				r.PutInt("totalScore", 600)
				r.PutSFSArray("boxRewards", bObjArray(
					obj("index", 2, "target", 500), // claimable
					obj("target", 100),             // no index
					obj("index", 4),                // no target
				))
				r.PutSFSArray("boxReceive", eventBoxInts(1))
			case "99050": // Slots, tableInfoType 1
				r.PutInt("totalScore", 0) // no box reached
				r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"901": 1, "902": 0, "903": 2, "": 1}))
			}
		default:
			return evOK()
		}
		return r
	})
	if err := runEventMilestones(conn, evInit(30, evActivity(14), evActivity(5301), evActivity(99050))); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests() {
		switch r.Cmd {
		case "receive.lucky.roll.stage.reward":
			got = append(got, "roll:"+evString(r.Params, "stage"))
		case "blue.shop.box.reward":
			got = append(got, "blue:"+evString(r.Params, "index"))
		case "slots.process.reward":
			got = append(got, "slots:"+evString(r.Params, "index"))
		case "slots.task.reward":
			got = append(got, "task:"+r.Params.GetString("taskId"))
		}
	}
	if want := []string{"roll:2", "blue:2", "task:901"}; !slices.Equal(got, want) {
		t.Errorf("claims %v\nwant   %v", got, want)
	}
}

// G26: the slot machine's box thresholds come from the activity_slots_info row named by the
// activity's tableInfoType (ActSlotMachineData.lua:111-114); 40413 has rows 1-11. With no such row
// (tableInfoType 0 or 12) the client builds no box, so slots.process.reward goes only to 99050
// (tableInfoType 1); every slot machine still claims its finished task.
func TestSlotsBoxesNeedAKnownSlotsInfoRow(t *testing.T) {
	withEvNow(t, evTestNow)
	withActivityRows(t, map[int32]activityRow{777158: {typ: 159}, 777159: {typ: 159, subType: 12}})
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd != "hero.event.info.get" {
			return evOK()
		}
		r := sfs.NewSFSObject()
		r.PutInt("totalScore", 1000) // every 40413 threshold (30, 100, 200, 500, 1000) reached
		r.PutSFSArray("rewardProcess", eventBoxInts())
		r.PutSFSArray("taskArr", eventTasks("taskId", map[string]int32{"901": 1}))
		return r
	})
	in := evInit(30, evActivity(99050), evActivity(777158), evActivity(777159))
	if err := runEventMilestones(conn, in); err != nil {
		t.Fatal(err)
	}
	boxes := map[int32]int{}
	for _, r := range fake.only("slots.process.reward") {
		boxes[r.Params.GetInt("activityId")]++
	}
	if len(boxes) != 1 || boxes[99050] != 5 {
		t.Errorf("slots boxes by activity %v, want the five of 99050 only", boxes)
	}
	tasks := map[int32]int{}
	for _, r := range fake.only("slots.task.reward") {
		tasks[r.Params.GetInt("activityId")]++
	}
	if len(tasks) != 3 {
		t.Errorf("slots tasks by activity %v, want one for each of the three activities", tasks)
	}
}

// G26: at most 50 claims per panel and run (the feature's own bound, no client line): 52 reached
// Blue Store boxes send 50 claims.
func TestEventMilestonesBoundPerPanel(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd != "hero.event.info.get" {
			return evOK()
		}
		r := sfs.NewSFSObject()
		r.PutInt("totalScore", 600)
		var rows [][]int32
		for i := int32(1); i <= 52; i++ {
			rows = append(rows, []int32{i, 100})
		}
		r.PutSFSArray("boxRewards", eventBoxObjs([]string{"index", "target"}, rows...))
		return r
	})
	if err := runEventMilestones(conn, evInit(30, evActivity(5301))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only("blue.shop.box.reward")); n != 50 {
		t.Errorf("sent %d box claims, want the bound 50", n)
	}
}

// G26: no claim command is benign; an errorCode stops the feature.
func TestEventMilestoneBoxesErrorStops(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			r := sfs.NewSFSObject()
			r.PutInt("totalScore", 600)
			r.PutSFSArray("boxRewards", eventBoxObjs([]string{"index", "target"}, []int32{1, 100}, []int32{2, 500}))
			return r
		}
		return evErr(evAlreadyExecuted)
	})
	err := runEventMilestones(conn, evInit(30, evActivity(5301), evActivity(5302)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake.only(blueStoreBoxCmd)); n != 1 {
		t.Errorf("box claims = %d, want 1", n)
	}
}

// TestSlotsScoreThresholdsMatchTheDump pins slotsScoreThresholds and slotsInfoRows to table
// activity_slots_info's score_reward.
func TestSlotsScoreThresholdsMatchTheDump(t *testing.T) {
	rows := eventTableDump(t, "activity_slots_info")
	if len(rows) != slotsInfoRows {
		t.Errorf("activity_slots_info has %d rows, slotsInfoRows is %d", len(rows), slotsInfoRows)
	}
	for key, r := range rows {
		var got []int64
		for _, part := range strings.Split(eventDumpScalar(r["score_reward"]), "|") {
			n, err := strconv.ParseInt(strings.Split(part, ";")[0], 10, 64)
			if err != nil {
				t.Fatalf("row %s score_reward %v", key, r["score_reward"])
			}
			got = append(got, n)
		}
		if !slices.Equal(got, slotsScoreThresholds) {
			t.Errorf("row %s thresholds %v, want %v", key, got, slotsScoreThresholds)
		}
	}
}
