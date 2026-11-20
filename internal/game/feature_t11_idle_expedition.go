package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// T11 idle expedition (MASTER.md §4 #26), static-only: VALIDATE LIVE FIRST. Collect only.
//
// idle.game.main {} returns idleGameIdleInfo{startTime, rewardPool[], passNode, ...}
// (T11IdleGameMainData.lua:26-55; T11IdleGameIdleInfoData.lua:35-74), and
// idle.game.reward.receive {} collects the pool whenever it is non-empty, during or after a run
// (T11IdleGameDataManager.lua:156-173). A changeLevel.idleGameMain.idleGameIdleInfo in the same
// reply replaces the top-level one, as the client applies it second (OnGetIdleGameMainMessage,
// T11IdleGameDataManager.lua:97-112; T11IdleGameMainData.lua:40-43), so the pool is read from it
// when present (t11IdleInfo).
//
// The feature exists only when IsT11IdleGameFunctionOn holds (T11IdleGameManager.lua:37-82): the
// switch t11_idle_game_open is on, and DataConfig idle_game_para.k1 "27;4;64" (table item, tables
// 40413; the bundled row replaces init's, DataConfig.lua:36-46, 52-74) needs a military camp at
// level 27 and season 4 day 64 or later (t11Gate). When an input of that test is missing from init
// the main read is sent anyway, and an errorCode reply counts as "not available".
//
// Not sent: idle.game.end and idle.game.start. End only clears a finished run so the next one can
// start (its button reads "Start", BattleContentComponent.lua:314-331), and whether it forfeits an
// uncollected pool is unknown; start spends one of the daily training attempts. idle.game.reward.update
// is not needed to collect: the client skips it when a run ended while closed
// (T11IdleGameIdleBattleLogic.lua:133-148).
const (
	t11MainCmd    = "idle.game.main"
	t11ReceiveCmd = "idle.game.reward.receive"
	// t11MilitaryCampID is BuildingTypes.LW_BUILD_MILITARY_CAMP (EnumType.lua:3655).
	t11MilitaryCampID int32 = 10103000
	// idle_game_para.k1 = "militaryCampLv;seasonNum;seasonDay" (T11IdleGameManager.lua:37-49).
	t11GateCampLevel = 27
	t11GateSeason    = 4
	t11GateSeasonDay = 64
	t11DayMs         = 86400000
)

func init() {
	registerFeature(Feature{
		Name:    "t11-idle-expedition",
		Summary: "collect the T11 idle expedition's reward pool (never ends or starts a run); static-only, VALIDATE LIVE FIRST",
		Run:     runT11IdleExpedition,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, t11ReceiveCmd)
}

// t11Gate is IsT11IdleGameFunctionOn after the switch (T11IdleGameManager.lua:51-82): the highest
// military camp (GetMaxMilitaryCampBuildingLevel, :51-62) reaches t11GateCampLevel, and the season
// (SeasonUtil.GetSeason: init playerServerSeasonInfo.seasonId) is above t11GateSeason, or equal with
// the season day (now - seasonStartTime) // 1 day + 1 (GetSeasonDayByOpenServerZero, SeasonUtil.lua:
// 324-332; SeasonDataManager.lua:1098-1103) at t11GateSeasonDay or later. known is false when init
// lacks an input (no buildings, no playerServerSeasonInfo, or season 4 without a seasonStartTime).
func t11Gate(in *Init) (open, known bool, why string) {
	if len(in.Buildings) == 0 {
		return false, false, "init has no buildings"
	}
	var camp int32
	for _, b := range in.Buildings {
		if b.BId() == t11MilitaryCampID && b.Level() > camp {
			camp = b.Level()
		}
	}
	if camp < t11GateCampLevel {
		return false, true, fmt.Sprintf("military camp level %d is below %d", camp, t11GateCampLevel)
	}
	p := in.Object("playerServerSeasonInfo")
	if p == nil {
		return false, false, "init has no playerServerSeasonInfo"
	}
	season, _ := evNum(p, "seasonId")
	switch {
	case season > t11GateSeason:
		return true, true, ""
	case season < t11GateSeason:
		return false, true, fmt.Sprintf("season %d is before season %d", season, t11GateSeason)
	}
	start, ok := evNum(p, "seasonStartTime")
	if !ok || start <= 0 {
		return false, false, "season 4 without a seasonStartTime"
	}
	if day := (evNow().UnixMilli()-start)/t11DayMs + 1; day < t11GateSeasonDay {
		return false, true, fmt.Sprintf("season %d day %d is before day %d", season, day, t11GateSeasonDay)
	}
	return true, true, ""
}

// t11Available reports whether a feature should send idle.game.main: the switch is on and t11Gate
// does not rule it out. A gate that can't be evaluated is logged and lets the read through, as
// before the gate existed.
func t11Available(in *Init, feature string) bool {
	if !evSwitchOn(in, "t11_idle_game_open") {
		return false
	}
	open, known, why := t11Gate(in)
	switch {
	case !known:
		slog.Info(feature+": the T11 camp/season gate can't be read from init; asking the server", "reason", why)
		return true
	case !open:
		slog.Info(feature+": the T11 idle expedition is closed for this account (idle_game_para k1)", "reason", why)
		return false
	}
	return true
}

// t11IdleInfo is the idle info the client ends up with after an idle.game.main reply:
// changeLevel.idleGameMain.idleGameIdleInfo when present, else the top-level one.
func t11IdleInfo(p *sfs.SFSObject) *sfs.SFSObject {
	if info := evObject(evObject(evObject(p, "changeLevel"), "idleGameMain"), "idleGameIdleInfo"); info != nil {
		return info
	}
	return evObject(p, "idleGameIdleInfo")
}

func runT11IdleExpedition(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || !t11Available(in, "t11 idle expedition") {
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
	if !evNonEmpty(t11IdleInfo(msg.Params), "rewardPool") {
		return nil
	}
	_, err = session.SendAndWait(conn, "t11 idle expedition rewards", t11ReceiveCmd, sfs.NewSFSObject())
	return err
}
