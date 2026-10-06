package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// battlePassInfo builds a pass reply at level with the given per-level normalState rows and
// claimable task ids.
func battlePassInfo(level, unlock int32, normal []int32, special []int32, tasks map[string]int32) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	bp := sfs.NewSFSObject()
	bp.PutInt("level", level)
	bp.PutInt("unlock", unlock)
	info.PutSFSObject("battlePass", bp)
	rows := sfs.NewSFSArray()
	for i, st := range normal {
		r := sfs.NewSFSObject()
		r.PutInt("level", int32(i+1))
		r.PutInt("normalState", st)
		r.PutInt("specialState", special[i])
		rows.AddSFSObject(r)
	}
	info.PutSFSArray("stateInfo", rows)
	ta := sfs.NewSFSArray()
	for id, st := range tasks {
		tk := sfs.NewSFSObject()
		tk.PutUtfString("taskId", id)
		tk.PutInt("state", st)
		ta.AddSFSObject(tk)
	}
	info.PutSFSArray("taskArr", ta)
	return info
}

func TestBattlePassV1TasksThenLevels(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case battlePassInfoCmd:
			// level 1 claimed, level 2 not reached yet; the task claim levels the pass to 2
			return battlePassInfo(1, 0, []int32{1, 0, 0}, []int32{0, 0, 0}, map[string]int32{"301": 1})
		case battlePassTaskCmd:
			r := sfs.NewSFSObject()
			bp := sfs.NewSFSObject()
			bp.PutInt("level", 2)
			r.PutSFSObject("battlePass", bp)
			return r
		}
		return evOK()
	})
	if err := runBattlePass(conn, evInit(30, evActivity(13))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.battlepass.info,receive.battlepass.task.reward,receive.battlepass.all.reward" {
		t.Fatalf("sent %s", got)
	}
	info := fake.only(battlePassInfoCmd)[0].Params
	if v, _ := info.Get("activityId"); v.Type != sfs.SFSInt || info.GetInt("activityId") != 13 {
		t.Errorf("info activityId = %#v, want Int 13", v)
	}
	task := fake.only(battlePassTaskCmd)[0].Params
	if v, _ := task.Get("taskId"); v.Type != sfs.SFSInt || task.GetInt("taskId") != 301 {
		t.Errorf("v1 taskId = %#v, want Int 301", v)
	}
}

func TestBattlePassV2UsesEventInfoAndUtfTaskID(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return battlePassInfo(3, 0, []int32{1, 1, 0}, []int32{0, 0, 0}, map[string]int32{"t9": 1})
		}
		return evOK()
	})
	if err := runBattlePass(conn, evInit(30, evActivity(5038))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "hero.event.info.get,bp.task.reward,receive.bpv2.all.reward" {
		t.Fatalf("sent %s", got)
	}
	if v, _ := fake.only(battlePassV2TaskCmd)[0].Params.Get("taskId"); v.Type != sfs.SFSUtfString || v.Val != "t9" {
		t.Errorf("v2 taskId = %#v, want UtfString t9", v)
	}
}

func TestBattlePassSkipsWhenOnlyLockedPremiumRows(t *testing.T) {
	withEvNow(t, evTestNow)
	replies := map[int32]*sfs.SFSObject{
		// free rows all claimed, premium rows unclaimed but the pass was not bought: nothing to send
		13: battlePassInfo(2, 0, []int32{1, 1}, []int32{0, 0}, map[string]int32{"1": 2}),
		// the same, with the pass bought: its premium rows are a free claim
		19: battlePassInfo(2, 1, []int32{1, 1}, []int32{0, 0}, nil),
	}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == battlePassInfoCmd {
			return replies[p.GetInt("activityId")]
		}
		return evOK()
	})
	if err := runBattlePass(conn, evInit(30, evActivity(13), evActivity(19))); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(battlePassAllCmd)
	if len(claims) != 1 || claims[0].Params.GetInt("activityId") != 19 {
		t.Errorf("claim-all sent for %v, want only activity 19", fake.cmds())
	}
	if len(fake.only(battlePassTaskCmd)) != 0 {
		t.Error("a state-2 task (free half claimed) must not be sent")
	}
}

func TestBattlePassBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case battlePassInfoCmd:
			return battlePassInfo(1, 0, []int32{0}, []int32{0}, nil)
		case battlePassAllCmd:
			if p.GetInt("activityId") == 13 {
				return evErr(evAlreadyExecuted)
			}
			return evErr("E7")
		}
		return evOK()
	})
	err := runBattlePass(conn, evInit(30, evActivity(13), evActivity(19)))
	if err == nil || !strings.Contains(err.Error(), "E7") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E7 failure", err)
	}
}
