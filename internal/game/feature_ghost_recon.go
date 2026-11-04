package game

import (
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Ghost Recon (team Secret Tasks) finished-task rewards (gap GHOST, MASTER.md §3.2 #4), OPT-IN,
// claim half only.
//
// When a Ghostrecon activity (type 308, rows 94110/94111 with needMainCityLevel 18) is added, the
// client sends ghost.recon.get.task.list {} (ActivityListDataManager.lua:624-631); s.Open applies
// the window and the table's HQ level, the gate the world point checks too (UIUtil.lua:1781-1784).
// The reply is only used when it carries openTime; it holds taskList[{uuid, ownerId, allianceId,
// teamStartTime, completionTime, memberList[{uid, rewarded}], ...}] (GhostreconGetTaskListHandler,
// ActGhostreconManager.lua:68-92; ActGhostreconTaskInfo:ParseData, ActGhostreconTaskInfo.lua:59-109).
// RefreshBubbleType (:111-144) puts a task in Own_WaitClaim or Alliance_WaitClaim when
// completionTime > 0, now > completionTime (ms) and the player's own memberList entry has
// rewarded ~= 1 (OwnIsRewarded, :179-188, is true without an own entry, so only own or joined
// tasks qualify); an own task also needs teamStartTime set. Tapping a waiting task sends every
// waiting uuid in one ghost.recon.batch.reward {uuidList: LongArray, ownerServer: Int}, with
// ownerServer = PlayerInfo:GetSourceServerId (ActGhostreconManager.lua:525-534;
// GhostReconBatchRewardMessage.lua:4-8). The batch list comes only from this task list, which
// already holds joined alliance tasks, so ghost.recon.get.alliance.task.list is not sent.
//
// Never sent: steal, join, start, kick, quit, disband, remind or autostart. Tasks finish only after
// the player (or the server's autoStart) started or joined them. Static-only, not validated live.
const (
	ghostReconFeature     = "ghost-recon"
	ghostReconTaskListCmd = "ghost.recon.get.task.list"
	ghostReconBatchCmd    = "ghost.recon.batch.reward"
)

func init() {
	registerFeature(Feature{
		Name:      ghostReconFeature,
		Summary:   "OPT-IN: claim finished Ghost Recon (team Secret Task) rewards in one batch; never starts, joins or steals (static-only, not validated live)",
		DefaultOn: false,
		Run:       runGhostRecon,
	})
}

func runGhostRecon(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	open := Activities(conn, in).Open(actTypeGhostRecon)
	if len(open) == 0 {
		return nil
	}
	ids := make([]int32, len(open))
	for i, a := range open {
		ids[i] = a.ID
	}
	// Only uid, serverId and crossFightSrcServerId of init user are read; user holds PII.
	uid := evString(in.Object("user"), "uid")
	server, ok := ghostReconSourceServer(in)
	if uid == "" || !ok {
		slog.Info(ghostReconFeature+": own uid or source server unknown; claiming nothing", "activities", ids,
			"haveUid", uid != "", "sourceServer", server)
		return nil
	}
	msg, err := session.SendAndWait(conn, ghostReconFeature+": task list", ghostReconTaskListCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	if !msg.Params.Has("openTime") {
		slog.Info(ghostReconFeature+": task list without openTime; the client ignores it", "activities", ids)
		return nil
	}
	now := evNow().UnixMilli()
	tasks := evObjects(msg.Params, "taskList")
	var due []int64
	for _, t := range tasks {
		if !ghostReconWaitClaim(t, uid, now) {
			continue
		}
		if id, ok := evNum(t, "uuid"); ok {
			due = append(due, id)
		}
	}
	openTime, _ := evNum(msg.Params, "openTime")
	slog.Info(ghostReconFeature+": task list", "activities", ids, "openTime", openTime, "tasks", len(tasks),
		"waitingClaim", len(due), "ownerServer", server)
	if len(due) == 0 {
		return nil
	}
	p := sfs.NewSFSObject()
	p.PutValue("uuidList", claimLongArray(due))
	p.PutInt("ownerServer", int32(server))
	r, err := session.SendAndWait(conn, ghostReconFeature+": batch reward", ghostReconBatchCmd, p)
	if err != nil {
		return err
	}
	slog.Info(ghostReconFeature+": rewards claimed", "uuids", due, "memberList", len(evObjects(r.Params, "memberList")),
		"granted", describeGrants(r.Params))
	return nil
}

// ghostReconWaitClaim is RefreshBubbleType's Own_WaitClaim / Alliance_WaitClaim for task t and
// the player uid at now (ms).
func ghostReconWaitClaim(t *sfs.SFSObject, uid string, now int64) bool {
	done, _ := evNum(t, "completionTime")
	if done <= 0 || now <= done {
		return false
	}
	if evString(t, "ownerId") == uid {
		if start, _ := evNum(t, "teamStartTime"); start == 0 {
			return false // Own_NotExecuted
		}
	}
	for _, m := range evObjects(t, "memberList") {
		if evString(m, "uid") == uid {
			r, _ := evNum(m, "rewarded")
			return r != 1
		}
	}
	return false // no own entry: OwnIsRewarded is true
}

// ghostReconSourceServer is PlayerInfo:GetSourceServerId (PlayerInfo.lua:794-796): init
// user.crossFightSrcServerId when it is >= 0, else user.serverId. Unlike railwayHomeServers, a 0
// crossFightSrcServerId is taken as the client takes it, and ok is false unless the result is a
// positive server id, so nothing is claimed for server 0.
func ghostReconSourceServer(in *Init) (int64, bool) {
	user := in.Object("user")
	id, ok := evNum(user, "crossFightSrcServerId")
	if !ok || id < 0 {
		id, ok = evNum(user, "serverId")
	}
	return id, ok && id > 0
}
