package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func congratulations(count int32, entries ...[3]string) *sfs.SFSObject { // uid, configId, "liked"/"expired"/""
	r := sfs.NewSFSObject()
	r.PutInt("count", count)
	l := sfs.NewSFSArray()
	for _, e := range entries {
		o := sfs.NewSFSObject()
		o.PutUtfString("uid", e[0])
		o.PutInt("configId", 7)
		if e[1] != "" {
			o.PutUtfString("configId", e[1])
		}
		o.PutBool("selfThumb", e[2] == "liked")
		exp := evTestNow.Add(time.Hour)
		if e[2] == "expired" {
			exp = evTestNow.Add(-time.Hour)
		}
		o.PutLong("expireTimeStamp", exp.UnixMilli())
		l.AddSFSObject(o)
	}
	r.PutSFSArray("congratulationList", l)
	return r
}

func TestAllianceLikesLikesEligibleEntriesWhileTheyPay(t *testing.T) {
	withEvNow(t, evTestNow)
	left := int32(2)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == congratulationListCmd {
			return congratulations(left,
				[3]string{"me", "1", ""},     // own: skip
				[3]string{"a", "1", "liked"}, // already liked
				[3]string{"b", "1", "expired"},
				[3]string{"c", "", ""}, // configId as an Int on the wire
				[3]string{"c", "", ""}, // duplicate
				[3]string{"d", "2", ""},
				[3]string{"e", "3", ""}, // count runs out first
			)
		}
		left--
		r := sfs.NewSFSObject()
		r.PutInt("count", left)
		return r
	})
	if err := runAllianceLikes(conn, evAllianceInit()); err != nil {
		t.Fatal(err)
	}
	likes := fake.only(congratulationLikeCmd)
	if len(likes) != 2 {
		t.Fatalf("sent %v, want likes for c and d only", fake.cmds())
	}
	for i, want := range [][2]string{{"c", "7"}, {"d", "2"}} {
		p := likes[i].Params
		cfg, _ := p.Get("configId")
		if p.GetString("targetUid") != want[0] || cfg.Type != sfs.SFSUtfString || cfg.Val != want[1] || p.GetInt("type") != 1 {
			t.Errorf("like %d = %v, want {targetUid:%s, configId:UtfString %s, type:1}", i, p, want[0], want[1])
		}
	}
}

func TestAllianceLikesNoAllianceOrNoneLeft(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject {
		return congratulations(0, [3]string{"c", "1", ""})
	})
	if err := runAllianceLikes(conn, evInit(30)); err != nil || len(fake.requests()) != 0 {
		t.Errorf("no alliance: err %v, sent %v", err, fake.cmds())
	}
	if err := runAllianceLikes(conn, evAllianceInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != congratulationListCmd {
		t.Errorf("count 0: sent %s, want only the list", got)
	}
}

func TestAllianceLikesBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == congratulationListCmd {
			return congratulations(5, [3]string{"c", "1", ""}, [3]string{"d", "1", ""})
		}
		if p.GetString("targetUid") == "c" {
			return evErr(evAlreadyExecuted)
		}
		return evErr("E2")
	})
	err := runAllianceLikes(conn, evAllianceInit())
	if err == nil || !strings.Contains(err.Error(), "E2") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E2 failure", err)
	}
}
