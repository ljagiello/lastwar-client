package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// S0 alliance boss first-kill rewards (MASTER G39, aud-alliance/F6), opt-in and static-only. On
// beginner (Season 0) servers the alliance boss activity (EnumActivity.S0AllianceBoss, type 269,
// EnumType.lua:6519; table activity row 94300) pays a one-time reward per difficulty level the
// alliance first beat. For a listed activity that CheckIsSend accepts, the client sends
// `alliance.boss.s0.act.info {type: Int 1}` (ActivityListDataManager.lua:896-900, 2046-2048;
// S0AllianceBossDataManager.lua:284-287; AllianceBossS0ActInfoMessage.lua:4-7), whose reply's
// firstRewardList[{difficultyLevel, state}] (ParseFirstRewardList, S0AllianceBossDataManager.lua:
// 218-232) drives the reward bubble: state 0 not earned, 1 claimable, 2 claimed
// (UIS0AllianceBossMainView.lua:513-548, 686-694). A claimable level is claimed with
// `receive.alliance.boss.s0.first.reward {difficultyLevel: Int}`
// (ReceiveAllianceBossS0FirstRewardMessage.lua:4-7), free and visible to no one. The boss is an
// alliance activity, so nothing is sent outside an alliance. The info reply is logged at Info;
// the first errorCode stops the run.
const (
	s0BossFeature          = "s0-alliance-boss"
	s0BossActivityType     = 269
	s0BossInfoCmd          = "alliance.boss.s0.act.info"
	s0BossFirstRewardCmd   = "receive.alliance.boss.s0.first.reward"
	s0BossInfoMain         = 1 // ReqActMainMessage's default type
	s0BossFirstRewardReady = 1
)

func init() {
	registerFeature(Feature{
		Name: s0BossFeature,
		Summary: "OPT-IN: on Season 0 servers, claim the S0 alliance boss first-kill reward of each difficulty the alliance " +
			"beat (receive.alliance.boss.s0.first.reward) (static-only, not validated live)",
		Run: runS0AllianceBoss,
	})
}

func runS0AllianceBoss(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if len(Activities(conn, in).Open(s0BossActivityType)) == 0 {
		slog.Info(s0BossFeature + ": no S0 alliance boss activity open; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info(s0BossFeature + ": not in an alliance; skipping")
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutInt("type", s0BossInfoMain)
	// fwRequest, not SendAndWait: the reply's donateList and boss data name other players.
	info, code, err := fwRequest(conn, s0BossFeature+": info", s0BossInfoCmd, p)
	if err != nil {
		return err
	}
	if code != "" {
		slog.Warn(s0BossFeature+": info refused", "errorCode", code, "errorMsg", evString(info, "errorMsg"))
		return fmt.Errorf("%s: %s: errorCode=%s", s0BossFeature, s0BossInfoCmd, code)
	}
	var due []int32
	list := evObjects(info, "firstRewardList")
	var levels []string
	for _, e := range list {
		lv, ok := evNum(e, "difficultyLevel")
		st, _ := evNum(e, "state")
		levels = append(levels, fmt.Sprintf("%d:%d", lv, st))
		if ok && lv >= 0 && lv <= math.MaxInt32 && st == s0BossFirstRewardReady {
			due = append(due, int32(lv))
		}
	}
	slog.Info(s0BossFeature+": first-kill rewards", "levelStates", levels, "claimable", due, "hasFirstRewardList", info.Has("firstRewardList"),
		"actEndTime", evString(info, "actEndTime"), "isSign", evString(info, "isSign"), "replyKeys", info.Keys())
	var errs []error
	for _, lv := range due {
		q := sfs.NewSFSObject()
		q.PutInt("difficultyLevel", lv)
		r, code, err := fwRequest(conn, s0BossFeature+": first-kill reward", s0BossFirstRewardCmd, q)
		if err != nil {
			errs = append(errs, err)
			break
		}
		if code != "" {
			slog.Warn(s0BossFeature+": claim refused; stopping this run", "difficultyLevel", lv, "errorCode", code, "errorMsg", evString(r, "errorMsg"))
			errs = append(errs, fmt.Errorf("%s: %s difficultyLevel %d: errorCode=%s", s0BossFeature, s0BossFirstRewardCmd, lv, code))
			break
		}
		slog.Info(s0BossFeature+": claimed", "difficultyLevel", lv, "reward", fwReward(r), "replyKeys", r.Keys())
	}
	return errors.Join(errs...)
}
