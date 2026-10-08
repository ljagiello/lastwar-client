package game

import (
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Fake claimers on the treasures. Like the fireworks fixtures' uids, the logs must never contain them.
const (
	atClaimerA = "uid-claimer-aaaa"
	atClaimerB = "uid-claimer-bbbb"
)

// atFix is one treasure fixture: a point at pointID, with the TreasurePointInfo built from the
// fields. pointUUID is WorldPointInfo.uuid (0: leave it out) and server its serverId (0: leave out).
type atFix struct {
	pointID, pointType int32
	pointUUID          int64
	server             int32
	uuid               int64
	owner, alliance    string
	eventID            string
	typ                int32
	complete           bool
	completedAgo       time.Duration // completionTime = now - completedAgo; 0 leaves it out
	expiresIn          time.Duration // expireTime = now + expiresIn; 0 leaves it out
	multiple           int32
	claimers           []string
}

// pbTreasurePoint encodes f the way the server sends a treasure in world.get.block's points[]. The
// owner's name (field 12) carries the owner uid, so a leak of it into the logs would show.
func pbTreasurePoint(f atFix) []byte {
	var tr []byte
	tr = pbVarintField(tr, 1, f.uuid)
	tr = pbBytesField(tr, 2, []byte(f.owner))
	tr = pbBytesField(tr, 3, []byte(f.eventID))
	if f.completedAgo != 0 {
		tr = pbVarintField(tr, 4, evTestNow.Add(-f.completedAgo).UnixMilli())
	}
	tr = pbBytesField(tr, 5, []byte(f.alliance))
	for _, c := range f.claimers {
		tr = pbBytesField(tr, 7, []byte(c))
	}
	if f.complete {
		tr = pbVarintField(tr, 10, 1)
	}
	tr = pbBytesField(tr, 12, []byte("name of "+f.owner))
	if f.expiresIn != 0 {
		tr = pbVarintField(tr, 13, evTestNow.Add(f.expiresIn).UnixMilli())
	}
	tr = pbVarintField(tr, 14, int64(f.typ))
	if f.multiple != 0 {
		tr = pbVarintField(tr, 18, int64(f.multiple))
	}
	var p []byte
	p = pbVarintField(p, 1, int64(f.pointID))
	p = pbVarintField(p, 2, int64(f.pointType))
	p = pbBytesField(p, 11, tr)
	if f.pointUUID != 0 {
		p = pbVarintField(p, 100, f.pointUUID)
	}
	if f.server != 0 {
		p = pbVarintField(p, 102, int64(f.server))
	}
	return p
}

// atDug is a claimable radar treasure of ally A 20 tiles east of their HQ (347520): dug a minute
// ago, despawning in expiresIn, with three claimers.
func atDug(uuid int64, expiresIn time.Duration) atFix {
	return atFix{pointID: 347540, pointType: atPointTypeTreasure, pointUUID: uuid, server: fwServer, uuid: uuid,
		owner: fwAllyA, alliance: fwAlliance, eventID: "25001", typ: 1, complete: true, completedAgo: time.Minute,
		expiresIn: expiresIn, claimers: []string{atClaimerA, atClaimerB, fwStranger}}
}

func atWith(f atFix, edit func(*atFix)) atFix {
	edit(&f)
	return f
}

// atMany is n distinct fake claimer uids.
func atMany(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "uid-many-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
	}
	return out
}

// atFixtures are the treasures on the tiles, with the verdict each must get.
var atFixtures = []struct {
	f   atFix
	why string
}{
	{atDug(7001, 3*time.Hour), ""},
	{atWith(atDug(7002, 3*time.Hour), func(f *atFix) { f.complete, f.completedAgo = false, -time.Hour }), "still digging"},
	// Dug by the clock though complete is unset, on ally B's side, despawning first.
	{atWith(atDug(7003, time.Hour), func(f *atFix) { f.complete, f.owner, f.pointID = false, fwAllyB, 500021 }), ""},
	{atWith(atDug(7004, 3*time.Hour), func(f *atFix) { f.claimers = atMany(20) }), "full"},
	// multiple 2 doubles the cap to 40; no WorldPointInfo uuid, so TreasurePointInfo.uuid is used.
	{atWith(atDug(7005, 2*time.Hour), func(f *atFix) { f.claimers, f.multiple, f.pointUUID = atMany(25), 2, 0 }), ""},
	{atWith(atDug(7006, 3*time.Hour), func(f *atFix) { f.claimers = []string{atClaimerA, fwSelf} }), "already claimed by you"},
	{atWith(atDug(7007, 3*time.Hour), func(f *atFix) { f.owner = fwSelf }), "own treasure"},
	{atWith(atDug(7008, 3*time.Hour), func(f *atFix) { f.alliance = "al-other" }), "another alliance's treasure"},
	{atWith(atDug(7009, 3*time.Hour), func(f *atFix) { f.alliance = "" }), "another alliance's treasure"},
	{atWith(atDug(7010, -time.Minute), func(*atFix) {}), "expired"},
	{atWith(atDug(7011, 3*time.Hour), func(f *atFix) { f.eventID = "24000" }), "not a treasure event"}, // Help Teammates
	{atWith(atDug(7012, 3*time.Hour), func(f *atFix) { f.eventID = "abc" }), "not a treasure event"},
	{atWith(atDug(7013, 3*time.Hour), func(f *atFix) { f.typ = 2 }), "not a radar treasure"}, // SiegeTreasure
	{atWith(atDug(7014, 3*time.Hour), func(f *atFix) { f.pointType = 1 }), "not a treasure point"},
	{atWith(atDug(7015, 3*time.Hour), func(f *atFix) { f.server = 999 }), "on another server"},
	// A season-1 radar treasure holds 10 claims.
	{atWith(atDug(7016, 3*time.Hour), func(f *atFix) { f.eventID, f.claimers = "1025001", atMany(10) }), "full"},
	{atWith(atDug(7017, 3*time.Hour), func(f *atFix) { f.uuid, f.pointUUID = 0, 0 }), "no uuid"},
}

// atBlockReply holds the fixtures, ally A's HQ, an undecodable blob and treasure 7001 again in
// alInfos.
func atBlockReply(serverNow time.Time) *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbHQ(347520, fwAllyA, fwAlliance))
	for _, c := range atFixtures {
		pts.AddByteArray(pbTreasurePoint(c.f))
	}
	pts.AddByteArray([]byte{0x1a, 0x05, 0x0a})
	al := sfs.NewSFSArray()
	al.AddByteArray(pbTreasurePoint(atFixtures[0].f))
	r := sfs.NewSFSObject()
	r.PutInt("serverId", fwServer)
	r.PutSFSArray("points", pts)
	r.PutSFSArray("alInfos", al)
	r.PutLong("timeStamp", serverNow.UnixMilli())
	return r
}

// atClaimOK is a successful claim reply; like the client's handler expects, it names the owner.
func atClaimOK(p *sfs.SFSObject) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutLong("uuid", p.GetLong("uuid"))
	owner := sfs.NewSFSObject()
	owner.PutUtfString("uid", fwAllyA)
	owner.PutUtfString("name", "Name of "+fwAllyA)
	r.PutSFSObject("ownerInfo", owner)
	rw := sfs.NewSFSArray()
	item := sfs.NewSFSObject()
	item.PutInt("type", 0)
	item.PutInt("itemId", 200000)
	item.PutInt("count", 2)
	rw.AddSFSObject(item)
	r.PutSFSArray("reward", rw)
	r.PutBool("isBigReward", true)
	r.PutInt("bigRewardMultiple", 2)
	r.PutInt("cfgId", 25001)
	return r
}

// startAtFake answers al.rank and world.get.block with the fixtures and every claim with claim(n),
// n counting claims from 1 (atClaimOK when claim is nil).
func startAtFake(t *testing.T, block *sfs.SFSObject, claim func(n int, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	n := 0
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case fireworksRankCmd:
			return fwRankReply()
		case fireworksBlockCmd:
			return block
		case allianceTreasureClaimCmd:
			n++
			if claim != nil {
				return claim(n, p)
			}
			return atClaimOK(p)
		}
		return evErr("unexpected command")
	})
}

func TestRadarTreasureCaps(t *testing.T) {
	for id, want := range map[int64]int32{25001: 20, 25198: 20, 1025001: 10, 1025168: 10, 27000: 20, 27015: 50, 505601: 100} {
		if got, ok := radarTreasureCap(id); !ok || got != want {
			t.Errorf("cap(%d) = %d %v, want %d", id, got, ok, want)
		}
	}
	for _, id := range []int64{0, 25000, 25199, 27003, 24000, 1025169} {
		if got, ok := radarTreasureCap(id); ok {
			t.Errorf("cap(%d) = %d; not a treasure event", id, got)
		}
	}
	// Every treasure event has a cap, so a type check that passes always finds one.
	for _, r := range radarEventTypeRanges {
		if !slices.Contains(atTreasureEventTypes, r.typ) {
			continue
		}
		for id := r.lo; id <= r.hi; id++ {
			if _, ok := radarTreasureCap(id); !ok {
				t.Errorf("treasure event %d (type %d) has no cap", id, r.typ)
			}
		}
	}
}

func TestAllianceTreasureBlocksAroundHQs(t *testing.T) {
	if !slices.Equal(fwPlanBlocksAround([]int64{347513}, 0)[0].Index, fwPlanBlocks([]int64{347513})[0].Index) {
		t.Error("radius 0 is the HQ's own block")
	}
	// Block (51, 34) widened by 3: x 48-54, y 31-37; tiles (480, 310) to (550, 380).
	reqs := fwPlanBlocksAround([]int64{347513, 347520}, 3)
	if len(reqs) != 1 || len(reqs[0].Index) != 49 || reqs[0].Index[0] != 3148 || reqs[0].Index[48] != 3754 ||
		reqs[0].LeftBottom != fwTileIndex(480, 310) || reqs[0].RightTop != fwTileIndex(550, 380) {
		t.Errorf("plan %+v", reqs)
	}
	// The corner tile clips to the map: blocks x 0-3, y 0-3.
	if r := fwPlanBlocksAround([]int64{1}, 3); len(r) != 1 || len(r[0].Index) != 16 || r[0].Index[15] != 303 {
		t.Errorf("corner plan %+v", r)
	}
	if r := fwPlanBlocksAround([]int64{1000000}, 3); len(r) != 1 || len(r[0].Index) != 16 || r[0].Index[0] != 9696 {
		t.Errorf("far corner plan %+v", r)
	}
	// Edge HQs: a corner (16 blocks), two edge middles (28) and the centre (49), in one request.
	if r := fwPlanBlocksAround([]int64{1, 500, 500001, 500500, 999001}, 3); len(r) != 1 || len(r[0].Index) != 16+28+28+49+16 {
		t.Errorf("plan sizes %d", len(r[0].Index))
	}
	// 16 + 3 x 49 = 163 blocks split at 160.
	var spread []int64
	for i := range 4 {
		spread = append(spread, int64(i*100*fwMapTiles+i*100+1))
	}
	if r := fwPlanBlocksAround(spread, 3); len(r) != 2 || len(r[0].Index) != fwMaxBlocksPerRequest {
		t.Errorf("spread plan: %d requests", len(r))
	}
	if atChebyshev(347520, 347540) != 20 || atChebyshev(347513, 352513) != 5 || atChebyshev(0, 1) != -1 {
		t.Error("tile distances")
	}
}

func TestAllianceTreasuresPlanIsReadOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	conn, fake := startAtFake(t, atBlockReply(evTestNow), nil)
	if err := runAllianceTreasuresPlan(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "al.rank,world.get.block" {
		t.Fatalf("alliance-treasures-plan sent %s; it may send only al.rank and world.get.block", got)
	}
	blk := fake.only(fireworksBlockCmd)[0].Params
	idx, _ := blk.Get("index")
	// Self and ally A share block 3451, ally B is in 5000: 49 + 28 blocks (x 0-3 at the map's edge).
	if got := idx.Val.([]int32); len(got) != 77 || !slices.Contains(got, 3451) || !slices.Contains(got, 5000) || !slices.Contains(got, 3148) {
		t.Errorf("index has %d blocks", len(got))
	}
	if blk.GetInt("clearUuidSet") != 1 || blk.GetInt("viewLvl") != 0 || blk.GetInt("serverId") != fwServer {
		t.Errorf("world.get.block params %s", blk.StringRedacted())
	}
}

func TestAllianceTreasureVerdicts(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, _ := startAtFake(t, atBlockReply(evTestNow), nil)
	sc, err := allianceTileScan(conn, fwTestInit(), "alliance-treasures", atBlockRadius)
	if err != nil || sc == nil {
		t.Fatalf("scan = %v, %v", sc, err)
	}
	if len(sc.treasures) != len(atFixtures) || sc.decodeErrors != 1 || sc.pointTypes["points"][atPointTypeTreasure] != len(atFixtures)-1 ||
		sc.pointTypes["alInfos"][atPointTypeTreasure] != 1 || sc.server != fwServer {
		t.Fatalf("scan: %d treasures, %d decode errors, point types %v", len(sc.treasures), sc.decodeErrors, sc.pointTypes)
	}
	for i, pt := range sc.treasures {
		a := sc.treasure(pt)
		if why := sc.verdict(a, nil); why != atFixtures[i].why {
			t.Errorf("treasure %d: verdict %q, want %q", atFixtures[i].f.uuid, why, atFixtures[i].why)
		}
	}
	if a := sc.treasure(sc.treasures[4]); a.uuid != 7005 || a.maxClaim != 40 || a.server != fwServer {
		t.Errorf("7005: uuid %d (from TreasurePointInfo), cap %d (20 x multiple 2)", a.uuid, a.maxClaim)
	}
	if a := sc.treasure(sc.treasures[0]); sc.verdict(a, map[int64]bool{7001: true}) != "already answered this run" {
		t.Error("a treasure answered this run is not sent again")
	}
	// A point without serverId is on the map read.
	noServer := sc.treasure(sc.treasures[0])
	noServer.pt.ServerID = 0
	if a := sc.treasure(noServer.pt); a.server != fwServer || sc.verdict(a, nil) != "" {
		t.Errorf("no serverId: server %d", a.server)
	}
	// Times in seconds read like ms ones.
	secs := sc.treasures[0]
	tr := *secs.Treasure
	tr.Complete, tr.CompletionTime, tr.ExpireTime = false, evTestNow.Add(-time.Minute).Unix(), evTestNow.Add(time.Hour).Unix()
	secs.Treasure = &tr
	if why := sc.verdict(sc.treasure(secs), nil); why != "" {
		t.Errorf("times in seconds: %q", why)
	}
	tr.ExpireTime = evTestNow.Add(-time.Second).Unix()
	if why := sc.verdict(sc.treasure(secs), nil); why != "expired" {
		t.Errorf("expired in seconds: %q", why)
	}
	// Without the player's uid nothing is claimable: own treasures and own claims look like others'.
	sc.ownUID = ""
	if why := sc.verdict(sc.treasure(sc.treasures[0]), nil); why != "own uid unknown" {
		t.Errorf("no own uid: %q", why)
	}
}

func TestAllianceTreasuresClaimsOnlyEligible(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startAtFake(t, atBlockReply(evTestNow), nil)
	if err := runAllianceTreasures(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	want := "al.rank,world.get.block," + strings.Repeat(allianceTreasureClaimCmd+",", 3)
	if got := strings.Join(fake.cmds(), ","); got != strings.TrimSuffix(want, ",") {
		t.Fatalf("sent %s", got)
	}
	// The one that despawns first goes first.
	for i, uuid := range []int64{7003, 7005, 7001} {
		p := fake.only(allianceTreasureClaimCmd)[i].Params
		u, _ := p.Get("uuid")
		s, _ := p.Get("targetServer")
		if u.Type != sfs.SFSLong || u.Val != uuid || s.Type != sfs.SFSInt || s.Val != int32(fwServer) {
			t.Errorf("claim %d = %s, want uuid Long %d, targetServer Int %d", i, p.StringRedacted(), uuid, fwServer)
		}
		if got := session.ParamKeysWithoutID(p); !slices.Equal(got, []string{"uuid", "targetServer"}) {
			t.Errorf("claim %d keys %v; the radar branch sends no type", i, got)
		}
	}
	if !slices.Equal(*slept, []time.Duration{atClaimPace, atClaimPace}) {
		t.Errorf("paused %v between claims", *slept)
	}
}

func TestAllianceTreasuresUsesTheServerClock(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	// Completes a minute after the host's clock; the server is two minutes ahead.
	pts := sfs.NewSFSArray()
	pts.AddByteArray(pbTreasurePoint(atWith(atDug(7001, 3*time.Hour), func(f *atFix) { f.complete, f.completedAgo = false, -time.Minute })))
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("points", pts)
	blk.PutLong("timeStamp", evTestNow.Add(2*time.Minute).UnixMilli())
	conn, fake := startAtFake(t, blk, nil)
	if err := runAllianceTreasures(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(allianceTreasureClaimCmd)); n != 1 {
		t.Errorf("sent %d claims; by the server's clock the dig is done", n)
	}
}

// atManyEligible is a tile with n claimable treasures, the first despawning first.
func atManyEligible(n int) *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	for i := range n {
		pts.AddByteArray(pbTreasurePoint(atWith(atDug(int64(100+i), time.Duration(i+1)*time.Minute), func(f *atFix) {
			f.pointID = int32(347540 + i)
		})))
	}
	blk := sfs.NewSFSObject()
	blk.PutSFSArray("points", pts)
	return blk
}

func TestAllianceTreasuresBenignContinuesUnknownStops(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	conn, fake := startAtFake(t, atManyEligible(5), func(n int, p *sfs.SFSObject) *sfs.SFSObject {
		switch n {
		case 1:
			return evErr("801348")
		case 2:
			return evErr("activity_wajueji_27000_tips13")
		case 4:
			return evErr("E_CAP")
		}
		return atClaimOK(p)
	})
	err := runAllianceTreasures(conn, fwTestInit())
	if err == nil || !strings.Contains(err.Error(), "E_CAP") || strings.Contains(err.Error(), "801348") {
		t.Errorf("err = %v, want only the E_CAP stop", err)
	}
	if n := len(fake.only(allianceTreasureClaimCmd)); n != 4 {
		t.Errorf("sent %d claims; the run must stop at the unknown answer", n)
	}
}

func TestAllianceTreasureBenignCodesAreRegistered(t *testing.T) {
	for code := range allianceTreasureBenignCodes {
		conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(code) })
		if _, err := session.SendAndWait(conn, "claim", allianceTreasureClaimCmd, sfs.NewSFSObject()); err != nil {
			t.Errorf("%s is not benign for %s: %v", code, allianceTreasureClaimCmd, err)
		}
	}
}

func TestAllianceTreasuresCapsClaimsPerRun(t *testing.T) {
	withEvNow(t, evTestNow)
	slept := withFwPace(t)
	conn, fake := startAtFake(t, atManyEligible(atMaxClaimsPerRun+5), nil)
	if err := runAllianceTreasures(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	claims := fake.only(allianceTreasureClaimCmd)
	if len(claims) != atMaxClaimsPerRun || len(*slept) != atMaxClaimsPerRun-1 || claims[0].Params.GetLong("uuid") != 100 {
		t.Errorf("sent %d claims with %d pauses, first %d", len(claims), len(*slept), claims[0].Params.GetLong("uuid"))
	}
}

func TestAllianceTreasuresLogsNoPlayerIDs(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	logs := captureFwLogs(t)
	conn, _ := startAtFake(t, atBlockReply(evTestNow), nil)
	if err := runAllianceTreasures(conn, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	out := logs.String()
	for _, uid := range append(slices.Clone(fwAllUIDs), atClaimerA, atClaimerB, "uid-many-aa", fwAlliance, "al-other") {
		if strings.Contains(out, uid) {
			t.Errorf("the log contains %s", uid)
		}
	}
	for _, want := range []string{"owner=member#1", "owner=member#2", "owner=self", "treasurePointsPresent=true", "treasurePoints=17",
		"pointTypes=1:2,21:16", "alInfoTypes=21:1", "distToOwnerHQ=20", "claimed=3", "isBigReward=true", "bigRewardMultiple=2",
		"blockRadius=3", "reason=\"own treasure\""} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestAllianceTreasuresFailures(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	t.Run("world.get.block error", func(t *testing.T) {
		conn, fake := startAtFake(t, evErr("world_tips_1005"), nil)
		if err := runAllianceTreasures(conn, fwTestInit()); err == nil || !strings.Contains(err.Error(), "world_tips_1005") {
			t.Errorf("err = %v", err)
		}
		if n := len(fake.only(allianceTreasureClaimCmd)); n != 0 {
			t.Errorf("claimed %d treasures without tiles", n)
		}
	})
	t.Run("dead connection", func(t *testing.T) {
		conn, fake := startAtFake(t, atBlockReply(evTestNow), func(int, *sfs.SFSObject) *sfs.SFSObject { return nil })
		if err := runAllianceTreasures(conn, fwTestInit()); !session.ContainsNonTimeoutNetError(err) {
			t.Errorf("err = %v, want a dead-connection error", err)
		}
		if n := len(fake.only(allianceTreasureClaimCmd)); n != 1 {
			t.Errorf("sent %d claims after the connection died", n)
		}
	})
	t.Run("no alliance or init", func(t *testing.T) {
		conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		noAlliance := evInit(30)
		u := sfs.NewSFSObject()
		u.PutUtfString("allianceId", "")
		noAlliance.Raw.PutSFSObject("user", u)
		for _, in := range []*Init{nil, {}, noAlliance} {
			if err := runAllianceTreasures(conn, in); err != nil {
				t.Error(err)
			}
			if err := runAllianceTreasuresPlan(conn, in); err != nil {
				t.Error(err)
			}
		}
		if len(fake.cmds()) != 0 {
			t.Errorf("sent %v", fake.cmds())
		}
	})
}

func TestAllianceTreasureFeaturesAreOptIn(t *testing.T) {
	for _, name := range []string{"alliance-treasures", "alliance-treasures-plan"} {
		f, ok := featureRegistry[name]
		if !ok || f.DefaultOn || len(f.Duel) != 0 || f.Hold {
			t.Errorf("%s: registered %v, DefaultOn %v, Duel %v, Hold %v", name, ok, f.DefaultOn, f.Duel, f.Hold)
		}
	}
	if !strings.HasPrefix(featureRegistry["alliance-treasures"].Summary, "OPT-IN") ||
		!strings.HasPrefix(featureRegistry["alliance-treasures-plan"].Summary, "read-only") {
		t.Error("summaries")
	}
}
