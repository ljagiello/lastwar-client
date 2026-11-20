package game

import (
	"fmt"
	"log/slog"
	"strings"

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
	due func(in *Init, a Activity, info *sfs.SFSObject) bool
	// params builds the claim; nil means {activityId: Int}.
	params func(a Activity, info *sfs.SFSObject) (*sfs.SFSObject, bool)
}

// eventFreebies, in type order. Static-only; units per type from the data classes cited.
//
// Not claimed: Bounty Hunter (350, bounty.hunter.receive.free.reward). Its pack exists only when
// table activity_hunter.box_reward > 0 (BountyHunterActData.lua:210-213), and every 1.0.364 row is
// 0, so the real client never shows it. The other boxes these panels offer are opt-in features
// that read the same replies: event-milestone-boxes (Lucky Roll stages, Blue Store and slot
// machine boxes), gift-box-free-draw and gift-box-score-boxes, food-event-boxes (Cooking and
// Banquet) and decoration-gacha-boxes.
var eventFreebies = []eventFreebie{
	// LuckyRoll: get.lucky.roll.info {activityId: Int}; rollInfo.lastReceiveFreeTime in ms; the pack
	// is Lucky_roulette_reward (ActLuckyRollInfo.lua:24-33, 89, 182-190). The field is copied only
	// from a present rollInfo, with no default, and the only sender divides it first
	// (UILuckyRollShopItem.lua:42-47), so without it the client cannot claim: evLastFreeDue.
	{typ: actTypeLuckyRoll, claim: "roll.receive.free.reward",
		info: evIntInfo("lucky roll info response", "get.lucky.roll.info", "activityId"),
		due: func(in *Init, a Activity, info *sfs.SFSObject) bool {
			return evNonEmpty(info, "Lucky_roulette_reward") && evLastFreeDue(in, a, evObject(info, "rollInfo"), "rollInfo.lastReceiveFreeTime")
		}},
	// GiftBox: get.activity.gift.box.info {activityId: Int}; ActivityFreeRewardData state flag
	// (ActGiftBoxInfo.lua:102), which ignores `reward` for this type.
	{typ: actTypeGiftBox, claim: "gift.box.free.reward",
		info: evIntInfo("gift box info response", "get.activity.gift.box.info", "activityId"),
		due:  func(_ *Init, _ Activity, info *sfs.SFSObject) bool { return evFreeFlagDue(info, false) }},
	// LuckyShop: hero.event.info.get (activityType 134) -> LuckyShopData:ParseData -> the same flag
	// (LuckyShopManager.lua:38-60).
	{typ: actTypeLuckyShop, claim: "discount.free.reward",
		due: func(_ *Init, _ Activity, info *sfs.SFSObject) bool { return evFreeFlagDue(info, true) }},
	// Cooking: activity.mak.food.info {aid: Int}; the client reads only info[0], whose id is the
	// claim's id (ActivityMakeFoodInfoMessage.lua:14; ActCookingData.lua:24-45).
	{typ: actTypeCooking, claim: "activity.mak.food.free.reward",
		info: evIntInfo("cooking info response", "activity.mak.food.info", "aid"),
		due: func(_ *Init, _ Activity, info *sfs.SFSObject) bool {
			return evFreeFlagDue(evFirstInfo(info), true)
		},
		params: evAidIDParams},
	// Banquet: activity.food.party.info {aid: Int}; same shape (ActivityFoodPartyInfoMessage.lua:14;
	// ActBanquetData.lua:45-70).
	{typ: actTypeBanquet, claim: "activity.food.party.reward.free",
		info: evIntInfo("banquet info response", "activity.food.party.info", "aid"),
		due: func(_ *Init, _ Activity, info *sfs.SFSObject) bool {
			return evFreeFlagDue(evFirstInfo(info), true)
		},
		params: evAidIDParams},
	// Monopoly: top-level lastReceiveFreeTime in seconds, pack dayReward (ActMonopolyData.lua:173-178, 272-278).
	{typ: actTypeMonopoly, claim: "rich.man.day.reward", due: evDayRewardDue},
	// Bargain Shop: same fields, seconds (ActBargainShopInfo.lua:64-72, 117-140). Unlike the
	// others its data class has no default: a reply without lastReceiveFreeTime aborts the parse
	// before the pack row is built (:138; ActBargainShopData.lua:26-40), so the pack is due only
	// with the field present (evLastFreeDue).
	{typ: actTypeBargainShop, claim: "bargain.day.reward",
		due: func(in *Init, a Activity, info *sfs.SFSObject) bool {
			return evNonEmpty(info, "dayReward") && evLastFreeDue(in, a, info, "lastReceiveFreeTime")
		}},
	// Titanium Blue Store: same fields, seconds (LWTitaniumBlueStoreManager.lua:72-79, 170).
	{typ: actTypeBlueStore, claim: "blue.shop.day.reward", due: evDayRewardDue},
	// Slot machine: same field name but ms (ActSlotMachineData.lua:78-82, 124-130).
	{typ: actTypeSlotMachine, claim: "slots.daily.reward", due: evDayRewardDue},
	// Decoration gacha: freeReward present and (nextResetTime, seconds, has passed or freeState is
	// 0) (ActivityDecorationGachaData.lua:18-29).
	{typ: actTypeDecorationGacha, claim: "decoration.free.reward",
		due: func(_ *Init, _ Activity, info *sfs.SFSObject) bool {
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
		due: func(in *Init, _ Activity, info *sfs.SFSObject) bool {
			return evNotToday(in, info, "lastReceiveFreeTime")
		}},
	// Survival VIP gift: survival.vip.gift.get.info {activityId: Int}; claimable while isShow is not
	// hidden and freeReceived is not true (VipGiftActDataManager.lua:444-455, 592-608). OnGetInfo
	// keeps a Bool as it is and reads anything else through string.lower(tostring(isShow)), hidden
	// when "0" or "false" (:610-616), so an integer 0 hides it too.
	{typ: actTypeSurvivalVipGift, claim: "survival.vip.gift.receive.free",
		info: evIntInfo("survival vip gift info response", "survival.vip.gift.get.info", "activityId"),
		due: func(_ *Init, _ Activity, info *sfs.SFSObject) bool {
			if v, ok := info.Get("isShow"); ok {
				switch s := v.Val.(type) {
				case bool:
					if !s {
						return false
					}
				case string:
					if l := strings.ToLower(s); l == "0" || l == "false" {
						return false
					}
				case int64, int32, int16, byte:
					if n, _ := evNum(info, "isShow"); n == 0 {
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
			// The Torch Relay free pack needs the activity's activity_torch_relay row with
			// free_iap_reward > 0 (CanClaimFreePackage, ActivityTorchRelayData.lua:220-224); every row
			// of tables 40413 has one, so an activity whose row is unknown is skipped, as
			// claimTorchRelay skips it (1.0.370 audit B61).
			if f.typ == actTypeTorchRelay && !torchRelayKnown(a) {
				slog.Info("event-shop-freebies: Torch Relay activity has no known activity_torch_relay row; skipped",
					"activity", a.ID, "tableVersion", activityTableVersion)
				continue
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
			if !f.due(in, a, info) {
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

// evDayRewardDue is the Monopoly/Blue Store/Slots rule: dayReward non-empty and
// lastReceiveFreeTime not today, absent meaning never claimed (each data class defaults the field to
// 0: ActMonopolyData.lua:19, ActSlotMachineData.lua:11, LWTitaniumBlueStoreManager.lua:17).
func evDayRewardDue(in *Init, _ Activity, info *sfs.SFSObject) bool {
	return evNonEmpty(info, "dayReward") && evNotToday(in, info, "lastReceiveFreeTime")
}

// evLastFreeDue is evNotToday for the Lucky Roll and Bargain Shop packs, whose client cannot claim
// without the last-claim time: an absent field (or, for Lucky Roll, an absent rollInfo) is not due
// and logs a Warn naming the activity and the field, so a live run shows whether a reply ever
// lacks it. A present 0 is never claimed and stays due, as on the client.
func evLastFreeDue(in *Init, a Activity, o *sfs.SFSObject, field string) bool {
	key := field[strings.LastIndex(field, ".")+1:]
	if _, ok := evNum(o, key); !ok {
		slog.Warn("event-shop-freebies: the detail reply has no last free claim time; the client cannot claim the pack then, so it is skipped",
			"activity", a.ID, "type", a.Type, "missing", field)
		return false
	}
	return evNotToday(in, o, key)
}

// evFreeFlagDue is ActivityFreeRewardData's rule (ActivityFreeRewardData.lua:3-10, 21-84), used by
// Gift Box, Lucky Shop, Cooking and Banquet. The server sends no claim time, only a state flag. The
// state starts at 0, claimable (:5), and ParseData overwrites it with free_reward, then freeReward,
// then free, each when present (the last present wins; 0 claimable, 1 claimed, :36-51), so a reply
// with none of the three keys stays claimable. The pack's reward list comes from boxReward, reward
// (not for Gift Box), box_reward or free_reward. free_reward can be the list instead of the flag;
// when the last present key is not a number the state is not 0 and CanGetFreePack (:21-25) says
// no, so that case stays unclaimed.
func evFreeFlagDue(o *sfs.SFSObject, rewardKey bool) bool {
	if o == nil {
		return false
	}
	state, isNum := int64(0), true
	for _, k := range []string{"free_reward", "freeReward", "free"} {
		if v, ok := o.Get(k); !ok || v.Val == nil || v.Val == false {
			continue // `if param.k then` skips nil and false
		}
		state, isNum = evNum(o, k)
	}
	if !isNum || state != 0 {
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
