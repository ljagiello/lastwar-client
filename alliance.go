package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// HelpAllianceMembers sends `al.help.all` ("help all") -- confirmed live
// via a real packet capture of the actual game client tapping the
// alliance help-all button. Bulk-completes every pending alliance-member
// help request (construction/research speedups other members requested)
// in one call, mirroring the Lua handler's own request-construction logic
// (extracted/lua_decompiled/4368_Net_Msgs_Alliance_AlHelpAllMessage.lua):
// the only field actually put on the wire is `cmdBaseTime`, a Long --
// everything else OnCreate takes (helpBtnPos, toPos, isOnlyDisperse,
// isOnlyShowDiff) is purely local UI-animation state, never sent. The real
// client sent an absolute Unix-epoch-milliseconds value
// (`cmdBaseTime=1783114317664`); this sends the equivalent live value via
// `time.Now().UnixMilli()`.
//
// The captured response fell outside the successfully-decoded portion of
// that capture (the same stream-reassembly artifact seen with the Truck
// Rewards and visitor.operate captures), so the live response shape has
// not been directly observed. Reading the Lua handler itself: success is
// "no errorCode", carrying an optional `accPoint` (accumulated
// alliance-help point total) -- the call is unconditional and shows a
// generic "helped" tip even when nothing was actually pending, so it's
// safe to call on every run regardless of whether any help requests
// currently exist.
func HelpAllianceMembers(conn *GameConn) error {
	const cmd = "al.help.all"
	params := NewSFSObject()
	params.PutLong("cmdBaseTime", time.Now().UnixMilli())
	_, err := sendAndWait(conn, "alliance help-all response", cmd, params)
	return err
}

// ClaimAllianceGifts sends `alliance.reward.allreceive` -- confirmed live
// via a real packet capture of the actual game client tapping "Claim All"
// on the Alliance Gifts panel. Like HelpAllianceMembers, this is a true
// bulk claim needing only a `type` field, no per-gift uids
// (extracted/lua_decompiled/4428_Net_Msgs_Alliance_AllianceReceiveAllGiftMessage.lua's
// OnCreate takes only `type`, PutInt directly). The panel has two
// independently-claimed tabs, confirmed via the handler's own tip-string
// branch (`type == 1` -> locale key alliance_system025 "Premium Alliance
// Gifts", else -> alliance_system024 "Regular Alliance Gifts") -- so
// `type=1` is Premium and `type=2` is Regular. Only `type=2` (Regular) was
// actually captured live (tips banner afterward read "2 Regular Alliance
// Gifts were claimed"); `type=1` is sent on the strength of that same
// Lua branch, not independently packet-captured.
//
// Reading the handler further: success carries `receiveResult`, `results`
// (per-gift uuid/receiveTime pairs), `receiveNum`, and `reward` -- but the
// call is only meaningfully separated from a no-op by `receiveResult == 1`
// and a non-empty `reward`; nothing in the handler treats calling this
// with zero pending gifts of a given type as an error, so it's safe to
// call both types unconditionally on every run.
//
// Honestly left open (round 16 audit): that safety argument is a static read
// of the decompiled handler, not a live-captured confirmation for type=1
// specifically -- unlike VIP's daily claims (vip.go, errorCode 120289) and
// the alliance tech donate cooldown (errorCode 120471 below), there is no
// benignErrorCodes entry backing the type=1 (Premium) call, since no
// Premium-ineligible response has actually been captured to know what its
// errorCode (if any) looks like. If a future run ever surfaces an
// unexpected fatal error specifically on the type=1 branch, capture it and
// register the real code here rather than guessing at one now.
const (
	allianceGiftPremium int32 = 1
	allianceGiftRegular int32 = 2
)

func ClaimAllianceGifts(conn *GameConn) error {
	const cmd = "alliance.reward.allreceive"
	var errs []error
	// The 2 gift types are independent (neither scoped to the other's outcome), so an ordinary
	// decoded business-logic errorCode failure on one must not stop the other from being
	// attempted -- every error, regardless of kind, still gets appended to errs. A plain net.Error
	// is not by itself proof of anything wrong: sendAndWait's ordinary "no matching response within
	// defaultCmdTimeout" outcome is itself a net.Error with Timeout()==true, an expected result on a
	// perfectly healthy connection, so it must fall through and be treated exactly like any other
	// per-request failure. Only a genuine non-timeout net.Error (connection reset, broken pipe, DNS
	// failure, TLS error, etc.) means the underlying TCP connection itself is known-dead, so the
	// remaining type is already doomed to independently burn a full defaultCmdTimeout before failing
	// the exact same way. Mirrors CollectAll's identical errors.As-against-net.Error early-abort
	// (buildings.go) and ClaimAllMail's (mail.go).
	for _, giftType := range []int32{allianceGiftPremium, allianceGiftRegular} {
		params := NewSFSObject()
		params.PutInt("type", giftType)
		_, err := sendAndWait(conn, fmt.Sprintf("alliance gift claim response (type %d)", giftType), cmd, params)
		errs = append(errs, err)
		var netErr net.Error
		if errors.As(err, &netErr) && !netErr.Timeout() {
			break
		}
	}
	return errors.Join(errs...)
}

// DonateRecommendedAllianceTech finds whichever alliance tech is currently
// marked "Recommended" (the thumbs-up-badged entry in the Alliance Techs
// panel) and sends one free/resource-based donation toward it -- confirmed
// live via this Go client itself, not just static analysis (the capture
// only showed the request shape, not enough of the surrounding discovery
// flow to be sure without testing).
//
// Discovery: `science.data.refresh` (no params) returns the account's
// ENTIRE alliance tech tree in one call -- every scienceId with its
// `currentPro`/`needPro` (donation progress) and a `state` field. Live
// testing found exactly one entry with `state=1` out of ~45 returned;
// every other entry was `state=0`. That one entry's `currentPro`/`needPro`
// (4,951,850 / 8,000,000) matched the "4.9M/8.0M" progress bar shown for
// the thumbs-up-badged "Senior Scientist" tech in the real UI at
// essentially the same moment -- confirming `state=1` marks the
// currently-recommended tech. `al.science.recommend {scienceId, state}` is
// the real client's own way of setting this (an alliance officer action);
// this only reads the current value, never sets it.
//
// Donation: `al.science.donate {scienceId, option: 1}` -- confirmed live
// via the real client's own captured traffic tapping the coin-cost donate
// button. `option`'s exact meaning wasn't independently determined (only
// `1` was ever observed), but it's the value the real client sent for a
// successful donation, so it's reused as-is.
//
// Deliberately NOT implemented: the gem-cost "Unlimited Attempts" button
// (`al.science.donate.gold {scienceId}`, no `option` field) and the
// "hold the button longer for a multiplier" UI behavior the user
// described. Reading both donate messages' OnCreate, neither has a
// count/times/multiplier field on the wire -- holding the button is
// client-side auto-repeat, firing the same single-donation request
// multiple times while held, not a batched server call. The free/coin
// donate path this function uses is rate-limited server-side to
// `maxNum=30` per day (confirmed live via `al.science.refreshNum`), so
// only one attempt makes sense per `-collect` run.
//
// CORRECTED AGAIN, fourth audit cycle: the previous ("third audit cycle")
// version of this correction drew the wrong conclusion from a real
// observation. It ran two `-collect` runs roughly 3 minutes apart, both of
// which donated successfully (`state=1`, `donateCDTime=0` both times, only
// the response's `maxdonate.count` field decrementing 6 -> 5), and
// concluded that `refreshTimeBlock=1200000` (20 minutes) wasn't a
// per-donation cooldown at all -- only the `maxNum=30` daily count noted
// above gates repeat donations. The raw observation was fine; the
// conclusion wasn't. docs/live-validation.mdx documents an earlier
// `al.science.donate` capture, from that same live-testing session, that
// got back a real cooldown error -- `errorCode=120471, "Donate science CD
// time is not finish"` -- because the real account had donated minutes
// earlier. A per-donation cooldown does exist; the two successful test
// runs just didn't happen to land inside its window. This project still
// hasn't independently re-measured the cooldown's actual duration (whether
// `refreshTimeBlock=1200000` is really it or something else) -- only that
// it's real and has a known errorCode now. `120471` is registered in
// conn.go's benignErrorCodes, so hitting it during `-collect` is correctly
// treated as a benign no-op rather than a hard failure. The daily
// `maxNum=30` count remains a separate, independent gate from this
// per-donation cooldown; what errorCode (if any) the server returns once
// that daily count hits 0 is still unconfirmed -- deliberately not
// simulated by exhausting the real account's daily donations just to
// capture it. The gem-cost path has no such
// cooldown (`useGoldNum`/`maxGoldNum` both came back as 999999999, i.e.
// unlimited) but spends real premium currency per use, so it's
// deliberately left out rather than auto-spending gems without being
// asked.
func DonateRecommendedAllianceTech(conn *GameConn) error {
	const refreshCmd = "science.data.refresh"
	msg, err := sendAndWait(conn, "alliance tech tree fetch", refreshCmd, NewSFSObject())
	if err != nil {
		return err
	}
	v, ok := msg.Params.Get("allianceScience")
	if !ok {
		slog.Info("no alliance tech tree data returned")
		return nil
	}
	arr, ok := v.Val.(*SFSArray)
	if !ok {
		slog.Warn("alliance tech tree: allianceScience field is present but not an array, skipping donation", "type", fmt.Sprintf("%T", v.Val))
		return nil
	}
	recommendedID, found := findRecommendedTech(arr)
	if !found {
		slog.Info("no alliance tech is currently recommended")
		return nil
	}

	const donateCmd = "al.science.donate"
	slog.Info("donating to recommended alliance tech", "scienceId", recommendedID)
	params := NewSFSObject()
	params.PutInt("scienceId", recommendedID)
	params.PutInt("option", 1)
	_, err = sendAndWait(conn, fmt.Sprintf("alliance tech donate response (scienceId %d)", recommendedID), donateCmd, params)
	return err
}

// findRecommendedTech scans an allianceScience array for the state==1 entry -- pulled out of
// DonateRecommendedAllianceTech as a standalone, network-free function so it can be unit tested
// without a live connection.
func findRecommendedTech(arr *SFSArray) (scienceId int32, found bool) {
	for _, item := range arr.items {
		tech, ok := item.Val.(*SFSObject)
		if !ok {
			continue
		}
		if tech.GetInt("state") != 1 {
			continue
		}
		if !requirePresentField(tech, "scienceId", "allianceScience") {
			continue
		}
		return tech.GetInt("scienceId"), true
	}
	return 0, false
}
