package game

import (
	"slices"
	"testing"

	"lastwar-client/internal/sfs"
)

type dailyQuest struct {
	id    string
	state int32
}

// dailyQuestsList builds a daily.quest.ls reply with the five live chest thresholds.
func dailyQuestsList(quests []dailyQuest, claimedChests ...int32) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	qs := sfs.NewSFSArray()
	for _, q := range quests {
		o := sfs.NewSFSObject()
		o.PutUtfString("id", q.id)
		o.PutInt("state", q.state)
		qs.AddSFSObject(o)
	}
	resp.PutSFSArray("dailyQuest", qs)
	chests := sfs.NewSFSArray()
	for _, p := range []int32{40, 80, 120, 160, 200} {
		o := sfs.NewSFSObject()
		o.PutInt("point", p)
		o.PutSFSArray("info", sfs.NewSFSArray())
		chests.AddSFSObject(o)
	}
	resp.PutSFSArray("rewardList", chests)
	cur := sfs.NewSFSArray()
	for _, c := range claimedChests {
		cur.AddInt(c)
	}
	resp.PutSFSArray("curReward", cur)
	return resp
}

func dailyQuestsRun(t *testing.T, list *sfs.SFSObject, taskReply, chestReply *sfs.SFSObject) (*rewardFake, error) {
	t.Helper()
	conn, fake := startRewardFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case dailyQuestsListCmd:
			return list
		case dailyQuestsTaskCmd:
			return taskReply
		}
		return chestReply
	})
	return fake, runDailyQuests(conn, rewardInit(func(*sfs.SFSObject) {}))
}

func TestDailyQuestsClaimsTasksThenChests(t *testing.T) {
	// 30 points already received + 50 (120) + 10 (101) claimed now = 90: chest 1 (40) is already
	// taken, chest 2 (80) becomes reachable.
	list := dailyQuestsList([]dailyQuest{
		{"121", 2}, {"120", 1}, {"101", 1}, {"108", 1}, {"102", 0},
	}, 1)
	fake, err := dailyQuestsRun(t, list, rewardOK(), rewardOK())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{dailyQuestsListCmd, dailyQuestsTaskCmd, dailyQuestsTaskCmd, dailyQuestsChestCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	reqs := fake.requests()
	for i, wantID := range []string{"120", "101"} {
		v, _ := reqs[i+1].Params.Get("taskId")
		if id, ok := v.Val.(string); !ok || id != wantID {
			t.Errorf("task claim %d taskId = %#v, want UtfString %q", i, v.Val, wantID)
		}
	}
	if stage := reqs[3].Params.GetInt("stage"); stage != -1 {
		t.Errorf("chest claim stage = %d, want -1", stage)
	}
}

func TestDailyQuestsNothingToClaim(t *testing.T) {
	// 30 points, every reachable chest below that already claimed: only the list read goes out.
	list := dailyQuestsList([]dailyQuest{{"121", 2}, {"101", 0}})
	fake, err := dailyQuestsRun(t, list, rewardOK(), rewardOK())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{dailyQuestsListCmd}) {
		t.Errorf("sent %v, want only the list read", got)
	}

	list = dailyQuestsList([]dailyQuest{{"119", 2}}, 1) // 40 points, chest 1 already claimed
	fake, err = dailyQuestsRun(t, list, rewardOK(), rewardOK())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{dailyQuestsListCmd}) {
		t.Errorf("sent %v, want only the list read", got)
	}
}

func TestDailyQuestsAlreadyClaimedIsBenign(t *testing.T) {
	list := dailyQuestsList([]dailyQuest{{"119", 2}, {"101", 1}})
	fake, err := dailyQuestsRun(t, list, rewardErr("120289"), rewardErr("120289"))
	if err != nil {
		t.Errorf("run = %v, want nil for 120289", err)
	}
	// The benign task reply does not count as received, but 40 points still reach chest 1.
	want := []string{dailyQuestsListCmd, dailyQuestsTaskCmd, dailyQuestsChestCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestDailyQuestsFailures(t *testing.T) {
	list := dailyQuestsList([]dailyQuest{{"101", 1}, {"102", 1}})
	fake, err := dailyQuestsRun(t, list, rewardErr("999999"), rewardOK())
	if err == nil {
		t.Error("run = nil, want the task failures")
	}
	// Both tasks are tried; with 0 points no chest claim follows.
	want := []string{dailyQuestsListCmd, dailyQuestsTaskCmd, dailyQuestsTaskCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}

	_, err = dailyQuestsRun(t, rewardErr("999999"), rewardOK(), rewardOK())
	if err == nil {
		t.Error("run = nil, want the list failure")
	}
}

func TestDailyQuestsNoInitSendsNothing(t *testing.T) {
	conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := runDailyQuests(conn, &Init{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("sent %v without an init push", got)
	}
}
