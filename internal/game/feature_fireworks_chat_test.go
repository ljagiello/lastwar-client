package game

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/chat"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// The chat twins' fixtures reuse the fireworks ones: the player fwSelf on server 783 in alliance
// fwAlliance, ally A's HQ at 347520 and ally B's at 500001 (fwRankReply).

// fuBuildingOwner owns the alliance building the S0 boss chest sits on; not an alliance member.
const fuBuildingOwner = "uid-albuilding-7777"

// fuChatInit is fwTestInit with the init chatToken the chat read needs.
func fuChatInit() *Init {
	in := fwTestInit()
	in.Raw.PutUtfString("chatToken", "secret-chat-token")
	return in
}

// fuTile is one world point the fake server holds, by point id.
type fuTile struct {
	point int64
	blob  []byte
}

// fuBlockReply answers a world.get.block the way the server's AOI does: the points whose 10-tile
// block is in the request's index, with the server clock.
func fuBlockReply(p *sfs.SFSObject, tiles []fuTile) *sfs.SFSObject {
	v, _ := p.Get("index")
	idx, _ := v.Val.([]int32)
	pts := sfs.NewSFSArray()
	for _, t := range tiles {
		x, y := int((t.point-1)%1000), int((t.point-1)/1000)
		if slices.Contains(idx, int32(y/10*100+x/10)) {
			pts.AddByteArray(t.blob)
		}
	}
	r := sfs.NewSFSObject()
	r.PutInt("serverId", fwServer)
	r.PutSFSArray("points", pts)
	r.PutLong("timeStamp", evTestNow.UnixMilli())
	return r
}

// pbAllianceBuilding is a WorldPointInfo for a non-HQ building at pointID owned by owner in
// alliance, carrying chests (BuildInfo field 54).
func pbAllianceBuilding(pointID int32, owner, alliance string, gifts ...fireworkGift) []byte {
	var bi []byte
	bi = pbBytesField(bi, 1, []byte(owner))
	bi = pbVarintField(bi, 3, 41000000) // not the HQ (10100000)
	bi = pbBytesField(bi, 7, []byte(alliance))
	for _, g := range gifts {
		bi = pbBytesField(bi, 54, pbGift(g))
	}
	var p []byte
	p = pbVarintField(p, 1, int64(pointID))
	p = pbVarintField(p, 2, 1)
	p = pbBytesField(p, 3, bi)
	return pbVarintField(p, 102, fwServer)
}

// fuFireworkTiles: ally A's HQ (347520) with chest 9101 (claimable), 9103 (full) and 9001 (the
// one fwBlockReply has too); the S0 boss chest 9102 (type 1, 671003) on an alliance building at
// 612345, far from every member HQ.
func fuFireworkTiles() []fuTile {
	return []fuTile{
		{347520, pbHQ(347520, fwAllyA, fwAlliance,
			fwChest(9101, 661501, 3, 10, 10*time.Minute, fwAllyB),
			fwChest(9103, 661501, 10, 10, 10*time.Minute, fwAllyB),
			fwChest(9001, 661501, 3, 10, 10*time.Minute, fwAllyB))},
		{612345, pbAllianceBuilding(612345, fuBuildingOwner, fwAlliance, fwChest(9102, 671003, 4, 100, 20*time.Minute, fwLauncher))},
	}
}

// fuFind is a find.fireworks.gift.world.point reply (FindFireworksGiftWorldPointMessage.lua:15).
func fuFind(server, point int64) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("currServerId", int32(server))
	r.PutInt("pointId", int32(point))
	return r
}

// fwCard is a post 693 (or 695) card with the card's field names (FireworkGift.lua:64-103).
func fwCard(post int, seq int64, ago time.Duration, custom, claimers string) chat.Post {
	return chat.Post{Type: post, RoomID: "alliance_783_" + fwAlliance, Group: chat.GroupAlliance, SeqID: seq, Sender: fwLauncher,
		ServerTimeMs: evTestNow.Add(-ago).UnixMilli(), CustomJSON: custom, ClientUpdateExtra: claimers}
}

// startFuFireworks answers al.rank, world.get.block (fuBlockReply over tiles), the lookups from
// finds (by uuid) and every claim with claim (fwClaimOK when nil).
func startFuFireworks(t *testing.T, tiles []fuTile, finds map[int64]*sfs.SFSObject, claim func(p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	saved := fwBlockSleep
	fwBlockSleep = func(time.Duration) {}
	t.Cleanup(func() { fwBlockSleep = saved })
	withFwPace(t)
	return startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "al.rank":
			return fwRankReply()
		case "world.get.block":
			return fuBlockReply(p, tiles)
		case "find.fireworks.gift.world.point":
			if r := finds[p.GetLong("uuid")]; r != nil {
				return r
			}
			return evErr("firework_tips_1018")
		case "get.fireworks.gift":
			if claim != nil {
				return claim(p)
			}
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
}

func TestChatTwinsAreOptInAndStaticOnly(t *testing.T) {
	for _, name := range []string{"fireworks-chat", "alliance-treasures-chat"} {
		f, ok := featureRegistry[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		if f.DefaultOn || len(f.Duel) > 0 || f.Hold {
			t.Errorf("%s: DefaultOn %v Duel %v Hold %v", name, f.DefaultOn, f.Duel, f.Hold)
		}
		if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
			t.Errorf("%s summary %q", name, f.Summary)
		}
	}
	// Each twin runs after its tile feature in the same phase (RunFeatures orders by phase, then
	// name), so the tile feature's answers and stops reach it.
	for twin, tile := range map[string]string{"fireworks-chat": "fireworks", "alliance-treasures-chat": "alliance-treasures"} {
		if featurePhase(twin) != featurePhase(tile) || twin < tile {
			t.Errorf("%s (phase %d) does not run after %s (phase %d)", twin, featurePhase(twin), tile, featurePhase(tile))
		}
	}
}

func TestFireworksChatClaimsTheCardsChests(t *testing.T) {
	withEvNow(t, evTestNow)
	soon := evTestNow.Add(time.Hour).UnixMilli()
	posts := []chat.Post{
		// 693, ten minutes old: its chest on ally A's HQ.
		fwCard(693, 1, 10*time.Minute, `{"uuid":9101,"ownerUid":"uid-ally-aaaa1111","configId":661501,"index":0}`, "uid-x|uid-y"),
		// 695, the S0 alliance-boss chest on an alliance building, expiring in an hour.
		fwCard(695, 2, 5*time.Minute, fmt.Sprintf(`{"uuid":9102,"ownerUid":%q,"configId":671003,"expiredTime":%d,"pointId":612345,"serverId":783}`, fuBuildingOwner, soon), ""),
		fwCard(693, 3, 9*time.Minute, `{"uuid":9101,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),            // the same chest again
		fwCard(693, 4, 120*time.Minute, `{"uuid":9104,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),          // fireworks.k2: 120 min
		fwCard(693, 5, time.Minute, `{"uuid":9105,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, "uid-self-0001"), // claimed by the player
		fwCard(695, 6, time.Minute, fmt.Sprintf(`{"uuid":9106,"ownerUid":"x","configId":671001,"expiredTime":%d}`, evTestNow.UnixMilli()), ""),
		fwCard(693, 7, time.Minute, `{"uuid":9107,"configId":661501}`, ""), // no ownerUid to send
		fwCard(693, 8, 2*time.Minute, `{"uuid":9108,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),
		fwCard(693, 9, 3*time.Minute, `{"uuid":9109,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),
		fwCard(693, 10, 4*time.Minute, `{"uuid":9103,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),
		func() chat.Post {
			p := fwCard(693, 11, 0, `{"uuid":9110,"ownerUid":"a"}`, "")
			p.ServerTimeMs = 0
			return p
		}(),
	}
	withRPChat(t, &chat.Result{RoomID: "alliance_783_" + fwAlliance, Granted: true, Posts: posts}, nil)
	logs := captureFwLogs(t)
	conn, fake := startFuFireworks(t, fuFireworkTiles(), map[int64]*sfs.SFSObject{
		9101: fuFind(783, 347520),
		9102: fuFind(783, 612345),
		9108: fuFind(783, 0),      // gone
		9109: fuFind(999, 6001),   // on another server
		9103: fuFind(783, 347520), // full on the tile
	}, nil)
	if err := runFireworksChat(conn, fuChatInit()); err != nil {
		t.Fatal(err)
	}
	finds := fake.only("find.fireworks.gift.world.point")
	var order []int64
	for _, f := range finds {
		order = append(order, f.Params.GetLong("uuid"))
		if keys := session.ParamKeysWithoutID(f.Params); !slices.Equal(keys, []string{"uuid", "ownerUid"}) {
			t.Errorf("lookup keys %v, want FindFireworksGiftWorldPointMessage's uuid, ownerUid", keys)
		}
		if v, _ := f.Params.Get("uuid"); v.Type != sfs.SFSLong {
			t.Errorf("lookup uuid %#v, want a Long", v)
		}
	}
	if want := []int64{9108, 9109, 9103, 9102, 9101}; !slices.Equal(order, want) {
		t.Errorf("looked up %v, want %v (newest card first; expired, claimed, ownerless, timeless and duplicate cards not)", order, want)
	}
	if i := slices.Index(order, 9102); i >= 0 {
		if v, _ := finds[i].Params.Get("ownerUid"); v.Type != sfs.SFSUtfString || v.Val != fuBuildingOwner {
			t.Errorf("lookup ownerUid type %d, want the card's ownerUid as a UtfString", v.Type)
		}
	}
	blocks := fake.only("world.get.block")
	if len(blocks) != 2 {
		t.Fatalf("sent %d world.get.block, want one per located point's block (2)", len(blocks))
	}
	var idx []int32
	for i, b := range blocks {
		v, _ := b.Params.Get("index")
		idx = append(idx, v.Val.([]int32)...)
		if b.Params.GetInt("serverId") != 783 || b.Params.Has("clearUuidSet") != (i == 0) {
			t.Errorf("block request %d: %s", i, b.Params.StringRedacted())
		}
	}
	if !slices.Equal(idx, []int32{3451, 6134}) { // 347520: tile (519, 347); 612345: tile (344, 612)
		t.Errorf("block indexes %v, want 3451 and 6134", idx)
	}
	claims := fake.only("get.fireworks.gift")
	if len(claims) != 2 {
		t.Fatalf("sent %d claims, want 9102 and 9101", len(claims))
	}
	for i, want := range []struct {
		uuid  int64
		owner string
		typ   int32
	}{{9102, fuBuildingOwner, 1}, {9101, fwAllyA, 0}} {
		p := claims[i].Params
		if p.GetLong("uuid") != want.uuid || p.GetString("ownerUid") != want.owner || p.GetInt("type") != want.typ {
			t.Errorf("claim %d %s, want uuid %d type %d", i, p.StringRedacted(), want.uuid, want.typ)
		}
		if keys := session.ParamKeysWithoutID(p); !slices.Equal(keys, []string{"uuid", "ownerUid", "type"}) {
			t.Errorf("claim keys %v", keys)
		}
	}
	if n := len(fake.only("al.rank")); n != 1 {
		t.Errorf("al.rank sent %d times, want once for the type-0 member check", n)
	}
	out := logs.String()
	for _, want := range []string{
		"first find.fireworks.gift.world.point reply", "chest gone (firework_tips_1018)", "on another server",
		"reason=expired", "already claimed by you", "no ownerUid", "no serverTime", "duplicate card", "reason=full",
		"tileOwnerPresent=true",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q", want)
		}
	}
	for _, leak := range append(slices.Clone(fwAllUIDs), fuBuildingOwner, "secret-chat-token") {
		if strings.Contains(out, leak) {
			t.Errorf("logs carry %q", leak)
		}
	}
}

// TestFireworksChatFollowsFireworksInOneRun: on one (conn, init) fireworks runs first and sends
// what it sends alone; the twin then skips the chest fireworks answered, takes the roster from
// the shared scan (no second al.rank) and reads its own block with clearUuidSet.
func TestFireworksChatFollowsFireworksInOneRun(t *testing.T) {
	withEvNow(t, evTestNow)
	tiles := fuFireworkTiles()
	cards := []chat.Post{
		fwCard(693, 1, 10*time.Minute, `{"uuid":9001,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),
		fwCard(695, 2, 5*time.Minute, fmt.Sprintf(`{"uuid":9102,"ownerUid":%q,"configId":671003}`, fuBuildingOwner), ""),
	}
	finds := map[int64]*sfs.SFSObject{9001: fuFind(783, 347520), 9102: fuFind(783, 612345)}

	alone, aloneFake := startFuFireworks(t, tiles, finds, nil)
	if err := runFireworks(alone, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: cards}, nil)
	conn, fake := startFuFireworks(t, tiles, finds, nil)
	in := fuChatInit()
	if err := runFireworks(conn, in); err != nil {
		t.Fatal(err)
	}
	n := len(fake.requests())
	if got, want := fake.cmds(), aloneFake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("fireworks sent %v, alone %v", got, want)
	}
	if err := runFireworksChat(conn, in); err != nil {
		t.Fatal(err)
	}
	twin := fake.requests()[n:]
	var cmds []string
	for _, r := range twin {
		cmds = append(cmds, r.Cmd)
	}
	if want := []string{"find.fireworks.gift.world.point", "world.get.block", "get.fireworks.gift"}; !slices.Equal(cmds, want) {
		t.Fatalf("the twin sent %v, want %v", cmds, want)
	}
	if twin[0].Params.GetLong("uuid") != 9102 || twin[2].Params.GetLong("uuid") != 9102 {
		t.Errorf("the twin looked up or claimed chest 9001, which fireworks answered")
	}
	if twin[1].Params.GetInt("clearUuidSet") != 1 {
		t.Errorf("the twin's block read is its own scan and carries clearUuidSet: %s", twin[1].Params.StringRedacted())
	}
}

// TestFireworksChatLeavesTheSharedScanAlone: the twin first, then fireworks on the same (conn,
// init): fireworks still sends al.rank and its whole HQ scan, as it does alone.
func TestFireworksChatLeavesTheSharedScanAlone(t *testing.T) {
	withEvNow(t, evTestNow)
	tiles := fuFireworkTiles()
	finds := map[int64]*sfs.SFSObject{9102: fuFind(783, 612345)}
	alone, aloneFake := startFuFireworks(t, tiles, finds, nil)
	if err := runFireworks(alone, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{
		fwCard(695, 2, 5*time.Minute, fmt.Sprintf(`{"uuid":9102,"ownerUid":%q,"configId":671003}`, fuBuildingOwner), ""),
	}}, nil)
	conn, fake := startFuFireworks(t, tiles, finds, nil)
	in := fuChatInit()
	if err := runFireworksChat(conn, in); err != nil {
		t.Fatal(err)
	}
	n := len(fake.requests())
	if err := runFireworks(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests()[n:] {
		got = append(got, r.Cmd)
	}
	if want := aloneFake.cmds(); !slices.Equal(got, want) {
		t.Errorf("fireworks after the twin sent %v, alone %v", got, want)
	}
}

// TestFireworksChatHonoursFireworksStop: a daily cap (firework_tips_1009) or slow-down
// (firework_tips_1017) fireworks got this run stops the twin before any lookup.
func TestFireworksChatHonoursFireworksStop(t *testing.T) {
	for _, code := range []string{"firework_tips_1009", "firework_tips_1017"} {
		t.Run(code, func(t *testing.T) {
			withEvNow(t, evTestNow)
			withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{
				fwCard(695, 2, 5*time.Minute, fmt.Sprintf(`{"uuid":9102,"ownerUid":%q,"configId":671003}`, fuBuildingOwner), ""),
			}}, nil)
			conn, fake := startFuFireworks(t, fuFireworkTiles(), map[int64]*sfs.SFSObject{9102: fuFind(783, 612345)},
				func(*sfs.SFSObject) *sfs.SFSObject { return evErr(code) })
			in := fuChatInit()
			_ = runFireworks(conn, in)
			n := len(fake.requests())
			if err := runFireworksChat(conn, in); err != nil {
				t.Fatal(err)
			}
			if extra := fake.cmds()[n:]; len(extra) != 0 {
				t.Errorf("the twin sent %v after fireworks stopped on %s", extra, code)
			}
		})
	}
}

// TestFireworksChatLookupErrorSkipsTheCard: an errorCode on the lookup skips that card only and is
// not an error of the run; no code is registered as benign.
func TestFireworksChatLookupErrorSkipsTheCard(t *testing.T) {
	withEvNow(t, evTestNow)
	withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{
		fwCard(693, 1, 10*time.Minute, `{"uuid":9101,"ownerUid":"uid-ally-aaaa1111","configId":661501}`, ""),
		fwCard(695, 2, 5*time.Minute, fmt.Sprintf(`{"uuid":9102,"ownerUid":%q,"configId":671003}`, fuBuildingOwner), ""),
	}}, nil)
	conn, fake := startFuFireworks(t, fuFireworkTiles(), map[int64]*sfs.SFSObject{
		9102: evErr("firework_tips_1019"),
		9101: fuFind(783, 347520),
	}, nil)
	if err := runFireworksChat(conn, fuChatInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "find.fireworks.gift.world.point,find.fireworks.gift.world.point,al.rank,world.get.block,get.fireworks.gift" {
		t.Errorf("sent %s", got)
	}
	fuCheckNotBenign(t, "find.fireworks.gift.world.point", "firework_tips_1019", "firework_tips_1018")
}

// fuCheckNotBenign fails when one of codes is registered as benign for cmd: SendAndWait returns
// no error for a benign code.
func fuCheckNotBenign(t *testing.T, cmd string, codes ...string) {
	t.Helper()
	for _, code := range codes {
		conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(code) })
		if _, err := session.SendAndWait(conn, "check", cmd, sfs.NewSFSObject()); err == nil {
			t.Errorf("%s is registered as benign for %s", code, cmd)
		}
	}
}

// TestChatTwinsShareTheRunsOneChatRead: chat-scan, red-packets and both twins on one (conn,
// init) read chat once.
func TestChatTwinsShareTheRunsOneChatRead(t *testing.T) {
	withEvNow(t, evTestNow)
	c := withRPChat(t, &chat.Result{Granted: true}, nil)
	conn, fake := startFuFireworks(t, nil, nil, nil)
	in := fuChatInit()
	for _, run := range []func(*session.GameConn, *Init) error{runAllianceTreasuresChat, runChatScan, runFireworksChat, runRedPackets} {
		if err := run(conn, in); err != nil {
			t.Fatal(err)
		}
	}
	if c.calls != 1 {
		t.Errorf("chat read %d times in one run, want 1", c.calls)
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v with no card", fake.cmds())
	}
}
