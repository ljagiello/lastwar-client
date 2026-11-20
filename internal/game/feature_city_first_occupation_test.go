package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// cityArrReply is a world.all.city.reward.info reply with the client's field names
// (OccupyRewardInfo.lua:21-43): 2001 has an unclaimed alliance reward, 2002 an unclaimed member
// reward, 2003 a claimed one, 2004 only stars (likes, never), 2005 a false flag; ranks and stars
// name other players.
func cityArrReply() *sfs.SFSObject {
	reward := func() *sfs.SFSArray {
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj("type", 0, "itemId", 200000, "count", 5))
		return a
	}
	rank := func() *sfs.SFSArray {
		a := sfs.NewSFSArray()
		a.AddSFSObject(obj("uid", fwAllyA, "name", "Name of "+fwAllyA))
		return a
	}
	city := func(id int32, fill func(o *sfs.SFSObject)) *sfs.SFSObject {
		o := sfs.NewSFSObject()
		o.PutInt("cityId", id)
		o.PutSFSArray("ranks", rank())
		fill(o)
		return o
	}
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(city(2001, func(o *sfs.SFSObject) {
		o.PutSFSArray("firstAllianceReward", reward())
		o.PutBool("rewardComplete", false)
	}))
	arr.AddSFSObject(city(2002, func(o *sfs.SFSObject) { o.PutSFSArray("firstReward", reward()) }))
	arr.AddSFSObject(city(2003, func(o *sfs.SFSObject) { o.PutSFSArray("firstReward", reward()); o.PutBool("rewardComplete", true) }))
	arr.AddSFSObject(city(2004, func(o *sfs.SFSObject) {
		stars := sfs.NewSFSArray()
		stars.AddSFSObject(obj("uuid", int64(77), "rewardComplete", 0, "uid", fwAllyB, "name", "Name of "+fwAllyB))
		o.PutSFSArray("starArr", stars)
	}))
	arr.AddSFSObject(city(2005, func(o *sfs.SFSObject) { o.PutBool("firstReward", false) }))
	r := sfs.NewSFSObject()
	r.PutSFSArray("cityArr", arr)
	return r
}

func startCityFake(t *testing.T, claim func(p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case cityRewardInfoCmd:
			return cityArrReply()
		case cityFirstOccClaimCmd:
			return claim(p)
		}
		return evErr("unexpected command")
	})
}

func TestCityFirstOccupationClaims(t *testing.T) {
	withFwPace(t)
	logs := captureFwLogs(t)
	conn, fake := startCityFake(t, func(p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
	if err := runCityFirstOccupation(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != cityRewardInfoCmd+","+cityFirstOccClaimCmd+","+cityFirstOccClaimCmd {
		t.Fatalf("sent %s", got)
	}
	var ids []int32
	for _, c := range fake.only(cityFirstOccClaimCmd) {
		v, _ := c.Params.Get("cityId")
		if v.Type != sfs.SFSInt || !slices.Equal(session.ParamKeysWithoutID(c.Params), []string{"cityId"}) {
			t.Errorf("claim params %s", c.Params.StringRedacted())
		}
		ids = append(ids, c.Params.GetInt("cityId"))
	}
	if !slices.Equal(ids, []int32{2001, 2002}) {
		t.Errorf("claimed %v", ids)
	}
	if p := fake.only(cityRewardInfoCmd)[0].Params; len(session.ParamKeysWithoutID(p)) != 0 {
		t.Errorf("info params %s", p.StringRedacted())
	}
	out := logs.String()
	for _, uid := range []string{fwAllyA, fwAllyB, fwLauncher} {
		if strings.Contains(out, uid) {
			t.Errorf("the log names player %s", uid)
		}
	}
	if !strings.Contains(out, "cities=5") || !strings.Contains(out, "claimable=2") {
		t.Errorf("log lacks the list summary:\n%s", out)
	}
}

func TestCityFirstOccupationGatesAndErrors(t *testing.T) {
	withFwPace(t)
	conn, fake := startCityFake(t, func(p *sfs.SFSObject) *sfs.SFSObject { return fwClaimOK(p) })
	noAlliance := evInit(30)
	for _, in := range []*Init{nil, {}, noAlliance} {
		if err := runCityFirstOccupation(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v outside an alliance", fake.cmds())
	}

	conn, fake = startCityFake(t, func(*sfs.SFSObject) *sfs.SFSObject { return evErr("E_CITY") })
	if err := runCityFirstOccupation(conn, fwTestInit()); err == nil || !strings.Contains(err.Error(), "E_CITY") {
		t.Errorf("err = %v", err)
	}
	if n := len(fake.only(cityFirstOccClaimCmd)); n != 1 {
		t.Errorf("sent %d claims; the first refusal ends the run", n)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E_INFO") })
	if err := runCityFirstOccupation(conn, fwTestInit()); err == nil || !strings.Contains(err.Error(), "E_INFO") {
		t.Errorf("err = %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{cityRewardInfoCmd}) {
		t.Errorf("sent %v", got)
	}
	f, ok := featureRegistry[cityFirstOccFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("registered %v, DefaultOn %v, summary %q", ok, f.DefaultOn, f.Summary)
	}
}

// TestCityFirstOccupationNeedsACityID: the claim sends the entry's cityId as an Int
// (WorldAllianceCityFirstOccupiedRewardMessage.lua:4-7). An entry with a free reward but no
// cityId, cityId 0, or one an Int cannot carry (it would wrap to 2002) is not claimed.
func TestCityFirstOccupationNeedsACityID(t *testing.T) {
	reward := []*sfs.SFSObject{obj("type", 0, "itemId", 200000, "count", 5)}
	ids := aCityRewardRun(t,
		obj("cityId", 2002, "firstReward", reward),
		obj("firstReward", reward),
		obj("cityId", 0, "firstReward", reward),
		obj("cityId", int64(1)<<32+2002, "firstReward", reward),
	)
	if !slices.Equal(ids, []int32{2002}) {
		t.Errorf("claimed %v, want [2002]", ids)
	}
}

// TestCityFirstOccupationClaimCap: 21 cities with a free reward, and a run claims 20
// (cityFirstOccMaxClaims, feature_city_first_occupation.go:39).
func TestCityFirstOccupationClaimCap(t *testing.T) {
	reward := []*sfs.SFSObject{obj("type", 0, "itemId", 200000, "count", 5)}
	var cities []*sfs.SFSObject
	for id := 3001; id <= 3021; id++ {
		cities = append(cities, obj("cityId", id, "firstReward", reward))
	}
	if ids := aCityRewardRun(t, cities...); len(ids) != 20 || ids[19] != 3020 {
		t.Errorf("claimed %d cities (%v), want 3001 to 3020", len(ids), ids)
	}
}
