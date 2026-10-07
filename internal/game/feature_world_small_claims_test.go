package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func evNotice(uuid int64, status int32, content string) *sfs.SFSObject {
	n := sfs.NewSFSObject()
	n.PutLong("uuid", uuid)
	n.PutInt("rewardStatus", status)
	n.PutUtfString("content", content)
	return n
}

func TestNoticeClaimable(t *testing.T) {
	plain := `{"b":{"reward":{"rewardInfo":[{"type":1,"num":5}]}}}`
	cases := []struct {
		name string
		n    *sfs.SFSObject
		hq   int32
		want bool
	}{
		{"plain reward", evNotice(1, 0, plain), 1, true},
		{"claimed", evNotice(1, 1, plain), 1, false},
		{"no reward", evNotice(1, 0, `{"b":{"content":"hi"}}`), 1, false},
		{"bad json", evNotice(1, 0, `{`), 1, false},
		{"older version, HQ reached", evNotice(1, 0, `{"b":{"reward":{"rewardVersion":"1.0.300","rewardLevel":10}}}`), 10, true},
		{"older version, HQ too low", evNotice(1, 0, `{"b":{"reward":{"rewardVersion":"1.0.300","rewardLevel":10}}}`), 9, false},
		{"newer version", evNotice(1, 0, `{"b":{"reward":{"rewardVersion":"1.0.400","rewardLevel":1}}}`), 30, false},
		{"versioned without a level", evNotice(1, 0, `{"b":{"reward":{"rewardVersion":"1.0.1"}}}`), 30, false},
	}
	for _, c := range cases {
		if got := noticeClaimable(c.n, c.hq); got != c.want {
			t.Errorf("%s: claimable = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestWorldSmallClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case noticeListCmd:
			l := sfs.NewSFSArray()
			l.AddSFSObject(evNotice(900, 0, `{"b":{"reward":{"rewardInfo":[]}}}`))
			l.AddSFSObject(evNotice(901, 1, `{"b":{"reward":{}}}`))
			r.PutSFSArray("noticeList", l)
		case trendsInfoCmd:
			l := sfs.NewSFSArray()
			for _, tr := range []struct {
				id          string
				rs, st, lim int32
			}{{"t1", 2, 4, 5}, {"t2", 3, 4, 5}, {"t3", 2, 4, 99}, {"t4", 2, 1, 5}} {
				o := sfs.NewSFSObject()
				o.PutUtfString("id", tr.id)
				o.PutInt("rewardStatus", tr.rs)
				o.PutInt("status", tr.st)
				o.PutInt("levelLimit", tr.lim)
				l.AddSFSObject(o)
			}
			r.PutSFSArray("trendsInfo", l)
		case bossCompInfoCmd:
			r.PutInt("rewardCount", 1)
		default:
			return evOK()
		}
		return r
	})
	in := evInit(30)
	in.Raw.PutInt("whistle_reward", 2)
	mons := sfs.NewSFSArray()
	for _, m := range [][3]int32{{7, 2, 1}, {8, 1, 1}, {9, 2, 0}} { // id, state, reward
		o := sfs.NewSFSObject()
		o.PutInt("pveMonsterId", m[0])
		o.PutInt("state", m[1])
		o.PutInt("reward", m[2])
		mons.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("pveMonsters", mons)
	if err := runWorldSmallClaims(conn, in); err != nil {
		t.Fatal(err)
	}
	want := "claim.whistle.box.reward,get.notice.list,receice.notice.reward,server.trends.info,server.trends.reward," +
		"receive.pve.monster.reward,berserk.boss.hit.base.gain.info,berserk.boss.hit.base.gain.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if v, _ := fake.only(noticeRewardCmd)[0].Params.Get("uuid"); v.Type != sfs.SFSLong || v.Val != int64(900) {
		t.Errorf("notice uuid = %#v, want Long 900", v)
	}
	if fake.only(trendsRewardCmd)[0].Params.GetString("id") != "t1" {
		t.Error("only trend t1 is claimable")
	}
	if fake.only(pveMonsterRewardCmd)[0].Params.GetInt("pveMonsterId") != 7 {
		t.Error("only monster 7 is finished with a reward")
	}
}

func TestWorldSmallClaimsNothingDueBenignFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == bossCompInfoCmd {
			r := sfs.NewSFSObject()
			r.PutInt("rewardCount", 1)
			return r
		}
		if cmd == bossCompRewardCmd {
			return evErr(evAlreadyExecuted)
		}
		if cmd == trendsInfoCmd {
			return evErr("E5")
		}
		return evOK()
	})
	in := evInit(30)
	err := runWorldSmallClaims(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E5") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E5 failure", err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.notice.list,server.trends.info,berserk.boss.hit.base.gain.info,berserk.boss.hit.base.gain.reward" {
		t.Errorf("sent %s", got)
	}
}
