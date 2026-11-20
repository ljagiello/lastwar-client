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
