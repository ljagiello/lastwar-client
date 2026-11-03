package game

import (
	"errors"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Railway truck cargo (MASTER.md §4 #9), claim half only: the cargo of own trucks that have
// arrived, and the station's one-time opening reward. Departures (`train.send`) are the opt-in
// truck-deploy feature's, and plundering is the opt-in railway-plunder feature's.
// `train.recover.reward` costs whatever `reward.recover.param` b_k5 names (§6) and isn't sent.
//
// Init trainData {openReward, openTime (ms), myTrainList[] {sendTime, arriveTime (ms), ...}, ...}
// (LWMyStationDataManager.lua:165-172, 511-518; the client renames sendTime/arriveTime to
// departureTs/arriveTs, TrainData.lua:83-84). A truck is in TruckStationState.Reward when
// sendTime > 0 && now >= arriveTime (:680-691), and then the client claims every arrived truck
// with the parameterless `train.batch.reward` (RailwayUtil.lua:66-79;
// CollectTrainBatchRewardMessage.lua:4-6). The opening reward, the parameterless
// `train.first.open.reward` (CollectStationFirstRewardMessage.lua:4-6), is claimable while
// openReward == 0 once now > openTime (:650-675), and only while the station is out of the city
// fog (see railwayOutOfFog); the cargo claim has no fog gate. Neither claim is sent while the city
// is in another warzone (see railwayAtHome). Static-only: not sent live yet.
//
// Anything that lists or targets other players' trucks or trains (railway-plunder) must first drop
// every one on the account's own server: the client only checks a target's serverId against the
// server-sent trainServers (LWMyStationDataManager:CheckTrainInMatchServer), and the own server
// can be in that list.
const (
	railwayBatchCmd     = "train.batch.reward"
	railwayFirstOpenCmd = "train.first.open.reward"
	// railwayCrossSwitch is the DataConfig switch that lets both claims through from another warzone.
	railwayCrossSwitch = "train_cross_server"
	// railwayFogStage is train_para row 16 in the 1.0.364 table (GET_TRAIN_FOG_ID): the station
	// stays fogged while the Monopoly curId is below it.
	railwayFogStage = 59
)

func init() {
	registerFeature(Feature{
		Name:    "railway-cargo",
		Summary: "claim arrived railway truck cargo and the station opening reward; never sends a truck",
		Run:     runRailwayCargo,
	})
	// 120289 is the generic "already executed this command" code. It is confirmed live for the
	// VIP claims only; for these two it is an assumption. The handlers just show whatever code
	// comes back (CollectTrainBatchRewardMessage, CollectStationFirstRewardMessage).
	session.RegisterBenignErrorCode("120289", railwayBatchCmd, railwayFirstOpenCmd)
}

// railwayAtHome mirrors the gate in front of both claims (RailwayUtil.ApplyArriveReward,
// LWMyStationDataManager:TryCollectFirstReward). Unless DataConfig train_cross_server is on, the
// client refuses with tip 500020 ("You're engaged in a raid in another Warzone") when
// PlayerInfo:IsLoginSourceServer is false: init user.crossFightSrcServerId is set (>= 0) and
// differs from user.serverId. A client that never browses another server gets the same answer
// from CrossServerUtil:NeedIntercept. Only those two fields of user are read; user holds PII.
func railwayAtHome(in *Init) bool {
	if claimSwitchOn(in, railwayCrossSwitch) {
		return true
	}
	user := in.Object("user")
	src, ok := claimInt(user, "crossFightSrcServerId")
	if !ok || src < 0 {
		return true
	}
	srv, ok := claimInt(user, "serverId")
	return ok && srv == src
}

// railwayOutOfFog mirrors LWMyStationDataManager:IsTruckFunctionLock. The opening reward is only
// offered in RailwayStationState.FirstReward, which GetRailwayStationState returns only when the
// station is not fogged. The fog lifts once the Monopoly curId reaches railwayFogStage. The client
// sets curId from init monopolyStageInfo.stageId via MonopolyManager_getNextId (stageId + 1), and
// leaves it 0 when init has none, so a missing stage counts as fogged. The civilization-spark B
// guide's wrap back to 1 at the end of its V0 interval is not modelled.
func railwayOutOfFog(in *Init) bool {
	stage, ok := claimInt(in.Object("monopolyStageInfo"), "stageId")
	return ok && stage+1 >= railwayFogStage
}

func runRailwayCargo(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("railway-cargo: no init push; skipping")
		return nil
	}
	train := in.Object("trainData")
	if train == nil {
		slog.Info("railway-cargo: init has no trainData; skipping")
		return nil
	}
	if !railwayAtHome(in) {
		slog.Info("railway-cargo: the city is in another warzone and " + railwayCrossSwitch + " is off; skipping, as the client does")
		return nil
	}
	now := claimNow(in).UnixMilli()
	var errs []error

	// The client's activityStart is `openTime and now > openTime` (GetRailwayStationState), so an
	// openTime of 0 counts as open.
	if open, ok := claimInt(train, "openReward"); ok && open == 0 {
		if at, ok := claimInt(train, "openTime"); ok && now > at {
			if !railwayOutOfFog(in) {
				slog.Info("railway-cargo: the station is still in the city fog; opening reward held")
			} else {
				_, err := claimAndLog(conn, "railway opening reward", railwayFirstOpenCmd, sfs.NewSFSObject())
				if session.ContainsNonTimeoutNetError(err) {
					return err
				}
				if err != nil {
					errs = append(errs, err)
				}
			}
		}
	}

	arrived := 0
	for _, t := range claimObjects(train, "myTrainList") {
		sent, _ := claimInt(t, "sendTime")
		arrive, ok := claimInt(t, "arriveTime")
		if sent > 0 && ok && now >= arrive {
			arrived++
		}
	}
	if arrived == 0 {
		slog.Info("railway-cargo: no truck has arrived")
		return errors.Join(errs...)
	}
	label := "railway cargo"
	msg, err := session.SendAndWait(conn, label, railwayBatchCmd, sfs.NewSFSObject())
	if err == nil && msg != nil && !msg.Params.Has("errorCode") {
		// The reply is trainRewardList[] {myTrain, reward}, one entry per truck
		// (CollectTrainBatchRewardMessage.lua:12-50).
		for _, item := range claimObjects(msg.Params, "trainRewardList") {
			slog.Info(label+" granted "+describeGrants(item), "cmd", railwayBatchCmd)
		}
	}
	if err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
