package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// T11 idle expedition (MASTER.md §4 #26), static-only: VALIDATE LIVE FIRST. Collect only.
//
// idle.game.main {} returns idleGameIdleInfo{startTime, rewardPool[], passNode, ...}
// (T11IdleGameMainData.lua:23-52; T11IdleGameIdleInfoData.lua:35-74), and
// idle.game.reward.receive {} collects the pool whenever it is non-empty, during or after a run
// (T11IdleGameDataManager.lua:156-173). The feature exists only with the server-only switch
// t11_idle_game_open on (T11IdleGameManager.lua:29-72); a main reply with an errorCode (the account
// does not meet idle_game_para's camp/season gate) is treated as "not available".
//
// Not sent: idle.game.end and idle.game.start. End only clears a finished run so the next one can
// start (its button reads "Start", BattleContentComponent.lua:314-331), and whether it forfeits an
// uncollected pool is unknown; start spends one of the daily training attempts. idle.game.reward.update
// is not needed to collect: the client skips it when a run ended while closed
// (T11IdleGameIdleBattleLogic.lua:133-148).
const (
	t11MainCmd    = "idle.game.main"
	t11ReceiveCmd = "idle.game.reward.receive"
)

func init() {
	registerFeature(Feature{
		Name:    "t11-idle-expedition",
		Summary: "collect the T11 idle expedition's reward pool (never ends or starts a run); static-only, VALIDATE LIVE FIRST",
		Run:     runT11IdleExpedition,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, t11ReceiveCmd)
}

func runT11IdleExpedition(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !evSwitchOn(in, "t11_idle_game_open") {
		return nil
	}
	msg, err := session.SendAndWait(conn, "t11 idle expedition", t11MainCmd, sfs.NewSFSObject())
	if err != nil {
		if msg != nil {
			slog.Info("t11 idle expedition not available", "errorCode", msg.Params.GetString("errorCode"))
			return nil
		}
		return err
	}
	if !evNonEmpty(evObject(msg.Params, "idleGameIdleInfo"), "rewardPool") {
		return nil
	}
	_, err = session.SendAndWait(conn, "t11 idle expedition rewards", t11ReceiveCmd, sfs.NewSFSObject())
	return err
}
