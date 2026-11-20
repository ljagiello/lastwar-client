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

// atCard is a post 600 card with the card's field names (ShareDecode.lua:723-757; ChatPushMsg.lua:
// 62-68).
func atCard(seq int64, ago time.Duration, custom string) chat.Post {
	return chat.Post{Type: 600, RoomID: "alliance_783_" + fwAlliance, Group: chat.GroupAlliance, SeqID: seq, Sender: fwAllyA,
		ServerTimeMs: evTestNow.Add(-ago).UnixMilli(), CustomJSON: custom}
}

// atInfo is a detect.event.get.treasure.claim.info reply with the fields
// DetectEventGetTreasureClaimInfo:InitData reads (DetectEventGetTreasureClaimInfo.lua:30-81).
func atInfo(point, server int64, num int32, claimers ...string) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("cfgId", 25001)
	r.PutInt("num", num)
	r.PutInt("pointId", int32(point))
	r.PutLong("createTime", evTestNow.Add(-time.Hour).UnixMilli())
	r.PutInt("targetServer", int32(server))
	r.PutInt("source", 1)
	owner := sfs.NewSFSObject()
	owner.PutUtfString("uid", fwAllyA)
	owner.PutUtfString("name", "Name of "+fwAllyA)
	r.PutSFSObject("ownerInfo", owner)
	arr := sfs.NewSFSArray()
	for _, c := range claimers {
		e := sfs.NewSFSObject()
		e.PutUtfString("uid", c)
		e.PutUtfString("name", "Name of "+c)
		arr.AddSFSObject(e)
	}
	r.PutSFSArray("array", arr)
	return r
}

// atFarDug is ally A's dug radar treasure at point, far from every member HQ (outside the shared
// scan's radius).
func atFarDug(uuid int64, point int32) atFix {
	return atWith(atDug(uuid, 3*time.Hour), func(f *atFix) { f.pointID = point })
}

// startFuTreasures answers al.rank, world.get.block (fuBlockReply over tiles), the info requests
// from infos (by uuid) and every claim with claim (atClaimOK when nil).
func startFuTreasures(t *testing.T, tiles []fuTile, infos map[int64]*sfs.SFSObject, claim func(p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
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
		case "detect.event.get.treasure.claim.info":
			if r := infos[p.GetLong("uuid")]; r != nil {
				return r
			}
			return evErr("radar_tips_99")
		case "detect.event.claim.treasure":
			if claim != nil {
				return claim(p)
			}
			return atClaimOK(p)
		}
		return evErr("unexpected command")
	})
}

func fuTreasureTile(f atFix) fuTile { return fuTile{int64(f.pointID), pbTreasurePoint(f)} }

func TestAllianceTreasuresChatClaimsTheCardsTreasure(t *testing.T) {
	withEvNow(t, evTestNow)
	posts := []chat.Post{
		atCard(1, 30*time.Minute, `{"uuid":7101,"targetServer":783,"pointId":612350,"eventId":25001}`),
		atCard(2, 29*time.Minute, `{"uuid":7102}`), // no targetServer: the source server
		atCard(3, 28*time.Minute, `{"uuid":7103,"targetServer":783}`),
		atCard(4, 27*time.Minute, `{"uuid":7104,"targetServer":783}`),
		atCard(5, 26*time.Minute, `{"uuid":7105,"targetServer":783}`),
		atCard(6, 25*time.Minute, `{"uuid":7106,"targetServer":784}`),
		atCard(7, 24*time.Minute, `{"uuid":7107,"targetServer":783}`),
		atCard(8, 23*time.Minute, `{"uuid":7101,"targetServer":783}`), // the same treasure again
		atCard(9, 22*time.Minute, `not json`),
		atCard(10, 21*time.Minute, `{"uuid":7108,"targetServer":783}`),
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: posts}, nil)
	logs := captureFwLogs(t)
	own := atWith(atFarDug(7107, 612360), func(f *atFix) { f.owner = fwSelf })
	conn, fake := startFuTreasures(t, []fuTile{fuTreasureTile(atFarDug(7101, 612350)), fuTreasureTile(own)},
		map[int64]*sfs.SFSObject{
			7101: atInfo(612350, 783, 17, atClaimerA, atClaimerB),
			7102: atInfo(612355, 783, 5, atClaimerA, fwSelf), // claimed by the player
			7103: atInfo(612356, 783, 0),                     // no claims left
			7104: atInfo(0, 783, 5),                          // gone: the chat opens the claim window
			7106: atInfo(612357, 784, 5),                     // on another server
			7107: atInfo(612360, 783, 5),
			7108: atInfo(2500001, 783, 5), // a big-map point
			// 7105: the fake answers errorCode radar_tips_99
		}, nil)
	if err := runAllianceTreasuresChat(conn, fuChatInit()); err != nil {
		t.Fatal(err)
	}
	infos := fake.only("detect.event.get.treasure.claim.info")
	var order []int64
	for _, r := range infos {
		order = append(order, r.Params.GetLong("uuid"))
		if keys := session.ParamKeysWithoutID(r.Params); !slices.Equal(keys, []string{"uuid", "source", "targetServer"}) {
			t.Errorf("info keys %v, want DetectEventGetTreasureClaimInfoMessage's uuid, source, targetServer", keys)
		}
		for key, typ := range map[string]byte{"uuid": sfs.SFSLong, "source": sfs.SFSInt, "targetServer": sfs.SFSInt} {
			if v, _ := r.Params.Get(key); v.Type != typ {
				t.Errorf("info %s type %d, want %d", key, v.Type, typ)
			}
		}
		if r.Params.GetInt("source") != 1 { // SeeDetectEventGetTreasureClaimInfoType.Chat
			t.Errorf("info source %d, want 1 (Chat)", r.Params.GetInt("source"))
		}
	}
	if want := []int64{7108, 7107, 7106, 7105, 7104, 7103, 7102, 7101}; !slices.Equal(order, want) {
		t.Fatalf("asked about %v, want %v (newest card first, each treasure once)", order, want)
	}
	for i, want := range []int32{783, 783, 784, 783, 783, 783, 783, 783} {
		if got := infos[i].Params.GetInt("targetServer"); got != want {
			t.Errorf("info %d targetServer %d, want %d (the card's, else the source server)", order[i], got, want)
		}
	}
	blocks := fake.only("world.get.block")
	if len(blocks) != 1 {
		t.Fatalf("sent %d world.get.block, want 1 for the adjacent blocks of 612350 and 612360", len(blocks))
	}
	v, _ := blocks[0].Params.Get("index")
	if idx := v.Val.([]int32); !slices.Equal(idx, []int32{6134, 6135}) { // tiles (349, 612) and (359, 612)
		t.Errorf("block index %v, want [6134 6135]", idx)
	}
	if blocks[0].Params.GetInt("clearUuidSet") != 1 || blocks[0].Params.GetInt("serverId") != 783 {
		t.Errorf("block request %s", blocks[0].Params.StringRedacted())
	}
	claims := fake.only("detect.event.claim.treasure")
	if len(claims) != 1 || claims[0].Params.GetLong("uuid") != 7101 || claims[0].Params.GetInt("targetServer") != 783 {
		t.Fatalf("claims %v, want 7101 on 783 only", claims)
	}
	if n := len(fake.only("al.rank")); n != 0 {
		t.Errorf("al.rank sent %d times; the treasure rules need no roster", n)
	}
	out := logs.String()
	for _, want := range []string{
		"first detect.event.get.treasure.claim.info reply", "numPresent=true", "already claimed by you", "no claims left (num)",
		"no point (the chat opens the claim window instead)", "info refused: radar_tips_99", "on another server",
		"outside the 1000-tile map", "own treasure", "duplicate card", "unreadable card",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q", want)
		}
	}
	for _, leak := range append(slices.Clone(fwAllUIDs), atClaimerA, atClaimerB, "secret-chat-token") {
		if strings.Contains(out, leak) {
			t.Errorf("logs carry %q", leak)
		}
	}
	fuCheckNotBenign(t, "detect.event.get.treasure.claim.info", "radar_tips_99")
}

// TestAllianceTreasuresChatOnlyAtTheLoginSourceServer: ChatPushMsg.lua:58-61 sends nothing away
// from the login source server (crossFightSrcServerId set and not the self server).
func TestAllianceTreasuresChatOnlyAtTheLoginSourceServer(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, src := range map[string]int32{"away": 7, "at home": 783, "never crossed": -1} {
		t.Run(name, func(t *testing.T) {
			c := withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{
				atCard(1, time.Minute, `{"uuid":7101,"targetServer":783}`),
			}}, nil)
			conn, fake := startFuTreasures(t, []fuTile{fuTreasureTile(atFarDug(7101, 612350))},
				map[int64]*sfs.SFSObject{7101: atInfo(612350, 783, 17)}, nil)
			in := fuChatInit()
			in.Object("user").PutInt("crossFightSrcServerId", src)
			if err := runAllianceTreasuresChat(conn, in); err != nil {
				t.Fatal(err)
			}
			sent := len(fake.only("detect.event.get.treasure.claim.info"))
			if away := src == 7; (sent == 0) != away || c.calls != 1 {
				t.Errorf("crossFightSrcServerId %d: sent %d info requests (chat reads %d)", src, sent, c.calls)
			}
		})
	}
}

// TestAllianceTreasuresChatFollowsAllianceTreasuresInOneRun: alliance-treasures first sends what it
// sends alone; the twin then skips the treasure alliance-treasures answered and needs no al.rank.
func TestAllianceTreasuresChatFollowsAllianceTreasuresInOneRun(t *testing.T) {
	withEvNow(t, evTestNow)
	near := atDug(7001, 3*time.Hour) // 347540, inside the shared scan
	tiles := []fuTile{fuTreasureTile(near), fuTreasureTile(atFarDug(7101, 612350))}
	infos := map[int64]*sfs.SFSObject{7001: atInfo(347540, 783, 10), 7101: atInfo(612350, 783, 17)}
	alone, aloneFake := startFuTreasures(t, tiles, infos, nil)
	if err := runAllianceTreasures(alone, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{
		atCard(1, 2*time.Minute, `{"uuid":7001,"targetServer":783}`),
		atCard(2, time.Minute, `{"uuid":7101,"targetServer":783}`),
	}}, nil)
	conn, fake := startFuTreasures(t, tiles, infos, nil)
	in := fuChatInit()
	if err := runAllianceTreasures(conn, in); err != nil {
		t.Fatal(err)
	}
	if got, want := fake.cmds(), aloneFake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("alliance-treasures sent %v, alone %v", got, want)
	}
	n := len(fake.requests())
	if err := runAllianceTreasuresChat(conn, in); err != nil {
		t.Fatal(err)
	}
	twin := fake.requests()[n:]
	var cmds []string
	for _, r := range twin {
		cmds = append(cmds, r.Cmd)
	}
	if want := []string{"detect.event.get.treasure.claim.info", "world.get.block", "detect.event.claim.treasure"}; !slices.Equal(cmds, want) {
		t.Fatalf("the twin sent %v, want %v", cmds, want)
	}
	if twin[0].Params.GetLong("uuid") != 7101 || twin[2].Params.GetLong("uuid") != 7101 {
		t.Error("the twin asked about or claimed 7001, which alliance-treasures answered")
	}
}

// TestAllianceTreasuresChatLeavesTheSharedScanAlone: the twin first, then alliance-treasures on
// the same (conn, init): alliance-treasures still sends al.rank and its whole scan.
func TestAllianceTreasuresChatLeavesTheSharedScanAlone(t *testing.T) {
	withEvNow(t, evTestNow)
	tiles := []fuTile{fuTreasureTile(atDug(7001, 3*time.Hour)), fuTreasureTile(atFarDug(7101, 612350))}
	infos := map[int64]*sfs.SFSObject{7101: atInfo(612350, 783, 17)}
	alone, aloneFake := startFuTreasures(t, tiles, infos, nil)
	if err := runAllianceTreasures(alone, fwTestInit()); err != nil {
		t.Fatal(err)
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{atCard(1, time.Minute, `{"uuid":7101,"targetServer":783}`)}}, nil)
	conn, fake := startFuTreasures(t, tiles, infos, nil)
	in := fuChatInit()
	if err := runAllianceTreasuresChat(conn, in); err != nil {
		t.Fatal(err)
	}
	n := len(fake.requests())
	if err := runAllianceTreasures(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests()[n:] {
		got = append(got, r.Cmd)
	}
	if want := aloneFake.cmds(); !slices.Equal(got, want) {
		t.Errorf("alliance-treasures after the twin sent %v, alone %v", got, want)
	}
}

// TestAllianceTreasuresChatHonoursTheStop: an unknown claim code stopped alliance-treasures this
// run, so the twin sends nothing.
func TestAllianceTreasuresChatHonoursTheStop(t *testing.T) {
	withEvNow(t, evTestNow)
	withRPChat(t, &chat.Result{Granted: true, Posts: []chat.Post{atCard(1, time.Minute, `{"uuid":7101,"targetServer":783}`)}}, nil)
	conn, fake := startFuTreasures(t, []fuTile{fuTreasureTile(atDug(7001, 3*time.Hour)), fuTreasureTile(atFarDug(7101, 612350))},
		map[int64]*sfs.SFSObject{7101: atInfo(612350, 783, 17)}, func(*sfs.SFSObject) *sfs.SFSObject { return evErr("801999") })
	in := fuChatInit()
	if err := runAllianceTreasures(conn, in); err == nil {
		t.Fatal("alliance-treasures should report the refusal")
	}
	n := len(fake.requests())
	if err := runAllianceTreasuresChat(conn, in); err != nil {
		t.Fatal(err)
	}
	if extra := fake.cmds()[n:]; len(extra) != 0 {
		t.Errorf("the twin sent %v after alliance-treasures stopped", extra)
	}
}

// TestAllianceTreasuresChatCapsLookups: at most atMaxClaimsPerRun (20) cards a run, less what
// alliance-treasures answered.
func TestAllianceTreasuresChatCapsLookups(t *testing.T) {
	withEvNow(t, evTestNow)
	var posts []chat.Post
	for i := range 25 {
		posts = append(posts, atCard(int64(i), time.Duration(i+1)*time.Minute, fmt.Sprintf(`{"uuid":%d,"targetServer":783}`, 8000+i)))
	}
	withRPChat(t, &chat.Result{Granted: true, Posts: posts}, nil)
	conn, fake := startFuTreasures(t, nil, nil, nil)
	if err := runAllianceTreasuresChat(conn, fuChatInit()); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only("detect.event.get.treasure.claim.info")); n != 20 {
		t.Errorf("asked about %d cards, want 20", n)
	}
}
