package game

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// This file is shared by the claim features (feature_*.go): a decoder that turns a claim
// response into a one-line "granted X" log, and the small field readers and send loop the
// features use. Static-only: the reward shapes below come from the 1.0.364 Lua RewardManager
// (DataCenter/RewardManager/RewardManager.lua:13-198, 808-964, ParseRewardInfo :2177-2199, as
// summarized in coverage/reports/net-protocol.md "Wire reward array"), not from a live capture.

// rewardTypeNames maps RewardType ids (Global/EnumType.lua:143-226) to log names. 5 GOLD is the
// premium currency the UI calls diamonds; 20 FOOD is "money" on the wire.
var rewardTypeNames = map[int32]string{
	0: "oil", 1: "metal", 2: "wood", 5: "diamonds", 6: "exp", 7: "item", 8: "general", 9: "power",
	10: "honor", 11: "alliance_point", 12: "house", 13: "hospital", 14: "equip", 15: "material",
	16: "part", 17: "item_effect", 18: "battle_honor", 19: "water", 20: "food", 21: "electricity",
	22: "people", 23: "troops", 24: "manor", 25: "hero", 26: "ptgold", 27: "resource_item",
	28: "pve_point", 29: "detect_event", 30: "formation_stamina", 31: "wood", 32: "stamina",
	33: "pve_monument_time", 34: "pve_act_score", 35: "museum_artifact", 36: "worker", 37: "visitor",
	38: "common_equip", 39: "decoration", 40: "dragon_world_point", 41: "building", 42: "obsidian",
	43: "flint", 44: "resistance_point", 45: "alliance_battle_reward", 46: "tw_skill_chip",
	47: "petroleum", 48: "tactical_card", 49: "emotion_select", 50: "chat_emotion",
	888: "party_monster", 999: "favor", 1001: "sevenday_score", 1002: "golloes", 1003: "sapphire",
	1004: "alliance_donate", 1005: "alliance_tech_point", 1007: "unlock_module", 1008: "hero_exp",
	1009: "battle_pass", 1010: "talent", 1011: "resource", 1012: "alliance_gift", 1013: "month_card",
	1014: "daily_task_point", 1015: "equip_stone", 1016: "screw_spike", 1017: "fourth_formation",
	1018: "month_card_buff", 1019: "march_def_reduction_buff", 1021: "act_gift_box", 1022: "credit",
	1023: "mastery_point", 1024: "fish", 7038: "tactical_weapon_part", 9001: "arms_medal",
	9002: "paid_diamonds", 11014: "alliance_stone", 11015: "alliance_coal",
	11016: "alliance_farmer_exp", 11017: "alliance_farmer_exp_item", 11018: "meteorite_nucleus",
	11019: "meteorite_crystal",
}

// Object-valued reward types whose value.count is the new total rather than the gain: 7 GOODS
// (RewardManager.lua:808-832; 17 ITEM_EFFECT is displayed as GOODS, :792-794) and 27 RESOURCE_ITEM
// (ResourceItemData.lua:41-58). 38 CommonEquip and 46 TWSkillChip nest their entry one level down
// under changes[] / updates[] (:924-951).
const (
	rewardTypeGoods        int32 = 7
	rewardTypeItemEffect   int32 = 17
	rewardTypeResourceItem int32 = 27
	rewardTypeCommonEquip  int32 = 38
	rewardTypeTWSkillChip  int32 = 46
)

// rewardGrant is one decoded reward[] entry: Gained is the amount added (HasGained false when the
// entry only carried a new total), Total the new balance when the server sent one.
type rewardGrant struct {
	Type      int32
	ItemID    int64
	Gained    int64
	HasGained bool
	Total     int64
	HasTotal  bool
}

// decodeRewardGrants reads the reward arrays under keys (default "reward"). A scalar entry is
// {type, value: amount, total?: balance}; an object entry is {type, value: {itemId|id|...,
// rewardAdd|add|num|count}} per RewardManager's per-type cases. Zero-amount entries are dropped,
// as the client does (:964).
func decodeRewardGrants(p *sfs.SFSObject, keys ...string) []rewardGrant {
	if len(keys) == 0 {
		keys = []string{"reward"}
	}
	var out []rewardGrant
	for _, key := range keys {
		v, ok := p.Get(key)
		if !ok {
			continue
		}
		arr, ok := v.Val.(*sfs.SFSArray)
		if !ok {
			continue
		}
		for _, it := range arr.Items() {
			e, ok := it.Val.(*sfs.SFSObject)
			if !ok {
				continue
			}
			if g, ok := decodeRewardGrant(e); ok {
				out = append(out, g)
			}
		}
	}
	return out
}

func decodeRewardGrant(e *sfs.SFSObject) (rewardGrant, bool) {
	t, ok := claimInt(e, "type")
	if !ok {
		if t, ok = claimInt(e, "rewardType"); !ok {
			return rewardGrant{}, false
		}
	}
	g := rewardGrant{Type: int32(t)}
	g.Total, g.HasTotal = claimInt(e, "total")
	v, ok := e.Get("value")
	if !ok {
		// ParseRewardInfo also accepts itemId/count at the top level.
		g.ItemID, _ = claimInt(e, "itemId")
		g.Gained, g.HasGained = claimInt(e, "count")
		return g, !g.HasGained || g.Gained != 0
	}
	if obj, isObj := v.Val.(*sfs.SFSObject); isObj {
		nestKey := map[int32]string{rewardTypeCommonEquip: "changes", rewardTypeTWSkillChip: "updates"}[g.Type]
		if nested := claimObjects(obj, nestKey); len(nested) > 0 {
			obj = nested[0]
		}
		g.ItemID, _ = claimFirstInt(obj, "itemId", "id", "goodsId", "heroId", "equipId", "workerId", "cfgId")
		countIsTotal := g.Type == rewardTypeGoods || g.Type == rewardTypeItemEffect || g.Type == rewardTypeResourceItem
		if c, ok := claimInt(obj, "count"); ok && countIsTotal {
			g.Total, g.HasTotal = c, true
		}
		if n, ok := claimFirstInt(obj, "rewardAdd", "addNum", "num", "add", "changeNum"); ok {
			g.Gained, g.HasGained = n, true
		} else if c, ok := claimInt(obj, "count"); ok && !countIsTotal {
			g.Gained, g.HasGained = c, true
		}
		return g, !g.HasGained || g.Gained != 0
	}
	g.Gained, g.HasGained = claimInt(e, "value")
	return g, !g.HasGained || g.Gained != 0 || g.HasTotal
}

// String renders one grant for the log line, e.g. "diamonds +50 (now 1200)" or "item 200101 +2".
func (g rewardGrant) String() string {
	name, ok := rewardTypeNames[g.Type]
	if !ok {
		name = fmt.Sprintf("type%d", g.Type)
	}
	var b strings.Builder
	b.WriteString(name)
	if g.ItemID != 0 {
		fmt.Fprintf(&b, " %d", g.ItemID)
	}
	if g.HasGained {
		fmt.Fprintf(&b, " +%d", g.Gained)
	}
	if g.HasTotal {
		fmt.Fprintf(&b, " (now %d)", g.Total)
	}
	return b.String()
}

// describeGrants is the one-line summary of what a claim response granted: the reward entries,
// or, when there are none, the resource{} balances the server sent instead (absolute totals, so
// they show that something changed, not by how much).
func describeGrants(p *sfs.SFSObject, keys ...string) string {
	grants := decodeRewardGrants(p, keys...)
	if len(grants) > 0 {
		parts := make([]string, len(grants))
		for i, g := range grants {
			parts[i] = g.String()
		}
		return strings.Join(parts, ", ")
	}
	if v, ok := p.Get("resource"); ok {
		if res, ok := v.Val.(*sfs.SFSObject); ok && len(res.Keys()) > 0 {
			parts := make([]string, 0, len(res.Keys()))
			for _, k := range res.Keys() {
				if n, ok := claimInt(res, k); ok {
					parts = append(parts, fmt.Sprintf("%s=%d", k, n))
				}
			}
			return "resource balances " + strings.Join(parts, ", ")
		}
	}
	return "nothing"
}

// claimAndLog sends one claim through session.SendAndWait and, when the server answered without
// an errorCode, logs "<label> granted <summary>". rewardKeys names the response arrays holding
// reward entries (default "reward"). A benign errorCode (already claimed) is returned as nil by
// SendAndWait and logged there, not here.
func claimAndLog(conn *session.GameConn, label, cmd string, params *sfs.SFSObject, rewardKeys ...string) (*session.ExtensionMessage, error) {
	msg, err := session.SendAndWait(conn, label, cmd, params)
	if err == nil && msg != nil && !msg.Params.Has("errorCode") {
		slog.Info(label+" granted "+describeGrants(msg.Params, rewardKeys...), "cmd", cmd)
	}
	return msg, err
}

// claimNow is the "now" every claim feature's eligibility gate uses. It is the host clock: Init
// records no receipt time, so no server-time delta is kept yet, and this is the one place to
// apply one. "Not yet claimed today" gates use in.ServerDayStart(claimNow(in)).
func claimNow(*Init) time.Time { return time.Now() }

// claimEach runs fn over items in order, collecting every error and stopping early only when the
// connection is known dead -- the same policy as collectCore.
func claimEach[T any](items []T, fn func(T) error) []error {
	var errs []error
	for _, it := range items {
		err := fn(it)
		if err != nil {
			errs = append(errs, err)
		}
		if session.ContainsNonTimeoutNetError(err) {
			break
		}
	}
	return errs
}

// sfsLongArrayTag is the SFS2X LongArray wire tag (13; sfs.sfsLongArray is unexported). Several
// claim commands take a LongArray of uuids.
const sfsLongArrayTag byte = 13

// claimLongArray builds a LongArray value for o.PutValue.
func claimLongArray(vals []int64) sfs.SFSValue {
	return sfs.SFSValue{Type: sfsLongArrayTag, Val: append([]int64(nil), vals...)}
}

// claimLongSFSArray builds an SFSArray whose items are Longs (the "SFSArray<Long>" shape some
// commands take instead of a LongArray).
func claimLongSFSArray(vals []int64) *sfs.SFSArray {
	a := sfs.NewSFSArray()
	for _, v := range vals {
		a.AddValue(sfs.SFSValue{Type: sfs.SFSLong, Val: v})
	}
	return a
}

// claimInt reads key as an integer, accepting every SFS integer width, a Double holding a whole
// number, and a decimal string (the Lua side calls tonumber on several of these fields). ok is
// false when the key is absent or not numeric.
func claimInt(o *sfs.SFSObject, key string) (int64, bool) {
	v, ok := o.Get(key)
	if !ok {
		return 0, false
	}
	switch n := v.Val.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int16:
		return int64(n), true
	case byte:
		return int64(n), true
	case float64:
		return claimWholeFloat(n)
	case float32:
		return claimWholeFloat(float64(n))
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i, err == nil
	}
	return 0, false
}

func claimWholeFloat(f float64) (int64, bool) {
	if f != math.Trunc(f) || f >= math.MaxInt64 || f < math.MinInt64 {
		return 0, false // also rejects NaN and ±Inf
	}
	return int64(f), true
}

// claimFirstInt returns the first of keys present as an integer.
func claimFirstInt(o *sfs.SFSObject, keys ...string) (int64, bool) {
	for _, k := range keys {
		if n, ok := claimInt(o, k); ok {
			return n, true
		}
	}
	return 0, false
}

// claimString reads key as a string, formatting an integer as decimal (ids the Lua passes to
// tostring before sending).
func claimString(o *sfs.SFSObject, key string) (string, bool) {
	v, ok := o.Get(key)
	if !ok {
		return "", false
	}
	if s, ok := v.Val.(string); ok {
		return s, true
	}
	if n, ok := claimInt(o, key); ok {
		return strconv.FormatInt(n, 10), true
	}
	return "", false
}

// claimBool reads key as a bool, accepting a Bool or an integer 0/1.
func claimBool(o *sfs.SFSObject, key string) (bool, bool) {
	v, ok := o.Get(key)
	if !ok {
		return false, false
	}
	if b, ok := v.Val.(bool); ok {
		return b, true
	}
	if n, ok := claimInt(o, key); ok {
		return n != 0, true
	}
	return false, false
}

// claimObjects returns the object items of the array under key in o, skipping anything else.
func claimObjects(o *sfs.SFSObject, key string) []*sfs.SFSObject {
	v, ok := o.Get(key)
	if !ok {
		return nil
	}
	a, ok := v.Val.(*sfs.SFSArray)
	if !ok || a == nil {
		return nil
	}
	var out []*sfs.SFSObject
	for _, it := range a.Items() {
		if e, ok := it.Val.(*sfs.SFSObject); ok {
			out = append(out, e)
		}
	}
	return out
}
