package game

import (
	"errors"
	"log/slog"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Count-battle and Civilization Spark soldiers (MASTER G6, cov-dc-social-city/F2), OPT-IN. Two city
// buildings hold soldiers the player won, shown as a bubble whose tap sends a request with no
// params (BuildBubbleManager.lua:4287-4290; the only send sites in the 1.0.370 Lua); the reply
// carries soldierItemId and soldierNum (LWClaimStageSoldierMessage.lua:9-28,
// CivilizationSpark/LWClaimSparkSoldierMessage.lua:8-26):
//
//   - `lw.claim.stage.soldier {}`: the count-battle building LW_BUILD_COUNT_BATTLE (10213000,
//     Global/EnumType.lua:3686) with lv > 0 and prodBase > 0 (BuildBubbleManager.lua:650-656,
//     2686-2687; prodBase is building_new[].prodBase, BuildingDate.lua:176-180). Soldiers appear
//     there only after the player plays count-battle stages.
//   - `lw.claim.spark.soldier {}`: the Civilization Spark LW_CIVILIZATION_SPARK (10230000,
//     EnumType.lua:3800) with prodBase > 0 and a Drill Ground (LW_BUILD_ARMY_YARD 10104000,
//     EnumType.lua:3656) of level 1 or more, unless the spark can be upgraded, when the client
//     shows the upgrade bubble instead (:657-668, 2697-2698). CheckCanUpgrade
//     (LWCivilizationSparkManager.lua:165-178) is true when the spark level (init sparkUpgrade,
//     :34-42) is 1 or more and the player holds its lw_civilization_spark.upgrade_need item; that
//     is mirrored with init items, so a spark that may be upgradable is left alone.
//
// Troop stock: the client checks nothing here, but the Military Camp path does (finished-timers),
// so these claims use the same lower-bound stock gate, counting first the output of every finished
// Military Camp as already taken (finished-timers runs earlier in the run and may have collected
// it). Whether the server applies the cap, or refuses the spark claim while an upgrade is
// available, is unknown. No benign error code is registered. Static-only, not validated live.
const (
	citySoldiersFeature  = "city-soldiers"
	citySoldiersStageCmd = "lw.claim.stage.soldier"
	citySoldiersSparkCmd = "lw.claim.spark.soldier"

	citySoldiersCountBattle = 10213000
	citySoldiersSpark       = 10230000
	citySoldiersArmyYard    = 10104000
)

// citySoldiersSparkUpgradeItem is lw_civilization_spark.upgrade_need by spark level (tables 40413,
// 6 rows): level n (1-5) needs one item 666700+n to reach n+1; level 6 has none.
var citySoldiersSparkUpgradeItem = map[int64]int64{1: 666701, 2: 666702, 3: 666703, 4: 666704, 5: 666705}

func init() {
	registerFeature(Feature{
		Name:      citySoldiersFeature,
		Summary:   "OPT-IN: claim the soldiers waiting at the count-battle building and the Civilization Spark (lw.claim.stage.soldier, lw.claim.spark.soldier) (static-only, not validated live)",
		DefaultOn: false,
		Run:       runCitySoldiers,
	})
}

// citySoldiersClaim is one claim the run decided to send.
type citySoldiersClaim struct {
	cmd, label string
	prodBase   int64
}

// citySoldiersPlan returns the claims due, in send order, after the bubble and stock gates.
func citySoldiersPlan(in *Init, nowMs int64) []citySoldiersClaim {
	var stage, spark *sfs.SFSObject
	var yard int64
	soldiers, stockCap, stockKnown := finishedTimersStockRoom(in)
	for _, b := range in.Objects("building_new") {
		id, _ := claimInt(b, "bId")
		lv, _ := claimInt(b, "lv")
		switch id {
		case citySoldiersCountBattle:
			if stage == nil {
				stage = b
			}
		case citySoldiersSpark:
			if spark == nil {
				spark = b
			}
		case citySoldiersArmyYard:
			yard = max(yard, lv)
		case finishedTimersMilitaryCamp:
			if finishedTimersCampDone(b, nowMs) {
				n, _ := claimInt(b, "prodBase")
				soldiers += max(n, 0)
			}
		}
	}

	var due []citySoldiersClaim
	if stage != nil {
		lv, _ := claimInt(stage, "lv")
		if n, _ := claimInt(stage, "prodBase"); lv > 0 && n > 0 {
			due = append(due, citySoldiersClaim{citySoldiersStageCmd, "count-battle soldiers", n})
		}
	}
	if spark != nil {
		n, _ := claimInt(spark, "prodBase")
		level, item, upgradable := citySoldiersSparkUpgradable(in)
		switch {
		case n <= 0:
		case yard < 1:
			slog.Info(citySoldiersFeature+": spark soldiers wait for a Drill Ground", "prodBase", n)
		case upgradable:
			slog.Info(citySoldiersFeature+": the spark can be upgraded, so the client shows the upgrade bubble, not the soldiers; leaving them",
				"prodBase", n, "sparkUpgrade", level, "upgradeItem", item)
		default:
			due = append(due, citySoldiersClaim{citySoldiersSparkCmd, "Civilization Spark soldiers", n})
		}
	}

	var out []citySoldiersClaim
	for _, c := range due {
		if !stockKnown || soldiers >= stockCap || c.prodBase > stockCap {
			slog.Info(citySoldiersFeature+": "+c.label+" left (troop stock full or unknown)", "prodBase", c.prodBase,
				"soldiers", soldiers, "stockCap", stockCap, "known", stockKnown)
			continue
		}
		soldiers += c.prodBase
		out = append(out, c)
	}
	return out
}

// citySoldiersSparkUpgradable mirrors LWCivilizationSparkManager:CheckCanUpgrade from init: the
// spark level (sparkUpgrade, 0 when absent) has an upgrade item and init items hold at least one.
func citySoldiersSparkUpgradable(in *Init) (level, item int64, upgradable bool) {
	level, _ = claimInt(in.Raw, "sparkUpgrade")
	item, ok := citySoldiersSparkUpgradeItem[level]
	if !ok {
		return level, 0, false
	}
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		// The stack's id and count as ItemInfo:UpdateInfo sets them (duelItemFields).
		id, n, ok := duelItemFields(it)
		if id != strconv.FormatInt(item, 10) {
			continue
		}
		if ok && n >= 1 {
			return level, item, true
		}
	}
	return level, item, false
}

func runCitySoldiers(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info(citySoldiersFeature + ": no init push; skipping")
		return nil
	}
	due := citySoldiersPlan(in, claimNow(in).UnixMilli())
	if len(due) == 0 {
		slog.Info(citySoldiersFeature + ": no soldiers to claim")
		return nil
	}
	return errors.Join(claimEach(due, func(c citySoldiersClaim) error {
		msg, err := session.SendAndWait(conn, citySoldiersFeature+": "+c.label, c.cmd, sfs.NewSFSObject())
		if err == nil && msg != nil && !msg.Params.Has("errorCode") {
			item, _ := claimString(msg.Params, "soldierItemId")
			num, _ := claimInt(msg.Params, "soldierNum")
			slog.Info(citySoldiersFeature+": claimed "+c.label, "soldierItemId", item, "soldierNum", num, "prodBase", c.prodBase)
		}
		return err
	})...)
}
