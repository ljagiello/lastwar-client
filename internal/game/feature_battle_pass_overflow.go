package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Battle-pass overflow boxes (gap BPOVER, MASTER.md §3.2 #3), OPT-IN, per-box claims only.
//
// At a pass's top level every further point of pass exp fills repeatable "unlimited" boxes, free
// track included. The info reply the battle-pass feature reads (get.battlepass.info for type 27,
// hero.event.info.get for type 142) carries extraExp (exp per box), infiniteNum, extraReward and
// battlePass{level, exp} (ActBattlePassInfo:ParseOther and ParseBattlePass,
// ActBattlePassInfo.lua:27-46, 222-256). Once there is no level template for level+1 and
// extraExp > 0, GetRedNum counts floor((exp - extraExp*infiniteNum) / extraExp) boxes for type 142
// and floor(exp / extraExp) for type 27 (:403-413); the box view does the same
// (BattlePassUnlimitedBox.lua:117-160). Its Get button sends receive.bpv2.extra.reward (142) or
// receive.battlepass.extra.reward (27), both {activityId: Int}, while extraReward is non-empty
// (BattlePassUnlimitedBox.lua:180-193; IsHaveExtraReward, ActBattlePassInfo.lua:610-616;
// NewReceiveBPExtraRewardMessage.lua:4-7, ReceiveBattlePassExtraRewardMessage.lua:5-8). Neither
// message carries gold, and buy.battle.pass.level is never sent.
//
// Go has no level-template table (battlepass, battlepass_v2), so "no template for level+1" is
// approximated: the level must reach the highest stateInfo level, and stateInfo must list every
// level from 1 up to it (the tables start at level 0, which ParseStateInfo drops, :48-71). With a
// gap nothing is sent. The type the client uses is the reply's activityType (ParseOther, :232-234);
// a reply whose activityType disagrees with table `activity` is skipped. The login passes
// (showUiType 2 SevenDayLogin, 6 NewYearSevenDayLogin, 9 ValentineLogin, EnumType.lua:14151-14158)
// count no boxes in GetRedNum (:357-369) and their views have no unlimited-box widget, so the
// client never claims one there: battlePassLoginPasses skips them before any request.
//
// The ON battle-pass feature runs first (by name) and its claims change exp and level, so the
// cached info reply is dropped and read again before the first box, and after every box. One box
// per claim, as the button does: claim, re-read, repeat while the count falls, at most
// battlePassOverflowMaxClaims per pass per run. Whether the server pays several boxes per call, or
// already pays them through claim-all, is open (MASTER.md §8), so each reply's exp and infiniteNum
// are logged. Static-only, not validated live.
const (
	battlePassOverflowFeature   = "battle-pass-overflow"
	battlePassExtraCmd          = "receive.battlepass.extra.reward"
	battlePassV2ExtraCmd        = "receive.bpv2.extra.reward"
	battlePassOverflowMaxClaims = 20
)

// battlePassLoginPasses are the type-27 rows of table activity 39879 with showUiType 2, 6 or 9
// (no type-142 row has those); Go's activity table carries no showUiType.
var battlePassLoginPasses = map[int32]bool{
	13: true, 2101: true, 5001: true, 14004: true, 99164: true, // 2 SevenDayLogin
	99022: true,                                        // 6 NewYearSevenDayLogin
	99044: true, 99093: true, 99107: true, 99114: true, // 9 ValentineLogin
}

func init() {
	registerFeature(Feature{
		Name:      battlePassOverflowFeature,
		Summary:   "OPT-IN: claim battle-pass overflow boxes at the top pass level, one box per claim (static-only, not validated live)",
		DefaultOn: false,
		Run:       runBattlePassOverflow,
	})
}

func runBattlePassOverflow(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, a := range s.Open(actTypeBattlePass, actTypeBattlePassNew) {
		if b.dead {
			break
		}
		if battlePassLoginPasses[a.ID] {
			slog.Info(battlePassOverflowFeature+": login pass, which has no overflow box; skipping", "activity", a.ID)
			continue
		}
		claimBattlePassOverflow(s, b, a)
	}
	return b.err()
}

// battlePassFreshInfo drops the sweep's cached info reply for a and reads it again.
func battlePassFreshInfo(s *ActivitySweep, a Activity) (*sfs.SFSObject, error) {
	if a.Type == actTypeBattlePassNew {
		s.Forget("hero.event.info.get", a)
		return s.EventInfo(a)
	}
	s.Forget(battlePassInfoCmd, a)
	p := sfs.NewSFSObject()
	p.PutInt("activityId", a.ID)
	return s.Info("battle pass info response", battlePassInfoCmd, a, p)
}

func claimBattlePassOverflow(s *ActivitySweep, b *evBatch, a Activity) {
	cmd := battlePassExtraCmd
	if a.Type == actTypeBattlePassNew {
		cmd = battlePassV2ExtraCmd
	}
	prev := int64(-1)
	for claims := 0; ; claims++ {
		info, err := battlePassFreshInfo(s, a)
		if err != nil {
			b.add(err)
			return
		}
		o := battlePassOverflowState(info, a.Type)
		slog.Info(battlePassOverflowFeature+": pass state", "activity", a.ID, "type", a.Type, "level", o.level,
			"maxStateLevel", o.maxLevel, "exp", o.exp, "extraExp", o.extraExp, "infiniteNum", o.infiniteNum,
			"boxes", o.boxes, "claimed", claims, "skip", o.skip)
		switch {
		case o.boxes <= 0:
			return
		case prev >= 0 && o.boxes >= prev:
			slog.Info(battlePassOverflowFeature+": the box count did not fall after a claim; stopping", "activity", a.ID,
				"before", prev, "after", o.boxes)
			return
		case claims >= battlePassOverflowMaxClaims:
			slog.Info(battlePassOverflowFeature+": per-run claim bound reached", "activity", a.ID, "left", o.boxes)
			return
		}
		prev = o.boxes
		msg := b.send(fmt.Sprintf("battle pass overflow box (activity %d)", a.ID), cmd, evActivityIDParam(a))
		if msg == nil {
			return // logged by SendAndWait and kept in b; an unvalidated claim is not retried
		}
		pass := evObject(msg.Params, "battlePass")
		exp, _ := evNum(pass, "exp")
		level, _ := evNum(pass, "level")
		recv, _ := evNum(pass, "receiveNum")
		inf, hasInf := evNum(msg.Params, "infiniteNum")
		slog.Info(battlePassOverflowFeature+": box claimed", "activity", a.ID, "cmd", cmd, "replyLevel", level,
			"replyExp", exp, "replyReceiveNum", recv, "replyInfiniteNum", inf, "replyHasInfiniteNum", hasInf,
			"granted", describeGrants(msg.Params))
	}
}

// battlePassOverflow is what one info reply says about a pass's overflow boxes. skip names the
// gate that held the boxes back ("" when boxes were counted).
type battlePassOverflow struct {
	level, maxLevel, exp, extraExp, infiniteNum, boxes int64
	skip                                               string
}

// battlePassOverflowState counts the claimable overflow boxes in info, GetRedNum's overflow term
// (ActBattlePassInfo.lua:403-413) with the max-level approximation described above. typ is the
// pass's type from table `activity`.
func battlePassOverflowState(info *sfs.SFSObject, typ int32) battlePassOverflow {
	var o battlePassOverflow
	pass := evObject(info, "battlePass")
	o.level, _ = evNum(pass, "level")
	o.exp, _ = evNum(pass, "exp")
	o.extraExp, _ = evNum(info, "extraExp")
	o.infiniteNum, _ = evNum(info, "infiniteNum")
	levels := map[int64]bool{}
	for _, r := range evObjects(info, "stateInfo") {
		if l, ok := evNum(r, "level"); ok && l > 0 {
			levels[l] = true
			o.maxLevel = max(o.maxLevel, l)
		}
	}
	if t, ok := evNum(info, "activityType"); ok && t != int64(typ) {
		o.skip = fmt.Sprintf("reply activityType %d is not the table's %d", t, typ)
		return o
	}
	switch {
	case pass == nil:
		o.skip = "no battlePass"
	case o.extraExp <= 0:
		o.skip = "no extraExp"
	case !evNonEmpty(info, "extraReward"):
		o.skip = "no extraReward"
	case o.maxLevel == 0 || int64(len(levels)) != o.maxLevel:
		o.skip = "stateInfo does not list every level, so the top level is unknown"
	case o.level < o.maxLevel:
		o.skip = "below the top level"
	}
	if o.skip != "" {
		return o
	}
	exp := o.exp
	if typ == actTypeBattlePassNew {
		exp -= o.extraExp * o.infiniteNum
	}
	if exp > 0 {
		o.boxes = exp / o.extraExp
	}
	return o
}
