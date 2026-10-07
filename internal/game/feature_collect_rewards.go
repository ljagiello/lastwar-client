package game

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Collect Rewards panel (MASTER.md §4 #1): loot from gathering marches, monster kills, rallies,
// plunder and supply drops waits in init collect_reward[] {uuid, type, expireTime (ms), reward[],
// ...} until claimed, and each entry expires (CollectRewardData.lua:15-52).
//
// The 1.0.364 client lists every entry with expireTime > now (CollectRewardDataManager.lua:105,
// :119-122) and claims them all in one `gather.collect.reward {uuidArr: SFSArray of Long}`
// (UICollectRewardView.lua:270-300; GatherCollectRewardMessage.lua:5-9). Its storage check is
// dead code in this build: ResourceItemDataManager:CheckIsStorageFull always returns false
// (ResourceItemDataManager.lua:232-234), so the client never splits the claim for storage, and
// neither does this. Static-only: not sent live yet.
const (
	collectRewardsCmd = "gather.collect.reward"

	// collectRewardsExpiryMargin skips entries about to expire, so a claim never races the
	// expiry (whether one expired uuid fails the whole claim is open, MASTER.md §8.2 #4).
	collectRewardsExpiryMargin = 10 * time.Second
	// collectRewardsBatchMax is a defensive cap per request, not a protocol limit: the client
	// sends every entry at once, and a real panel holds far fewer.
	collectRewardsBatchMax = 200
)

func init() {
	registerFeature(Feature{
		Name:    "collect-rewards",
		Summary: "claim the Collect Rewards panel (world loot that expires) before it expires",
		Run:     runCollectRewards,
	})
	session.RegisterBenignErrorCode("120289", collectRewardsCmd)
}

// collectRewardsDue returns the uuids of init collect_reward entries that have not expired.
func collectRewardsDue(in *Init, now time.Time) []int64 {
	cutoff := now.Add(collectRewardsExpiryMargin).UnixMilli()
	var out []int64
	for _, e := range in.Objects("collect_reward") {
		uuid, ok := claimInt(e, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		if exp, ok := claimInt(e, "expireTime"); !ok || exp <= cutoff {
			continue
		}
		out = append(out, uuid)
	}
	return out
}

func runCollectRewards(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("collect-rewards: no init push; skipping")
		return nil
	}
	due := collectRewardsDue(in, claimNow(in))
	if len(due) == 0 {
		slog.Info("collect-rewards: nothing waiting", "entries", len(in.Objects("collect_reward")))
		return nil
	}
	var batches [][]int64
	for start := 0; start < len(due); start += collectRewardsBatchMax {
		batches = append(batches, due[start:min(start+collectRewardsBatchMax, len(due))])
	}
	var errs []error
	for i, batch := range batches {
		params := sfs.NewSFSObject()
		params.PutSFSArray("uuidArr", claimLongSFSArray(batch))
		label := fmt.Sprintf("collect rewards (%d entries, batch %d/%d)", len(batch), i+1, len(batches))
		msg, err := claimAndLog(conn, label, collectRewardsCmd, params)
		if err != nil {
			// Stop at the first failure: the remaining batches would likely fail the same way.
			errs = append(errs, err)
			break
		}
		if msg != nil {
			slog.Info("collect-rewards: entries left after claim", "remaining", len(claimObjects(msg.Params, "collect_reward")))
		}
	}
	return errors.Join(errs...)
}
