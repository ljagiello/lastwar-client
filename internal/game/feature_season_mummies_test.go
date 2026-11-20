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

// TestSeasonMummiesSeason7Yard pins B20: Season 7 (MummyV2) uses yard 858000; S6's yard there is
// a past season's building and does not count.
func TestSeasonMummiesSeason7Yard(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, c := range map[string]struct {
		yard, lv int32
		want     string
	}{
		"S7 yard 858000":           {858000, 1, seasonMummyCmd},
		"only the S6 yard 851000":  {851000, 1, ""},
		"S7 yard not built (lv 0)": {858000, 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
			in := mummiesWaiting(evWithSeason(evInit(30), 7), 40)
			in.Buildings = append(in.Buildings, NewTestBuilding(9, c.yard, c.lv))
			if err := runSeasonMummies(conn, in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

func TestSeasonMummiesIsOptIn(t *testing.T) {
	f, ok := featureRegistry["season-mummies"]
	if !ok || f.DefaultOn || !strings.HasPrefix(f.Summary, "OPT-IN:") {
		t.Errorf("season-mummies must be registered, default off and marked OPT-IN: %+v", f)
	}
}
