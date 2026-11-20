package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Battle Plan captive visitors (MASTER G34, aud-city/F6, cov-scene-chat/F4), OPT-IN. Season 7's
// Battle Plan sends captives to the city as visitors of behaviour type 40, BattlePlanCaptive
// (Global/EnumType.lua:12061; lw_base_visitor_event row 22001 in tables 40413), queue type 9 in
// Queue1 (Scene/CityVisitor/Const.lua:19, 42). The client gives them VisitorBattlePlanCaptive
// (CityVisitorManager.lua:117-118), whose tap goes straight to VisitorGift's accept
// (VisitorBattlePlanCaptive.lua:53-55): one `visitor.operate {uid: Long, operate: Int 1}`
// (VisitorGift.lua:147-171; the batch branch is for GIFT only), with no cost check. The captive
// then walks to the Drill Ground (:57-87): the accept is free and inward.
//
// The reply (CityVisitorManager.lua:435-445, FinishVisitor) carries visitorType 9, captiveVisitors
// for the result window (UIBattlePlanCaptiveGetView sends nothing) and removedVisitorUids, every
// visitor the accept cleared (the client falls back to the sent uid without it). Those uids are
// marked greeted, so they are not sent again and visitor-fresh, which runs after this feature,
// counts the queue right. The reply shape and any troop-stock interaction are unobserved:
// static-only, not validated live. The core greet (GreetVisitors) leaves type 40 alone.
const captiveVisitorsFeature = "captive-visitors"

func init() {
	registerFeature(Feature{
		Name:      captiveVisitorsFeature,
		Summary:   "OPT-IN: accept Season 7 Battle Plan captive visitors (type 40, visitor.operate) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runCaptiveVisitors,
	})
}

func runCaptiveVisitors(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(captiveVisitorsFeature + ": no init push; skipping")
		return nil
	}
	return greetOptInVisitors(conn, in, captiveVisitorsFeature, visitorTypeBattlePlanCaptive, connNow(conn), captiveVisitorsCleared)
}

// captiveVisitorsCleared returns the reply's removedVisitorUids, read as any list of integers.
func captiveVisitorsCleared(reply *sfs.SFSObject) []int64 {
	uids := claimInts(reply, "removedVisitorUids")
	visitorType, _ := claimInt(reply, "visitorType")
	slog.Info(captiveVisitorsFeature+": accept reply", "removedVisitorUids", uids,
		"captiveVisitors", len(claimObjects(reply, "captiveVisitors")), "visitorType", visitorType)
	return uids
}
