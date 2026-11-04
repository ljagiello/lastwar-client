package game

import (
	"bytes"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// stageQuestsInit is evInit plus an init task[] (TaskManager.lua:27-35).
func stageQuestsInit(hq int32, tasks []*sfs.SFSObject, acts ...*sfs.SFSObject) *Init {
	in := evInit(hq, acts...)
	arr := sfs.NewSFSArray()
	for _, t := range tasks {
		arr.AddSFSObject(t)
	}
	in.Raw.PutSFSArray("task", arr)
	return in
}

// stageQuestIDsSent returns the task.reward.get ids, failing on any other command or param type.
func stageQuestIDsSent(t *testing.T, f *evFake) []string {
	t.Helper()
	var out []string
	for _, r := range f.requests() {
		if r.Cmd != questRewardCmd {
			t.Errorf("sent %s, want only %s", r.Cmd, questRewardCmd)
			continue
		}
		v, _ := r.Params.Get("id")
		if v.Type != sfs.SFSUtfString {
			t.Errorf("task.reward.get id type %d, want UtfString", v.Type)
		}
		out = append(out, r.Params.GetString("id"))
	}
	return out
}

func TestStageQuestsClaimsOnlyFreeStageQuests(t *testing.T) {
	withEvNow(t, evTestNow)
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })

	tasks := []*sfs.SFSObject{
		questTask("2201003", 1), // Strongest Commander, stage 1
		questTask("2307005", 1), // Strongest Commander, stage 101
		questTask("2400004", 1), // Lockhart
		questTask("2500001", 1), // King personal target
		questTask("8010012", 1), // task event, stage 4
		questTask("9010005", 1), // task event, stage 98002
		questTask("9010103", 1), // task event, stage 98003
		questTask("9010207", 1), // TaskActivity2, stage 7002
		questTask("9100149", 1), // task event, stage 98501
		questTask("9100300", 1), // task event, stage 98505
		questTask("9100749", 1), // task event, stage 98509
		questTask("2201003", 1), // duplicate
		questTask("2201001", 0), // in progress
		questTask("2201002", 2), // received
		questTask("8000001", 1), // Growth Foundation: pays only after purchase
		questTask("8000112", 1), // Growth Foundation
		questTask("8010101", 1), // Hero Trial: claimed with hero.experiment.reward
		questTask("8010344", 1), // Hero Trial
		questTask("8010018", 1), // stage type 200 only, no activity row
		questTask("9100150", 1), // gap between stage runs: not a stage quest
		questTask("2201006", 1), // gap between stage runs: not a stage quest
		questTask("1001", 1),    // a main quest: the quests feature's
	}
	in := stageQuestsInit(30, tasks, evActivity(11), evActivity(3007), evActivity(3008), evActivity(7002), evActivity(85000))
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runStageQuests(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{"2201003", "2307005", "2400004", "2500001", "8010012", "9010005", "9010103", "9010207", "9100149", "9100300", "9100749"}
	if got := stageQuestIDsSent(t, fake); !slices.Equal(got, want) {
		t.Errorf("claimed %v\nwant    %v", got, want)
	}
	if n := strings.Count(buf.String(), "first task.reward.get reply"); n != 1 {
		t.Errorf("logged the first reply %d times, want once:\n%s", n, buf.String())
	}
}

// TestStageQuestsWaitForTheirActivity: an id is claimed only while an activity of its type is
// valid (GetValidType): HQ at needMainCityLevel, now inside readyTime/startTime ..
// endViewTime/rewardTime/endTime.
func TestStageQuestsWaitForTheirActivity(t *testing.T) {
	withEvNow(t, evTestNow)
	lockhart := evActivity(3007) // past endTime, still inside endViewTime: valid
	lockhart.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	ended := evActivity(3008) // task event past endTime and endViewTime
	ended.PutLong("endTime", evTestNow.Add(-2*time.Hour).UnixMilli())
	ended.PutLong("endViewTime", evTestNow.Add(-time.Hour).UnixMilli())
	early := evActivity(7002) // not started yet
	early.PutLong("startTime", evTestNow.Add(time.Hour).UnixMilli())
	rewardWindow := sfs.NewSFSObject() // Strongest Commander: past endTime, inside rewardTime
	rewardWindow.PutInt("id", 94022)
	rewardWindow.PutLong("startTime", evTestNow.Add(-48*time.Hour).UnixMilli())
	rewardWindow.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	rewardWindow.PutLong("rewardTime", evTestNow.Add(time.Hour).UnixMilli())
	king := evActivity(85000) // needMainCityLevel 12, HQ 10

	tasks := []*sfs.SFSObject{
		questTask("2201001", 1), questTask("2400000", 1), questTask("2500001", 1),
		questTask("8010011", 1), questTask("9010201", 1),
	}
	in := stageQuestsInit(10, tasks, lockhart, ended, early, rewardWindow, king)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runStageQuests(conn, in); err != nil {
		t.Fatal(err)
	}
	if got, want := stageQuestIDsSent(t, fake), []string{"2201001", "2400000"}; !slices.Equal(got, want) {
		t.Errorf("claimed %v, want %v", got, want)
	}
}

func TestStageQuestsFailuresAndNoInit(t *testing.T) {
	withEvNow(t, evTestNow)
	tasks := []*sfs.SFSObject{questTask("2201001", 1), questTask("2201002", 1), questTask("2201003", 1)}
	in := stageQuestsInit(30, tasks, evActivity(11))
	conn, fake := startEvFake(t, func(_ string, p *sfs.SFSObject) *sfs.SFSObject {
		switch p.GetString("id") {
		case "2201001":
			return evErr(evAlreadyExecuted)
		case "2201002":
			return evErr("E42")
		}
		return evOK()
	})
	err := runStageQuests(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E42") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E42 failure", err)
	}
	if n := len(fake.requests()); n != 3 {
		t.Errorf("sent %d claims, want 3 (a failed claim does not stop the others)", n)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runStageQuests(conn, &Init{}); err != nil {
		t.Errorf("no init: %v", err)
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("no init: sent %v", fake.cmds())
	}
}

func TestStageQuestsIsOptInAndStaticOnly(t *testing.T) {
	f, ok := featureRegistry[stageQuestsFeature]
	if !ok {
		t.Fatal("stage-quests is not registered")
	}
	if f.DefaultOn {
		t.Error("stage-quests is DefaultOn; a new claim must be opt-in until validated live")
	}
	if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("Summary = %q, want it to end with (static-only, not validated live)", f.Summary)
	}
}

// TestStageQuestRangesAreStageQuestsTheQuestsFeatureSkips: every id stage-quests claims is an
// activity_stage quest the ON quests feature leaves alone, so the two never claim the same id and
// the quests feature's claims are unchanged; Growth Foundation and Hero Trial ids stay out.
func TestStageQuestRangesAreStageQuestsTheQuestsFeatureSkips(t *testing.T) {
	n := 0
	for _, r := range stageQuestRanges {
		if r.lo > r.hi {
			t.Errorf("range %d-%d is empty", r.lo, r.hi)
		}
		for id := r.lo; id <= r.hi; id++ {
			n++
			if !questIsStageQuest(strconv.FormatInt(id, 10)) {
				t.Errorf("%d is not in questStageRanges", id)
			}
		}
	}
	// 70 Strongest Commander + 8 Lockhart + 5 King + 5 + 8 + 7 + 450 task-event + 7 TaskActivity2.
	if n != 560 {
		t.Errorf("stageQuestRanges cover %d ids, want 560", n)
	}
	for _, r := range [][2]int64{{8000001, 8000010}, {8000101, 8000112}, {8010101, 8010108}, {8010201, 8010208}, {8010301, 8010344}, {8010018, 8010018}} {
		for id := r[0]; id <= r[1]; id++ {
			if _, ok := stageQuestType(strconv.FormatInt(id, 10)); ok {
				t.Errorf("%d (Growth Foundation, Hero Trial or stage 200 only) is claimable", id)
			}
		}
	}
}
