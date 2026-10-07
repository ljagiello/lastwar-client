package game

import (
	"errors"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Railway truck cargo (MASTER.md §4 #9), claim half only: the cargo of own trucks that have
// arrived, and the station's one-time opening reward. Departures (`train.send`) need a squad and
// risk robbery, and `train.recover.reward` is paid (§6); neither is sent.
//
// Init trainData {openReward, openTime (ms), myTrainList[] {sendTime, arriveTime (ms), ...}, ...}
// (LWMyStationDataManager.lua:165-172, 511-518; the client renames sendTime/arriveTime to
// departureTs/arriveTs, TrainData.lua:83-84). A truck is in TruckStationState.Reward when
// sendTime > 0 && now >= arriveTime (:680-691), and then the client claims every arrived truck
// with the parameterless `train.batch.reward` (RailwayUtil.lua:66-79;
// CollectTrainBatchRewardMessage.lua:4-6). The opening reward, the parameterless
// `train.first.open.reward` (CollectStationFirstRewardMessage.lua:4-6), is claimable while
// openReward == 0 once now > openTime (:650-675). The client also requires the station area to be
// out of the city fog (Monopoly progress, :719-722), which init alone does not show; on an
// account still fogged in the server is expected to refuse it. Static-only: not sent live yet.
const (
	railwayBatchCmd     = "train.batch.reward"
	railwayFirstOpenCmd = "train.first.open.reward"
)

func init() {
	registerFeature(Feature{
		Name:    "railway-cargo",
		Summary: "claim arrived railway truck cargo and the station opening reward; never sends a truck",
		Run:     runRailwayCargo,
	})
	session.RegisterBenignErrorCode("120289", railwayBatchCmd, railwayFirstOpenCmd)
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
	now := claimNow(in).UnixMilli()
	var errs []error

	if open, ok := claimInt(train, "openReward"); ok && open == 0 {
		if at, _ := claimInt(train, "openTime"); at > 0 && now > at {
			_, err := claimAndLog(conn, "railway opening reward", railwayFirstOpenCmd, sfs.NewSFSObject())
			if session.ContainsNonTimeoutNetError(err) {
				return err
			}
			if err != nil {
				errs = append(errs, err)
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
