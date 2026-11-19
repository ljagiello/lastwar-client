package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// assistInit is an HQ-10 init with DispatchTask activity 94101, in an alliance when allied is set.
func assistInit(allied bool) *Init {
	return rewardInitWithDay(10, func(raw *sfs.SFSObject) {
		acts := sfs.NewSFSObject()
		acts.PutSFSObject("94101", sfs.NewSFSObject())
		raw.PutSFSObject("activity", acts)
		if allied {
			raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}
	})
}

// assistList is a hero.dispatch.alliance.list reply: ls[] entries {uuid, cfgId, completionTime,
// rewarded, targetServer} (DispatchGetAllianceTasksMessage.lua:9-17, DispatchTaskItem.lua:486).
func assistList(now time.Time) *sfs.SFSObject {
	ls := sfs.NewSFSArray()
	for _, e := range []struct {
		done     time.Duration
		rewarded int
	}{{-time.Hour, 0}, {-time.Minute, 0}, {-time.Hour, 1}, {time.Hour, 0}} {
		ls.AddSFSObject(duelObj("uuid", int64(100), "cfgId", 500101, "completionTime", now.Add(e.done).UnixMilli(),
			"rewarded", e.rewarded, "targetServer", 1234))
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("ls", ls)
	return r
}

func TestSecretTaskAssistPlanOnlyReads(t *testing.T) {
	f, ok := featureRegistry[secretTaskAssistPlanFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("%s registered as %+v; want an opt-in read-only plan", secretTaskAssistPlanFeature, f)
	}
	now := time.Now()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case secretTasksListCmd:
			return duelObj("todayAssistNum", 4)
		case secretTaskAllianceListCmd:
			return assistList(now)
		}
		return rewardErr("999999")
	})
	if err := runSecretTaskAssistPlan(conn, assistInit(true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{secretTasksListCmd, secretTaskAllianceListCmd}) {
		t.Errorf("sent %v, want only the two lists (never an assist)", got)
	}
	// The map gate: finished (completionTime in the past) and not rewarded.
	n := 0
	for _, e := range claimObjects(assistList(now), "ls") {
		if secretTaskAssistable(e, now.UnixMilli()) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d assistable entries, want 2", n)
	}
}

func TestSecretTaskAssistPlanGates(t *testing.T) {
	for name, in := range map[string]*Init{
		"no alliance": assistInit(false),
		"no activity": rewardInitWithDay(10, func(raw *sfs.SFSObject) { raw.PutSFSObject("alliance", duelObj("uid", "a1")) }),
		"HQ 4": rewardInitWithDay(4, func(raw *sfs.SFSObject) {
			acts := sfs.NewSFSObject()
			acts.PutSFSObject("94101", sfs.NewSFSObject())
			raw.PutSFSObject("activity", acts)
			raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}),
	} {
		conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSecretTaskAssistPlan(conn, in); err != nil || len(fake.cmds()) != 0 {
			t.Errorf("%s: sent %v (err %v), want nothing", name, fake.cmds(), err)
		}
	}
}
