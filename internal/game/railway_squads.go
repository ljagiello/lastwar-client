package game

import (
	"cmp"
	"log/slog"
	"math"
	"slices"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Shared by truck-deploy and railway-plunder: which trucks count as UR, the account's home
// servers, the truck squads, and a lower bound on a squad's power. Static-only: from the 1.0.364
// Lua (LWMyStationDataManager, ArenaArmyFormationInfo, SoldierDataManager) and tables
// (train_property, train_para, lw_soldier, lw_effect_number), not from a live capture.

// railwayURRanges and railwaySleighRanges are the train_property rows with type 1 (truck) and
// quality 5 (UR) or 10 (the Reindeer Sleigh, a "special UR", TrainData isSpecialURQuality) in the
// 1.0.364 table. The wire carries only cfgId; the client looks the quality up in this table.
var (
	railwayURRanges     = [][2]int64{{121, 150}, {641, 675}, {5031, 5035}}
	railwaySleighRanges = [][2]int64{{201, 235}, {676, 710}}
)

// railwayTruckQuality is the quality of truck cfgId: 5 for UR, 10 for the sleigh, 0 for anything
// else, including ids this table doesn't know (so an unknown truck is never treated as UR).
func railwayTruckQuality(cfgID int64) int32 {
	in := func(rs [][2]int64) bool {
		for _, r := range rs {
			if cfgID >= r[0] && cfgID <= r[1] {
				return true
			}
		}
		return false
	}
	switch {
	case in(railwaySleighRanges):
		return 10
	case in(railwayURRanges):
		return 5
	}
	return 0
}

// railwayIsURTruck is the owner's rule for both deploying and plundering: only UR trucks,
// counting the sleigh, never anything below.
func railwayIsURTruck(cfgID int64) bool { return railwayTruckQuality(cfgID) != 0 }

// railwayHomeServers returns every server id that counts as the account's own: init
// user.serverId, and user.crossFightSrcServerId when it is >= 0 (the home server during a
// cross-server fight, PlayerInfo:GetSourceServerId). ok is false when neither can be read, and
// callers must then plunder nothing.
func railwayHomeServers(in *Init) (map[int64]bool, bool) {
	user := in.Object("user")
	out := map[int64]bool{}
	if id, ok := claimInt(user, "serverId"); ok && id > 0 {
		out[id] = true
	}
	if id, ok := claimInt(user, "crossFightSrcServerId"); ok && id >= 0 {
		out[id] = true
	}
	return out, len(out) > 0
}

// railwayOwnIDs returns the account's uid and alliance id ("" when not in an alliance). The
// client hides the attack button on its own and its alliance's trucks and trains
// (TrainData:IsMyOrAllyTrain). Only those fields of user are read; user holds PII.
func railwayOwnIDs(in *Init) (uid, alliance string) {
	uid, _ = claimString(in.Object("user"), "uid")
	for _, id := range []string{
		func() string { s, _ := claimString(in.Object("alliance"), "uid"); return s }(),
		func() string { s, _ := claimString(in.Object("user"), "allianceId"); return s }(),
	} {
		if id != "" && id != "0" {
			return uid, id
		}
	}
	return uid, ""
}

// railwayMatched parses trainServers, the comma-separated list of servers the account is matched
// against (LWMyStationDataManager:SetMatchServer).
func railwayMatched(s string) map[int64]bool {
	out := map[int64]bool{}
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if id, ok := claimIntValue(s[start:i]); ok {
				out[id] = true
			}
			start = i + 1
		}
	}
	return out
}

// Truck squad templates (formation_template, FormationSaveType): index 5..8 are defence squads
// 1..4, index 9..12 attack squads 1..4.
const (
	railwayDefenceAction = 4
	railwayAttackAction  = 8
	railwayOverlordSlot  = 6
	railwayMilitaryCamp  = 10103000
)

type railwayHero struct {
	uuid  int64
	index int32 // 1..5, or railwayOverlordSlot
	// Template fallbacks for the hero's power (effect 100000) and leadership (effect 50079),
	// used when hero.init.detail.get didn't cover the hero (ArenaArmyFormationInfo heroDataMap).
	power, lead       float64
	hasPower, hasLead bool
}

// railwaySquad is one truck squad as the client would send it.
type railwaySquad struct {
	slot    int32 // 1..4, sent as squadNoClient on deploy
	heroes  []railwayHero
	squadNo int32 // team buff 1..4 (localSquadNo)
	chip    int32 // chipEquipGroup 1..4
	soldier int32 // template soldierType; 0 means the highest trainable unit
}

// mainHeroes counts the heroes in slots 1..5; the client refuses a squad without one (457564).
func (s railwaySquad) mainHeroes() int {
	n := 0
	for _, h := range s.heroes {
		if h.index >= 1 && h.index <= 5 {
			n++
		}
	}
	return n
}

// railwayUnlockedSquads is the set of world squad slots that exist: army_formation entries with a
// non-zero uuid (ArmyFormationDataManager:HasArmyFormationInIndex).
func railwayUnlockedSquads(in *Init) map[int32][]railwayHero {
	out := map[int32][]railwayHero{}
	for _, f := range claimObjectsOrMap(in.Raw, "army_formation") {
		idx, _ := claimInt(f, "index")
		uuid, _ := claimInt(f, "uuid")
		if idx < 1 || idx > 4 || uuid == 0 {
			continue
		}
		out[int32(idx)] = railwayHeroesOf(f, false)
	}
	return out
}

// railwayHeroesOf reads heroes|heros[{heroUuid, index, power?, effect_50079?}]. withOverlord keeps
// the slot-6 entry; the client drops it when it copies a world squad (InitTruckFormationWithoutServerHelper).
func railwayHeroesOf(f *sfs.SFSObject, withOverlord bool) []railwayHero {
	list := claimObjects(f, "heroes")
	if len(list) == 0 {
		list = claimObjects(f, "heros")
	}
	var out []railwayHero
	seen := map[int64]bool{}
	for _, o := range list {
		uuid, ok := claimFirstInt(o, "heroUuid", "uuid")
		idx, _ := claimInt(o, "index")
		if !ok || uuid == 0 || seen[uuid] || idx < 1 || idx > railwayOverlordSlot || (idx == railwayOverlordSlot && !withOverlord) {
			continue
		}
		seen[uuid] = true
		h := railwayHero{uuid: uuid, index: int32(idx)}
		h.power, h.hasPower = duelFloat(o, "power")
		h.lead, h.hasLead = duelFloat(o, "effect_50079")
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b railwayHero) int { return cmp.Compare(a.index, b.index) })
	return out
}

// railwaySquads returns the truck squads of one set (railwayDefenceAction or railwayAttackAction)
// in slot order, as LWMyStationDataManager parses formation_template: only unlocked slots, a hero
// kept in the lowest slot when two squads of the set share it (CheckIfHasMultiHero; the client's
// pairs order is not deterministic), team buffs filled by InitTeamUsingBuff, and chip set 1 when
// the template has none (ArmyFormationInfo default). With no truck templates at all the client
// copies the world squads (InitTruckFormationWithoutServerHelper), squadNo and chip = slot.
func railwaySquads(in *Init, action int32) []railwaySquad {
	unlocked := railwayUnlockedSquads(in)
	anyTemplate := false
	bySlot := map[int32]*sfs.SFSObject{}
	for _, t := range claimObjectsOrMap(in.Raw, "formation_template") {
		idx, _ := claimInt(t, "index")
		if idx >= 5 && idx <= 12 {
			anyTemplate = true
		}
		if s := int32(idx) - action; idx > int64(action) && s >= 1 && s <= 4 {
			bySlot[s] = t
		}
	}
	var out []railwaySquad
	if !anyTemplate {
		for s := int32(1); s <= 4; s++ {
			if heroes, ok := unlocked[s]; ok {
				out = append(out, railwaySquad{slot: s, heroes: heroes, squadNo: s, chip: s})
			}
		}
		return out
	}
	used := map[int64]bool{}
	buffs := map[int32]bool{}
	for s := int32(1); s <= 4; s++ {
		t, ok := bySlot[s]
		if _, open := unlocked[s]; !ok || !open {
			continue
		}
		sq := railwaySquad{slot: s, chip: 1}
		for _, h := range railwayHeroesOf(t, true) {
			if !used[h.uuid] {
				used[h.uuid] = true
				sq.heroes = append(sq.heroes, h)
			}
		}
		if n, ok := claimInt(t, "squadNo"); ok && n >= 1 && n <= 4 {
			sq.squadNo = int32(n)
		}
		if n, ok := claimInt(t, "chipEquipGroup"); ok && n >= 1 && n <= 4 {
			sq.chip = int32(n)
		}
		if n, ok := claimInt(t, "soldierType"); ok && n > 0 {
			sq.soldier = int32(n)
		}
		if sq.squadNo != 0 {
			buffs[sq.squadNo] = true
		}
		out = append(out, sq)
	}
	for i := range out {
		if out[i].squadNo != 0 {
			continue
		}
		pick := out[i].slot
		if buffs[pick] {
			pick = 0
			for b := int32(1); b <= 4; b++ {
				if !buffs[b] {
					pick = b
					break
				}
			}
		}
		if pick != 0 {
			out[i].squadNo = pick
			buffs[pick] = true
		}
	}
	return out
}

// railwayHeroStat is a hero's power (effect 100000) and leadership (effect 50079).
type railwayHeroStat struct{ power, lead float64 }

// railwayHeroStats sends hero.init.detail.get {} (read-only; the real client sends it after every
// init, HeroDataManager:InitData) and maps each userHero uuid to its effect 100000 and 50079
// (HeroInfo:RefreshHeroPower, GetSoldierCapacity). A failed or empty reply returns nil: squad
// power then falls back to the template fields, and missing heroes count as 0.
func railwayHeroStats(conn *session.GameConn) map[int64]railwayHeroStat {
	msg, err := session.SendAndWait(conn, "hero details", "hero.init.detail.get", sfs.NewSFSObject())
	if err != nil || msg == nil || msg.Params.Has("errorCode") {
		slog.Info("railway: hero.init.detail.get gave no hero details; squad power uses the template fields only", "err", err)
		return nil
	}
	out := map[int64]railwayHeroStat{}
	for _, h := range claimObjectsOrMap(msg.Params, "userHero") {
		uuid, ok := claimInt(h, "uuid")
		eff := evObject(h, "effect")
		if !ok || eff == nil {
			continue
		}
		var st railwayHeroStat
		st.power, _ = duelFloat(eff, "100000")
		st.lead, _ = duelFloat(eff, "50079")
		out[uuid] = st
	}
	return out
}

// railwaySoldierStat is a player soldier's lw_soldier soldier_effect_number base life, attack and
// defense (the first three entries, SoldierDataTemplate:InitSoliderEffectNumber).
type railwaySoldierStat struct{ life, attack, defense float64 }

var railwaySoldierStats = map[int32]railwaySoldierStat{
	3005: {34.8, 0.3, 0.1}, 3006: {52.2, 0.5, 0.1}, 3007: {125, 1, 0.2}, 3008: {323.1, 2.6, 0.6},
	3009: {624.6, 5, 1}, 3010: {881.7, 7, 1.4}, 3011: {1187.9, 9.5, 1.9}, 3012: {1622.6, 12.9, 2.6},
	3013: {2064.3, 16.4, 3.3}, 3014: {2514, 20, 4}, 3015: {2769.7, 24.1, 4.4},
}

// lw_effect_number power for 50066 (life), 50068 (attack) and 50070 (defense).
const (
	railwayLifePower    = 0.5
	railwayAttackPower  = 12.5
	railwayDefensePower = 35
)

// railwayPower estimates a squad's power from below, the number the owner's rule compares with a
// target's defensive power. It follows ArenaArmyFormationInfo:GetTotalCapacity for the two big
// terms and leaves out the rest, which are all >= 0 (T11 attributes, tactical weapon, chip set,
// Overlord power), so the estimate never exceeds what the client would show:
//   - heroes: floor(effect 100000) per hero in slots 1..5;
//   - soldiers: floor(leadership) virtual soldiers per hero (and the Overlord), all of the
//     template's soldierType or else the highest unit the camps can train, each worth
//     SoldierDataManager:CalcSoldierPower with the init game effects.
//
// A hero with no known stats counts as 0.
func railwayPower(in *Init, sq railwaySquad, stats map[int64]railwayHeroStat) int64 {
	unit, ok := railwaySoldierStats[sq.soldier]
	if !ok {
		var lv int64
		for _, b := range in.Objects("building_new") {
			if id, _ := evNum(b, "bId"); id == railwayMilitaryCamp {
				if l, _ := evNum(b, "lv"); l > lv {
					lv = l
				}
			}
		}
		if top, found := duelTopUnit(lv, duelTrainUnlocksOf(in)); found {
			unit, ok = railwaySoldierStats[top.id]
		}
	}
	eff := func(id string) float64 { f, _ := initEffect(in, id); return f }
	life := (unit.life + eff("50065")) * (1 + eff("50076"))
	atk := (unit.attack + eff("50067")) * (1 + eff("50077"))
	def := (unit.defense + eff("50069")) * (1 + eff("50078"))
	var total int64
	for _, h := range sq.heroes {
		st, known := stats[h.uuid]
		power, lead := st.power, st.lead
		if !known {
			power, lead = h.power, h.lead
		}
		if h.index <= 5 {
			total += int64(math.Floor(power))
		}
		if n := math.Floor(lead); ok && n > 0 {
			total += int64(math.Floor(life*railwayLifePower*n)) + int64(math.Floor(atk*railwayAttackPower*n)) +
				int64(math.Floor(def*railwayDefensePower*n))
		}
	}
	return total
}

// railwayHeroArray is ArmyFormationUtils.GenerateServerHeroArray: {heroUuid: Long, index: Int}
// per hero, the Overlord in slot 6.
func railwayHeroArray(sq railwaySquad) *sfs.SFSArray {
	arr := sfs.NewSFSArray()
	for _, h := range sq.heroes {
		o := sfs.NewSFSObject()
		o.PutLong("heroUuid", h.uuid)
		o.PutInt("index", h.index)
		arr.AddSFSObject(o)
	}
	return arr
}

// railwaySaveSquad sends formation.save.template for sq, as the client does before every
// train.send (action 4) and train.attack (action 8) (LWMyStationDataManager:TrySaveTruckFormation,
// FormationSaveMessage).
func railwaySaveSquad(conn *session.GameConn, feature string, sq railwaySquad, action int32) error {
	p := sfs.NewSFSObject()
	p.PutInt("index", sq.slot+action)
	p.PutSFSArray("heroInfos", railwayHeroArray(sq))
	p.PutInt("action", action)
	if sq.chip >= 1 && sq.chip <= 4 {
		p.PutInt("chipEquipGroup", sq.chip)
	}
	if sq.squadNo >= 1 && sq.squadNo <= 4 {
		p.PutInt("squadNo", sq.squadNo)
	}
	_, err := session.SendAndWait(conn, feature+" squad save", "formation.save.template", p)
	return err
}

// railwayStation is the fresh station state from train.data (GetMyStationDataMessage): the
// fields sit at the top level of the reply. It falls back to init trainData when the call fails.
func railwayStation(conn *session.GameConn, in *Init, feature string) *sfs.SFSObject {
	msg, err := session.SendAndWait(conn, feature+" station data", "train.data", sfs.NewSFSObject())
	if err == nil && msg != nil && !msg.Params.Has("errorCode") && msg.Params.Has("myTrainList") {
		return msg.Params
	}
	slog.Info(feature+": train.data gave no station data; using the login init", "err", err)
	return in.Object("trainData")
}

// railwayStationOpen is GetRailwayStationState past FirstReward: out of the fog, the activity
// started (openTime and now > openTime), and the opening reward taken. Trucks can't leave and the
// enemy list doesn't open before that.
func railwayStationOpen(in *Init, station *sfs.SFSObject, nowMs int64) bool {
	at, ok := claimInt(station, "openTime")
	open, _ := claimInt(station, "openReward")
	return railwayOutOfFog(in) && ok && nowMs > at && open != 0
}
