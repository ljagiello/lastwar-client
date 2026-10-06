package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func questTask(id string, state int32) *sfs.SFSObject {
	t := sfs.NewSFSObject()
	t.PutUtfString("id", id)
	t.PutInt("state", state)
	return t
}

func questInit(tasks []*sfs.SFSObject, chapterState string, subTasks string, subs []*sfs.SFSObject) *Init {
	raw := sfs.NewSFSObject()
	ta := sfs.NewSFSArray()
	for _, t := range tasks {
		ta.AddSFSObject(t)
	}
	raw.PutSFSArray("task", ta)
	ch := sfs.NewSFSObject()
	ch.PutUtfString("chapterid", "3")
	ch.PutUtfString("state", chapterState)
	ch.PutUtfString("subTasks", subTasks)
	sa := sfs.NewSFSArray()
	for _, s := range subs {
		sa.AddSFSObject(s)
	}
	w := sfs.NewSFSObject()
	w.PutSFSObject("chapterTask", ch)
	w.PutSFSArray("chapterSubTaskArray", sa)
	raw.PutSFSObject("chapterTask", w)
	return &Init{Raw: raw}
}

func TestQuestsClaimsFollowUpsSubTasksThenChapter(t *testing.T) {
	in := questInit(
		[]*sfs.SFSObject{questTask("1001", 1), questTask("1002", 0), questTask("2201003", 1)},
		"0", "501|502",
		[]*sfs.SFSObject{questTask("501", 2), questTask("502", 1)},
	)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == questRewardCmd && p.GetString("id") == "1001" {
			r := sfs.NewSFSObject()
			next := sfs.NewSFSArray()
			next.AddSFSObject(questTask("1003", 1)) // a follow-up that is already done
			r.PutSFSArray("tasks", next)
			return r
		}
		return evOK()
	})
	if err := runQuests(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests() {
		v, _ := r.Params.Get(map[string]string{questRewardCmd: "id", chapterTaskCmd: "chapterid"}[r.Cmd])
		if v.Type != sfs.SFSUtfString {
			t.Errorf("%s param type %d, want UtfString", r.Cmd, v.Type)
		}
		got = append(got, r.Cmd+":"+v.Val.(string))
	}
	want := "task.reward.get:502,task.reward.get:1001,task.reward.get:1003,chapter.task:3"
	if strings.Join(got, ",") != want {
		t.Errorf("sent %s\nwant %s (the Growth Foundation stage quest 2201003 is left out)", strings.Join(got, ","), want)
	}
}

func TestQuestsChapterNotReady(t *testing.T) {
	for _, c := range []struct {
		name  string
		state string
		subs  []*sfs.SFSObject
	}{
		{"sub-task still open", "0", []*sfs.SFSObject{questTask("501", 2), questTask("502", 0)}},
		{"chapter already claimed", "1", []*sfs.SFSObject{questTask("501", 2), questTask("502", 2)}},
	} {
		in := questInit(nil, c.state, "501|502", c.subs)
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runQuests(conn, in); err != nil {
			t.Fatal(err)
		}
		if n := len(fake.requests()); n != 0 {
			t.Errorf("%s: sent %v, want nothing", c.name, fake.cmds())
		}
	}
}

func TestQuestsBenignFailureAndNoInit(t *testing.T) {
	in := questInit([]*sfs.SFSObject{questTask("1", 1), questTask("2", 1)}, "1", "", nil)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if p.GetString("id") == "1" {
			return evErr(evAlreadyExecuted)
		}
		return evErr("E42")
	})
	err := runQuests(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E42") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E42 failure", err)
	}
	if n := len(fake.requests()); n != 2 {
		t.Errorf("sent %d, want 2", n)
	}
	if err := runQuests(nil, &Init{}); err != nil {
		t.Errorf("no init: %v", err)
	}
}
