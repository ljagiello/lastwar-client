package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Premium alliance gifts below gift level 15, one at a time (MASTER §3.2 #2 AGIFT; aud-alliance F3,
// cov-ui-social F2). The gift window's Premium tab is gift type 1 (TabToGiftType[2] = 1,
// UILWAllianceGiftView.lua:40-43) and offers Claim All only at alliance_gift.k5 (15) <= the gift
// level (:307-309), which collectCore's claimAllianceGifts already sends. Below that level each gift
// cell has its own get button (LWAllianceGiftItem.lua:36-39, 195-209), which goes through the
// view's 0.4 s button lock (UILWAllianceGiftView.lua:354-369) to
// `alliance.reward.receive {uuid: UtfString, type: Int}` with the tab's type
// (UILWAllianceGiftCtrl.lua:82-85; AllianceGiftGetRewardMessage.lua:5-9). Success is
// receiveResult == 1 (:16-19).
//
// The gifts come from the `alliance.reward.list {index: 0, len: 1000}` reply the window opens with
// (UILWAllianceGiftCtrl.lua:19-21): onLevel is the gift level, info.list1[] the Premium gifts
// {uuid, receiveState, expirationTime, type, ...} and info.redPoint1 their waiting count
// (AllianceGiftDataManager.lua:29-87, AllianceGiftData.lua:33-79). A gift is claimable while
// receiveState ~= 1 and now < expirationTime (LWAllianceGiftItem.lua:136-141; GetGiftInfoList drops
// the expired ones, AllianceGiftDataManager.lua:142-157) and its type, 1 when absent, is the tab's.
// The window lists at most alliance_gift.k4 gifts, latest expiry first (UpdateGiftList,
// UILWAllianceGiftView.lua:247-283), so a run claims in that order, at most premiumGiftsMaxPerRun
// (50). The window's extra alliance_gift_group type filter (UILWAllianceGiftCtrl.lua:31-33) is not
// applied: list1 is the type-1 list already.
//
// Eligibility: init shows an alliance and allianceNewMail is not 0, as for the core claim; the gift
// level is known and below 15; redPoint1 > 0 (the window's count, 0 when absent,
// AllianceGiftDataManager.lua:195-200). Free; never sends alliance.reward.remove/allremove. No
// errorCode is registered as benign: the first errorCode stops the run.
const (
	premiumGiftsListCmd    = "alliance.reward.list"
	premiumGiftsReceiveCmd = "alliance.reward.receive"
	// premiumGiftsMaxPerRun bounds one run's claims (at the pace below, under half a minute). The
	// window lists up to DataConfig alliance_gift.k4 gifts (200, table item 39879); a backlog past
	// 50 waits for the next run.
	premiumGiftsMaxPerRun = 50
)

// premiumGiftsPace is the gap between claims, above the client's 0.4 s button lock.
var premiumGiftsPace = 500 * time.Millisecond

// premiumGiftsSleep is time.Sleep; tests replace it.
var premiumGiftsSleep = time.Sleep

func init() {
	registerFeature(Feature{
		Name: "alliance-premium-gifts",
		Summary: "OPT-IN: claim Premium alliance gifts one at a time while the alliance gift level is below 15, " +
			"where the gift window has no Claim All (alliance.reward.receive, free) (static-only, not validated live)",
		Run: runAlliancePremiumGifts,
	})
}

// premiumGift is one claimable info.list1 entry.
type premiumGift struct {
	uuid       string
	expiration int64
}

func runAlliancePremiumGifts(conn *session.GameConn, in *Init) error {
	const name = "alliance-premium-gifts"
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info(name + ": not in an alliance; skipping")
		return nil
	}
	if n, ok := claimInt(in.Raw, "allianceNewMail"); ok && n == 0 {
		slog.Info(name + ": no alliance gifts waiting (init allianceNewMail=0)")
		return nil
	}
	params := sfs.NewSFSObject()
	params.PutInt("index", 0)
	params.PutInt("len", 1000)
	resp, code, err := fwRequest(conn, name+": gift list", premiumGiftsListCmd, params)
	if err != nil {
		return err
	}
	if code != "" {
		return fmt.Errorf("%s: %s: errorCode=%s", name, premiumGiftsListCmd, code)
	}
	level, levelOK := claimInt(resp, "onLevel")
	info := objectField(resp, "info")
	waiting, _ := claimInt(info, "redPoint1")
	gifts, listed := premiumGiftsDue(info, claimNow(in).UnixMilli())
	slog.Info(name+": eligibility", "giftLevel", level, "giftLevelKnown", levelOK, "claimAllFromLevel", allianceGiftPremiumMinLevel,
		"redPoint1", waiting, "listed", listed, "claimable", len(gifts))
	switch {
	case !levelOK:
		slog.Info(name + ": gift level unknown; skipping")
		return nil
	case level >= allianceGiftPremiumMinLevel:
		slog.Info(name + ": gift level allows Claim All, which the core collect sends; skipping")
		return nil
	case waiting <= 0:
		slog.Info(name + ": no Premium gifts waiting")
		return nil
	case len(gifts) == 0:
		slog.Info(name + ": no claimable Premium gift in the list")
		return nil
	}
	if len(gifts) > premiumGiftsMaxPerRun {
		slog.Info(name+": more claimable gifts than the window lists; the rest wait for the next run", "claimable", len(gifts), "cap", premiumGiftsMaxPerRun)
		gifts = gifts[:premiumGiftsMaxPerRun]
	}

	claimed := 0
	for i, g := range gifts {
		if i > 0 {
			premiumGiftsSleep(premiumGiftsPace)
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("uuid", g.uuid)
		p.PutInt("type", allianceGiftPremium)
		resp, code, err := fwRequest(conn, name+": gift claim", premiumGiftsReceiveCmd, p)
		if err != nil {
			return errors.Join(err, fmt.Errorf("%s: stopped after %d of %d claims", name, claimed, len(gifts)))
		}
		result, _ := claimInt(resp, "receiveResult")
		if i == 0 {
			typ, _ := claimInt(resp, "type")
			redPoint, hasRedPoint := claimInt(resp, "redPoint")
			newMail, hasNewMail := claimInt(resp, "allianceNewMail")
			slog.Info(name+": first "+premiumGiftsReceiveCmd+" reply", "errorCode", code, "receiveResult", result,
				"type", typ, "uuid", idString(resp, "uuid"), "sentUuid", g.uuid, "receiveTime", idString(resp, "receiveTime"),
				"redPoint", redPoint, "hasRedPoint", hasRedPoint, "allianceNewMail", newMail, "hasAllianceNewMail", hasNewMail,
				"hasNext", resp.Has("next"), "rewardEntries", len(claimObjects(resp, "info")), "replyKeys", resp.Keys())
		}
		if code != "" {
			slog.Error(name+": gift claim refused; stopping", "uuid", g.uuid, "errorCode", code, "claimed", claimed)
			return fmt.Errorf("%s: %s uuid %s: errorCode=%s", name, premiumGiftsReceiveCmd, g.uuid, code)
		}
		// The client acts only on receiveResult 1 (AllianceGiftGetRewardMessage.lua:16-19). Any other
		// or a missing one is a reply this static-only claim doesn't know: stop, so a refusal without
		// an errorCode can't turn into a claim per gift on every run.
		if hasResult := resp.Has("receiveResult"); !hasResult || result != 1 {
			slog.Warn(name+": gift not claimed; stopping", "uuid", g.uuid, "receiveResult", result,
				"hasReceiveResult", hasResult, "claimed", claimed)
			return fmt.Errorf("%s: %s uuid %s: receiveResult %d (present %v), want 1", name, premiumGiftsReceiveCmd,
				g.uuid, result, hasResult)
		}
		claimed++
		slog.Info(name+": claimed a Premium gift", "uuid", g.uuid, "rewardEntries", len(claimObjects(resp, "info")))
	}
	slog.Info(name+": claims done", "claimed", claimed, "sent", len(gifts))
	return nil
}

// premiumGiftsDue returns the claimable info.list1 gifts at nowMs, latest expiry first, and how many
// entries the list had.
func premiumGiftsDue(info *sfs.SFSObject, nowMs int64) ([]premiumGift, int) {
	entries := objectsField(info, "list1")
	var out []premiumGift
	for _, g := range entries {
		uuid := idString(g, "uuid")
		if uuid == "" {
			continue
		}
		if state, _ := claimInt(g, "receiveState"); state == 1 {
			continue
		}
		if typ, ok := claimInt(g, "type"); ok && typ != int64(allianceGiftPremium) {
			continue
		}
		exp, _ := claimInt(g, "expirationTime")
		if nowMs >= exp {
			continue
		}
		out = append(out, premiumGift{uuid: uuid, expiration: exp})
	}
	slices.SortStableFunc(out, func(a, b premiumGift) int { return cmp.Compare(b.expiration, a.expiration) })
	return out, len(entries)
}
