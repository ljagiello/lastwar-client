package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// signInInfo is a 7-day calendar that started three days before evTestNow.
func signInInfo(states ...int32) *sfs.SFSObject {
	info := sfs.NewSFSObject()
	info.PutLong("startTime", evTestNow.Add(-3*24*time.Hour+time.Hour).UnixMilli())
	info.PutLong("endViewTime", evTestNow.Add(5*24*time.Hour).UnixMilli())
	days := sfs.NewSFSArray()
	for i, st := range states {
		d := sfs.NewSFSObject()
		d.PutInt("day", int32(i+1))
		d.PutInt("state", st)
		days.AddSFSObject(d)
	}
	info.PutSFSArray("dayArr", days)
	return info
}

func TestSignInClaimsAllWithDayZero(t *testing.T) {
	withEvNow(t, evTestNow)
	replies := map[string]*sfs.SFSObject{
		"5401": signInInfo(2, 2, 0, 0, 0, 0, 0), // day 3 reached and unclaimed: claim
		"5402": signInInfo(2, 2, 2, 0, 0, 0, 0), // days 1-3 claimed, day 4 not reached: skip
		"5403": signInInfo(2, 2, 2, 2, 2, 1, 0), // state 1: claim
	}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return replies[p.GetString("activityId")]
		}
		return evOK()
	})
	if err := runSignIn(conn, evInit(30, evActivity(5401), evActivity(5402), evActivity(5403))); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(signInClaimCmd)
	if len(claims) != 2 {
		t.Fatalf("sent %v, want claims for 5401 and 5403", fake.cmds())
	}
	for i, want := range []int32{5401, 5403} {
		if p := claims[i].Params; p.GetInt("activityId") != want || p.GetInt("day") != 0 || !p.Has("day") {
			t.Errorf("claim %d = %v, want {activityId:%d, day:0}", i, p, want)
		}
	}
}

func TestSignInEndedOrNoEndView(t *testing.T) {
	ended := signInInfo(0, 0)
	ended.PutLong("endViewTime", evTestNow.Add(-time.Second).UnixMilli())
	a := Activity{ID: 5401, Type: actTypeSignIn, Raw: sfs.NewSFSObject()}
	if signInClaimable(a, ended, evTestNow.UnixMilli()) {
		t.Error("a calendar past endViewTime must not be claimed")
	}
	noView := sfs.NewSFSObject()
	days := sfs.NewSFSArray()
	for _, d := range evObjects(signInInfo(1), "dayArr") {
		days.AddSFSObject(d)
	}
	noView.PutSFSArray("dayArr", days)
	if signInClaimable(a, noView, evTestNow.UnixMilli()) {
		t.Error("no endViewTime anywhere counts as ended")
	}
}

func TestSignInBenignAndFailure(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return signInInfo(1)
		}
		if p.GetInt("activityId") == 5401 {
			return evErr(evAlreadyExecuted)
		}
		return evErr("E1")
	})
	err := runSignIn(conn, evInit(30, evActivity(5401), evActivity(5402)))
	if err == nil || !strings.Contains(err.Error(), "E1") || strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want only the E1 failure", err)
	}
}
