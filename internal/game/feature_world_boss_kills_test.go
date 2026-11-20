package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// worldBossInit is an Init with a BerserkBoss activity (80004) and init actBerserkBoss.array
// entries {uuid, times, hasReward?}; hasReward < 0 leaves it out.
func worldBossInit(entries ...[3]int64) *Init {
	in := evInit(30, evActivity(80004))
	arr := sfs.NewSFSArray()
	for _, e := range entries {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", e[0])
		o.PutInt("times", int32(e[1]))
		if e[2] >= 0 {
			o.PutBool("hasReward", e[2] == 1)
		}
		arr.AddSFSObject(o)
	}
	ab := sfs.NewSFSObject()
	ab.PutSFSArray("array", arr)
	in.Raw.PutSFSObject("actBerserkBoss", ab)
	return in
}

func worldBossMarches(bosses ...[2]int64) *sfs.SFSObject { // uuid, curHp
	r := sfs.NewSFSObject()
	l := sfs.NewSFSArray()
	for _, b := range bosses {
		o := sfs.NewSFSObject()
		o.PutLong("uuid", b[0])
		o.PutInt("bossId", 7)
		o.PutLong("curHp", b[1])
		o.PutLong("maxHp", 1000)
		l.AddSFSObject(o)
	}
	r.PutSFSArray("marches", l)
	return r
}

// G9: a dead boss (curHp <= 0) the player hit (times > 0) whose reward is not received sends
// berserk.boss.reward {uuid: Long} (UIUtil.lua:851-871; BerserkBossRewardMessage.lua:4-7).
func TestWorldBossKillsClaimsDeadBossesThePlayerHit(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == worldBossMarchesCmd {
			// 11 dead, hit, unclaimed; 12 alive; 13 dead, received; 14 dead, never hit; 15 dead, no entry;
			// 16 dead, hasReward absent
			return worldBossMarches([2]int64{11, 0}, [2]int64{12, 500}, [2]int64{13, 0}, [2]int64{14, -5},
				[2]int64{15, 0}, [2]int64{16, 0})
		}
		return evOK()
	})
	in := worldBossInit([3]int64{11, 2, 0}, [3]int64{12, 1, 0}, [3]int64{13, 3, 1}, [3]int64{14, 0, 0}, [3]int64{16, 1, -1})
	if err := runWorldBossKills(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.only(worldBossRewardCmd) {
		v, _ := r.Params.Get("uuid")
		if v.Type != sfs.SFSLong {
			t.Errorf("uuid %#v, want Long", v)
		}
		got = append(got, evString(r.Params, "uuid"))
	}
	if strings.Join(got, ",") != "11,16" {
		t.Errorf("claimed %v, want 11 and 16", got)
	}
	if c := fake.cmds(); c[0] != worldBossMarchesCmd {
		t.Errorf("first request %s, want the boss list", c[0])
	}
}

// G9: no type-307 activity, or away from home, sends nothing; an errorCode on the claim stops it.
func TestWorldBossKillsGates(t *testing.T) {
	withEvNow(t, evTestNow)
	reply := func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == worldBossMarchesCmd {
			return worldBossMarches([2]int64{11, 0}, [2]int64{12, 0})
		}
		return evErr(evAlreadyExecuted)
	}
	noActivity := worldBossInit([3]int64{11, 1, 0})
	noActivity.Raw.PutSFSObject("activity", sfs.NewSFSObject())
	away := worldBossInit([3]int64{11, 1, 0})
	u := sfs.NewSFSObject()
	u.PutInt("serverId", 1500)
	u.PutInt("crossFightSrcServerId", 1200)
	away.Raw.PutSFSObject("user", u)
	for name, in := range map[string]*Init{"no activity": noActivity, "away from home": away} {
		conn, fake := startEvFake(t, reply)
		if err := runWorldBossKills(conn, in); err != nil || len(fake.requests()) != 0 {
			t.Errorf("%s: err %v, sent %v; want nothing", name, err, fake.cmds())
		}
	}
	home := worldBossInit([3]int64{11, 1, 0}, [3]int64{12, 1, 0})
	u2 := sfs.NewSFSObject()
	u2.PutInt("serverId", 1200)
	u2.PutInt("crossFightSrcServerId", 1200)
	home.Raw.PutSFSObject("user", u2)
	conn, fake := startEvFake(t, reply)
	if err := runWorldBossKills(conn, home); err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure (no benign code)", err, evAlreadyExecuted)
	}
	if n := len(fake.only(worldBossRewardCmd)); n != 1 {
		t.Errorf("claims = %d, want 1: the first error stops", n)
	}
}
