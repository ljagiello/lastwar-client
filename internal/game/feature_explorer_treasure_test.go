package game

import (
	"strings"
	"testing"

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

func TestExplorerTreasureRegistration(t *testing.T) {
	f, ok := featureRegistry[explorerTreasurePlanFeature]
	if !ok || f.DefaultOn || f.Hold || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("%s registered as %+v; want opt-in, static-only", explorerTreasurePlanFeature, f)
	}
	// MASTER G57 / IMPLEMENT rule 9: opening a chest spends 5 keys (771001), so no feature sends
	// the open until the user decides.
	if _, ok := featureRegistry["explorer-treasure"]; ok {
		t.Error("explorer-treasure is registered; the chest open spends items and waits for the user's decision")
	}
}

func TestExplorerTreasurePlanSendsNothing(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, in := range map[string]*Init{
		"activity open, 500 keys": explorerInit(true, map[string]int{"771001": 500, "771002": 3}),
		"activity closed":         explorerInit(false, map[string]int{"771001": 50}),
	} {
		conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			t.Errorf("%s: the plan sent %s", name, cmd)
			return evErr("999999")
		})
		if err := runExplorerTreasurePlan(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := fake.cmds(); len(got) != 0 {
			t.Errorf("%s: the plan sent %v, want nothing", name, got)
		}
	}
}

func TestExplorerTreasureStateFollowsTheOpenButton(t *testing.T) {
	// UIExplorerTreasureView.lua:182-198: keys >= explorer_treasure_key k1 (5) and the type-251
	// activity open; the key box 771002 is only counted.
	withEvNow(t, evTestNow)
	hq4 := evInit(4, evActivity(94105)) // activity 94105 needMainCityLevel 5
	items, _ := explorerInit(true, map[string]int{"771001": 50}).Raw.Get("items")
	hq4.Raw.PutValue("items", items)
	for name, c := range map[string]struct {
		in   *Init
		want explorerTreasureState
	}{
		"12 keys, open":   {explorerInit(true, map[string]int{"771001": 12, "771002": 3}), explorerTreasureState{keys: 12, keyBoxes: 3, chests: 2, open: true}},
		"500 keys, open":  {explorerInit(true, map[string]int{"771001": 500}), explorerTreasureState{keys: 500, chests: 100, open: true}},
		"four keys":       {explorerInit(true, map[string]int{"771001": 4}), explorerTreasureState{keys: 4, open: true}},
		"only key boxes":  {explorerInit(true, map[string]int{"771002": 50}), explorerTreasureState{keyBoxes: 50, open: true}},
		"activity closed": {explorerInit(false, map[string]int{"771001": 50}), explorerTreasureState{keys: 50}},
		"HQ below 5":      {hq4, explorerTreasureState{keys: 50}},
	} {
		conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("999999") })
		if got := explorerTreasureStateOf(conn, c.in); got != c.want {
			t.Errorf("%s: state %+v, want %+v", name, got, c.want)
		}
	}
}
