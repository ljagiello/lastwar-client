package game

import (
	"fmt"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance congratulation likes (MASTER.md §4 #23), static-only and OPT-IN: each like pays the
// giver but is visible to the liked member.
//
// alliance.congratulation.gain.congratulation.list {} returns congratulationList[{uid, configId,
// expireTimeStamp (ms), selfThumb}] and count, the rewards left today (alliance_Congratulations.k5
// per day). alliance.congratulation.thumbs.up {targetUid: UtfString, configId: UtfString, type:
// Int 1} likes one entry that is someone else's, unexpired and not yet liked; the client's list pop
// likes every such entry (AllianceCongratulationDataManager.lua:43-62, 121-151, 206-213;
// LWAllianceCongratulationListPopView.lua:70-92). This stops once count reaches 0, where likes no
// longer pay.
//
// Not sent: the arena and world-boss praise likes (their target lists need more tracing).
const (
	congratulationListCmd = "alliance.congratulation.gain.congratulation.list"
	congratulationLikeCmd = "alliance.congratulation.thumbs.up"
)

func init() {
	registerFeature(Feature{
		Name:    "alliance-likes",
		Summary: "OPT-IN: like alliance congratulations while likes still pay (visible to the liked member); static-only",
		Run:     runAllianceLikes,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, congratulationLikeCmd)
}

func runAllianceLikes(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !evInAlliance(in) {
		return nil
	}
	me := evString(in.Object("user"), "uid")
	b := &evBatch{conn: conn}
	msg := b.send("alliance congratulation list", congratulationListCmd, sfs.NewSFSObject())
	if msg == nil {
		return b.err()
	}
	left, _ := evNum(msg.Params, "count")
	now := evNow().UnixMilli()
	seen := map[string]bool{}
	for _, e := range evObjects(msg.Params, "congratulationList") {
		if left <= 0 || b.dead {
			break
		}
		uid, cfg := evString(e, "uid"), evString(e, "configId")
		exp, _ := evNum(e, "expireTimeStamp")
		key := uid + "_" + cfg
		if uid == "" || cfg == "" || uid == me || now >= exp || evBool(e, "selfThumb") || seen[key] {
			continue
		}
		seen[key] = true
		p := sfs.NewSFSObject()
		p.PutUtfString("targetUid", uid)
		p.PutUtfString("configId", cfg)
		p.PutInt("type", 1)
		r := b.send(fmt.Sprintf("alliance congratulation like (config %s)", cfg), congratulationLikeCmd, p)
		if r == nil {
			continue
		}
		if n, ok := evNum(r.Params, "count"); ok {
			left = n
		} else {
			left--
		}
	}
	return b.err()
}
