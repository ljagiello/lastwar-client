package game

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/chat"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

const rpRoom = "alliance_321_al1"

// No game test may reach the real chat servers (rule 1): the default reader fails loudly unless a
// test replaces it (withRPChat).
func init() {
	chatRead = func(chat.Identity, chat.Options) (*chat.Result, error) {
		return nil, errors.New("test: the real chat reader is not reachable from the game tests")
	}
}

// rpInit is an Init in alliance al1 on server 321 with own uid "me" and a chatToken; edit tweaks
// the raw init and user.
func rpInit(edit func(raw, user *sfs.SFSObject)) *Init {
	in := evInit(30)
	u := sfs.NewSFSObject()
	u.PutUtfString("uid", "me")
	u.PutUtfString("allianceId", "al1")
	u.PutInt("serverId", 321)
	in.Raw.PutUtfString("chatToken", "secret-chat-token")
	if edit != nil {
		edit(in.Raw, u)
	}
	in.Raw.PutSFSObject("user", u)
	return in
}

// rpCustom is a post 611 customJsonParam with the card's field names.
func rpCustom(uuid string, packetID int, expires time.Time, more string) string {
	s := fmt.Sprintf(`{"uuid":%q,"packetId":%d,"goodsId":653052,"serverId":321,"expiredTime":%d`, uuid, packetID, expires.UnixMilli())
	if more != "" {
		s += "," + more
	}
	return s + "}"
}

func rpPost(seq int64, sender, custom, claimers string) chat.Post {
	return chat.Post{Type: chat.PostRedPacket, RoomID: rpRoom, Group: chat.GroupAlliance, SeqID: seq, Sender: sender,
		ServerTimeMs: evTestNow.Add(-time.Hour).UnixMilli(), CustomJSON: custom, ClientUpdateExtra: "0|" + claimers + "|0"}
}

// rpChat replaces the chat reader with one returning res (or err) and counting calls.
type rpChat struct {
	calls int
	id    chat.Identity
	opt   chat.Options
}

func withRPChat(t *testing.T, res *chat.Result, err error) *rpChat {
	t.Helper()
	c := &rpChat{}
	saved, savedSleep := chatRead, rpSleep
	chatRead = func(id chat.Identity, opt chat.Options) (*chat.Result, error) {
		c.calls++
		c.id, c.opt = id, opt
		return res, err
	}
	var slept []time.Duration
	rpSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() {
		chatRead, rpSleep = saved, savedSleep
		chatReadMu.Lock()
		chatReadLast = nil
		chatReadMu.Unlock()
	})
	return c
}

func TestChatFeaturesAreOptInAndStaticOnly(t *testing.T) {
	for _, name := range []string{chatScanFeature, redPacketsFeature} {
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
}

func TestRedPacketVerdict(t *testing.T) {
	me := chatSelf{uid: "me", sourceServer: 321}
	now := evTestNow
	soon := now.Add(time.Hour)
	twenty := strings.TrimSuffix(strings.Repeat("x,", 20), ",")
	cases := []struct {
		name string
		post chat.Post
		want string
	}{
		{"openable", rpPost(1, "u2", rpCustom("a", 1, soon, ""), "u3,u4"), ""},
		{"openable with an empty group", func() chat.Post { p := rpPost(1, "u2", rpCustom("a", 1, soon, ""), ""); p.Group = ""; return p }(), ""},
		{"another room's group", func() chat.Post { p := rpPost(1, "u2", rpCustom("a", 1, soon, ""), ""); p.Group = "country"; return p }(), "not an alliance-room post"},
		{"unreadable", rpPost(1, "u2", `{"packetId":1}`, ""), "unreadable card: red packet without a uuid"},
		{"no packetId", rpPost(1, "u2", `{"uuid":"a","expiredTime":1}`, ""), "no packetId"},
		{"unknown packet", rpPost(1, "u2", rpCustom("a", 999, soon, ""), ""), "packetId not in table red_packet"},
		{"receive condition", rpPost(1, "u2", rpCustom("a", 301, soon, ""), ""), "receive condition (item count) not evaluated"},
		{"camp restricted", rpPost(1, "u2", rpCustom("a", 1, soon, `"campId":3`), ""), "camp-restricted packet (camp index unknown)"},
		{"own server barred", rpPost(1, "u2", rpCustom("a", 1, soon, `"banServerId":321`), ""), "barred for the own server"},
		{"another server barred", rpPost(1, "u2", rpCustom("a", 1, soon, `"banServerId":322`), ""), ""},
		{"no expiry", rpPost(1, "u2", `{"uuid":"a","packetId":1}`, ""), "no expiredTime"},
		{"expired", rpPost(1, "u2", rpCustom("a", 1, now, ""), ""), "expired"},
		{"own packet", rpPost(1, "me", rpCustom("a", 1, soon, ""), ""), "own packet"},
		{"opened before", rpPost(1, "u2", rpCustom("a", 1, soon, ""), "u3,me"), "already opened by you"},
		{"all taken", rpPost(1, "u2", rpCustom("a", 1, soon, ""), twenty), "all taken"},
		{"packet 2 takes five", rpPost(1, "u2", rpCustom("a", 2, soon, ""), "a,b,c,d,e"), "all taken"},
		{"hasRob fills no slot in the window", rpPost(1, "u2", rpCustom("a", 2, soon, `"hasRob":true`), "a,b,c,d"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, got := redPacketVerdict(c.post, me, now.UnixMilli()); got != c.want {
				t.Errorf("verdict %q, want %q", got, c.want)
			}
		})
	}
}

func TestRedPacketsOpensEligiblePacketsExpiringFirst(t *testing.T) {
	withEvNow(t, evTestNow)
	in := rpInit(nil)
	later, sooner := evTestNow.Add(3*time.Hour), evTestNow.Add(time.Hour)
	res := &chat.Result{RoomID: rpRoom, Granted: true, Posts: []chat.Post{
		rpPost(1, "u2", rpCustom("9007199254740993", 1, later, `"serverId":0`), "u3"),
		rpPost(2, "u4", rpCustom("b-2", 100, sooner, ""), ""),
		rpPost(3, "u4", rpCustom("b-2", 100, sooner, ""), ""), // the same packet twice
		rpPost(4, "u5", rpCustom("c", 1, evTestNow.Add(-time.Minute), ""), ""),
		rpPost(5, "me", rpCustom("d", 1, later, ""), ""),
		rpPost(6, "u6", rpCustom("e", 1, later, ""), "me"),
		rpPost(7, "u7", rpCustom("f", 311, later, ""), ""),
		{Type: chat.PostFireworkGift, RoomID: rpRoom, SeqID: 8, CustomJSON: `{"uuid":1}`},
	}}
	withRPChat(t, res, nil)
	logs := captureActivityLog(t)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		r.PutInt("red_packet_new", 1)
		return r
	})
	if err := runRedPackets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{redPacketOpenCmd, redPacketOpenCmd}) {
		t.Fatalf("sent %v", got)
	}
	reqs := fake.only(redPacketOpenCmd)
	for i, want := range []struct {
		uuid   string
		cfg    int32
		server int32
	}{{"b-2", 100, 321}, {"9007199254740993", 1, 0}} {
		p := reqs[i].Params
		uuid, _ := p.Get("uuid")
		if s, ok := uuid.Val.(string); !ok || s != want.uuid {
			t.Errorf("open %d uuid %#v, want UtfString %s", i, uuid.Val, want.uuid)
		}
		for key, v := range map[string]int32{"cfgId": want.cfg, "chatType": rpChatTypeAlliance, "serverId": want.server} {
			got, _ := p.Get(key)
			if n, ok := got.Val.(int32); !ok || n != v {
				t.Errorf("open %d %s %#v, want Int %d", i, key, got.Val, v)
			}
		}
		if keys := session.ParamKeysWithoutID(p); !slices.Equal(keys, []string{"uuid", "cfgId", "chatType", "serverId"}) {
			t.Errorf("open %d keys %v, want OpenRedPacketMessage's four", i, keys)
		}
	}
	out := logs.String()
	for _, leak := range []string{"secret-chat-token", "u2", "u3", "u4", "u5", "u6", "u7"} {
		if strings.Contains(out, "="+leak+" ") || strings.Contains(out, `"`+leak+`"`) {
			t.Errorf("logs carry %q:\n%s", leak, out)
		}
	}
	if !strings.Contains(out, "first open.red.packet reply") {
		t.Errorf("the first reply is not logged at Info:\n%s", out)
	}
	for _, reason := range []string{"duplicate card", "expired", "own packet", "already opened by you", "receive condition"} {
		if !strings.Contains(out, reason) {
			t.Errorf("no %q verdict logged", reason)
		}
	}
}

func TestRedPacketsDailyCapStopsWithoutError(t *testing.T) {
	withEvNow(t, evTestNow)
	soon := evTestNow.Add(time.Hour)
	withRPChat(t, &chat.Result{RoomID: rpRoom, Granted: true, Posts: []chat.Post{
		rpPost(1, "u2", rpCustom("a", 1, soon, ""), ""),
		rpPost(2, "u2", rpCustom("b", 1, soon.Add(time.Minute), ""), ""),
	}}, nil)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(rpDailyCapCode) })
	if err := runRedPackets(conn, rpInit(nil)); err != nil {
		t.Errorf("the daily cap is an expected stop, got %v", err)
	}
	if n := len(fake.cmds()); n != 1 {
		t.Errorf("sent %d opens after the cap", n)
	}
}

func TestRedPacketsUnknownErrorCodeStopsWithAnError(t *testing.T) {
	withEvNow(t, evTestNow)
	soon := evTestNow.Add(time.Hour)
	withRPChat(t, &chat.Result{RoomID: rpRoom, Granted: true, Posts: []chat.Post{
		rpPost(1, "u2", rpCustom("a", 1, soon, ""), ""),
		rpPost(2, "u2", rpCustom("b", 1, soon.Add(time.Minute), ""), ""),
	}}, nil)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("120035") })
	err := runRedPackets(conn, rpInit(nil))
	if err == nil || !strings.Contains(err.Error(), "errorCode=120035") {
		t.Errorf("err %v", err)
	}
	if n := len(fake.cmds()); n != 1 {
		t.Errorf("sent %d opens after a refusal", n)
	}
}

func TestRedPacketsCapPerRun(t *testing.T) {
	withEvNow(t, evTestNow)
	var posts []chat.Post
	for i := range 12 {
		posts = append(posts, rpPost(int64(i), "u2", rpCustom("p"+strconv.Itoa(i), 1, evTestNow.Add(time.Duration(i+1)*time.Minute), ""), ""))
	}
	withRPChat(t, &chat.Result{RoomID: rpRoom, Granted: true, Posts: posts}, nil)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runRedPackets(conn, rpInit(nil)); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.cmds()); n != rpMaxClaimsPerRun {
		t.Errorf("sent %d opens, want the daily cap %d", n, rpMaxClaimsPerRun)
	}
}

func TestChatCardsReadsOncePerRunWithTheInitIdentityAndSwitches(t *testing.T) {
	withEvNow(t, evTestNow)
	in := rpInit(func(raw, user *sfs.SFSObject) {
		user.PutInt("crossFightSrcServerId", 7)
		dc := sfs.NewSFSObject()
		dc.PutUtfString("CheckChatNewLine", "1")
		dc.PutInt("room_join_control", 1)
		raw.PutSFSObject("dataConfig", dc)
		raw.PutLong("immediate_toggle", 1<<chatUseNewLineBit|1)
	})
	c := withRPChat(t, &chat.Result{RoomID: "alliance_7_al1", Granted: true}, nil)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runChatScan(conn, in); err != nil {
		t.Fatal(err)
	}
	if err := runRedPackets(conn, in); err != nil {
		t.Fatal(err)
	}
	if c.calls != 1 {
		t.Errorf("chat read %d times in one run, want 1", c.calls)
	}
	want := chat.Identity{UID: "me", SelfServerID: 321, SourceServerID: 7, AllianceID: "al1", ChatToken: "secret-chat-token"}
	if c.id != want {
		t.Errorf("identity %+v", c.id)
	}
	if !c.opt.NewLineList || !c.opt.SynPermission || !c.opt.UseDataNew {
		t.Errorf("switches NewLineList %v SynPermission %v UseDataNew %v", c.opt.NewLineList, c.opt.SynPermission, c.opt.UseDataNew)
	}
	if got := c.opt.Now(); !got.Equal(evTestNow) {
		t.Errorf("chat clock %v, want the game clock", got)
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v with nothing to open", fake.cmds())
	}

	// Switches off, and a new run reads again.
	in2 := rpInit(func(raw, _ *sfs.SFSObject) { raw.PutLong("immediate_toggle", 1) })
	if err := runChatScan(conn, in2); err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 || c.opt.NewLineList || c.opt.SynPermission || c.opt.UseDataNew || c.id.SourceServerID != 321 {
		t.Errorf("second run: calls %d, opt %+v, id %+v", c.calls, c.opt, c.id)
	}
}

func TestChatFeaturesSkipWithoutAnAllianceOrAToken(t *testing.T) {
	withEvNow(t, evTestNow)
	for name, edit := range map[string]func(raw, user *sfs.SFSObject){
		"no alliance": func(_, u *sfs.SFSObject) { u.PutUtfString("allianceId", "") },
		"no token":    func(raw, _ *sfs.SFSObject) { raw.PutUtfString("chatToken", "") },
		"no uid":      func(_, u *sfs.SFSObject) { u.PutUtfString("uid", "") },
		"no server":   func(_, u *sfs.SFSObject) { u.PutInt("serverId", 0) },
	} {
		t.Run(name, func(t *testing.T) {
			c := withRPChat(t, &chat.Result{Granted: true}, nil)
			conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
			if err := runRedPackets(conn, rpInit(edit)); err != nil || c.calls != 0 || len(fake.cmds()) != 0 {
				t.Errorf("err %v, chat reads %d, sent %v", err, c.calls, fake.cmds())
			}
		})
	}
}

func TestChatScanSendsNothingAndLogsEveryCard(t *testing.T) {
	withEvNow(t, evTestNow)
	posts := []chat.Post{
		rpPost(1, "u2", rpCustom("a", 1, evTestNow.Add(time.Hour), ""), ""),
		{Type: chat.PostFireworkGift, RoomID: rpRoom, SeqID: 2, Sender: "u3", ServerTimeMs: evTestNow.Add(-3 * time.Hour).UnixMilli(),
			CustomJSON: `{"uuid":11,"ownerUid":"u3","configId":670001}`, ClientUpdateExtra: "me|u9"},
		{Type: chat.PostS0BossFireworks, RoomID: rpRoom, SeqID: 3, Sender: "u4", ServerTimeMs: evTestNow.UnixMilli(),
			CustomJSON: `{"uuid":12,"ownerUid":"me","configId":671001,"expiredTime":1}`},
		{Type: chat.PostTreasureDug, RoomID: rpRoom, SeqID: 4, Sender: "u5", ServerTimeMs: evTestNow.UnixMilli(),
			CustomJSON: `{"uuid":13,"targetServer":321,"pointId":500500,"eventId":190001}`},
		{Type: chat.PostTreasureDug, RoomID: rpRoom, SeqID: 5, CustomJSON: `nope`},
	}
	withRPChat(t, &chat.Result{RoomID: rpRoom, Granted: true, Posts: posts}, nil)
	logs := captureActivityLog(t)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	if err := runChatScan(conn, rpInit(nil)); err != nil {
		t.Fatal(err)
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("chat-scan sent %v on the game socket", fake.cmds())
	}
	out := logs.String()
	for _, want := range []string{
		"chat-scan: red packet", "eligible=true",
		`msg="chat-scan: firework card" post=693 seqId=2 uuid=11`, "claimedByYou=true", "expired=true",
		`msg="chat-scan: firework card" post=695 seqId=3 uuid=12`, "owner=self",
		`msg="chat-scan: treasure-dug card" seqId=4 uuid=13 targetServer=321`, "pointId=500500",
		`msg="chat-scan: treasure card unreadable" seqId=5`,
		"redPackets=1 openable=1 fireworkCards=1 s0BossFireworkCards=1 treasureCards=2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "u3") || strings.Contains(out, "u5") {
		t.Errorf("logs carry another player's uid:\n%s", out)
	}
}

func TestChatReadFailureDoesNotStopTheRun(t *testing.T) {
	withEvNow(t, evTestNow)
	withRPChat(t, nil, errors.New("chat: discovery: all three URLs failed"))
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	err := runRedPackets(conn, rpInit(nil))
	if err == nil || session.ContainsNonTimeoutNetError(err) {
		t.Errorf("err %v (must be reported, and must not read as a dead game connection)", err)
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v", fake.cmds())
	}
}
