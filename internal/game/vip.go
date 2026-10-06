package game

import (
	"errors"
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

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
// errorMsg="no score" -- not a protocol error, so a claim that is no longer
// available is harmless. claimVIPDailies decides when to send it.
func ClaimVIPDailyLoginScore(conn *session.GameConn) error {
	const cmd = "vip.add.login.score"
	_, err := session.SendAndWait(conn, "vip daily login score response", cmd, sfs.NewSFSObject())
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
// sfs.SFSObject, matching the real client's own captured request having no
// params either).
//
// Available once per day: replaying the identical call through this Go
// client, on an account that had already claimed it today via the real
// client, got a real, well-formed response -- errorCode=120289,
// errorMsg="no reward" -- the same error code family as the login score
// above. claimVIPDailies decides when to send it.
func ClaimVIPDailyFreebie(conn *session.GameConn) error {
	const cmd = "vip.get.every.day.reward"
	_, err := session.SendAndWait(conn, "vip daily freebie response", cmd, sfs.NewSFSObject())
	return err
}

// vipFlags are the vipInfo fields the real client gates the two daily claims on
// (DataCenter/VIPData/VIPDataInfo.lua:161-200).
type vipFlags struct {
	loginScoreState int32 // 1 = today's login score is claimable
	everyDayReward  int32 // 1 = today's free pack is claimable
	endTime         int64 // VIP expiry, unix seconds; the free pack needs VIP active
}

// parseVIPFlags reads a vipInfo object, or reports ok=false when it is missing or carries neither
// claim flag.
func parseVIPFlags(info *sfs.SFSObject) (vipFlags, bool) {
	if info == nil || (!info.Has("loginScoreState") && !info.Has("everyDayReward")) {
		return vipFlags{}, false
	}
	return vipFlags{
		loginScoreState: info.GetInt("loginScoreState"),
		everyDayReward:  info.GetInt("everyDayReward"),
		endTime:         info.GetLong("endTime"),
	}, true
}

// vipActive is VIPDataInfo:IsVIPActive: endTime set and still in the future.
func (f vipFlags) vipActive(now time.Time) bool {
	return f.endTime > 0 && now.Unix() < f.endTime
}

// claimVIPDailies sends the two daily VIP claims the way the real client gates them
// (VIPDataInfo.lua:161-200, CanGetDailyPoint / CanGetDailyFreeReward):
//
//   - vip.add.login.score only when vipInfo.loginScoreState == 1;
//   - vip.get.every.day.reward only when vipInfo.everyDayReward == 1 and VIP is active
//     (endTime, in seconds, still in the future).
//
// The flags come from init.vip.vipInfo. The client also treats flags received before today's
// server-day reset as stale (claimable); here init is stale when the run has crossed init's own
// `tomorrow` reset, and then, or when init carried no vipInfo, the flags are refreshed with
// `vip.info {}` (its reply carries vipInfo, VipInfoMessage.lua). If that refresh fails, stale flags
// count as claimable (the client's rule) and missing flags fall back to sending both claims.
// Without init (in.Raw nil) both claims are sent unconditionally, as before this gating existed;
// errorCode 120289 stays benign either way. The live init showed both flags 0 after a claim
// (capture §3); 1 meaning claimable is from the Lua only.
func claimVIPDailies(conn *session.GameConn, in *Init, now time.Time) error {
	login, freebie := true, true
	var errs []error
	if in != nil && in.Raw != nil {
		var err error
		login, freebie, err = vipEligibility(conn, in, now)
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			return err
		}
	}
	if login {
		err := ClaimVIPDailyLoginScore(conn)
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			return errors.Join(errs...)
		}
	}
	if freebie {
		errs = append(errs, ClaimVIPDailyFreebie(conn))
	}
	return errors.Join(errs...)
}

// vipEligibility decides the two claims from init (see claimVIPDailies). err is the vip.info
// refresh's error, returned only so the caller can stop on a dead connection.
func vipEligibility(conn *session.GameConn, in *Init, now time.Time) (login, freebie bool, err error) {
	flags, ok := parseVIPFlags(objectField(in.Object("vip"), "vipInfo"))
	tomorrow := in.Raw.GetLong("tomorrow")
	stale := ok && tomorrow > 0 && now.Unix() >= tomorrow
	if !ok || stale {
		slog.Info("refreshing VIP flags with vip.info", "initHadFlags", ok, "initStale", stale)
		var msg *session.ExtensionMessage
		msg, err = session.SendAndWait(conn, "vip info response", "vip.info", sfs.NewSFSObject())
		fresh, freshOK := vipFlags{}, false
		if err == nil && !msg.Params.Has("errorCode") {
			fresh, freshOK = parseVIPFlags(objectField(msg.Params, "vipInfo"))
		}
		if freshOK {
			flags, stale = fresh, false
		} else if !ok {
			slog.Info("no VIP flags in init or vip.info; sending both VIP claims")
			return true, true, err
		}
	}
	if stale {
		// VIPDataInfo:CanGetDailyPoint/CanGetDailyFreeReward: flags from before today's reset
		// count as claimable.
		login, freebie = true, flags.vipActive(now)
	} else {
		login, freebie = flags.loginScoreState == 1, flags.everyDayReward == 1 && flags.vipActive(now)
	}
	slog.Info("VIP daily claim eligibility", "loginScoreState", flags.loginScoreState, "everyDayReward", flags.everyDayReward,
		"vipEndTime", flags.endTime, "stale", stale, "claimLoginScore", login, "claimFreebie", freebie)
	return login, freebie, err
}
