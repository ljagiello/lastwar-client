package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// allianceInit is an Init in an alliance, with switches on and the star ceremony running.
func allianceInit(switches ...string) *Init {
	in := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("allianceId", "fake-alliance")
	u.PutUtfString("uid", "me")
	in.Raw.PutSFSObject("user", u)
	dc := sfs.NewSFSObject()
	for _, s := range switches {
		dc.PutUtfString(s, "1")
	}
	in.Raw.PutSFSObject("dataConfig", dc)
	in.Raw.PutLong("allianceStarEndTime", evTestNow.Add(time.Hour).UnixMilli())
	return in
}

func starCeremony(participated bool, scratch *bool) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutBool("participateReceive", participated)
	if scratch != nil {
		r.PutBool("scratchClaimed", *scratch)
	}
	return r
}

func TestAllianceExtrasQuestThenScratch(t *testing.T) {
	withEvNow(t, evTestNow)
	no := false
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case allianceStarActivityCmd:
			r := sfs.NewSFSObject()
			r.PutBool("hasCeremony", true)
			return r
		case allianceStarInfoCmd:
			return starCeremony(false, &no)
		}
		return evOK()
	})
	if err := runAllianceExtras(conn, allianceInit("alliance_star")); err != nil {
		t.Fatal(err)
	}
	want := "alliance.star.gain.activity.info.new,alliance.star.gain.ceremony.info.new," +
		"alliance.star.ceremony.quest.reward.new,alliance.star.gain.scratch.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Errorf("sent %s\nwant %s", got, want)
	}
}

func TestAllianceExtrasNothingDue(t *testing.T) {
	withEvNow(t, evTestNow)
	yes := true
	for _, c := range []struct {
		name string
		in   *Init
		info *sfs.SFSObject
		want string
	}{
		{"switch off", allianceInit(), starCeremony(false, nil), ""},
		{"ceremony over", func() *Init {
			in := allianceInit("alliance_star")
			in.Raw.PutLong("allianceStarEndTime", evTestNow.Add(-time.Hour).UnixMilli())
			return in
		}(), starCeremony(false, nil), ""},
		{"all claimed", func() *Init {
			in := allianceInit("alliance_star")
			in.Raw.PutBool("allianceStarNotify", true)
			return in
		}(), starCeremony(true, &yes), "alliance.star.gain.ceremony.info.new"},
		{"no scratch card", func() *Init {
			in := allianceInit("alliance_star")
			in.Raw.PutBool("allianceStarNotify", true)
			return in
		}(), starCeremony(true, nil), "alliance.star.gain.ceremony.info.new"},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return c.info })
		if err := runAllianceExtras(conn, c.in); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(fake.cmds(), ","); got != c.want {
			t.Errorf("%s: sent %q, want %q", c.name, got, c.want)
		}
	}
}

func TestAllianceExtrasBenignQuestStillScratches(t *testing.T) {
	withEvNow(t, evTestNow)
	no := false
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case allianceStarInfoCmd:
			return starCeremony(false, &no)
		case allianceStarQuestCmd:
			return evErr(evAlreadyExecuted)
		}
		return evErr("E4")
	})
	in := allianceInit("alliance_star")
	in.Raw.PutBool("allianceStarNotify", true)
	err := runAllianceExtras(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E4") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the scratch E4 failure", err)
	}
	if len(fake.only(allianceStarScratchCmd)) != 1 {
		t.Error("an already-claimed quest reward must still lead to the scratch claim")
	}
}
