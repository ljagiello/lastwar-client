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
		Run:       func(conn *session.GameConn, _ *Init) error { return donateAllAllianceTech(conn) },
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
// Opt-in because every donation spends resources. The core run still sends its one donation
// first (collectCore cannot see the feature config), so with this enabled a -collect run donates
// once there and spends the rest of the charges here; -run alliance-donate-all spends them all.
func donateAllAllianceTech(conn *session.GameConn) error {
	msg, err := session.SendAndWait(conn, "alliance tech tree fetch", "science.data.refresh", sfs.NewSFSObject())
	if err != nil {
		return err
	}
	arr, ok := objectArray(msg.Params, "allianceScience")
	if !ok {
		slog.Info("no alliance tech tree data returned")
		return nil
	}
	scienceID, found := findRecommendedTech(arr)
	if !found {
		slog.Info("no alliance tech is currently recommended; nothing to donate to")
		return nil
	}

	params := sfs.NewSFSObject()
	params.PutInt("scienceId", scienceID)
	msg, err = session.SendAndWait(conn, "alliance tech donation charges", "al.science.refreshNum", params)
	if err != nil {
		return err
	}
	charges, ok := donationCharges(msg.Params)
	if !ok {
		slog.Warn("al.science.refreshNum reply carries no useNum; not donating", "scienceId", scienceID)
		return nil
	}
	slog.Info("alliance tech donation charges", "scienceId", scienceID, "useNum", charges, "maxNum", msg.Params.GetInt("maxNum"))

	donated := 0
	for charges > 0 && donated < allianceDonateMaxPerRun {
		params := sfs.NewSFSObject()
		params.PutInt("scienceId", scienceID)
		params.PutInt("option", 1)
		msg, err := session.SendAndWait(conn, fmt.Sprintf("alliance tech donate response (scienceId %d)", scienceID), "al.science.donate", params)
		if err != nil {
			slog.Warn("alliance tech donation failed; stopping", "scienceId", scienceID, "donated", donated)
			return err
		}
		if msg.Params.Has("errorCode") {
			slog.Info("alliance tech donation refused; stopping", "scienceId", scienceID, "donated", donated, "errorCode", idString(msg.Params, "errorCode"))
			return nil
		}
		donated++
		if charges, ok = donationCharges(msg.Params); !ok {
			slog.Warn("donate reply carries no useNum; stopping", "scienceId", scienceID, "donated", donated)
			return nil
		}
	}
	slog.Info("alliance tech donations done", "scienceId", scienceID, "donated", donated, "useNumLeft", charges)
	return nil
}

// donationCharges reads useNum, the donation charges remaining, from a refreshNum or donate reply.
func donationCharges(resp *sfs.SFSObject) (int32, bool) {
	if !session.RequireFieldType(resp, "useNum", "alliance tech donation charges", session.SFSFieldKindInt) {
		return 0, false
	}
	return resp.GetInt("useNum"), true
}

// objectArray returns the array under key, or ok=false (with a warning when present but of the
// wrong type, as DonateRecommendedAllianceTech does).
func objectArray(o *sfs.SFSObject, key string) (*sfs.SFSArray, bool) {
	v, ok := o.Get(key)
	if !ok {
		return nil, false
	}
	arr, ok := v.Val.(*sfs.SFSArray)
	if !ok {
		slog.Warn("alliance tech tree: field is present but not an array", "field", key, "type", fmt.Sprintf("%T", v.Val))
	}
	return arr, ok
}
