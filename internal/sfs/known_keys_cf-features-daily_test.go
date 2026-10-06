package sfs

// Request keys put by the claim features in internal/game/feature_*.go (cf-features-daily). All
// are gameplay ids, flags or counters for claim commands; none is a credential or PII.
func init() {
	for _, k := range []string{
		"aiPushStatus", // lottery.hero.card: the client's AI-chat push switches (ai_switch ids -> bool)
		"configId",     // alliance.salary.gain.reward: salary config id (101 daily, 2xx/301 weekly)
		"id",           // lottery.hero.card: recruit pool id
		"isAutoClaim",  // free.reward.receive: tip-text flag, echoed back
		"isLogin",      // get.week.card.info: login-refresh flag
		"isSeason",     // get.alliance.task.info: season task list flag
		"isTen",        // lottery.hero.card / lottery.worker.card: pull size, always 0 (one pull)
		"itemId",       // lottery.hero.card: the pool's cost item id; al.call.help: the queue's item id
		"officerId",    // worker.lottery.info / lottery.worker.card: survivor recruit pool id
		"openWnd",      // get.detect.info: whether the radar window is open
		"qType",        // al.call.help: queue type (6 research, 3 hospital, 117 rebirth hospital)
		"rewardLevel",  // detect.event.claim.level.reward: radar level whose reward is claimed
		"stage",        // daily.quest.reward: chest index, -1 for every reached chest
		"taskId",       // daily.task.reward / receive.alliance.task.reward: quest or task id
		"useFree",      // lottery.hero.card / lottery.worker.card: free-pull flag, always 1
		"uuidArr",      // gather.collect.reward: Collect Rewards entry uuids
		"uuidList",     // hero.dispatch.batch.reward: dispatch task uuids (LongArray via PutValue)
	} {
		knownNonSensitiveSFSKeys[k] = true
	}
}
