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
// The owner's hard rules come first, and every target must pass all of them:
//   - Never on the account's own server. serverId must be a real id that is neither user.serverId
//     nor user.crossFightSrcServerId. Without a known user.serverId, nothing is plundered. This is
//     CRITICAL: the client itself only checks the server-sent trainServers
//     (CheckTrainInMatchServer), and the own server can be in that list.
//   - Trucks: UR only, the sleigh included (railwayIsURTruck).
//   - The target's defensive power must be known and below the attacking power. A truck's power
//     is marchInfo.power; a train's is the sum of its defending teams' power, unknown if any team
//     lacks it. The attacking power is railwayPower in strict mode, a lower bound on what the client
//     shows, built only from fresh hero.init.detail.get stats.
//
// Then the client's own gates (RailwayUtil.ClickAttackTrain, UITrainInfoView):
//   - today's plunders are below train_para 11 (4), shared by trucks and trains, and a station
//     without a dailyRobCount plunders nothing;
//   - the station is open and the city is at home;
//   - the target is travelling and on a matched server, and isn't the account's own or its
//     alliance's;
//   - its robTimes is under its cap: train_para 21 (2) for a truck, 1 for the sleigh, and
//     DataConfig alliance_train k10 (3) for a train, less one when a VIP rides it;
//   - a train is past marchInfo.protectTime;
//   - a truck whose effect 90107 caps the account's wins on it is skipped once that cap is reached.
//
// Requests:
//   - train.list {isRefresh: false}. The client sends true only from its Refresh button and on its
//     first open of the day.
//   - Truck: formation.save.template {index: 8+slot, action: 8, ...}, then
//     train.attack {uuid, serverId, squadNo, heroInfo, chipEquipGroup?} with the strongest attack
//     squad.
//   - Train: alliance.train.formation {type: 1} before each attack, then alliance.train.attack.kof
//     {uuid, serverId, teamInfos[3]}, or the legacy alliance.train.attack while init
//     allianceTrainStartInfo.isFunctionOn is off. The teams come from that reply's
//     formation.armyUnit1-3, the lineup the client attacks with. Only when the reply has no three
//     usable teams are the three strongest truck attack squads sent instead (a deviation: the client
//     would wait for the player to fill the lineup).
//
// The server resolves each fight in the reply. E400207 means someone else got there first, and the
// next target is tried. Any other refusal, a timeout or a dead connection stops the run.
const (
	railwayPlunderFeature = "railway-plunder"
	railwayDailyRobs      = 4 // train_para 11
	railwayTruckRobs      = 2 // train_para 21
	railwayTrainRobs      = 3 // DataConfig alliance_train k10 default (LWAllyStationDataManager)
	railwayWinCapEffect   = "90107"
	railwayTakenCode      = "E400207"
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
	uuid, serverID, cfgID, typ          int64
	ownerID, allianceID                 string
	sendTime, arriveTime, robs, protect int64
	power                               int64 // defensive power; 0 when unknown
	vipOn                               bool
	winCap, wins                        int64
}

func railwayTargetOf(o *sfs.SFSObject, uid string) railwayTarget {
	var t railwayTarget
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
	if t.typ == trainTypeTrain {
		t.power = railwayTrainPower(evObject(mi, "formation"))
	} else if p, ok := duelFloat(mi, "power"); ok && p > 0 {
		// marchInfo.power is what the client shows (TrainData). Its fallback, the sum of
		// marchInfo.heroInfo powers, leaves the troops out and would undercount the target.
		t.power = railwayCeil(p)
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

// railwayTrainPower is the sum of a train's defending teams (TrainData). It is 0 (unknown) when no
// team is present or a present team has no positive power.
func railwayTrainPower(f *sfs.SFSObject) int64 {
	var sum float64
	teams := 0
	for _, k := range []string{"armyUnit1", "armyUnit2", "armyUnit3"} {
		u := evObject(f, k)
		if u == nil {
			continue
		}
		p, ok := duelFloat(u, "power")
		if !ok || p <= 0 {
			return 0
		}
		sum += p
		teams++
	}
	if teams == 0 {
		return 0
	}
	return railwayCeil(sum)
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
	case len(homes) == 0 || t.serverID <= 0 || t.serverID > math.MaxInt32 || homes[t.serverID]:
		return "own or unknown server"
	case t.typ == trainTypeTruck && !railwayIsURTruck(t.cfgID):
		return "truck below UR"
	case t.typ != trainTypeTruck && t.typ != trainTypeTrain:
		return "unknown type"
	case t.power <= 0:
		return "defensive power unknown"
	case atk <= 0 || t.power >= atk:
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

// railwayLineup reads alliance.train.formation's formation.armyUnit1-3 as three attack teams
// (LWKOFBattleManager:ParseOneAtkTeam). ok is false unless all three have a slot 1..5 hero, no hero
// sits in two teams, and the team buffs are distinct.
func railwayLineup(reply *sfs.SFSObject) ([]railwaySquad, bool) {
	f := evObject(reply, "formation")
	var teams []railwaySquad
	heroes := map[int64]bool{}
	buffs := map[int32]bool{}
	for i, k := range []string{"armyUnit1", "armyUnit2", "armyUnit3"} {
		u := evObject(f, k)
		sq := railwaySquad{slot: int32(i + 1), chip: int32(i + 1), heroes: railwayHeroesOf(u, true)}
		if n, ok := claimInt(u, "squadNo"); ok && n >= 1 && n <= 4 {
			sq.squadNo = int32(n)
		}
		if n, ok := claimInt(u, "chipEquipGroup"); ok {
			sq.chip = 0
			if n >= 1 && n <= 4 {
				sq.chip = int32(n)
			}
		}
		if n, ok := claimInt(u, "soldierType"); ok && n > 0 {
			sq.soldier = int32(n)
		}
		if u == nil || sq.mainHeroes() == 0 || (sq.squadNo != 0 && buffs[sq.squadNo]) {
			return nil, false
		}
		buffs[sq.squadNo] = true
		for _, h := range sq.heroes {
			if h.index <= 5 && heroes[h.uuid] {
				return nil, false
			}
			heroes[h.uuid] = true
		}
		teams = append(teams, sq)
	}
	return teams, true
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
	robs, ok := claimInt(station, "dailyRobCount")
	if !ok || robs >= railwayDailyRobs {
		slog.Info(feature+": daily plunder count unknown or used up", "robs", robs, "known", ok)
		return nil
	}

	stats := railwayHeroStats(conn)
	if len(stats) == 0 {
		slog.Info(feature + ": no fresh hero stats, so the attacking power is unknown; plundering nothing")
		return nil
	}
	power := func(sq railwaySquad) int64 { return railwayPower(in, sq, stats, true) }
	var squads []railwayAttacker
	for _, sq := range railwaySquads(in, railwayAttackAction) {
		if sq.mainHeroes() > 0 {
			squads = append(squads, railwayAttacker{sq, power(sq)})
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

	// allowed applies every rule to one list entry for attacking power atk.
	allowed := func(t railwayTarget, typ, atk int64) string {
		if t.typ != typ {
			return "unknown type"
		}
		return railwayPlunderReason(t, homes, matched, uid, alliance, atk, now, railwayTrainCap(in, t.vipOn))
	}
	targets := func(key string, typ, atk int64) []railwayTarget {
		skipped := map[string]int{}
		var out []railwayTarget
		for _, o := range claimObjects(list.Params, key) {
			t := railwayTargetOf(o, uid)
			if why := allowed(t, typ, atk); why != "" {
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

	// attack sends one attack and reports whether to go on with the next target.
	var errs []error
	attack := func(label, cmd string, p *sfs.SFSObject, t railwayTarget) bool {
		msg, err := session.SendAndWait(conn, label, cmd, p)
		if msg == nil {
			errs = append(errs, err)
			return false // a timeout or a dead connection: the reply could still come late
		}
		if code, _ := claimString(msg.Params, "errorCode"); code != "" {
			slog.Info(feature+": attack refused", "uuid", t.uuid, "serverId", t.serverID, "errorCode", code)
			if code == railwayTakenCode {
				return true
			}
			errs = append(errs, err)
			return false
		}
		robs++ // never let a reply lower the count: an attack was just made
		if n, ok := claimInt(msg.Params, "dailyRobCount"); ok {
			robs = max(robs, n)
		}
		slog.Info(feature+": plundered", "type", t.typ, "uuid", t.uuid, "serverId", t.serverID, "defence", t.power,
			"robsToday", robs, "granted", describeGrants(msg.Params))
		return true
	}

	best := squads[0]
	saved := false
	for _, t := range targets("ls", trainTypeTruck, best.power) {
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

	cmd := "alliance.train.attack"
	if railwayBool(in.Object("allianceTrainStartInfo"), "isFunctionOn") {
		cmd = "alliance.train.attack.kof"
	}
	// The train list is filtered with the best possible lineup first; each train is then checked
	// again against the lineup actually sent.
	var fallback []railwaySquad
	var fallbackPower int64
	if len(squads) >= 3 {
		buffs := map[int32]bool{}
		for _, sq := range squads[:3] {
			if sq.squadNo != 0 && buffs[sq.squadNo] {
				fallback, fallbackPower = nil, 0 // the client needs distinct team buffs (500262)
				break
			}
			buffs[sq.squadNo] = true
			fallback = append(fallback, sq.railwaySquad)
			fallbackPower += sq.power
		}
	}
	for _, t := range targets("allianceTrainList", trainTypeTrain, math.MaxInt64) {
		if robs >= railwayDailyRobs {
			break
		}
		fp := sfs.NewSFSObject()
		fp.PutInt("type", 1)
		reply, err := session.SendAndWait(conn, feature+" train lineup", "alliance.train.formation", fp)
		if err != nil || reply == nil {
			return errors.Join(append(errs, err)...)
		}
		teams, ok := railwayLineup(reply.Params)
		var teamPower int64
		if ok {
			for _, sq := range teams {
				p := power(sq)
				if p <= 0 {
					teamPower = 0
					break
				}
				teamPower += p
			}
		} else {
			teams, teamPower = fallback, fallbackPower
		}
		if len(teams) != 3 || teamPower <= 0 {
			slog.Info(feature + ": no three attack teams with known power; trains left alone")
			break
		}
		if why := allowed(t, trainTypeTrain, teamPower); why != "" {
			slog.Info(feature+": train skipped", "uuid", t.uuid, "reason", why, "defence", t.power, "attack", teamPower)
			continue
		}
		arr := sfs.NewSFSArray()
		for i, sq := range teams {
			o := sfs.NewSFSObject()
			o.PutInt("teamNo", int32(i+1))
			o.PutInt("squadNo", sq.squadNo)
			o.PutSFSArray("heroInfos", railwayHeroArray(sq))
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
