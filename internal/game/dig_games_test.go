package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func TestDigsCheckRequest(t *testing.T) {
	obj := func(kv ...string) *sfs.SFSObject {
		p := sfs.NewSFSObject()
		for _, k := range kv {
			p.PutInt(k, 1)
		}
		return p
	}
	ok := []struct {
		cmd string
		p   *sfs.SFSObject
	}{
		{treasureHuntDigV2Cmd, obj("activityId", "digIndex")},
		{treasureHuntTierCmd, obj("aid")},
		{offSeasonDigListCmd, obj()},
		{seasonDigBlockCmd, obj("uuid", "pos")},
	}
	for _, c := range ok {
		if err := digsCheckRequest(c.cmd, c.p); err != nil {
			t.Errorf("%s: %v", c.cmd, err)
		}
	}
	bad := []struct {
		cmd  string
		p    *sfs.SFSObject
		want string
	}{
		{"buy.activity.dig.goods", obj("activityId", "num"), "purchase"},
		{"buy.activity.dig.v2.goods", obj("activityId", "num"), "purchase"},
		{"auto.activity.dig.v2", obj("activityId"), "not an allowed"},
		{"start.activity.batch.dig.v2", obj("activityId"), "not an allowed"},
		{"recommend.select.v2", obj("activityId", "select"), "not an allowed"},
		{treasureHuntDigCmd, obj("activityId", "digIndex", "useGold"), "refusing key"},
		{treasureHuntDigCmd, obj("activityId", "digIndex", "num"), "unexpected key"},
		{treasureHuntDigCmd, obj("activityId"), "missing"},
		{seasonDigOpenCmd, obj("type", "uuid", "uid", "pos", "gold"), "refusing key"},
	}
	for _, c := range bad {
		err := digsCheckRequest(c.cmd, c.p)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s %v: err = %v, want %q", c.cmd, c.p.Keys(), err, c.want)
		}
	}
	for cmd := range digsReadOnly {
		if _, ok := digsAllowedKeys[cmd]; !ok {
			t.Errorf("read-only %s is missing from the allowlist", cmd)
		}
	}
}

func TestDigsWriteNeverSendsARefusedRequest(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	r := newDigsRun(conn, digsInit(0), "test", false)
	p := sfs.NewSFSObject()
	p.PutInt("activityId", 1)
	p.PutInt("num", 5)
	if _, res := r.write("buy", "buy.activity.dig.goods", p); res != digsFailed {
		t.Errorf("buy write result = %v", res)
	}
	if r.err() == nil || !r.halt {
		t.Error("a refused request must record an error and halt the run")
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("sent %v", fake.cmds())
	}
	// A plan run may not send a write, and a write may not be a read.
	r = newDigsRun(conn, digsInit(0), "test", true)
	info := sfs.NewSFSObject()
	info.PutInt("activityId", 1)
	if _, res := r.write("info as write", treasureHuntInfoCmd, info); res != digsFailed || !strings.Contains(r.err().Error(), "is a read") {
		t.Errorf("info sent as a write: %v, %v", res, r.err())
	}
	r = newDigsRun(conn, digsInit(0), "test", false)
	if got := r.read("dig as read", treasureHuntDigCmd, sfs.NewSFSObject()); got != nil || !r.halt {
		t.Error("a write sent through read must be refused")
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("sent %v", fake.cmds())
	}
}

func TestDigsWriteCap(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	r := newDigsRun(conn, digsInit(0), "test", true)
	p := func() *sfs.SFSObject {
		o := sfs.NewSFSObject()
		o.PutInt("aid", 1)
		return o
	}
	for range digsMaxWrites {
		if _, res := r.write("claim", treasureHuntStoredCmd, p()); res != digsDone {
			t.Fatalf("write under the cap: %v", res)
		}
	}
	if _, res := r.write("claim", treasureHuntStoredCmd, p()); res != digsFailed || !r.halt {
		t.Error("the write past the cap must be refused and halt the run")
	}
}
