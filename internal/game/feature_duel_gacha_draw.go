package game

import (
	"fmt"
	"log/slog"
	"math"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel score gacha draw (MASTER G20, §6.1 #2; user ruling 2026-11-19), OPT-IN: spend the
// gacha's own score balance on draws, one at a time, while it pays for one. A user carve-out of the
// never-automate list: it spends only that score, never diamonds, items or any paid path.
//
// The gate and the info read are duel-gacha's (feature_duel_gacha.go): in an alliance, switch
// alliance_duel_zhuanpan on and science 90010200's time condition (IsScienceOpen,
// AllyDuelScoreGachaManager.lua:298-305), then `al.duel.lottery.get.info {}`. The draw view sits on
// the duel Today page under the same two conditions (AllyDuelToday.lua:77-81) and on the Sunday tab
// (UIAllyDuelCtrl.lua:21-26).
//
// A draw is `al.duel.lottery.lottery {configId: Int, num: Int}` (AlDuelLotteryLotteryMessage.lua:
// 4-8: those two keys only; no currency or item in the request). Every send is a button of the two
// draw views, guarded the same way (UIAllyDuelScoreGacha.lua:213-242, UIAllyDuelScoreGachaSunday.lua:
// 279-308): a configId, IsUnlock (the info reply carried a configId and effect
// ALCOMPETE_SCORE_GACHA_UNLOCK 92019 > 0, AllyDuelScoreGachaManager.lua:84-90, 267-269; init effect
// is a lower bound of the client's, so this is stricter) and IsGachaCostItemEnough: the reply's
// score >= num * gachaOnceCost (:122-129). gachaOnceCost is DataConfig alliance_duel_league k6
// (:13), 10,000,000 in table item 40413 (duelGachaDrawCost); init dataConfig cannot change it,
// because InitFromNet keeps the server's copy and InitFromLocalTable then overwrites every row the
// local item table has (DataConfig.lua:36-74, 111-144). Live tables are 40460: the first reply's
// score drop is logged as the live check of that cost.
//
// num is 1, the one-draw button (UIAllyDuelScoreGacha.lua:225, UIAllyDuelScoreGachaSunday.lua:291).
// The other button sends buttonPara, hard-coded 3 (AllyDuelScoreGachaInfoTemplate.lua:27), and only
// when the local al_duel_infinity_box_season table has the configId's row (GetConfigInfo,
// AllyDuelScoreGachaManager.lua:25-37), which this client does not carry.
//
// The draw reply carries score, protectNum and rewardArr[] {pos, reward[]}; the client keeps the
// new score and pity counter and adds the rewards, and touches no other balance
// (OnGachaMessageCallback, :182-205; ShowGachaResult :148-162; RewardManager AddRewards adds only).
// So each reply's score is the balance the next draw is checked against. A run draws at most
// floor(info score / cost) times and duelGachaDrawMaxPerRun, stops on the first errorCode (no
// benign codes) or failed send, and stops (Warn) when a reply has no score or a balance that did not
// fall. It never retries and never claims the pity reward: duel-gacha does that, on the next run
// once the draws filled the counter (the surplus carries over: the wish red dot counts
// floor(protectNum / num), :282-296).
//
// No duel score: the duel themes 110000-110005 (heroevent 40413, score and condition_score) score
// types 1, 4, 6, 7, 20, 23, 42, 51, 79, 82, 83, 86, 87, 95-99, 108, 118, 119, 120, 126, 156 and 157.
// None is a gacha draw (42 is ScoreType.Recruit, 20 ScoreType.Shop, EnumType.lua:15799-15809), and
// the item-valued rows (118, 119) name none of the gacha's prize items (al_duel_infinity_box_reward
// group 1001), so it is not duel-held. Static-only, not validated live.
const (
	duelGachaDrawFeature = "duel-gacha-draw"
	duelGachaDrawCmd     = "al.duel.lottery.lottery"
	duelGachaDrawNum     = 1 // the one-draw button
	// duelGachaDrawMaxPerRun bounds one run's draws (at the pace below, under a minute); a larger
	// balance waits for the next run.
	duelGachaDrawMaxPerRun = 50
)

// duelGachaDrawPace is the gap between draws, a human tap rate.
var duelGachaDrawPace = time.Second

// duelGachaDrawSleep is time.Sleep; tests replace it.
var duelGachaDrawSleep = time.Sleep

func init() {
	registerFeature(Feature{
		Name: duelGachaDrawFeature,
		Summary: "OPT-IN: draw the Alliance Duel score gacha one at a time (al.duel.lottery.lottery, num 1) while its own " +
			"score balance pays 10,000,000 a draw; spends only that score, never diamonds or items (static-only, not validated live)",
		DefaultOn: false,
		Run:       runDuelGachaDraw,
	})
}

// duelGachaDrawDue says why the client's draw button would not send, or "" when it would.
// hasScore is whether the info reply carried a numeric score.
func duelGachaDrawDue(in *Init, st duelGachaState, hasScore bool) string {
	switch {
	case !st.hasConfig || st.configID <= 0 || st.configID > math.MaxInt32:
		return "the reply carries no usable configId (the gacha is not unlocked server-side)"
	case evInitEffect(in, duelGachaUnlock) <= 0:
		return "init effect 92019 (ALCOMPETE_SCORE_GACHA_UNLOCK) is not above 0"
	case !hasScore:
		return "the reply carries no score"
	case st.score < duelGachaDrawNum*duelGachaDrawCost:
		return fmt.Sprintf("the score %d pays for no draw (%d a draw)", st.score, int64(duelGachaDrawCost))
	}
	return ""
}

// duelGachaDrawStop says why no draw may follow a draw reply, or "" when the reply's score (after)
// is the balance the next draw is checked against. before is the balance the draw was sent on.
func duelGachaDrawStop(before, after int64, hasAfter bool) string {
	switch {
	case !hasAfter:
		return "the draw reply carries no score, so the balance is unknown"
	case after >= before:
		return fmt.Sprintf("the balance did not fall after a draw (%d -> %d)", before, after)
	}
	return ""
}

func runDuelGachaDraw(conn *session.GameConn, in *Init) error {
	const name = duelGachaDrawFeature
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
	_, hasScore := claimInt(info.Params, "score")
	slog.Info(name+": gacha state", "configId", st.configID, "hasConfigId", st.hasConfig, "score", st.score,
		"hasScore", hasScore, "drawCost", int64(duelGachaDrawCost), "pity", st.pity, "pityCounter", st.pityCounter,
		"unlockEffect", evInitEffect(in, duelGachaUnlock))
	if why := duelGachaDrawDue(in, st, hasScore); why != "" {
		slog.Info(name + ": no draw: " + why)
		return nil
	}
	planned := min(st.score/(duelGachaDrawNum*duelGachaDrawCost), duelGachaDrawMaxPerRun)
	if st.score/(duelGachaDrawNum*duelGachaDrawCost) > duelGachaDrawMaxPerRun {
		slog.Info(name+": the score pays for more draws than one run makes; the rest wait for the next run",
			"draws", st.score/(duelGachaDrawNum*duelGachaDrawCost), "cap", duelGachaDrawMaxPerRun)
	}

	score, pityCounter := st.score, st.pityCounter
	var done int64
	for done < planned && score >= duelGachaDrawNum*duelGachaDrawCost {
		if done > 0 {
			duelGachaDrawSleep(duelGachaDrawPace)
		}
		p := sfs.NewSFSObject()
		p.PutInt("configId", int32(st.configID))
		p.PutInt("num", duelGachaDrawNum)
		msg, err := session.SendAndWait(conn, name+": draw", duelGachaDrawCmd, p)
		if err != nil {
			return fmt.Errorf("%s: stopped after %d of %d draws: %w", name, done, planned, err)
		}
		done++
		after, hasAfter := claimInt(msg.Params, "score")
		counter, hasCounter := claimInt(msg.Params, "protectNum")
		if done == 1 {
			slog.Info(name+": first "+duelGachaDrawCmd+" reply", "configId", st.configID, "num", duelGachaDrawNum,
				"scoreBefore", score, "scoreAfter", after, "hasScore", hasAfter, "spent", score-after,
				"expectedCost", int64(duelGachaDrawNum*duelGachaDrawCost), "protectNum", counter, "hasProtectNum", hasCounter,
				"rewardArr", len(evObjects(msg.Params, "rewardArr")), "replyKeys", msg.Params.Keys())
		}
		if hasCounter {
			pityCounter = counter
		}
		if why := duelGachaDrawStop(score, after, hasAfter); why != "" {
			slog.Warn(name+": "+why+"; no further draw this run", "draws", done)
			return nil
		}
		score = after
	}
	slog.Info(name+": draws done", "draws", done, "scoreBefore", st.score, "scoreAfter", score,
		"pity", st.pity, "pityCounter", pityCounter)
	if st.pity > 0 && pityCounter >= st.pity {
		slog.Info(name+": the pity counter is full; duel-gacha claims the wish reward on its next run when enabled",
			"pity", st.pity, "pityCounter", pityCounter)
	}
	return nil
}
