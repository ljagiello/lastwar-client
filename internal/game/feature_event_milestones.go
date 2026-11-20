package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Milestone boxes on event panels whose info reply event-shop-freebies already reads (MASTER.md
// G26; cov-ui-activity F2 and its verdict), OPT-IN, claims only. Static-only, from the 1.0.370 Lua
// and tables 40413, not sent live yet. The boxes exist only after the player has played the event;
// each claim is the box button's own.
//
//   - Lucky Roll (type 124): get.lucky.roll.info {activityId: Int} answers stageArr[{stage,
//     needLotteryNum, state}] and rollInfo.totalLotteryCount (ActLuckyRollInfo.lua:24-73). A stage
//     in state 0 whose needLotteryNum the total reached sends receive.lucky.roll.stage.reward
//     {activityId: Int, stage: Int} (UILuckyRollBox.lua:90-98; UILuckyRoll.lua:471-479;
//     ReceiveLuckyRollStageRewardMessage.lua:5-9).
//   - Titanium Blue Store (type 156): hero.event.info.get answers boxRewards[{index, target}],
//     boxReceive[] (claimed indexes) and totalScore (LWTitaniumBlueStoreManager.lua:64-101;
//     LWTitaniumBlueBoxRewardInfo.lua:15-25). A box not in boxReceive whose target totalScore
//     reached is CanReceive (GetBoxRewardState, :287-305), and its button sends blue.shop.box.reward
//     {activityId: Int, index: Int} (UILWTitaniumBlueBoxRewardItemRender.lua:72-83;
//     BlueShopBoxRewardMessage.lua:5-9).
//   - Slot machine (type 159): hero.event.info.get answers totalScore, rewardProcess[] (claimed
//     0-based box indexes) and taskArr[{taskId, state}] (ActSlotMachineData.lua:59-115). The box
//     thresholds are the activity_slots_info row (the activity's tableInfoType) score_reward
//     "score;reward|..." (:110-114; ActivitySlotsInfoTemplate.lua:77): 30, 100, 200, 500, 1000 in all
//     11 rows of 40413. Box i (1-based) with its threshold reached and i-1 not in rewardProcess sends
//     slots.process.reward {activityId: Int, index: Int i-1} (UIActSlotProgressItem.lua:86-90;
//     GetProgressRewardNum :275-287; SlotsProcessRewardMessage.lua:4-8). A task in state 1
//     (TaskState.CanReceive) sends slots.task.reward {activityId: Int, taskId: UtfString}
//     (UIActSlotTaskItem.lua:188-190; ActSlotMachineTaskContent.lua:192-203; SlotsTaskRewardMessage.
//     lua:4-8).
//
// Not here: Monopoly's boss and damage boxes (rich_man_boss thresholds, a normal/weak state machine
// and command names no verifier checked) and Scratch-off (its own info read): both left for a
// later pass. Each claim's reply is logged at Info; an errorCode stops the feature (no benign code
// is registered).
const (
	eventMilestonesFeature   = "event-milestone-boxes"
	luckyRollInfoCmd         = "get.lucky.roll.info"
	luckyRollStageCmd        = "receive.lucky.roll.stage.reward"
	blueStoreBoxCmd          = "blue.shop.box.reward"
	slotsProcessCmd          = "slots.process.reward"
	slotsTaskCmd             = "slots.task.reward"
	slotsTaskCanReceive      = 1  // TaskState.CanReceive
	slotsInfoRows            = 11 // activity_slots_info rows of tables 40413 (ids 1-11)
	eventMilestonesMaxClaims = 50 // a bound on claims per activity and run
)

// slotsScoreThresholds is activity_slots_info.score_reward's scores, the same in all 11 rows of
// tables 40413; TestSlotsScoreThresholdsMatchTheDump pins them.
var slotsScoreThresholds = []int64{30, 100, 200, 500, 1000}

func init() {
	registerFeature(Feature{
		Name: eventMilestonesFeature,
		Summary: "OPT-IN: claim reached milestone boxes of Lucky Roll, Titanium Blue Store and the slot machine (and its tasks) " +
			"(static-only, not validated live)",
		Run: runEventMilestones,
	})
}

// milestoneClaim is one box or task claim a panel offers.
type milestoneClaim struct {
	what string
	cmd  string
	p    *sfs.SFSObject
}

func luckyRollStageClaims(a Activity, info *sfs.SFSObject) []milestoneClaim {
	total, _ := evNum(evObject(info, "rollInfo"), "totalLotteryCount")
	var out []milestoneClaim
	for _, st := range evObjects(info, "stageArr") {
		stage, okS := evNum(st, "stage")
		need, okN := evNum(st, "needLotteryNum")
		state, okSt := evNum(st, "state")
		if !okS || !okN || !okSt || state != 0 || total < need {
			continue
		}
		p := evActivityIDParam(a)
		p.PutInt("stage", int32(stage))
		out = append(out, milestoneClaim{fmt.Sprintf("lucky roll stage %d (activity %d, draws %d/%d)", stage, a.ID, total, need),
			luckyRollStageCmd, p})
	}
	return out
}

func blueStoreBoxClaims(a Activity, info *sfs.SFSObject) []milestoneClaim {
	score, _ := evNum(info, "totalScore")
	done := map[int64]bool{}
	for _, n := range evNums(info, "boxReceive") {
		done[n] = true
	}
	var out []milestoneClaim
	for _, bx := range evObjects(info, "boxRewards") {
		idx, okI := evNum(bx, "index")
		target, okT := evNum(bx, "target")
		if !okI || !okT || done[idx] || score < target {
			continue
		}
		p := evActivityIDParam(a)
		p.PutInt("index", int32(idx))
		out = append(out, milestoneClaim{fmt.Sprintf("blue store box %d (activity %d, score %d/%d)", idx, a.ID, score, target),
			blueStoreBoxCmd, p})
	}
	return out
}

func slotsClaims(a Activity, info *sfs.SFSObject) []milestoneClaim {
	var out []milestoneClaim
	if sub := a.SubType(); sub >= 1 && sub <= slotsInfoRows {
		score, _ := evNum(info, "totalScore")
		done := map[int64]bool{}
		for _, n := range evNums(info, "rewardProcess") {
			done[n] = true
		}
		for i, need := range slotsScoreThresholds {
			if score < need || done[int64(i)] {
				continue
			}
			p := evActivityIDParam(a)
			p.PutInt("index", int32(i))
			out = append(out, milestoneClaim{fmt.Sprintf("slots box %d (activity %d, score %d/%d)", i, a.ID, score, need),
				slotsProcessCmd, p})
		}
	} else {
		slog.Info(eventMilestonesFeature+": slot machine without a known activity_slots_info row; no boxes",
			"activity", a.ID, "tableInfoType", sub)
	}
	for _, t := range evObjects(info, "taskArr") {
		id := evString(t, "taskId")
		if st, _ := evNum(t, "state"); st != slotsTaskCanReceive || id == "" {
			continue
		}
		p := evActivityIDParam(a)
		p.PutUtfString("taskId", id)
		out = append(out, milestoneClaim{fmt.Sprintf("slots task %s (activity %d)", id, a.ID), slotsTaskCmd, p})
	}
	return out
}

func runEventMilestones(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	luckyRoll := evIntInfo("lucky roll info response", luckyRollInfoCmd, "activityId")
	for _, panel := range []struct {
		typ    int32
		cmd    string
		fetch  func(*ActivitySweep, Activity) (*sfs.SFSObject, error)
		claims func(Activity, *sfs.SFSObject) []milestoneClaim
	}{
		{actTypeLuckyRoll, luckyRollInfoCmd, luckyRoll, luckyRollStageClaims},
		{actTypeBlueStore, "hero.event.info.get", (*ActivitySweep).EventInfo, blueStoreBoxClaims},
		{actTypeSlotMachine, "hero.event.info.get", (*ActivitySweep).EventInfo, slotsClaims},
	} {
		for _, a := range s.Open(panel.typ) {
			if b.dead {
				return b.err()
			}
			info, err := panel.fetch(s, a)
			if err != nil {
				b.add(err)
				continue
			}
			claims := panel.claims(a, info)
			slog.Info(eventMilestonesFeature+": panel", "activity", a.ID, "type", a.Type, "due", len(claims))
			if len(claims) > eventMilestonesMaxClaims {
				claims = claims[:eventMilestonesMaxClaims]
			}
			for _, c := range claims {
				if !evOptInClaim(b, eventMilestonesFeature, c.what, c.cmd, c.p) {
					return b.err()
				}
			}
			if len(claims) > 0 {
				s.Forget(panel.cmd, a)
			}
		}
	}
	return b.err()
}
