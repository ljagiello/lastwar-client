package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"lastwar-client/internal/chat"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Chat-driven claims (gaps G55, G56; MASTER §7 WP13), OPT-IN, static-only. The chat itself runs
// over a separate WebSocket that internal/chat reads once per run, read-only: it joins the
// alliance room, pulls the newest history page, listens a few seconds and closes. Nothing is ever
// posted, liked, shared or marked read. chatCards caches that read per (conn, in), so every
// chat-driven feature of one run shares one chat connection, and none is opened unless such a
// feature runs.
//
// red-packets opens the red packets (post 611) posted in the alliance room. A tap on the card
// opens LWUIRedPacketOperation (RedPackage.lua:23-37), whose open button sends
//
//	open.red.packet {uuid: Utf, cfgId: Int <packetId>, chatType: Int, serverId: Int}
//
// with uuid, packetId and serverId (else 0) from the card's customJsonParam and chatType from the
// room's group, Alliance = 2 (LWUIRedPacketOperationView.lua:98-112; OpenRedPacketMessage.lua:5-10;
// RedPacketManager.ChannelType, RedPacketManager.lua:3-8). The reply opens the details window
// (RedPacketManager.lua:56-68), whose like button and gift button are taps of their own
// (LWUIRedPacketDetailsView.lua:44-82, 345-351): nothing else is sent after an open. The like and
// the "lucky" share post the client sends automatically belong to the legacy alliance red
// envelope (get.red.pack -> AllianceRedPacketManager:UpdateRecordByUUid,
// AllianceRedPacketManager.lua:66-117, from GetRedPackMessage.lua:20), not to post 611; that flow
// (redPackets.status -> get.red.pack) is left out.
//
// A packet is opened when, as for the card and the window:
//   - the card decodes and names a packetId of table red_packet (40413), whose num caps the
//     claimers; packets with a send_receive_condition (rows 301, 311, 321: fewer than 5 of item
//     650071, RedPacketTemplate:Condition) are skipped, because the item count is not evaluated;
//   - CanNotPickRedPacket is false: campId > 0 needs the server's camp index, which Go does not
//     have, so such a packet is skipped; otherwise banServerId must not be the source server
//     (ChatManager2.lua:683-691);
//   - it has not expired: now < expiredTime ms (GetIsOverdue, LWUIRedPacketOperationView.lua:
//     141-148); a card without expiredTime is skipped;
//   - the player is not in its claimer list and the list is short of num (GetIsReceive and
//     GetIsNone, LWUIRedPacketOperationView.lua:130-154). The player's own packets are skipped
//     too: the window lets the sender open one only while hasRob is unset, but
//     RedPacketManager:GetIsReceive counts every own packet as received (RedPacketManager.lua:
//     103-115), and skipping is the safe reading.
//
// At most red_pocket_config.k1 (10; item table, 40413) packets a run, one a second, the one that
// expires first first. errorCode 400600 is that daily cap (OpenRedPacketMessage.lua:13-19): the
// run stops claiming, without an error. Any other errorCode stops the run's claims with an error;
// no code is registered as benign. Other players see the claim: the server adds the player to the
// packet's claimer list on the chat card.
//
// chat-scan is read-only: it logs every red packet, firework card (693/695) and treasure-dug card
// (600) of that read with its verdict. Claims from the firework and treasure cards belong next to
// the fireworks and alliance-treasures features, which can read the same cached chatCards.
//
// Not done: the country, season and alliance-friend rooms (internal/chat explains why), the legacy
// alliance red envelopes, red.packet.detail (the window's details button), and sending or copying
// red packets.

const (
	chatScanFeature   = "chat-scan"
	redPacketsFeature = "red-packets"
	redPacketOpenCmd  = "open.red.packet"

	rpDailyCapCode     = "400600"
	rpMaxClaimsPerRun  = 10 // DataConfig red_pocket_config.k1 (item table, 40413)
	rpChatTypeAlliance = 2  // RedPacketManager.ChannelType.Alliance

	// chatUseNewLineBit is ServerSwitch.ChatUseNewLine (EnumType.lua:10779), a bit of init
	// immediate_toggle (FunctionOnManager.lua:100-106; InitMessage.lua:250).
	chatUseNewLineBit = 12
	// chatFireworkLifetime is DataConfig fireworks.k2 (item table, 40413): a post 693 card expires
	// that long after its serverTime (FireworkGift.lua:66-68).
	chatFireworkLifetime = 120 * time.Minute
)

// rpTemplate is one red_packet row: num, and whether send_receive_condition limits who may open.
type rpTemplate struct {
	num          int
	hasCondition bool
}

// redPacketTemplates is table red_packet (40413): id -> num and send_receive_condition present.
var redPacketTemplates = map[int64]rpTemplate{
	1: {num: 20}, 2: {num: 5}, 3: {num: 20}, 4: {num: 20}, 5: {num: 20},
	100: {num: 20}, 101: {num: 20},
	201: {num: 20}, 202: {num: 20}, 203: {num: 20}, 204: {num: 20}, 205: {num: 20}, 206: {num: 20},
	207: {num: 20}, 208: {num: 20}, 209: {num: 20}, 210: {num: 20}, 211: {num: 20}, 212: {num: 20},
	213: {num: 20}, 214: {num: 20}, 215: {num: 20},
	301: {num: 5, hasCondition: true}, 311: {num: 10, hasCondition: true}, 321: {num: 10, hasCondition: true},
	341: {num: 20}, 342: {num: 20}, 343: {num: 20}, 344: {num: 20}, 345: {num: 20}, 346: {num: 20},
	347: {num: 20}, 348: {num: 20},
	401: {num: 40}, 501: {num: 20}, 502: {num: 20}, 601: {num: 20},
}

// rpClaimPace is the gap between opens, about a person tapping one card after another.
var rpClaimPace = time.Second

// rpSleep is time.Sleep; tests replace it.
var rpSleep = time.Sleep

// chatRead and chatOptions are the chat reader; tests replace them.
var (
	chatRead    = chat.Read
	chatOptions = chat.DefaultOptions
)

func init() {
	registerFeature(Feature{
		Name: chatScanFeature,
		Summary: "read-only: read the alliance chat room over the chat WebSocket (joins and reads history, never posts, " +
			"likes or shares) and log every red packet, firework chest card and treasure-dug card with whether it is " +
			"claimable (static-only, not validated live)",
		Run: runChatScan,
	})
	registerFeature(Feature{
		Name: redPacketsFeature,
		Summary: "OPT-IN: open the red packets posted in alliance chat (open.red.packet, free, at most 10 a day); reads " +
			"chat, never posts, likes or shares; your name shows in the packet's claimer list (static-only, not validated live)",
		Run: runRedPackets,
	})
}

// chatSelf is what the verdicts need about the player.
type chatSelf struct {
	uid          string
	sourceServer int64
}

// chatRunRead is one run's chat read.
type chatRunRead struct {
	conn *session.GameConn
	in   *Init
	res  *chat.Result
	me   chatSelf
	err  error
}

var (
	chatReadMu   sync.Mutex
	chatReadLast *chatRunRead
)

// chatCards reads the alliance chat room once per (conn, in) and returns the read, the player and
// the read's error. res is nil when nothing was read: not in an alliance, no chatToken, or the
// read failed (err says which).
func chatCards(conn *session.GameConn, in *Init) (*chat.Result, chatSelf, error) {
	chatReadMu.Lock()
	defer chatReadMu.Unlock()
	if c := chatReadLast; c != nil && c.conn == conn && c.in == in {
		return c.res, c.me, c.err
	}
	c := &chatRunRead{conn: conn, in: in}
	chatReadLast = c
	id, why := chatIdentity(in)
	c.me = chatSelf{uid: id.UID, sourceServer: id.SourceServerID}
	if why != "" {
		slog.Info("chat: not reading chat", "reason", why)
		return nil, c.me, nil
	}
	opt := chatOptions()
	opt.Now = func() time.Time { return evNow() }
	opt.NewLineList = claimSwitchOn(in, "CheckChatNewLine")
	opt.SynPermission = claimSwitchOn(in, "room_join_control")
	if t, ok := evNum(in.Raw, "immediate_toggle"); ok {
		opt.UseDataNew = t&(1<<chatUseNewLineBit) != 0
	}
	start := time.Now()
	c.res, c.err = chatRead(id, opt)
	if c.err != nil {
		slog.Warn("chat: read failed", "error", c.err)
	}
	if r := c.res; r != nil {
		oldestMin := int64(-1)
		if r.OldestMs > 0 {
			oldestMin = (evNow().UnixMilli() - r.OldestMs) / 60000
		}
		slog.Info("chat: alliance room read", "granted", r.Granted, "historyMsgs", r.HistoryMsgs,
			"oldestMsgAgeMin", oldestMin, "pushes", r.Pushes, "keptPosts", len(r.Posts),
			"synPermission", opt.SynPermission, "newLineList", opt.NewLineList, "useDataNew", opt.UseDataNew,
			"tookMs", time.Since(start).Milliseconds())
	}
	return c.res, c.me, c.err
}

// chatIdentity reads the chat identity from init; why is non-empty when chat can't be read. Only
// uid, serverId, crossFightSrcServerId and allianceId of user are read; user holds PII.
func chatIdentity(in *Init) (chat.Identity, string) {
	if in == nil || in.Raw == nil {
		return chat.Identity{}, "no init"
	}
	user := in.Object("user")
	id := chat.Identity{
		UID:        evString(user, "uid"),
		AllianceID: evString(user, "allianceId"),
		ChatToken:  evString(in.Raw, "chatToken"),
	}
	// PlayerInfo:GetSelfServerId and GetSourceServerId (PlayerInfo.lua:794-812).
	id.SelfServerID, _ = evNum(user, "serverId")
	id.SourceServerID = id.SelfServerID
	if src, ok := evNum(user, "crossFightSrcServerId"); ok && src >= 0 {
		id.SourceServerID = src
	}
	switch {
	case id.UID == "":
		return id, "own uid unknown"
	case id.AllianceID == "":
		return id, "not in an alliance" // AddAllianceRoom needs IsInAlliance (ChatRoomManager.lua:304-310)
	case id.ChatToken == "":
		return id, "init has no chatToken"
	case id.SelfServerID <= 0 || id.SourceServerID <= 0:
		return id, "own server unknown"
	}
	return id, ""
}

// redPacketVerdict decodes a post 611 card and applies the open rules at nowMs; "" is openable.
func redPacketVerdict(p chat.Post, me chatSelf, nowMs int64) (chat.RedPacket, rpTemplate, string) {
	rp, err := p.RedPacket()
	if err != nil {
		return rp, rpTemplate{}, "unreadable card: " + err.Error()
	}
	tpl, known := redPacketTemplates[rp.PacketID]
	switch {
	case p.Group != "" && p.Group != chat.GroupAlliance:
		return rp, tpl, "not an alliance-room post"
	case !rp.HasPacketID:
		return rp, tpl, "no packetId"
	case !known:
		return rp, tpl, "packetId not in table red_packet"
	case tpl.hasCondition:
		return rp, tpl, "receive condition (item count) not evaluated"
	case rp.CampID > 0:
		return rp, tpl, "camp-restricted packet (camp index unknown)"
	case rp.BanServerID == me.sourceServer:
		return rp, tpl, "barred for the own server"
	case !rp.HasExpiry:
		return rp, tpl, "no expiredTime"
	case rp.ExpiredTime <= nowMs:
		return rp, tpl, "expired"
	case p.Sender != "" && p.Sender == me.uid:
		return rp, tpl, "own packet"
	case slices.Contains(rp.Claimers, me.uid):
		return rp, tpl, "already opened by you"
	case len(rp.Claimers) >= tpl.num:
		return rp, tpl, "all taken"
	}
	return rp, tpl, ""
}

// senderRef names a post's sender without their uid.
func senderRef(p chat.Post, me chatSelf) string {
	switch p.Sender {
	case "":
		return "none"
	case me.uid:
		return "self"
	}
	return "other"
}

// rpPick is one openable packet.
type rpPick struct {
	post chat.Post
	rp   chat.RedPacket
}

// reviewRedPackets logs every red packet with its verdict and returns the openable ones, the one
// that expires first first.
func reviewRedPackets(name string, res *chat.Result, me chatSelf, nowMs int64) []rpPick {
	var picks []rpPick
	seen := map[string]bool{}
	for _, p := range res.Posts {
		if p.Type != chat.PostRedPacket {
			continue
		}
		rp, tpl, why := redPacketVerdict(p, me, nowMs)
		if why == "" && seen[rp.UUID] {
			why = "duplicate card"
		}
		expiresIn := int64(-1)
		if rp.HasExpiry {
			expiresIn = (rp.ExpiredTime - nowMs) / 60000
		}
		slog.Info(name+": red packet", "seqId", p.SeqID, "packetId", rp.PacketID, "goodsId", rp.GoodsID,
			"sender", senderRef(p, me), "group", p.Group, "claimers", len(rp.Claimers), "shownTaken", rp.Taken(), "num", tpl.num, "expiresInMin", expiresIn,
			"hasRob", rp.HasRob, "campId", rp.CampID, "eligible", why == "", "reason", why)
		if why == "" {
			seen[rp.UUID] = true
			picks = append(picks, rpPick{post: p, rp: rp})
		}
	}
	slices.SortStableFunc(picks, func(a, b rpPick) int {
		return cmp.Or(cmp.Compare(a.rp.ExpiredTime, b.rp.ExpiredTime), cmp.Compare(a.post.SeqID, b.post.SeqID))
	})
	return picks
}

func runChatScan(conn *session.GameConn, in *Init) error {
	res, me, err := chatCards(conn, in)
	if res == nil {
		return err
	}
	now := evNow()
	nowMs := now.UnixMilli()
	picks := reviewRedPackets(chatScanFeature, res, me, nowMs)
	counts := map[int]int{}
	for _, p := range res.Posts {
		counts[p.Type]++
		ageMin := int64(-1)
		if p.ServerTimeMs > 0 {
			ageMin = (nowMs - p.ServerTimeMs) / 60000
		}
		switch p.Type {
		case chat.PostFireworkGift, chat.PostS0BossFireworks:
			f, ferr := p.Firework()
			if ferr != nil {
				slog.Info(chatScanFeature+": firework card unreadable", "post", p.Type, "seqId", p.SeqID, "error", ferr)
				continue
			}
			expired := f.ExpiredTime > 0 && f.ExpiredTime <= nowMs // ChatItemS0AllianceBossPushMsg.GetIsOverdue
			if p.Type == chat.PostFireworkGift {
				expired = p.ServerTimeMs > 0 && !now.Before(time.UnixMilli(p.ServerTimeMs).Add(chatFireworkLifetime))
			}
			owner := "other"
			if f.OwnerUID == me.uid {
				owner = "self"
			}
			slog.Info(chatScanFeature+": firework card", "post", p.Type, "seqId", p.SeqID, "uuid", f.UUID,
				"configId", f.ConfigID, "index", f.Index, "owner", owner, "claimers", len(f.Claimers),
				"claimedByYou", slices.Contains(f.Claimers, me.uid), "ageMin", ageMin, "expired", expired,
				"pointId", f.PointID, "serverId", f.ServerID)
		case chat.PostTreasureDug:
			t, terr := p.TreasureDug()
			if terr != nil {
				slog.Info(chatScanFeature+": treasure card unreadable", "seqId", p.SeqID, "error", terr)
				continue
			}
			slog.Info(chatScanFeature+": treasure-dug card", "seqId", p.SeqID, "uuid", t.UUID,
				"targetServer", t.TargetServer, "hasTargetServer", t.HasTarget, "pointId", t.PointID,
				"eventId", t.EventID, "sender", senderRef(p, me), "ageMin", ageMin)
		}
	}
	slog.Info(chatScanFeature+": done", "redPackets", counts[chat.PostRedPacket], "openable", len(picks),
		"fireworkCards", counts[chat.PostFireworkGift], "s0BossFireworkCards", counts[chat.PostS0BossFireworks],
		"treasureCards", counts[chat.PostTreasureDug])
	return err
}

func runRedPackets(conn *session.GameConn, in *Init) error {
	res, me, err := chatCards(conn, in)
	if res == nil {
		return err
	}
	errs := []error{err}
	picks := reviewRedPackets(redPacketsFeature, res, me, evNow().UnixMilli())
	if len(picks) > rpMaxClaimsPerRun {
		slog.Info(redPacketsFeature+": more openable packets than the daily cap; the rest wait",
			"openable", len(picks), "cap", rpMaxClaimsPerRun)
		picks = picks[:rpMaxClaimsPerRun]
	}
	opened, loggedFirst := 0, false
	for i, pk := range picks {
		if i > 0 {
			rpSleep(rpClaimPace)
		}
		p := sfs.NewSFSObject()
		p.PutUtfString("uuid", pk.rp.UUID)
		p.PutInt("cfgId", int32(pk.rp.PacketID))
		p.PutInt("chatType", rpChatTypeAlliance)
		p.PutInt("serverId", int32(pk.rp.ServerID))
		resp, code, rerr := rpRequest(conn, redPacketOpenCmd, p)
		if rerr != nil {
			errs = append(errs, rerr)
			break
		}
		if !loggedFirst {
			loggedFirst = true
			newCount, _ := evNum(resp, "red_packet_new")
			newExpire, _ := evNum(resp, "red_packet_new_expire")
			slog.Info(redPacketsFeature+": first "+redPacketOpenCmd+" reply", "errorCode", code, "replyKeys", resp.Keys(),
				"redPacketKeys", evObject(resp, "redPacket").Keys(), "red_packet_new", newCount,
				"red_packet_new_expire", newExpire, "granted", describeGrants(resp))
		}
		who := []any{"seqId", pk.post.SeqID, "packetId", pk.rp.PacketID}
		switch code {
		case "":
			opened++
			slog.Info(redPacketsFeature+": opened a red packet", append(who, "granted", describeGrants(resp))...)
			continue
		case rpDailyCapCode:
			slog.Info(redPacketsFeature+": daily red packet cap reached (400600); stopping", who...)
		default:
			slog.Warn(redPacketsFeature+": open refused; stopping this run", append(who, "errorCode", code)...)
			errs = append(errs, fmt.Errorf("%s: %s packet %d: errorCode=%s", redPacketsFeature, redPacketOpenCmd, pk.rp.PacketID, code))
		}
		break
	}
	slog.Info(redPacketsFeature+": done", "opened", opened, "openable", len(picks))
	return errors.Join(errs...)
}

// rpRequest sends cmd and returns the reply and its errorCode ("" when none). An errorCode is not
// an error here: the caller decides, so no code needs registering as benign.
func rpRequest(conn *session.GameConn, cmd string, params *sfs.SFSObject) (*sfs.SFSObject, string, error) {
	if err := conn.SendExtension(cmd, params); err != nil {
		wrapped := session.SendStageError{Err: err}
		slog.Error(redPacketsFeature+": send failed", "cmd", cmd, "error", wrapped)
		return nil, "", wrapped
	}
	msg, err := session.WaitForCmd(conn, session.DefaultCmdTimeout, cmd)
	if err != nil {
		slog.Error(redPacketsFeature+": no response", "cmd", cmd, "error", err)
		return nil, "", err
	}
	return msg.Params, evString(msg.Params, "errorCode"), nil
}
