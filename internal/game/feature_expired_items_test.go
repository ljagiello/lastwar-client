package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// expiredInit is an Init whose items hold {itemId, count} stacks.
func expiredInit(stacks map[int32]int32) *Init {
	in := evInit(30)
	a := sfs.NewSFSArray()
	ids := make([]int32, 0, len(stacks))
	for id := range stacks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		e := sfs.NewSFSObject()
		e.PutInt("itemId", id) // the client reads itemId whatever its type
		e.PutInt("count", stacks[id])
		a.AddSFSObject(e)
	}
	in.Raw.PutSFSArray("items", a)
	return in
}

// expiryEntry is one dataArr entry; timeType 0 leaves it out (the client's default 1) and para 0
// leaves para out.
func expiryEntry(id int32, expireSec int64, timeType, para int32) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutInt("id", id)
	e.PutLong("expireTime", expireSec)
	if timeType != 0 {
		e.PutInt("timeType", timeType)
	}
	if para != 0 {
		e.PutInt("para", para)
	}
	return e
}

func TestExpiredItemsDueUsesTheServerExpiryOnly(t *testing.T) {
	now := evTestNow.Unix()
	list := evOK()
	a := sfs.NewSFSArray()
	for _, e := range []*sfs.SFSObject{
		expiryEntry(620001, now-60, 0, 0),  // default timeType 1, expired
		expiryEntry(620005, now+60, 1, 0),  // not yet
		expiryEntry(641000, now, 3, 0),     // timeType 3, expires now
		expiryEntry(992002, now-60, 4, 10), // timeType 4, 3 owned < 10
		expiryEntry(992007, now-60, 4, 2),  // timeType 4, 5 owned >= 2
		expiryEntry(992008, now-60, 4, 0),  // timeType 4 without para: the client's comparison fails
		expiryEntry(653000, 0, 1, 0),       // no expiry
		expiryEntry(653001, now-60, 2, 0),  // a timeType IsExpiredNow does not expire
		expiryEntry(521050, now-60, 1, 0),  // times_type 7 row: left to the player
		expiryEntry(653003, now-60, 1, 0),  // not owned
	} {
		a.AddSFSObject(e)
	}
	list.PutSFSArray("dataArr", a)
	owned := expiredOwnedCounts(expiredInit(map[int32]int32{
		620001: 4, 620005: 1, 641000: 2, 992002: 3, 992007: 5, 992008: 1, 653000: 1, 653001: 1, 521050: 7, 200001: 9,
	}))
	if _, ok := owned[521050]; ok {
		t.Error("the times_type 7 row 521050 must not count as exchangeable")
	}
	got := expiredItemsDue(list, owned, now)
	want := []expiredItem{{620001, 4}, {641000, 2}, {992002, 3}}
	if !slices.Equal(got, want) {
		t.Errorf("due = %v, want %v", got, want)
	}
}

func TestExpiredItemsExchangeTheOwnedCount(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		switch {
		case cmd == expiredItemsListCmd:
			a := sfs.NewSFSArray()
			a.AddSFSObject(expiryEntry(620001, evTestNow.Unix()-1, 1, 0))
			a.AddSFSObject(expiryEntry(620012, evTestNow.Unix()-1, 1, 0))
			a.AddSFSObject(expiryEntry(620013, evTestNow.Unix()-1, 1, 0))
			r.PutSFSArray("dataArr", a)
		case cmd == expiredItemsRecoverCmd && p.GetString("goodsId") == "620012":
			return evErr("E500001")
		}
		return r
	})
	err := runExpiredItems(conn, expiredInit(map[int32]int32{620001: 6, 620012: 2, 620013: 1}))
	if err == nil || !strings.Contains(err.Error(), "E500001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	ex := fake.only(expiredItemsRecoverCmd)
	if len(ex) != 2 {
		t.Fatalf("exchanges = %+v, want two (the refusal of 620012 stops 620013)", ex)
	}
	p := ex[0].Params
	if v, _ := p.Get("goodsId"); v.Type != sfs.SFSUtfString || v.Val != "620001" {
		t.Errorf("goodsId = %#v, want UtfString 620001", v)
	}
	if v, _ := p.Get("num"); v.Type != sfs.SFSInt || p.GetInt("num") != 6 {
		t.Errorf("num = %#v, want Int 6", v)
	}

	conn, fake = startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runExpiredItems(conn, expiredInit(map[int32]int32{200001: 3})); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 0 {
		t.Errorf("no exchangeable item owned, sent %v", got)
	}
}
