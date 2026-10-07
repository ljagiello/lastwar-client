package game

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Small world and city claims (MASTER.md §4 #21), static-only. Each is decided from init or one
// read the real client already sends at login.
//
//   - Bloody Night box: claim.whistle.box.reward {} while init whistle_reward > 0
//     (MonsterManager.lua:85-94).
//   - Announcements and compensation: get.notice.list {} -> noticeList[{uuid, rewardStatus,
//     content}]; receice.notice.reward {uuid: Long} (the wire name is misspelled) for a notice whose
//     content JSON has b.reward and rewardStatus 0. A reward with rewardVersion is held back while
//     any component is above this client's version, and needs HQ >= rewardLevel
//     (MailSystem.lua:198-224; WorldNoticeDataInfo.lua:24-110).
//   - Server trends: server.trends.info {} -> trendsInfo[]; server.trends.reward {id: UtfString}
//     for rewardStatus 2 (unreceived) with status 2 or 4 and levelLimit <= HQ
//     (UIWorldTrendItem.lua:204-258; WorldTrendManager.lua:7-20).
//   - Locked monsters: receive.pve.monster.reward {pveMonsterId: Int} for init pveMonsters[] in
//     state 2 (finished) with reward > 0 (MonsterLockData.lua:8-20; MonsterLockDataManager.lua:219-284).
//   - World-boss base compensation: berserk.boss.hit.base.gain.info {} -> rewardCount;
//     berserk.boss.hit.base.gain.reward {} while it is above 0 (LWBerserkBossManager.lua:442-462).
//
// Not sent:
//   - chip.collect for a finished Tactical Chip Factory craft: the finished-timers feature collects
//     that building (building.camp.collect, falling back to chip.collect);
//   - the tower-up/dominator first rewards: their stage list needs table lw_towerup and the
//     server-only truck_first_reward switch, and -1 is unverified;
//   - goods.auto.convert: no caller in 1.0.364, and it consumes items;
//   - receive.building.growval.reward and receive.hero.bounty.task.reward: their buildings have no
//     1.0.364 table rows;
//   - hero.wish.preview.reward: a one-time claim with a season-day window from recruit_wish_config.
const (
	whistleBoxCmd       = "claim.whistle.box.reward"
	noticeListCmd       = "get.notice.list"
	noticeRewardCmd     = "receice.notice.reward"
	trendsInfoCmd       = "server.trends.info"
	trendsRewardCmd     = "server.trends.reward"
	pveMonsterRewardCmd = "receive.pve.monster.reward"
	bossCompInfoCmd     = "berserk.boss.hit.base.gain.info"
	bossCompRewardCmd   = "berserk.boss.hit.base.gain.reward"
	trendUnreceived     = 2
	pveMonsterFinished  = 2 // MonsterLockState.Finished (EnumType.lua:10464-10468)
)

func init() {
	registerFeature(Feature{
		Name:    "world-small-claims",
		Summary: "claim the whistle box, notice compensation, server trends, locked-monster chests, boss base compensation; static-only",
		Run:     runWorldSmallClaims,
	})
	session.RegisterBenignErrorCode(evAlreadyExecuted, whistleBoxCmd, noticeRewardCmd, trendsRewardCmd, pveMonsterRewardCmd,
		bossCompRewardCmd)
}

func runWorldSmallClaims(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	b := &evBatch{conn: conn}
	hq := activityHQLevel(in)
	if n, _ := evNum(in.Raw, "whistle_reward"); n > 0 {
		b.send("whistle box", whistleBoxCmd, sfs.NewSFSObject())
	}
	if !b.dead {
		if msg := b.send("notice list", noticeListCmd, sfs.NewSFSObject()); msg != nil {
			for _, n := range evObjects(msg.Params, "noticeList") {
				uuid, ok := evNum(n, "uuid")
				if !ok || b.dead || !noticeClaimable(n, hq) {
					continue
				}
				p := sfs.NewSFSObject()
				p.PutLong("uuid", uuid)
				b.send("notice reward", noticeRewardCmd, p)
			}
		}
	}
	if !b.dead {
		if msg := b.send("server trends", trendsInfoCmd, sfs.NewSFSObject()); msg != nil {
			for _, tr := range evObjects(msg.Params, "trendsInfo") {
				rs, _ := evNum(tr, "rewardStatus")
				st, _ := evNum(tr, "status")
				lim, _ := evNum(tr, "levelLimit")
				id := evString(tr, "id")
				if rs != trendUnreceived || (st != 2 && st != 4) || lim > int64(hq) || id == "" || b.dead {
					continue
				}
				p := sfs.NewSFSObject()
				p.PutUtfString("id", id)
				b.send(fmt.Sprintf("server trend %s", id), trendsRewardCmd, p)
			}
		}
	}
	for _, m := range in.Objects("pveMonsters") {
		st, _ := evNum(m, "state")
		reward, _ := evNum(m, "reward")
		id, ok := evNum(m, "pveMonsterId")
		if !ok || st != pveMonsterFinished || reward <= 0 || b.dead {
			continue
		}
		p := sfs.NewSFSObject()
		p.PutInt("pveMonsterId", int32(id))
		b.send(fmt.Sprintf("locked monster %d chest", id), pveMonsterRewardCmd, p)
	}
	if !b.dead {
		if msg := b.send("boss base compensation info", bossCompInfoCmd, sfs.NewSFSObject()); msg != nil {
			if n, _ := evNum(msg.Params, "rewardCount"); n > 0 {
				b.send("boss base compensation", bossCompRewardCmd, sfs.NewSFSObject())
			}
		}
	}
	return b.err()
}

// noticeClaimable applies MailSystem:ShowRewardBtn to a notice: content.b.reward must exist (the
// client forces rewardStatus 1 without it) and rewardStatus must be 0; a rewardVersion newer than
// this client in any component holds it back, and with a rewardVersion the HQ must reach
// rewardLevel.
func noticeClaimable(n *sfs.SFSObject, hq int32) bool {
	if st, ok := evNum(n, "rewardStatus"); !ok || st != 0 {
		return false
	}
	var content struct {
		B struct {
			Reward map[string]any `json:"reward"`
		} `json:"b"`
	}
	if err := json.Unmarshal([]byte(n.GetString("content")), &content); err != nil || content.B.Reward == nil {
		return false
	}
	ver, hasVer := content.B.Reward["rewardVersion"]
	if !hasVer || ver == nil {
		return true
	}
	local := strings.Split(gsl.AppVersion, ".")
	for i, s := range strings.Split(fmt.Sprint(ver), ".") {
		mail, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return false
		}
		have := 0
		if i < len(local) {
			have, _ = strconv.Atoi(local[i])
		}
		if mail > have {
			return false
		}
	}
	level, ok := content.B.Reward["rewardLevel"].(float64)
	return ok && float64(hq) >= level
}
