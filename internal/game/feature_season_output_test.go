package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func seasonOutputInit(season int32, acts ...*sfs.SFSObject) *Init {
	in := evWithSeason(evInit(30, acts...), season)
	u := sfs.NewSFSObject()
	u.PutUtfString("allianceId", "fake-alliance")
	in.Raw.PutSFSObject("user", u)
	return in
}

func seasonRewardInfo(entries ...[3]int32) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, e := range entries {
		o := sfs.NewSFSObject()
		o.PutInt("id", e[0])
		o.PutInt("serverId", e[1])
		o.PutInt("leftNum", e[2])
		a.AddSFSObject(o)
	}
	return a
}

func TestSeasonOutputS6(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		switch cmd {
		case seasonCityInfoCmd:
			r.PutSFSArray("rewardInfo", seasonRewardInfo([3]int32{1, 783, 0}, [3]int32{2, 783, 3}))
		case seasonStrongholdInfoCmd:
			r.PutSFSArray("rewardInfo", seasonRewardInfo([3]int32{11, 783, 0}, [3]int32{12, 790, 2}))
		case seasonCampInfoCmd:
			prod := sfs.NewSFSArray()
			for _, c := range [][3]int32{{5, 783, 10}, {6, 783, 4}} { // cityId, serverId, num
				o := sfs.NewSFSObject()
				o.PutInt("cityId", c[0])
				o.PutInt("serverId", c[1])
				o.PutInt("num", c[2])
				prod.AddSFSObject(o)
			}
			r.PutSFSArray("campProductArr", prod)
			recv := sfs.NewSFSArray()
			o := sfs.NewSFSObject()
			o.PutInt("cityId", 6)
			o.PutInt("receiveNum", 4)
			recv.AddSFSObject(o)
			r.PutSFSArray("userCampCityRewardArr", recv)
		default:
			return evOK()
		}
		return r
	})
	in := seasonOutputInit(6, evActivity(1200104))
	in.Buildings = append(in.Buildings, NewTestBuilding(9, 851000, 1))
	w := sfs.NewSFSArray()
	e := sfs.NewSFSObject()
	e.PutLong("armyNum", 40)
	w.AddSFSObject(e)
	in.Raw.PutSFSArray("mummyWaitRecInfo", w)
	if err := runSeasonOutput(conn, in); err != nil {
		t.Fatal(err)
	}
	// Mummies are waiting, but season-output never sends lw.season.mummy.get (see season-mummies).
	want := "lw.season.alliance.city.occupy.info,batch.get.city.output," +
		"lw.season.city.stronghold.occupy.info,get.stronghold.output,camp.product.view,get.camp.product.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	sh := fake.only(seasonStrongholdCmd)[0].Params
	if sh.GetInt("serverId") != 790 || sh.GetInt("strongholdId") != 12 {
		t.Errorf("stronghold claim = %v, want {serverId:790, strongholdId:12}", sh)
	}
	camp := fake.only(seasonCampRewardCmd)[0].Params
	v, _ := camp.Get("serverCityArr")
	arr, _ := v.Val.(*sfs.SFSArray)
	if arr == nil || len(arr.Items()) != 1 || camp.Has("isShake") == false {
		t.Fatalf("camp claim = %v, want one city and isShake", camp)
	}
	if c := arr.Items()[0].Val.(*sfs.SFSObject); c.GetInt("cityId") != 5 || c.GetInt("serverId") != 783 {
		t.Errorf("camp city = %v, want city 5 on 783 (city 6 is fully received)", c)
	}
}

func TestSeasonOutputOutOfSeasonNoAllianceS1(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runSeasonOutput(conn, evInit(30)); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests()) != 0 {
		t.Errorf("out of season sent %v", fake.cmds())
	}
	noAlliance := evWithSeason(evInit(30), 5)
	if err := runSeasonOutput(conn, noAlliance); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests()) != 0 {
		t.Errorf("no alliance and no mummies sent %v", fake.cmds())
	}
	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		if cmd == seasonAttachInfoCmd {
			l := sfs.NewSFSArray()
			e := sfs.NewSFSObject()
			e.PutInt("leftNum", 2)
			l.AddSFSObject(e)
			r.PutSFSArray("rewardList", l)
		}
		return r
	})
	if err := runSeasonOutput(conn2, seasonOutputInit(1, evActivity(1000058))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake2.cmds(), ","); got != "city.attachment.list,city.attachment.rec.all.output" {
		t.Errorf("S1 sent %s", got)
	}
}

func TestSeasonOutputBenignRefreshAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case seasonStrongholdInfoCmd:
			r := sfs.NewSFSObject()
			r.PutSFSArray("rewardInfo", seasonRewardInfo([3]int32{1, 783, 1}, [3]int32{2, 783, 1}, [3]int32{3, 783, 1}))
			return r
		case seasonStrongholdCmd:
			switch p.GetInt("strongholdId") {
			case 1:
				return evErr(seasonStrongholdRefresh)
			case 2:
				return evErr(evAlreadyExecuted)
			}
			return evErr("E8")
		}
		return evOK()
	})
	err := runSeasonOutput(conn, seasonOutputInit(3))
	if err == nil || !strings.Contains(err.Error(), "E8") || strings.Contains(err.Error(), seasonStrongholdRefresh) {
		t.Errorf("err = %v, want only the E8 failure", err)
	}
	if n := len(fake.only(seasonStrongholdCmd)); n != 3 {
		t.Errorf("sent %d stronghold claims, want 3", n)
	}
}
