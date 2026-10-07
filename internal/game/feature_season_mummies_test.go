package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func mummiesWaiting(in *Init, armyNum int64) *Init {
	w := sfs.NewSFSArray()
	e := sfs.NewSFSObject()
	e.PutLong("armyNum", armyNum)
	w.AddSFSObject(e)
	in.Raw.PutSFSArray("mummyWaitRecInfo", w)
	return in
}

func TestSeasonMummiesClaimsOnlyWithYardAndWaitingTroops(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })

	withYard := mummiesWaiting(seasonOutputInit(6), 40)
	withYard.Buildings = append(withYard.Buildings, NewTestBuilding(9, 851000, 1))
	if err := runSeasonMummies(conn, withYard); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != seasonMummyCmd {
		t.Fatalf("sent %q, want only %s", got, seasonMummyCmd)
	}

	noYard := mummiesWaiting(seasonOutputInit(6), 40)
	none := seasonOutputInit(6)
	none.Buildings = append(none.Buildings, NewTestBuilding(9, 851000, 1))
	for name, in := range map[string]*Init{"no yard": noYard, "nothing waiting": none, "out of season": evInit(30)} {
		before := len(fake.requests())
		if err := runSeasonMummies(conn, in); err != nil {
			t.Fatal(err)
		}
		if len(fake.requests()) != before {
			t.Errorf("%s: sent %v, want nothing", name, fake.cmds()[before:])
		}
	}
}

func TestSeasonMummiesIsOptIn(t *testing.T) {
	f, ok := featureRegistry["season-mummies"]
	if !ok || f.DefaultOn || !strings.HasPrefix(f.Summary, "OPT-IN:") {
		t.Errorf("season-mummies must be registered, default off and marked OPT-IN: %+v", f)
	}
}
