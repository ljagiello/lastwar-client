package game

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Finished-timer collection (MASTER.md §4 #12): the free completion of work whose timer has already
// run out. None of these is an instant finish: each request carries only the uuid (or nothing),
// and the client sends it only once the timer has ended (CheckSendBuildFinish, BuildManager.lua:
// 3045-3118; ScienceManager.lua:131-151). The paid paths are the separate `build.ccd.*` /
// `queue.ccd.*` speed-ups and `free.building.up.new {gold}`, never sent here. The client's free
// research finish (queue.ccd.m.new with an empty itemIDs inside effect 30100's window,
// QueueDataManager.lua:572-590) is not sent either: the same bytes are its diamond instant finish
// (ArmyManager.lua:204-210), and on a 3-hour cron it would complete nothing earlier, since the
// window is at most 3 h and the research is completed here by queue.finish on the next run anyway.
//
//   - Upgrades: `free.building.upgrade.finish {uuid: Long}` for each init building_new entry with
//     uT > 0 && now >= uT (ms; BuildingDate.lua:453-456; FreeBuildingUpgradeFinishMessage.lua:3-9;
//     CheckSendBuildFinish, BuildManager.lua:3045-3118), where uT is the one the client uses: the
//     occupying build queue's uT when init has one (buildingUpgradeTime, building_times.go).
//     Skipped: the wormhole sub-building (792000) at level 0 (:3060-3062); HQ level 7 while
//     defend_wall.protectEndTime (ms) >= now, because finishing HQ 8 drops the newbie shield and
//     the client asks first (:3070-3087), and HQ 7 is also skipped when init has no defend_wall;
//     and every upgrade while the player's "confirm finished building" setting is on
//     (finishedTimersConfirmOn, :3089-3113).
//   - Repairs: `user.finish.fix.building {uuid: Long}` when dEndT > 0 && now >= dEndT (ms;
//     BuildingDate.lua:462-465; UserFinishFixBuildingMessage.lua:3-6).
//   - Queues: `queue.finish {uuid: Long}` (QueueFinishMessage.lua:3-8) for each init queue_new
//     entry in state Finish -- (sT != 0 && sT == uT) || (uT != 0 && now >= uT), with startTime /
//     updateTime overriding sT / uT (QueueInfo.lua:34-124) -- of type 6 research
//     (ScienceManager.lua:120-139), 3 hospital (HospitalManager.lua:355-372) or 117 rebirth
//     hospital (RebirthHospitalManager.lua:347-364). Other types are not sent: pastures use
//     queue.finish.batch, the dragon hospital is battlefield-only and auto-sent, T11 breaks are
//     optional.
//   - Camp output: `building.camp.collect {uuid: Long}` (BuildingCampCollectMessage.lua:3-6) for the
//     Military Camp (10103000), Smith Shop (10101000) and Tactical Chip Factory (10232000) with
//     lv > 0 && prodST > 0 && now >= prodET (BuildingUtils.lua:1813-1852; BuildBubbleManager.lua:
//     587-598, 2289-2321). These three are the only building types the command accepts (Go saw
//     E000001 "Building type error" for production lines). For the chip factory the city bubble
//     sends building.camp.collect, whose reply handler has a chip branch (:45-61); the factory panel
//     sends `chip.collect {}` instead (TacticalChipFactoryCreatePage.lua:576-578), so that is the
//     fallback when the factory answers E000001.
//
// Troop stock cap: the client refuses to take healed troops, or Military Camp output, when the
// player's soldier total is at LW_SOLDIER_MAX_STOCK (effect 30122), and camp output larger than the
// cap (HospitalManager.lua:357-362; BuildingUtils.lua:1961-1978, tip 120083). The total is the
// resource items 3005-3015 (table lw_soldier, type 1) from init resource_items
// (SoldierDataManager.lua:75-162). The cap is GetGameEffect(30122), a sum of eight sources of which
// init effect carries only the base, so init effect["30122"] is a lower bound: checking against it
// can only skip a collect the client would allow, never the reverse. Without effect["30122"] or
// resource_items (or with an effect value that is not a number), hospital, rebirth and camp collects
// are skipped. Each planned camp collect adds its prodBase to the total before the next check: the
// server fills the stock part-way from one camp (its reply keeps prodST) and answers the next with
// E000000 "addNum error" (addNum:0), seen live on 2026-10-18 and 2026-10-24 when the room was
// checked once for every camp. The hospital checks are static-only: not sent live yet.
//
// Alliance Duel hold (MASTER §6.1 #4, the user's decision of 2026-11-19; aud-city/V1). A finished
// upgrade or research earns its duel points when it is completed, which is the request above, not
// when its timer ends. The upgrade's reply carries the building at its new level, which the client
// applies (FreeBuildingUpgradeFinishHandle, BuildManager.lua:2906-2915). Until then init shows it
// with uT passed (BuildingDate:IsUpgradeFinish). And the duel's own "go to" for building power
// (score 90202, type 23, gotype2 504 = QuestGoType.GotoBuildingPowerUp, EnumType.lua:5471) points
// the player at the highest such finished, uncollected upgrade (GoToUtil.lua:1289-1290, 4343-4344,
// 4440-4458; BuildManager:GetMaxLevelUpgradeFinishBuild, BuildManager.lua:4993-5006). Research
// power (score 90302, type 6) opens the science queue panel (GoToUtil.lua:4339-4340, 4391-4397),
// whose finished cells send queue.finish (UILWScienceQueueCell.lua:143-146). A finished research
// stays in queue_new until then (live 2026-10-06: this feature completed two). The score types are
// ScoreType.BuildPowerUp 23 and SciencePowerUp 6 (EnumType.lua:15799-15809). In tables 40413,
// heroevent 110001 (Base Expansion) and 110004 (Total Mobilization) score 90202, and 110002 (Age of
// Science) and 110004 score 90302.
//
// So each finished upgrade and research is held, per kind, the way supplies-search gates each
// radar search. It is held only while the account is in a running duel cycle and today's entry
// does not list the kind's type: in an alliance, the duel activity open (with its HQ level) and its
// info read. A rest day with no current entry counts as not listing it. With no duel (no
// alliance, no open duel activity, no type-15 entry at all) it finishes as before. When the duel
// cannot be read, it finishes with a Warn, so a duel read bug never stalls the queues. Unreadable
// means an error or success-only reply, no eventList, no type-15 entry with a day window, or
// today's list naming no score id the table knows. The duel is read only when something finished
// reaches the hold, through the run's activity sweep (one cached hero.event.info.get).
const (
	finishedTimersUpgradeCmd = "free.building.upgrade.finish"
	finishedTimersRepairCmd  = "user.finish.fix.building"
	finishedTimersQueueCmd   = "queue.finish"
	finishedTimersCampCmd    = "building.camp.collect"
	finishedTimersChipCmd    = "chip.collect"

	finishedTimersMilitaryCamp  = 10103000
	finishedTimersSmithShop     = 10101000
	finishedTimersChipFactory   = 10232000
	finishedTimersWormholeSub   = 792000
	finishedTimersShieldHQLevel = 7

	finishedTimersQueueHospital = 3
	finishedTimersQueueResearch = 6
	finishedTimersQueueRebirth  = 117

	finishedTimersStockEffect = "30122" // LW_SOLDIER_MAX_STOCK (EnumType.lua:4474)

	// finishedTimersConfirmSetting is UserSettingKey.FINISH_BUILDING_RECEIVE_REMINDER
	// (Global/EnumType.lua:5596), and finishedTimersConfirmHQLevel is DataConfig
	// building_finish_check k1 (table item, 40413), the HQ level from which the setting applies.
	finishedTimersConfirmSetting = "19"
	finishedTimersConfirmHQLevel = 16

	// The duel score types a completion earns (ScoreType, EnumType.lua:15799-15809).
	finishedTimersDuelBuildPower   int32 = 23 // BuildPowerUp: score 90202
	finishedTimersDuelSciencePower int32 = 6  // SciencePowerUp: score 90302
)

// finishedTimersDuelThemes lists, per completion score type, the duel themes whose heroevent row
// (tables 40413) scores it; only the hold's log line uses it.
var finishedTimersDuelThemes = map[int32][]int32{
	finishedTimersDuelBuildPower:   {DuelThemeBase, DuelThemeMobilization},
	finishedTimersDuelSciencePower: {DuelThemeScience, DuelThemeMobilization},
}

func init() {
	registerFeature(Feature{
		Name: "finished-timers",
		Summary: "collect ended timers for free: finished upgrades, repairs, research/heal queues and camp output; " +
			"finished upgrades and research wait for an Alliance Duel day that scores them",
		Run: runFinishedTimers,
	})
	session.RegisterBenignErrorCode("120289", finishedTimersUpgradeCmd, finishedTimersRepairCmd,
		finishedTimersQueueCmd, finishedTimersCampCmd, finishedTimersChipCmd)
}

// finishedTimersQueue is one init queue_new entry, with the client's field precedence.
type finishedTimersQueue struct {
	UUID, Type, Start, End int64
	Raw                    *sfs.SFSObject
}

func finishedTimersQueues(in *Init) []finishedTimersQueue {
	var out []finishedTimersQueue
	for _, q := range in.Objects("queue_new") {
		uuid, ok := claimInt(q, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		e := finishedTimersQueue{UUID: uuid, Raw: q}
		e.Type, _ = claimInt(q, "type")
		e.Start, _ = claimFirstInt(q, "startTime", "sT")
		e.End, _ = claimFirstInt(q, "updateTime", "uT")
		out = append(out, e)
	}
	return out
}

// Finished is QueueInfo:SetQueueState's Finish state.
func (q finishedTimersQueue) Finished(nowMs int64) bool {
	return (q.Start != 0 && q.Start == q.End) || (q.End != 0 && nowMs >= q.End)
}

// Working is QueueInfo:SetQueueState's Work state.
func (q finishedTimersQueue) Working(nowMs int64) bool {
	return (q.Start == 0 || q.Start != q.End) && nowMs < q.End
}

// finishedTimersStockRoom returns the soldier total and the (lower-bound) stock cap; ok is false
// when init lacks either.
func finishedTimersStockRoom(in *Init) (total, limit int64, ok bool) {
	effects := in.Object("effect")
	v, has := effects.Get(finishedTimersStockEffect)
	if !has {
		return 0, 0, false
	}
	var capF float64
	switch n := v.Val.(type) {
	case float64:
		capF = n
	case float32:
		capF = float64(n)
	default:
		i, iok := claimIntValue(n)
		if !iok {
			return 0, 0, false
		}
		capF = float64(i)
	}
	if _, has := in.Raw.Get("resource_items"); !has {
		return 0, 0, false
	}
	for _, it := range claimObjectsOrMap(in.Raw, "resource_items") {
		id, _ := claimInt(it, "itemId")
		if id < 3005 || id > 3015 {
			continue
		}
		if n, ok := claimFirstInt(it, "number", "count"); ok && n > 0 {
			total += n
		}
	}
	return total, int64(math.Floor(capF)), true
}

func runFinishedTimers(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("finished-timers: no init push; skipping")
		return nil
	}
	now := claimNow(in).UnixMilli()
	soldiers, stockCap, stockKnown := finishedTimersStockRoom(in)
	stockRoom := func() bool { return stockKnown && soldiers < stockCap }
	confirmOn := finishedTimersConfirmOn(in)
	hold := newCompletionHold("finished-timers", conn, in)

	var actions []func() error
	send := func(label, cmd string, uuid int64) {
		actions = append(actions, func() error {
			p := sfs.NewSFSObject()
			p.PutLong("uuid", uuid)
			_, err := claimAndLog(conn, label, cmd, p)
			return err
		})
	}

	for _, b := range in.Objects("building_new") {
		uuid, ok := claimInt(b, "uuid")
		if !ok || uuid == 0 {
			continue
		}
		bID, _ := claimInt(b, "bId")
		lv, _ := claimInt(b, "lv")
		name := fmt.Sprintf("%s %d", BuildingNameOf(int32(bID)), uuid)

		uT, fromQueue := buildingUpgradeTime(in, b)
		if own, _ := claimInt(b, "uT"); fromQueue && own != uT {
			slog.Info("finished-timers: the build queue's uT overrides the building's, as in the client", "uuid", uuid,
				"buildingUT", own, "queueUT", uT)
		}
		if uT > 0 && now >= uT {
			switch {
			case bID == finishedTimersWormholeSub && lv == 0:
			case bID == claimHQBuildingID && lv == finishedTimersShieldHQLevel && finishedTimersShieldUp(in, now):
				slog.Info("finished-timers: leaving the HQ 8 upgrade to the player while the shield is up", "uuid", uuid)
			case confirmOn:
				slog.Info("finished-timers: leaving the finished upgrade to the player: the \"confirm finished building\" setting is on",
					"uuid", uuid, "name", name)
			case hold.held(finishedTimersDuelBuildPower):
				slog.Info("finished-timers: holding the finished upgrade for an Alliance Duel day that scores building power",
					"uuid", uuid, "name", name, "today", hold.today(), "waitsFor", completionHoldWaitsFor(finishedTimersDuelBuildPower))
			default:
				send("finish upgrade "+name, finishedTimersUpgradeCmd, uuid)
			}
		}
		if dEndT, _ := claimInt(b, "dEndT"); dEndT > 0 && now >= dEndT {
			send("finish repair "+name, finishedTimersRepairCmd, uuid)
		}
		if finishedTimersCampDone(b, now) {
			switch bID {
			case finishedTimersMilitaryCamp:
				prodBase, _ := claimInt(b, "prodBase")
				if !stockRoom() || prodBase > stockCap {
					slog.Info("finished-timers: camp output skipped (troop stock full or unknown)", "uuid", uuid,
						"soldiers", soldiers, "stockCap", stockCap, "known", stockKnown)
					continue
				}
				soldiers += prodBase
				send("collect camp "+name, finishedTimersCampCmd, uuid)
			case finishedTimersSmithShop:
				send("collect smith "+name, finishedTimersCampCmd, uuid)
			case finishedTimersChipFactory:
				actions = append(actions, func() error { return finishedTimersChip(conn, uuid) })
			}
		}
	}

	for _, q := range finishedTimersQueues(in) {
		if !q.Finished(now) {
			continue
		}
		switch q.Type {
		case finishedTimersQueueResearch:
			if hold.held(finishedTimersDuelSciencePower) {
				slog.Info("finished-timers: holding the finished research for an Alliance Duel day that scores research power",
					"queue", q.UUID, "today", hold.today(), "waitsFor", completionHoldWaitsFor(finishedTimersDuelSciencePower))
				continue
			}
			send(fmt.Sprintf("finish research queue %d", q.UUID), finishedTimersQueueCmd, q.UUID)
		case finishedTimersQueueHospital, finishedTimersQueueRebirth:
			if !stockRoom() {
				slog.Info("finished-timers: healed troops left in the hospital (troop stock full or unknown)",
					"queue", q.UUID, "type", q.Type, "soldiers", soldiers, "stockCap", stockCap, "known", stockKnown)
				continue
			}
			send(fmt.Sprintf("finish hospital queue %d (type %d)", q.UUID, q.Type), finishedTimersQueueCmd, q.UUID)
		}
	}

	if len(actions) == 0 {
		slog.Info("finished-timers: no ended timer to collect")
		return nil
	}
	return errors.Join(claimEach(actions, func(a func() error) error { return a() })...)
}

// finishedTimersConfirmOn reports whether the client would ask before finishing an upgrade: the
// player turned on user setting 19 (init userSetting, an object keyed by the setting id as a
// string, PlayerInfo.lua:158-159, 1524-1530) and the HQ is at building_finish_check.k1 or above
// (BuildManager.lua:3089-3113). The client compares the value with the string "1" (an integer 1
// would not match in Lua) and sends "0" itself when the server has none (InitMessage.lua:707-711),
// so a missing userSetting or key is off. A player who ticked "don't show again today" gets no
// dialog (UIUtil.lua:3288-3305), which Go cannot see; leaving the upgrade for the player is the
// side that only sends less. Repairs, queues and camp output are not behind this dialog.
func finishedTimersConfirmOn(in *Init) bool {
	v, ok := in.Object("userSetting").Get(finishedTimersConfirmSetting)
	if !ok {
		return false
	}
	on, _ := v.Val.(string)
	return on == "1" && claimHQLevel(in) >= finishedTimersConfirmHQLevel
}

// finishedTimersShieldUp reports whether the city shield is up (defend_wall.protectEndTime >= now),
// treating a missing defend_wall as up so HQ 7 is never finished blind.
func finishedTimersShieldUp(in *Init, nowMs int64) bool {
	end, ok := claimInt(in.Object("defend_wall"), "protectEndTime")
	return !ok || end >= nowMs
}

// finishedTimersCampDone is BuildingFunctioningFinish: lv > 0, a production cycle started and its
// end passed.
func finishedTimersCampDone(b *sfs.SFSObject, nowMs int64) bool {
	lv, _ := claimInt(b, "lv")
	start, _ := claimInt(b, "prodST")
	end, ok := claimInt(b, "prodET")
	return lv > 0 && start > 0 && ok && end > 0 && nowMs >= end
}

// finishedTimersChip collects the Tactical Chip Factory the way the city bubble does and falls back
// to the factory panel's chip.collect {} if the server rejects the building type.
func finishedTimersChip(conn *session.GameConn, uuid int64) error {
	p := sfs.NewSFSObject()
	p.PutLong("uuid", uuid)
	label := fmt.Sprintf("collect chip factory %d", uuid)
	msg, err := claimAndLog(conn, label, finishedTimersCampCmd, p)
	if err != nil && msg != nil && msg.Params.GetString("errorCode") == "E000001" {
		_, err = claimAndLog(conn, label+" (chip.collect)", finishedTimersChipCmd, sfs.NewSFSObject())
	}
	return err
}

// completionHold is one run's view of the Alliance Duel for the completion hold (see the comment
// at the top of this file), read on first use.
type completionHold struct {
	feature string
	conn    *session.GameConn
	in      *Init
	read    bool
	running bool     // in a running duel cycle whose info was read
	day     *DuelDay // today's entry; nil on the rest day or outside a day window
}

func newCompletionHold(feature string, conn *session.GameConn, in *Init) *completionHold {
	return &completionHold{feature: feature, conn: conn, in: in}
}

// held reports whether a completion that earns duel score type typ waits for another day: the
// account is in a running duel cycle and today's entry (or the lack of one) doesn't list typ.
func (h *completionHold) held(typ int32) bool {
	if !h.read {
		h.read = true
		h.running, h.day = completionHoldRead(h.feature, h.conn, h.in)
	}
	return h.running && !h.day.Scores(typ, "")
}

// today names today's duel theme for the hold's log lines.
func (h *completionHold) today() string {
	if h.day == nil {
		return "no duel day now (rest day)"
	}
	return h.day.ThemeName()
}

// completionHoldWaitsFor names the themes that score typ.
func completionHoldWaitsFor(typ int32) string {
	var names []string
	for _, th := range finishedTimersDuelThemes[typ] {
		names = append(names, (&DuelDay{Theme: th}).ThemeName())
	}
	return strings.Join(names, " or ")
}

// completionHoldRead reads the duel for the hold: running is true only when the account is in an
// alliance, a duel activity is open for it (CheckIsSend, with its needMainCityLevel) and its info
// reply reads; day is today's entry, nil on a day with none. Every other case returns running
// false, so the completions go ahead: with an Info line when there is no duel, with a Warn when the
// duel could not be read.
func completionHoldRead(feature string, conn *session.GameConn, in *Init) (running bool, day *DuelDay) {
	if !claimInAlliance(in) {
		slog.Info(feature + ": not in an alliance, so no Alliance Duel hold")
		return false, nil
	}
	s := Activities(conn, in)
	acts := s.Open(actTypeAllianceCompete)
	if len(acts) == 0 {
		slog.Info(feature + ": no Alliance Duel activity open for this account, so no duel hold")
		return false, nil
	}
	now := evNow()
	for _, a := range acts {
		info, err := s.EventInfo(a)
		if err != nil {
			slog.Warn(feature+": could not read today's Alliance Duel; finishing without the duel hold",
				"activity", a.ID, "error", err)
			return false, nil
		}
		d, entries, why := completionHoldDay(info, now)
		switch {
		case why != "":
			slog.Warn(feature+": could not read today's Alliance Duel; finishing without the duel hold",
				"activity", a.ID, "reason", why)
			return false, nil
		case entries == 0:
			slog.Info(feature+": the Alliance Duel info lists no duel day, so no duel hold", "activity", a.ID)
			return false, nil
		case d != nil:
			slog.Info(feature+": Alliance Duel hold: today's entry read", "activity", a.ID, "theme", d.ThemeName(),
				"themeId", d.Theme, "dayEnds", d.End.UTC().Format(time.RFC3339))
			return true, d
		}
	}
	a := acts[0]
	slog.Info(feature+": Alliance Duel hold: the duel is open but no day entry covers now (rest day); completions wait",
		"activity", a.ID, "activityStart", time.UnixMilli(a.StartTime()).UTC().Format(time.RFC3339),
		"activityEnd", time.UnixMilli(a.EndTime()).UTC().Format(time.RFC3339))
	return true, nil
}

// completionHoldDay picks today's entry from a duel info reply (parseDuelDay). entries counts the
// type-15 entries; why is set when the reply cannot be read: no eventList, type-15 entries none of
// which has a day window, or a current entry whose score list names no id in duelScoreTable.
func completionHoldDay(info *sfs.SFSObject, now time.Time) (d *DuelDay, entries int, why string) {
	if !info.Has("eventList") {
		return nil, 0, "the info reply has no eventList"
	}
	windows := 0
	for _, e := range evObjects(info, "eventList") {
		if t, ok := evNum(e, "t"); !ok || t != allianceDuelEventType {
			continue
		}
		entries++
		if _, _, ok := duelEntryWindow(e); ok {
			windows++
		}
	}
	if entries > 0 && windows == 0 {
		return nil, entries, "no duel entry carries a day window (st/et or begintime/endtime)"
	}
	d = parseDuelDay(info, now)
	if d == nil {
		return nil, entries, ""
	}
	for _, id := range d.ScoreIDs {
		if _, ok := duelScoreTable[id]; ok {
			return d, entries, ""
		}
	}
	return nil, entries, fmt.Sprintf("today's score list %v names no score id the table knows", d.ScoreIDs)
}
