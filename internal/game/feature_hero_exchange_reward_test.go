package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// G8: an open type-323 activity whose entry's receiveReward is absent or Bool false sends
// lw.season.hero.receive.switch.reward {activityId: Int} once; true, or a number (truthy in Lua),
// does not (ActExchangeHeroDataManager.lua:11-22; UIHeroExchangeGuideView.lua:150-168).
func TestHeroExchangeRewardClaimsUnreceived(t *testing.T) {
	withEvNow(t, evTestNow)
	mk := func(id int32, set func(*sfs.SFSObject)) *sfs.SFSObject {
		a := evActivity(id)
		set(a)
		return a
	}
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	in := evInit(30,
		mk(1030002, func(*sfs.SFSObject) {}),                                      // absent: claim
		mk(1030003, func(a *sfs.SFSObject) { a.PutBool("receiveReward", false) }), // false: claim
		mk(1200024, func(a *sfs.SFSObject) { a.PutBool("receiveReward", true) }),  // received
		mk(1200066, func(a *sfs.SFSObject) { a.PutInt("receiveReward", 0) }),      // a number is truthy in Lua
	)
	if err := runHeroExchangeReward(conn, in); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range fake.only(heroExchangeRewardCmd) {
		if v, _ := r.Params.Get("activityId"); v.Type != sfs.SFSInt {
			t.Errorf("activityId %#v, want Int", v)
		}
		ids = append(ids, evString(r.Params, "activityId"))
	}
	if got := strings.Join(ids, ","); got != "1030002,1030003" {
		t.Errorf("claimed %s, want 1030002,1030003", got)
	}
	if n := len(fake.requests()); n != 2 {
		t.Errorf("sent %d requests, want only the 2 claims", n)
	}
}

// G8: the claim registers no benign code: an errorCode stops the feature.
func TestHeroExchangeRewardErrorStops(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(evAlreadyExecuted) })
	err := runHeroExchangeReward(conn, evInit(30, evActivity(1030002), evActivity(1030003)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake.requests()); n != 1 {
		t.Errorf("sent %d claims, want 1", n)
	}
}
