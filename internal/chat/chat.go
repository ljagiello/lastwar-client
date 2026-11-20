// Package chat is a short-lived, read-only reader for the game's chat WebSocket. It connects
// once, joins the alliance room, reads the room's newest history page and the pushes that arrive
// for a few seconds, keeps only the posts the game package claims from (red packets 611, firework
// chests 693/695, treasure-dug cards 600), and closes. It never sends a chat message, a share, a
// like or a read receipt: the only commands it can send are room.joinMulti and history.roomsv2
// (readOnlyCmds), and send refuses every other one. Static-only, not validated live.
//
// The real client (1.0.370) runs chat as a separate JSON-over-WebSocket channel
// (Chat/Controller/ChatNetManager.lua, Chat/ChatManager2.lua; C# ChatService and WebSocketItem,
// Assembly-CSharp.decompiled.cs:70325-70750):
//
//  1. Discovery: an HTTPS form POST {t, s, a, u, serverId} to the first of three URLs that
//     answers {code: 1, data: [{protocol, ip, port}], data_new} (ChatNetManager.lua:470-487,
//     588-615, 85-123; ChatService.RequestServerList, A-CS:70626-70660). See discover.
//  2. Connect: "{protocol}://{ip}:{port}" with the headers APPID, TIME (server ms), UID, SIGN =
//     MD5(MD5(appId+uid)+TIME) and CHATTOKEN, the init push's chatToken (WebSocketItem.CalcSign and
//     Connect, A-CS:70348-70403; InitMessage.lua:261).
//  3. The server pushes login.success (ChatManager2.lua:96-117). The client then sends
//     user.setInfo, user.setSwitchFlag, room.getCustomRoomListV2 and room.joinMulti
//     (StartChatSession, ChatManager2.lua:128-141), and once the room list and the join have
//     answered, history.roomsv2 for every joined room (OnInitRoomCmdBack, ChatRoomManager.lua:
//     1758-1774). This reader sends only the join, for the alliance room alone, and the history
//     pull. user.setInfo writes the chat profile (name, alliance tag) other players see and
//     user.setSwitchFlag a filter setting, so both are left out; whether the server wants them
//     before it serves history is unverified. room.getCustomRoomListV2 lists private group rooms,
//     which hold none of the posts read here, and chat.at only fetches @-mentions.
//  4. Every outbound frame is {cmd, params, sendTime, sign} with sign = MD5(uid +
//     json(params, keys sorted) + sendTime + the connection SIGN) (ChatNetManager.lua:623-698).
//
// The alliance room is "alliance_<sourceServerId>_<allianceId>" on the live servers (the "test_"
// prefix is for non-Online URL groups; GenAllianceRoomId, ChatRoomManager.lua:699-703;
// ChatInterface.lua:597-602, 318-324). With DataConfig room_join_control on, the client never
// joins it: the server grants it with push.room.synPermission and the client pulls its history
// (OnRoomSynPermission, ChatRoomManager.lua:514-556). The country, language, season and
// alliance-friend rooms are not read: their ids depend on season data (GetSeasonConfigId,
// ChatRoomManager.lua:671-697) or on the SFS common.chat.room.id behind the C# switch
// CHAT_SERVER_ROOMLIST (ChatRoomManager.lua:80-87), so red packets posted there are missed.
//
// The chat server allows one session per uid: another.login closes the other one, which then does
// not reconnect (PushAnotherLoginMessage.lua:8-11). A read therefore drops the phone's chat
// connection the same way the game login drops the phone's game session.
package chat

import (
	"context"
	"crypto/md5"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// AppID is CHAT_APP_ID as ChatNetManager:Init passes it on every URL group (ChatNetManager.lua:6,
// 471); ChatService's own default "100013" is always overwritten (A-CS:70521, 70559-70565).
const AppID = "100017"

// Group names (Chat/Constant/ChatGroupType.lua:2-3).
const (
	GroupAlliance = "alliance"
)

// The posts the reader keeps (Chat/Constant/PostType.lua:89, 101, 169, 171).
const (
	PostTreasureDug     = 600 // Detect_Treasure_Fin_Info
	PostRedPacket       = 611 // RedPackge_New
	PostFireworkGift    = 693 // FIREWORK_GIFT_REWARD
	PostS0BossFireworks = 695 // S0_ALLIANCE_BOSS_GIFT_REWARD
)

// keptPosts are the posts Read returns; every other message is dropped unread.
var keptPosts = map[int]bool{PostTreasureDug: true, PostRedPacket: true, PostFireworkGift: true, PostS0BossFireworks: true}

// Chat commands and pushes (Chat/WebMessage/Config/ChatMsgDefines.lua).
const (
	cmdJoinMulti     = "room.joinMulti"
	cmdHistoryRooms  = "history.roomsv2"
	pushLoginSuccess = "login.success"
	pushAnotherLogin = "another.login"
	pushChatRoom     = "push.chat.room"
	pushMsgUpdate    = "push.chat.msg.update.room"
	pushSynPerm      = "push.room.synPermission"
	pushNotMember    = "push.no.room.member"
	heartbeatFrame   = "heartbeat" // a literal text frame (WebSocketItem.Socket_OnMessage, A-CS:70455-70459)
)

// readOnlyCmds are the only commands send writes. Everything that posts, likes, shares, marks
// read or changes a setting (chat.room, chat.up, chat.recv, chat.read, user.setInfo, ...) is
// refused.
var readOnlyCmds = map[string]bool{cmdJoinMulti: true, cmdHistoryRooms: true}

// Identity is who connects, all from the game's init push.
type Identity struct {
	UID            string // user.uid
	SelfServerID   int64  // PlayerInfo:GetSelfServerId: the discovery form's serverId
	SourceServerID int64  // PlayerInfo:GetSourceServerId: the alliance room id's server
	AllianceID     string // user.allianceId
	ChatToken      string // init chatToken
}

// Options are the read's switches and its injectable transport.
type Options struct {
	// NewLineList is DataConfig CheckChatNewLine: discover through chat_server_links.php
	// (ChatNetManager.lua:472-479).
	NewLineList bool
	// UseDataNew is server switch ChatUseNewLine (init immediate_toggle bit 12): connect to
	// data_new when the reply has it (ChatNetManager.lua:103-107, 349-357).
	UseDataNew bool
	// SynPermission is DataConfig room_join_control (ChatInterface.lua:1401-1403).
	SynPermission bool

	Now  func() time.Time // the game server clock
	HTTP *http.Client
	Dial DialFunc
	TLS  *tls.Config
	// Sleep is the pause between discovery attempts (the client retries after 3 s,
	// ChatNetManager.lua:194-203).
	Sleep func(time.Duration)

	Timeout   time.Duration // the whole read
	StepWait  time.Duration // each awaited reply or push
	Listen    time.Duration // how long pushes are read after the history page
	PingEvery time.Duration
}

// DefaultOptions are the timings a run uses; the caller fills the switches and the clock.
func DefaultOptions() Options {
	return Options{
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		Dial:      (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		Sleep:     time.Sleep,
		Timeout:   45 * time.Second,
		StepWait:  10 * time.Second,
		Listen:    3 * time.Second,
		PingEvery: 3 * time.Second,
	}
}

// Post is one kept chat message. Sender and the custom JSON belong to other players: log neither.
type Post struct {
	Type              int
	RoomID            string
	Group             string
	SeqID             int64
	Sender            string
	ServerTimeMs      int64
	CustomJSON        string // extra.customJsonParam
	ClientUpdateExtra string // the claimer list the server keeps on the message
}

// Result is what one read saw.
type Result struct {
	RoomID      string
	Posts       []Post // kept posts by seqId; an update push replaces the history copy
	HistoryMsgs int    // messages in the history page
	OldestMs    int64  // the oldest history message's serverTime; 0 when the page was empty
	Pushes      int    // push.chat.room and update pushes for the room
	Granted     bool   // the alliance room was joined (or granted in room_join_control mode)
}

// AllianceRoomID is GenAllianceRoomId on the live servers.
func AllianceRoomID(sourceServer int64, allianceID string) string {
	return "alliance_" + strconv.FormatInt(sourceServer, 10) + "_" + allianceID
}

// md5Hex is BaseUtils.StringUtils.GetMD5 and AESHelper.GetMd5Hash: lowercase hex
// (BaseUtils.decompiled.cs:34864-34876; A-CS:33868-33877).
func md5Hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

// connectionSign is WebSocketItem.CalcSign: MD5(MD5(appId + uid) + serverTimeMs).
func connectionSign(uid string, timeMs int64) string {
	return md5Hex(md5Hex(AppID+uid) + strconv.FormatInt(timeMs, 10))
}

// frameSign is ChatNetManager:WriteSign: MD5(uid + params JSON + sendTime + connection sign).
func frameSign(uid string, params []byte, sendTime int64, connSign string) string {
	return md5Hex(uid + string(params) + strconv.FormatInt(sendTime, 10) + connSign)
}

// encodeJSON marshals v without HTML escaping; map keys come out sorted, as serialize_sorted
// intends (ChatNetManager.lua:645-669). Whether the server re-sorts before checking the sign is
// unverified.
func encodeJSON(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

// flat returns err's text as a new error with no wrapped cause: a chat failure must never look
// like a dead game connection to session.ContainsNonTimeoutNetError, which would stop the run's
// remaining features.
func flat(format string, args ...any) error {
	for i, a := range args {
		if e, ok := a.(error); ok {
			args[i] = e.Error()
		}
	}
	return fmt.Errorf(format, args...) // no %w: args hold no error any more
}

// reader is one connection's state.
type reader struct {
	ws       *wsConn
	id       Identity
	opt      Options
	connSign string
	deadline time.Time
	logged   map[string]bool
	room     string
	res      *Result
	byKey    map[int64]int // seqId -> index in res.Posts
}

// frame is one decoded server frame.
type frame struct {
	cmd string
	raw map[string]json.RawMessage
}

// Read runs one read as id. A nil Result comes with an error; a Result can also come with one
// (the room was read, then the connection failed). Errors carry no wrapped cause (see flat).
func Read(id Identity, opt Options) (*Result, error) {
	if id.UID == "" || id.ChatToken == "" || id.AllianceID == "" || id.SourceServerID <= 0 || id.SelfServerID <= 0 {
		return nil, errors.New("chat: uid, chatToken, allianceId or a server id is missing")
	}
	if opt.Now == nil || opt.HTTP == nil || opt.Dial == nil || opt.Sleep == nil {
		return nil, errors.New("chat: Options need Now, HTTP, Dial and Sleep")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opt.Timeout)
	defer cancel()
	lines, err := discover(ctx, id, opt)
	if err != nil {
		return nil, flat("chat: discovery: %v", err)
	}
	var ws *wsConn
	var connSign string
	for i, l := range lines {
		timeMs := opt.Now().UnixMilli()
		connSign = connectionSign(id.UID, timeMs)
		hdrs := []header{
			{"APPID", AppID},
			{"TIME", strconv.FormatInt(timeMs, 10)},
			{"UID", id.UID},
			{"SIGN", connSign},
			{"CHATTOKEN", id.ChatToken},
		}
		dctx, dcancel := context.WithTimeout(ctx, opt.StepWait)
		ws, err = dialWS(dctx, opt.Dial, opt.TLS, l.url(), hdrs)
		dcancel()
		if err == nil {
			slog.Info("chat: connected", "line", i+1, "of", len(lines), "protocol", l.Protocol, "port", l.Port)
			break
		}
		slog.Warn("chat: connect failed", "line", i+1, "of", len(lines), "protocol", l.Protocol, "port", l.Port, "error", flat("%v", err))
	}
	if ws == nil {
		return nil, flat("chat: no chat line accepted the connection: %v", err)
	}
	stopPings := ws.startPings(opt.PingEvery)
	defer func() {
		stopPings()
		_ = ws.close()
	}()
	dl, _ := ctx.Deadline()
	r := &reader{
		ws: ws, id: id, opt: opt, connSign: connSign, deadline: dl, logged: map[string]bool{},
		room:  AllianceRoomID(id.SourceServerID, id.AllianceID),
		res:   &Result{RoomID: AllianceRoomID(id.SourceServerID, id.AllianceID)},
		byKey: map[int64]int{},
	}
	if err := r.run(); err != nil {
		if r.res.Granted {
			return r.res, err
		}
		return nil, err
	}
	return r.res, nil
}

func (r *reader) run() error {
	if _, err := r.await(pushLoginSuccess); err != nil {
		return err
	}
	if r.opt.SynPermission {
		if !r.awaitGrant() {
			slog.Info("chat: room_join_control is on and the server granted no alliance room in time; reading nothing")
			return nil
		}
	} else {
		if err := r.send(cmdJoinMulti, map[string]any{
			"rooms": []map[string]any{{"id": r.room, "group": GroupAlliance}},
		}); err != nil {
			return err
		}
		if _, err := r.await(cmdJoinMulti); err != nil {
			return err
		}
	}
	r.res.Granted = true
	if err := r.send(cmdHistoryRooms, map[string]any{"rooms": map[string]string{r.room: "0"}}); err != nil {
		return err
	}
	f, err := r.await(cmdHistoryRooms)
	if err != nil {
		return err
	}
	r.history(f)
	return r.listen()
}

// send writes one signed frame; only readOnlyCmds can be sent.
func (r *reader) send(cmd string, params map[string]any) error {
	if !readOnlyCmds[cmd] {
		return fmt.Errorf("chat: refusing to send %s: the reader only joins rooms and reads history", cmd)
	}
	p, err := encodeJSON(params)
	if err != nil {
		return err
	}
	sendTime := r.opt.Now().UnixMilli()
	out, err := encodeJSON(struct {
		Cmd      string          `json:"cmd"`
		Params   json.RawMessage `json:"params"`
		SendTime int64           `json:"sendTime"`
		Sign     string          `json:"sign"`
	}{cmd, p, sendTime, frameSign(r.id.UID, p, sendTime, r.connSign)})
	if err != nil {
		return err
	}
	if err := r.ws.writeText(out); err != nil {
		return flat("chat: send %s: %v", cmd, err)
	}
	return nil
}

// next reads the next frame before until (or the read's deadline), skipping heartbeats. A
// timeout returns errTimeout.
func (r *reader) next(until time.Time) (*frame, error) {
	if until.After(r.deadline) {
		until = r.deadline
	}
	for {
		_ = r.ws.conn.SetReadDeadline(until)
		msg, err := r.ws.readMessage()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return nil, errTimeout
			}
			return nil, flat("chat: read: %v", err)
		}
		if string(msg) == heartbeatFrame {
			continue
		}
		f, err := decodeFrame(msg)
		if err != nil {
			slog.Warn("chat: undecodable frame skipped", "bytes", len(msg), "error", err)
			continue
		}
		r.logFirst(f)
		return f, nil
	}
}

var errTimeout = errors.New("chat: timed out")

func decodeFrame(msg []byte) (*frame, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(msg, &raw); err != nil {
		return nil, err
	}
	f := &frame{raw: raw}
	_ = json.Unmarshal(raw["cmd"], &f.cmd)
	return f, nil
}

// logFirst logs the first frame of each cmd at Info: its keys and those of result and data, no
// values (frames carry other players' names and text).
func (r *reader) logFirst(f *frame) {
	if r.logged[f.cmd] {
		return
	}
	r.logged[f.cmd] = true
	args := []any{"cmd", f.cmd, "keys", slices.Sorted(maps.Keys(f.raw))}
	for _, k := range []string{"result", "data"} {
		var sub map[string]json.RawMessage
		if json.Unmarshal(f.raw[k], &sub) == nil && sub != nil {
			args = append(args, k+"Keys", slices.Sorted(maps.Keys(sub)))
		}
	}
	slog.Info("chat: first frame of its kind", args...)
}

// await reads frames until cmd arrives, handling pushes on the way.
func (r *reader) await(cmd string) (*frame, error) {
	until := time.Now().Add(r.opt.StepWait)
	for {
		f, err := r.next(until)
		if errors.Is(err, errTimeout) {
			return nil, fmt.Errorf("chat: no %s within %s", cmd, r.opt.StepWait)
		}
		if err != nil {
			return nil, err
		}
		if f.cmd == cmd {
			return f, nil
		}
		if err := r.push(f); err != nil {
			return nil, err
		}
	}
}

// awaitGrant waits for push.room.synPermission granting the alliance room.
func (r *reader) awaitGrant() bool {
	until := time.Now().Add(r.opt.StepWait)
	for {
		f, err := r.next(until)
		if err != nil {
			return false
		}
		if f.cmd != pushSynPerm {
			if r.push(f) != nil {
				return false
			}
			continue
		}
		// PushRoomSynPermissionMessage.lua:13: the payload is data, else the frame itself.
		body := f.raw
		var data map[string]json.RawMessage
		if json.Unmarshal(f.raw["data"], &data) == nil && data != nil {
			body = data
		}
		var g struct {
			RoomID string `json:"roomId"`
			Auth   bool   `json:"auth"`
			Group  string `json:"group"`
		}
		b, _ := json.Marshal(body)
		if json.Unmarshal(b, &g) != nil || g.Group != GroupAlliance || g.RoomID == "" {
			continue
		}
		if !g.Auth {
			return false
		}
		if g.RoomID != r.room {
			slog.Info("chat: the server granted another alliance room id than the derived one; reading the granted room")
			r.room, r.res.RoomID = g.RoomID, g.RoomID
		}
		return true
	}
}

// push handles a frame that is not the awaited reply.
func (r *reader) push(f *frame) error {
	switch f.cmd {
	case pushAnotherLogin:
		return errors.New("chat: another.login: this uid connected to chat elsewhere")
	case pushNotMember:
		return errors.New("chat: push.no.room.member: not a member of the room")
	case pushChatRoom, pushMsgUpdate:
		var data map[string]json.RawMessage
		if json.Unmarshal(f.raw["data"], &data) != nil || data == nil {
			return nil
		}
		if roomOf(data) != r.room {
			return nil
		}
		r.res.Pushes++
		r.keep(data, f.cmd == pushMsgUpdate)
	}
	return nil
}

// listen reads pushes for opt.Listen.
func (r *reader) listen() error {
	until := time.Now().Add(r.opt.Listen)
	for {
		f, err := r.next(until)
		if errors.Is(err, errTimeout) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := r.push(f); err != nil {
			return err
		}
	}
}

// history keeps the target posts of the history.roomsv2 reply: result.rooms[{roomId, msgs}]
// (GetHistoryMsgsCommand.lua:23-53; ChatRoomManager:onRequestLatestMsg, ChatRoomManager.lua:
// 1086-1111).
func (r *reader) history(f *frame) {
	var res struct {
		Rooms []struct {
			RoomID string                       `json:"roomId"`
			Msgs   []map[string]json.RawMessage `json:"msgs"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal(f.raw["result"], &res); err != nil {
		slog.Warn("chat: history reply without a readable result", "error", err)
		return
	}
	for _, room := range res.Rooms {
		if room.RoomID != r.room {
			continue
		}
		for _, m := range room.Msgs {
			r.res.HistoryMsgs++
			if t := msgTimeMs(m); t > 0 && (r.res.OldestMs == 0 || t < r.res.OldestMs) {
				r.res.OldestMs = t
			}
			r.keep(m, false)
		}
	}
}

// keep records m when it is a kept post. An update push carries the message again
// (PushChatMsgUpdateRoomMessage.lua:6-31): its claimer list replaces the earlier one.
func (r *reader) keep(m map[string]json.RawMessage, update bool) {
	seq, ok := num(m["seqId"])
	var extra map[string]json.RawMessage
	_ = json.Unmarshal(m["extra"], &extra)
	if !ok {
		seq, ok = num(extra["seqId"])
	}
	if !ok {
		return
	}
	i, seen := r.byKey[seq]
	post, hasPost := num(extra["post"])
	if !hasPost {
		if update && seen {
			if cue, present := str(m["clientUpdateExtra"]); present {
				r.res.Posts[i].ClientUpdateExtra = cue
			}
		}
		return
	}
	if !keptPosts[int(post)] {
		return
	}
	p := Post{Type: int(post), RoomID: r.room, SeqID: seq, ServerTimeMs: msgTimeMs(m)}
	p.Group, _ = str(m["group"])
	p.Sender, _ = str(m["sender"])
	p.CustomJSON, _ = str(extra["customJsonParam"])
	p.ClientUpdateExtra, _ = str(m["clientUpdateExtra"])
	if seen {
		r.res.Posts[i] = p
		return
	}
	r.byKey[seq] = len(r.res.Posts)
	r.res.Posts = append(r.res.Posts, p)
}

func roomOf(m map[string]json.RawMessage) string {
	s, _ := str(m["roomId"])
	return s
}

// msgTimeMs is a message's serverTime in ms: a 10-digit value is seconds
// (ChatMessage:onParseServerData, ChatMessage.lua:493-499).
func msgTimeMs(m map[string]json.RawMessage) int64 {
	t, ok := num(m["serverTime"])
	if !ok {
		return 0
	}
	if t < 1e11 {
		return t * 1000
	}
	return t
}

// num reads a JSON number or a decimal string (Lua's checknumber/tonumber take both).
func num(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var n json.Number
	if err := json.Unmarshal(raw, &n); err == nil {
		if i, err := n.Int64(); err == nil {
			return i, true
		}
		if f, err := n.Float64(); err == nil {
			return int64(f), true
		}
		return 0, false
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return 0, false
	}
	if i, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64); err == nil {
		return i, true
	}
	return 0, false
}

// str reads a JSON string, or a number as its decimal text.
func str(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String(), true
	}
	return "", false
}
