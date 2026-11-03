package game

import (
	"cmp"
	"errors"
	"log/slog"
	"math"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Railway plunder (OPT-IN): rob other players' UR trade trucks and other alliances' trains with the
// account's attack squads. Static-only: from the 1.0.364 Lua, not sent live yet.
//
// The owner's hard rules come first. Every target must pass all of them:
//   - never on the account's own server: serverId is not user.serverId and not
//     user.crossFightSrcServerId. If neither id is known, nothing is plundered. This is CRITICAL;
//     the client itself only checks the server-sent trainServers (CheckTrainInMatchServer), and
//     the own server can be in that list;
//   - trucks: UR only, the sleigh included (railwayIsURTruck);
//   - the target's defensive power is known and below the attacking power, where the attacking
//     power is railwayPower, a lower bound on what the client shows.
//
// Then the client's own gates (RailwayUtil.ClickAttackTrain, UITrainInfoView):
//   - today's plunders are below train_para 11 (4), shared by trucks and trains;
//   - the station is open;
//   - the city is at home;
//   - the target is travelling and on a matched server;
//   - it isn't the account's own or its alliance's;
//   - its robTimes is under its cap: train_para 21 (2) for a truck, 1 for the sleigh, and
//     DataConfig alliance_train k10 (3) for a train, less one when a VIP rides it;
//   - a train is past marchInfo.protectTime;
//   - a truck whose effect 90107 caps the account's wins on it is skipped once that cap is reached.
//
// Requests:
//   - list: train.list {isRefresh: false};
//   - truck: formation.save.template {index: 8+slot, action: 8, ...}, then
//     train.attack {uuid, serverId, squadNo, heroInfo, chipEquipGroup?} with the strongest
//     attack squad;
//   - train: alliance.train.formation {type: 1}, then alliance.train.attack.kof (or the legacy
//     alliance.train.attack while init allianceTrainStartInfo.isFunctionOn is off)
//     {uuid, serverId, teamInfos[3]} with the three strongest attack squads.
//
// The server resolves each fight in the reply. E400207 means someone else got there first.
const (
	railwayPlunderFeature = "railway-plunder"
	railwayDailyRobs      = 4 // train_para 11
	railwayTruckRobs      = 2 // train_para 21
	railwayTrainRobs      = 3 // DataConfig alliance_train k10 default (LWAllyStationDataManager)
	railwayWinCapEffect   = "90107"
)

func init() {
	registerFeature(Feature{
		Name:    railwayPlunderFeature,
		Summary: "OPT-IN: plunder UR trucks (sleigh too) and alliance trains weaker than the attacking squads; never on the own server",
		Run:     runRailwayPlunder,
	})
}

// railwayTarget is one train.list entry.
type railwayTarget struct {
	obj                                 *sfs.SFSObject
	uuid, serverID, cfgID, typ          int64
	ownerID, allianceID                 string
	sendTime, arriveTime, robs, protect int64
	power                               int64 // defensive power; 0 when unknown
	vipOn                               bool
	winCap, wins                        int64
}

func railwayTargetOf(o *sfs.SFSObject, uid string) railwayTarget {
	t := railwayTarget{obj: o}
	t.uuid, _ = claimInt(o, "uuid")
	t.serverID, _ = claimInt(o, "serverId")
	t.cfgID, _ = claimInt(o, "cfgId")
	t.typ, _ = claimInt(o, "type")
	t.ownerID, _ = claimString(o, "ownerId")
	t.allianceID, _ = claimString(o, "allianceId")
	t.sendTime, _ = claimInt(o, "sendTime")
	t.arriveTime, _ = claimInt(o, "arriveTime")
	mi := evObject(o, "marchInfo")
	t.robs, _ = claimInt(mi, "robTimes")
	t.protect, _ = claimInt(mi, "protectTime")
	t.vipOn = railwayBool(mi, "vipOn")
	if t.typ == int64(trainTypeTrain) {
		// TrainData: a train's power is the sum of its three defending teams.
		f := evObject(mi, "formation")
		var sum float64
		for _, k := range []string{"armyUnit1", "armyUnit2", "armyUnit3"} {
			if p, ok := duelFloat(evObject(f, k), "power"); ok {
				sum += p
			}
		}
		t.power = railwayCeil(sum)
	} else if p, ok := duelFloat(mi, "power"); ok && p > 0 {
		t.power = railwayCeil(p)
	} else if hi := evObject(mi, "heroInfo"); hi != nil {
		var sum float64
		for _, k := range hi.Keys() {
			if p, ok := duelFloat(evObject(hi, k), "power"); ok {
				sum += p
			}
		}
		t.power = railwayCeil(sum)
	}
	if c, ok := duelFloat(evObject(mi, "effectInfo"), railwayWinCapEffect); ok && c > 0 {
		t.winCap = int64(c)
		for _, r := range claimObjectsOrMap(mi, "plunderRecord") {
			if who, _ := claimString(r, "uid"); who == uid && railwayBool(r, "isWin") {
				t.wins++
			}
		}
	}
	return t
}

// railwayCeil rounds a target's power up, the safe direction for "target below the attacker".
func railwayCeil(f float64) int64 { return int64(math.Ceil(f)) }

// railwayBool is claimBool without the ok flag.
func railwayBool(o *sfs.SFSObject, key string) bool { b, _ := claimBool(o, key); return b }

// TrainType (EnumType.lua): 1 truck, 2 alliance train.
const (
	trainTypeTruck = 1
	trainTypeTrain = 2
)

// railwayPlunderReason returns why t must not be attacked with attacking power atk, or "" when it
// may be. The owner's hard rules are checked first.
func railwayPlunderReason(t railwayTarget, homes, matched map[int64]bool, uid, alliance string, atk, nowMs, trainCap int64) string {
	switch {
	case len(homes) == 0 || homes[t.serverID]:
		return "own server"
	case t.typ == trainTypeTruck && !railwayIsURTruck(t.cfgID):
		return "truck below UR"
	case t.typ != trainTypeTruck && t.typ != trainTypeTrain:
		return "unknown type"
	case t.power <= 0:
		return "defensive power unknown"
	case t.power >= atk:
		return "not weaker than the attacking squad"
	case !matched[t.serverID]:
		return "server not matched"
	case t.ownerID == "" || t.ownerID == uid || (alliance != "" && t.allianceID == alliance):
		return "own or alliance"
	case t.sendTime <= 0 || nowMs >= t.arriveTime:
		return "not travelling"
	case t.typ == trainTypeTruck && railwayTruckQuality(t.cfgID) == 10 && t.robs > 0:
		return "sleigh already plundered"
	case t.typ == trainTypeTruck && t.robs >= railwayTruckRobs:
		return "plunder cap reached"
	case t.typ == trainTypeTruck && t.winCap > 0 && t.wins >= t.winCap:
		return "win cap reached"
	case t.typ == trainTypeTrain && t.robs >= trainCap:
		return "plunder cap reached"
	case t.typ == trainTypeTrain && nowMs < t.protect:
		return "protected"
	}
	return ""
}

// railwayTrainCap is the per-train plunder cap: init dataConfig.alliance_train.k10 when present,
// else 3. A VIP-boarded train is capped one lower; the client subtracts the alliance-right para1
// (1) only when the VIP switch and gift level allow, so this can only be stricter.
func railwayTrainCap(in *Init, vip bool) int64 {
	c := int64(railwayTrainRobs)
	if n, ok := claimInt(evObject(in.Object("dataConfig"), "alliance_train"), "k10"); ok && n > 0 {
		c = n
	}
	if vip {
		c--
	}
	return c
}

type railwayAttacker struct {
	railwaySquad
	power int64
}

func runRailwayPlunder(conn *session.GameConn, in *Init) error {
	const feature = railwayPlunderFeature
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	homes, ok := railwayHomeServers(in)
	if !ok {
		slog.Info(feature + ": the account's own server id is unknown; plundering nothing")
		return nil
	}
	if !railwayAtHome(in) {
		slog.Info(feature + ": the city is in another warzone and " + railwayCrossSwitch + " is off; skipping, as the client does")
		return nil
	}
	station := railwayStation(conn, in, feature)
	now := claimNow(in).UnixMilli()
	if !railwayStationOpen(in, station, now) {
		slog.Info(feature + ": the railway station isn't open yet; skipping")
		return nil
	}
	robs, _ := claimInt(station, "dailyRobCount")
	if robs >= railwayDailyRobs {
		slog.Info(feature+": daily plunder limit reached", "robs", robs)
		return nil
	}

	stats := railwayHeroStats(conn)
	var squads []railwayAttacker
	for _, sq := range railwaySquads(in, railwayAttackAction) {
		if sq.mainHeroes() > 0 {
			squads = append(squads, railwayAttacker{sq, railwayPower(in, sq, stats)})
		}
	}
	slices.SortStableFunc(squads, func(a, b railwayAttacker) int { return cmp.Compare(b.power, a.power) })
	if len(squads) == 0 || squads[0].power <= 0 {
		slog.Info(feature + ": no attack squad with known power; plundering nothing")
		return nil
	}

	lp := sfs.NewSFSObject()
	lp.PutBool("isRefresh", false)
	list, err := session.SendAndWait(conn, feature+" list", "train.list", lp)
	if err != nil || list == nil {
		return err
	}
	servers, _ := claimString(list.Params, "trainServers")
	if servers == "" {
		servers, _ = claimString(in.Raw, "trainServers")
	}
	matched := railwayMatched(servers)
	uid, alliance := railwayOwnIDs(in)

	pick := func(key string, typ int64, atk int64) []railwayTarget {
		skipped := map[string]int{}
		var out []railwayTarget
		for _, o := range claimObjects(list.Params, key) {
			t := railwayTargetOf(o, uid)
			if t.typ != typ {
				skipped["unknown type"]++
				continue
			}
			if why := railwayPlunderReason(t, homes, matched, uid, alliance, atk, now, railwayTrainCap(in, t.vipOn)); why != "" {
				skipped[why]++
				continue
			}
			out = append(out, t)
		}
		if len(skipped) > 0 {
			slog.Info(feature+": targets skipped", "list", key, "reasons", skipped)
		}
		slices.SortStableFunc(out, func(a, b railwayTarget) int {
			return cmp.Or(cmp.Compare(railwayTruckQuality(b.cfgID), railwayTruckQuality(a.cfgID)),
				cmp.Compare(a.robs, b.robs), cmp.Compare(a.power, b.power))
		})
		return out
	}

	var errs []error
	attack := func(label, cmd string, p *sfs.SFSObject, t railwayTarget) bool {
		msg, err := session.SendAndWait(conn, label, cmd, p)
		if session.ContainsNonTimeoutNetError(err) {
			errs = append(errs, err)
			return false
		}
		if err != nil || msg == nil || msg.Params.Has("errorCode") {
			slog.Info(feature+": attack refused", "uuid", t.uuid, "serverId", t.serverID, "err", err)
			return true
		}
		// Never let a reply lower the count: an attack was just made.
		robs++
		if n, ok := claimInt(msg.Params, "dailyRobCount"); ok {
			robs = max(robs, n)
		}
		slog.Info(feature+": plundered", "type", t.typ, "uuid", t.uuid, "serverId", t.serverID, "defence", t.power,
			"robsToday", robs, "granted", describeGrants(msg.Params))
		return true
	}

	best := squads[0]
	saved := false
	for _, t := range pick("ls", trainTypeTruck, best.power) {
		if robs >= railwayDailyRobs {
			break
		}
		if !saved {
			if err := railwaySaveSquad(conn, feature, best.railwaySquad, railwayAttackAction); err != nil {
				return errors.Join(append(errs, err)...)
			}
			saved = true
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", t.uuid)
		p.PutInt("serverId", int32(t.serverID))
		p.PutInt("squadNo", best.squadNo)
		p.PutSFSArray("heroInfo", railwayHeroArray(best.railwaySquad))
		if best.chip >= 1 && best.chip <= 4 {
			p.PutInt("chipEquipGroup", best.chip)
		}
		if !attack(feature+" truck", "train.attack", p, t) {
			return errors.Join(errs...)
		}
	}

	if robs >= railwayDailyRobs || len(squads) < 3 {
		return errors.Join(errs...)
	}
	teams := squads[:3]
	teamPower := teams[0].power + teams[1].power + teams[2].power
	trains := pick("allianceTrainList", trainTypeTrain, teamPower)
	if len(trains) == 0 {
		return errors.Join(errs...)
	}
	fp := sfs.NewSFSObject()
	fp.PutInt("type", 1)
	if _, err := session.SendAndWait(conn, feature+" train lineup", "alliance.train.formation", fp); session.ContainsNonTimeoutNetError(err) {
		return errors.Join(append(errs, err)...)
	}
	cmd := "alliance.train.attack"
	if railwayBool(in.Object("allianceTrainStartInfo"), "isFunctionOn") {
		cmd = "alliance.train.attack.kof"
	}
	for _, t := range trains {
		if robs >= railwayDailyRobs {
			break
		}
		arr := sfs.NewSFSArray()
		for i, sq := range teams {
			o := sfs.NewSFSObject()
			o.PutInt("teamNo", int32(i+1))
			o.PutInt("squadNo", sq.squadNo)
			o.PutSFSArray("heroInfos", railwayHeroArray(sq.railwaySquad))
			if sq.chip >= 1 && sq.chip <= 4 {
				o.PutInt("chipEquipGroup", sq.chip)
			}
			arr.AddSFSObject(o)
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", t.uuid)
		p.PutInt("serverId", int32(t.serverID))
		p.PutSFSArray("teamInfos", arr)
		if !attack(feature+" train", cmd, p, t) {
			break
		}
	}
	return errors.Join(errs...)
}
