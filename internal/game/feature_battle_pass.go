package game

import (
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Battle passes (MASTER.md §4 #15), static-only. Type 27 (BattlePass) is read with
// get.battlepass.info {activityId: Int}, type 142 (BattlePass_new, also the season and login
// passes) with hero.event.info.get (ActivityListDataManager.lua:261-272). Both replies carry
// battlePass{level, unlock, high_unlock}, stateInfo[{level, normalState, specialState,
// special2State}] (0 unclaimed, 1 claimed) and taskArr[{taskId, state}] (ActBattlePassData.lua:13-36,
// ActBattlePassInfo.lua:306-331).
//
//   - Tasks with state 1 (claimable) are claimed one at a time, as the task cells do:
//     receive.battlepass.task.reward {activityId: Int, taskId: Int} for type 27,
//     bp.task.reward {activityId: Int, taskId: UtfString} for type 142
//     (UIBattlePassNormalNewTaskCell.lua:82-103, 255-266). A task in state 2 (free half claimed)
//     with premium exp left (payExp != 0) is claimed the same way when a track was bought (unlock 1,
//     or high_unlock 1 as the season cell allows, UISeasonBattlePassTaskItem.lua:249-257): the
//     client's counter adds it (ActBattlePassInfo.lua:425-426) and its cell sends it. With no
//     track bought the cell opens the purchase popup instead, so that case is never sent. payExp is
//     the task's wire field, except in a BattlePass_new reply (activityType 142), where it is the
//     count of the activity's score item (table activity para_5) in the task's payReward, 0 when
//     there is none (ParseTaskArr, :73-140, 244-249).
//   - Levels: receive.battlepass.all.reward / receive.bpv2.all.reward {activityId: Int} when a
//     reached level's free row is unclaimed (ActBattlePassInfo:GetRedNum, :348-435; the one-click
//     button, UIBattlePass.lua:946-957), when a bought pass (unlock 1) has a reached premium row
//     unclaimed, or, for a pass with a high tier (the reply's highExchangeId set, HaveHighPayStage
//     :622-624) bought (high_unlock 1), a reached row whose special2State is present and 0 (:398-400;
//     the client defaults a missing one to 0, :60, but a pass without the tier never sends it). The
//     high rows count only on the general pass views, not the login or seven-day ones (showUiType
//     2, 3, 6, 9; :356-385). The request has no track parameter: the server pays the premium rows
//     only when the pass was bought, so it never buys anything; its handler never touches gold,
//     unlike buy.battle.pass.level, which this never sends. Whether the server also pays the
//     overflow box is unverified.
//
// receive.season.battlepass.all.reward is not sent: its only sender (SeasonPassManager:
// ClaimAllLevelRewardsReq) has no callers in 1.0.364, and the season pass UI uses the commands above.
const (
	battlePassInfoCmd    = "get.battlepass.info"
	battlePassTaskCmd    = "receive.battlepass.task.reward"
	battlePassV2TaskCmd  = "bp.task.reward"
	battlePassAllCmd     = "receive.battlepass.all.reward"
	battlePassV2AllCmd   = "receive.bpv2.all.reward"
	battlePassTaskReady  = 1 // task state CanReceive
	battlePassTaskDone   = 2 // task state Received: the free half claimed
	battlePassRowPending = 0 // stateInfo normalState: unclaimed
)

// battlePassNoHighRowViews are the BattlePassType showUiTypes whose GetRedNum branch counts no high
// row: SevenDayLogin 2, ActSevenDay 3, NewYearSevenDayLogin 6, ValentineLogin 9 (EnumType.lua:
// 14210-14222; ActBattlePassInfo.lua:356-385).
var battlePassNoHighRowViews = map[int32]bool{2: true, 3: true, 6: true, 9: true}

func init() {
	registerFeature(Feature{
		Name:    "battle-pass",
		Summary: "claim battle-pass tasks and reached free levels (claim-all takes no track; never buys levels); static-only",
		Run:     runBattlePass,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, battlePassTaskCmd, battlePassV2TaskCmd, battlePassAllCmd, battlePassV2AllCmd)
}

func runBattlePass(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeBattlePass, actTypeBattlePassNew) {
		if b.dead {
			break
		}
		claimBattlePass(s, b, a)
	}
	return b.err()
}

func claimBattlePass(s *ActivitySweep, b *evBatch, a Activity) {
	v2 := a.Type == actTypeBattlePassNew
	var info *sfs.SFSObject
	var err error
	if v2 {
		info, err = s.EventInfo(a)
	} else {
		p := sfs.NewSFSObject()
		p.PutInt("activityId", a.ID)
		info, err = s.Info("battle pass info response", battlePassInfoCmd, a, p)
	}
	if err != nil {
		b.add(err)
		return
	}
	pass := evObject(info, "battlePass")
	level, _ := evNum(pass, "level")
	unlock, _ := evNum(pass, "unlock")
	highUnlock, _ := evNum(pass, "high_unlock")
	bought := unlock == 1 || highUnlock == 1

	for _, t := range evObjects(info, "taskArr") {
		if b.dead {
			return
		}
		switch st, _ := evNum(t, "state"); {
		case st == battlePassTaskReady:
		case st == battlePassTaskDone && bought && battlePassTaskPayExp(info, t) != 0:
			slog.Info("battle pass: claiming a finished task's premium exp (bought pass)", "activity", a.ID,
				"taskId", evString(t, "taskId"), "unlock", unlock, "high_unlock", highUnlock)
		default:
			continue
		}
		id := evString(t, "taskId")
		if id == "" {
			id = evString(t, "id")
		}
		p := evActivityIDParam(a)
		cmd := battlePassV2TaskCmd
		if v2 {
			p.PutUtfString("taskId", id)
		} else {
			n, err := strconv.ParseInt(id, 10, 32)
			if err != nil {
				continue
			}
			p.PutInt("taskId", int32(n))
			cmd = battlePassTaskCmd
		}
		msg := b.send(fmt.Sprintf("battle pass task %s (activity %d)", id, a.ID), cmd, p)
		if msg != nil {
			if l, ok := evNum(evObject(msg.Params, "battlePass"), "level"); ok && l > level {
				level = l
			}
		}
	}

	highRows := highUnlock == 1 && evString(info, "highExchangeId") != "" && !battlePassNoHighRowViews[a.ShowUIType()]
	pending := false
	for _, r := range evObjects(info, "stateInfo") {
		l, ok := evNum(r, "level")
		if !ok || l <= 0 || l > level {
			continue
		}
		if st, ok := evNum(r, "normalState"); ok && st == battlePassRowPending {
			pending = true
		}
		if st, ok := evNum(r, "specialState"); ok && unlock == 1 && st == battlePassRowPending {
			pending = true // a bought pass: its premium rows are a free claim too
		}
		if st, ok := evNum(r, "special2State"); ok && highRows && st == battlePassRowPending {
			pending = true // the bought high tier's rows, likewise
		}
	}
	if !pending || b.dead {
		return
	}
	cmd := battlePassAllCmd
	if v2 {
		cmd = battlePassV2AllCmd
	}
	b.send(fmt.Sprintf("battle pass levels (activity %d)", a.ID), cmd, evActivityIDParam(a))
}

// battlePassTaskPayExp is the client's param.payExp of a task (ActBattlePassInfo:ParseTaskArr,
// ActBattlePassInfo.lua:73-140). A BattlePass_new reply (activityType 142) replaces the wire
// field by the count of the score item, table activity para_5 of the reply's activityId (:244-249),
// in the task's payReward, and by 0 when the activity has none or the reply names no activity. Any
// other reply keeps the wire payExp; an absent one counts as 0 here (the client's nil ~= 0 would
// show the button, so this only stays on the safe side).
func battlePassTaskPayExp(info, task *sfs.SFSObject) int64 {
	if t, _ := evNum(info, "activityType"); t != int64(actTypeBattlePassNew) {
		n, _ := evNum(task, "payExp")
		return n
	}
	id, ok := evNum(info, "activityId")
	item := battlePassScoreItem(int32(id))
	if !ok || item == 0 {
		return 0
	}
	for _, g := range decodeRewardGrants(task, "payReward") {
		if (g.Type == rewardTypeGoods || g.Type == rewardTypeItemEffect) && g.ItemID == item {
			if g.HasGained {
				return g.Gained
			}
			return g.Total
		}
	}
	return 0
}

// battlePassScoreItem is a BattlePass_new pass's score item, tonumber(para_5) of the reply's activity
// (ActBattlePassInfo.lua:244-249): its count in a task's payReward is the task's premium exp
// (:103-135). 0 when the table lacks the row or the cell is no number.
func battlePassScoreItem(id int32) int64 {
	r, _ := activityRowOf(id)
	n, err := strconv.ParseInt(strings.TrimSpace(r.para5), 10, 64)
	if err != nil {
		return 0
	}
	return n
}
