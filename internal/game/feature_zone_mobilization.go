package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Zone mobilization free claims (MASTER.md §3.2 #9, gap ZONEMOB), opt-in and static-only: none of
// these commands has been sent live.
//
// Gate: ZoneMobilization has no EnumActivity type and no init activity entry. The client sends
// get.zone.mobilization.info {tabType: Int 4} whenever the main UI is built (UIMainTop.lua:286-293,
// 543-545; GetZoneMobilizationInfoMessage.lua:4-7) and treats the event as running only while the
// reply's endTime (ms) is not past (InitZoneMobilizationInfoData, LWZoneMobilizationManager.lua:
// 103-113). Its entrance shows from HQ level zone_mobilization.k24, 6 in table item 39879
// (ActZoneMobilizationItem.lua:65-70); below that nothing is sent.
//
// From that one reply, parsed as LWZoneMobilizationManager.lua:114-149 does:
//
//   - zone.mobilization.claim.challenge.reward {index: Int 0}, the client's claim-all
//     (LWUIZoneMobilizationAttackSubView.lua:389-391), when an attackInfo.rewardList entry
//     {target, rewarded} is not rewarded and attackInfo.damage reaches its target (CanReceiveReward,
//     LWZoneMobilizationAttackInfo.lua:32-53, 63-72; LWZoneMobilizationAttackOrDefendRewardInfo.lua:
//     15-22).
//   - zone.mobilization.claim.defend.reward {index: Int 1-based} per defendInfo.rewardList box not
//     rewarded whose target is at least the enemy boss's remaining HP percent: defendInfo.curHp /
//     zone_mobilization_boss[defendInfo.bossId].rallyboss_hp * 100, or 100 while the enemy has no
//     boss placed (bossPointId 0) (LWUIZoneMobilizationDefendSubView.lua:329-347;
//     LWUIZoneMobilizationBoxRewardItemRender.lua:78-108; LWZoneMobilizationManager.lua:180-186).
//   - zone.mobilization.donate.box.reward {boxType: Int} for the personal box (1) while
//     donateInfo.personalDonateInfo.boxNum > 0, and for the alliance box (2) while
//     donateInfo.allianceDonateInfo.boxNum > 0 and the account is in an alliance (the box is locked
//     otherwise) (LWUIZoneMobilizationDonatedInfoItemRender.lua:47-52, 252-266;
//     LWZoneMobilizationDonatedInfo.lua:94-117; EnumType.lua:16378).
//   - zone.mobilization.donate.progress.reward {} when IsCanReceiveDonatedStageReward holds
//     (LWZoneMobilizationDonatedInfo.lua:132-141, 167-215; LWUIZoneMobilizationDonatedSubView.lua:
//     439-452).
//
// `rewarded` is read the way the Lua reads `message.rewarded or false`: only absent or false is
// unclaimed (a number, even 0, is truthy). Never sent: the donations (they spend item
// zone_mobilization.k7), airship or boss placement, marches to the boss, and the daily-task and
// supply-point claims. No benign error codes: an errorCode on a claim is logged (SendAndWait logs
// every reply in full, at Info on success) and stops that command.
const (
	zoneMobFeature      = "zone-mobilization"
	zoneMobInfoCmd      = "get.zone.mobilization.info"
	zoneMobChallengeCmd = "zone.mobilization.claim.challenge.reward"
	zoneMobDefendCmd    = "zone.mobilization.claim.defend.reward"
	zoneMobBoxCmd       = "zone.mobilization.donate.box.reward"
	zoneMobProgressCmd  = "zone.mobilization.donate.progress.reward"
	zoneMobTabRefresh   = 4 // the tabType every generic refresh sends (LWZoneMobilizationManager.lua:195, 265, 292)
	zoneMobMinHQ        = 6 // DataConfig zone_mobilization.k24 (table item 39879)
	zoneMobBoxPersonal  = 1 // ZoneMobilizationDonatedBoxType.Personal (EnumType.lua:16378)
	zoneMobBoxAlliance  = 2 // ZoneMobilizationDonatedBoxType.Alliance
	zoneMobFullHPPct    = 100.0
	zoneMobStageClaimed = 2 // TaskState.Received (EnumType.lua:5258-5264)
)

// zoneMobBossHP is table zone_mobilization_boss's rallyboss_hp by boss id, all 53 rows of tables
// 39879 (GetOppositeBossMaxHp, LWZoneMobilizationManager.lua:180-186).
var zoneMobBossHP = map[int64]int64{
	1: 2000, 2: 2000, 3: 2000, 4: 2000, 5: 2000, 11: 2000, 12: 4000, 13: 6000, 14: 8000, 15: 10000, 16: 12000,
	17: 14000, 18: 16000, 21: 1800, 22: 3600, 23: 5400, 24: 7200, 25: 9000, 26: 10800, 27: 12600, 28: 14400,
	31: 1600, 32: 3200, 33: 4800, 34: 6400, 35: 8000, 36: 9600, 37: 11200, 38: 12800, 41: 1200, 42: 2400,
	43: 3600, 44: 4800, 45: 6000, 46: 7200, 47: 8400, 48: 9600, 51: 1000, 52: 2000, 53: 3000, 54: 4000,
	55: 5000, 56: 6000, 57: 7000, 58: 8000, 61: 1000, 62: 2000, 63: 3000, 64: 4000, 65: 5000, 66: 6000,
	67: 7000, 68: 8000,
}

func init() {
	registerFeature(Feature{
		Name: zoneMobFeature,
		Summary: "OPT-IN: claim zone mobilization challenge, defence, donation-box and donation-stage rewards; " +
			"never donates (static-only, not validated live)",
		Run: runZoneMobilization,
	})
}

func runZoneMobilization(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if hq := activityHQLevel(in); hq < zoneMobMinHQ {
		slog.Info(zoneMobFeature+": HQ below the event's entrance level; skipping", "hqLevel", hq, "need", zoneMobMinHQ)
		return nil
	}
	b := &evBatch{conn: conn}
	p := sfs.NewSFSObject()
	p.PutInt("tabType", zoneMobTabRefresh)
	msg := b.send("zone mobilization info", zoneMobInfoCmd, p)
	if msg == nil {
		return b.err()
	}
	info := msg.Params
	end, ok := evNum(info, "endTime")
	if !ok || end < evNow().UnixMilli() {
		slog.Info(zoneMobFeature+": event not running", "endTime", info.GetLong("endTime"), "hasEndTime", ok)
		return nil
	}
	attack, defend, donate := evObject(info, "attackInfo"), evObject(info, "defendInfo"), evObject(info, "donateInfo")
	stage, _ := evNum(info, "stage")
	slog.Info(zoneMobFeature+": event info", "stage", stage, "endTime", end,
		"pointId", info.GetLong("pointId"), "bossId", info.GetLong("bossId"),
		"attackInfo", attack != nil, "attackDamage", attack.GetLong("damage"), "attackRewards", len(evObjects(attack, "rewardList")),
		"defendInfo", defend != nil, "defendCurHp", defend.GetLong("curHp"), "defendBossId", defend.GetLong("bossId"),
		"defendBossPointId", defend.GetLong("bossPointId"), "defendRewards", len(evObjects(defend, "rewardList")),
		"donateInfo", donate != nil, "personalBoxNum", evObject(donate, "personalDonateInfo").GetLong("boxNum"),
		"allianceBoxNum", evObject(donate, "allianceDonateInfo").GetLong("boxNum"),
		"donateProgress", donate.GetLong("donateProgress"), "maxProgress", donate.GetLong("maxProgress"),
		"stageRewards", len(evObjects(donate, "stageReward")), "showRedPoint", evObject(info, "showRedPoint").StringRedacted())

	if zoneMobChallengeDue(attack) {
		c := sfs.NewSFSObject()
		c.PutInt("index", 0)
		b.send("zone mobilization challenge rewards", zoneMobChallengeCmd, c)
	}
	for _, i := range zoneMobDefendDue(defend) {
		if b.dead {
			return b.err()
		}
		d := sfs.NewSFSObject()
		d.PutInt("index", i)
		if b.send(fmt.Sprintf("zone mobilization defence box %d", i), zoneMobDefendCmd, d) == nil {
			break
		}
	}
	boxes := []struct {
		typ int32
		key string
		ok  bool
	}{
		{zoneMobBoxPersonal, "personalDonateInfo", true},
		{zoneMobBoxAlliance, "allianceDonateInfo", claimInAlliance(in)},
	}
	for _, bx := range boxes {
		if n, _ := evNum(evObject(donate, bx.key), "boxNum"); n <= 0 || !bx.ok || b.dead {
			continue
		}
		d := sfs.NewSFSObject()
		d.PutInt("boxType", bx.typ)
		if b.send(fmt.Sprintf("zone mobilization donation box %d", bx.typ), zoneMobBoxCmd, d) == nil {
			break
		}
	}
	if zoneMobProgressDue(donate, stage) {
		b.send("zone mobilization donation stage rewards", zoneMobProgressCmd, sfs.NewSFSObject())
	}
	return b.err()
}

// zoneMobLuaTruthy is Lua truthiness of o[key]: absent and false are false, anything else true.
func zoneMobLuaTruthy(o *sfs.SFSObject, key string) bool {
	v, ok := o.Get(key)
	if !ok || v.Val == nil {
		return false
	}
	b, isBool := v.Val.(bool)
	return !isBool || b
}

// zoneMobChallengeDue is LWZoneMobilizationAttackInfo:CanReceiveReward (:63-72).
func zoneMobChallengeDue(attack *sfs.SFSObject) bool {
	damage, _ := evNum(attack, "damage")
	for _, r := range evObjects(attack, "rewardList") {
		target, _ := evNum(r, "target")
		if !zoneMobLuaTruthy(r, "rewarded") && damage >= target {
			return true
		}
	}
	return false
}

// zoneMobDefendDue returns the 1-based defence boxes the defend panel lets the player open
// (LWUIZoneMobilizationDefendSubView.lua:329-347; LWUIZoneMobilizationBoxRewardItemRender.lua:97-102).
// The percent is computed in float64 as the Lua does. A boss id the embedded table lacks (or 0)
// sends nothing: the client's division has no HP to divide by either.
func zoneMobDefendDue(defend *sfs.SFSObject) []int32 {
	rewards := evObjects(defend, "rewardList")
	if len(rewards) == 0 {
		return nil
	}
	bossID, _ := evNum(defend, "bossId")
	maxHP := zoneMobBossHP[bossID]
	if maxHP <= 0 {
		slog.Info(zoneMobFeature+": enemy boss not in the embedded zone_mobilization_boss rows; no defence boxes", "bossId", bossID)
		return nil
	}
	pct := zoneMobFullHPPct
	if pointID, _ := evNum(defend, "bossPointId"); pointID > 0 {
		curHP, _ := evNum(defend, "curHp")
		pct = float64(curHP) / float64(maxHP) * 100
	}
	var due []int32
	for i, r := range rewards {
		target, _ := evNum(r, "target")
		if !zoneMobLuaTruthy(r, "rewarded") && pct <= float64(target) {
			due = append(due, int32(i+1))
		}
	}
	return due
}

// zoneMobProgressDue is LWZoneMobilizationDonatedInfo:IsCanReceiveDonatedStageReward (:167-215)
// over donateInfo.stageReward[{target, state}] and the reply's top-level stage. No entries counts
// as all received, so nothing is due.
func zoneMobProgressDue(donate *sfs.SFSObject, stage int64) bool {
	rewards := evObjects(donate, "stageReward")
	allReceived := true
	for _, r := range rewards {
		if st, _ := evNum(r, "state"); st != zoneMobStageClaimed {
			allReceived = false
		}
	}
	if allReceived {
		return false
	}
	progress, _ := evNum(donate, "donateProgress")
	maxProgress, _ := evNum(donate, "maxProgress")
	if progress >= maxProgress {
		received := false
		for _, r := range rewards {
			if target, _ := evNum(r, "target"); target == stage {
				st, _ := evNum(r, "state")
				received = st == zoneMobStageClaimed
				break
			}
		}
		if !received {
			return true
		}
	}
	for _, r := range rewards {
		if st, _ := evNum(r, "state"); st == eventTaskCanReceive {
			return true
		}
	}
	return false
}
