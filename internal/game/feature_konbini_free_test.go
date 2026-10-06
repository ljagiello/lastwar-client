package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

func konbiniInit(extraFree, used int32, last time.Time, withBuilding bool) *Init {
	in := evInit(30)
	if withBuilding {
		in.Buildings = append(in.Buildings, NewTestBuilding(5, konbiniBuildingID, 1))
	}
	k := sfs.NewSFSObject()
	k.PutInt("freeBuyCount", used)
	k.PutLong("lastBuyTime", last.UnixMilli())
	in.Raw.PutSFSObject("konbiniInfo", k)
	eff := sfs.NewSFSObject()
	eff.PutInt("30025", extraFree)
	in.Raw.PutSFSObject("effect", eff)
	return in
}

func TestKonbiniFreeOnlyWhenAFreeBuyIsProvablyLeft(t *testing.T) {
	withEvNow(t, evTestNow)
	today := evTestTomorrow.Add(-time.Hour)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	for _, c := range []struct {
		name string
		in   *Init
		want int
	}{
		{"free left today", konbiniInit(2, 1, today, true), 1},
		{"used up today", konbiniInit(1, 1, today, true), 0},
		{"counter is yesterday's", konbiniInit(1, 1, yesterday, true), 1},
		{"no extra-free effect: para2 unknown", konbiniInit(0, 0, yesterday, true), 0},
		{"no konbini building (1.0.364)", konbiniInit(2, 0, yesterday, false), 0},
	} {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runKonbiniFree(conn, c.in); err != nil {
			t.Fatal(err)
		}
		reqs := fake.only(konbiniBuyCmd)
		if len(reqs) != c.want {
			t.Errorf("%s: sent %d buys, want %d", c.name, len(reqs), c.want)
			continue
		}
		for _, r := range reqs {
			v, _ := r.Params.Get("useFree")
			if v.Type != sfs.SFSBool || v.Val != true || r.Params.GetInt("index") != 1 {
				t.Errorf("%s: buy = %v, want {index:1, useFree:Bool true}", c.name, r.Params)
			}
		}
	}
}

func TestKonbiniFreeBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(evAlreadyExecuted) })
	if err := runKonbiniFree(conn, konbiniInit(1, 0, yesterday, true)); err != nil {
		t.Errorf("benign: %v", err)
	}
	conn2, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E1") })
	if err := runKonbiniFree(conn2, konbiniInit(1, 0, yesterday, true)); err == nil || !strings.Contains(err.Error(), "E1") {
		t.Errorf("failure: %v", err)
	}
}
