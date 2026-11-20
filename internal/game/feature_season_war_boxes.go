package game

import (
	"fmt"
	"log/slog"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Season war score boxes, two OPT-IN, static-only features.
//
// season-outpost-boxes (MASTER G12): the personal score boxes of the season 5/6 outpost battle.
// The client fetches outpost.battle.info {} while a SeasonWarZoneOutpostAttack activity (type 266,
// EnumType.lua:6347) is open (WorldAllianceCityDataManager.lua:72-79;
// SeasonMainActivitySignUtils.lua:528-532) and keeps a reply that has `user`
// (FetchOutpostBattleInfoMessage.lua:9-21). Box i (1-based) of user.scoreBox can be opened when
// user.score >= scoreBox[i].target and i-1 is not in user.scoreRewardIndex, the 0-based opened
// boxes (UILWSeasonOutpostAttackS6PlayerReward.lua:231-253; ZoneWarManager.lua:669-678); the box
// sends outpost.claim.box.reward {index: Int i-1} (FetchOutpostBattleRewardMessage.lua:4-7).
//
// cross-throne-rewards (MASTER G13): the cross-server throne war's server-win reward and personal
// score boxes. At every login the client sends cross.throne.schedule {} (InitMessage.lua:664;
// ZoneWarManager.lua:47-61) and, only when the reply has startTime, endTime, crossStartTime,
// curRound and initServerGroup and the war has started, cross.throne.fight.info {}
// (:94-119), whose reply it keeps when it has curVsRound[1] (:250-256). The boxes work as the
// outpost's, with cross.throne.claim.person.score.reward {index: Int i-1}
// (UIServerBattleLastKingPlayerReward.lua:211-240; CrossKingPersonScoreRewardMessage.lua:4-7). The
// server-win reward, cross.throne.claim.server.win.reward {} (CrossKingServerRewardWinMessage.lua:
// 4-6), is offered when user.winServerReward != 1 and the player's own curVsRound entry has win == 1
// (HasZoneWinRewardRedPoint, ZoneWarManager.lua:637-648; UIServerBattleLastKingZoneReward.lua:
// 59-85). The own entry is the one whose campId is the player's camp in a camp battle (schedule
// cfg.type VSCamp 2 or VSCampCenter 12, the source server in initServerGroup.group.a = camp 1 or .b
// = camp 2), else the one whose serverId (filled from campHeadServer, or the camp's first server,
// when missing) is the source server (:390-437, 1225-1301). Where the client would fall back to the
// first entry, no entry matching fails closed. A personal score needs fighting in the war, so
// mostly only the server-win reward is due for the sidecar.
//
// Both features send nothing that spends, marches or fights. No error code is registered as
// benign: the first replies are logged and an error stops the claims.
const (
	seasonOutpostFeature   = "season-outpost-boxes"
	seasonOutpostInfoCmd   = "outpost.battle.info"
	seasonOutpostBoxCmd    = "outpost.claim.box.reward"
	seasonActTypeOutpost   = 266 // SeasonWarZoneOutpostAttack (EnumType.lua:6347)
	crossThroneFeature     = "cross-throne-rewards"
	crossThroneScheduleCmd = "cross.throne.schedule"
	crossThroneFightCmd    = "cross.throne.fight.info"
	crossThroneBoxCmd      = "cross.throne.claim.person.score.reward"
	crossThroneWinCmd      = "cross.throne.claim.server.win.reward"
)

// crossThroneCampBattleTypes are ServerBattleType VSCamp and VSCampCenter (EnumType.lua:3859-3865),
// the schedule cfg types IsCampBattle tests (ZoneWarManager.lua:1225-1230).
var crossThroneCampBattleTypes = []int64{2, 12}

func init() {
	registerFeature(Feature{
		Name:    seasonOutpostFeature,
		Summary: "OPT-IN: in season, open the outpost battle's personal score boxes the player's score reached (static-only, not validated live)",
		Run:     runSeasonOutpostBoxes,
	})
	registerFeature(Feature{
		Name:    crossThroneFeature,
		Summary: "OPT-IN: claim the cross-server throne war's server-win reward and reached personal score boxes (static-only, not validated live)",
		Run:     runCrossThroneRewards,
	})
}

func runSeasonOutpostBoxes(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || evSeasonID(in) <= 0 {
		return nil
	}
	open := Activities(conn, in).Open(seasonActTypeOutpost)
	if len(open) == 0 {
		return nil
	}
	msg, err := session.SendAndWait(conn, seasonOutpostFeature+": outpost battle info", seasonOutpostInfoCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	user := evObject(msg.Params, "user")
	due := seasonScoreBoxesDue(user)
	slog.Info(seasonOutpostFeature+": outpost battle info", "activity", open[0].ID, "hasUser", user != nil,
		"score", evString(user, "score"), "boxes", len(evObjects(user, "scoreBox")), "opened", evNums(user, "scoreRewardIndex"), "due", due)
	return seasonClaimBoxes(conn, seasonOutpostFeature, seasonOutpostBoxCmd, due)
}

// seasonScoreBoxesDue returns the 0-based indexes of user's boxes the score reached and that are
// not opened (ZoneWarManager.lua:650-678); none without a user or score.
func seasonScoreBoxesDue(user *sfs.SFSObject) []int32 {
	if user == nil {
		return nil
	}
	score, ok := evNum(user, "score")
	if !ok {
		return nil
	}
	opened := evNums(user, "scoreRewardIndex")
	var due []int32
	for i, box := range evObjects(user, "scoreBox") {
		target, ok := evNum(box, "target")
		if ok && score >= target && !slices.Contains(opened, int64(i)) {
			due = append(due, int32(i))
		}
	}
	return due
}

// seasonClaimBoxes opens each box with cmd {index: Int}; an error stops.
func seasonClaimBoxes(conn *session.GameConn, feature, cmd string, due []int32) error {
	for _, i := range due {
		p := sfs.NewSFSObject()
		p.PutInt("index", i)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s: box %d", feature, i), cmd, p); err != nil {
			return err
		}
	}
	return nil
}

func runCrossThroneRewards(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	source, ok := ghostReconSourceServer(in)
	if !ok {
		slog.Info(crossThroneFeature + ": source server unknown; skipping")
		return nil
	}
	msg, err := session.SendAndWait(conn, crossThroneFeature+": schedule", crossThroneScheduleCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	sched := msg.Params
	start, _ := evNum(sched, "startTime")
	for _, k := range []string{"startTime", "endTime", "crossStartTime", "curRound", "initServerGroup"} {
		if !sched.Has(k) {
			slog.Info(crossThroneFeature+": no throne war scheduled (the schedule lacks "+k+"); nothing to claim", "startTime", start)
			return nil
		}
	}
	if evNow().UnixMilli() < start {
		slog.Info(crossThroneFeature+": the throne war has not started; nothing to claim", "startTime", start)
		return nil
	}
	fight, err := session.SendAndWait(conn, crossThroneFeature+": fight info", crossThroneFightCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	rounds := evObjects(fight.Params, "curVsRound")
	user := evObject(fight.Params, "user")
	if len(rounds) == 0 || user == nil {
		slog.Info(crossThroneFeature+": fight info without curVsRound or user; nothing to claim", "rounds", len(rounds), "hasUser", user != nil)
		return nil
	}
	own, how := crossThroneOwnEntry(sched, rounds, source)
	won, _ := evNum(own, "win")
	claimedWin, _ := evNum(user, "winServerReward")
	due := seasonScoreBoxesDue(user)
	slog.Info(crossThroneFeature+": fight info", "curRound", evString(sched, "curRound"), "sourceServer", source,
		"ownEntry", how, "ownWin", won, "winServerReward", claimedWin, "score", evString(user, "score"),
		"boxes", len(evObjects(user, "scoreBox")), "opened", evNums(user, "scoreRewardIndex"), "due", due)
	if own != nil && won == 1 && claimedWin != 1 {
		if _, err := claimAndLog(conn, crossThroneFeature+": server win reward", crossThroneWinCmd, sfs.NewSFSObject()); err != nil {
			return err
		}
	}
	return seasonClaimBoxes(conn, crossThroneFeature, crossThroneBoxCmd, due)
}

// crossThroneOwnEntry finds the player's side in rounds as SortDataByCampOrServer puts it first
// (ZoneWarManager.lua:390-437): by camp when the schedule is a camp battle, the source server is in
// one of its groups and the first entry carries a campId, else by serverId, a missing one filled
// from campHeadServer or the camp's first server (ParseVsRoundData). how says which; nil when no
// entry matches.
func crossThroneOwnEntry(sched *sfs.SFSObject, rounds []*sfs.SFSObject, source int64) (*sfs.SFSObject, string) {
	group := evObject(evObject(sched, "initServerGroup"), "group")
	campLeader := map[int64]int64{}
	var myCamp int64
	cfgType, _ := evNum(evObject(sched, "cfg"), "type")
	if slices.Contains(crossThroneCampBattleTypes, cfgType) && group != nil && group.Has("a") && group.Has("b") {
		for camp, key := range map[int64]string{1: "a", 2: "b"} {
			servers := evNums(group, key)
			if len(servers) > 0 {
				campLeader[camp] = servers[0]
			}
			if slices.Contains(servers, source) {
				myCamp = camp
			}
		}
	}
	if first, _ := evNum(rounds[0], "campId"); myCamp > 0 && first > 0 {
		for _, r := range rounds {
			if c, _ := evNum(r, "campId"); c == myCamp {
				return r, "camp"
			}
		}
		return nil, "camp, no match"
	}
	for _, r := range rounds {
		server, _ := evNum(r, "serverId")
		if server <= 0 {
			if head, _ := evNum(r, "campHeadServer"); head > 0 {
				server = head
			} else if c, _ := evNum(r, "campId"); c > 0 {
				server = campLeader[c]
			}
		}
		if server == source {
			return r, "server"
		}
	}
	return nil, "server, no match"
}
