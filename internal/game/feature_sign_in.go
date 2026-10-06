package game

import (
	"fmt"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Sign-in calendars (MASTER.md §4 #15), static-only. Activity type 158; hero.event.info.get returns
// dayArr[{day, state}] (0 not yet claimed, 1 claimable, 2 claimed), startTime and endViewTime (ms)
// (LWActSignInManager.lua:9-18). sign.receive.day.reward {activityId: Int, day: Int 0} claims every
// claimable day, missed days included; the client sends it while GetCanClaimRewardDay() > 0 and
// the calendar has not passed endViewTime (LWActSignInManager.lua:28-45).
const (
	signInClaimCmd = "sign.receive.day.reward"
	signInDayMs    = 86400000
)

func init() {
	registerFeature(Feature{
		Name:    "sign-in",
		Summary: "claim every claimable day of running sign-in calendars (day 0 = all); static-only",
		Run:     runSignIn,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, signInClaimCmd)
}

func runSignIn(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeSignIn) {
		if b.dead {
			break
		}
		info, err := s.EventInfo(a)
		if err != nil {
			b.add(err)
			continue
		}
		if !signInClaimable(a, info, evNow().UnixMilli()) {
			continue
		}
		p := evActivityIDParam(a)
		p.PutInt("day", 0)
		b.send(fmt.Sprintf("sign-in days (activity %d)", a.ID), signInClaimCmd, p)
	}
	return b.err()
}

// signInClaimable is LWActSignInInfo:IsEnd and GetCanClaimRewardDay (LWActSignInInfo.lua:42-65):
// now is not past endViewTime (a calendar with none counts as ended) and some day is in state 1,
// or in state 0 and already reached (day <= days since startTime + 1). The reply's times win; the
// init entry's fill in when the reply has none.
func signInClaimable(a Activity, info *sfs.SFSObject, now int64) bool {
	start, ok := evNum(info, "startTime")
	if !ok {
		start = a.StartTime()
	}
	endView, ok := evNum(info, "endViewTime")
	if !ok {
		endView = a.EndViewTime()
	}
	if endView <= 0 || now > endView {
		return false
	}
	reached := (now - start) / signInDayMs
	for _, d := range evObjects(info, "dayArr") {
		state, _ := evNum(d, "state")
		day, _ := evNum(d, "day")
		if state == 1 || (state == 0 && reached >= day-1) {
			return true
		}
	}
	return false
}
