package game

import (
	"errors"
	"fmt"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
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
// `time.Now().UnixMilli()`. The real client uses its server-synced clock
// (UITimeManager:GetServerTime, UILWAlHelpCtrl.lua:25-26); this client keeps
// no server clock, so the host clock stands in for it.
//
// The captured response fell outside the successfully-decoded portion of
// that capture (the same stream-reassembly artifact seen with the Truck
// Rewards and visitor.operate captures), so the live response shape has
// not been directly observed. Reading the Lua handler itself: success is
// "no errorCode", carrying an optional `accPoint` (accumulated
// alliance-help point total). This sends it unconditionally; collectCore
// goes through helpAllianceMembers, which first checks there is someone
// to help.
func HelpAllianceMembers(conn *session.GameConn) error {
	const cmd = "al.help.all"
	params := sfs.NewSFSObject()
	params.PutLong("cmdBaseTime", time.Now().UnixMilli())
	_, err := session.SendAndWait(conn, "alliance help-all response", cmd, params)
	return err
}

// helpAllianceMembers sends al.help.all only when another member has an open help request, as the
// real help window does (UILWAlHelpCtrl.lua:15-29: it sends only when some listed request has
// isSelf == false, else shows tip 393023). It lists the requests first with `al.show.help {}`,
// whose reply carries `helpArr` (AllianceHelpDataManager.lua:47-78; see openHelpRequests). Without
// init (in.Raw nil) it sends al.help.all unconditionally, as before this gating existed. The
// HUD/chat help bubbles in the real client also send al.help.all unconditionally, but only while
// they are showing, which is the same condition.
func helpAllianceMembers(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return HelpAllianceMembers(conn)
	}
	msg, err := session.SendAndWait(conn, "alliance help list response", "al.show.help", sfs.NewSFSObject())
	if err != nil {
		return err
	}
	listed, open := openHelpRequests(msg.Params, ownUID(in))
	if open == 0 {
		slog.Info("no other member's help request is open; skipping al.help.all", "listed", listed)
		return nil
	}
	slog.Info("helping alliance members", "listed", listed, "open", open)
	return HelpAllianceMembers(conn)
}

// openHelpRequests counts al.show.help's helpArr entries and, of those, the open requests of other
// members. The wire field names are AllianceHelpInfo.lua's ParseData: helpId, senderId, stats,
// and the lowercase nowcount/maxcount. An entry is another member's when its senderId differs from
// ownUID: CheckStats (AllianceHelpInfo.lua:108-114) recomputes `stats` that way and ignores the
// server's value, so the server's stats (0 = own) is only the fallback when ownUID or senderId is
// unknown. It is open while nowcount < maxcount; the Lua defaults a missing maxcount to 0
// (finished), but an entry without maxcount counts as open here, so a renamed field cannot
// silently stop the helping.
func openHelpRequests(resp *sfs.SFSObject, ownUID string) (listed, open int) {
	for _, h := range objectsField(resp, "helpArr") {
		if idString(h, "helpId") == "" {
			continue
		}
		listed++
		if sender := idString(h, "senderId"); ownUID != "" && sender != "" {
			if sender == ownUID {
				continue
			}
		} else if stats, ok := numberField(h, "stats"); ok && stats == 0 {
			continue
		}
		if maxCount, ok := numberField(h, "maxcount"); ok {
			if nowCount, _ := numberField(h, "nowcount"); nowCount >= maxCount {
				continue
			}
		}
		open++
	}
	return listed, open
}

// ownUID is the player's uid from init.user.uid, as a string ("" when absent).
func ownUID(in *Init) string {
	return idString(in.Object("user"), "uid")
}

// allianceGiftPremiumMinLevel is DataConfig alliance_gift.k5: the alliance gift level from which
// the gift window offers Premium "Claim All" (UILWAllianceGiftView.lua:296-297; table item row
// alliance_gift, k5 = 15 in 39432 and the live 39516). init.dataConfig cannot change it in
// practice: DataConfig:InitFromNet re-applies the local table over the server's copy
// (DataConfig.lua:47-68).
const allianceGiftPremiumMinLevel = 15

// claimAllianceGifts claims the alliance gifts the way the real gift window offers them:
//
//   - nothing at all when init's allianceNewMail (the unclaimed gift count, InitMessage.lua:153) is 0;
//   - otherwise it lists the gifts with `alliance.reward.list {index: 0, len: 1000}`
//     (UILWAllianceGiftCtrl.lua:17), whose reply carries the gift level `onLevel` and the
//     per-type waiting counts `info.redPoint1`/`info.redPoint2` (AllianceGiftDataManager.lua);
//   - Premium (type 1) only at gift level >= allianceGiftPremiumMinLevel with redPoint1 > 0;
//   - Regular (type 2) when redPoint2 > 0.
//
// A missing redPoint counts as waiting. If the list fails, only Regular is claimed, since Premium
// needs the level, which init does not carry. Without init (in.Raw nil) both types are claimed
// blind, as before this gating existed. This never sends alliance.reward.remove/allremove. Below
// level 15 the real client claims Premium gifts one at a time (alliance.reward.receive); that is
// not done here. Static: the server's reply to a Premium claim-all below level 15 has not been
// observed.
func claimAllianceGifts(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return ClaimAllianceGifts(conn)
	}
	if n, ok := numberField(in.Raw, "allianceNewMail"); ok && n == 0 {
		slog.Info("no alliance gifts waiting (init allianceNewMail=0); skipping gift claims")
		return nil
	}
	params := sfs.NewSFSObject()
	params.PutInt("index", 0)
	params.PutInt("len", 1000)
	msg, err := session.SendAndWait(conn, "alliance gift list response", "alliance.reward.list", params)
	if err != nil {
		if session.ContainsNonTimeoutNetError(err) {
			return err
		}
		slog.Warn("alliance gift list failed; claiming Regular gifts only", "error", err)
		return errors.Join(err, claimAllianceGiftType(conn, allianceGiftRegular))
	}
	level, levelOK := numberField(msg.Params, "onLevel")
	info := objectField(msg.Params, "info")
	premium := giftsWaiting(info, "redPoint1")
	regular := giftsWaiting(info, "redPoint2")
	claimPremium := premium && levelOK && level >= allianceGiftPremiumMinLevel
	slog.Info("alliance gift eligibility", "giftLevel", level, "giftLevelKnown", levelOK, "premiumWaiting", premium,
		"regularWaiting", regular, "claimPremium", claimPremium, "claimRegular", regular)

	var errs []error
	if claimPremium {
		err := claimAllianceGiftType(conn, allianceGiftPremium)
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			return errors.Join(errs...)
		}
	}
	if regular {
		errs = append(errs, claimAllianceGiftType(conn, allianceGiftRegular))
	}
	return errors.Join(errs...)
}

// giftsWaiting reports whether the gift list's redPoint<type> count is above 0, or absent.
func giftsWaiting(info *sfs.SFSObject, key string) bool {
	n, ok := numberField(info, key)
	return !ok || n > 0
}

// claimAllianceGiftType sends one `alliance.reward.allreceive {type}`.
func claimAllianceGiftType(conn *session.GameConn, giftType int32) error {
	params := sfs.NewSFSObject()
	params.PutInt("type", giftType)
	_, err := session.SendAndWait(conn, fmt.Sprintf("alliance gift claim response (type %d)", giftType), "alliance.reward.allreceive", params)
	return err
}

// allianceGiftPremium and allianceGiftRegular are the two independently-claimed Alliance Gifts
// panel tabs' `type` values -- see ClaimAllianceGifts' doc comment immediately below for how each
// was confirmed (only allianceGiftRegular via a direct live packet capture; allianceGiftPremium on
// the strength of the decompiled handler's tip-string branch alone).
const (
	allianceGiftPremium int32 = 1
	allianceGiftRegular int32 = 2
)

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
// with zero pending gifts of a given type as an error (live, with nothing
// pending, both types answered receiveResult=0).
//
// This claims both types unconditionally, which is CollectAll's behaviour
// without init. collectCore goes through claimAllianceGifts, which applies
// the real client's gates, including the gift-level gate on Premium. Left
// open (round 16 audit): no Premium-ineligible response has been captured,
// so there is no benignErrorCodes entry for type=1; if one surfaces, capture
// it and register the real code rather than guessing at one now.
func ClaimAllianceGifts(conn *session.GameConn) error {
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
		err := claimAllianceGiftType(conn, giftType)
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
// Charges (corrected for 1.0.364): `al.science.refreshNum {scienceId}` and
// every donate reply carry `maxNum` (30, the size of a regenerating charge
// pool, not a daily count), `useNum` -- the charges REMAINING despite its
// name (AllianceScienceDataManager.lua:87-89, GetResDonateRestCount returns
// it) -- and `timePoint`/`refreshTimeBlock` (1,200,000 ms): one charge comes
// back per block. The real client refuses to send a donation once useNum
// reaches 0 and shows locale 120471 itself, "Donation Count has reached its
// limit, please wait" (AlScienceDonateInfo.lua:415-419,
// UIAllianceScienceInfoCtrl.lua:141-146); while charges remain it donates
// back to back (holding the button repeats the same single-donation
// request). So errorCode 120471, whose server text reads "Donate science CD
// time is not finish", means the charges are used up, not a per-donation
// cooldown. That fits the live observations: two runs 3 minutes apart both
// donated (with a `maxdonate.count` field stepping 6 -> 5), and the earlier
// 120471 (docs/live-validation.mdx) came minutes after the real client had
// donated, consistent with its charges being spent. 120471 is registered in
// conn.go's benignErrorCodes, so it is a benign no-op here.
//
// This sends one donation per run, the default: each donation spends the
// player's resources. The opt-in feature alliance-donate-all
// (feature_alliance_donate_all.go) spends every remaining charge instead.
// Deliberately NOT implemented: the gem-cost "Unlimited Attempts" button
// (`al.science.donate.gold {scienceId}`, no `option` field;
// `useGoldNum`/`maxGoldNum` came back as 999999999), since it spends
// premium currency per use. When no tech is recommended the real UI
// suggests the highest-progress open one (FindCanRecommendScience); this
// donates only to an explicitly recommended tech.
func DonateRecommendedAllianceTech(conn *session.GameConn) error {
	const refreshCmd = "science.data.refresh"
	msg, err := session.SendAndWait(conn, "alliance tech tree fetch", refreshCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	v, ok := msg.Params.Get("allianceScience")
	if !ok {
		slog.Info("no alliance tech tree data returned")
		return nil
	}
	arr, ok := v.Val.(*sfs.SFSArray)
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
	params := sfs.NewSFSObject()
	params.PutInt("scienceId", recommendedID)
	params.PutInt("option", 1)
	_, err = session.SendAndWait(conn, fmt.Sprintf("alliance tech donate response (scienceId %d)", recommendedID), donateCmd, params)
	return err
}

// allianceScienceRawItemCap bounds how many RAW entries in an allianceScience array
// findRecommendedTech will examine. Live testing found ~45 entries in a real account's full
// alliance tech tree (see DonateRecommendedAllianceTech's doc comment); this function returns
// immediately on the first state==1 match, so there's no unbounded OUTPUT growth to worry about,
// only unbounded scan/log cost -- a hostile peer responding to science.data.refresh with an array
// where many/all entries have state==1 but a missing scienceId would otherwise cause
// requireFieldType's Warn to fire on every single one, with no bound, since the raw
// allianceScience array is bounded only by sfsobject.go's much larger sfs.MaxDecodedNodes=300,000
// decode budget. Same gap-class as visitors.go's ParseInitVisitors (round 26) and ListMail's
// mailListRawItemCap (mail.go), applied here. Set generously above the live-confirmed ~45 real
// entries -- a legitimately larger tech tree from a future game update shouldn't be needlessly
// clamped -- but still finite and well below the decode-level ceiling.
const allianceScienceRawItemCap = 1000

// findRecommendedTech scans an allianceScience array for the state==1 entry -- pulled out of
// DonateRecommendedAllianceTech as a standalone, network-free function so it can be unit tested
// without a live connection.
//
// The scan is capped at allianceScienceRawItemCap RAW items examined (round 27), not just at
// however many turn out valid -- see that constant's doc comment. A malformed or non-recommended
// entry hits a `continue` that doesn't advance any output-based counter, so without this cap a
// hostile/misbehaving peer could force an unbounded scan/log cost regardless of how quickly a real
// recommended entry would otherwise be found.
//
// scienceId is guarded via requireFieldType, not just requirePresentField (round 28): a state==1
// entry whose scienceId is present but the WRONG concrete SFS type (e.g. sent as a string) used to
// pass a presence-only guard and then silently coerce to scienceId=0 via GetInt's own zero-value
// fallback -- indistinguishable from a genuine scienceId=0, and enough to make
// DonateRecommendedAllianceTech send a real al.science.donate request against the wrong tech. See
// TestFindRecommendedTechWrongTypedScienceIdIsRejected (alliance_test.go).
func findRecommendedTech(arr *sfs.SFSArray) (scienceId int32, found bool) {
	if len(arr.Items()) > allianceScienceRawItemCap {
		slog.Warn("alliance tech tree: allianceScience array longer than raw-item scan cap; truncating scan", "arrayLen", len(arr.Items()), "cap", allianceScienceRawItemCap)
	}
	for i, item := range arr.Items() {
		if i >= allianceScienceRawItemCap {
			break
		}
		tech, ok := item.Val.(*sfs.SFSObject)
		if !ok {
			continue
		}
		// state is guarded via requireFieldType purely for consistency/diagnosability with the
		// scienceId guard immediately below (round 29 audit): a wrong-typed state used to coerce
		// silently to state=0 via GetInt's own zero-value fallback, which simply fails the `!= 1`
		// comparison below and is treated the same as a genuine non-recommended entry -- fail-safe
		// (never a false match), but with zero diagnostic signal that the entry was malformed
		// rather than legitimately not recommended. See TestFindRecommendedTechWrongTypedStateIsRejected.
		if !session.RequireFieldType(tech, "state", "allianceScience", session.SFSFieldKindInt) {
			continue
		}
		if tech.GetInt("state") != 1 {
			continue
		}
		if !session.RequireFieldType(tech, "scienceId", "allianceScience", session.SFSFieldKindInt) {
			continue
		}
		return tech.GetInt("scienceId"), true
	}
	return 0, false
}
