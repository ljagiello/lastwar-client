package game

import (
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// S4 Gold Tree daily prayer (MASTER G19), OPT-IN, static-only.
//
// On an open GoldTree activity (type 256, EnumType.lua:6446; row 1200021, a Darkness-season event)
// the client sends gold.tree.act.view {} (ActivityListDataManager.lua:559-563;
// SeasonGoldTreeManager.lua:47-51) -> goldTreeInfo {finishTime, ...}, userGoldTreeDataInfo
// {weekTime, userGoldTeeInfo {startTime, endTime, ...}, userGoldTreeCardArr [{day, cardId}]}
// (GoldTreeActViewMessage.lua:15; UserGoldTreeDataInfo.lua:15-24; GoldTreeInfo.lua:19-30). Its
// count of open prayers, CanPrayCardCount (SeasonGoldTreeManager.lua:190-210), needs the activity
// open and in season (IsSeasonActivityOpen, SeasonUtil.lua:1416-1441), now before the activity's
// endTime, the tree charged (finishTime > 0 and now >= finishTime, GoldTreeInfo.lua:40-45), the
// player's tree week active (startTime <= now <= endTime, UserGoldTreeDataInfo.lua:37-43), and then
// counts each weekday index d in startDay..endDay that is available (d <= today, today + 7 when
// today < startDay: IsAvailable, :103-108) and has no card (HasPray, :26-35). Each one is a prayer
// the button sends with no cost check (GoldTreePrayItem.lua:60-64): gold.tree.open.card {weekTime:
// Long, day: Int} (OpenCard, :94-101; GoldTreeOpenCardMessage.lua:4-8), so missed days of the week
// are caught up. startDay..endDay is the season config's season_golden_tree row's cycle ("2|6" in
// 40413), 2..6 by default (SeasonGoldTreeManager.lua:14-15, 24-33).
//
// The weekday index is UITimeManager:GetNowWeekdayIndex: the server-time date's weekday by Kim
// Larsen's formula mapped 0..6 -> 1..7, so Monday is 1 and Sunday 7 (UITimeManager.lua:638-646,
// 667-690). The server time is UTC plus changeDeltaTime, read here off the server-day anchor
// (seasonServerOffset).
//
// Never sent: gold.tree.lottery.buy (a purchase) and the third-phase lottery. No error code is
// registered as benign: the first replies are logged and an error stops the prayers.
const (
	goldTreeFeature  = "gold-tree-prayer"
	goldTreeViewCmd  = "gold.tree.act.view"
	goldTreeCardCmd  = "gold.tree.open.card"
	goldTreeActType  = 256 // GoldTree (EnumType.lua:6446)
	goldTreeStartDay = 2
	goldTreeEndDay   = 6
)

func init() {
	registerFeature(Feature{
		Name:    goldTreeFeature,
		Summary: "OPT-IN: in season, take the Gold Tree's free daily prayers, catching up missed days of the week (static-only, not validated live)",
		Run:     runGoldTreePrayer,
	})
}

func runGoldTreePrayer(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil || evSeasonID(in) <= 0 {
		return nil
	}
	open := Activities(conn, in).Open(goldTreeActType)
	if len(open) == 0 {
		return nil
	}
	a := open[0]
	now := evNow()
	dayStart, ok := in.ServerDayStart(now)
	if !ok {
		slog.Info(goldTreeFeature + ": init has no tomorrow, so the server weekday is unknown; skipping")
		return nil
	}
	if end := a.EndTime(); end <= 0 || now.UnixMilli() >= end {
		return nil
	}
	msg, err := session.SendAndWait(conn, goldTreeFeature+": tree view", goldTreeViewCmd, sfs.NewSFSObject())
	if err != nil {
		return err
	}
	tree := evObject(msg.Params, "goldTreeInfo")
	user := evObject(msg.Params, "userGoldTreeDataInfo")
	week := evObject(user, "userGoldTeeInfo")
	finish, _ := evNum(tree, "finishTime")
	weekTime, hasWeekTime := evNum(user, "weekTime")
	weekStart, ok1 := evNum(week, "startTime")
	weekEnd, ok2 := evNum(week, "endTime")
	var prayed []int64
	for _, c := range evObjects(user, "userGoldTreeCardArr") {
		if d, ok := evNum(c, "day"); ok {
			prayed = append(prayed, d)
		}
	}
	first, last := goldTreeCycle(in)
	today := goldTreeWeekday(now, dayStart)
	nowMs := now.UnixMilli()
	charged := finish > 0 && nowMs >= finish
	active := ok1 && ok2 && nowMs >= weekStart && nowMs <= weekEnd
	var due []int32
	if charged && active && hasWeekTime {
		due = goldTreeDays(first, last, today, prayed)
	}
	slog.Info(goldTreeFeature+": tree view", "activity", a.ID, "finishTime", finish, "charged", charged, "weekTime", weekTime,
		"weekStart", weekStart, "weekEnd", weekEnd, "active", active, "cycle", fmt.Sprintf("%d|%d", first, last),
		"today", today, "prayed", prayed, "due", due)
	for _, d := range due {
		p := sfs.NewSFSObject()
		p.PutLong("weekTime", weekTime)
		p.PutInt("day", d)
		if _, err := claimAndLog(conn, fmt.Sprintf("%s: day %d", goldTreeFeature, d), goldTreeCardCmd, p, "cardReward", "combinationRewardArr"); err != nil {
			return err
		}
	}
	return nil
}

// goldTreeCycle is the season's prayer weekdays: the season config's season_golden_tree row's
// cycle "start|end", each part falling back to 2 and 6 (SeasonGoldTreeManager.lua:14-15, 24-33).
func goldTreeCycle(in *Init) (first, last int32) {
	first, last = goldTreeStartDay, goldTreeEndDay
	cfg, _, ok := evSeasonConfig(in)
	if !ok {
		return first, last
	}
	parts := strings.Split(seasonGoldenTreeCycles[cfg.goldenTree], "|")
	if n, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
		first = int32(n)
	}
	if len(parts) > 1 {
		if n, err := strconv.Atoi(strings.TrimSpace(parts[1])); err == nil {
			last = int32(n)
		}
	}
	return first, last
}

// goldTreeWeekday is GetNowWeekdayIndex: the weekday of now's server-time date, Monday 1 to
// Sunday 7.
func goldTreeWeekday(now, dayStart time.Time) int32 {
	wd := now.UTC().Add(seasonServerOffset(dayStart)).Weekday()
	if wd == time.Sunday {
		return 7
	}
	return int32(wd)
}

// goldTreeDays is CanPrayCardCount's list: each day of first..last that IsAvailable on today and
// has no card.
func goldTreeDays(first, last, today int32, prayed []int64) []int32 {
	if today < first {
		today += 7
	}
	var due []int32
	for d := first; d <= last; d++ {
		if d <= today && !slices.Contains(prayed, int64(d)) {
			due = append(due, d)
		}
	}
	return due
}
