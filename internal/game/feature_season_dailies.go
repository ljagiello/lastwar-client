package game

import (
	"fmt"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season dailies (MASTER.md §4 #18), static-only, sent only while the account is in a season.
//
//   - Power milestones: lw.season.user.max.force.reward.info {} -> maxForceValue, list[{score,
//     receive}]; lw.season.user.max.force.reward.get.all {} when a milestone at or below
//     maxForceValue is unclaimed (SeasonRewardDataManager.lua:139-170, 296-322, 435-450).
//   - S6 military daily: military.act.info {} -> dailyRewardNum, militaryLevel, isDailySettleIng;
//     military.get.daily.reward {viewLevel: Int max(1, militaryLevel)} while dailyRewardNum < 1,
//     the SeasonMilitary activity (389) runs and no daily settlement is in progress
//     (SeasonMilitaryManager.lua:61-69, SeasonMilitaryInfoData.lua:58-67,
//     UILWSeasonMilitaryDailyRewardComp.lua:84-91).
//   - Pre-season tasks: view.season.pre.task {activityId: UtfString} -> seasonPreTaskArr[]; a task
//     in state 1 is claimed with season.pre.task.get.reward {activityId: UtfString, taskId:
//     UtfString} (SeasonPreviewManager.lua:82-96, 139-148). These run in the season-preview activity
//     (248) before a season starts, so they are gated on that activity, not on the season.
//
// Not sent: lw.season.week.card.free.reward (#3, another feature); receive.season.week.buff.reward
// and season.force.get.all.reward (no sender in 1.0.364); get.chicken.daily.reward (an S5 tap-the-
// chicken claim the client paces by an animation, up to 50 a day: needs a live check of server
// pacing); season.tower.reward / season.tower.box.award (their gates need season_tower tables;
// listed for a follow-up).
const (
	seasonMaxForceInfoCmd   = "lw.season.user.max.force.reward.info"
	seasonMaxForceClaimCmd  = "lw.season.user.max.force.reward.get.all"
	seasonMilitaryInfoCmd   = "military.act.info"
	seasonMilitaryDailyCmd  = "military.get.daily.reward"
	seasonPreTaskInfoCmd    = "view.season.pre.task"
	seasonPreTaskRewardCmd  = "season.pre.task.get.reward"
	seasonMilitarySeasonID  = 6
	seasonPreTaskCanReceive = 1
)

func init() {
	registerFeature(Feature{
		Name:    "season-dailies",
		Summary: "in season: power-milestone claim-all, S6 military daily; pre-season task rewards; static-only",
		Run:     runSeasonDailies,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, seasonMaxForceClaimCmd, seasonMilitaryDailyCmd, seasonPreTaskRewardCmd)
}

// evSeasonID is the season the account is playing, or 0 outside one. It is SeasonUtil.IsInSeason(true)
// without the lw_season table checks (SeasonUtil.lua:62-84; SeasonInfoTemplate.lua:13-63, 267-281):
// init playerServerSeasonInfo (the source server's season) is open with now before its
// seasonEndTime (ms), and curServerSeasonInfo (the login server's) is open with the same end.
func evSeasonID(in *Init) int64 {
	p, s := in.Object("playerServerSeasonInfo"), in.Object("curServerSeasonInfo")
	if p == nil || s == nil {
		return 0
	}
	id, _ := evNum(p, "seasonId")
	pEnd, ok1 := evNum(p, "seasonEndTime")
	sEnd, ok2 := evNum(s, "seasonEndTime")
	if id <= 0 || !ok1 || !ok2 || pEnd != sEnd || evNow().UnixMilli() >= pEnd || !evBool(p, "open") || !evBool(s, "open") {
		return 0
	}
	return id
}

// evBool reads a Bool, or a 0/1 number, as the Lua's truthiness of `== true`/`== 1`.
func evBool(o *sfs.SFSObject, key string) bool {
	v, ok := o.Get(key)
	if !ok {
		return false
	}
	if b, ok := v.Val.(bool); ok {
		return b
	}
	n, ok := evNum(o, key)
	return ok && n == 1
}

func runSeasonDailies(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	b := &evBatch{conn: conn}
	s := Activities(conn, in)
	if season := evSeasonID(in); season > 0 {
		if msg := b.send("season power milestones", seasonMaxForceInfoCmd, sfs.NewSFSObject()); msg != nil {
			maxForce, _ := evNum(msg.Params, "maxForceValue")
			for _, m := range evObjects(msg.Params, "list") {
				score, ok := evNum(m, "score")
				if ok && score <= maxForce && !evBool(m, "receive") {
					b.send("season power milestone rewards", seasonMaxForceClaimCmd, sfs.NewSFSObject())
					break
				}
			}
		}
		if season == seasonMilitarySeasonID && len(s.Open(actTypeSeasonMilitary)) > 0 && !b.dead {
			claimSeasonMilitary(b)
		}
	}
	for _, a := range s.Open(actTypeSeasonPreview) {
		if b.dead {
			break
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("activityId", a.IDString())
		info, err := s.Info("season pre-task info response", seasonPreTaskInfoCmd, a, p)
		if err != nil {
			b.add(err)
			continue
		}
		for _, t := range evObjects(info, "seasonPreTaskArr") {
			if st, _ := evNum(t, "state"); st != seasonPreTaskCanReceive || b.dead {
				continue
			}
			taskID := evString(t, "taskId")
			if taskID == "" {
				continue
			}
			p := sfs.NewSFSObject()
			p.PutUtfString("activityId", a.IDString())
			p.PutUtfString("taskId", taskID)
			b.send(fmt.Sprintf("season pre-task %s", taskID), seasonPreTaskRewardCmd, p)
		}
	}
	return b.err()
}

func claimSeasonMilitary(b *evBatch) {
	msg := b.send("season military info", seasonMilitaryInfoCmd, sfs.NewSFSObject())
	if msg == nil {
		return
	}
	n, ok := evNum(msg.Params, "dailyRewardNum")
	if !ok || n >= 1 || evBool(msg.Params, "isDailySettleIng") {
		return
	}
	level, _ := evNum(msg.Params, "militaryLevel")
	p := sfs.NewSFSObject()
	p.PutInt("viewLevel", int32(max(1, level)))
	b.send("season military daily", seasonMilitaryDailyCmd, p)
}
