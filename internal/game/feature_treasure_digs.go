package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Treasure digs (MASTER.md §4 #80): the three hammer-and-brick dig games of 1.0.364 and the
// Secret Vault chest track. Every board is a num_width x num_height grid of bricks numbered 1..w*h
// row by row (DiggingDataManager.lua:86-93); gems ("blocks") lie under them, and a block is
// uncovered when every brick of its size_width x size_height area is open (CheckBlockGet,
// :134-148). One open costs one owned hammer (Treasure_map_hummer k2 and early_digging_hummer
// k2 = 1, table item). The boards:
//
//   - Secret Vault (DigTreasureManager; activity type 252, ids 2200011-2200015 and 2200021, Dig
//     Hammer 771030 = Treasure_map_hummer k1). State is `hero.dispatch.get.dig.game.info {}`, which
//     the client sends at login while the activity is open (InitMessage.lua:695;
//     DigTreasureManager.lua:13-24) and on opening the vault (UIDigTreasureView.lua:15). The reply
//     carries levelMap (the stage), timeLimitMap (a timed stage), mapBoxList (the stage chests
//     {mapConfigId, rewardState}), helpInfo and isHammerGetMax (:86-112). A timed stage that isn't
//     HaveGot, or whose endTime hasn't passed, replaces the stage (IsHaveTimeLimitMap, :140-152):
//       - NotBegin: the client asks "Start the challenge? No penalty for failure." and on confirm
//         sends `hero.dispatch.get.time.game.hammer {uuid}`, whose reply grants the stage's
//         hammers (hammer_num, 25) and the started map (:385-396; DigTreasureGetTimeGameHammer
//         Message.lua). This starts it only when the hammers held plus that grant cover every
//         brick, so the 180 s stage cannot fail; otherwise it logs and leaves the vault alone;
//       - CanGet: `hero.dispatch.get.time.dig.game.reward {uuid}` (UIDigTreasureView.lua:470-485);
//       - Fail, or CanNotGet past endTime: `hero.dispatch.dig.game.verify.fail {uuid}`, the
//         "Time's up ... Tap to return" mask (:765-792), which clears it. give.up is never sent.
//     A brick is `hero.dispatch.dig.game.open.block {uuid, pos, checkOpenAll}` (DigTreasureGame
//     OpenBlockMessage.lua:4-12). On a stage the client first marks pos in its own open set and
//     sends checkOpenAll 1 when that set would uncover every block in blockInfo, else 0
//     (DigTreasureManager.lua:185-228); a reply carrying checkOpenAll makes it re-read the info
//     (DigTreasureGameOpenBlockMessage.lua:19-25). The reply's newLevelMap / newTimeLimitMap
//     replace the boards (:69-80). A stage chest is `hero.dispatch.get.level.dig.game.reward {}`
//     while a mapBoxList entry is CanGet (UIDigTreasureView.lua:451-469); its reply carries
//     mapBoxList and newMap (DigTreasureGameGetRewardMessage.lua).
//   - Secret Vault chest track (DigTreasureBoxRewardManager; activity type 261, ids 2200030-2200033):
//     `hero.dispatch.treasure.v2.get.info {}` returns rewardList {target, isReward, ...}; the
//     client sends `hero.dispatch.treasure.v2.reward {}` for a chest with isReward != 1 and
//     target <= the count of the activity's para_3 item, 771051 (boxRewardComponent.lua:47-53;
//     DigTreasureBoxRewardManager.lua:16-58; DispatchTreasure.lua:449-456).
//   - City ruin (BuildingDigTreasureManager; building 10226000, the only one whose bubble or tap
//     opens it, BuildBubbleManager.lua:880-881, 3967-3968, UIUtil.lua:2911-2912; Explorer's Hammer
//     771050 = early_digging_hummer k1): init building_new[i].buildingDigGame {gameInfo,
//     mapBoxList} (BuildingDate.lua:219-223). Opening the ruin sends `building.dig.game.receive.hammer
//     {buildingUuid}` when gameInfo.rewardState is NotBegin (UIBuildingDigTreasureView.lua:761-765;
//     BuildingDigTreasureManager.lua:19-25); a brick is `building.dig.game.open.block
//     {buildingUuid, pos}` (BuildingDigGameOpenBlockMessage.lua:4-8), whose reply's newGameInfo
//     is the next stage (:60-63); a chest is `building.get.dig.game.reward {buildingUuid}` while a
//     mapBoxList entry is CanGet (UIBuildingDigTreasureView.lua:400-408).
//   - Radar ruin (DetectDigTreasureManager; detect_event types 30 and 39, also hammer 771050): the
//     event's digGameInfo in get.detect.info (DetectEventInfo.lua:106-110). The client opens it
//     from the event's world point (UIWorldPointBtn.lua:2552-2556), placing an event in state 3
//     first with `detect.event.put.point.in.world {uuid}` (UIDetectEventCtrl.lua:51-58). Then
//     `detect.event.dig.game.receive.hammer {eventUuid}` when NotBegin (UIDetectDigTreasureView.lua:
//     503-507; its maps grant hammer_num = w*h), `detect.event.dig.game.open.block {eventUuid,
//     pos}`, and `detect.event.get.dig.game.reward {eventUuid}` when CanGet (:331-340).
//
// The free hammer's gate is the board's rewardState == NotBegin, not isHammerGetMax: that flag is
// the daily cap on hammers received from allies and only gates asking for help
// (DigTreasureManager.lua:244-249, 290-313; PushDigTreasureGetHammerMessage.lua).
//
// Which brick: none of these games has an auto-dig; the player taps any closed brick
// (DigMapBrick.lua:78-83 -> OpenBrick(self.index)). This picks, by its own rule, the lowest closed
// brick of a block the player can see (one of the block's bricks already open) that isn't
// uncovered yet, else the lowest closed brick. Block positions the server sends for blocks with no
// open brick are not used. A brick is opened only while a hammer is held: the ledger starts at the
// init items count (ItemData:GetItemById, ItemData.lua:38-47), drops by one per open, and follows
// the hammer totals the replies grant.
//
// A radar ruin is a radar task, which scores in the Alliance Duel (90402, Mon/Wed/Fri) when it is
// claimed (confirmed live for radar tasks, 2026-10-07). So the chest is claimed only on a day whose
// duel scores radar tasks, like radar-claims. No other dig action is in the score table.
//
// Every request goes through an allowlist of commands and their keys (digCheckParams): no buy,
// exchange (hero.dispatch.hammer.exchange), help or share command, and no gold key, is ever sent.
// None of the replies above carries a diamond balance, so there is no tripwire. At most
// digMaxOpens bricks and digMaxClaims claims per run. treasure-digs-plan sends only the info
// reads and logs the rest. Static-only: not sent live yet.

//go:generate go run ./gendig -maps ${LASTWAR_TABLES}/lw_season_digging_game.json -blocks ${LASTWAR_TABLES}/lw_season_block.json -version ${LASTWAR_TABLE_VERSION} -out dig_tables_gen.go

const (
	digVaultInfoCmd       = "hero.dispatch.get.dig.game.info"
	digVaultOpenCmd       = "hero.dispatch.dig.game.open.block"
	digVaultStageChestCmd = "hero.dispatch.get.level.dig.game.reward"
	digVaultTimedStartCmd = "hero.dispatch.get.time.game.hammer"
	digVaultTimedChestCmd = "hero.dispatch.get.time.dig.game.reward"
	digVaultTimedFailCmd  = "hero.dispatch.dig.game.verify.fail"
	digTrackInfoCmd       = "hero.dispatch.treasure.v2.get.info"
	digTrackChestCmd      = "hero.dispatch.treasure.v2.reward"
	digCityHammerCmd      = "building.dig.game.receive.hammer"
	digCityOpenCmd        = "building.dig.game.open.block"
	digCityChestCmd       = "building.get.dig.game.reward"
	digRadarPlaceCmd      = "detect.event.put.point.in.world"
	digRadarHammerCmd     = "detect.event.dig.game.receive.hammer"
	digRadarOpenCmd       = "detect.event.dig.game.open.block"
	digRadarChestCmd      = "detect.event.get.dig.game.reward"

	digVaultHammer  = "771030" // Dig Hammer (Treasure_map_hummer k1)
	digRuinHammer   = "771050" // Explorer's Hammer (early_digging_hummer k1)
	digTrackScore   = "771051" // the chest track's score item (activity 2200030-2200033 para_3)
	digCityBuilding = 10226000 // BuildingTypes.LW_BUILD_DIG_GAME (EnumType.lua:3764)
	// detect_event.type of the radar ruins (image Detect_Event_DigGame): 30, and 39 for ids
	// 501001-501105.
	digRadarTypeRuin  = 30
	digRadarTypeRuin2 = 39

	// DigRewardState (EnumType.lua:16973-16979).
	digCanNotGet int64 = 0
	digCanGet    int64 = 1
	digHaveGot   int64 = 2
	digFail      int64 = 3
	digNotBegin  int64 = 4

	digMaxOpens  = 150
	digMaxClaims = 40
	digMaxRounds = 12
	// digTapPause spaces the opens like taps on the board.
	digTapPause = 300 * time.Millisecond
)

// digVaultActivities and digTrackActivities are the type-252 (DigTreasure) and type-261
// (OFF_SEASON_Treasure_V2) rows of table activity; neither has a needMainCityLevel.
var (
	digVaultActivities = []int32{2200011, 2200012, 2200013, 2200014, 2200015, 2200021}
	digTrackActivities = []int32{2200030, 2200031, 2200032, 2200033}
)

const (
	actTypeDigTreasure   int32 = 252
	actTypeDigTreasureV2 int32 = 261
)

// digSleep is the pause between opens; tests replace it.
var digSleep = time.Sleep

// digLevel is one lw_season_digging_game row (dig_tables_gen.go).
type digLevel struct {
	w, h, typ, hammers int
}

// digBlockSize is one lw_season_block row's size in bricks (dig_tables_gen.go).
type digBlockSize struct {
	w, h int
}

// digAllowedParams is every command this feature sends and the only keys its params may carry.
var digAllowedParams = map[string][]string{
	digVaultInfoCmd:       nil,
	digVaultOpenCmd:       {"uuid", "pos", "checkOpenAll"},
	digVaultStageChestCmd: nil,
	digVaultTimedStartCmd: {"uuid"},
	digVaultTimedChestCmd: {"uuid"},
	digVaultTimedFailCmd:  {"uuid"},
	digTrackInfoCmd:       nil,
	digTrackChestCmd:      nil,
	digCityHammerCmd:      {"buildingUuid"},
	digCityOpenCmd:        {"buildingUuid", "pos"},
	digCityChestCmd:       {"buildingUuid"},
	radarInfoCmd:          {"openWnd"},
	digRadarPlaceCmd:      {"uuid"},
	digRadarHammerCmd:     {"eventUuid"},
	digRadarOpenCmd:       {"eventUuid", "pos"},
	digRadarChestCmd:      {"eventUuid"},
}

// digForbidden are fragments no command or key this feature sends may contain: purchases, the
// hammer exchange, gifts to allies, chat shares and diamonds.
var digForbidden = []string{"buy", "gold", "diamond", "exchange", "give", "share", "cost", "pay"}

// digCheckParams refuses a request outside digAllowedParams or touching digForbidden.
func digCheckParams(cmd string, p *sfs.SFSObject) error {
	allowed, ok := digAllowedParams[cmd]
	if !ok {
		return fmt.Errorf("treasure-digs: refusing %s: not a command this feature sends", cmd)
	}
	for _, f := range digForbidden {
		if strings.Contains(strings.ToLower(cmd), f) {
			return fmt.Errorf("treasure-digs: refusing %s: it contains %q", cmd, f)
		}
	}
	if p == nil {
		return nil
	}
	for _, k := range p.Keys() {
		if !slices.Contains(allowed, k) {
			return fmt.Errorf("treasure-digs: refusing %s: key %q is not allowed", cmd, k)
		}
		for _, f := range digForbidden {
			if strings.Contains(strings.ToLower(k), f) {
				return fmt.Errorf("treasure-digs: refusing %s: key %q contains %q", cmd, k, f)
			}
		}
	}
	return nil
}

func init() {
	registerFeature(Feature{
		Name: "treasure-digs",
		Summary: "opt-in, SPENDS OWNED HAMMERS: claim free dig hammers, open bricks with owned hammers only and claim the chests " +
			"of the Secret Vault, the city and radar ruins and the vault chest track; never buys, exchanges or gives hammers",
		// A radar ruin is a radar task (score 90402); its chest is held on days that don't score
		// radar tasks, so the feature itself runs every day.
		Duel: []DuelScore{{Type: DuelScoreRadarTask}},
		Run:  runTreasureDigs,
	})
	registerFeature(Feature{
		Name:    "treasure-digs-plan",
		Summary: "read-only: read the dig boards and log the hammers, bricks and chests treasure-digs would claim and open",
		Run:     runTreasureDigsPlan,
	})
}

// digBlock is one blockInfo entry: a gem's id, its top-left brick as the client holds it (0 when
// not sent), and at, that brick as far as known, filled from openBlockInfo too.
type digBlock struct {
	bid     int32
	pos, at int
}

// digMap is one board (DiggingMapData.lua:31-111).
type digMap struct {
	uuid    int64
	cfg     int32
	lvl     digLevel
	known   bool // cfg is in the table
	state   int64
	endTime int64
	opened  map[int]bool // open bricks: personalOpenInfo, openInfo, allianceOpenInfo[].pos
	check   map[int]bool // the vault's checkOpenAll set (brickDicCheck)
	blocks  []digBlock
}

// parseDigMap reads a board. The client's open set is personalOpenInfo, else openInfo, plus
// allianceOpenInfo[].pos, and its brickDicCheck is openInfo only (DiggingMapData.lua:47-72).
func parseDigMap(o *sfs.SFSObject) *digMap {
	if o == nil {
		return nil
	}
	m := &digMap{opened: map[int]bool{}, check: map[int]bool{}}
	m.uuid, _ = claimInt(o, "uuid")
	cfg, _ := claimInt(o, "mapConfigId")
	m.cfg = int32(cfg)
	m.lvl, m.known = digLevels[m.cfg]
	m.state, _ = claimInt(o, "rewardState")
	m.endTime, _ = claimInt(o, "endTime")
	if o.Has("personalOpenInfo") {
		for _, p := range evNums(o, "personalOpenInfo") {
			m.opened[int(p)] = true
		}
	} else {
		for _, p := range evNums(o, "openInfo") {
			m.opened[int(p)] = true
			m.check[int(p)] = true
		}
	}
	for _, a := range evObjects(o, "allianceOpenInfo") {
		if p, ok := claimInt(a, "pos"); ok {
			m.opened[int(p)] = true
		}
	}
	for _, b := range evObjects(o, "blockInfo") {
		m.addBlock(b)
	}
	return m
}

// addBlock records a blockInfo or openBlockInfo entry. Like the open replies' handlers, a known
// block keeps the position the client holds and an unknown one is added as sent
// (DigTreasureGameOpenBlockMessage.lua:33-46); at is filled either way.
func (m *digMap) addBlock(o *sfs.SFSObject) {
	bid, ok := claimInt(o, "bid")
	if !ok {
		return
	}
	pos, _ := claimInt(o, "pos")
	for i := range m.blocks {
		if m.blocks[i].bid == int32(bid) {
			if m.blocks[i].at == 0 {
				m.blocks[i].at = int(pos)
			}
			return
		}
	}
	m.blocks = append(m.blocks, digBlock{bid: int32(bid), pos: int(pos), at: int(pos)})
}

// bricks returns the bricks a block of id bid at brick pos covers, or nil when the position or
// size is unknown or the block runs off the board.
func (m *digMap) bricks(bid int32, pos int) []int {
	size, ok := digBlockSizes[bid]
	if !ok || pos <= 0 || pos > m.lvl.w*m.lvl.h {
		return nil
	}
	x, y := (pos-1)%m.lvl.w, (pos-1)/m.lvl.w
	if x+size.w > m.lvl.w || y+size.h > m.lvl.h {
		return nil
	}
	var out []int
	for j := y; j < y+size.h; j++ {
		for i := x; i < x+size.w; i++ {
			out = append(out, j*m.lvl.w+i+1)
		}
	}
	return out
}

// covered reports whether every brick of the block at the client's position is in set
// (CheckBlockGet: false without a position).
func (m *digMap) covered(b digBlock, set map[int]bool) bool {
	bs := m.bricks(b.bid, b.pos)
	if len(bs) == 0 {
		return false
	}
	for _, p := range bs {
		if !set[p] {
			return false
		}
	}
	return true
}

// nextBrick is the brick to open: the lowest closed brick of a visible, not yet uncovered block,
// else the lowest closed brick; 0 when every brick is open.
func (m *digMap) nextBrick() int {
	best := 0
	for _, b := range m.blocks {
		bs := m.bricks(b.bid, b.at)
		visible := slices.ContainsFunc(bs, func(p int) bool { return m.opened[p] })
		if !visible {
			continue
		}
		for _, p := range bs {
			if !m.opened[p] && (best == 0 || p < best) {
				best = p
			}
		}
	}
	if best != 0 {
		return best
	}
	for p := 1; p <= m.lvl.w*m.lvl.h; p++ {
		if !m.opened[p] {
			return p
		}
	}
	return 0
}

// checkOpenAll is the vault stage's checkOpenAll for opening pos: the client adds pos to its
// check set, then sends 1 when every block in blockInfo is covered by that set (a block without a
// position never is), else 0 (DigTreasureManager.lua:203-220).
func (m *digMap) checkOpenAll(pos int) int32 {
	m.check[pos] = true
	for _, b := range m.blocks {
		if !m.covered(b, m.check) {
			return 0
		}
	}
	return 1
}

// hiddenBlocks counts blocks whose position the server sent although none of their bricks is
// open: the player can't see them, so nextBrick ignores them.
func (m *digMap) hiddenBlocks() int {
	n := 0
	for _, b := range m.blocks {
		if bs := m.bricks(b.bid, b.at); len(bs) > 0 && !slices.ContainsFunc(bs, func(p int) bool { return m.opened[p] }) {
			n++
		}
	}
	return n
}

// summary is a one-line description of the board for the log.
func (m *digMap) summary() string {
	return fmt.Sprintf("map %d (%dx%d, uuid %d) state %d, %d/%d bricks open, %d blocks known (%d hidden)", m.cfg, m.lvl.w, m.lvl.h,
		m.uuid, m.state, len(m.opened), m.lvl.w*m.lvl.h, len(m.blocks), m.hiddenBlocks())
}

// digBoxes is a mapBoxList: stage chest state by mapConfigId, in list order.
type digBoxes []struct {
	cfg   int64
	state int64
}

func parseDigBoxes(o *sfs.SFSObject) digBoxes {
	var out digBoxes
	for _, b := range evObjects(o, "mapBoxList") {
		cfg, _ := claimInt(b, "mapConfigId")
		st, _ := claimInt(b, "rewardState")
		out = append(out, struct{ cfg, state int64 }{cfg, st})
	}
	return out
}

func (bs digBoxes) canGet() int {
	n := 0
	for _, b := range bs {
		if b.state == digCanGet {
			n++
		}
	}
	return n
}

// set updates the chest of cfg, as the open replies do (BuildingDigGameOpenBlockMessage.lua:44-56).
func (bs digBoxes) set(cfg, state int64) {
	for i := range bs {
		if bs[i].cfg == cfg {
			bs[i].state = state
		}
	}
}

// digBoard is how one board is dug: its hammer, its open command and params.
type digBoard struct {
	label   string
	hammer  string
	openCmd string
	params  func(pos int) *sfs.SFSObject
	vault   bool // a vault stage: send checkOpenAll
}

// digRun is one run's state.
type digRun struct {
	conn    *session.GameConn
	in      *Init
	dry     bool
	name    string
	hammers map[string]int64
	items   map[string]int64
	opens   int
	claims  int
	errs    []error
	dead    bool
}

func runTreasureDigs(conn *session.GameConn, in *Init) error { return treasureDigs(conn, in, false) }

func runTreasureDigsPlan(conn *session.GameConn, in *Init) error { return treasureDigs(conn, in, true) }

func treasureDigs(conn *session.GameConn, in *Init, dry bool) error {
	name := "treasure-digs"
	if dry {
		name = "treasure-digs-plan"
	}
	if in == nil || in.Raw == nil {
		slog.Info(name + ": no init push; skipping")
		return nil
	}
	items := duelInitItemCounts(in)
	r := &digRun{conn: conn, in: in, dry: dry, name: name, items: items,
		hammers: map[string]int64{digVaultHammer: items[digVaultHammer], digRuinHammer: items[digRuinHammer]}}
	slog.Info(name+": start", "digHammers", r.hammers[digVaultHammer], "explorerHammers", r.hammers[digRuinHammer])
	// Radar ruins expire with their event, so they go first; they grant their own hammers.
	r.radarRuins()
	if !r.dead {
		r.cityRuins()
	}
	if !r.dead {
		r.vault()
	}
	if !r.dead {
		r.chestTrack()
	}
	slog.Info(name+": done", "bricksOpened", r.opens, "claims", r.claims, "digHammersLeft", r.hammers[digVaultHammer],
		"explorerHammersLeft", r.hammers[digRuinHammer])
	return errors.Join(r.errs...)
}

func (r *digRun) fail(err error) {
	r.errs = append(r.errs, err)
	if session.ContainsNonTimeoutNetError(err) {
		r.dead = true
	}
}

// read sends an info request, in the plan too.
func (r *digRun) read(label, cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	if err := digCheckParams(cmd, p); err != nil {
		r.fail(err)
		return nil
	}
	if r.dead {
		return nil
	}
	msg, err := session.SendAndWait(r.conn, label, cmd, p)
	if err != nil {
		r.fail(err)
		return nil
	}
	return msg.Params
}

// act sends one claim, free hammer, placement or open, or in the plan logs it. ok is true only for
// a sent request whose reply has no errorCode.
func (r *digRun) act(label, cmd string, p *sfs.SFSObject) (*sfs.SFSObject, bool) {
	if err := digCheckParams(cmd, p); err != nil {
		slog.Error(err.Error())
		r.fail(err)
		return nil, false
	}
	if r.dead {
		return nil, false
	}
	if r.dry {
		slog.Info(r.name+": would send "+label, "cmd", cmd, "params", p.StringRedacted())
		return nil, false
	}
	msg, err := session.SendAndWait(r.conn, label, cmd, p)
	if err != nil {
		r.fail(err)
		return nil, false
	}
	if msg.Params.Has("errorCode") {
		return msg.Params, false
	}
	return msg.Params, true
}

// claim sends a chest or hammer claim, logs what it granted and follows the hammer totals.
func (r *digRun) claim(label, cmd string, p *sfs.SFSObject) (*sfs.SFSObject, bool) {
	if r.claims >= digMaxClaims {
		slog.Info(r.name+": claim cap reached for this run", "claims", r.claims)
		return nil, false
	}
	r.claims++
	reply, ok := r.act(label, cmd, p)
	if ok {
		slog.Info(label+" granted "+describeGrants(reply), "cmd", cmd)
		r.grants(reply)
	}
	return reply, ok
}

// grants follows the hammer counts a reply's rewards carry: a new total when sent, else the gain.
func (r *digRun) grants(p *sfs.SFSObject) {
	for _, g := range decodeRewardGrants(p) {
		if g.Type != rewardTypeGoods {
			continue
		}
		id := strconv.FormatInt(g.ItemID, 10)
		if _, tracked := r.hammers[id]; !tracked {
			continue
		}
		switch {
		case g.HasTotal:
			r.hammers[id] = g.Total
		case g.HasGained:
			r.hammers[id] += g.Gained
		}
	}
}

func digIDParams(key string, id int64) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutLong(key, id)
	return p
}

// dig opens bricks of m while it is CanNotGet, a hammer is held and the run's cap allows, and
// returns the last successful reply; failed is set when an open failed or carried an errorCode,
// after which the board is left alone. It also stops at a reply carrying a new board or
// checkOpenAll, which the caller handles. In the plan it logs the bricks it would open.
func (r *digRun) dig(b digBoard, m *digMap) (last *sfs.SFSObject, failed bool) {
	if m.state != digCanNotGet {
		return nil, false
	}
	slog.Info(r.name+": "+b.label, "board", m.summary(), "hammers", r.hammers[b.hammer])
	var planned []int
	defer func() {
		if len(planned) > 0 {
			slog.Info(r.name+": would open "+b.label+" bricks", "cmd", b.openCmd, "bricks", fmt.Sprint(planned),
				"hammersLeft", r.hammers[b.hammer])
		}
	}()
	for m.state == digCanNotGet && !r.dead {
		if r.opens >= digMaxOpens {
			slog.Info(r.name+": brick cap reached for this run", "opens", r.opens)
			return last, false
		}
		if r.hammers[b.hammer] < 1 {
			slog.Info(r.name+": no hammer left for "+b.label, "hammer", b.hammer)
			return last, false
		}
		pos := m.nextBrick()
		if pos == 0 {
			if !r.dry {
				// The board is still CanNotGet with nothing left to open: the server decides.
				slog.Warn(r.name+": every brick of "+b.label+" is open but the board isn't finished", "board", m.summary())
			}
			return last, false
		}
		p := b.params(pos)
		if b.vault {
			p.PutInt("checkOpenAll", m.checkOpenAll(pos))
		}
		if err := digCheckParams(b.openCmd, p); err != nil {
			slog.Error(err.Error())
			r.fail(err)
			return last, true
		}
		if r.opens > 0 && !r.dry {
			digSleep(digTapPause)
		}
		r.opens++
		r.hammers[b.hammer]--
		if r.dry {
			m.opened[pos] = true
			planned = append(planned, pos)
			continue
		}
		reply, ok := r.act(fmt.Sprintf("%s brick %d", b.label, pos), b.openCmd, p)
		if !ok {
			slog.Warn(r.name+": "+b.label+" brick failed; leaving the board", "brick", pos)
			return last, true
		}
		last = reply
		r.grants(reply)
		if digRecheck(reply) {
			slog.Warn(r.name+": the server asked to re-check "+b.label+"; re-reading it", "brick", pos)
			return reply, false
		}
		m.opened[pos] = true
		if obi := evObject(reply, "openBlockInfo"); obi != nil {
			m.addBlock(obi)
		}
		if st, ok := claimInt(reply, "rewardState"); ok {
			m.state = st
		}
		if reply.Has("newLevelMap") || reply.Has("newTimeLimitMap") || reply.Has("newGameInfo") {
			return reply, false
		}
	}
	return last, r.dead
}

// digRecheck reports whether an open reply carries checkOpenAll, the server's "re-read the board"
// (DigTreasureGameOpenBlockMessage.lua:19-25): true or a non-zero number.
func digRecheck(reply *sfs.SFSObject) bool {
	if v, ok := claimBool(reply, "checkOpenAll"); ok {
		return v
	}
	n, ok := claimInt(reply, "checkOpenAll")
	return ok && n != 0
}

// radarRuins digs the radar ruins in get.detect.info, soonest to expire first.
func (r *digRun) radarRuins() {
	p := sfs.NewSFSObject()
	p.PutBool("openWnd", false)
	info := r.read("radar info", radarInfoCmd, p)
	if info == nil {
		return
	}
	type ruin struct {
		ev  detectEvent
		dig *sfs.SFSObject
	}
	var ruins []ruin
	for _, o := range claimObjectsOrMap(info, "events") {
		ev := parseDetectEvent(o)
		dig := evObject(o, "digGameInfo")
		if dig == nil && ev.typ != digRadarTypeRuin && ev.typ != digRadarTypeRuin2 {
			continue
		}
		ruins = append(ruins, ruin{ev, dig})
	}
	if len(ruins) == 0 {
		slog.Info(r.name + ": no radar ruin")
		return
	}
	end := func(ev detectEvent) int64 {
		if ev.endTime <= 0 {
			return 1<<62 - 1
		}
		return ev.endTime
	}
	slices.SortStableFunc(ruins, func(a, b ruin) int { return cmp.Compare(end(a.ev), end(b.ev)) })
	nowMs := evNow().UnixMilli()
	for _, ru := range ruins {
		if r.dead {
			return
		}
		ev, dig := ru.ev, ru.dig
		label := fmt.Sprintf("radar ruin %d (event %d)", ev.uuid, ev.eventID)
		switch {
		case ev.uuid == 0:
			continue
		case ev.state != radarStateNotFinish && ev.state != radarStateNotInWorld:
			slog.Info(r.name+": "+label+" skipped: not open on the map", "state", ev.state)
			continue
		case ev.endTime > 0 && ev.endTime <= nowMs:
			slog.Info(r.name + ": " + label + " skipped: expired")
			continue
		}
		if ev.state == radarStateNotInWorld {
			reply, ok := r.act(label+" place", digRadarPlaceCmd, digIDParams("uuid", ev.uuid))
			if ok {
				if d := evObject(reply, "digGameInfo"); d != nil {
					dig = d
				}
			} else if !r.dry {
				continue
			}
		}
		m := parseDigMap(dig)
		switch {
		case m == nil:
			slog.Info(r.name + ": " + label + " has no digGameInfo; the client can't open it")
			continue
		case !m.known:
			slog.Warn(r.name+": "+label+" has a map the table lacks; skipping", "mapConfigId", m.cfg)
			continue
		case m.state != digCanNotGet:
			slog.Info(r.name+": "+label, "board", m.summary(), "endTime", ev.endTime)
		}
		if m.state == digNotBegin {
			if _, ok := r.claim(label+" free hammers", digRadarHammerCmd, digIDParams("eventUuid", ev.uuid)); ok || r.dry {
				m.state = digCanNotGet
				if r.dry {
					r.hammers[digRuinHammer] += int64(m.lvl.hammers)
					slog.Info(r.name+": "+label+" free hammers would add the table's hammer_num", "hammers", m.lvl.hammers)
				}
			}
		}
		board := digBoard{label: label, hammer: digRuinHammer, openCmd: digRadarOpenCmd,
			params: func(pos int) *sfs.SFSObject {
				q := digIDParams("eventUuid", ev.uuid)
				q.PutInt("pos", int32(pos))
				return q
			}}
		if _, failed := r.dig(board, m); failed || m.state != digCanGet {
			continue
		}
		if held, why := r.radarChestHeld(ev); held {
			slog.Info(r.name+": "+label+" chest held for the Alliance Duel", "reason", why)
			continue
		}
		r.claim(label+" chest", digRadarChestCmd, digIDParams("eventUuid", ev.uuid))
	}
}

// radarChestHeld reports whether a radar ruin's chest waits: radar tasks are claimed only on a day
// whose Alliance Duel entry, read at login, scores them (score type 82), whatever the event's end.
func (r *digRun) radarChestHeld(ev detectEvent) (bool, string) {
	if TodayDuel(r.conn, r.in).Scores(DuelScoreRadarTask, "") {
		return false, ""
	}
	return true, "today's Alliance Duel doesn't score radar tasks"
}

// cityRuins digs every building carrying a buildingDigGame and claims its stage chests.
func (r *digRun) cityRuins() {
	found := false
	for _, b := range r.in.Buildings {
		g := evObject(b.Raw, "buildingDigGame")
		if g == nil || b.BId() != digCityBuilding || r.dead {
			continue
		}
		found = true
		uuid := b.Uuid()
		label := fmt.Sprintf("city ruin %d (building %d)", uuid, b.BId())
		boxes := parseDigBoxes(g)
		m := parseDigMap(evObject(g, "gameInfo"))
		board := digBoard{label: label, hammer: digRuinHammer, openCmd: digCityOpenCmd,
			params: func(pos int) *sfs.SFSObject {
				q := digIDParams("buildingUuid", uuid)
				q.PutInt("pos", int32(pos))
				return q
			}}
		for round := 0; round < digMaxRounds && m != nil && !r.dead; round++ {
			if !m.known {
				slog.Warn(r.name+": "+label+" has a map the table lacks; skipping", "mapConfigId", m.cfg)
				break
			}
			if m.state != digCanNotGet {
				slog.Info(r.name+": "+label, "board", m.summary(), "chestsCanGet", boxes.canGet())
			}
			if m.state == digNotBegin {
				if _, ok := r.claim(label+" free hammers", digCityHammerCmd, digIDParams("buildingUuid", uuid)); ok || r.dry {
					m.state = digCanNotGet
				}
			}
			reply, failed := r.dig(board, m)
			if failed || reply == nil {
				break
			}
			if st, ok := claimInt(reply, "rewardState"); ok {
				cfg, _ := claimInt(reply, "mapConfigId")
				boxes.set(cfg, st)
			}
			next := evObject(reply, "newGameInfo")
			if next == nil {
				break
			}
			m = parseDigMap(next)
		}
		// One claim per chest the client shows as CanGet; the reply's mapBoxList is the new state.
		for i := 0; i < len(boxes) && boxes.canGet() > 0 && !r.dead; i++ {
			reply, ok := r.claim(label+" stage chest", digCityChestCmd, digIDParams("buildingUuid", uuid))
			if !ok {
				break
			}
			if reply.Has("mapBoxList") {
				boxes = parseDigBoxes(reply)
			} else {
				// No mapBoxList: every stage is passed (PassAllLevel, BuildingDigTreasureManager.lua:63-70).
				boxes = nil
			}
		}
	}
	if !found {
		slog.Info(r.name + ": no city ruin")
	}
}

// digActivityOpen reports whether one of ids is in init activity, keyed by id or as an array, and
// open (ActivityListDataManager:CheckIsSend, as Activity.open).
func digActivityOpen(in *Init, typ int32, ids []int32) bool {
	type entry struct {
		key string
		obj *sfs.SFSObject
	}
	var entries []entry
	if acts := in.Object("activity"); acts != nil {
		for _, k := range acts.Keys() {
			if o := evObject(acts, k); o != nil {
				entries = append(entries, entry{k, o})
			}
		}
	}
	for _, o := range in.Objects("activity") {
		entries = append(entries, entry{"", o})
	}
	now, hq := evNow().UnixMilli(), activityHQLevel(in)
	for _, e := range entries {
		id, ok := activityEntryID(e.obj, e.key)
		if ok && slices.Contains(ids, id) && (Activity{ID: id, Type: typ, Raw: e.obj}).open(now, hq) {
			return true
		}
	}
	return false
}

// digVaultState is a hero.dispatch.get.dig.game.info reply.
type digVaultState struct {
	stage, timed *digMap
	boxes        digBoxes
}

func parseVault(p *sfs.SFSObject) *digVaultState {
	return &digVaultState{stage: parseDigMap(evObject(p, "levelMap")), timed: parseDigMap(evObject(p, "timeLimitMap")),
		boxes: parseDigBoxes(p)}
}

// timedActive is IsHaveTimeLimitMap: a timed stage within its time or not yet HaveGot.
func (s *digVaultState) timedActive(nowMs int64) bool {
	t := s.timed
	return t != nil && t.uuid != 0 && ((t.endTime > 0 && nowMs < t.endTime) || t.state != digHaveGot)
}

func (r *digRun) vaultBoard(m *digMap, timed bool) digBoard {
	label := "secret vault stage"
	if timed {
		label = "secret vault timed stage"
	}
	uuid := m.uuid
	return digBoard{label: label, hammer: digVaultHammer, openCmd: digVaultOpenCmd, vault: !timed,
		params: func(pos int) *sfs.SFSObject {
			q := digIDParams("uuid", uuid)
			q.PutInt("pos", int32(pos))
			if timed {
				// The client sends checkOpenAll 0 on a timed stage (DigTreasureManager.lua:203-226).
				q.PutInt("checkOpenAll", 0)
			}
			return q
		}}
}

// vault plays the Secret Vault while its activity is open.
func (r *digRun) vault() {
	if !digActivityOpen(r.in, actTypeDigTreasure, digVaultActivities) {
		slog.Info(r.name + ": Secret Vault activity not open")
		return
	}
	info := r.read("secret vault info", digVaultInfoCmd, sfs.NewSFSObject())
	if info == nil {
		return
	}
	s := parseVault(info)
	for _, m := range []*digMap{s.stage, s.timed} {
		if m != nil && m.uuid != 0 {
			slog.Info(r.name+": secret vault", "board", m.summary(), "endTime", m.endTime)
		}
	}
	slog.Info(r.name+": secret vault stage chests", "canGet", s.boxes.canGet(), "chests", len(s.boxes),
		"isHammerGetMax", evString(info, "isHammerGetMax"))
	for round := 0; round < digMaxRounds && !r.dead; round++ {
		progressed := false
		if s.boxes.canGet() > 0 {
			reply, ok := r.claim("secret vault stage chests", digVaultStageChestCmd, sfs.NewSFSObject())
			switch {
			case ok:
				progressed = true
				if reply.Has("mapBoxList") {
					s.boxes = parseDigBoxes(reply)
				}
				if nm := parseDigMap(evObject(reply, "newMap")); nm != nil {
					s.stage = nm
				}
			case r.dry:
				for i := range s.boxes {
					if s.boxes[i].state == digCanGet {
						s.boxes[i].state = digHaveGot
					}
				}
			}
		}
		nowMs := evNow().UnixMilli()
		if s.timedActive(nowMs) {
			t := s.timed
			label := fmt.Sprintf("secret vault timed stage %d", t.uuid)
			switch {
			case !t.known:
				slog.Warn(r.name+": "+label+" has a map the table lacks; leaving the vault", "mapConfigId", t.cfg)
				return
			case t.state == digNotBegin:
				need, have := int64(t.lvl.w*t.lvl.h-t.lvl.hammers), r.hammers[digVaultHammer]
				if t.lvl.hammers <= 0 || have < need {
					slog.Info(r.name+": "+label+" not started: the hammers held plus its grant don't cover every brick, "+
						"so it could fail; start it in the game or hold more Dig Hammers", "held", have, "need", need)
					return
				}
				reply, ok := r.claim(label+" start", digVaultTimedStartCmd, digIDParams("uuid", t.uuid))
				switch {
				case ok:
					progressed = true
					if gm := parseDigMap(evObject(reply, "gameMap")); gm != nil {
						s.timed = gm
					} else {
						t.state = digCanNotGet
					}
					// The gate assumed the grant is hammer_num Dig Hammers; the ledger follows the reply.
					if closed := int64(t.lvl.w*t.lvl.h - len(s.timed.opened)); r.hammers[digVaultHammer] < closed {
						slog.Warn(r.name+": "+label+" grant didn't cover the board; it may fail", "held", r.hammers[digVaultHammer],
							"closedBricks", closed)
					}
				case r.dry:
					progressed = true
					t.state = digCanNotGet
					r.hammers[digVaultHammer] += int64(t.lvl.hammers)
				}
			case t.state == digCanNotGet && (t.endTime <= 0 || nowMs < t.endTime):
				reply, failed := r.dig(r.vaultBoard(t, true), t)
				if failed {
					return
				}
				progressed = reply != nil
			case t.state == digCanGet:
				if _, ok := r.claim(label+" chest", digVaultTimedChestCmd, digIDParams("uuid", t.uuid)); ok || r.dry {
					progressed = true
					s.timed = nil
				}
			case t.state == digFail || t.state == digCanNotGet:
				if _, ok := r.act(label+" acknowledge the failure", digVaultTimedFailCmd, digIDParams("uuid", t.uuid)); ok || r.dry {
					progressed = true
					s.timed = nil
				}
			default:
				slog.Info(r.name+": "+label+" done; the stage returns when its time ends", "state", t.state)
				return
			}
		} else if st := s.stage; st != nil && st.uuid != 0 {
			if !st.known {
				slog.Warn(r.name+": secret vault stage has a map the table lacks; leaving the vault", "mapConfigId", st.cfg)
				return
			}
			if st.state != digCanNotGet {
				if !progressed {
					slog.Info(r.name+": secret vault stage not diggable", "state", st.state)
				}
			} else if reply, failed := r.dig(r.vaultBoard(st, false), st); failed {
				return
			} else if reply != nil {
				progressed = true
				if digRecheck(reply) {
					fresh := r.read("secret vault info", digVaultInfoCmd, sfs.NewSFSObject())
					if fresh == nil {
						return
					}
					s = parseVault(fresh)
					continue
				}
				if st, ok := claimInt(reply, "rewardState"); ok {
					cfg, _ := claimInt(reply, "mapConfigId")
					s.boxes.set(cfg, st)
				}
				if nt := parseDigMap(evObject(reply, "newTimeLimitMap")); nt != nil {
					s.timed = nt
				}
				if ns := parseDigMap(evObject(reply, "newLevelMap")); ns != nil {
					s.stage = ns
				}
			}
		}
		if !progressed {
			return
		}
	}
}

// chestTrack claims the vault chest track's reached chests.
func (r *digRun) chestTrack() {
	if !digActivityOpen(r.in, actTypeDigTreasureV2, digTrackActivities) {
		slog.Info(r.name + ": vault chest track not open")
		return
	}
	info := r.read("vault chest track", digTrackInfoCmd, sfs.NewSFSObject())
	if info == nil {
		return
	}
	score := r.items[digTrackScore]
	reached := func(list []*sfs.SFSObject) int {
		n := 0
		for _, e := range list {
			target, ok := claimInt(e, "target")
			got, _ := claimInt(e, "isReward")
			if ok && got != 1 && target <= score {
				n++
			}
		}
		return n
	}
	list := claimObjects(info, "rewardList")
	slog.Info(r.name+": vault chest track", "score", score, "chests", len(list), "reached", reached(list))
	for i := 0; i <= len(list) && reached(list) > 0 && !r.dead; i++ {
		reply, ok := r.claim("vault chest track chest", digTrackChestCmd, sfs.NewSFSObject())
		if !ok || !reply.Has("rewardList") {
			return
		}
		list = claimObjects(reply, "rewardList")
	}
}
