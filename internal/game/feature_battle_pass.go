package game

import (
	"fmt"
	"log/slog"
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
	item := battlePassScoreItems[int32(id)]
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

// battlePassScoreItems is table activity's para_5 for every BattlePass_new (type 142) row, tables
// 40413: the pass's score item, whose count in a task's payReward is the task's premium exp
// (ActBattlePassInfo.lua:244-249, 103-135). Every type-142 row has one. The generated activity
// table does not carry para_5 yet; TestBattlePassScoreItemsCoverTheActivityTable keeps this map in
// step with it, and TestBattlePassScoreItemsMatchTheDump checks it against a dump.
var battlePassScoreItems = map[int32]int64{
	5038: 620011, 5039: 620011, 5405: 654108, 70002: 620008, 70003: 620009, 70004: 620010, 70005: 620011,
	70006: 620008, 70007: 620009, 70008: 620010, 70009: 620011, 70010: 620011, 70011: 620011, 70012: 620019,
	70013: 620019, 70014: 620019, 70015: 620011, 70016: 620011, 70017: 620011, 70018: 620011, 70019: 620011,
	70020: 620011, 70021: 620010, 70022: 620019, 70023: 620019, 70024: 620011, 70025: 620011, 70026: 620011,
	70027: 620021, 70028: 620022, 70029: 620021, 70030: 620022, 70031: 620022, 70032: 620021, 98512: 620006,
	99002: 620017, 99013: 653002, 99021: 653008, 99029: 653032, 99043: 653037, 99049: 653071, 99053: 653045,
	99060: 653049, 99062: 653055, 99074: 653071, 99075: 653071, 99083: 653064, 99091: 653115, 99098: 653121,
	99105: 653127, 99109: 654104, 99123: 654113, 99133: 654145, 99140: 654150, 99148: 654173, 99158: 654183,
	99173: 654219, 99184: 654244, 99188: 654248, 99198: 654253, 1000001: 620020, 1000011: 620020,
	1000023: 620011, 1000025: 620011, 1000027: 620011, 1000028: 620011, 1000030: 620011, 1000033: 620020,
	1000066: 620020, 1000235: 620008, 1000236: 620008, 1000237: 620008, 1000238: 620022, 1000239: 620022,
	1000240: 620022, 1000241: 620021, 1000242: 620021, 1000243: 620021, 1000260: 620008, 1000277: 620008,
	1000278: 620008, 1000279: 620008, 1000280: 620022, 1000281: 620022, 1000283: 620021, 1000284: 620021,
	1000310: 620022, 1000311: 620021, 1000317: 620008, 1000320: 620022, 1000321: 620022, 1000322: 620022,
	1000506: 620008, 1000507: 620022, 1000508: 620021, 1000514: 620008, 1000607: 620022, 1010032: 620023,
	1010034: 620023, 1010036: 620023, 1010038: 620023, 1010040: 620023, 1010042: 620023, 1010245: 620024,
	1010246: 620008, 1010247: 620024, 1010248: 620024, 1010250: 620022, 1010251: 620022, 1010252: 620022,
	1010253: 620022, 1010255: 620021, 1010256: 620021, 1010257: 620021, 1010258: 620021, 1010295: 620022,
	1010296: 620022, 1010297: 620022, 1010298: 620022, 1010300: 620025, 1010301: 620025, 1010302: 620025,
	1020001: 620023, 1020005: 620023, 1020009: 620023, 1020217: 620024, 1020218: 620025, 1020219: 620022,
	1020220: 620021, 1030217: 620024, 1030218: 620025, 1030219: 620022, 1030220: 620021, 1031001: 620023,
	1031003: 620023, 1031005: 620023, 1041001: 620023, 1041003: 620023, 1041005: 620023, 1041015: 620024,
	1041016: 620025, 1041017: 620022, 1041018: 620021, 1051001: 620026, 1051003: 620026, 1051005: 620026,
	1051015: 620024, 1051016: 620025, 1051017: 620022, 1051018: 620021, 1071014: 620024, 1071015: 620025,
	1071016: 620022, 1200006: 620020, 1200032: 620020, 1200050: 620020, 1200110: 620020, 1201006: 620020,
	10202181: 620025,
}
