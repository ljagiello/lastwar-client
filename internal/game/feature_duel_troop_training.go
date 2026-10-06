package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel troop training (DUEL.md §4.2 "Starts", §7.2-7.3). OPT-IN: it spends food and iron.
// It trains only when today's theme is Total Mobilization (110004, Fri) or Enemy Buster (110005,
// Sat) AND today scores training (type 4) or training speed-ups (type 51, value 4). The scheduler
// releases the feature on any day that scores either type, so Run checks the theme again.
//
// Every idle Military Camp (init building_new bId 10103000) gets what the camp panel's Train
// button sends (UILWMilitaryCampPanelView.lua:863-872; BuildingCampTrainingMessage.lua:4-9):
// `building.camp.training {uuid: Long, type: 0, sLevel: Int, sNum: Int}`. The instant-finish
// fields itemId, goldForTime and goldForResource (:100, :124) are never sent, and a reply that
// echoes one fails the feature.
//   - Idle: lv > 0, no prodST and no prodET. That is the client's "train" bubble state
//     (BuildBubbleManager.lua:2372-2373; IsBuildingFunctioning, BuildingUtils.lua:1813-1818). A camp
//     that is training, or that has finished and waits for its output to be collected, is left
//     alone. finished-timers collects that output. A camp with an upgrade timer (uT) is skipped too.
//   - Tier: the panel preselects the highest unlocked player unit (maxId, :651-670;
//     UILWMilitaryCampPanelCtrl.lua:6-125). A unit is unlocked when the camp level >=
//     train_need_barrack and the science train_need_science is researched (T10 and T11: init
//     science_new itemId 120011100 at level > 0, ScienceManager.lua:152-155). T11 also needs
//     effect train_need_buff (90031 UNLOCK_T11) > 0, and the camp shows it only from season 4 day
//     64 on (soldier_eleven_param k1 "4|64", T11Util.lua:311-333). T11 is trained here only past
//     season 4, because the season day isn't read. sLevel is the tier, soldier_lv 1-11
//     (GetSoldierLevelById, SoldierDataManager.lua:208-214; soldier_lv, SoldierDataTemplate.lua:57).
//   - Count: the slider maximum is floor((para1 + worker TrainLimitNum) * (1 + rate)), where rate
//     is the worker TrainLimitRate plus effect 50105 (:628-648). Here it is floor(para1 * (1 + init
//     effect 50105)). Init effect is a lower bound of GetGameEffect, and worker bonuses only add,
//     so this count never exceeds the client's.
//   - Cost: per unit, train_cost_consume iron ("1") and food ("14") times (1 - reduction), where
//     the reduction is the worker TrainCostRate plus effect 50106, and the totals are rounded up
//     (:197-227, :246-250). Here only init effect 50106, floored to 4 decimals, is taken off, so
//     the cost never falls below the client's. sNum is the largest count whose totals the food
//     and iron cover (the GotoResLack guard, :842-862). Food and iron are init resource.money and
//     resource.metal (ResourceInfo.lua:163-174). Their ledger goes down by each camp's cost and
//     by any lower balance pushed, so later camps see what earlier ones spent.
//   - Every spend goes through the duel spend guard (feature_duel_recruit_tickets.go): the diamond
//     tripwire and a gold check after each batch.
//
// The tables are 1.0.364 lw_soldier 3005-3015 and building 10103001-10103035 para1. building_B
// has the same para1, and both tables are unchanged in live 39516 (coverage/reports/tables.md).
// Static-only: not sent live yet.
const (
	duelTrainCmd        = "building.camp.training"
	duelTrainScience    = "120011100" // science_id of train_need_science 120011101 (APS_science)
	duelTrainT11Buff    = "90031"     // UNLOCK_T11, lw_soldier#3015 train_need_buff
	duelTrainLimitRate  = "50105"     // WorkerEffectDefine.TrainLimitRate
	duelTrainCostRate   = "50106"     // WorkerEffectDefine.TrainCostRate
	duelTrainT11Season  = 4           // soldier_eleven_param k1 "4|64"
	duelTrainResIron    = "metal"
	duelTrainResFood    = "money"
	duelTrainBasisPoint = 10000
)

func init() {
	registerFeature(Feature{
		Name:    "duel-troop-training",
		Summary: "OPT-IN: on Alliance Duel Total Mobilization (Fri) / Enemy Buster (Sat), train the highest tier at the largest affordable count in every idle camp (food/iron only, diamond tripwire)",
		Duel:    []DuelScore{{Type: DuelScoreTrainUnit}, {Type: DuelScoreSpeedUp, Value: DuelQueueTraining}},
		Hold:    true,
		Run:     runDuelTroopTraining,
	})
}

// duelUnit is one lw_soldier player unit (type 1).
type duelUnit struct {
	id      int32 // lw_soldier id, also the type-4 score value
	tier    int32 // soldier_lv, sent as sLevel
	camp    int64 // train_need_barrack
	science bool  // train_need_science 120011101
	t11     bool  // train_need_buff 90031 and the T11 season gate
	iron    int64 // train_cost_consume["1"]
	food    int64 // train_cost_consume["14"]
}

var duelUnits = []duelUnit{
	{3005, 1, 1, false, false, 97, 97},
	{3006, 2, 3, false, false, 145, 145},
	{3007, 3, 6, false, false, 349, 349},
	{3008, 4, 10, false, false, 903, 903},
	{3009, 5, 14, false, false, 1746, 1746},
	{3010, 6, 17, false, false, 2464, 2464},
	{3011, 7, 20, false, false, 3320, 3320},
	{3012, 8, 24, false, false, 4535, 4535},
	{3013, 9, 27, false, false, 5770, 5770},
	{3014, 10, 30, true, false, 7027, 7027},
	{3015, 11, 35, true, true, 8829, 8829},
}

// duelCampCapacity is building#10103000+lv para1, the base units per batch, by camp level 0-35.
var duelCampCapacity = []int64{
	0, 30, 50, 100, 150, 200, 250, 300, 350, 360, 370,
	380, 390, 400, 410, 420, 430, 440, 450, 460, 470,
	480, 490, 500, 510, 520, 530, 540, 550, 560, 570,
	575, 580, 585, 590, 600,
}

// duelCamp is one idle Military Camp.
type duelCamp struct {
	uuid int64
	lv   int64
}

// duelIdleCamps returns the idle Military Camps by uuid, logging the busy ones.
func duelIdleCamps(in *Init) []duelCamp {
	objs := in.Objects("building_new")
	if len(objs) == 0 {
		for _, b := range in.Buildings {
			objs = append(objs, b.Raw)
		}
	}
	var out []duelCamp
	for _, b := range objs {
		if id, _ := evNum(b, "bId"); id != finishedTimersMilitaryCamp {
			continue
		}
		uuid, ok := evNum(b, "uuid")
		lv, _ := evNum(b, "lv")
		if !ok || uuid == 0 || lv <= 0 {
			continue
		}
		st, _ := evNum(b, "prodST")
		et, _ := evNum(b, "prodET")
		ut, _ := evNum(b, "uT")
		if st > 0 || et > 0 || ut > 0 {
			slog.Info("duel-troop-training: camp is busy; left alone", "uuid", uuid, "prodST", st, "prodET", et, "uT", ut)
			continue
		}
		out = append(out, duelCamp{uuid: uuid, lv: lv})
	}
	slices.SortFunc(out, func(a, b duelCamp) int { return cmp.Compare(a.uuid, b.uuid) })
	return out
}

// duelTrainUnlocks is what the unit unlocks depend on besides the camp level.
type duelTrainUnlocks struct {
	science bool  // science 120011100 researched
	t11Buff bool  // init effect 90031 > 0
	season  int64 // init playerServerSeasonInfo.seasonId
}

func duelTrainUnlocksOf(in *Init) duelTrainUnlocks {
	var u duelTrainUnlocks
	for _, s := range claimObjectsOrMap(in.Raw, "science_new") {
		if lv, _ := evNum(s, "level"); evString(s, "itemId") == duelTrainScience && lv > 0 {
			u.science = true
		}
	}
	if f, ok := duelFloat(in.Object("effect"), duelTrainT11Buff); ok && f > 0 {
		u.t11Buff = true
	}
	u.season, _ = evNum(in.Object("playerServerSeasonInfo"), "seasonId")
	return u
}

// duelTopUnit returns the highest unit a camp of level lv can train.
func duelTopUnit(lv int64, u duelTrainUnlocks) (duelUnit, bool) {
	for i := len(duelUnits) - 1; i >= 0; i-- {
		unit := duelUnits[i]
		switch {
		case lv < unit.camp:
		case unit.science && !u.science:
		case unit.t11 && (!u.t11Buff || u.season <= duelTrainT11Season):
		default:
			return unit, true
		}
	}
	return duelUnit{}, false
}

// duelBasisPoints is f in units of 1/10000, rounded down (the client rounds the rates to 4
// decimals, Mathf.RoundTo(x, 4)).
func duelBasisPoints(f float64) int64 {
	return int64(math.Floor(f * duelTrainBasisPoint))
}

// duelTrainCost is ceil(n * perUnit * costBP / 10000), the total the client charges for n units.
func duelTrainCost(n, perUnit, costBP int64) int64 {
	return (n*perUnit*costBP + duelTrainBasisPoint - 1) / duelTrainBasisPoint
}

// duelTrainAffordable is the largest n whose duelTrainCost is at most have.
func duelTrainAffordable(have, perUnit, costBP int64) int64 {
	if have <= 0 || perUnit <= 0 || costBP <= 0 {
		return 0
	}
	return have * duelTrainBasisPoint / (perUnit * costBP)
}

func runDuelTroopTraining(conn *session.GameConn, in *Init) error {
	const feature = "duel-troop-training"
	if in == nil || in.Raw == nil {
		slog.Info(feature + ": no init push; skipping")
		return nil
	}
	if err := duelSpendHaltedErr(feature); err != nil {
		return err
	}
	d := TodayDuel(conn, in)
	if d == nil || (d.Theme != DuelThemeMobilization && d.Theme != DuelThemeEnemyBuster) {
		slog.Info(feature+": trains only on Total Mobilization (Fri) and Enemy Buster (Sat); held", "theme", d.ThemeName())
		return nil
	}
	if !d.Scores(DuelScoreTrainUnit, "") && !d.Scores(DuelScoreSpeedUp, DuelQueueTraining) {
		slog.Info(feature+": today's duel scores neither training nor training speed-ups; held", "theme", d.ThemeName())
		return nil
	}
	res := in.Object("resource")
	iron, okIron := evNum(res, duelTrainResIron)
	food, okFood := evNum(res, duelTrainResFood)
	if !okIron || !okFood {
		slog.Info(feature + ": init has no resource.metal / resource.money, so affordability can't be computed; not training")
		return nil
	}
	camps := duelIdleCamps(in)
	if len(camps) == 0 {
		slog.Info(feature + ": no idle Military Camp")
		return nil
	}
	unlocks := duelTrainUnlocksOf(in)
	limitRate, _ := duelFloat(in.Object("effect"), duelTrainLimitRate)
	reduceBP := int64(0)
	if f, ok := duelFloat(in.Object("effect"), duelTrainCostRate); ok {
		reduceBP = duelBasisPoints(f)
	}
	if reduceBP > duelTrainBasisPoint*9/10 {
		reduceBP = 0 // implausible; the full cost is the safe side
	}
	costBP := duelTrainBasisPoint - reduceBP

	g, err := newDuelSpendGuard(conn, in, feature)
	if g == nil {
		return err
	}
	g.res[duelTrainResIron], g.res[duelTrainResFood] = iron, food

	var errs []error
	for _, c := range camps {
		if !evNow().Before(d.End) {
			slog.Info(feature+": the duel day ended; stopping", "dayEnd", d.End)
			break
		}
		if c.lv >= int64(len(duelCampCapacity)) {
			slog.Info(feature+": camp level beyond the 1.0.364 table; skipped", "uuid", c.uuid, "lv", c.lv)
			continue
		}
		unit, ok := duelTopUnit(c.lv, unlocks)
		if !ok {
			continue
		}
		maxCount := max(0, duelCampCapacity[c.lv]*(duelTrainBasisPoint+duelBasisPoints(limitRate))/duelTrainBasisPoint)
		n := min(maxCount, duelTrainAffordable(g.res[duelTrainResIron], unit.iron, costBP),
			duelTrainAffordable(g.res[duelTrainResFood], unit.food, costBP), math.MaxInt32)
		costIron, costFood := duelTrainCost(n, unit.iron, costBP), duelTrainCost(n, unit.food, costBP)
		if n <= 0 || costIron > g.res[duelTrainResIron] || costFood > g.res[duelTrainResFood] {
			slog.Info(feature+": can't afford one unit of the camp's top tier; camp left idle", "uuid", c.uuid,
				"tier", unit.tier, "iron", g.res[duelTrainResIron], "food", g.res[duelTrainResFood],
				"ironPerUnit", unit.iron, "foodPerUnit", unit.food)
			continue
		}
		p := sfs.NewSFSObject()
		p.PutLong("uuid", c.uuid)
		p.PutInt("type", 0)
		p.PutInt("sLevel", unit.tier)
		p.PutInt("sNum", int32(n))
		g.res[duelTrainResIron] -= costIron
		g.res[duelTrainResFood] -= costFood
		label := fmt.Sprintf("duel train %d T%d in camp %d", n, unit.tier, c.uuid)
		msg, err := g.spend(label, duelTrainCmd, p)
		if err == nil {
			for _, k := range []string{"itemId", "goldForTime", "goldForResource"} {
				if msg.Params.Has(k) {
					err = fmt.Errorf("%s: the reply carries %s, the instant-finish path; stopping", label, k)
					slog.Error(err.Error())
					break
				}
			}
		}
		if err != nil {
			errs = append(errs, err)
			break
		}
		slog.Info(feature+": training started", "uuid", c.uuid, "tier", unit.tier, "units", n,
			"maxCount", maxCount, "iron", costIron, "food", costFood,
			"expectedBasePoints", n*duelPoints(d, DuelScoreTrainUnit, strconv.Itoa(int(unit.id))))
		if err := g.check(); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}
