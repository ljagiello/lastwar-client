package game

import (
	"errors"
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Radar (detect events, MASTER.md §4 #14), claim half only: rewards of events already finished
// and the radar level rewards. Nothing here starts, places or runs an event.
//
// `get.detect.info {openWnd: Bool}` (false after init, DetectInfoGetMessage.lua:4-11;
// InitMessage.lua:652) returns detectInfo {level, rewardLevel, ...} and events {uuid (Long),
// eventId, state, ...} (RadarCenterDataManager.lua:39-54, 251-266; the client walks events with
// table.walk, so an array or a map is accepted). DetectEventState 1 is FINISHED (EnumType.lua:
// 8465-8470); each finished event is claimed with `receive.detect.event.reward {uuid: Long}`
// (DetectEventRewardReceiveMessage.lua:4-7), as the client's quick-claim does one uuid at a time
// (DetectEventCompleteOneClickBtnView.lua:262-337). Skipped, as in the client:
//   - the Dominator cockatrice guide events (types 35-37; RadarCenterDataManager.lua:60);
//   - rescue events (type 11), whose soldiers the client only takes after a confirm when they
//     would exceed the troop stock cap (:1167-1180), which init alone cannot check.
//
// rewardLevel is the lowest unclaimed radar level reward; while it is below level the client sends
// `detect.event.claim.level.reward {rewardLevel: Int}` with that value, and the reply carries the
// next rewardLevel (DetectEventLevelUpRewardBtn.lua:85-98; RadarCenterDataManager.lua:328-341).
// Level rewards are new radar events that arrive unstarted. Static-only: not sent live yet.
const (
	radarInfoCmd  = "get.detect.info"
	radarEventCmd = "receive.detect.event.reward"
	radarLevelCmd = "detect.event.claim.level.reward"

	radarStateFinished = 1
	// radarMaxLevelClaims bounds the level-reward loop against a reply that never advances.
	radarMaxLevelClaims = 50
)

// radarSkippedEvent reports whether eventId (table detect_event, 1.0.364) is a cockatrice guide
// event (ids 410000-410004, types 35-37) or a rescue event (199 and 310001-310099, type 11).
func radarSkippedEvent(eventID int64) bool {
	return (eventID >= 410000 && eventID <= 410004) || eventID == 199 || (eventID >= 310001 && eventID <= 310099)
}

func init() {
	registerFeature(Feature{
		Name:    "radar-claims",
		Summary: "claim finished radar events and radar level rewards; never starts an event",
		// Radar tasks score 10,000 duel points each on Mon/Wed/Fri (score type 82; DUEL.md §2.7),
		// so claims are held for those days. Finished events never expire.
		Duel: []DuelScore{{Type: DuelScoreRadarTask}},
		Hold: true,
		Run:  runRadarClaims,
	})
	session.RegisterBenignErrorCode("120289", radarEventCmd, radarLevelCmd)
}

func runRadarClaims(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("radar-claims: no init push; skipping")
		return nil
	}
	params := sfs.NewSFSObject()
	params.PutBool("openWnd", false)
	info, err := session.SendAndWait(conn, "radar info", radarInfoCmd, params)
	if err != nil {
		return err
	}

	var due []int64
	for _, ev := range claimObjectsOrMap(info.Params, "events") {
		uuid, ok := claimInt(ev, "uuid")
		state, _ := claimInt(ev, "state")
		eventID, _ := claimInt(ev, "eventId")
		if ok && uuid != 0 && state == radarStateFinished && !radarSkippedEvent(eventID) {
			due = append(due, uuid)
		}
	}
	errs := claimEach(due, func(uuid int64) error {
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		_, err := claimAndLog(conn, fmt.Sprintf("radar event %d", uuid), radarEventCmd, p)
		return err
	})
	if session.ContainsNonTimeoutNetError(errors.Join(errs...)) {
		return errors.Join(errs...)
	}

	var detect *sfs.SFSObject
	if v, ok := info.Params.Get("detectInfo"); ok {
		detect, _ = v.Val.(*sfs.SFSObject)
	}
	level, _ := claimInt(detect, "level")
	rewardLevel, ok := claimInt(detect, "rewardLevel")
	if !ok {
		rewardLevel = 1 // the client's default (RadarCenterDataManager.lua:39-54)
	}
	for i := 0; i < radarMaxLevelClaims && rewardLevel < level; i++ {
		p := sfs.NewSFSObject()
		p.PutInt("rewardLevel", int32(rewardLevel))
		msg, err := claimAndLog(conn, fmt.Sprintf("radar level %d reward", rewardLevel), radarLevelCmd, p)
		if err != nil {
			errs = append(errs, err)
			break
		}
		next, ok := claimInt(msg.Params, "rewardLevel")
		if !ok || next <= rewardLevel {
			break
		}
		rewardLevel = next
	}
	if len(due) == 0 && rewardLevel >= level {
		slog.Info("radar-claims: nothing to claim", "level", level, "rewardLevel", rewardLevel)
	}
	return errors.Join(errs...)
}
