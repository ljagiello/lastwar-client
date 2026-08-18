package main

import (
	"fmt"
	"log/slog"
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
func HelpAllianceMembers(conn *GameConn) {
	const cmd = "al.help.all"
	params := NewSFSObject()
	params.PutLong("cmdBaseTime", time.Now().UnixMilli())
	if err := conn.SendExtension(cmd, params); err != nil {
		slog.Error("alliance help-all send failed", "error", err)
		return
	}
	msg, err := waitForCmd(conn, 8*time.Second, cmd)
	if err != nil {
		slog.Error("alliance help-all no response", "error", err)
		return
	}
	logCommandResult("alliance help-all response", msg)
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
func ClaimAllianceGifts(conn *GameConn) {
	const cmd = "alliance.reward.allreceive"
	for _, giftType := range []int32{1, 2} {
		params := NewSFSObject()
		params.PutInt("type", giftType)
		if err := conn.SendExtension(cmd, params); err != nil {
			slog.Error("alliance gift claim send failed", "type", giftType, "error", err)
			continue
		}
		msg, err := waitForCmd(conn, 8*time.Second, cmd)
		if err != nil {
			slog.Error("alliance gift claim no response", "type", giftType, "error", err)
			continue
		}
		logCommandResult(fmt.Sprintf("alliance gift claim response (type %d)", giftType), msg)
	}
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
// donate path this function uses is rate-limited server-side (confirmed
// live via `al.science.refreshNum`: `maxNum=30` per day, gated by a
// `refreshTimeBlock=1200000` -- 20 minutes -- cooldown between individual
// donations), so only one attempt makes sense per `-collect` run; it will
// legitimately fail with a cooldown error if run again too soon, which is
// expected and not treated as fatal here. The gem-cost path has no such
// cooldown (`useGoldNum`/`maxGoldNum` both came back as 999999999, i.e.
// unlimited) but spends real premium currency per use, so it's
// deliberately left out rather than auto-spending gems without being
// asked.
func DonateRecommendedAllianceTech(conn *GameConn) {
	const refreshCmd = "science.data.refresh"
	if err := conn.SendExtension(refreshCmd, NewSFSObject()); err != nil {
		slog.Error("alliance tech tree fetch send failed", "error", err)
		return
	}
	msg, err := waitForCmd(conn, 8*time.Second, refreshCmd)
	if err != nil {
		slog.Error("alliance tech tree fetch no response", "error", err)
		return
	}
	v, ok := msg.Params.Get("allianceScience")
	if !ok {
		slog.Info("no alliance tech tree data returned")
		return
	}
	arr, ok := v.Val.(*SFSArray)
	if !ok {
		return
	}
	var recommendedID int32
	found := false
	for _, item := range arr.items {
		tech, ok := item.Val.(*SFSObject)
		if !ok {
			continue
		}
		if tech.GetInt("state") == 1 {
			recommendedID = tech.GetInt("scienceId")
			found = true
			break
		}
	}
	if !found {
		slog.Info("no alliance tech is currently recommended")
		return
	}

	const donateCmd = "al.science.donate"
	slog.Info("donating to recommended alliance tech", "scienceId", recommendedID)
	params := NewSFSObject()
	params.PutInt("scienceId", recommendedID)
	params.PutInt("option", 1)
	if err := conn.SendExtension(donateCmd, params); err != nil {
		slog.Error("alliance tech donate send failed", "error", err)
		return
	}
	donateMsg, err := waitForCmd(conn, 8*time.Second, donateCmd)
	if err != nil {
		slog.Error("alliance tech donate no response", "error", err)
		return
	}
	logCommandResult(fmt.Sprintf("alliance tech donate response (scienceId %d)", recommendedID), donateMsg)
}
