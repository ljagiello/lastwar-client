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
// user.serverId, and user.crossFightSrcServerId when it is set (the home server during a
// cross-server fight, PlayerInfo:GetSourceServerId). The client treats 0 and -1 as unset for both
// (PlayerInfo defaults, GetSelfServerId). ok is false unless user.serverId is known, and callers
// must then plunder nothing.
func railwayHomeServers(in *Init) (map[int64]bool, bool) {
	user := in.Object("user")
	id, ok := claimInt(user, "serverId")
	if !ok || id <= 0 {
		return nil, false
	}
	out := map[int64]bool{id: true}
	if src, ok := claimInt(user, "crossFightSrcServerId"); ok && src > 0 {
		out[src] = true
	}
	return out, true
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
	// Template copies of the hero's power (effect 100000) and leadership (effect 50079)
	// (ArenaArmyFormationInfo heroDataMap); only truck-deploy's ranking falls back to them.
	power, lead float64
}

// railwaySquad is one truck squad as the client would send it.
type railwaySquad struct {
	slot    int32 // 1..4, sent as squadNoClient on deploy
	heroes  []railwayHero
	squadNo int32 // team buff 1..4 (localSquadNo)
	chip    int32 // chipEquipGroup, sent only when 1..4
	soldier int32 // soldierType; with soldierSet false, the highest trainable unit instead
	// soldierSet is true when the wire carried soldierType; a value without a lw_soldier row (0
	// included) then adds no soldiers (see readSoldier).
	soldierSet bool
}

// readSoldier reads o's soldierType the way ArenaArmyFormationInfo:ParseData does
// (ArenaArmyFormationInfo.lua:128-133): a present value is kept as is, 0 included (0 is true in
// Lua), and ConscriptSoldier finds no lw_soldier row for 0 and adds no soldiers (:30-36). Only an
// absent soldierType takes the highest trainable unit (RecalculateMaxLevelSoldier, :208-214).
func (s *railwaySquad) readSoldier(o *sfs.SFSObject) {
	if v, ok := o.Get("soldierType"); !ok || v.Val == nil {
		return
	}
	s.soldierSet = true
	if n, ok := claimInt(o, "soldierType"); ok && n > 0 && n <= math.MaxInt32 {
		s.soldier = int32(n)
	}
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
// non-zero uuid (ArmyFormationDataManager:HasArmyFormationInIndex), with their slot 1..5 heroes.
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

// railwayHeroesOf reads heroes|heros[{heroUuid, index, power?, effect_50079?}] the way
// ArmyFormationInfo:ParseData fills localHeroes (uuid -> slot) and localIndexToHeroDic (slot ->
// uuid): a later entry wins a slot, and a uuid keeps its last slot. withOverlord keeps the slot-6
// entry; the client drops it when it copies a world squad (InitTruckFormationWithoutServerHelper).
func railwayHeroesOf(f *sfs.SFSObject, withOverlord bool) []railwayHero {
	list := claimObjects(f, "heroes")
	if len(list) == 0 {
		list = claimObjects(f, "heros")
	}
	bySlot := map[int32]railwayHero{}
	slotOf := map[int64]int32{}
	for _, o := range list {
		uuid, ok := claimFirstInt(o, "heroUuid", "uuid")
		idx, _ := claimInt(o, "index")
		if !ok || uuid == 0 || idx < 1 || idx > railwayOverlordSlot || (idx == railwayOverlordSlot && !withOverlord) {
			continue
		}
		h := railwayHero{uuid: uuid, index: int32(idx)}
		h.power, _ = duelFloat(o, "power")
		h.lead, _ = duelFloat(o, "effect_50079")
		bySlot[h.index] = h
		slotOf[uuid] = h.index
	}
	var out []railwayHero
	for _, h := range bySlot {
		if slotOf[h.uuid] == h.index {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b railwayHero) int { return cmp.Compare(a.index, b.index) })
	return out
}

// railwaySquads returns the truck squads of one set (railwayDefenceAction or railwayAttackAction)
// in slot order, as LWMyStationDataManager:InitArmyFormationListData parses formation_template
// (LWMyStationDataManager.lua:509-550). It walks the templates in wire order and inserts each
// squad as it goes, so every step sees only the squads inserted before it:
//   - only templates of unlocked slots are inserted (of two templates for one slot, which the
//     server isn't expected to send, only the later one is kept);
//   - a missing team buff is filled by InitTeamUsingBuff before the insert: the squad's own slot
//     unless an inserted squad uses it, else the first buff 1..4 none does (:1155-1166,
//     1181-1194);
//   - a slot 1..5 hero shared by two squads stays in the first one in that order
//     (CheckIfHasMultiHero walks the inserted list, :493-507; localHeroes never holds the
//     Overlord);
//   - chip set 1 when the template has none (ArmyFormationInfo default), and none sent when it has
//     one outside 1..4;
//   - soldierType as readSoldier reads it.
//
// An unlocked slot without a template has no squad here: the client saves an empty one, with no
// hero to send. When the set has no template for an unlocked slot at all, the client copies the
// world squads' heroes instead (InitTruckFormationWithoutServerHelper, :1024-1057), with squadNo
// and chip = slot.
func railwaySquads(in *Init, action int32) []railwaySquad {
	unlocked := railwayUnlockedSquads(in)
	type template struct {
		slot int32
		obj  *sfs.SFSObject
	}
	var templates []template
	last := map[int32]int{} // slot -> index in templates of its last template
	for _, t := range claimObjectsOrMap(in.Raw, "formation_template") {
		idx, _ := claimInt(t, "index")
		s := int32(idx) - action
		if idx <= int64(action) || s < 1 || s > 4 {
			continue
		}
		if _, open := unlocked[s]; open {
			last[s] = len(templates)
			templates = append(templates, template{s, t})
		}
	}
	var out []railwaySquad
	if len(templates) == 0 {
		for s := int32(1); s <= 4; s++ {
			if heroes, ok := unlocked[s]; ok {
				out = append(out, railwaySquad{slot: s, heroes: heroes, squadNo: s, chip: s})
			}
		}
		return out
	}
	used := map[int64]bool{}
	buffs := map[int32]bool{}
	for i, t := range templates {
		if last[t.slot] != i {
			continue
		}
		sq := railwaySquad{slot: t.slot, chip: 1}
		for _, h := range railwayHeroesOf(t.obj, true) {
			if h.index != railwayOverlordSlot {
				if used[h.uuid] {
					continue
				}
				used[h.uuid] = true
			}
			sq.heroes = append(sq.heroes, h)
		}
		if n, ok := claimInt(t.obj, "squadNo"); ok && n >= 1 && n <= 4 {
			sq.squadNo = int32(n)
		}
		if n, ok := claimInt(t.obj, "chipEquipGroup"); ok {
			sq.chip = 0
			if n >= 1 && n <= 4 {
				sq.chip = int32(n)
			}
		}
		sq.readSoldier(t.obj)
		if sq.squadNo == 0 {
			sq.squadNo = sq.slot
			if buffs[sq.slot] {
				sq.squadNo = 0 // stays 0 when every buff is taken
				for b := int32(1); b <= 4 && sq.squadNo == 0; b++ {
					if !buffs[b] {
						sq.squadNo = b
					}
				}
			}
		}
		if sq.squadNo != 0 {
			buffs[sq.squadNo] = true
		}
		out = append(out, sq)
	}
	slices.SortFunc(out, func(a, b railwaySquad) int { return cmp.Compare(a.slot, b.slot) })
	return out
}

// railwayHeroStat is a hero's power (effect 100000) and leadership (effect 50079).
type railwayHeroStat struct{ power, lead float64 }

// railwayHeroStats sends hero.init.detail.get {} (read-only; the real client sends it after every
// init, HeroDataManager:InitData) and maps each userHero uuid to its effect 100000 and 50079
// (HeroInfo:RefreshHeroPower, GetSoldierCapacity). A failed or empty reply returns nil.
func railwayHeroStats(conn *session.GameConn) map[int64]railwayHeroStat {
	msg, err := session.SendAndWait(conn, "hero details", "hero.init.detail.get", sfs.NewSFSObject())
	if err != nil || msg == nil || msg.Params.Has("errorCode") {
		slog.Info("railway: hero.init.detail.get gave no hero details", "err", err)
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
//   - soldiers: floor(leadership) virtual soldiers per hero (and the Overlord), each worth
//     SoldierDataManager:CalcSoldierPower with the init game effects. The unit is the squad's
//     soldierType, or the highest one the camps can train when the wire carried none
//     (readSoldier). A soldierType this table doesn't know (0, or e.g. a Mummy unit) adds no
//     soldier power. The client's truck-rob panel recalculates the unit to the highest trainable
//     one (UIHeroFakePVPFormationPanelView.lua:2023-2025), so for a truck attack a template's
//     lower unit only makes this figure lower than the panel's; the KOF train lineup has no such
//     recalculation.
//
// strict is railway-plunder's mode. Power comes only from hero.init.detail.get, and any slot 1..5
// hero it doesn't cover makes the result 0 (unknown), because a template copy can be stale and
// higher than now. The Overlord's stats aren't in that reply, so its soldiers are left out. Without
// strict (truck-deploy, which only ranks squads), the template copies stand in.
func railwayPower(in *Init, sq railwaySquad, stats map[int64]railwayHeroStat, strict bool) int64 {
	soldier := sq.soldier
	if !sq.soldierSet && soldier == 0 {
		var lv int64
		for _, b := range in.Objects("building_new") {
			if id, _ := evNum(b, "bId"); id == railwayMilitaryCamp {
				if l, _ := evNum(b, "lv"); l > lv {
					lv = l
				}
			}
		}
		if top, found := duelTopUnit(lv, duelTrainUnlocksOf(in)); found {
			soldier = top.id
		}
	}
	unit, ok := railwaySoldierStats[soldier]
	eff := func(id string) float64 { f, _ := initEffect(in, id); return f }
	life := (unit.life + eff("50065")) * (1 + eff("50076"))
	atk := (unit.attack + eff("50067")) * (1 + eff("50077"))
	def := (unit.defense + eff("50069")) * (1 + eff("50078"))
	var total int64
	for _, h := range sq.heroes {
		st, known := stats[h.uuid]
		switch {
		case known:
		case strict && h.index == railwayOverlordSlot:
			continue
		case strict:
			return 0
		default:
			st = railwayHeroStat{power: h.power, lead: h.lead}
		}
		if h.index <= 5 {
			total += int64(math.Floor(st.power))
		}
		if n := math.Floor(st.lead); ok && n > 0 {
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
// train.send (action 4, LWMyStationDataManager.lua:323) and train.attack (action 8,
// UIHeroFakePVPFormationPanelView.lua:434-436) (TrySaveTruckFormation, :433-438;
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

// railwayStation is the station state: the fresh train.data reply (GetMyStationDataMessage), whose
// fields sit at the top level, or init trainData. TryGetMyStationData asks for train.data only
// out of the city fog and while a TruckActivity (type 110) is open (LWMyStationDataManager.lua:
// 207-214; the client checks the first one, this any); otherwise the client has only init
// trainData, and so does this. Init trainData is also the fallback when the call fails.
func railwayStation(conn *session.GameConn, in *Init, feature string) *sfs.SFSObject {
	switch {
	case !railwayOutOfFog(in):
		slog.Info(feature + ": the station is in the city fog; train.data not asked for, as the client does")
		return in.Object("trainData")
	case len(Activities(conn, in).Open(actTypeTruckActivity)) == 0:
		slog.Info(feature + ": no open truck activity; train.data not asked for, using the login init as the client does")
		return in.Object("trainData")
	}
	msg, err := session.SendAndWait(conn, feature+" station data", "train.data", sfs.NewSFSObject())
	if err == nil && msg != nil && !msg.Params.Has("errorCode") && msg.Params.Has("myTrainList") {
		return msg.Params
	}
	slog.Info(feature+": train.data gave no station data; using the login init", "err", err)
	return in.Object("trainData")
}

// railwayStationOpen is GetRailwayStationState past FirstReward (LWMyStationDataManager.lua:
// 718-743): out of the fog, the activity started (openTime and now > openTime), and the opening
// reward taken. RefreshMyStationData sets hasFirstReward = (openReward == 0) (:566), so only a
// present 0 holds the station; an absent openReward counts as taken, as in the client. Trucks
// can't leave and the enemy list doesn't open before that.
func railwayStationOpen(in *Init, station *sfs.SFSObject, nowMs int64) bool {
	at, ok := claimInt(station, "openTime")
	open, known := claimInt(station, "openReward")
	return railwayOutOfFog(in) && ok && nowMs > at && (!known || open != 0)
}
