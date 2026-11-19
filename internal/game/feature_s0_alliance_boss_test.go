package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// s0BossInit is fwTestInit (in an alliance) with the S0 alliance boss activity 94300 listed.
func s0BossInit() *Init {
	in := fwTestInit()
	in.Object("activity").PutSFSObject("94300", evActivity(94300))
	return in
}

// s0BossInfo is an alliance.boss.s0.act.info reply: difficulty 1 claimed, 2 and 3 claimable, 4 not
// earned, plus a donor list naming a player.
func s0BossInfo() *sfs.SFSObject {
	r := sfs.NewSFSObject()
	l := sfs.NewSFSArray()
	for _, e := range [][2]int32{{1, 2}, {2, 1}, {3, 1}, {4, 0}} {
		l.AddSFSObject(obj("difficultyLevel", e[0], "state", e[1]))
	}
	r.PutSFSArray("firstRewardList", l)
	donors := sfs.NewSFSArray()
	donors.AddSFSObject(obj("uid", fwAllyA, "name", "Name of "+fwAllyA))
	r.PutSFSArray("donateList", donors)
	r.PutInt("isSign", 1)
	return r
}

func startS0Fake(t *testing.T, claim func(p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case s0BossInfoCmd:
			return s0BossInfo()
		case s0BossFirstRewardCmd:
			return claim(p)
		}
		return evErr("unexpected command")
	})
}

func TestS0AllianceBossFirstRewards(t *testing.T) {
	withEvNow(t, evTestNow)
	logs := captureFwLogs(t)
	conn, fake := startS0Fake(t, func(p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
	if err := runS0AllianceBoss(conn, s0BossInit()); err != nil {
		t.Fatal(err)
	}
	info := fake.only(s0BossInfoCmd)
	if len(info) != 1 {
		t.Fatalf("sent %v", fake.cmds())
	}
	if v, _ := info[0].Params.Get("type"); v.Type != sfs.SFSInt || v.Val != int32(1) {
		t.Errorf("info params %s", info[0].Params.StringRedacted())
	}
	var levels []int32
	for _, c := range fake.only(s0BossFirstRewardCmd) {
		if v, _ := c.Params.Get("difficultyLevel"); v.Type != sfs.SFSInt || !slices.Equal(session.ParamKeysWithoutID(c.Params), []string{"difficultyLevel"}) {
			t.Errorf("claim params %s", c.Params.StringRedacted())
		}
		levels = append(levels, c.Params.GetInt("difficultyLevel"))
	}
	if !slices.Equal(levels, []int32{2, 3}) {
		t.Errorf("claimed %v, want [2 3]", levels)
	}
	if out := logs.String(); strings.Contains(out, fwAllyA) || !strings.Contains(out, "levelStates") {
		t.Errorf("log names a player or lacks the level states:\n%s", out)
	}
}

func TestS0AllianceBossGatesAndErrors(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startS0Fake(t, func(p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
	noAlliance := evInit(30, evActivity(94300))
	for _, in := range []*Init{nil, {}, fwTestInit(), noAlliance} {
		if err := runS0AllianceBoss(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v without an open S0 boss activity or an alliance", fake.cmds())
	}
	conn, fake = startS0Fake(t, func(*sfs.SFSObject) *sfs.SFSObject { return evErr("E_S0") })
	if err := runS0AllianceBoss(conn, s0BossInit()); err == nil || !strings.Contains(err.Error(), "E_S0") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.only(s0BossFirstRewardCmd)); n != 1 {
		t.Errorf("sent %d claims; the first refusal ends the run", n)
	}
	if typ, ok := activityTypeOf(94300); !ok || typ != s0BossActivityType {
		t.Errorf("table activity 94300 = type %d (%v)", typ, ok)
	}
	f, ok := featureRegistry[s0BossFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("registered %v, DefaultOn %v, summary %q", ok, f.DefaultOn, f.Summary)
	}
}
