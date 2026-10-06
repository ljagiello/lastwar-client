package sfs

// Request keys put by the claim features in internal/game/feature_*.go (cf-features-daily). All
// are gameplay ids, flags or counters for claim commands; none is a credential or PII.
func init() {
	for _, k := range []string{
		"configId",    // alliance.salary.gain.reward: salary config id (101 daily, 2xx/301 weekly)
		"isAutoClaim", // free.reward.receive: tip-text flag, echoed back
		"isLogin",     // get.week.card.info: login-refresh flag
		"isSeason",    // get.alliance.task.info: season task list flag
		"openWnd",     // get.detect.info: whether the radar window is open
		"rewardLevel", // detect.event.claim.level.reward: radar level whose reward is claimed
		"stage",       // daily.quest.reward: chest index, -1 for every reached chest
		"taskId",      // daily.task.reward / receive.alliance.task.reward: quest or task id
		"uuidArr",     // gather.collect.reward: Collect Rewards entry uuids
		"uuidList",    // hero.dispatch.batch.reward: dispatch task uuids (LongArray via PutValue)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
