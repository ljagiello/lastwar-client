package game

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Fake players. The logs must never contain any of these uids or names.
const (
	fwSelf     = "uid-self-0001"
	fwAllyA    = "uid-ally-aaaa1111"
	fwAllyB    = "uid-ally-bbbb2222"
	fwAway     = "uid-away-cccc3333"
	fwStranger = "uid-stranger-ffff"
	fwLauncher = "uid-launcher-9999"
	fwAlliance = "al-fake-0001"
	fwServer   = 783
)

var fwAllUIDs = []string{fwSelf, fwAllyA, fwAllyB, fwAway, fwStranger, fwLauncher}

func fwTestInit() *Init {
	in := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("uid", fwSelf)
	u.PutInt("serverId", fwServer)
	u.PutUtfString("allianceId", fwAlliance)
	in.Raw.PutSFSObject("user", u)
	a := sfs.NewSFSObject()
	a.PutUtfString("uid", fwAlliance)
	in.Raw.PutSFSObject("alliance", a)
	return in
}

func fwMemberEntry(uid string, point int64, server, rank int32) *sfs.SFSObject {
	m := sfs.NewSFSObject()
	m.PutUtfString("uid", uid)
	m.PutUtfString("name", "Name of "+uid)
	if point != 0 {
		m.PutInt("pointId", int32(point))
	}
	m.PutInt("serverId", fwServer)
	m.PutInt("curServerId", server)
	m.PutInt("rank", rank)
	return m
}

// fwRankReply lists: self at 347513 (block 3451), ally A at 347520 (block 3451), ally B at 500001
// (block 5000), a member on another server, one without a point and one off the 1000-tile map.
func fwRankReply() *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutUtfString("allianceId", fwAlliance)
	l := sfs.NewSFSArray()
	l.AddSFSObject(fwMemberEntry(fwSelf, 347513, fwServer, 3))
	l.AddSFSObject(fwMemberEntry(fwAllyA, 347520, fwServer, 4))
	l.AddSFSObject(fwMemberEntry(fwAllyB, 500001, fwServer, 1))
	l.AddSFSObject(fwMemberEntry(fwAway, 123456, 999, 1))
	l.AddSFSObject(fwMemberEntry("uid-nopoint-dddd", 0, fwServer, 1))
	l.AddSFSObject(fwMemberEntry("uid-far-eeee5555", 2000001, fwServer, 1))
	r.PutSFSArray("list", l)
	return r
}

// pbHQ builds a WorldPointInfo for a player HQ at pointID owned by owner in alliance, with the
// given chests, the way the server sends it in world.get.block's points[]. The name field (14),
// which the scan never reads, carries the owner uid so a leak into the logs would show.
func pbHQ(pointID int32, owner, alliance string, gifts ...fireworkGift) []byte {
	var bi []byte
	bi = pbBytesField(bi, 1, []byte(owner))
	bi = pbVarintField(bi, 2, int64(pointID)*7)
	bi = pbVarintField(bi, 3, claimHQBuildingID)
	bi = pbBytesField(bi, 7, []byte(alliance))
	bi = pbBytesField(bi, 14, []byte("name of "+owner))
	for _, g := range gifts {
		bi = pbBytesField(bi, 54, pbGift(g))
	}
	var p []byte
	p = pbVarintField(p, 1, int64(pointID))
	p = pbVarintField(p, 2, 1)
	p = pbBytesField(p, 3, bi)
	return pbVarintField(p, 102, fwServer)
}

// fwChest is a chest sent age before evTestNow.
func fwChest(uuid int64, cfg, num, maxClaims int32, age time.Duration, sender string) fireworkGift {
	return fireworkGift{UUID: uuid, ConfigID: cfg, Num: num, Max: maxClaims, SendTime: evTestNow.Add(-age).UnixMilli(), SendUID: sender}
}

// fwBlockReply holds, on the tiles: ally A's HQ with one claimable chest (9001) and three that
// aren't (full, expired, not sent yet); the player's own HQ with a claimable type-1 chest (9005);
// a non-member's HQ with a chest (9006); ally B's building tagged with another alliance carrying a
// type-1 chest (9007); a resource point; an undecodable blob; and ally A's HQ again in alInfos.
func fwBlockReply(serverNow time.Time) *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance,
		fwChest(9001, 661501, 3, 10, 10*time.Minute, fwAllyB),
		fwChest(9002, 661502, 15, 15, 5*time.Minute, fwAllyB),
		fwChest(9003, 661503, 0, 15, 121*time.Minute, fwAllyB),
		fwChest(9004, 661501, 0, 10, -time.Minute, fwAllyB)))
	pts.AddByteArray(pbHQ(347513, fwSelf, fwAlliance, fwChest(9005, 671003, 50, 100, 30*time.Minute, fwLauncher)))
	pts.AddByteArray(pbHQ(347515, fwStranger, "al-other", fwChest(9006, 661501, 0, 10, time.Minute, fwStranger)))
	pts.AddByteArray(pbHQ(500001, fwAllyB, "al-other", fwChest(9007, 671001, 0, 100, time.Minute, fwAllyB)))
	pts.AddByteArray([]byte{0x08, 0x05, 0x10, 0x03})
	pts.AddByteArray([]byte{0x1a, 0x05, 0x0a})
	al := sfs.NewSFSArray()
	al.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance, fwChest(9001, 661501, 3, 10, 10*time.Minute, fwAllyB)))
	r := sfs.NewSFSObject()
	r.PutInt("serverId", fwServer)
	r.PutSFSArray("points", pts)
	r.PutSFSArray("alInfos", al)
	r.PutLong("timeStamp", serverNow.UnixMilli())
	return r
}

// fwClaimOK is a successful claim reply; like the live one it names the launcher.
func fwClaimOK(p *sfs.SFSObject) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutLong("uuid", p.GetLong("uuid"))
	info := sfs.NewSFSObject()
	info.PutUtfString("uid", fwLauncher)
	info.PutUtfString("name", "Name of "+fwLauncher)
	r.PutSFSObject("sendGiftPlayerInfo", info)
	rw := sfs.NewSFSArray()
	item := sfs.NewSFSObject()
	item.PutInt("type", 0)
	item.PutInt("itemId", 200000)
	item.PutInt("count", 2)
	rw.AddSFSObject(item)
	r.PutSFSArray("reward", rw)
	r.PutBool("isDouble", false)
	return r
}

// startFwFake answers al.rank and world.get.block with the fixtures and every claim with claim(n),
// n counting claims from 1 (fwClaimOK when claim is nil).
func startFwFake(t *testing.T, block *sfs.SFSObject, claim func(n int, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	n := 0
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case fireworksRankCmd:
			return fwRankReply()
		case fireworksBlockCmd:
			return block
		case fireworksGiftCmd:
			n++
			if claim != nil {
				return claim(n, p)
			}
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
}

// withFwPace records the pauses between claims instead of sleeping.
func withFwPace(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	saved := fwSleep
	fwSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { fwSleep = saved })
	return &slept
}

// lockedBuffer is a log sink safe to share with the fake server's goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureFwLogs(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

func TestFireworksBlockArithmetic(t *testing.T) {
	tiles := []struct {
		point      int64
		x, y, blk  int
		lb, rt     int32
		camX, camY int32
	}{
		{347513, 512, 347, 3451, 340511, 350521, 515, 345},   // FIREWORKS.md §1.2 worked example
		{1, 0, 0, 0, 1, 10011, 5, 5},                         // first tile
		{1000, 999, 0, 99, 991, 11000, 995, 5},               // end of the first row: rightTop's x clamps to 999
		{1001, 0, 1, 0, 1, 10011, 5, 5},                      // start of the second row
		{1000000, 999, 999, 9999, 990991, 1000000, 995, 995}, // last tile: rightTop clamps to (999, 999), not 0
	}
	for _, c := range tiles {
		x, y, ok := fwTilePos(c.point)
		if !ok || x != c.x || y != c.y || fwBlockIndex(x, y) != c.blk {
			t.Errorf("point %d: tile (%d, %d) ok=%v block %d, want (%d, %d) block %d", c.point, x, y, ok, fwBlockIndex(x, y), c.x, c.y, c.blk)
		}
		if p := fwTileIndex(x, y); int64(p) != c.point {
			t.Errorf("point %d: TilePosToIndex round trip = %d", c.point, p)
		}
		reqs := fwPlanBlocks([]int64{c.point})
		if len(reqs) != 1 {
			t.Fatalf("point %d: %d requests", c.point, len(reqs))
		}
		r := reqs[0]
		if !slices.Equal(r.Index, []int32{int32(c.blk)}) || r.LeftBottom != c.lb || r.RightTop != c.rt || r.X != c.camX || r.Y != c.camY {
			t.Errorf("point %d: request %+v, want index %d lb %d rt %d camera (%d, %d)", c.point, r, c.blk, c.lb, c.rt, c.camX, c.camY)
		}
	}
	for _, p := range []int64{0, -5, 1000001} {
		if _, _, ok := fwTilePos(p); ok {
			t.Errorf("point %d is off the map", p)
		}
	}
	if reqs := fwPlanBlocks([]int64{0, 1000001}); reqs != nil {
		t.Errorf("off-map points planned %+v", reqs)
	}

	// Two HQs in block 3451 and one in 5000: one request, blocks deduplicated and sorted, the
	// rectangle spanning both: tiles (0, 340) to (520, 510).
	reqs := fwPlanBlocks([]int64{500001, 347513, 347520})
	if len(reqs) != 1 || !slices.Equal(reqs[0].Index, []int32{3451, 5000}) || reqs[0].LeftBottom != 340001 ||
		reqs[0].RightTop != 510521 || reqs[0].X != 260 || reqs[0].Y != 425 {
		t.Errorf("plan = %+v", reqs)
	}
}

func TestFireworksPlanSplitsAt160Blocks(t *testing.T) {
	var points []int64
	for i := range 161 {
		bx, by := i%fwBlockCount, i/fwBlockCount
		points = append(points, int64(bx*fwBlockSize+by*fwBlockSize*fwMapTiles+1))
	}
	reqs := fwPlanBlocks(points)
	if len(reqs) != 2 || len(reqs[0].Index) != 160 || len(reqs[1].Index) != 1 || reqs[1].Index[0] != 160 {
		t.Fatalf("plan: %d requests, sizes %d", len(reqs), len(reqs[0].Index))
	}
	if reqs[0].LeftBottom != 1 || reqs[0].RightTop != fwTileIndex(1000, 20) {
		t.Errorf("first rectangle %d..%d, want rows 0-1 of blocks", reqs[0].LeftBottom, reqs[0].RightTop)
	}
}

func TestFireworksBlockParamsMatchTheClient(t *testing.T) {
	r := fwBlockRequest{X: 515, Y: 345, Index: []int32{3451}, LeftBottom: 340511, RightTop: 350521}
	now := time.UnixMilli(1791212400123)
	p := fwBlockParams(r, fwServer, true, now)
	wantKeys := []string{"bigMap", "x", "y", "serverId", "worldId", "type", "viewLvl", "timeStamp", "index", "blockSize",
		"clearUuidSet", "leftBottom", "rightTop"}
	if !slices.Equal(p.Keys(), wantKeys) {
		t.Errorf("keys %v, want %v", p.Keys(), wantKeys)
	}
	ints := map[string]int32{"bigMap": 0, "x": 515, "y": 345, "serverId": fwServer, "worldId": 0, "type": 0, "viewLvl": 0,
		"timeStamp": int32(now.UnixMilli()), "blockSize": 10, "clearUuidSet": 1, "leftBottom": 340511, "rightTop": 350521}
	for k, want := range ints {
		if v, _ := p.Get(k); v.Type != sfs.SFSInt || v.Val != want {
			t.Errorf("%s = %#v, want Int %d", k, v, want)
		}
	}
	if p.GetInt("timeStamp") != 211037691 { // 1791212400123 mod 2^32, C#'s unchecked (int) cast
		t.Errorf("timeStamp = %d, want the low 32 bits of server ms", p.GetInt("timeStamp"))
	}
	if v, _ := p.Get("index"); v.Type != 12 || !slices.Equal(v.Val.([]int32), []int32{3451}) { // 12: IntArray
		t.Errorf("index = %#v, want IntArray [3451]", v)
	}
	if fwBlockParams(r, fwServer, false, now).Has("clearUuidSet") {
		t.Error("only the first request carries clearUuidSet")
	}
}

func TestFireworksScanIsReadOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	conn, fake := startFwFake(t, fwBlockReply(evTestNow), nil)
	if err := runFireworksScan(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block" {
		t.Fatalf("fireworks-scan sent %s; it may send only al.rank and world.get.block", got)
	}
	rank := fake.only(fireworksRankCmd)[0].Params
	if v, _ := rank.Get("allianceId"); v.Type != sfs.SFSUtfString || v.Val != fwAlliance {
		t.Errorf("al.rank allianceId = %#v", v)
	}
	blk := fake.only(fireworksBlockCmd)[0].Params
	if v, _ := blk.Get("index"); !slices.Equal(v.Val.([]int32), []int32{3451, 5000}) {
		t.Errorf("index = %v; the member on another server and the off-map one must not be read", v.Val)
	}
	if blk.GetInt("serverId") != fwServer || blk.GetInt("leftBottom") != 340001 || blk.GetInt("rightTop") != 510521 ||
		blk.GetInt("clearUuidSet") != 1 || blk.GetInt("blockSize") != 10 {
		t.Errorf("world.get.block params %s", blk.StringRedacted())
	}
}

func TestFireworksScanFindsAndJudgesChests(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startFwFake(t, fwBlockReply(evTestNow), nil)
	sc, err := fireworksScan(conn, fwTestInit())
	if err != nil || sc == nil {
		t.Fatalf("scan = %v, %v", sc, err)
	}
	if sc.points != 6 || sc.alInfos != 1 || sc.decodeErrors != 1 || sc.buildings != 5 || len(sc.memberHQs) != 3 ||
		sc.giftLists != 5 || len(sc.boxes) != 7 || !sc.serverClock {
		t.Errorf("scan counts: points %d alInfos %d decodeErrors %d buildings %d memberHQs %d giftLists %d chests %d serverClock %v",
			sc.points, sc.alInfos, sc.decodeErrors, sc.buildings, len(sc.memberHQs), sc.giftLists, len(sc.boxes), sc.serverClock)
	}
	if sc.roster.listed != 6 || len(sc.roster.points) != 3 || sc.roster.away != 1 || sc.roster.noPoint != 1 || sc.roster.outsideMap != 1 {
		t.Errorf("roster %+v", sc.roster)
	}
	want := map[int64]string{9001: "", 9002: "full", 9003: "expired", 9004: "not sent yet", 9005: "",
		9006: "owner not an alliance member", 9007: "type 1 from another alliance"}
	for _, b := range sc.boxes {
		ok, why := sc.eligible(b, nil)
		if why != want[b.UUID] || ok != (why == "") {
			t.Errorf("chest %d: eligible %v %q, want %q", b.UUID, ok, why, want[b.UUID])
		}
	}
}

func TestFireworksEligibilityEdges(t *testing.T) {
	sc := &fwScan{allianceID: fwAlliance, now: evTestNow, roster: fwRoster{byUID: map[string]fwMember{fwAllyA: {}}}}
	box := func(g fireworkGift, owner, alliance string) fwBox {
		return fwBox{fireworkGift: g, OwnerUID: owner, AllianceID: alliance}
	}
	cases := []struct {
		name    string
		b       fwBox
		claimed map[int64]bool
		why     string
	}{
		{"sent just now", box(fwChest(1, 661501, 0, 10, time.Millisecond, ""), fwAllyA, fwAlliance), nil, ""},
		{"sent this very ms", box(fwChest(1, 661501, 0, 10, 0, ""), fwAllyA, fwAlliance), nil, "not sent yet"},
		{"one ms before 120 min", box(fwChest(1, 661501, 0, 10, fwBoxLifetime-time.Millisecond, ""), fwAllyA, fwAlliance), nil, ""},
		{"exactly 120 min", box(fwChest(1, 661501, 0, 10, fwBoxLifetime, ""), fwAllyA, fwAlliance), nil, "expired"},
		{"type 0 expires too", box(fwChest(1, 661501, 0, 10, 3*time.Hour, ""), fwAllyA, "al-other"), nil, "expired"},
		{"one claim left", box(fwChest(1, 661501, 9, 10, time.Minute, ""), fwAllyA, fwAlliance), nil, ""},
		{"no max", box(fwChest(1, 661501, 0, 0, time.Minute, ""), fwAllyA, fwAlliance), nil, "full"},
		{"answered this run", box(fwChest(1, 661501, 0, 10, time.Minute, ""), fwAllyA, fwAlliance), map[int64]bool{1: true}, "already answered this run"},
		{"no uuid", box(fwChest(0, 661501, 0, 10, time.Minute, ""), fwAllyA, fwAlliance), nil, "no uuid"},
		{"no owner", box(fwChest(1, 661501, 0, 10, time.Minute, ""), "", fwAlliance), nil, "owner not an alliance member"},
		{"type 0 needs no alliance match", box(fwChest(1, 661501, 0, 10, time.Minute, ""), fwAllyA, "al-other"), nil, ""},
		{"type 1 same alliance", box(fwChest(1, 671005, 0, 100, time.Minute, ""), fwAllyA, fwAlliance), nil, ""},
		{"type 1 untagged building", box(fwChest(1, 671005, 0, 100, time.Minute, ""), fwAllyA, ""), nil, "type 1 from another alliance"},
		{"sendTime in seconds", box(fireworkGift{UUID: 1, ConfigID: 661501, Max: 10, SendTime: evTestNow.Add(-time.Minute).Unix()}, fwAllyA, fwAlliance), nil, ""},
	}
	for _, c := range cases {
		ok, why := sc.eligible(c.b, c.claimed)
		if why != c.why || ok != (c.why == "") {
			t.Errorf("%s: eligible %v %q, want %q", c.name, ok, why, c.why)
		}
	}
	for cfg, want := range map[int32]int32{661501: 0, 670999: 0, 671000: 0, 671001: 1, 671005: 1, 671006: 0, 681001: 0, 0: 0} {
		if fwType(cfg) != want {
			t.Errorf("fwType(%d) = %d, want %d", cfg, fwType(cfg), want)
		}
	}
}

func TestFireworksClaimsOnlyEligibleChests(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startFwFake(t, fwBlockReply(evTestNow), nil)
	if err := runFireworks(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block,get.fireworks.gift,get.fireworks.gift" {
		t.Fatalf("sent %s", got)
	}
	claims := fake.only(fireworksGiftCmd)
	want := []struct {
		uuid  int64
		owner string
		typ   int32
	}{{9005, fwSelf, 1}, {9001, fwAllyA, 0}} // oldest first
	for i, w := range want {
		p := claims[i].Params
		uuid, _ := p.Get("uuid")
		owner, _ := p.Get("ownerUid")
		typ, _ := p.Get("type")
		if uuid.Type != sfs.SFSLong || uuid.Val != w.uuid || owner.Type != sfs.SFSUtfString || owner.Val != w.owner ||
			typ.Type != sfs.SFSInt || typ.Val != w.typ {
			t.Errorf("claim %d = %s, want uuid Long %d, ownerUid Utf %s, type Int %d", i, p.StringRedacted(), w.uuid, w.owner, w.typ)
		}
		if got := session.ParamKeysWithoutID(p); !slices.Equal(got, []string{"uuid", "ownerUid", "type"}) {
			t.Errorf("claim %d keys %v", i, got)
		}
	}
	if !slices.Equal(*slept, []time.Duration{time.Second}) {
		t.Errorf("paused %v between claims, want one 1s pause", *slept)
	}
}

func TestFireworksUsesTheServerClock(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance, fwChest(9001, 661501, 0, 10, 119*time.Minute, "")))
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("points", pts)
	blk.PutLong("timeStamp", evTestNow.Add(2*time.Minute).UnixMilli()) // the server is 2 min ahead of the host
	conn, fake := startFwFake(t, blk, nil)
	if err := runFireworks(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(fireworksGiftCmd)); n != 0 {
		t.Errorf("claimed a chest that is 121 min old by the server's clock")
	}
}

func TestFireworksReadsServerPointArr(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance, fwChest(9001, 661501, 0, 10, time.Minute, "")))
	part := sfs.NewSFSObject()
	part.PutInt("serverId", fwServer)
	part.PutSFSArray("points", pts)
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(part)
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("serverPointArr", arr)
	conn, fake := startFwFake(t, blk, nil)
	if err := runFireworks(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if c := fake.only(fireworksGiftCmd); len(c) != 1 || c[0].Params.GetLong("uuid") != 9001 {
		t.Errorf("claims %v", fake.cmds())
	}
}

// fwManyEligible is a tile with n claimable chests on ally A's HQ.
func fwManyEligible(n int) *sfs.SFSObject {
	var gifts []fireworkGift
	for i := range n {
		gifts = append(gifts, fwChest(int64(100+i), 661501, 0, 10, time.Duration(n-i)*time.Minute, ""))
	}
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance, gifts...))
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("points", pts)
	return blk
}

func TestFireworksStopsOnDailyCapAndSlowDown(t *testing.T) {
	for _, c := range []struct {
		code    string
		wantErr bool
	}{{fwDailyCapCode, false}, {fwSlowDownCode, true}} {
		t.Run(c.code, func(t *testing.T) {
			withEvNow(t, evTestNow)
			withFwPace(t)
			conn, fake := startFwFake(t, fwManyEligible(3), func(n int, p *sfs.SFSObject) *sfs.SFSObject {
				if n == 2 {
					return evErr(c.code)
				}
				return fwClaimOK(p)
			})
			err := runFireworks(conn, fwTestInit())
			if (err != nil) != c.wantErr || (err != nil && !strings.Contains(err.Error(), c.code)) {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if n := len(fake.only(fireworksGiftCmd)); n != 2 {
				t.Errorf("sent %d claims; the run must stop at the %s answer", n, c.code)
			}
		})
	}
}

func TestFireworksBenignAnswersContinue(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	codes := []string{"firework_tips_1016", "firework_tips_1015", "firework_tips_1014", "E9"}
	conn, fake := startFwFake(t, fwManyEligible(5), func(n int, p *sfs.SFSObject) *sfs.SFSObject {
		if n <= len(codes) {
			return evErr(codes[n-1])
		}
		return fwClaimOK(p)
	})
	err := runFireworks(conn, fwTestInit())
	if err == nil || !strings.Contains(err.Error(), "E9") || strings.Contains(err.Error(), "firework_tips") {
		t.Errorf("err = %v, want only the E9 failure", err)
	}
	if n := len(fake.only(fireworksGiftCmd)); n != 5 {
		t.Errorf("sent %d claims, want all 5", n)
	}
}

func TestFireworksBenignCodesAreRegistered(t *testing.T) {
	for code := range fireworksBenignCodes {
		conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(code) })
		if _, err := session.SendAndWait(conn, "claim", fireworksGiftCmd, sfs.NewSFSObject()); err != nil {
			t.Errorf("%s is not benign for %s: %v", code, fireworksGiftCmd, err)
		}
	}
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(fwDailyCapCode) })
	if _, err := session.SendAndWait(conn, "claim", fireworksGiftCmd, sfs.NewSFSObject()); err == nil {
		t.Error("the daily cap is not a benign answer")
	}
}

func TestFireworksCapsClaimsPerRun(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startFwFake(t, fwManyEligible(fwMaxClaimsPerRun+5), nil)
	if err := runFireworks(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(fireworksGiftCmd)
	if len(claims) != fwMaxClaimsPerRun || len(*slept) != fwMaxClaimsPerRun-1 {
		t.Fatalf("sent %d claims with %d pauses, want %d", len(claims), len(*slept), fwMaxClaimsPerRun)
	}
	if claims[0].Params.GetLong("uuid") != 100 {
		t.Errorf("the oldest chest goes first, got %d", claims[0].Params.GetLong("uuid"))
	}
}

func TestFireworksLogsNoPlayerIDs(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	logs := captureFwLogs(t)
	conn, _ := startFwFake(t, fwBlockReply(evTestNow), nil)
	if err := runFireworks(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, uid := range fwAllUIDs {
		if strings.Contains(out, uid) {
			t.Errorf("the log contains player uid %s", uid)
		}
	}
	for _, want := range []string{"owner=self", "owner=member#1", "owner=non-member", "sender=member#2", "memberHQs=3",
		"field54Present=true", "points=6", "claimed=2", "reward="} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestFireworksFailures(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)

	t.Run("al.rank error", func(t *testing.T) {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E1") })
		if err := runFireworks(conn, fwTestInit()); err == nil || !strings.Contains(err.Error(), "E1") {
			t.Errorf("err = %v", err)
		}
		if got := strings.Join(fake.cmds(), ","); got != "al.rank" {
			t.Errorf("sent %s", got)
		}
	})
	t.Run("world.get.block error", func(t *testing.T) {
		conn, fake := startFwFake(t, evErr("world_tips_1005"), nil)
		if err := runFireworks(conn, fwTestInit()); err == nil || !strings.Contains(err.Error(), "world_tips_1005") {
			t.Errorf("err = %v", err)
		}
		if n := len(fake.only(fireworksGiftCmd)); n != 0 {
			t.Errorf("claimed %d chests without tiles", n)
		}
	})
	t.Run("dead connection", func(t *testing.T) {
		conn, fake := startFwFake(t, fwBlockReply(evTestNow), func(int, *sfs.SFSObject) *sfs.SFSObject { return nil })
		if err := runFireworks(conn, fwTestInit()); !session.ContainsNonTimeoutNetError(err) {
			t.Errorf("err = %v, want a dead-connection error", err)
		}
		if n := len(fake.only(fireworksGiftCmd)); n != 1 {
			t.Errorf("sent %d claims after the connection died", n)
		}
	})
}

func TestFireworksSkipsWithoutAllianceOrInit(t *testing.T) {
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	noAlliance := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("allianceId", "")
	noAlliance.Raw.PutSFSObject("user", u)
	for _, in := range []*Init{nil, {}, noAlliance} {
		if err := runFireworks(conn, in); err != nil {
			t.Error(err)
		}
		if err := runFireworksScan(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v", fake.cmds())
	}
}

func TestFireworksAllianceAndServerFromInit(t *testing.T) {
	in := fwTestInit()
	if fwAllianceID(in) != fwAlliance {
		t.Error("alliance.uid")
	}
	userOnly := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("allianceId", "al-user")
	userOnly.Raw.PutSFSObject("user", u)
	if fwAllianceID(userOnly) != "al-user" {
		t.Error("user.allianceId fallback")
	}
	if s, ok := fwOwnServer(in, nil, fwSelf); !ok || s != fwServer {
		t.Errorf("own server = %d %v", s, ok)
	}
	list := evObjects(fwRankReply(), "list")
	if s, ok := fwOwnServer(&Init{Raw: sfs.NewSFSObject()}, list, fwSelf); !ok || s != fwServer {
		t.Errorf("own server from al.rank = %d %v", s, ok)
	}
	if _, ok := fwOwnServer(&Init{Raw: sfs.NewSFSObject()}, list, "uid-unknown"); ok {
		t.Error("no own server without user.serverId or an own al.rank entry")
	}
}

func TestFireworksFeaturesAreOptIn(t *testing.T) {
	for _, name := range []string{"fireworks", "fireworks-scan"} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || len(f.Duel) != 0 || f.Hold {
			t.Errorf("%s: registered %v, DefaultOn %v, Duel %v, Hold %v", name, ok, f.DefaultOn, f.Duel, f.Hold)
		}
	}
	if !strings.HasPrefix(featureRegistry["fireworks"].Summary, "OPT-IN") || !strings.HasPrefix(featureRegistry["fireworks-scan"].Summary, "read-only") {
		t.Error("summaries")
	}
}
