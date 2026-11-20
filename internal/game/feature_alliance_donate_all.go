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
// (the client sends it after every tree refresh, AllianceScienceDataManager.lua:118-134),
// then sends `al.science.donate {scienceId, option: 1}` while useNum > 0, re-reading useNum from
// each donate reply, at most allianceDonateMaxPerRun times a run. It stops at the first errorCode
// (allianceDonateStops: 120471 = charges used up, 120836 = the day reset, both without an error;
// anything else, for example too few resources, is returned) and when a reply carries no useNum.
//
// The init and tech gates of donateRecommendedAllianceTech apply before the loop, and the tech
// gates again before every further donation: the long press stops at currentPro >= needPro
// (AlScienceDonateInfo.lua:397). The live donate reply carries no progress fields
// (docs/live-validation.mdx:422), so the tech record changes only when a reply or a
// push.al.science(.update) for this tech carries them; otherwise the loop runs on useNum alone.
// The resource ledger starts from init resource{} and goes down by lowNum per donation, or takes
// the reply's resource{} when it has one (AlScienceDonateMessage.lua:19-21).
//
// Opt-in because every donation spends resources. collectCore cannot see the feature config, so
// the core run still sends its one donation first. Since the 1.0.370 audit (MASTER B32) both share
// this run's allianceScienceRun: in a -collect run this continues from the core's tree, charge read
// and donation, so science.data.refresh and al.science.refreshNum go out once per run; -run
// alliance-donate-all, with no core run before it, fetches both itself. A day-reset 120836 or a
// 120471 seen by either ends the run's donations without an error. When the core's charge read or
// donation timed out, this reads the charges again before donating (allianceScienceRun.failed).
func donateAllAllianceTech(conn *session.GameConn, in *Init) error {
	if !allianceScienceAllowed(in) {
		return nil
	}
	run := cachedAllianceScience(conn, in)
	if run != nil {
		slog.Info("alliance-donate-all: continuing from this run's alliance tech read", "donatedThisRun", run.donated,
			"useNum", run.charges, "useNumKnown", run.chargesKnown, "stopped", run.stop)
	} else {
		var err error
		if run, err = allianceScienceFor(conn, in); err != nil {
			return err
		}
	}
	before := run.donated
	err := run.donate(conn, "alliance-donate-all", max(allianceDonateMaxPerRun-run.donated, 0))
	if run.tech != nil {
		slog.Info("alliance tech donations done", "scienceId", run.tech.id, "donated", run.donated-before,
			"donatedThisRun", run.donated, "useNumLeft", run.charges, "stopped", run.stop)
	}
	return err
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
	id, err := conn.SendExtensionID(allianceDonateCmd, params)
	if err != nil {
		wrapped := session.SendStageError{Err: err}
		slog.Error(label+" send failed", "cmd", allianceDonateCmd, "error", wrapped)
		return nil, wrapped
	}
	var reply *session.ExtensionMessage
	_, err = session.WaitFor(conn, session.DefaultCmdTimeout, func(e *session.Envelope) bool {
		msg, ok := e.AsExtension()
		if !ok {
			return false
		}
		switch msg.Cmd {
		case allianceDonateCmd:
			// A late reply to an earlier donation whose wait timed out is not this one's (the
			// client pairs replies by _id, Assembly-CSharp.decompiled.cs:82052-82056).
			if session.StaleReply(msg, id) {
				return false
			}
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
	slog.Info(label, append([]any{"cmd", allianceDonateCmd, "errorCode", idString(reply.Params, "errorCode"),
		"errorMsg", idString(reply.Params, "errorMsg")}, append(allianceChargeAttrs(reply.Params), "replyKeys", reply.Params.Keys())...)...)
	return reply, nil
}

// donationCharges reads useNum, the donation charges remaining, from a refreshNum or donate reply.
func donationCharges(resp *sfs.SFSObject) (int32, bool) {
	if !session.RequireFieldType(resp, "useNum", "alliance tech donation charges", session.SFSFieldKindInt) {
		return 0, false
	}
	return resp.GetInt("useNum"), true
}
