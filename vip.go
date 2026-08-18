package main

// ClaimVIPDailyLoginScore sends `vip.add.login.score` -- confirmed live via
// a real packet capture of the actual game client's VIP screen "Collect"
// action on the daily login-streak bonus (200 VIP points/day at VIP12,
// scaling with consecutive login days -- the real client's own captured
// session showed a "CONGRATULATIONS!" popup reading "Today's VIP Points:
// 200, Consecutive Login Days: 322, Next Day's VIP Points: 200"). Genuinely
// parameterless
// (extracted/lua_decompiled/5809_Net_Msgs_Vip_VipAddLoginScoreMessage.lua's
// OnCreate takes nothing beyond self, matching the real client's own
// captured request having no params at all).
//
// Available once per day: replaying the identical call through this Go
// client, on an account that had already claimed it today via the real
// client, got a real, well-formed response -- errorCode=120289,
// errorMsg="no score" -- not a protocol error, so it's safe to call
// unconditionally on every run and let the server say no when there's
// nothing left to claim.
func ClaimVIPDailyLoginScore(conn *GameConn) error {
	const cmd = "vip.add.login.score"
	_, err := sendAndWait(conn, "vip daily login score response", cmd, NewSFSObject())
	return err
}

// ClaimVIPDailyFreebie sends `vip.get.every.day.reward` -- confirmed live
// via a real packet capture of the actual game client's VIP screen
// "Claim" button on the "VIP<level> Daily Freebie" chest -- a genuinely
// separate reward from the login score above (both showed up as two
// distinct actions in the same capture, on the same VIP screen). Also
// parameterless on the wire
// (extracted/lua_decompiled/5810_Net_Msgs_Vip_VipGetEveryDayRewardMessage.lua's
// OnCreate declares an `actId` argument but never actually puts it on the
// SFSObject, matching the real client's own captured request having no
// params either).
//
// Available once per day: replaying the identical call through this Go
// client, on an account that had already claimed it today via the real
// client, got a real, well-formed response -- errorCode=120289,
// errorMsg="no reward" -- the same error code family as the login score
// above, so it's likewise safe to call unconditionally on every run.
func ClaimVIPDailyFreebie(conn *GameConn) error {
	const cmd = "vip.get.every.day.reward"
	_, err := sendAndWait(conn, "vip daily freebie response", cmd, NewSFSObject())
	return err
}
