package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// World-boss kill rewards (MASTER.md G9; cov-dc-world F1, cov-core F7 and their verdicts), OPT-IN,
// a claim only (no march). Static-only, from the 1.0.370 Lua, not sent live yet.
//
// With a BerserkBoss activity (type 307, EnumType.lua:6438) in the activity list the client sends
// user.get.all.berserk.boss.march {} (ActivityListDataManager.lua:638-640; LWBerserkBossManager.lua:
// 57-69), whose marches[] are {uuid, bossId, monsterId, status, curHp, maxHp, ...}
// (LWBerserkBossInfo.lua:35-77). Init actBerserkBoss.array[{uuid, times, hasReward}] holds the
// player's attack count and whether the kill reward was received (InitData, LWBerserkBossManager.lua:
// 45-55, 105-157; LWBerserkBossRewardAndAttackTimesInfo.lua:15-25). Tapping a dead boss (curHp <= 0)
// whose reward is not received sends berserk.boss.reward {uuid: Long} (UIUtil.lua:851-871;
// LWBerserkBossManager.lua:101-103; BerserkBossRewardMessage.lua:4-19, reply reward), and only at home:
// PlayerInfo:AtHomeNow, the login server is the source server (PlayerInfo.lua:828-838).
//
// Gates here: a type-307 entry in init; at home (worldBossAtHome); a march with curHp present and
// <= 0; its array entry with times > 0 (the player hit it: whether a boss the player never hit pays
// is unknown, so this starts on the safe side) and hasReward absent or Bool false (Lua truthiness:
// any other value counts as received). Whether a dead boss stays in marches[] and the "already
// claimed" errorCode are unknown: each claim's reply is logged at Info and an errorCode stops the
// feature (no benign code is registered).
const (
	worldBossKillsFeature       = "world-boss-kill-rewards"
	worldBossActType      int32 = 307 // EnumActivity.BerserkBoss
	worldBossMarchesCmd         = "user.get.all.berserk.boss.march"
	worldBossRewardCmd          = "berserk.boss.reward"
)

func init() {
	registerFeature(Feature{
		Name:    worldBossKillsFeature,
		Summary: "OPT-IN: claim the kill chest of each dead world boss the player hit (no march) (static-only, not validated live)",
		Run:     runWorldBossKills,
	})
}

// worldBossAtHome is PlayerInfo:AtHomeNow (PlayerInfo.lua:828-838): IsLoginSourceServer, init
// user.crossFightSrcServerId absent or < 0, or equal to user.serverId; IsInSourceServer holds for a
// session that never views another server. Only those two fields of user are read (user holds PII).
func worldBossAtHome(in *Init) bool {
	user := in.Object("user")
	src, ok := claimInt(user, "crossFightSrcServerId")
	if !ok || src < 0 {
		return true
	}
	srv, ok := claimInt(user, "serverId")
	return ok && srv == src
}

// worldBossHit is one init actBerserkBoss.array entry.
type worldBossHit struct {
	times    int64
	received bool
}

func worldBossHits(in *Init) map[int64]worldBossHit {
	out := map[int64]worldBossHit{}
	for _, e := range evObjects(in.Object("actBerserkBoss"), "array") {
		uuid, ok := evNum(e, "uuid")
		if !ok {
			continue
		}
		times, _ := evNum(e, "times")
		v, has := e.Get("hasReward")
		out[uuid] = worldBossHit{times: times, received: has && v.Val != nil && v.Val != false}
	}
	return out
}

func runWorldBossKills(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if len(Activities(conn, in).All(worldBossActType)) == 0 {
		return nil
	}
	if !worldBossAtHome(in) {
		slog.Info(worldBossKillsFeature + ": not at home (the client taps a boss only on the source server); skipping")
		return nil
	}
	hits := worldBossHits(in)
	b := &evBatch{conn: conn}
	msg := b.send(worldBossKillsFeature+": bosses", worldBossMarchesCmd, sfs.NewSFSObject())
	if msg == nil {
		return b.err()
	}
	marches := evObjects(msg.Params, "marches")
	slog.Info(worldBossKillsFeature+": bosses", "marches", len(marches), "hitEntries", len(hits))
	for _, m := range marches {
		uuid, ok := evNum(m, "uuid")
		hp, okHp := evNum(m, "curHp")
		if !ok || !okHp || hp > 0 || b.dead {
			continue
		}
		h := hits[uuid]
		bossID, _ := evNum(m, "bossId")
		slog.Info(worldBossKillsFeature+": dead boss", "uuid", uuid, "bossId", bossID, "times", h.times, "received", h.received)
		if h.received || h.times <= 0 {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", uuid)
		if !evOptInClaim(b, worldBossKillsFeature, fmt.Sprintf("kill chest (boss %d)", uuid), worldBossRewardCmd, p) {
			break
		}
	}
	return b.err()
}
