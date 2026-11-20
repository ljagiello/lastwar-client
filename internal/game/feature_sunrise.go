package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Sunrise Foundation free track (MASTER.md G1; cov-ui-store F4 and its verdict), OPT-IN, a claim
// only. Static-only, from the 1.0.370 Lua and tables 40413, not sent live yet.
//
// For an open SunriseFoundation activity (type 183, EnumType.lua:6362; 94310, 94312 and 94322 in
// 40413) the client sends sunrise.info {activityId: Int} when the activity is listed
// (ActivityListDataManager.lua:431-435; SunriseInfoMessage.lua:4-7). The reply carries unlock (1 when
// the paid track was bought) and rewardArr[{level, rewardFlag, specialRewardFlag}]
// (ActSunriseFoundationData.lua:17-33). GetCanRewardNum (:41-66) counts each row whose level is at or
// below the HQ level (BuildManager.MainLv) with rewardFlag 0, plus, with the track bought, those with
// specialRewardFlag 0. The page's claim-all button sends sunrise.receive.reward {activityId: Int}
// when that count is above 0 (UILWSunriseFoundation.lua:318-324; SunriseReceiveRewardMessage.lua:
// 4-7); a free cell at or below the HQ level sends the same (UILWSunriseFoundationTargetItem.lua:
// 190-210). The free rows never raise a red dot (GetRedNum counts the paid track only, :35-39), so
// they are easy to miss by hand. Never the purchase popup (UISunriseFoundationPopUpPanel).
//
// The claim's reply is logged at Info; an errorCode stops the feature (no benign code is
// registered for a command not validated live).
const (
	sunriseFeature         = "sunrise-foundation"
	sunriseActType   int32 = 183 // EnumActivity.SunriseFoundation
	sunriseInfoCmd         = "sunrise.info"
	sunriseRewardCmd       = "sunrise.receive.reward"
)

func init() {
	registerFeature(Feature{
		Name:    sunriseFeature,
		Summary: "OPT-IN: claim the Sunrise Foundation's reached HQ-level rows (free track; the paid one only if bought) (static-only, not validated live)",
		Run:     runSunrise,
	})
}

// sunriseDue is GetCanRewardNum (ActSunriseFoundationData.lua:41-66): the reached rows with the free
// reward unclaimed, and, when the track was bought (unlock 1), those with the paid one unclaimed.
func sunriseDue(info *sfs.SFSObject, hq int32) (free, paid int) {
	unlock, _ := evNum(info, "unlock")
	for _, r := range evObjects(info, "rewardArr") {
		lv, ok := evNum(r, "level")
		if !ok || lv > int64(hq) {
			continue
		}
		if f, ok := evNum(r, "rewardFlag"); ok && f == 0 {
			free++
		}
		if sp, ok := evNum(r, "specialRewardFlag"); ok && unlock == 1 && sp == 0 {
			paid++
		}
	}
	return free, paid
}

func runSunrise(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	fetch := evIntInfo(sunriseFeature+": info", sunriseInfoCmd, "activityId")
	for _, a := range s.Open(sunriseActType) {
		if b.dead {
			break
		}
		info, err := fetch(s, a)
		if err != nil {
			b.add(err)
			break // a command not validated live: an error stops the feature
		}
		free, paid := sunriseDue(info, s.hq)
		unlock, _ := evNum(info, "unlock")
		slog.Info(sunriseFeature+": info", "activity", a.ID, "unlock", unlock, "rows", len(evObjects(info, "rewardArr")),
			"hq", s.hq, "freeDue", free, "paidDue", paid)
		if free+paid == 0 {
			continue
		}
		if !evOptInClaim(b, sunriseFeature, fmt.Sprintf("rows (activity %d)", a.ID), sunriseRewardCmd, evActivityIDParam(a)) {
			break
		}
		s.Forget(sunriseInfoCmd, a)
	}
	return b.err()
}

// evOptInClaim sends one claim of an opt-in, static-only feature and logs what the reply granted at
// Info, so the first live run shows the reply. It returns false when the claim failed (an errorCode
// is a failure: no benign code is registered for these commands) or the connection is dead; the
// caller then stops that claim.
func evOptInClaim(b *evBatch, feature, what, cmd string, p *sfs.SFSObject) bool {
	r := b.send(feature+": "+what, cmd, p)
	if r == nil {
		return false
	}
	slog.Info(feature+": claimed", "what", what, "cmd", cmd, "granted", describeGrants(r.Params))
	return true
}
