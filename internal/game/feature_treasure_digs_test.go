package game

import (
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// noDigPause drops the pause between opens for one test.
func noDigPause(t *testing.T) {
	t.Helper()
	saved := digSleep
	digSleep = func(time.Duration) {}
	t.Cleanup(func() { digSleep = saved })
}

// digTestInit is an init with an open duel, the given activities and hammers held
// (item id -> count), as init items carries them.
func digTestInit(items map[string]int32, acts ...*sfs.SFSObject) *Init {
	in := evInit(30, append([]*sfs.SFSObject{duelActivity()}, acts...)...)
	arr := sfs.NewSFSArray()
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range items {
			if !yield(k) {
				return
			}
		}
	}) {
		it := sfs.NewSFSObject()
		it.PutUtfString("uuid", "stack-"+id)
		it.PutUtfString("itemId", id)
		it.PutInt("count", items[id])
		arr.AddSFSObject(it)
	}
	in.Raw.PutSFSArray("items", arr)
	return in
}

// digBoardSFS builds a board as the server sends it: open bricks in openInfo and blockInfo
// {bid, pos} (pos 0 leaves it out).
func digBoardSFS(uuid int64, cfg int32, state int64, opened []int, blocks map[int32]int) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutLong("uuid", uuid)
	o.PutInt("mapConfigId", cfg)
	o.PutInt("rewardState", int32(state))
	open := sfs.NewSFSArray()
	for _, p := range opened {
		open.AddInt(int32(p))
	}
	o.PutSFSArray("openInfo", open)
	bi := sfs.NewSFSArray()
	for _, bid := range slices.Sorted(func(yield func(int32) bool) {
		for k := range blocks {
			if !yield(k) {
				return
			}
		}
	}) {
		b := sfs.NewSFSObject()
		b.PutInt("bid", bid)
		if blocks[bid] > 0 {
			b.PutInt("pos", int32(blocks[bid]))
		}
		bi.AddSFSObject(b)
	}
	o.PutSFSArray("blockInfo", bi)
	return o
}

// digFakeBoard is the server side of one board: the blocks under it and the bricks opened.
type digFakeBoard struct {
	m      *digMap
	hidden map[int32]int // bid -> top-left brick
	opened map[int]bool
}

func newDigFakeBoard(cfg int32, hidden map[int32]int, opened ...int) *digFakeBoard {
	b := &digFakeBoard{m: &digMap{cfg: cfg, lvl: digLevels[cfg], opened: map[int]bool{}}, hidden: hidden, opened: map[int]bool{}}
	for _, p := range opened {
		b.opened[p] = true
	}
	return b
}

// open answers an open: the block under the brick, if any, and rewardState 1 once every block
// is uncovered.
func (b *digFakeBoard) open(uuid int64, pos int) *sfs.SFSObject {
	b.opened[pos] = true
	r := sfs.NewSFSObject()
	r.PutLong("uuid", uuid)
	r.PutInt("pos", int32(pos))
	r.PutInt("mapConfigId", b.m.cfg)
	done := true
	for bid, at := range b.hidden {
		bricks := b.m.bricks(bid, at)
		if slices.Contains(bricks, pos) {
			obi := sfs.NewSFSObject()
			obi.PutInt("bid", bid)
			obi.PutInt("pos", int32(at))
			r.PutSFSObject("openBlockInfo", obi)
		}
		for _, p := range bricks {
			done = done && b.opened[p]
		}
	}
	state := digCanNotGet
	if done {
		state = digCanGet
	}
	r.PutInt("rewardState", int32(state))
	return r
}

// hammerGrant is a free-hammer reply: goods reward with the new total and the gain.
func hammerGrant(item string, total, add int32) *sfs.SFSObject {
	v := sfs.NewSFSObject()
	v.PutUtfString("id", item)
	v.PutInt("count", total)
	v.PutInt("rewardAdd", add)
	e := sfs.NewSFSObject()
	e.PutInt("type", rewardTypeGoods)
	e.PutSFSObject("value", v)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(e)
	r := sfs.NewSFSObject()
	r.PutSFSArray("reward", arr)
	return r
}

// digRuinEvent is a radar ruin event (eventId 208, detect_event type 30) carrying board.
func digRuinEvent(uuid int64, state int64, end time.Time, board *sfs.SFSObject) *sfs.SFSObject {
	ev := detectEv(uuid, 208, state)
	ev.PutLong("endTime", end.UnixMilli())
	if board != nil {
		ev.PutSFSObject("digGameInfo", board)
	}
	return ev
}

// digPositions lists the pos of every request with cmd.
func digPositions(f *evFake, cmd string) []int {
	var out []int
	for _, r := range f.only(cmd) {
		out = append(out, int(r.Params.GetInt("pos")))
	}
	return out
}

// checkDigSent fails on any request outside the feature's allowlist (plus the duel read), and on
// any buy, gold or exchange command or key. The transport's own _id (the future id SendExtension
// adds on the wire) is not a request key.
func checkDigSent(t *testing.T, f *evFake) {
	t.Helper()
	for _, r := range f.requests() {
		if r.Cmd == "hero.event.info.get" {
			continue
		}
		p := sfs.NewSFSObject()
		for _, k := range r.Params.Keys() {
			if v, ok := r.Params.Get(k); ok && k != "_id" {
				p.PutValue(k, v)
			}
		}
		if err := digCheckParams(r.Cmd, p); err != nil {
			t.Errorf("sent a request the allowlist refuses: %v", err)
		}
		low := strings.ToLower(r.Cmd)
		for _, bad := range []string{"buy", "gold", "exchange", "give", "share"} {
			if strings.Contains(low, bad) {
				t.Errorf("sent %s", r.Cmd)
			}
		}
		for _, k := range r.Params.Keys() {
			if strings.Contains(strings.ToLower(k), "gold") {
				t.Errorf("%s carried key %q", r.Cmd, k)
			}
		}
	}
}

// startDigFake serves today's duel, a get.detect.info of events, and handle (nil falls through to
// a plain success).
func startDigFake(t *testing.T, duel []*sfs.SFSObject, events []*sfs.SFSObject, handle func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	noDigPause(t)
	return startRadarFake(t, duel, []*sfs.SFSObject{detectReply(20, 10, events...)}, handle)
}

func TestDigTablesGenerated(t *testing.T) {
	if got := digLevels[12001]; got != (digLevel{w: 6, h: 6, typ: 6, hammers: 25}) {
		t.Errorf("timed vault stage 12001 = %+v", got)
	}
	if got := digLevels[12501]; got != (digLevel{w: 4, h: 4, typ: 7, hammers: 16}) {
		t.Errorf("radar ruin 12501 = %+v", got)
	}
	if got := digBlockSizes[20001]; got != (digBlockSize{w: 2, h: 4}) {
		t.Errorf("block 20001 = %+v", got)
	}
}

func TestDigCheckParamsRefusesBuysGoldAndUnknownKeys(t *testing.T) {
	open := sfs.NewSFSObject()
	open.PutLong("eventUuid", 1)
	open.PutInt("pos", 3)
	if err := digCheckParams(digRadarOpenCmd, open); err != nil {
		t.Errorf("the open itself must pass: %v", err)
	}
	gold := sfs.NewSFSObject()
	gold.PutLong("eventUuid", 1)
	gold.PutInt("pos", 3)
	gold.PutInt("useGold", 1)
	if digCheckParams(digRadarOpenCmd, gold) == nil {
		t.Error("an open carrying useGold must be refused")
	}
	extra := sfs.NewSFSObject()
	extra.PutLong("buildingUuid", 1)
	extra.PutInt("num", 5)
	if digCheckParams(digCityHammerCmd, extra) == nil {
		t.Error("a key outside the allowlist must be refused")
	}
	for _, cmd := range []string{"buy.activity.dig.goods", "buy.activity.dig.v2.goods", "hero.dispatch.hammer.exchange",
		"hero.dispatch.give.dig.hammer", "hero.dispatch.share.dig.hammer.chat", "hero.dispatch.dig.game.give.up",
		"auto.activity.dig"} {
		if digCheckParams(cmd, sfs.NewSFSObject()) == nil {
			t.Errorf("%s must be refused", cmd)
		}
	}
	for cmd, keys := range digAllowedParams {
		for _, f := range digForbidden {
			if strings.Contains(strings.ToLower(cmd), f) {
				t.Errorf("allowlisted command %s contains %q", cmd, f)
			}
			for _, k := range keys {
				if strings.Contains(strings.ToLower(k), f) {
					t.Errorf("allowlisted key %s.%s contains %q", cmd, k, f)
				}
			}
		}
	}
}

func TestDigNextBrickFinishesVisibleBlocksAndIgnoresHiddenOnes(t *testing.T) {
	// 4x4 radar ruin; block 20009 (2x2) at 1 has brick 1 open, block 20001 (2x4) at 3 is sent with
	// its position but none of its bricks is open.
	m := parseDigMap(digBoardSFS(1, 12501, digCanNotGet, []int{1}, map[int32]int{20009: 1, 20001: 3}))
	var got []int
	for p := m.nextBrick(); p != 0 && len(got) < 16; p = m.nextBrick() {
		got = append(got, p)
		m.opened[p] = true
	}
	// 2, 5, 6 finish the visible block; 3 is next by number, which makes 20001 visible and
	// finished next; the empty bricks come last.
	if want := []int{2, 5, 6, 3, 4, 7, 8, 11, 12, 15, 16, 9, 10, 13, 14}; !slices.Equal(got, want) {
		t.Errorf("brick order = %v, want %v", got, want)
	}
	if h := parseDigMap(digBoardSFS(1, 12501, digCanNotGet, []int{1}, map[int32]int{20009: 1, 20001: 3})).hiddenBlocks(); h != 1 {
		t.Errorf("hiddenBlocks = %d, want 1", h)
	}
}

func TestDigCheckOpenAllMirrorsTheClient(t *testing.T) {
	// 6x6 vault stage; block 20009 (2x2) at 1 covers bricks 1, 2, 7, 8.
	m := parseDigMap(digBoardSFS(1, 11001, digCanNotGet, []int{1, 2}, map[int32]int{20009: 1}))
	if got := m.checkOpenAll(7); got != 0 {
		t.Errorf("checkOpenAll(7) = %d with brick 8 still closed", got)
	}
	if got := m.checkOpenAll(8); got != 1 {
		t.Errorf("checkOpenAll(8) = %d, want 1: every block is covered", got)
	}
	// A block without a position is never covered (CheckBlockGet with a nil pos).
	m = parseDigMap(digBoardSFS(1, 11001, digCanNotGet, nil, map[int32]int{20009: 0}))
	if got := m.checkOpenAll(1); got != 0 {
		t.Errorf("checkOpenAll with an unplaced block = %d", got)
	}
	// No blockInfo at all: the client's loop finds nothing uncovered and sends 1.
	m = parseDigMap(digBoardSFS(1, 11001, digCanNotGet, nil, nil))
	if got := m.checkOpenAll(1); got != 1 {
		t.Errorf("checkOpenAll with no blocks = %d, want 1", got)
	}
	// personalOpenInfo opens bricks but doesn't seed the check set (DiggingMapData.lua:49-55).
	o := digBoardSFS(1, 11001, digCanNotGet, nil, map[int32]int{20009: 1})
	p := sfs.NewSFSArray()
	for _, b := range []int{1, 2, 7} {
		p.AddInt(int32(b))
	}
	o.PutSFSArray("personalOpenInfo", p)
	m = parseDigMap(o)
	if !m.opened[7] || m.checkOpenAll(8) != 0 {
		t.Errorf("personalOpenInfo: opened=%v, want brick 7 open and checkOpenAll(8) 0", m.opened)
	}
}

func TestTreasureDigsRadarRuinClaimsFreeHammersDigsAndClaims(t *testing.T) {
	board := newDigFakeBoard(12501, map[int32]int{20009: 1, 20001: 3})
	ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(4*time.Hour), digBoardSFS(9001, 12501, digNotBegin, nil, nil))
	conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{ruin}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case digRadarHammerCmd:
			return hammerGrant(digRuinHammer, 16, 16)
		case digRadarOpenCmd:
			return board.open(9001, int(p.GetInt("pos")))
		}
		return nil
	})
	if err := runTreasureDigs(conn, digTestInit(nil)); err != nil {
		t.Fatal(err)
	}
	checkDigSent(t, f)
	if got := f.only(digRadarHammerCmd); len(got) != 1 || got[0].Params.GetLong("eventUuid") != 900 {
		t.Fatalf("free hammer claims = %v, want one for event 900", got)
	}
	want := []int{1, 2, 5, 6, 3, 4, 7, 8, 11, 12, 15, 16}
	if got := digPositions(f, digRadarOpenCmd); !slices.Equal(got, want) {
		t.Errorf("bricks opened = %v, want %v", got, want)
	}
	for _, r := range f.only(digRadarOpenCmd) {
		if r.Params.GetLong("eventUuid") != 900 {
			t.Errorf("open for event %d", r.Params.GetLong("eventUuid"))
		}
	}
	if got := f.only(digRadarChestCmd); len(got) != 1 || got[0].Params.GetLong("eventUuid") != 900 {
		t.Errorf("chest claims = %v, want one on a radar-scoring day", got)
	}
	if n := len(f.only(digRadarPlaceCmd)); n != 0 {
		t.Errorf("an event already on the map was placed %d times", n)
	}
}

func TestTreasureDigsOpensOnlyWithOwnedHammers(t *testing.T) {
	for _, held := range []int32{0, 3} {
		board := newDigFakeBoard(12501, map[int32]int{20009: 1, 20001: 3})
		ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(4*time.Hour), digBoardSFS(9001, 12501, digCanNotGet, nil, nil))
		conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{ruin}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == digRadarOpenCmd {
				return board.open(9001, int(p.GetInt("pos")))
			}
			return nil
		})
		if err := runTreasureDigs(conn, digTestInit(map[string]int32{digRuinHammer: held, digVaultHammer: 50})); err != nil {
			t.Fatal(err)
		}
		if n := len(f.only(digRadarHammerCmd)); n != 0 {
			t.Errorf("held %d: a board past NotBegin got %d free-hammer claims", held, n)
		}
		if got := len(f.only(digRadarOpenCmd)); got != int(held) {
			t.Errorf("held %d Explorer's Hammers: opened %d bricks (Dig Hammers must not count)", held, got)
		}
		if n := len(f.only(digRadarChestCmd)); n != 0 {
			t.Errorf("held %d: claimed an unfinished ruin's chest", held)
		}
	}
}

// The ledger follows the free-hammer reply: a new total when it sends one, else the gain, and the
// opens stop at it even though the board isn't finished.
func TestTreasureDigsStopsAtTheGrantedHammers(t *testing.T) {
	cases := []struct {
		name  string
		held  int32
		grant *sfs.SFSObject
	}{
		{"total in the reply", 0, hammerGrant(digRuinHammer, 7, 7)},
		{"gain only", 3, func() *sfs.SFSObject {
			v := sfs.NewSFSObject()
			v.PutUtfString("id", digRuinHammer)
			v.PutInt("rewardAdd", 4)
			e := sfs.NewSFSObject()
			e.PutInt("type", rewardTypeGoods)
			e.PutSFSObject("value", v)
			arr := sfs.NewSFSArray()
			arr.AddSFSObject(e)
			r := sfs.NewSFSObject()
			r.PutSFSArray("reward", arr)
			return r
		}()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			board := newDigFakeBoard(12501, map[int32]int{20009: 1, 20001: 3})
			ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(4*time.Hour), digBoardSFS(9001, 12501, digNotBegin, nil, nil))
			conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{ruin}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case digRadarHammerCmd:
					return tc.grant
				case digRadarOpenCmd:
					return board.open(9001, int(p.GetInt("pos")))
				}
				return nil
			})
			if err := runTreasureDigs(conn, digTestInit(map[string]int32{digRuinHammer: tc.held})); err != nil {
				t.Fatal(err)
			}
			if n := len(f.only(digRadarHammerCmd)); n != 1 {
				t.Errorf("free hammer claims = %d, want 1", n)
			}
			if got := digPositions(f, digRadarOpenCmd); !slices.Equal(got, []int{1, 2, 5, 6, 3, 4, 7}) {
				t.Errorf("bricks opened = %v, want the 7 hammers' worth", got)
			}
			if n := len(f.only(digRadarChestCmd)); n != 0 {
				t.Error("claimed the chest of an unfinished ruin")
			}
		})
	}
}

func TestTreasureDigsFailedFreeHammerOpensNothing(t *testing.T) {
	ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(4*time.Hour), digBoardSFS(9001, 12501, digNotBegin, nil, nil))
	conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{ruin}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == digRadarHammerCmd {
			return evErr("E120001")
		}
		return nil
	})
	if err := runTreasureDigs(conn, digTestInit(map[string]int32{digRuinHammer: 5})); err == nil {
		t.Error("a failed free-hammer claim must be reported")
	}
	if n := len(f.only(digRadarOpenCmd)); n != 0 {
		t.Errorf("opened %d bricks on a board still NotBegin", n)
	}
}

func TestTreasureDigsRadarChestHeldOnOffDays(t *testing.T) {
	cases := []struct {
		name  string
		duel  []*sfs.SFSObject
		end   time.Time
		claim bool
	}{
		{"off day, event outlives today", radarOffDay(), evTestNow.Add(4 * time.Hour), false},
		{"off day, event ends today", radarOffDay(), evTestNow.Add(30 * time.Minute), false},
		{"radar-scoring day", radarScoringDay(), evTestNow.Add(4 * time.Hour), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ruin := digRuinEvent(900, radarStateNotFinish, tc.end, digBoardSFS(9001, 12501, digCanGet, nil, nil))
			conn, f := startDigFake(t, tc.duel, []*sfs.SFSObject{ruin}, nil)
			if err := runTreasureDigs(conn, digTestInit(nil)); err != nil {
				t.Fatal(err)
			}
			if got := len(f.only(digRadarChestCmd)) == 1; got != tc.claim {
				t.Errorf("chest claimed = %v, want %v", got, tc.claim)
			}
		})
	}
}

func TestTreasureDigsPlacesRuinsOffTheMapAndSkipsExpired(t *testing.T) {
	off := digRuinEvent(901, radarStateNotInWorld, evTestNow.Add(4*time.Hour), nil)
	expired := digRuinEvent(902, radarStateNotFinish, evTestNow.Add(-time.Minute), digBoardSFS(9002, 12501, digNotBegin, nil, nil))
	finished := digRuinEvent(903, radarStateFinished, evTestNow.Add(4*time.Hour), digBoardSFS(9003, 12501, digCanGet, nil, nil))
	conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{off, expired, finished}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == digRadarPlaceCmd {
			r := detectEv(p.GetLong("uuid"), 208, radarStateNotFinish)
			r.PutSFSObject("digGameInfo", digBoardSFS(9001, 12501, digNotBegin, nil, nil))
			return r
		}
		return nil
	})
	if err := runTreasureDigs(conn, digTestInit(nil)); err != nil {
		t.Fatal(err)
	}
	if got := radarUUIDs(f, digRadarPlaceCmd); !slices.Equal(got, []int64{901}) {
		t.Errorf("placed %v, want [901]", got)
	}
	var claimed []int64
	for _, r := range f.only(digRadarHammerCmd) {
		claimed = append(claimed, r.Params.GetLong("eventUuid"))
	}
	if !slices.Equal(claimed, []int64{901}) {
		t.Errorf("free hammers claimed for %v, want only the placed ruin 901", claimed)
	}
	if n := len(f.only(digRadarChestCmd)); n != 0 {
		t.Errorf("claimed %d chests of expired or finished events", n)
	}
}

func TestTreasureDigsOpenErrorLeavesTheBoard(t *testing.T) {
	a := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(time.Hour), digBoardSFS(9001, 12501, digCanNotGet, nil, nil))
	b := digRuinEvent(901, radarStateNotFinish, evTestNow.Add(2*time.Hour), digBoardSFS(9002, 12501, digCanNotGet, nil, nil))
	boardB := newDigFakeBoard(12501, map[int32]int{20009: 1})
	conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{b, a}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == digRadarOpenCmd {
			if p.GetLong("eventUuid") == 900 {
				return evErr("E900")
			}
			return boardB.open(9002, int(p.GetInt("pos")))
		}
		return nil
	})
	if err := runTreasureDigs(conn, digTestInit(map[string]int32{digRuinHammer: 10})); err == nil {
		t.Error("the failed open must be reported")
	}
	var a1, b1 int
	for _, r := range f.only(digRadarOpenCmd) {
		if r.Params.GetLong("eventUuid") == 900 {
			a1++
		} else {
			b1++
		}
	}
	if a1 != 1 || b1 != 4 {
		t.Errorf("opens: ruin 900 %d (want 1, then left), ruin 901 %d (want 4)", a1, b1)
	}
}

// cityRuinBuilding is the city ruin building (10226000) with its dig game.
func cityRuinBuilding(uuid int64, bid int32, game *sfs.SFSObject, boxes map[int32]int64) Building {
	b := NewTestBuilding(uuid, bid, 1)
	list := sfs.NewSFSArray()
	for _, cfg := range slices.Sorted(func(yield func(int32) bool) {
		for k := range boxes {
			if !yield(k) {
				return
			}
		}
	}) {
		e := sfs.NewSFSObject()
		e.PutInt("mapConfigId", cfg)
		e.PutInt("rewardState", int32(boxes[cfg]))
		list.AddSFSObject(e)
	}
	g := sfs.NewSFSObject()
	g.PutSFSObject("gameInfo", game)
	g.PutSFSArray("mapBoxList", list)
	b.Raw.PutSFSObject("buildingDigGame", g)
	return b
}

func TestTreasureDigsCityRuin(t *testing.T) {
	board := newDigFakeBoard(13501, map[int32]int{20002: 1, 20001: 3})
	var mu sync.Mutex
	hammerClaims := 0
	conn, f := startDigFake(t, radarScoringDay(), nil, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		mu.Lock()
		defer mu.Unlock()
		switch cmd {
		case digCityHammerCmd:
			hammerClaims++
			if hammerClaims > 1 {
				return evErr("E_NO_MORE")
			}
			return hammerGrant(digRuinHammer, 30, 10)
		case digCityOpenCmd:
			r := board.open(9100, int(p.GetInt("pos")))
			r.PutLong("buildingUuid", p.GetLong("buildingUuid"))
			if r.GetInt("rewardState") == int32(digCanGet) {
				r.PutSFSObject("newGameInfo", digBoardSFS(9101, 13502, digNotBegin, nil, nil))
			}
			return r
		case digCityChestCmd:
			list := sfs.NewSFSArray()
			e := sfs.NewSFSObject()
			e.PutInt("mapConfigId", 13501)
			e.PutInt("rewardState", int32(digHaveGot))
			list.AddSFSObject(e)
			r := sfs.NewSFSObject()
			r.PutSFSArray("mapBoxList", list)
			return r
		}
		return nil
	})
	in := digTestInit(map[string]int32{digRuinHammer: 20})
	in.Buildings = append(in.Buildings,
		cityRuinBuilding(77, digCityBuilding, digBoardSFS(9100, 13501, digNotBegin, nil, nil), map[int32]int64{13501: digCanNotGet, 13502: digCanNotGet}),
		// A dig game on any other building is not one the client opens.
		cityRuinBuilding(78, 10100000, digBoardSFS(9200, 13501, digNotBegin, nil, nil), map[int32]int64{13501: digCanGet}))
	if err := runTreasureDigs(conn, in); err == nil {
		t.Error("the refused second free-hammer claim must be reported")
	}
	checkDigSent(t, f)
	for _, r := range f.requests() {
		if v, ok := r.Params.Get("buildingUuid"); ok && v.Val.(int64) != 77 {
			t.Errorf("%s for building %v", r.Cmd, v.Val)
		}
	}
	// 20002 (2x2) at 1 covers 1, 2, 5, 6; 20001 (2x4) at 3 covers 3, 4, 7, 8, 11, 12, 15, 16.
	if got, want := digPositions(f, digCityOpenCmd), []int{1, 2, 5, 6, 3, 4, 7, 8, 11, 12, 15, 16}; !slices.Equal(got, want) {
		t.Errorf("bricks opened = %v, want %v", got, want)
	}
	if hammerClaims != 2 {
		t.Errorf("free hammer claims = %d, want 2 (stage 1, then the next stage's NotBegin)", hammerClaims)
	}
	if n := len(f.only(digCityChestCmd)); n != 1 {
		t.Errorf("stage chest claims = %d, want 1", n)
	}
}

// vaultInfo is a hero.dispatch.get.dig.game.info reply.
func vaultInfo(stage, timed *sfs.SFSObject, boxes map[int32]int64) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	if stage != nil {
		r.PutSFSObject("levelMap", stage)
	}
	if timed != nil {
		r.PutSFSObject("timeLimitMap", timed)
	}
	list := sfs.NewSFSArray()
	for _, cfg := range slices.Sorted(func(yield func(int32) bool) {
		for k := range boxes {
			if !yield(k) {
				return
			}
		}
	}) {
		e := sfs.NewSFSObject()
		e.PutInt("mapConfigId", cfg)
		e.PutInt("rewardState", int32(boxes[cfg]))
		list.AddSFSObject(e)
	}
	r.PutSFSArray("mapBoxList", list)
	r.PutInt("isHammerGetMax", 0)
	return r
}

func TestTreasureDigsVaultStage(t *testing.T) {
	stage := digBoardSFS(501, 11001, digCanNotGet, []int{1, 2, 7}, map[int32]int{20009: 1})
	conn, f := startDigFake(t, radarScoringDay(), nil, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case digVaultInfoCmd:
			return vaultInfo(stage, nil, map[int32]int64{11001: digCanNotGet})
		case digVaultOpenCmd:
			r := sfs.NewSFSObject()
			r.PutLong("uuid", p.GetLong("uuid"))
			r.PutInt("pos", p.GetInt("pos"))
			if p.GetLong("uuid") == 501 {
				r.PutInt("rewardState", int32(digCanGet))
				r.PutInt("mapConfigId", 11001)
			}
			return r
		case digVaultStageChestCmd:
			r := vaultInfo(nil, nil, map[int32]int64{11001: digHaveGot})
			r.PutSFSObject("newMap", digBoardSFS(502, 11002, digCanNotGet, nil, map[int32]int{20009: 20}))
			return r
		}
		return nil
	})
	in := digTestInit(map[string]int32{digVaultHammer: 2}, evActivity(2200011))
	if err := runTreasureDigs(conn, in); err != nil {
		t.Fatal(err)
	}
	checkDigSent(t, f)
	var got []string
	for _, r := range f.requests() {
		switch r.Cmd {
		case digVaultInfoCmd, digVaultStageChestCmd:
			got = append(got, r.Cmd)
		case digVaultOpenCmd:
			got = append(got, fmt.Sprintf("%s %d %d %d", r.Cmd, r.Params.GetLong("uuid"), r.Params.GetInt("pos"),
				r.Params.GetInt("checkOpenAll")))
		}
	}
	want := []string{digVaultInfoCmd,
		digVaultOpenCmd + " 501 8 1", // brick 8 finishes the only block: checkOpenAll 1
		digVaultStageChestCmd,
		digVaultOpenCmd + " 502 1 0", // the next stage, its block unseen: index order
	}
	if !slices.Equal(got, want) {
		t.Errorf("vault requests =\n%v\nwant\n%v", got, want)
	}
}

func TestTreasureDigsVaultTimedStage(t *testing.T) {
	stage := digBoardSFS(501, 11001, digCanNotGet, nil, nil)
	timedAt := func(state int64, end time.Time) *sfs.SFSObject {
		o := digBoardSFS(601, 12001, state, nil, nil)
		o.PutLong("endTime", end.UnixMilli())
		return o
	}
	cases := []struct {
		name      string
		timed     *sfs.SFSObject
		held      int32
		wantCmds  []string // timed-stage commands, in order
		stageOpen bool     // the stage is dug afterwards
	}{
		{"not begun, too few hammers to finish it", timedAt(digNotBegin, time.Time{}), 10, nil, false},
		{"not begun, enough hammers", timedAt(digNotBegin, time.Time{}), 11,
			[]string{digVaultTimedStartCmd, digVaultOpenCmd, digVaultTimedChestCmd}, true},
		{"chest ready", timedAt(digCanGet, evTestNow.Add(time.Minute)), 1, []string{digVaultTimedChestCmd}, true},
		{"failed", timedAt(digFail, evTestNow.Add(time.Minute)), 1, []string{digVaultTimedFailCmd}, true},
		{"time ran out", timedAt(digCanNotGet, evTestNow.Add(-time.Second)), 1, []string{digVaultTimedFailCmd}, true},
		{"done, still in its time", timedAt(digHaveGot, evTestNow.Add(time.Minute)), 1, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conn, f := startDigFake(t, radarScoringDay(), nil, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case digVaultInfoCmd:
					return vaultInfo(stage, tc.timed, nil)
				case digVaultTimedStartCmd:
					r := hammerGrant(digVaultHammer, tc.held+25, 25)
					started := digBoardSFS(601, 12001, digCanNotGet, nil, nil)
					started.PutLong("endTime", evTestNow.Add(3*time.Minute).UnixMilli())
					r.PutSFSObject("gameMap", started)
					return r
				case digVaultOpenCmd:
					r := sfs.NewSFSObject()
					r.PutLong("uuid", p.GetLong("uuid"))
					r.PutInt("pos", p.GetInt("pos"))
					if p.GetLong("uuid") == 601 {
						r.PutInt("rewardState", int32(digCanGet)) // the key is under the first brick
					}
					return r
				}
				return nil
			})
			in := digTestInit(map[string]int32{digVaultHammer: tc.held}, evActivity(2200021))
			if err := runTreasureDigs(conn, in); err != nil {
				t.Fatal(err)
			}
			checkDigSent(t, f)
			var timedCmds []string
			stageOpens := 0
			for _, r := range f.requests() {
				switch {
				case r.Cmd == digVaultOpenCmd && r.Params.GetLong("uuid") == 501:
					stageOpens++
				case r.Cmd == digVaultOpenCmd && r.Params.GetLong("uuid") == 601:
					if r.Params.GetInt("checkOpenAll") != 0 {
						t.Error("a timed-stage open must send checkOpenAll 0")
					}
					timedCmds = append(timedCmds, r.Cmd)
				case r.Cmd == digVaultTimedStartCmd || r.Cmd == digVaultTimedChestCmd || r.Cmd == digVaultTimedFailCmd:
					if r.Params.GetLong("uuid") != 601 {
						t.Errorf("%s for uuid %d", r.Cmd, r.Params.GetLong("uuid"))
					}
					timedCmds = append(timedCmds, r.Cmd)
				}
			}
			if !slices.Equal(timedCmds, tc.wantCmds) {
				t.Errorf("timed-stage requests = %v, want %v", timedCmds, tc.wantCmds)
			}
			if (stageOpens > 0) != tc.stageOpen {
				t.Errorf("stage opens = %d, want any: %v", stageOpens, tc.stageOpen)
			}
			if n := len(f.only("hero.dispatch.dig.game.give.up")); n != 0 {
				t.Error("give.up must never be sent")
			}
		})
	}
}

func TestTreasureDigsVaultClosedActivitySendsNothing(t *testing.T) {
	conn, f := startDigFake(t, radarScoringDay(), nil, nil)
	closed := evActivity(2200011)
	closed.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	if err := runTreasureDigs(conn, digTestInit(map[string]int32{digVaultHammer: 10}, closed)); err != nil {
		t.Fatal(err)
	}
	if n := len(f.only(digVaultInfoCmd)) + len(f.only(digTrackInfoCmd)); n != 0 {
		t.Errorf("read a closed activity %d times", n)
	}
}

func TestTreasureDigsChestTrack(t *testing.T) {
	track := func(claimed ...int64) *sfs.SFSObject {
		list := sfs.NewSFSArray()
		for _, target := range []int64{50, 100, 200} {
			e := sfs.NewSFSObject()
			e.PutInt("target", int32(target))
			got := int32(0)
			if slices.Contains(claimed, target) {
				got = 1
			}
			e.PutInt("isReward", got)
			list.AddSFSObject(e)
		}
		r := sfs.NewSFSObject()
		r.PutSFSArray("rewardList", list)
		return r
	}
	conn, f := startDigFake(t, radarScoringDay(), nil, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case digTrackInfoCmd:
			return track(50)
		case digTrackChestCmd:
			return track(50, 100)
		}
		return nil
	})
	if err := runTreasureDigs(conn, digTestInit(map[string]int32{digTrackScore: 120}, evActivity(2200030))); err != nil {
		t.Fatal(err)
	}
	checkDigSent(t, f)
	if n := len(f.only(digTrackChestCmd)); n != 1 {
		t.Errorf("chest track claims = %d, want 1 (only target 100 is reached and unclaimed)", n)
	}
}

func TestTreasureDigsPlanIsReadOnly(t *testing.T) {
	ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(4*time.Hour), digBoardSFS(9001, 12501, digNotBegin, nil, nil))
	off := digRuinEvent(901, radarStateNotInWorld, evTestNow.Add(4*time.Hour), nil)
	conn, f := startDigFake(t, radarScoringDay(), []*sfs.SFSObject{ruin, off}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case digVaultInfoCmd:
			timed := digBoardSFS(601, 12001, digNotBegin, nil, nil)
			return vaultInfo(digBoardSFS(501, 11001, digCanNotGet, nil, nil), timed, map[int32]int64{11001: digCanGet})
		case digTrackInfoCmd:
			e := sfs.NewSFSObject()
			e.PutInt("target", 10)
			e.PutInt("isReward", 0)
			list := sfs.NewSFSArray()
			list.AddSFSObject(e)
			r := sfs.NewSFSObject()
			r.PutSFSArray("rewardList", list)
			return r
		}
		return nil
	})
	in := digTestInit(map[string]int32{digVaultHammer: 40, digRuinHammer: 5, digTrackScore: 20}, evActivity(2200011), evActivity(2200030))
	in.Buildings = append(in.Buildings,
		cityRuinBuilding(77, digCityBuilding, digBoardSFS(9100, 13501, digNotBegin, nil, nil), map[int32]int64{13501: digCanGet}))
	if err := runTreasureDigsPlan(conn, in); err != nil {
		t.Fatal(err)
	}
	reads := []string{"hero.event.info.get", radarInfoCmd, digVaultInfoCmd, digTrackInfoCmd}
	for _, cmd := range f.cmds() {
		if !slices.Contains(reads, cmd) {
			t.Errorf("the plan sent %s", cmd)
		}
	}
	for _, cmd := range []string{radarInfoCmd, digVaultInfoCmd, digTrackInfoCmd} {
		if len(f.only(cmd)) != 1 {
			t.Errorf("the plan read %s %d times, want once", cmd, len(f.only(cmd)))
		}
	}
}

func TestTreasureDigsStopsOnDeadConnection(t *testing.T) {
	noDigPause(t)
	withEvNow(t, evTestNow)
	ruin := digRuinEvent(900, radarStateNotFinish, evTestNow.Add(time.Hour), digBoardSFS(9001, 12501, digCanNotGet, nil, nil))
	conn, f := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case radarInfoCmd:
			return detectReply(20, 10, ruin)
		case digRadarOpenCmd:
			return nil // the server drops the connection
		}
		return evOK()
	})
	in := digTestInit(map[string]int32{digRuinHammer: 10, digVaultHammer: 10}, evActivity(2200011))
	err := runTreasureDigs(conn, in)
	if !session.ContainsNonTimeoutNetError(err) {
		t.Fatalf("err = %v, want the dead connection", err)
	}
	if got := f.cmds(); !slices.Equal(got, []string{radarInfoCmd, digRadarOpenCmd}) {
		t.Errorf("requests = %v, want nothing after the connection died", got)
	}
}
