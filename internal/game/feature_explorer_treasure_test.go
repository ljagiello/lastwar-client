package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// explorerInit is an HQ-10 init with activity 94105 (type 251) open when open is set, and the
// given init items {uuid, itemId, count}.
func explorerInit(open bool, items map[string]int) *Init {
	var in *Init
	if open {
		in = evInit(10, evActivity(94105))
	} else {
		in = evInit(10)
	}
	arr := sfs.NewSFSArray()
	for id, n := range items {
		arr.AddSFSObject(duelObj("uuid", "u"+id, "itemId", id, "count", n))
	}
	in.Raw.PutSFSArray("items", arr)
	return in
}

// explorerReply is an ExplorerTreasureOpenMessage reply: the reward, quality and the guarantee
// counter (ExplorerTreasureOpenMessage.lua:8-19).
func explorerReply() *sfs.SFSObject {
	r := duelObj("quality", 3, "dispatch_explorer_treasure_guarantee", 4)
	rw := sfs.NewSFSArray()
	rw.AddSFSObject(duelObj("type", 7, "itemId", 200220, "num", 10))
	r.PutSFSArray("reward", rw)
	return r
}

func runExplorerFake(t *testing.T, in *Init, reply func(n int) *sfs.SFSObject) ([]evReq, []time.Duration, error) {
	t.Helper()
	withEvNow(t, evTestNow)
	var slept []time.Duration
	saved := explorerTreasureSleep
	explorerTreasureSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { explorerTreasureSleep = saved })
	n := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd != explorerTreasureCmd {
			t.Errorf("sent %s; only the chest open is allowed", cmd)
			return evErr("999999")
		}
		if len(p.Keys()) > 1 {
			t.Errorf("open params %v, want none", p)
		}
		n++
		return reply(n)
	})
	err := runExplorerTreasure(conn, in)
	return fake.requests(), slept, err
}

func TestExplorerTreasureRegistration(t *testing.T) {
	for _, name := range []string{explorerTreasureFeature, explorerTreasurePlanFeature} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || f.Hold || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s registered as %+v; want opt-in, static-only", name, f)
		}
	}
}

func TestExplorerTreasurePlanSendsNothing(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return explorerReply() })
	if err := runExplorerTreasurePlan(conn, explorerInit(true, map[string]int{"771001": 12})); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("the plan sent %v, want nothing", got)
	}
}

func TestExplorerTreasureOpensOneChestPerFiveKeys(t *testing.T) {
	// 12 keys: two chests, a second apart; the key box 771002 is only logged.
	reqs, slept, err := runExplorerFake(t, explorerInit(true, map[string]int{"771001": 12, "771002": 3}),
		func(int) *sfs.SFSObject { return explorerReply() })
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 {
		t.Errorf("sent %d opens, want 2", len(reqs))
	}
	if len(slept) != 1 || slept[0] < time.Second {
		t.Errorf("slept %v, want one pause of at least a second between the opens", slept)
	}
}

func TestExplorerTreasureGates(t *testing.T) {
	for name, in := range map[string]*Init{
		"activity closed": explorerInit(false, map[string]int{"771001": 50}),
		"four keys":       explorerInit(true, map[string]int{"771001": 4}),
		"only key boxes":  explorerInit(true, map[string]int{"771002": 50}),
		"HQ below 5": func() *Init {
			i := evInit(4, evActivity(94105)) // activity 94105 needMainCityLevel 5
			items, _ := explorerInit(true, map[string]int{"771001": 50}).Raw.Get("items")
			i.Raw.PutValue("items", items)
			return i
		}(),
	} {
		reqs, _, err := runExplorerFake(t, in, func(int) *sfs.SFSObject { return explorerReply() })
		if err != nil || len(reqs) != 0 {
			t.Errorf("%s: sent %d opens (err %v), want none", name, len(reqs), err)
		}
	}
}

func TestExplorerTreasureCapAndStopOnError(t *testing.T) {
	reqs, _, err := runExplorerFake(t, explorerInit(true, map[string]int{"771001": 500}),
		func(int) *sfs.SFSObject { return explorerReply() })
	if err != nil || len(reqs) != explorerTreasureMaxPerRun {
		t.Errorf("sent %d opens (err %v), want the cap of %d", len(reqs), err, explorerTreasureMaxPerRun)
	}
	// No benign codes: the first errorCode stops the run with an error.
	reqs, _, err = runExplorerFake(t, explorerInit(true, map[string]int{"771001": 15}), func(n int) *sfs.SFSObject {
		if n == 2 {
			return evErr("370100")
		}
		return explorerReply()
	})
	if err == nil || !strings.Contains(err.Error(), "370100") || len(reqs) != 2 {
		t.Errorf("sent %d opens, err %v; want a stop after the 370100 failure on the second", len(reqs), err)
	}
}
