package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func init() {
	registerFeature(Feature{
		Name:      "alliance-donate-all",
		Summary:   "OPT-IN: spend every remaining alliance tech donation charge on the recommended tech (each donation costs resources)",
		DefaultOn: false,
		Run:       donateAllAllianceTech,
	})
}

// allianceDonateMaxPerRun bounds the donation loop: maxNum, the charge pool's size, was 30 live
// (al.science.refreshNum), so a full pool never needs more.
const allianceDonateMaxPerRun = 30

// donateAllAllianceTech is the alliance-donate-all feature: what the real client does when the
// player holds the donate button (AlScienceDonateInfo.lua CheckPressInterval), spending every
// remaining donation charge on the recommended tech rather than one per run (see
// DonateRecommendedAllianceTech for the charge semantics). It finds the recommended tech with
// science.data.refresh, reads the charges left (`useNum`) with `al.science.refreshNum {scienceId}`
// (the client sends it after every tree refresh, AllianceScienceDataManager.lua:105-121),
// then sends `al.science.donate {scienceId, option: 1}` while useNum > 0, re-reading useNum from
// each donate reply, at most allianceDonateMaxPerRun times. It stops at the first errorCode
// (120471 = charges used up, benign; anything else, for example too few resources, is returned)
// and when a reply carries no useNum.
//
// The init and tech gates of donateRecommendedAllianceTech apply before the loop, and the tech
// gates again before every further donation: the long press stops at currentPro >= needPro
// (AlScienceDonateInfo.lua:397). The live donate reply carries no progress fields
// (docs/live-validation.mdx:422), so the tech record changes only when a reply or a
// push.al.science(.update) for this tech carries them; otherwise the loop runs on useNum alone.
// The resource ledger starts from init resource{} and goes down by lowNum per donation, or takes
// the reply's resource{} when it has one (AlScienceDonateMessage.lua:19-21).
//
// Opt-in because every donation spends resources. The core run still sends its one donation
// first (collectCore cannot see the feature config), so with this enabled a -collect run donates
// once there and spends the rest of the charges here; -run alliance-donate-all spends them all.
func donateAllAllianceTech(conn *session.GameConn, in *Init) error {
	if !allianceScienceAllowed(in) {
		return nil
	}
	tech, found, err := fetchRecommendedTech(conn)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	ledger := newAllianceLedger(in)
	if why := tech.blocked(claimNow(in).UnixMilli(), ledger); why != "" {
		slog.Info("alliance-donate-all: not donating to the recommended tech", append([]any{"reason", why}, tech.logAttrs()...)...)
		return nil
	}

	params := sfs.NewSFSObject()
	params.PutInt("scienceId", tech.id)
	msg, err := session.SendAndWait(conn, "alliance tech donation charges", "al.science.refreshNum", params)
	if err != nil {
		return err
	}
	charges, ok := donationCharges(msg.Params)
	if !ok {
		slog.Warn("al.science.refreshNum reply carries no useNum; not donating", "scienceId", tech.id)
		return nil
	}
	slog.Info("alliance tech donation charges", "scienceId", tech.id, "useNum", charges, "maxNum", msg.Params.GetInt("maxNum"))

	donated := 0
	for charges > 0 && donated < allianceDonateMaxPerRun {
		if why := tech.blocked(claimNow(in).UnixMilli(), ledger); donated > 0 && why != "" {
			slog.Info("alliance tech donations stop", append([]any{"reason", why, "donated", donated}, tech.logAttrs()...)...)
			return nil
		}
		costRes, cost, costKnown := tech.lowRes, tech.lowNum, tech.hasLowRes && tech.hasLowNum
		msg, err := sendAllianceDonation(conn, tech)
		if err != nil {
			slog.Warn("alliance tech donation failed; stopping", "scienceId", tech.id, "donated", donated)
			return err
		}
		if msg.Params.Has("errorCode") {
			code := idString(msg.Params, "errorCode")
			if code == allianceChargesUsedUpCode {
				slog.Info("alliance tech donation refused; stopping", "scienceId", tech.id, "donated", donated, "errorCode", code)
				return nil
			}
			slog.Error("alliance tech donation failed; stopping", "scienceId", tech.id, "donated", donated, "errorCode", code)
			return fmt.Errorf("alliance tech donate response (scienceId %d): errorCode=%s", tech.id, code)
		}
		donated++
		tech.apply(msg.Params)
		if res := objectField(msg.Params, "resource"); res != nil {
			ledger.set(res)
		} else if have, ok := ledger.balance(costRes); ok && costKnown {
			ledger[costRes] = have - cost
		}
		if charges, ok = donationCharges(msg.Params); !ok {
			slog.Warn("donate reply carries no useNum; stopping", "scienceId", tech.id, "donated", donated)
			return nil
		}
	}
	slog.Info("alliance tech donations done", "scienceId", tech.id, "donated", donated, "useNumLeft", charges)
	return nil
}

// apply is update for a message about any tech: only one whose scienceId is t's changes t
// (UpdateOneAllianceScience updates the record named by the message's scienceId).
func (t *allianceTech) apply(o *sfs.SFSObject) {
	if id, ok := claimInt(o, "scienceId"); ok && id == int64(t.id) {
		t.update(o)
	}
}

// sendAllianceDonation sends one `al.science.donate {scienceId, option: 1}` and waits for its
// reply, applying every push.al.science / push.al.science.update for t read meanwhile
// (PushAlScienceUpdateMessage.lua:8-13, PushAlScienceUpdateMessage1.lua:8-13). The error is only a
// send or wait failure; an errorCode reply is returned as is, for the caller to judge.
func sendAllianceDonation(conn *session.GameConn, t *allianceTech) (*session.ExtensionMessage, error) {
	label := fmt.Sprintf("alliance tech donate response (scienceId %d)", t.id)
	params := sfs.NewSFSObject()
	params.PutInt("scienceId", t.id)
	params.PutInt("option", 1)
	if err := conn.SendExtension(allianceDonateCmd, params); err != nil {
		wrapped := session.SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", allianceDonateCmd, "error", wrapped)
		return nil, wrapped
	}
	var reply *session.ExtensionMessage
	_, err := session.WaitFor(conn, session.DefaultCmdTimeout, func(e *session.Envelope) bool {
		msg, ok := e.AsExtension()
		if !ok {
			return false
		}
		switch msg.Cmd {
		case allianceDonateCmd:
			reply = msg
			return true
		case allianceSciencePushCmd, allianceSciencePushUpdateCmd:
			t.apply(msg.Params)
			slog.Info("alliance tech update pushed", append([]any{"push", msg.Cmd}, t.logAttrs()...)...)
		}
		return false
	})
	if err != nil {
		slog.Error(label+" no response", "cmd", allianceDonateCmd, "error", err)
		return nil, err
	}
	slog.Info(label, "cmd", allianceDonateCmd, "errorCode", idString(reply.Params, "errorCode"),
		"useNum", reply.Params.GetInt("useNum"), "maxNum", reply.Params.GetInt("maxNum"), "replyKeys", reply.Params.Keys())
	return reply, nil
}

// donationCharges reads useNum, the donation charges remaining, from a refreshNum or donate reply.
func donationCharges(resp *sfs.SFSObject) (int32, bool) {
	if !session.RequireFieldType(resp, "useNum", "alliance tech donation charges", session.SFSFieldKindInt) {
		return 0, false
	}
	return resp.GetInt("useNum"), true
}
