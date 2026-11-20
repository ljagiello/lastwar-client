package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// seasonScoreUser is a fight/battle info `user` with score, box targets and the opened 0-based
// box indexes.
func seasonScoreUser(score int64, targets []int64, opened []int32, kv ...any) *sfs.SFSObject {
	var boxes []*sfs.SFSObject
	for _, t := range targets {
		boxes = append(boxes, obj("target", t, "reward", []*sfs.SFSObject{}))
	}
	u := obj(append([]any{"score", score, "scoreBox", boxes}, kv...)...)
	u.PutIntArray("scoreRewardIndex", opened)
	return u
}

// boxIndexes returns the Int index of each cmd request, checking its type.
func boxIndexes(t *testing.T, fake *evFake, cmd string) []int32 {
	t.Helper()
	var out []int32
	for _, r := range fake.only(cmd) {
		seasonParamIs(t, r.Params, "index", sfs.SFSInt, r.Params.GetInt("index"))
		out = append(out, r.Params.GetInt("index"))
	}
	return out
}

// TestSeasonOutpostBoxes: score 500 reaches boxes 1-3 (targets 100, 300, 500); box 1 (index 0) is
// opened, so indexes 1 and 2 are claimed.
func TestSeasonOutpostBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonOutpostInfoCmd {
			return obj("user", seasonScoreUser(500, []int64{100, 300, 500, 900}, []int32{0}))
		}
		return obj("reward", []*sfs.SFSObject{})
	})
	if err := runSeasonOutpostBoxes(conn, evWithSeason(evInit(30, evActivity(1200108)), 6)); err != nil {
		t.Fatal(err)
	}
	if got := boxIndexes(t, fake, seasonOutpostBoxCmd); !slices.Equal(got, []int32{1, 2}) {
		t.Errorf("claimed %v, want [1 2]", got)
	}
}

func TestSeasonOutpostBoxesSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	noUser := func(string, *sfs.SFSObject) *sfs.SFSObject { return obj("outpostInfoList", []*sfs.SFSObject{}) }
	refused := func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonOutpostInfoCmd {
			return obj("user", seasonScoreUser(500, []int64{100, 300}, nil))
		}
		return evErr("E4")
	}
	for name, c := range map[string]struct {
		in      *Init
		reply   func(string, *sfs.SFSObject) *sfs.SFSObject
		want    string
		wantErr bool
	}{
		"no outpost activity": {evWithSeason(evInit(30), 6), noUser, "", false},
		"out of season":       {evInit(30, evActivity(1200108)), noUser, "", false},
		"reply without user":  {evWithSeason(evInit(30, evActivity(1200108)), 6), noUser, seasonOutpostInfoCmd, false},
		"a refused box stops": {evWithSeason(evInit(30, evActivity(1200108)), 6), refused,
			"outpost.battle.info,outpost.claim.box.reward", true},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			if err := runSeasonOutpostBoxes(conn, c.in); (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

// crossThroneSchedule is a cross.throne.schedule reply for a war that started an hour ago, of cfg
// type typ, with camps a and b.
func crossThroneSchedule(typ int32, a, b []int32) *sfs.SFSObject {
	group := sfs.NewSFSObject()
	group.PutIntArray("a", a)
	group.PutIntArray("b", b)
	return obj("startTime", evTestNow.Add(-3600e9).UnixMilli(), "endTime", evTestNow.Add(3600e9).UnixMilli(),
		"crossStartTime", evTestNow.Add(-3600e9).UnixMilli(), "curRound", 2, "cfg", obj("type", typ),
		"initServerGroup", obj("group", group))
}

// crossThroneFake answers the schedule and a fight info with rounds and user.
func crossThroneFake(t *testing.T, sched *sfs.SFSObject, rounds []*sfs.SFSObject, user *sfs.SFSObject) (*evFake, func() error) {
	t.Helper()
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case crossThroneScheduleCmd:
			return sched
		case crossThroneFightCmd:
			r := obj("curVsRound", rounds)
			if user != nil {
				r.PutSFSObject("user", user)
			}
			return r
		}
		return obj("reward", []*sfs.SFSObject{})
	})
	in := evInit(30)
	in.Raw.PutSFSObject("user", obj("serverId", 812))
	return fake, func() error { return runCrossThroneRewards(conn, in) }
}

func TestCrossThroneRewards(t *testing.T) {
	withEvNow(t, evTestNow)
	won := seasonScoreUser(0, []int64{100}, nil, "winServerReward", 0)
	for name, c := range map[string]struct {
		sched  *sfs.SFSObject
		rounds []*sfs.SFSObject
		user   *sfs.SFSObject
		want   string
	}{
		"own server won": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 900, "win", 0), obj("serverId", 812, "win", 1)}, won,
			"cross.throne.schedule,cross.throne.fight.info,cross.throne.claim.server.win.reward"},
		"own server lost": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 900, "win", 1), obj("serverId", 812, "win", 0)}, won,
			"cross.throne.schedule,cross.throne.fight.info"},
		"already claimed": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 812, "win", 1)}, seasonScoreUser(0, nil, nil, "winServerReward", 1),
			"cross.throne.schedule,cross.throne.fight.info"},
		"no own entry fails closed": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 900, "win", 1), obj("serverId", 901, "win", 1)}, won,
			"cross.throne.schedule,cross.throne.fight.info"},
		"serverId from campHeadServer": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("campHeadServer", 812, "win", 1)}, won,
			"cross.throne.schedule,cross.throne.fight.info,cross.throne.claim.server.win.reward"},
		"camp battle: own camp b won": {crossThroneSchedule(2, []int32{700, 701}, []int32{810, 812}),
			[]*sfs.SFSObject{obj("campId", 1, "serverId", 812, "win", 0), obj("campId", 2, "serverId", 810, "win", 1)}, won,
			"cross.throne.schedule,cross.throne.fight.info,cross.throne.claim.server.win.reward"},
		"camp battle: own camp a lost": {crossThroneSchedule(12, []int32{812}, []int32{900}),
			[]*sfs.SFSObject{obj("campId", 2, "win", 1), obj("campId", 1, "win", 0)}, won,
			"cross.throne.schedule,cross.throne.fight.info"},
		"score boxes": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 812, "win", 0)}, seasonScoreUser(250, []int64{100, 200, 300}, []int32{0}),
			"cross.throne.schedule,cross.throne.fight.info,cross.throne.claim.person.score.reward"},
		"not scheduled": {obj("cfg", obj("type", 4)), nil, nil, "cross.throne.schedule"},
		"not started": {func() *sfs.SFSObject {
			s := crossThroneSchedule(4, nil, nil)
			s.PutLong("startTime", evTestNow.Add(3600e9).UnixMilli())
			return s
		}(), nil, nil, "cross.throne.schedule"},
		"fight info without user": {crossThroneSchedule(4, nil, nil),
			[]*sfs.SFSObject{obj("serverId", 812, "win", 1)}, nil, "cross.throne.schedule,cross.throne.fight.info"},
	} {
		t.Run(name, func(t *testing.T) {
			fake, run := crossThroneFake(t, c.sched, c.rounds, c.user)
			if err := run(); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q\nwant %q", got, c.want)
			}
			if name == "score boxes" {
				if got := boxIndexes(t, fake, crossThroneBoxCmd); !slices.Equal(got, []int32{1}) {
					t.Errorf("boxes %v, want [1]", got)
				}
			}
		})
	}
}

func TestSeasonWarBoxFeaturesRegistered(t *testing.T) {
	for _, name := range []string{seasonOutpostFeature, crossThroneFeature} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s: registered %v, %+v", name, ok, f)
		}
	}
}
