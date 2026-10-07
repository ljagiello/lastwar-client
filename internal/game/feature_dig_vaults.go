package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Dig vaults (MASTER.md §4 #89, #121), opt-in and static-only. Three games share the client's
// DiggingMapData model (DiggingMapData.lua:31-111). A vault is a num_width x num_height grid of
// stones with relics (blocks) under some of them. An info reply carries:
//
//   - uuid, type, uid and mapConfigId;
//   - rewardState: 0 open, 1 a reward to claim, 2 done (DiggingInfoData.lua:48-59);
//   - allianceOpenInfo[{pos, playerInfo{uid}}], the stones opened and by whom;
//   - personalOpenInfo or openInfo[pos], the stones of a single-player vault;
//   - blockInfo[{bid, pos}], the relics revealed so far. A push adds one when a stone on it opens
//     (OffSeasonDigGameOpenPushMessage.lua:35-47).
//
// Positions are 1-based, row-major on num_width (DiggingDataManager.lua:86-93).
//
// The features, one per game, each with a -plan twin:
//
//   - season-dig: the S3 Archaeologist vaults, activity type 241 (id 1000070). The client sends
//     season.dig.activity.info {} once the activity is open (ActivityListDataManager.lua:517-521,
//     DiggingDataManager.lua:24-34), which returns mapList[{uuid, mapConfigId, startTime, endTime,
//     rewardState}]. The client also requires the season map type to be Mummy
//     (DiggingDataManager.lua:39-41). init does not carry the season map type, so this requires
//     only a running season (evSeasonID).
//   - offseason-dig: the Street Rush alliance vault, type 260 (ids 80052-80057). Its list is
//     offseason.dig.activity.info {} (ActivityListDataManager.lua:522-526,
//     OffSeasonDiggingDataManager.lua:13-16).
//   - alliance-boss-dig: the plague-boss vault of Alliance Drill, type 202. The client sends
//     alliance.boss.act.info {serverId: Int} for a listed type-202 activity, with serverId =
//     init user.serverId (ActivityListDataManager.lua:174-176, AllyDrillDataManager.lua:45-62,
//     PlayerInfo.lua:490-491, 761-765). A vault exists while the reply's digGameInfo has a uuid
//     and now <= endTime (AllyDrillDataManager.lua:750-759). Its info is
//     alliance.boss.dig.game.info {}, and the type defaults to alliance
//     (AllianceBossDigGameInfoMessage.lua:13-17). E100172 is the reply when there is no such
//     activity, which the client ignores (AllianceBossActInfoMessage.lua:10-14).
//
// A vault opens only between startTime and endTime (DiggingInfoItem.lua:129-146,
// OffSeasonDiggingInfoItem.lua:135-152). mapConfigId decides the vault kind from
// lw_season_digging_game.type: the season's types 1 and 3 are single-player vaults
// (DiggingInfoData.lua:25-32), and types 2, 10 and 11 are alliance vaults.
//
// Alliance vaults. Each member opens one stone, for free: OpenBrick checks only rewardState == 0
// and no item (DiggingDataManager.lua:203-209, OffSeasonDiggingDataManager.lua:159-173,
// AllyDrillDataManager.lua:760-771). A stone over a relic earns that relic's reward ("Rewards are
// granted if a treasure is found underneath", en parkour_260_desc13).
//
//   - The claim. The client shows it on the relic whose footprint holds the player's own stone,
//     when rewardState == 1. It sends {uuid, pos: that stone} with
//     season.dig.game.get.alliance.block.reward, offseason.dig.game.get.alliance.block.reward or
//     alliance.boss.dig.game.reward (DiggingLevelRewardItem.lua:86-189,
//     OffSeasonDiggingLevelRewardItem.lua:86-189, DiggingLevelRewardItemC.lua:86-189).
//   - The dig. The stone is the player's choice; the client has no auto-dig and no recommended
//     stone. This opens an unopened stone of a revealed relic, a sure hit, and picks the relic
//     with the fewest stones left. With no revealed relic it opens the first unopened stone. It
//     sends season.dig.game.open {type, uuid, uid, pos}, offseason.dig.game.open {uuid, pos} or
//     alliance.boss.dig.game.open {uuid, pos} (SeasonDigGameOpenMessage.lua:3-9,
//     OffSeasonDigGameOpenMessage.lua:3-7, AllianceBossDigGameOpenMessage.lua:3-7).
//   - After the dig. The open reply is empty; the result comes as a push.*.dig.game.open. So the
//     run reads the vault again and claims if the dig made rewardState 1.
//
// Season single-player vaults: only the claim, season.dig.game.get.personal.reward {uuid}. It is
// sent when the info shows rewardState 1 and uid is the player's own (DiggingLevelSingleView.lua:
// 254-271). Their stones cost an owned Archaeologist Hammer each (activity para_1, item 650054;
// DiggingDataManager.lua:24-32, 188-202). Which stone to break is the player's call, with a confirm
// per stone and no client auto-dig. Hammers also help allies' vaults, and the relics of a fresh
// vault are not revealed. So spending hammers is left to the player. Helping an ally's vault
// starts from a chat share (DiggingGameShare.lua:20) and is not done either.
//
// The season info and open requests carry type and uid. info sends the list type with the
// player's uid, as DiggingInfoItem.lua:127 does through SeasonDigGameInfoMessage.lua:4-9. open
// sends the info reply's type and uid (DiggingMapBrick.lua:103), falling back to the list type and
// the player's uid when the reply has none.
const (
	seasonDigListCmd      = "season.dig.activity.info"
	seasonDigInfoCmd      = "season.dig.game.info"
	seasonDigOpenCmd      = "season.dig.game.open"
	seasonDigPersonalCmd  = "season.dig.game.get.personal.reward"
	seasonDigBlockCmd     = "season.dig.game.get.alliance.block.reward"
	offSeasonDigListCmd   = "offseason.dig.activity.info"
	offSeasonDigInfoCmd   = "offseason.dig.game.info"
	offSeasonDigOpenCmd   = "offseason.dig.game.open"
	offSeasonDigBlockCmd  = "offseason.dig.game.get.alliance.block.reward"
	allianceBossActCmd    = "alliance.boss.act.info"
	allianceBossInfoCmd   = "alliance.boss.dig.game.info"
	allianceBossOpenCmd   = "alliance.boss.dig.game.open"
	allianceBossRewardCmd = "alliance.boss.dig.game.reward"
	allianceBossNoActCode = "E100172"

	digVaultSingle   = 1 // SeasonDigGameType (EnumType.lua:15051)
	digVaultAlliance = 2
	digVaultMaxMaps  = 50
)

// digVaultMap is a vault layout from lw_season_digging_game (table 39432).
type digVaultMap struct {
	alliance      bool
	width, height int64
}

// digVaultMapOf returns the layout of mapConfigId, for the map groups these games use: season
// 1000 (type 1), 1001 (type 2) and 1002 (type 3), off-season 1501 (type 10) and plague boss 1601
// (type 11).
func digVaultMapOf(id int64) (digVaultMap, bool) {
	switch {
	case id >= 10001 && id <= 10042, id >= 10053 && id <= 10062:
		return digVaultMap{alliance: false, width: 4, height: 7}, true
	case id >= 10043 && id <= 10052, id >= 15001 && id <= 15010, id >= 16001 && id <= 16010:
		return digVaultMap{alliance: true, width: 10, height: 12}, true
	}
	return digVaultMap{}, false
}

// digVaultBlockSize is lw_season_block size_width x size_height (table 39432) for the relics of
// the alliance vaults: 10011-10020 (season), 60011-60020 (off-season), 70001-70010 (plague boss).
var digVaultBlockSize = map[int64][2]int64{
	10011: {2, 2}, 10012: {1, 2}, 10013: {1, 1}, 10014: {4, 1}, 10015: {3, 2},
	10016: {1, 4}, 10017: {2, 3}, 10018: {3, 3}, 10019: {3, 4}, 10020: {3, 4},
	60011: {2, 2}, 60012: {1, 2}, 60013: {1, 1}, 60014: {4, 1}, 60015: {3, 2},
	60016: {1, 4}, 60017: {2, 3}, 60018: {3, 3}, 60019: {3, 4}, 60020: {3, 4},
	70001: {1, 1}, 70002: {1, 2}, 70003: {2, 2}, 70004: {1, 4}, 70005: {4, 1},
	70006: {3, 2}, 70007: {2, 3}, 70008: {3, 3}, 70009: {3, 4}, 70010: {3, 4},
}

// digVaultGame is one vault game's commands.
type digVaultGame struct {
	name       string // feature name
	infoCmd    string
	openCmd    string
	blockCmd   string
	infoParams func(l digVaultListed, me string) *sfs.SFSObject
	openParams func(l digVaultListed, v *digVault, me string, pos int64) *sfs.SFSObject
}

// digVaultListed is a vault from a list reply (or the plague boss's digGameInfo).
type digVaultListed struct {
	uuid, mapConfigID int64
	start, end        int64 // ms; 0 = no bound
	rewardState       int64
	hasState          bool
}

// digVault is one info reply.
type digVault struct {
	uuid, typ, mapConfigID, rewardState int64
	uid                                 string
	layout                              digVaultMap
	opened                              map[int64]string // pos -> opener uid
	blocks                              []digVaultBlock  // the first blockInfo entry per bid
}

type digVaultBlock struct{ bid, pos int64 }

func parseDigVault(reply *sfs.SFSObject) (*digVault, error) {
	cfgID, ok := claimInt(reply, "mapConfigId")
	if !ok {
		return nil, fmt.Errorf("no mapConfigId")
	}
	layout, ok := digVaultMapOf(cfgID)
	if !ok {
		return nil, fmt.Errorf("mapConfigId %d is not a known vault map", cfgID)
	}
	v := &digVault{mapConfigID: cfgID, layout: layout, opened: map[int64]string{}}
	v.uuid, _ = claimInt(reply, "uuid")
	v.typ, _ = claimInt(reply, "type")
	v.uid = evString(reply, "uid")
	v.rewardState, _ = claimInt(reply, "rewardState")
	for _, key := range []string{"personalOpenInfo", "openInfo"} {
		for _, pos := range claimInts(reply, key) {
			v.opened[pos] = ""
		}
	}
	for _, o := range claimObjectsOrMap(reply, "allianceOpenInfo") {
		if pos, ok := claimInt(o, "pos"); ok {
			v.opened[pos] = evString(evObject(o, "playerInfo"), "uid")
		}
	}
	seen := map[int64]bool{}
	for _, b := range claimObjectsOrMap(reply, "blockInfo") {
		bid, ok1 := claimInt(b, "bid")
		pos, ok2 := claimInt(b, "pos")
		if !ok1 || !ok2 || seen[bid] { // GetBlock takes the first entry per bid (DiggingDataManager.lua:94-101)
			continue
		}
		seen[bid] = true
		v.blocks = append(v.blocks, digVaultBlock{bid, pos})
	}
	return v, nil
}

// cells is a relic's footprint on the grid (GetBrickListByBlock, DiggingDataManager.lua:116-133);
// nil for a relic of unknown size.
func (v *digVault) cells(b digVaultBlock) []int64 {
	size, ok := digVaultBlockSize[b.bid]
	if !ok || b.pos < 1 {
		return nil
	}
	w := v.layout.width
	x, y := (b.pos-1)%w, (b.pos-1)/w
	var out []int64
	for j := y; j < y+size[1] && j < v.layout.height; j++ {
		for i := x; i < x+size[0] && i < w; i++ {
			out = append(out, j*w+i+1)
		}
	}
	return out
}

// ownStone is the stone the player opened, or 0.
func (v *digVault) ownStone(me string) int64 {
	var best int64
	for pos, uid := range v.opened {
		if uid != "" && uid == me && (best == 0 || pos < best) {
			best = pos
		}
	}
	return best
}

// claimPos is the player's own stone when it lies on a revealed relic (selfPos in
// DiggingLevelRewardItem:RefreshHead), or 0.
func (v *digVault) claimPos(me string) int64 {
	own := v.ownStone(me)
	if own == 0 {
		return 0
	}
	for _, b := range v.blocks {
		for _, c := range v.cells(b) {
			if c == own {
				return own
			}
		}
	}
	return 0
}

// pickStone chooses the stone to open: an unopened stone of the revealed relic with the fewest
// left, else the first unopened stone. pos is 0 when every stone is open.
func (v *digVault) pickStone() (pos int64, why string) {
	bestLeft := int64(-1)
	for _, b := range v.blocks {
		var left []int64
		for _, c := range v.cells(b) {
			if _, open := v.opened[c]; !open {
				left = append(left, c)
			}
		}
		if len(left) > 0 && (bestLeft < 0 || int64(len(left)) < bestLeft) {
			bestLeft, pos = int64(len(left)), left[0]
			why = fmt.Sprintf("unopened stone of revealed relic %d (%d left)", b.bid, len(left))
		}
	}
	if pos != 0 {
		return pos, why
	}
	for p := int64(1); p <= v.layout.width*v.layout.height; p++ {
		if _, open := v.opened[p]; !open {
			return p, "no revealed relic has an unopened stone: first unopened stone"
		}
	}
	return 0, ""
}

var (
	seasonDigGame = &digVaultGame{
		name:     "season-dig",
		infoCmd:  seasonDigInfoCmd,
		openCmd:  seasonDigOpenCmd,
		blockCmd: seasonDigBlockCmd,
		infoParams: func(l digVaultListed, me string) *sfs.SFSObject {
			p := sfs.NewSFSObject()
			p.PutInt("type", seasonDigListType(l))
			p.PutLong("uuid", l.uuid)
			p.PutUtfString("uid", me)
			return p
		},
		openParams: func(l digVaultListed, v *digVault, me string, pos int64) *sfs.SFSObject {
			typ, uid := int32(v.typ), v.uid
			if typ <= 0 {
				typ = seasonDigListType(l)
			}
			if uid == "" {
				uid = me
			}
			p := sfs.NewSFSObject()
			p.PutInt("type", typ)
			p.PutLong("uuid", v.uuid)
			p.PutUtfString("uid", uid)
			p.PutInt("pos", int32(pos))
			return p
		},
	}
	offSeasonDigGame = &digVaultGame{
		name:     "offseason-dig",
		infoCmd:  offSeasonDigInfoCmd,
		openCmd:  offSeasonDigOpenCmd,
		blockCmd: offSeasonDigBlockCmd,
		infoParams: func(l digVaultListed, _ string) *sfs.SFSObject {
			p := sfs.NewSFSObject()
			p.PutLong("uuid", l.uuid)
			return p
		},
		openParams: digVaultUUIDPos,
	}
	allianceBossDigGame = &digVaultGame{
		name:       "alliance-boss-dig",
		infoCmd:    allianceBossInfoCmd,
		openCmd:    allianceBossOpenCmd,
		blockCmd:   allianceBossRewardCmd,
		infoParams: func(digVaultListed, string) *sfs.SFSObject { return sfs.NewSFSObject() },
		openParams: digVaultUUIDPos,
	}
)

// seasonDigListType is the SeasonDigGameType of a listed season vault.
func seasonDigListType(l digVaultListed) int32 {
	if m, ok := digVaultMapOf(l.mapConfigID); ok && m.alliance {
		return digVaultAlliance
	}
	return digVaultSingle
}

func digVaultUUIDPos(_ digVaultListed, v *digVault, _ string, pos int64) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutLong("uuid", v.uuid)
	p.PutInt("pos", int32(pos))
	return p
}

func init() {
	for _, f := range []struct {
		name, summary string
		run           func(*session.GameConn, *Init, bool) error
	}{
		{"season-dig", "OPT-IN: S3 Archaeologist vaults: open the free alliance-vault stone and claim alliance-relic " +
			"and personal-vault rewards (never spends hammers); static-only", runSeasonDigs},
		{"offseason-dig", "OPT-IN: off-season Street Rush alliance vaults: open the free stone and claim the relic reward; static-only",
			runOffSeasonDigs},
		{"alliance-boss-dig", "OPT-IN: plague-boss vault (Alliance Drill): open the free stone and claim the relic reward; static-only",
			runAllianceBossDigs},
	} {
		registerFeature(Feature{Name: f.name, Summary: f.summary,
			Run: func(conn *session.GameConn, in *Init) error { return f.run(conn, in, false) }})
		registerFeature(Feature{Name: f.name + "-plan",
			Summary: "read-only: log the stones " + f.name + " would open and the rewards it would claim; sends only info requests",
			Run:     func(conn *session.GameConn, in *Init) error { return f.run(conn, in, true) }})
	}
	session.RegisterBenignErrorCode(allianceBossNoActCode, allianceBossActCmd)
}

func digVaultFeature(name string, dry bool) string {
	if dry {
		return name + "-plan"
	}
	return name
}

// digVaultListedFrom reads a mapList entry.
func digVaultListedFrom(o *sfs.SFSObject) (digVaultListed, bool) {
	var l digVaultListed
	var ok bool
	if l.uuid, ok = claimInt(o, "uuid"); !ok || l.uuid == 0 {
		return l, false
	}
	l.mapConfigID, _ = claimInt(o, "mapConfigId")
	l.start, _ = claimInt(o, "startTime")
	l.end, _ = claimInt(o, "endTime")
	l.rewardState, l.hasState = claimInt(o, "rewardState")
	return l, true
}

// runVaultList runs a list-based game: list, then each vault in it.
func runVaultList(conn *session.GameConn, in *Init, g *digVaultGame, listCmd string, dry bool) error {
	feature := digVaultFeature(g.name, dry)
	me := ownUID(in)
	if me == "" {
		slog.Info(feature + ": init has no user.uid; skipping")
		return nil
	}
	r := newDigsRun(conn, in, feature, dry)
	list := r.read(feature+" vault list", listCmd, sfs.NewSFSObject())
	if list == nil {
		return r.err()
	}
	if !list.Has("mapList") {
		slog.Info(feature+": list reply carries no mapList; treating as no vaults", "reply", list.StringRedacted())
		return r.err()
	}
	maps := claimObjectsOrMap(list, "mapList")
	slog.Info(feature+": vaults listed", "count", len(maps))
	for i, o := range maps {
		if r.halt {
			break
		}
		if i >= digVaultMaxMaps {
			slog.Warn(feature+": vault cap reached; leaving the rest for the next run", "cap", digVaultMaxMaps)
			break
		}
		l, ok := digVaultListedFrom(o)
		if !ok {
			continue
		}
		g.process(r, l, me, true)
	}
	return r.err()
}

func runSeasonDigs(conn *session.GameConn, in *Init, dry bool) error {
	feature := digVaultFeature("season-dig", dry)
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if evSeasonID(in) <= 0 {
		slog.Info(feature + ": not in a season; skipping")
		return nil
	}
	if len(Activities(conn, in).Open(actTypeDiggingGame)) == 0 {
		slog.Info(feature + ": the season dig activity is not open")
		return nil
	}
	return runVaultList(conn, in, seasonDigGame, seasonDigListCmd, dry)
}

func runOffSeasonDigs(conn *session.GameConn, in *Init, dry bool) error {
	feature := digVaultFeature("offseason-dig", dry)
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if len(Activities(conn, in).Open(actTypeOffSeasonDig)) == 0 {
		slog.Info(feature + ": the off-season dig activity is not open")
		return nil
	}
	return runVaultList(conn, in, offSeasonDigGame, offSeasonDigListCmd, dry)
}

func runAllianceBossDigs(conn *session.GameConn, in *Init, dry bool) error {
	feature := digVaultFeature("alliance-boss-dig", dry)
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if !claimInAlliance(in) {
		slog.Info(feature + ": not in an alliance; skipping")
		return nil
	}
	now := evNow().UnixMilli()
	listed := false
	for _, a := range Activities(conn, in).All(actTypeAllyDrill) {
		if now >= a.StartTime()-activityOpenToleranceMs && now <= max(a.EndTime(), a.EndViewTime())+activityOpenToleranceMs {
			listed = true
		}
	}
	if !listed {
		slog.Info(feature + ": no Alliance Drill activity running")
		return nil
	}
	me := ownUID(in)
	server, ok := claimInt(in.Object("user"), "serverId")
	if me == "" || !ok || server <= 0 {
		slog.Info(feature + ": init has no user.uid or user.serverId; skipping")
		return nil
	}
	r := newDigsRun(conn, in, feature, dry)
	p := sfs.NewSFSObject()
	p.PutInt("serverId", int32(server))
	act := r.read(feature+" alliance boss info", allianceBossActCmd, p)
	if act == nil {
		return r.err()
	}
	dg := evObject(act, "digGameInfo")
	if dg == nil {
		slog.Info(feature + ": no plague-boss vault (no digGameInfo)")
		return r.err()
	}
	l, ok := digVaultListedFrom(dg)
	if !ok {
		slog.Info(feature + ": digGameInfo has no uuid; no vault")
		return r.err()
	}
	if l.end > 0 && now > l.end {
		slog.Info(feature+": the plague-boss vault has ended", "uuid", l.uuid)
		return r.err()
	}
	l.start = 0 // HasDigGame checks only the end
	allianceBossDigGame.process(r, l, me, false)
	return r.err()
}

// process handles one vault: claim what is due, else open the free stone and claim if it hit.
// strictEnd is the list games' now < endTime (IsLock); the plague boss checks now <= endTime
// itself.
func (g *digVaultGame) process(r *digsRun, l digVaultListed, me string, strictEnd bool) {
	now := evNow().UnixMilli()
	if (l.start > 0 && now < l.start) || (strictEnd && l.end > 0 && now >= l.end) {
		slog.Info(r.feature+": vault outside its open window", "uuid", l.uuid)
		return
	}
	if m, ok := digVaultMapOf(l.mapConfigID); ok && !m.alliance {
		g.personal(r, l, me)
		return
	}
	if l.hasState && l.rewardState == 2 {
		slog.Info(r.feature+": vault already finished", "uuid", l.uuid)
		return
	}
	v := g.read(r, l, me)
	if v == nil {
		return
	}
	if !v.layout.alliance {
		g.personal(r, l, me)
		return
	}
	own := v.ownStone(me)
	slog.Info(r.feature+": vault", "uuid", v.uuid, "mapConfigId", v.mapConfigID, "rewardState", v.rewardState,
		"stonesOpen", len(v.opened), "relicsRevealed", len(v.blocks), "ownStone", own)
	switch {
	case v.rewardState == 1:
		g.claim(r, v, me)
	case v.rewardState == 0 && own == 0:
		pos, why := v.pickStone()
		if pos == 0 {
			slog.Info(r.feature+": every stone is open; nothing to dig", "uuid", v.uuid)
			return
		}
		label := fmt.Sprintf("%s vault %d stone %d (%s)", r.feature, v.uuid, pos, why)
		if _, res := r.write(label, g.openCmd, g.openParams(l, v, me, pos)); res != digsDone || r.dry {
			return
		}
		after := g.read(r, l, me)
		if after == nil {
			return
		}
		if after.rewardState == 1 {
			g.claim(r, after, me)
		} else {
			slog.Info(r.feature+": stone opened; no relic reward to claim", "uuid", after.uuid, "pos", pos,
				"rewardState", after.rewardState)
		}
	}
}

// read sends the game's info request and parses it; nil when it failed or held no vault.
func (g *digVaultGame) read(r *digsRun, l digVaultListed, me string) *digVault {
	reply := r.read(fmt.Sprintf("%s vault %d info", r.feature, l.uuid), g.infoCmd, g.infoParams(l, me))
	if reply == nil {
		return nil
	}
	v, err := parseDigVault(reply)
	if err != nil {
		slog.Info(r.feature+": vault info carries no usable map; skipping", "uuid", l.uuid, "reason", err,
			"reply", reply.StringRedacted())
		return nil
	}
	if v.uuid == 0 {
		v.uuid = l.uuid
	}
	return v
}

// claim sends the relic claim for the player's own stone.
func (g *digVaultGame) claim(r *digsRun, v *digVault, me string) {
	pos := v.claimPos(me)
	if pos == 0 {
		slog.Info(r.feature+": rewardState is 1 but the own stone is not on a revealed relic; the client shows no claim",
			"uuid", v.uuid)
		return
	}
	p := sfs.NewSFSObject()
	p.PutLong("uuid", v.uuid)
	p.PutInt("pos", int32(pos))
	r.write(fmt.Sprintf("%s vault %d relic reward (stone %d)", r.feature, v.uuid, pos), g.blockCmd, p)
}

// personal claims a season single-player vault's reward.
func (g *digVaultGame) personal(r *digsRun, l digVaultListed, me string) {
	if g != seasonDigGame {
		return
	}
	if !l.hasState || l.rewardState != 1 {
		return
	}
	v := g.read(r, l, me)
	if v == nil {
		return
	}
	if v.rewardState != 1 || v.uid != "" && v.uid != me {
		slog.Info(r.feature+": personal vault not claimable", "uuid", v.uuid, "rewardState", v.rewardState)
		return
	}
	p := sfs.NewSFSObject()
	p.PutLong("uuid", v.uuid)
	r.write(fmt.Sprintf("%s personal vault %d reward", r.feature, v.uuid), seasonDigPersonalCmd, p)
}
