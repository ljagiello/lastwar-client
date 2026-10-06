package game

import (
	"fmt"
	"log/slog"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// evAlreadyExecuted is the server's generic "already executed" errorCode, the only already-claimed
// code confirmed live (vip.go, 120289). Each event claim registers it as benign. No claim handler
// in the Lua branches on a specific code (all show the generic tip), so any other code stays a
// failure until a live log shows what it means (MASTER.md §4 "Before any of these ship").
const evAlreadyExecuted = "120289"

// eventFreebie is one event type's daily free pack (MASTER.md §4 #10). The event-shop panel shows
// the pack while the activity's reward list is non-empty and claims it when the last free claim is
// not in the current server day (UILuckyRollShopItem.lua:38-77; ActivityListDataManager:
// GetActHasFreeDailyReward, :2257-2266). Each type keeps that state in its own detail reply.
type eventFreebie struct {
	typ   int32
	claim string
	// info fetches the detail reply the client parses the pack from (the request AddOneActivity
	// sends for the type, ActivityListDataManager.lua:154-932); nil means hero.event.info.get.
	info func(s *ActivitySweep, a Activity) (*sfs.SFSObject, error)
	// due reports whether the pack exists and is unclaimed today.
	due func(in *Init, info *sfs.SFSObject) bool
	// params builds the claim; nil means {activityId: Int}.
	params func(a Activity, info *sfs.SFSObject) (*sfs.SFSObject, bool)
}

// eventFreebies, in type order. Static-only; units per type from the data classes cited.
//
// Not claimed: Bounty Hunter (350, bounty.hunter.receive.free.reward). Its pack exists only when
// table activity_hunter.box_reward > 0 (BountyHunterActData.lua:210-213), and every 1.0.364 row is
// 0, so the real client never shows it.
var eventFreebies = []eventFreebie{
	// LuckyRoll: get.lucky.roll.info {activityId: Int}; rollInfo.lastReceiveFreeTime in ms; the pack
	// is Lucky_roulette_reward (ActLuckyRollInfo.lua:28, 85, 169-178).
	{typ: actTypeLuckyRoll, claim: "roll.receive.free.reward",
		info: evIntInfo("lucky roll info response", "get.lucky.roll.info", "activityId"),
		due: func(in *Init, info *sfs.SFSObject) bool {
			return evNonEmpty(info, "Lucky_roulette_reward") && evNotToday(in, evObject(info, "rollInfo"), "lastReceiveFreeTime")
		}},
	// GiftBox: get.activity.gift.box.info {activityId: Int}; ActivityFreeRewardData state flag
	// (ActGiftBoxInfo.lua:102), which ignores `reward` for this type.
	{typ: actTypeGiftBox, claim: "gift.box.free.reward",
		info: evIntInfo("gift box info response", "get.activity.gift.box.info", "activityId"),
		due:  func(_ *Init, info *sfs.SFSObject) bool { return evFreeFlagDue(info, false) }},
	// LuckyShop: hero.event.info.get (activityType 134) -> LuckyShopData:ParseData -> the same flag
	// (LuckyShopManager.lua:38-60).
	{typ: actTypeLuckyShop, claim: "discount.free.reward",
		due: func(_ *Init, info *sfs.SFSObject) bool { return evFreeFlagDue(info, true) }},
	// Cooking: activity.mak.food.info {aid: Int}; the client reads only info[0], whose id is the
	// claim's id (ActivityMakeFoodInfoMessage.lua:14; ActCookingData.lua:24-45).
	{typ: actTypeCooking, claim: "activity.mak.food.free.reward",
		info: evIntInfo("cooking info response", "activity.mak.food.info", "aid"),
		due: func(_ *Init, info *sfs.SFSObject) bool {
			return evFreeFlagDue(evFirstInfo(info), true)
		},
		params: evAidIDParams},
	// Banquet: activity.food.party.info {aid: Int}; same shape (ActivityFoodPartyInfoMessage.lua:14;
	// ActBanquetData.lua:45-70).
	{typ: actTypeBanquet, claim: "activity.food.party.reward.free",
		info: evIntInfo("banquet info response", "activity.food.party.info", "aid"),
		due: func(_ *Init, info *sfs.SFSObject) bool {
			return evFreeFlagDue(evFirstInfo(info), true)
		},
		params: evAidIDParams},
	// Monopoly: top-level lastReceiveFreeTime in seconds, pack dayReward (ActMonopolyData.lua:173-178, 272-278).
	{typ: actTypeMonopoly, claim: "rich.man.day.reward", due: evDayRewardDue},
	// Bargain Shop: same fields, seconds (ActBargainShopInfo.lua:61-68, 111-129).
	{typ: actTypeBargainShop, claim: "bargain.day.reward", due: evDayRewardDue},
	// Titanium Blue Store: same fields, seconds (LWTitaniumBlueStoreManager.lua:72-79, 170).
	{typ: actTypeBlueStore, claim: "blue.shop.day.reward", due: evDayRewardDue},
	// Slot machine: same field name but ms (ActSlotMachineData.lua:78-82, 124-130).
	{typ: actTypeSlotMachine, claim: "slots.daily.reward", due: evDayRewardDue},
	// Decoration gacha: freeReward present and (nextResetTime, seconds, has passed or freeState is
	// 0) (ActivityDecorationGachaData.lua:18-29).
	{typ: actTypeDecorationGacha, claim: "decoration.free.reward",
		due: func(_ *Init, info *sfs.SFSObject) bool {
			if !evNonEmpty(info, "freeReward") {
				return false
			}
			reset, hasReset := evNum(info, "nextResetTime")
			state, hasState := evNum(info, "freeState")
			return !hasReset || !evNow().Before(evUnix(reset)) || (hasState && state == 0)
		}},
	// Torch relay: lastReceiveFreeTime in ms, absent meaning never claimed; all three
	// activity_torch_relay rows have free_iap_reward > 0 (ActivityTorchRelayData.lua:201-212).
	{typ: actTypeTorchRelay, claim: "torch.relay.daily.reward",
		due: func(in *Init, info *sfs.SFSObject) bool { return evNotToday(in, info, "lastReceiveFreeTime") }},
	// Survival VIP gift: survival.vip.gift.get.info {activityId: Int}; claimable while isShow is not
	// false and freeReceived is not true (VipGiftActDataManager.lua:444-455, 592-608).
	{typ: actTypeSurvivalVipGift, claim: "survival.vip.gift.receive.free",
		info: evIntInfo("survival vip gift info response", "survival.vip.gift.get.info", "activityId"),
		due: func(_ *Init, info *sfs.SFSObject) bool {
			if v, ok := info.Get("isShow"); ok {
				switch s := v.Val.(type) {
				case bool:
					if !s {
						return false
					}
				case string:
					if s == "0" || s == "false" {
						return false
					}
				}
			}
			v, ok := info.Get("freeReceived")
			received, _ := v.Val.(bool)
			return !ok || !received
		}},
}

func init() {
	registerFeature(Feature{
		Name:    "event-shop-freebies",
		Summary: "claim each running event's daily free pack (Monopoly, Slots, Bargain, Blue Store, Lucky Roll, ...); static-only",
		Run:     runEventShopFreebies,
	})
	for _, f := range eventFreebies {
		session.RegisterBenignErrorCode(evAlreadyExecuted, f.claim)
	}
}

func runEventShopFreebies(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	if _, ok := in.ServerDayStart(evNow()); !ok {
		slog.Warn("event-shop-freebies: init has no tomorrow, so today's claims can't be told apart; skipping")
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	for _, f := range eventFreebies {
		for _, a := range s.Open(f.typ) {
			if b.dead {
				return b.err()
			}
			fetch := f.info
			if fetch == nil {
				fetch = (*ActivitySweep).EventInfo
			}
			info, err := fetch(s, a)
			if err != nil {
				b.add(err)
				continue
			}
			if !f.due(in, info) {
				continue
			}
			params := evActivityIDParam(a)
			if f.params != nil {
				var ok bool
				if params, ok = f.params(a, info); !ok {
					continue
				}
			}
			b.send(fmt.Sprintf("event free pack (activity %d)", a.ID), f.claim, params)
		}
	}
	return b.err()
}

// evActivityIDParam is the {activityId: Int} most event claims take.
func evActivityIDParam(a Activity) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutInt("activityId", a.ID)
	return p
}

// evIntInfo is a detail request that takes the activity id as an Int under key.
func evIntInfo(label, cmd, key string) func(s *ActivitySweep, a Activity) (*sfs.SFSObject, error) {
	return func(s *ActivitySweep, a Activity) (*sfs.SFSObject, error) {
		p := sfs.NewSFSObject()
		p.PutInt(key, a.ID)
		return s.Info(label, cmd, a, p)
	}
}

// evNotToday reports whether o[key] (s or ms) is not in the current server day; absent counts as
// never claimed.
func evNotToday(in *Init, o *sfs.SFSObject, key string) bool {
	ts, _ := evNum(o, key)
	claimed, ok := evClaimedToday(in, ts)
	return ok && !claimed
}

// evDayRewardDue is the Monopoly/Bargain/Blue Store/Slots rule: dayReward non-empty and
// lastReceiveFreeTime not today.
func evDayRewardDue(in *Init, info *sfs.SFSObject) bool {
	return evNonEmpty(info, "dayReward") && evNotToday(in, info, "lastReceiveFreeTime")
}

// evFreeFlagDue is ActivityFreeRewardData's rule (ActivityFreeRewardData.lua:18-84), used by Gift
// Box, Lucky Shop, Cooking and Banquet. The server sends no claim time, only a state flag read from
// free_reward, then freeReward, then free (the last present wins; 0 claimable, 1 claimed), and the
// pack's reward list from boxReward, reward (not for Gift Box), box_reward or free_reward.
// free_reward can be either, so only numeric values count as the flag. Unlike the client, an
// absent flag is not claimable here: which key each type sends needs a live capture.
func evFreeFlagDue(o *sfs.SFSObject, rewardKey bool) bool {
	if o == nil {
		return false
	}
	state, hasState := int64(-1), false
	for _, k := range []string{"free_reward", "freeReward", "free"} {
		if n, ok := evNum(o, k); ok {
			state, hasState = n, true
		}
	}
	if !hasState || state != 0 {
		return false
	}
	keys := []string{"boxReward", "box_reward", "free_reward"}
	if rewardKey {
		keys = append(keys, "reward")
	}
	for _, k := range keys {
		if evNonEmpty(o, k) {
			return true
		}
	}
	return false
}

// evFirstInfo is info[0] of the Cooking and Banquet info replies, the only entry the client reads.
func evFirstInfo(info *sfs.SFSObject) *sfs.SFSObject {
	if l := evObjects(info, "info"); len(l) > 0 {
		return l[0]
	}
	return nil
}

// evAidIDParams is the Cooking/Banquet claim {aid: Int, id: Int info[0].id}.
func evAidIDParams(a Activity, info *sfs.SFSObject) (*sfs.SFSObject, bool) {
	id, ok := evNum(evFirstInfo(info), "id")
	if !ok {
		return nil, false
	}
	p := sfs.NewSFSObject()
	p.PutInt("aid", a.ID)
	p.PutInt("id", int32(id))
	return p, true
}

// evNonEmpty reports whether o[key] is a non-empty array, object or string (a reward list in any
// of the shapes ReturnRewardParamForView accepts).
func evNonEmpty(o *sfs.SFSObject, key string) bool {
	v, ok := o.Get(key)
	if !ok {
		return false
	}
	switch c := v.Val.(type) {
	case *sfs.SFSArray:
		return len(c.Items()) > 0
	case *sfs.SFSObject:
		return len(c.Keys()) > 0
	case string:
		return c != "" && c != "0"
	}
	return false
}
