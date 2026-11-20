package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
)

// The expected signs were computed outside Go (Python hashlib) from the client's formulas.
func TestSignsMatchTheClientFormulas(t *testing.T) {
	conn := connectionSign(testUID, testNowMs)
	if conn != "415a811ec14c5711533d090baa17513e" {
		t.Errorf("connection sign %s", conn)
	}
	params := `{"rooms":[{"group":"alliance","id":"alliance_321_abc"}]}`
	if got := frameSign(testUID, []byte(params), 1763500001000, conn); got != "554155bcaaeaa51b6a706c7f40fc2e6b" {
		t.Errorf("frame sign %s", got)
	}
	want := "t=1763500000&s=240d88f17b91f4e8a97264fa84a9824c&a=100017&u=812345678901&serverId=321"
	if got := discoveryForm(testIdentity(), 1763500000); got != want {
		t.Errorf("discovery form\n got %s\nwant %s", got, want)
	}
}

func TestEncodeJSONSortsKeysWithoutHTMLEscaping(t *testing.T) {
	b, err := encodeJSON(map[string]any{"rooms": []map[string]any{{"id": "a<b>&", "group": "alliance"}}})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"rooms":[{"group":"alliance","id":"a<b>&"}]}` {
		t.Errorf("got %s", got)
	}
}

// fullScript plays a normal session: login.success, the join reply, a history page with every
// kept post type (and posts to drop), then pushes.
func fullScript(s *fakeServer) {
	s.sendJSON(`{"cmd":"login.success","server":"chat1","clientid":"c9","serverTime":1763500000000,"data":{"uid":"812345678901"}}`)
	if cmdOf(s.recv()) != cmdJoinMulti {
		return
	}
	s.writeFrame(opText, true, []byte(heartbeatFrame))
	s.sendJSON(`{"cmd":"room.joinMulti","result":{"status":1}}`)
	if cmdOf(s.recv()) != cmdHistoryRooms {
		return
	}
	rp := `{\"uuid\":9007199254740993,\"packetId\":1,\"goodsId\":653052,\"serverId\":321,\"expiredTime\":1763520000000,\"hasRob\":0}`
	fw := `{\"uuid\":\"9007199254740995\",\"ownerUid\":\"u7\",\"configId\":671001,\"index\":1}`
	tr := `{\"uuid\":123456789012,\"targetServer\":321,\"pointId\":500500,\"eventId\":190001,\"owner\":{\"name\":\"x\"}}`
	hist := `{"cmd":"history.roomsv2","result":{"rooms":[` +
		`{"roomId":"alliance_321_abc","group":"alliance","msgs":[` +
		`{"roomId":"alliance_321_abc","group":"alliance","sender":"u2","seqId":10,"serverTime":1763499000,"msg":"hello","extra":{"post":0}},` +
		`{"roomId":"alliance_321_abc","group":"alliance","sender":"u3","seqId":11,"serverTime":1763499100000,"extra":{"post":611,"customJsonParam":"` + rp + `"},"clientUpdateExtra":"0|u4,u5|2"},` +
		`{"roomId":"alliance_321_abc","group":"alliance","sender":"u7","serverTime":1763499200000,"extra":{"seqId":12,"post":"693","customJsonParam":"` + fw + `"},"clientUpdateExtra":"u8|u9"},` +
		`{"roomId":"alliance_321_abc","group":"alliance","sender":"u10","seqId":13,"serverTime":1763499300000,"extra":{"post":600,"customJsonParam":"` + tr + `"}}]},` +
		`{"roomId":"country_321","msgs":[{"roomId":"country_321","seqId":99,"extra":{"post":611,"customJsonParam":"{}"}}]}]}}`
	s.sendJSON(hist)
	s.sendJSON(`{"cmd":"push.chat.room","data":{"roomId":"country_321","seqId":100,"extra":{"post":611,"customJsonParam":"{}"}}}`)
	s.sendJSON(`{"cmd":"push.chat.room","data":{"roomId":"alliance_321_abc","group":"alliance","sender":"u11","seqId":14,"serverTime":1763500000000,"extra":{"post":695,"customJsonParam":"{\"uuid\":5,\"ownerUid\":\"u11\",\"configId\":671003,\"expiredTime\":1763507200000,\"pointId\":42,\"serverId\":321}"}}}`)
	s.sendJSON(`{"cmd":"push.chat.msg.update.room","data":{"roomId":"alliance_321_abc","seqId":11,"clientUpdateExtra":"0|u4,u5,u6|3"}}`)
	s.writeFrame(opPing, true, []byte("p1"))
}

func TestReadJoinsTheAllianceRoomReadsHistoryAndKeepsOnlyTargetPosts(t *testing.T) {
	var formBodies []string
	var hosts []string
	rt := func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		formBodies = append(formBodies, string(b))
		hosts = append(hosts, r.URL.Host)
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("discovery Content-Type %q", ct)
		}
		if r.Method != http.MethodPost {
			t.Errorf("discovery method %s", r.Method)
		}
		return jsonResponse(200, testLinksOK), nil
	}
	logs := captureLog(t)
	dial, servers := fakeDial(t, fullScript)
	var dialled string
	opt := testOptions(rt, func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialled = addr
		return dial(ctx, network, addr)
	})
	res, err := Read(testIdentity(), opt)
	if err != nil {
		t.Fatal(err)
	}
	s := <-servers
	s.wait(t)

	if len(hosts) != 1 || hosts[0] != "lastwar-chat-cf.lastwarapp.net" {
		t.Errorf("discovery hosts %v, want the first links URL only", hosts)
	}
	if want := "t=1763500000&s=240d88f17b91f4e8a97264fa84a9824c&a=100017&u=812345678901&serverId=321"; formBodies[0] != want {
		t.Errorf("discovery body %q", formBodies[0])
	}
	if dialled != "10.0.0.1:8080" {
		t.Errorf("dialled %q", dialled)
	}
	if s.reqLine != "GET / HTTP/1.1" {
		t.Errorf("request line %q", s.reqLine)
	}
	for name, want := range map[string]string{
		"APPID": "100017", "TIME": strconv.Itoa(testNowMs), "UID": testUID,
		"SIGN": "415a811ec14c5711533d090baa17513e", "CHATTOKEN": testChatTokn,
		"Upgrade": "websocket", "Sec-WebSocket-Version": "13",
	} {
		if got := s.headers[name]; got != want {
			t.Errorf("handshake header %s = %q, want %q (spelling kept)", name, got, want)
		}
	}
	if got := s.cmds(); !slices.Equal(got, []string{cmdJoinMulti, cmdHistoryRooms}) {
		t.Fatalf("client sent %v, want only the join and the history pull", got)
	}
	wantFrames := []string{
		`{"cmd":"room.joinMulti","params":{"rooms":[{"group":"alliance","id":"alliance_321_abc"}]},"sendTime":1763500000123,"sign":"bc3ce7e4876f67b81feed340d7d47b39"}`,
		`{"cmd":"history.roomsv2","params":{"rooms":{"alliance_321_abc":"0"}},"sendTime":1763500000123,"sign":"f3ceaa63f52f15c81063aa93586d8744"}`,
	}
	for i, w := range wantFrames {
		if got := string(s.raw[i]); got != w {
			t.Errorf("frame %d\n got %s\nwant %s", i, got, w)
		}
	}
	s.mu.Lock()
	pongs, closed := s.pongs, s.closed
	s.mu.Unlock()
	if pongs != 1 || !closed {
		t.Errorf("pongs %d (want 1, the ping answered), close frame sent %v", pongs, closed)
	}

	out := logs.String()
	for _, want := range []string{
		`msg="chat: history page shape" msgs=4 msgKeys="[extra group msg roomId sender seqId serverTime]" extraKeys=[post] postCounts=0:1,600:1,611:1,693:1 withoutPost=0`,
		`msg="chat: first frame of its kind" cmd=history.roomsv2 keys="[cmd result]" resultKeys=[rooms]`,
		`msg="chat: discovery reply" code=1 lines=1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("logs lack %s:\n%s", want, out)
		}
	}
	for _, leak := range []string{"hello", testChatTokn, "u3", "415a811ec14c5711533d090baa17513e"} {
		if strings.Contains(out, leak) {
			t.Errorf("logs carry %q", leak)
		}
	}
	if res.RoomID != testRoom || !res.Granted || res.HistoryMsgs != 4 || res.Pushes != 2 || res.OldestMs != 1763499000000 {
		t.Errorf("result %+v", *res)
	}
	var types []int
	for _, p := range res.Posts {
		types = append(types, p.Type)
	}
	if !slices.Equal(types, []int{611, 693, 600, 695}) {
		t.Fatalf("kept posts %v", types)
	}
	rp, err := res.Posts[0].RedPacket()
	if err != nil {
		t.Fatal(err)
	}
	if rp.UUID != "9007199254740993" || rp.PacketID != 1 || !rp.HasRob || len(rp.Claimers) != 3 || rp.Taken() != 3 {
		t.Errorf("red packet %+v (the update push's claimer list must win, the uuid keep every digit)", rp)
	}
	if res.Posts[0].Sender != "u3" || res.Posts[0].Group != GroupAlliance || res.Posts[0].SeqID != 11 {
		t.Errorf("post %+v", res.Posts[0])
	}
	fw, err := res.Posts[1].Firework()
	if err != nil || fw.UUID != 9007199254740995 || fw.ConfigID != 671001 || fw.Index != 1 || len(fw.Claimers) != 2 || res.Posts[1].SeqID != 12 {
		t.Errorf("firework %+v %v", fw, err)
	}
	tr, err := res.Posts[2].TreasureDug()
	if err != nil || tr.UUID != 123456789012 || tr.TargetServer != 321 || !tr.HasTarget || tr.PointID != 500500 || tr.EventID != 190001 {
		t.Errorf("treasure %+v %v", tr, err)
	}
	s0, err := res.Posts[3].Firework()
	if err != nil || s0.UUID != 5 || s0.ExpiredTime != 1763507200000 || s0.PointID != 42 {
		t.Errorf("s0 boss firework %+v %v", s0, err)
	}
}

func TestSendRefusesEveryCommandThatWrites(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close(); _ = server.Close() }()
	// Drain the server end, so a send that got past the allowlist would succeed (net.Pipe writes
	// block until read) and its bytes would be counted.
	got := make(chan int, 1)
	go func() {
		n, _ := io.Copy(io.Discard, server)
		got <- int(n)
	}()
	r := &reader{ws: &wsConn{conn: client}, id: testIdentity(), opt: testOptions(linksOK, nil)}
	for _, cmd := range []string{"chat.room", "chat.up", "chat.recv", "chat.read", "chat.recvsync", "user.setInfo",
		"user.setSwitchFlag", "chat.share.send", "al.msg", "room.leave", "chat.delete", "redPackets.rvd.id"} {
		if err := r.send(cmd, map[string]any{"x": 1}); err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Errorf("send(%s) = %v; want the read-only refusal (only %v may be sent)", cmd, err, readOnlyCmds)
		}
	}
	_ = client.Close()
	if n := <-got; n != 0 {
		t.Errorf("refused sends wrote %d bytes", n)
	}
	if len(readOnlyCmds) != 2 || !readOnlyCmds[cmdJoinMulti] || !readOnlyCmds[cmdHistoryRooms] {
		t.Errorf("readOnlyCmds %v", readOnlyCmds)
	}
}

func TestReadErrorsNeverLookLikeADeadGameConnection(t *testing.T) {
	refused := func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	}
	res, err := Read(testIdentity(), testOptions(linksOK, refused))
	if res != nil || err == nil {
		t.Fatalf("got %v, %v", res, err)
	}
	if session.ContainsNonTimeoutNetError(err) {
		t.Errorf("chat dial failure %q reads as a dead game connection; RunFeatures would stop the run", err)
	}
	httpFail := func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("no route")}
	}
	if _, err := Read(testIdentity(), testOptions(httpFail, refused)); err == nil || session.ContainsNonTimeoutNetError(err) {
		t.Errorf("discovery failure %v", err)
	}
	if strings.Contains(err.Error(), testChatTokn) {
		t.Errorf("error carries the chat token: %v", err)
	}
}

func TestDiscoveryTriesTheNextURLAndPicksTheLine(t *testing.T) {
	cases := []struct {
		name       string
		replies    []string
		useNew     bool
		newLine    bool
		wantHosts  []string
		wantLine   string
		wantErr    bool
		wantSleeps int
	}{
		{"code 0 then ok", []string{`{"code":0}`, testLinksOK}, false, false,
			[]string{"lastwar-chat-cf.lastwarapp.net", "lastwar-chat-us-aws-ali.lastwargame.com"}, "ws://10.0.0.1:8080", false, 1},
		{"undecodable, bad lines, then ok", []string{`<html>`, `{"code":1,"data":[{"protocol":"http","ip":"a","port":1},{"protocol":"wss","ip":"b","port":0}]}`, testLinksOK}, false, false,
			[]string{"lastwar-chat-cf.lastwarapp.net", "lastwar-chat-us-aws-ali.lastwargame.com", "lastwar-chat-us-gcp-ali.lastwargame.com"}, "ws://10.0.0.1:8080", false, 2},
		{"data_new when switched on", []string{`{"code":"1","data":[{"protocol":"ws","ip":"old","port":"80"}],"data_new":[{"protocol":"WSS","ip":"new.example","port":443}]}`}, true, false,
			[]string{"lastwar-chat-cf.lastwarapp.net"}, "wss://new.example:443", false, 0},
		{"data when data_new is empty", []string{`{"code":1,"data":[{"protocol":"ws","ip":"old","port":"80"}],"data_new":[]}`}, true, true,
			[]string{"lastwar-serverlist-cf.lastwarapp.net"}, "ws://old:80", false, 0},
		{"all fail", []string{`{"code":2}`, `{"code":2}`, `{"code":2}`}, false, false,
			[]string{"lastwar-chat-cf.lastwarapp.net", "lastwar-chat-us-aws-ali.lastwargame.com", "lastwar-chat-us-gcp-ali.lastwargame.com"}, "", true, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var hosts []string
			rt := func(r *http.Request) (*http.Response, error) {
				hosts = append(hosts, r.URL.Host)
				return jsonResponse(200, c.replies[len(hosts)-1]), nil
			}
			opt := testOptions(rt, nil)
			opt.UseDataNew, opt.NewLineList = c.useNew, c.newLine
			sleeps := 0
			opt.Sleep = func(d time.Duration) {
				if d != 3*time.Second {
					t.Errorf("sleep %v, want the client's 3 s retry", d)
				}
				sleeps++
			}
			lines, err := discover(context.Background(), testIdentity(), opt)
			if (err != nil) != c.wantErr {
				t.Fatalf("err %v", err)
			}
			if !slices.Equal(hosts, c.wantHosts) || sleeps != c.wantSleeps {
				t.Errorf("hosts %v sleeps %d", hosts, sleeps)
			}
			if c.wantLine != "" && (len(lines) != 1 || lines[0].url() != c.wantLine) {
				t.Errorf("lines %+v, want %s", lines, c.wantLine)
			}
		})
	}
}

func TestAnotherLoginEndsTheRead(t *testing.T) {
	dial, _ := fakeDial(t, func(s *fakeServer) {
		s.sendJSON(`{"cmd":"another.login"}`)
	})
	res, err := Read(testIdentity(), testOptions(linksOK, dial))
	if res != nil || err == nil || !strings.Contains(err.Error(), "another.login") {
		t.Errorf("got %v, %v", res, err)
	}
}

func TestSynPermissionModeWaitsForTheGrantAndNeverJoins(t *testing.T) {
	dial, servers := fakeDial(t, func(s *fakeServer) {
		s.sendJSON(`{"cmd":"login.success","serverTime":1763500000000,"data":{"uid":"812345678901"}}`)
		s.sendJSON(`{"cmd":"push.room.synPermission","data":{"roomId":"alliance_manager_321_abc","auth":true,"group":"alliance_manager"}}`)
		s.sendJSON(`{"cmd":"push.room.synPermission","data":{"roomId":"alliance_321_abc","auth":true,"group":"alliance"}}`)
		if cmdOf(s.recv()) != cmdHistoryRooms {
			return
		}
		s.sendJSON(`{"cmd":"history.roomsv2","result":{"rooms":[{"roomId":"alliance_321_abc","msgs":[]}]}}`)
	})
	opt := testOptions(linksOK, dial)
	opt.SynPermission = true
	res, err := Read(testIdentity(), opt)
	if err != nil {
		t.Fatal(err)
	}
	s := <-servers
	if got := s.cmds(); !slices.Equal(got, []string{cmdHistoryRooms}) {
		t.Errorf("sent %v; in room_join_control mode the client never joins the alliance room itself", got)
	}
	if !res.Granted || res.HistoryMsgs != 0 {
		t.Errorf("result %+v", *res)
	}
}

func TestSynPermissionModeReportsAnotherLogin(t *testing.T) {
	dial, _ := fakeDial(t, func(s *fakeServer) {
		s.sendJSON(`{"cmd":"login.success","data":{"uid":"812345678901"}}`)
		s.sendJSON(`{"cmd":"another.login"}`)
	})
	opt := testOptions(linksOK, dial)
	opt.SynPermission = true
	if res, err := Read(testIdentity(), opt); err == nil || res != nil {
		t.Errorf("got %+v, %v; another.login while waiting for the grant is an error, not \"no grant\"", res, err)
	}
}

func TestSynPermissionModeWithoutAGrantReadsNothing(t *testing.T) {
	dial, servers := fakeDial(t, func(s *fakeServer) {
		s.sendJSON(`{"cmd":"login.success","data":{"uid":"812345678901"}}`)
		s.sendJSON(`{"cmd":"push.room.synPermission","data":{"roomId":"alliance_321_abc","auth":false,"group":"alliance"}}`)
	})
	opt := testOptions(linksOK, dial)
	opt.SynPermission = true
	res, err := Read(testIdentity(), opt)
	if err != nil || res == nil || res.Granted || len(res.Posts) != 0 {
		t.Fatalf("got %+v, %v", res, err)
	}
	if got := (<-servers).cmds(); len(got) != 0 {
		t.Errorf("sent %v", got)
	}
}

func TestReadNeedsTheIdentity(t *testing.T) {
	for _, id := range []Identity{
		{SelfServerID: 1, SourceServerID: 1, AllianceID: "a", ChatToken: "t"},
		{UID: "u", SelfServerID: 1, SourceServerID: 1, AllianceID: "a"},
		{UID: "u", SelfServerID: 1, SourceServerID: 1, ChatToken: "t"},
		{UID: "u", SourceServerID: 1, AllianceID: "a", ChatToken: "t"},
	} {
		called := false
		rt := func(*http.Request) (*http.Response, error) { called = true; return jsonResponse(200, testLinksOK), nil }
		if _, err := Read(id, testOptions(rt, nil)); err == nil || called {
			t.Errorf("Read(%+v) err %v, discovery called %v", id, err, called)
		}
	}
}

func TestPostDecoders(t *testing.T) {
	rp := Post{Type: PostRedPacket, CustomJSON: `{"uuid":"abc-1","packetId":"301","expiredTime":"1763520000000","banServerId":5,"campId":2}`, ClientUpdateExtra: "x||0"}
	got, err := rp.RedPacket()
	if err != nil || got.UUID != "abc-1" || got.PacketID != 301 || !got.HasExpiry || got.BanServerID != 5 || got.CampID != 2 ||
		got.HasRob || len(got.Claimers) != 0 || got.Taken() != 0 {
		t.Errorf("%+v %v", got, err)
	}
	for _, rob := range []struct {
		json string
		want bool
	}{{`true`, true}, {`0`, true}, {`1`, true}, {`false`, false}, {`null`, false}} {
		p := Post{Type: PostRedPacket, CustomJSON: `{"uuid":"u","hasRob":` + rob.json + `}`}
		if r, _ := p.RedPacket(); r.HasRob != rob.want || (rob.want && r.Taken() != 1) {
			t.Errorf("hasRob %s: %v", rob.json, r.HasRob)
		}
	}
	bad := []Post{
		{Type: PostRedPacket},
		{Type: PostRedPacket, CustomJSON: `[1]`},
		{Type: PostRedPacket, CustomJSON: `{"packetId":1}`},
		{Type: PostFireworkGift, CustomJSON: `{"ownerUid":"x"}`},
		{Type: PostTreasureDug, CustomJSON: `{"pointId":1}`},
	}
	for _, p := range bad {
		var err error
		switch p.Type {
		case PostRedPacket:
			_, err = p.RedPacket()
		case PostFireworkGift:
			_, err = p.Firework()
		case PostTreasureDug:
			_, err = p.TreasureDug()
		}
		if err == nil {
			t.Errorf("%+v decoded", p)
		}
	}
	if _, err := (Post{Type: PostTreasureDug, CustomJSON: `{"uuid":1}`}).RedPacket(); err == nil {
		t.Error("a treasure card decoded as a red packet")
	}
	td, _ := Post{Type: PostTreasureDug, CustomJSON: `{"uuid":1}`}.TreasureDug()
	if td.HasTarget {
		t.Error("absent targetServer reported present")
	}
}

func TestAllianceRoomID(t *testing.T) {
	if got := AllianceRoomID(1203, "a1b2"); got != "alliance_1203_a1b2" {
		t.Errorf("got %s", got)
	}
}

func TestDecodeFrameKeepsUnknownFrames(t *testing.T) {
	f, err := decodeFrame([]byte(`{"cmd":"x","result":{"a":1}}`))
	if err != nil || f.cmd != "x" {
		t.Fatalf("%v %v", f, err)
	}
	var r map[string]json.RawMessage
	if json.Unmarshal(f.raw["result"], &r) != nil || len(r) != 1 {
		t.Errorf("result %s", f.raw["result"])
	}
}

// captureLog routes slog to a buffer for one test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}
