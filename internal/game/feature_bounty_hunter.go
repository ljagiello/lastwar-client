package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
)

// Bounty Hunter (type 350) score milestones and scene free chests (MASTER.md G25; aud-events F8,
// cov-ui-activity F4 and their verdicts), two OPT-IN features, claims only. Static-only, from the
// 1.0.370 Lua and tables 40413, not sent live yet. Both read bounty.hunter.get.info {activityId:
// Int}, the reply event-claims already fetches for the stash (shared through the activity sweep).
//
//   - bounty-hunter-milestones: the reply's commonKey.score and commonKey.phaseReward (the claimed
//     thresholds) (BountyHunterActData.lua:80-95, 328-333). The milestones are the thresholds of the
//     activity_hunter row (the activity's tableInfoType) score_reward column, paired with
//     score_reward_show (ParsePhaseReward, :125-159); every one of the 17 rows in 40413 reads 300,
//     1000, 1800, 3000, 5000. A milestone whose threshold the score reached and that is not in
//     phaseReward sends bounty.hunter.receive.phase.reward {activityId: Int, phaseId: Int <the
//     threshold>} (BountyHunterPhaseItemComponent.lua:64-103; BountyHunterReceivePhaseRewardMessage.
//     lua:6-7: the wire's phaseId is the score threshold, not an ordinal).
//   - bounty-hunter-chests: the reply's stageKey.chestArray[{uuid, id}] are the free chests in the
//     hunt scene (BountyHunterSceneData.lua:25-60; BountyHunterFreeChestData.lua:12-18). Tapping one
//     sends bounty.hunter.click.chest.reward {activityId: Int, targetId: UtfString tostring(uuid)}
//     with no cost (BountyHunterSceneView.lua:938-942; BountyHunterClickChestRewardMessage.lua:4-8);
//     its reply moves the chest's reward into the stash (stashReward, :10-30). After the clicks this
//     feature claims a non-empty stash with event-claims' bounty.hunter.receive.drop.reward
//     {activityId: Int} (BountyHunterActData.lua:99-101), so it does not wait for the next run.
//
// Each claim's reply is logged at Info; an errorCode stops that feature (no benign code is
// registered for the new commands).
const (
	bountyMilestonesFeature = "bounty-hunter-milestones"
	bountyChestsFeature     = "bounty-hunter-chests"
	bountyPhaseRewardCmd    = "bounty.hunter.receive.phase.reward"
	bountyClickChestCmd     = "bounty.hunter.click.chest.reward"
	// bountyHunterRows is the activity_hunter row count of tables 40413 (ids 1-17).
	bountyHunterRows = 17
)

// bountyPhaseScores is activity_hunter.score_reward's thresholds, the same in all 17 rows of tables
// 40413 ("300;388017006|1000;...|1800;...|3000;...|5000;388017010"); TestBountyHunterPhaseScoresMatchTheDump
// pins them.
var bountyPhaseScores = []int64{300, 1000, 1800, 3000, 5000}

func init() {
	registerFeature(Feature{
		Name:    bountyMilestonesFeature,
		Summary: "OPT-IN: claim Bounty Hunter score milestones the score reached (static-only, not validated live)",
		Run:     runBountyMilestones,
	})
	registerFeature(Feature{
		Name:    bountyChestsFeature,
		Summary: "OPT-IN: open the Bounty Hunter scene's free chests, then claim the stash (static-only, not validated live)",
		Run:     runBountyChests,
	})
}

var bountyHunterInfo = evIntInfo("bounty hunter info response", bountyHunterInfoCmd, "activityId")

// bountyRowKnown reports whether the activity's tableInfoType names an activity_hunter row; without
// one the client builds no milestone (UpdateData logs and returns, BountyHunterActData.lua:62-79).
func bountyRowKnown(a Activity) bool {
	return a.SubType() >= 1 && a.SubType() <= bountyHunterRows
}

func runBountyMilestones(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeBountyHunter) {
		if b.dead {
			break
		}
		if !bountyRowKnown(a) {
			slog.Info(bountyMilestonesFeature+": activity_hunter row unknown; no milestones", "activity", a.ID, "tableInfoType", a.SubType())
			continue
		}
		info, err := bountyHunterInfo(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		common := evObject(info, "commonKey")
		score, _ := evNum(common, "score")
		done := map[int64]bool{}
		for _, n := range evNums(common, "phaseReward") {
			done[n] = true
		}
		slog.Info(bountyMilestonesFeature+": info", "activity", a.ID, "hasCommonKey", common != nil, "score", score,
			"phaseReward", len(done))
		for _, need := range bountyPhaseScores {
			if score < need || done[need] {
				continue
			}
			p := evActivityIDParam(a)
			p.PutInt("phaseId", int32(need))
			if !evOptInClaim(b, bountyMilestonesFeature, fmt.Sprintf("milestone %d (activity %d, score %d)", need, a.ID, score),
				bountyPhaseRewardCmd, p) {
				return b.err()
			}
		}
	}
	return b.err()
}

func runBountyChests(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeBountyHunter) {
		if b.dead {
			break
		}
		info, err := bountyHunterInfo(s, a)
		if err != nil {
			b.add(err)
			continue
		}
		chests := evObjects(evObject(info, "stageKey"), "chestArray")
		slog.Info(bountyChestsFeature+": info", "activity", a.ID, "chests", len(chests))
		stash := false
		for _, c := range chests {
			uuid := evString(c, "uuid")
			if uuid == "" {
				continue
			}
			chestID, _ := evNum(c, "id")
			p := evActivityIDParam(a)
			p.PutUtfString("targetId", uuid)
			r := b.send(fmt.Sprintf("%s: chest %s (activity %d)", bountyChestsFeature, uuid, a.ID), bountyClickChestCmd, p)
			if r == nil {
				return b.err() // a command not validated live: an error stops the feature
			}
			slog.Info(bountyChestsFeature+": chest opened", "activity", a.ID, "uuid", uuid, "chestId", chestID,
				"chestReward", describeGrants(r.Params, "chestReward"), "stash", evNonEmpty(r.Params, "stashReward"))
			stash = stash || evNonEmpty(r.Params, "stashReward")
		}
		if len(chests) > 0 {
			s.Forget(bountyHunterInfoCmd, a)
		}
		if stash && !b.dead {
			b.send(fmt.Sprintf("%s: stash (activity %d)", bountyChestsFeature, a.ID), bountyHunterDropCmd, evActivityIDParam(a))
		}
	}
	return b.err()
}
