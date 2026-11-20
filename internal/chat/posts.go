package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The kept posts' cards. Each carries extra.customJsonParam, a JSON string the client decodes with
// rapidjson; its uuids are Longs, read here without going through float64.

// RedPacket is a post 611 card (ChatItemPost_RedPackage:RefreshView and LWUIRedPacketOperationView,
// UI/UIChatNewV2/Component/ChatItem/Post/RedPackage.lua:55-153; UI/LWUIRedPacketOperation/View/
// LWUIRedPacketOperationView.lua:98-154, 204-221).
type RedPacket struct {
	UUID        string // sent back as a UtfString (OpenRedPacketMessage.lua:7)
	PacketID    int64  // red_packet id, sent as cfgId
	HasPacketID bool
	GoodsID     int64
	ServerID    int64 // extraJson.serverId or 0
	ExpiredTime int64 // ms; 0 when absent
	HasExpiry   bool
	HasRob      bool // Lua truthiness: present and not false/null
	BanServerID int64
	CampID      int64
	// Claimers is the second "|" field of clientUpdateExtra split on "," (RedPackage.lua:89-95).
	Claimers []string
}

// Firework is a post 693 or 695 card (ChatItemPost_FireworkGift:RefreshView,
// UI/UIChatNewV2/Component/ChatItem/Post/FireworkGift.lua:58-104; ChatItemS0AllianceBossPushMsg,
// UI/UIChatNew/Component/ChatItem/ChatItemS0AllianceBossPushMsg.lua:34-61).
type Firework struct {
	UUID     int64
	OwnerUID string
	ConfigID int64
	Index    int64 // the reward slot in firework.reward; 0 when absent
	// ExpiredTime is the card's own expiry (ms), which only post 695 uses; post 693 expires
	// DataConfig fireworks.k2 minutes after the message's serverTime (FireworkGift.lua:66-68).
	ExpiredTime int64
	PointID     int64 // post 695 only
	ServerID    int64 // post 695 only
	// Claimers is clientUpdateExtra split on "|" (FireworkGift.lua:97-103).
	Claimers []string
}

// TreasureDug is a post 600 card (ChatPushMsg:OnPointerClick, UI/UIChatNew/Component/ChatPushMsg/
// ChatPushMsg.lua:62-70; Decode_DetectTreasureFinInfo, Chat/Other/ShareDecode.lua:723-757).
type TreasureDug struct {
	UUID         int64
	TargetServer int64 // 0 when absent: the client then sends its source server
	HasTarget    bool
	PointID      int64
	EventID      int64
}

func (p Post) custom() (map[string]json.RawMessage, error) {
	if p.CustomJSON == "" {
		return nil, errors.New("no customJsonParam")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(p.CustomJSON), &m); err != nil || m == nil {
		return nil, errors.New("customJsonParam is not a JSON object")
	}
	return m, nil
}

// RedPacket decodes a post 611 card.
func (p Post) RedPacket() (RedPacket, error) {
	if p.Type != PostRedPacket {
		return RedPacket{}, fmt.Errorf("post %d is not a red packet", p.Type)
	}
	m, err := p.custom()
	if err != nil {
		return RedPacket{}, err
	}
	var rp RedPacket
	rp.UUID, _ = str(m["uuid"])
	if rp.UUID == "" {
		return RedPacket{}, errors.New("red packet without a uuid")
	}
	rp.PacketID, rp.HasPacketID = num(m["packetId"])
	rp.GoodsID, _ = num(m["goodsId"])
	rp.ServerID, _ = num(m["serverId"])
	rp.ExpiredTime, rp.HasExpiry = num(m["expiredTime"])
	rp.HasRob = luaTruthy(m["hasRob"])
	rp.BanServerID, _ = num(m["banServerId"])
	rp.CampID, _ = num(m["campId"])
	if parts := strings.Split(p.ClientUpdateExtra, "|"); len(parts) >= 2 {
		rp.Claimers = nonEmpty(strings.Split(parts[1], ","))
	}
	return rp, nil
}

// Taken is the claim count the card shows: max(#claimers, 1 if hasRob)
// (RedPacketManager:GetReceiveCountByChatData, DataCenter/RedPacketManager/RedPacketManager.lua:
// 281-296).
func (rp RedPacket) Taken() int {
	if rp.HasRob {
		return max(len(rp.Claimers), 1)
	}
	return len(rp.Claimers)
}

// Firework decodes a post 693 or 695 card.
func (p Post) Firework() (Firework, error) {
	if p.Type != PostFireworkGift && p.Type != PostS0BossFireworks {
		return Firework{}, fmt.Errorf("post %d is not a firework card", p.Type)
	}
	m, err := p.custom()
	if err != nil {
		return Firework{}, err
	}
	var f Firework
	var ok bool
	if f.UUID, ok = num(m["uuid"]); !ok {
		return Firework{}, errors.New("firework card without a uuid")
	}
	f.OwnerUID, _ = str(m["ownerUid"])
	f.ConfigID, _ = num(m["configId"])
	f.Index, _ = num(m["index"])
	f.ExpiredTime, _ = num(m["expiredTime"])
	f.PointID, _ = num(m["pointId"])
	f.ServerID, _ = num(m["serverId"])
	f.Claimers = nonEmpty(strings.Split(p.ClientUpdateExtra, "|"))
	return f, nil
}

// TreasureDug decodes a post 600 card.
func (p Post) TreasureDug() (TreasureDug, error) {
	if p.Type != PostTreasureDug {
		return TreasureDug{}, fmt.Errorf("post %d is not a treasure-dug card", p.Type)
	}
	m, err := p.custom()
	if err != nil {
		return TreasureDug{}, err
	}
	var t TreasureDug
	var ok bool
	if t.UUID, ok = num(m["uuid"]); !ok {
		return TreasureDug{}, errors.New("treasure card without a uuid")
	}
	t.TargetServer, t.HasTarget = num(m["targetServer"])
	t.PointID, _ = num(m["pointId"])
	t.EventID, _ = num(m["eventId"])
	return t, nil
}

// luaTruthy is Lua's truth test on a decoded JSON value: everything but absent, null and false.
func luaTruthy(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "false"
}

func nonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
