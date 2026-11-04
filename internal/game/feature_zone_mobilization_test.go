package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// zoneRewards builds a rewardList of {target, rewarded}; rewarded is put as given (bool or int)
// or left out when nil.
func zoneRewards(rows ...[2]any) *sfs.SFSArray {
	l := sfs.NewSFSArray()
	for _, r := range rows {
		o := sfs.NewSFSObject()
		o.PutInt("target", int32(r[0].(int)))
		switch v := r[1].(type) {
		case bool:
			o.PutBool("rewarded", v)
		case int:
			o.PutInt("rewarded", int32(v))
		}
		o.PutSFSArray("reward", freebieReward())
		l.AddSFSObject(o)
	}
	return l
}

func zoneStageRewards(rows ...[2]int32) *sfs.SFSArray { // target (stage id), state
	l := sfs.NewSFSArray()
	for _, r := range rows {
		o := sfs.NewSFSObject()
		o.PutInt("target", r[0])
		o.PutInt("state", r[1])
		l.AddSFSObject(o)
	}
	return l
}

// zoneInfo is a get.zone.mobilization.info reply with every claim due: challenge row 2 reached,
// defence boxes 2 and 3 (boss 12, 4000 HP, at 1000 = 25%), both donation boxes, and the stage-3
// donation reward (progress full, stage 3 not received). withEnd false leaves endTime out.
func zoneInfo(withEnd bool) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	if withEnd {
		r.PutLong("endTime", evTestNow.Add(24*time.Hour).UnixMilli())
	}
	r.PutInt("stage", 3)
	r.PutInt("bossId", 13)
	r.PutLong("pointId", 555123)
	at := sfs.NewSFSObject()
	at.PutLong("damage", 5000)
	at.PutSFSArray("rewardList", zoneRewards([2]any{1000, true}, [2]any{5000, false}, [2]any{9000, false}))
	r.PutSFSObject("attackInfo", at)
	de := sfs.NewSFSObject()
	de.PutInt("bossId", 12)
	de.PutLong("bossPointId", 777)
	de.PutLong("curHp", 1000)
	de.PutSFSArray("rewardList", zoneRewards([2]any{20, nil}, [2]any{25, nil}, [2]any{50, false}, [2]any{75, true}))
	r.PutSFSObject("defendInfo", de)
	do := sfs.NewSFSObject()
	do.PutLong("donateProgress", 100)
	do.PutLong("maxProgress", 100)
	pers := sfs.NewSFSObject()
	pers.PutInt("boxNum", 2)
	do.PutSFSObject("personalDonateInfo", pers)
	al := sfs.NewSFSObject()
	al.PutInt("boxNum", 1)
	do.PutSFSObject("allianceDonateInfo", al)
	do.PutSFSArray("stageReward", zoneStageRewards([2]int32{1, 2}, [2]int32{2, 2}, [2]int32{3, 0}))
	r.PutSFSObject("donateInfo", do)
	red := sfs.NewSFSObject()
	red.PutBool("donate", true)
	r.PutSFSObject("showRedPoint", red)
	return r
}

func zoneInit(hq int32, alliance bool) *Init {
	in := evInit(hq)
	u := sfs.NewSFSObject()
	if alliance {
		u.PutUtfString("allianceId", "a1")
	}
	in.Raw.PutSFSObject("user", u)
	return in
}

func TestZoneMobilizationIsOptIn(t *testing.T) {
	f, ok := featureRegistry[zoneMobFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("zone-mobilization = %+v, want registered, off by default, static-only summary", f)
	}
}

func TestZoneMobilizationClaimsEveryFreeReward(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == zoneMobInfoCmd {
			return zoneInfo(true)
		}
		return evOK()
	})
	if err := runZoneMobilization(conn, zoneInit(30, true)); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(fake.cmds(), ",")
	want := strings.Join([]string{zoneMobInfoCmd, zoneMobChallengeCmd, zoneMobDefendCmd, zoneMobDefendCmd,
		zoneMobBoxCmd, zoneMobBoxCmd, zoneMobProgressCmd}, ",")
	if got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	assertInt := func(p *sfs.SFSObject, key string, want int32) {
		t.Helper()
		v, ok := p.Get(key)
		if !ok || v.Type != sfs.SFSInt || p.GetInt(key) != want {
			t.Errorf("%s = %v, want Int %d", key, p, want)
		}
	}
	assertInt(fake.only(zoneMobInfoCmd)[0].Params, "tabType", 4)
	assertInt(fake.only(zoneMobChallengeCmd)[0].Params, "index", 0)
	def := fake.only(zoneMobDefendCmd)
	assertInt(def[0].Params, "index", 2) // 25% <= 25
	assertInt(def[1].Params, "index", 3) // 25% <= 50; box 1 (20%) not reached, box 4 rewarded
	boxes := fake.only(zoneMobBoxCmd)
	assertInt(boxes[0].Params, "boxType", 1)
	assertInt(boxes[1].Params, "boxType", 2)
	for _, k := range fake.only(zoneMobProgressCmd)[0].Params.Keys() {
		if k != "_id" { // the request id the transport adds to every request
			t.Errorf("progress claim carries %q, want no params", k)
		}
	}
}

func TestZoneMobilizationGates(t *testing.T) {
	withEvNow(t, evTestNow)
	cases := []struct {
		name  string
		hq    int32
		ally  bool
		noEnd bool
		edit  func(r *sfs.SFSObject)
		wants []string
	}{
		{"HQ below zone_mobilization.k24", 5, true, false, nil, nil},
		{"event over", 30, true, false, func(r *sfs.SFSObject) { r.PutLong("endTime", evTestNow.Add(-time.Minute).UnixMilli()) },
			[]string{zoneMobInfoCmd}},
		{"no endTime", 30, true, true, nil, []string{zoneMobInfoCmd}},
		{"no alliance: personal box only", 30, false, false, nil,
			[]string{zoneMobInfoCmd, zoneMobChallengeCmd, zoneMobDefendCmd, zoneMobDefendCmd, zoneMobBoxCmd, zoneMobProgressCmd}},
		{"numeric rewarded is truthy, unknown boss sends no defence", 30, true, false, func(r *sfs.SFSObject) {
			at := evObject(r, "attackInfo")
			at.PutSFSArray("rewardList", zoneRewards([2]any{1000, 0}, [2]any{5000, 0}))
			evObject(r, "defendInfo").PutInt("bossId", 99)
		}, []string{zoneMobInfoCmd, zoneMobBoxCmd, zoneMobBoxCmd, zoneMobProgressCmd}},
		{"every stage reward received", 30, true, false, func(r *sfs.SFSObject) {
			evObject(r, "donateInfo").PutSFSArray("stageReward", zoneStageRewards([2]int32{1, 2}, [2]int32{3, 2}))
		}, []string{zoneMobInfoCmd, zoneMobChallengeCmd, zoneMobDefendCmd, zoneMobDefendCmd, zoneMobBoxCmd, zoneMobBoxCmd}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == zoneMobInfoCmd {
					r := zoneInfo(!c.noEnd)
					if c.edit != nil {
						c.edit(r)
					}
					return r
				}
				return evOK()
			})
			if err := runZoneMobilization(conn, zoneInit(c.hq, c.ally)); err != nil {
				t.Fatal(err)
			}
			if got, want := strings.Join(fake.cmds(), ","), strings.Join(c.wants, ","); got != want {
				t.Errorf("sent %s\nwant %s", got, want)
			}
		})
	}
}

func TestZoneMobilizationDefendDue(t *testing.T) {
	mk := func(bossID, pointID, curHP int64, targets ...int) *sfs.SFSObject {
		d := sfs.NewSFSObject()
		d.PutLong("bossId", bossID)
		d.PutLong("bossPointId", pointID)
		d.PutLong("curHp", curHP)
		var rows [][2]any
		for _, tg := range targets {
			rows = append(rows, [2]any{tg, false})
		}
		d.PutSFSArray("rewardList", zoneRewards(rows...))
		return d
	}
	cases := []struct {
		name string
		d    *sfs.SFSObject
		want []int32
	}{
		{"enemy boss not placed counts as 100%", mk(12, 0, 0, 50, 100), []int32{2}},
		{"boss dead", mk(12, 777, 0, 0, 50), []int32{1, 2}},
		{"float percent as the Lua computes it: 7/100*100 > 7", mk(51, 777, 70, 7, 8), []int32{2}},
		{"boss id 0", mk(0, 777, 0, 50), nil},
	}
	for _, c := range cases {
		if got := zoneMobDefendDue(c.d); !slices.Equal(got, c.want) {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestZoneMobilizationProgressDue(t *testing.T) {
	mk := func(progress, maxProgress int64, rows ...[2]int32) *sfs.SFSObject {
		d := sfs.NewSFSObject()
		d.PutLong("donateProgress", progress)
		d.PutLong("maxProgress", maxProgress)
		if rows != nil {
			d.PutSFSArray("stageReward", zoneStageRewards(rows...))
		}
		return d
	}
	cases := []struct {
		name  string
		d     *sfs.SFSObject
		stage int64
		want  bool
	}{
		{"no stage rewards counts as all received", mk(100, 100), 3, false},
		{"full progress, current stage not listed", mk(100, 100, [2]int32{1, 2}, [2]int32{2, 0}), 3, true},
		{"full progress, current stage received, none claimable", mk(100, 100, [2]int32{3, 2}, [2]int32{4, 0}), 3, false},
		{"partial progress, an earlier stage claimable", mk(10, 100, [2]int32{1, 1}, [2]int32{3, 0}), 3, true},
		{"partial progress, nothing claimable", mk(10, 100, [2]int32{1, 2}, [2]int32{3, 0}), 3, false},
	}
	for _, c := range cases {
		if got := zoneMobProgressDue(c.d, c.stage); got != c.want {
			t.Errorf("%s: due = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestZoneMobilizationClaimErrorStopsThatCommand(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case zoneMobInfoCmd:
			return zoneInfo(true)
		case zoneMobDefendCmd:
			return evErr("E91")
		}
		return evOK()
	})
	err := runZoneMobilization(conn, zoneInit(30, true))
	if err == nil || !strings.Contains(err.Error(), "E91") {
		t.Fatalf("err = %v, want the E91 defence failure", err)
	}
	if n := len(fake.only(zoneMobDefendCmd)); n != 1 {
		t.Errorf("sent %d defence claims, want 1 (the error stops that command)", n)
	}
	if n := len(fake.only(zoneMobProgressCmd)); n != 1 {
		t.Errorf("sent %d progress claims, want 1 (other claims go on)", n)
	}
}
