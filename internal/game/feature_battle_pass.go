package game

import (
	"fmt"
	"strconv"

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
//     (UIBattlePassNormalNewTaskCell.lua:77-105). State 2 with a locked premium pass opens the
//     purchase popup in the client and is never sent.
//   - Levels: receive.battlepass.all.reward / receive.bpv2.all.reward {activityId: Int} when a
//     reached level's free row is unclaimed (ActBattlePassInfo:GetRedNum, :374-435; the one-click
//     button, UIBattlePass.lua:858-868). The request has no track parameter: the server pays the
//     premium rows only when the pass was bought (unlock 1), so it never buys anything; its handler
//     never touches gold, unlike buy.battle.pass.level, which this never sends. Whether the server
//     also pays the overflow box is unverified.
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
	battlePassRowPending = 0 // stateInfo normalState: unclaimed
)

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

	for _, t := range evObjects(info, "taskArr") {
		if b.dead {
			return
		}
		if st, _ := evNum(t, "state"); st != battlePassTaskReady {
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
