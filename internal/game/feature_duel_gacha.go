package game

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel score gacha (the duel "wheel", MASTER G20), OPT-IN: read its balance and pity
// counter, and claim the pity ("wish") reward once the counter is full. It never draws.
//
// The client reads the gacha with `al.duel.lottery.get.info {}` (AlDuelLotteryGetInfoMessage.lua:
// 4-6), sent from the main screen's duel button and the duel pages only while
// AllyDuelScoreGachaManager:IsScienceOpen (AllyDuelScoreGachaManager.lua:131-136, 298-305;
// UIMainAllyDuelBtn.lua:9-14): DataConfig switch alliance_duel_zhuanpan on (init dataConfig; table
// item 40413 has no such row) and science 90010200's time condition, whose template is row
// 90010201 (GetScienceTemplateById and GetScienceTemplate add level 1, ScienceTemplateManager.lua:
// 43-65) with time_condition "1;57": past season 1, or season 1 from day 57 (ScienceTemplate.lua:
// 267-278, duelSeasonDay).
// The reply is configId, score, protect {num, protectNum, reward[]} and posArray
// (AllyDuelScoreGachaManager.lua:80-92, AllyDuelScoreGachaData.lua:10-22).
//
// The pity claim is `al.duel.lottery.get.protect.reward {configId: Int}`
// (AlDuelLotteryGetProtectRewardMessage.lua:4-7), which the wish panel sends when CanClaimWish
// holds (UIAllyDuelWishContentComponent.lua:118-128): protect.num > 0, protectNum >= num and a
// wish reward in protect.reward (AllyDuelScoreGachaData.lua:83-114). Here it also needs a configId
// in the reply (the manager's serverUnlock) and init effect ALCOMPETE_SCORE_GACHA_UNLOCK 92019 > 0
// (IsUnlock, AllyDuelScoreGachaManager.lua:267-269; init effect is a lower bound of the client's).
// protectNum moves only in draw replies (:182-195), so this collects what the player's own draws
// filled. The draw itself (AlDuelLotteryLotteryMessage.lua:6-7, 10,000,000 score per draw,
// DataConfig alliance_duel_league k6) spends the balance on a random prize; the user ruled it in
// as the separate opt-in duel-gacha-draw (feature_duel_gacha_draw.go), so this file has no draw
// command; the balance is logged as the number of draws it would pay for.
//
// duel-gacha-plan sends only the info read and logs whether duel-gacha would claim. The pity
// claim scores no duel points, so neither feature is duel-held. Static-only, not validated live.
const (
	duelGachaFeature     = "duel-gacha"
	duelGachaPlanFeature = "duel-gacha-plan"
	duelGachaInfoCmd     = "al.duel.lottery.get.info"
	duelGachaPityCmd     = "al.duel.lottery.get.protect.reward"
	duelGachaSwitch      = "alliance_duel_zhuanpan"
	duelGachaUnlock      = 92019    // EffectDefine.ALCOMPETE_SCORE_GACHA_UNLOCK (EnumType.lua:4480)
	duelGachaDrawCost    = 10000000 // DataConfig alliance_duel_league k6 (table item 40413): score per draw
	duelGachaSeason      = 1        // APS_science#90010201 time_condition "1;57" (tables 40413)
	duelGachaSeasonDay   = 57
)

func init() {
	registerFeature(Feature{
		Name: duelGachaFeature,
		Summary: "OPT-IN: read the Alliance Duel score gacha's balance and pity counter and claim the full pity (wish) reward; " +
			"never draws (static-only, not validated live)",
		DefaultOn: false,
		Run:       runDuelGacha,
	})
	registerFeature(Feature{
		Name:      duelGachaPlanFeature,
		Summary:   "read-only: log the Alliance Duel score gacha's balance and pity counter; sends only al.duel.lottery.get.info (static-only, not validated live)",
		DefaultOn: false,
		Run:       runDuelGachaPlan,
	})
}

func runDuelGacha(conn *session.GameConn, in *Init) error { return duelGachaRun(conn, in, false) }

func runDuelGachaPlan(conn *session.GameConn, in *Init) error { return duelGachaRun(conn, in, true) }

// duelGachaGate says why the client would not read the gacha now, or "" when it would.
func duelGachaGate(in *Init, now time.Time) string {
	if !claimInAlliance(in) {
		return "not in an alliance"
	}
	if !claimSwitchOn(in, duelGachaSwitch) {
		return "init dataConfig does not turn on " + duelGachaSwitch
	}
	season, day, dayOK := duelSeasonDay(in, now)
	switch {
	case season > duelGachaSeason:
	case season == duelGachaSeason && dayOK && day >= duelGachaSeasonDay:
	default:
		return fmt.Sprintf("science 90010200 opens in season %d from day %d (season %d, day %d known %v)",
			duelGachaSeason, duelGachaSeasonDay, season, day, dayOK)
	}
	return ""
}

// duelGachaState is what the info reply holds.
type duelGachaState struct {
	configID          int64
	hasConfig         bool
	score             int64
	pity, pityCounter int64 // protect.num, protect.protectNum
	pityRewards       int   // entries in protect.reward
}

func duelGachaStateOf(reply *sfs.SFSObject) duelGachaState {
	var st duelGachaState
	st.configID, st.hasConfig = claimInt(reply, "configId")
	st.score, _ = claimInt(reply, "score")
	protect := evObject(reply, "protect")
	st.pity, _ = claimInt(protect, "num")
	st.pityCounter, _ = claimInt(protect, "protectNum")
	if v, ok := protect.Get("reward"); ok {
		if a, ok := v.Val.(*sfs.SFSArray); ok && a != nil {
			st.pityRewards = len(a.Items())
		}
	}
	return st
}

// pityDue says why the pity reward can't be claimed, or "" when it can.
func (st duelGachaState) pityDue(in *Init) string {
	switch {
	case !st.hasConfig || st.configID <= 0 || st.configID > math.MaxInt32:
		return "the reply carries no usable configId (the gacha is not unlocked server-side)"
	case evInitEffect(in, duelGachaUnlock) <= 0:
		return "init effect 92019 (ALCOMPETE_SCORE_GACHA_UNLOCK) is not above 0"
	case st.pity <= 0 || st.pityCounter < st.pity:
		return "the pity counter is not full"
	case st.pityRewards == 0:
		return "protect.reward names no wish reward"
	}
	return ""
}

func duelGachaRun(conn *session.GameConn, in *Init, dry bool) error {
	name := duelGachaFeature
	if dry {
		name = duelGachaPlanFeature
	}
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	if why := duelGachaGate(in, evNow()); why != "" {
		slog.Info(name + ": the client would not read the gacha: " + why)
		return nil
	}
	// SendAndWait logs the whole reply at Info; an errorCode reply is an error and stops here.
	info, err := session.SendAndWait(conn, name+": gacha info", duelGachaInfoCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	st := duelGachaStateOf(info.Params)
	slog.Info(name+": gacha state", "configId", st.configID, "hasConfigId", st.hasConfig, "score", st.score,
		"drawsTheScorePays", st.score/duelGachaDrawCost, "pity", st.pity, "pityCounter", st.pityCounter,
		"pityRewards", st.pityRewards, "unlockEffect", evInitEffect(in, duelGachaUnlock))
	if why := st.pityDue(in); why != "" {
		slog.Info(name + ": no pity reward to claim: " + why)
		return nil
	}
	if dry {
		slog.Info(name+": WOULD claim the pity reward", "cmd", duelGachaPityCmd, "configId", st.configID)
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutInt("configId", int32(st.configID))
	msg, err := claimAndLog(conn, name+": pity reward", duelGachaPityCmd, p)
	if err != nil {
		return err
	}
	after, ok := claimInt(msg.Params, "protectNum")
	slog.Info(name+": pity reward claimed", "configId", st.configID, "pityCounterAfter", after, "pityCounterInReply", ok)
	return nil
}
