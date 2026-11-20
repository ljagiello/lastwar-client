package game

import (
	"log/slog"

	"lastwar-client/internal/session"
)

// DOMINATOR_COCKATRICE visitors (MASTER G35, aud-city/F7, cov-scene-chat/F4), OPT-IN, type 13 only.
// The client gives behaviour type 13 the same class as RECRUITMENT (CityVisitorManager.lua:111-112),
// which the core greet already accepts: the recruit window's two buttons both run the callback that
// sends `visitor.operate {uid: Long, operate: Int 1}` (VisitorRecruitWorker.lua:62-71,
// UIWorkerDetailRecruitView.lua:30-41), with no cost and no guard. The one type-13 row is 9002
// (para officer_list215029, tables 40413). Whether the server ever sends one, and whether its entry
// carries a worker object like the RECRUITMENT rows, is unobserved: static-only, not validated live.
//
// AllianceCongratulation (14), the other half of G35, is not accepted: it waits for the user's
// ruling (MASTER §6.1 #16).
const cockatriceVisitorsFeature = "cockatrice-visitors"

func init() {
	registerFeature(Feature{
		Name:      cockatriceVisitorsFeature,
		Summary:   "OPT-IN: accept DOMINATOR_COCKATRICE recruit visitors (type 13, visitor.operate) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runCockatriceVisitors,
	})
}

func runCockatriceVisitors(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(cockatriceVisitorsFeature + ": no init push; skipping")
		return nil
	}
	return greetOptInVisitors(conn, in, cockatriceVisitorsFeature, visitorTypeDominatorCockatrice, connNow(conn), nil)
}
