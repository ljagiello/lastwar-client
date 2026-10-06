package game

import (
	"cmp"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Radar execute half (DUEL.md §5.2): run the radar tasks the client finishes without a real march,
// so they sit FINISHED (state 1) and stop expiring until a day whose Alliance Duel scores radar
// tasks (score type 82, Mon/Wed/Fri). The duel points land at the claim, not at the finish: the
// client's Claim-All warning says so on off days (en:45902, detect_quick_reward_confirm_text;
// DetectEventCompleteOneClickBtnView.lua:262-305), and the client lists FINISHED tasks whatever
// their endTime while it hides an unstarted one past it (RadarCenterDataManager.lua:55-79). That
// the server keeps returning a FINISHED task past its endTime is unverified (DUEL.md §8 1a).
//
// The flow is the client's Quick Execute (DetectEventCompleteOneClickBtnView.lua:93-217,
// RadarFakeUIMarchManager.lua:52-77), which runs every march-free task at once:
//   - a task in state 3 (NOT_IN_WORLD) is first placed on the map with
//     `detect.event.batch.put.point.in.world {uuidList: "uuid|uuid"}`; the reply's `array` holds
//     the placed tasks, now in state 0 (DetectEventBatchPutPointInWorldMessage.lua:4-29);
//   - PickGarbage (6) and DOMINATOR_CURE (26): `start.pick.garbage {uuid}`, then after the fake
//     march `finish.sampling {uuid}` (RadarFakeUIMarchData_CollectGarbage.lua:12-17); SEASON_VISITOR
//     (44) also sends `finish.visitor {uuid}` (RadarFakeUIMarchData_SeasonVisitor.lua:7-13);
//   - HELPER (18): `detect.event.help.start` / `.end {uuid, eventType: 18}`
//     (RadarFakeUIMarchData_Help.lua:12-17). It costs 10 stamina (GetDetectHelpTypeCostNum,
//     RadarCenterDataManager.lua:767-769) unless the task has cost == 1
//     (DetectEventCompleteOneClickBtnView.lua:139-152, 194-216), and is skipped below a reserve;
//   - PLOT (14, Rescue the Survivor): `start.detect.event.talk {uuid}`, then
//     `end.detect.event.talk {uuid}` once the plot has played (UIDetectEventView.lua:1082-1099). The
//     end reply carries the reward, so a talk finishes and claims in one step and can't be banked:
//     it runs only on a radar-scoring day.
//
// The fake march lasts min(3000 ms, distance x 1000 / detect_quick_finish_config.k2)
// (RadarFakeUIMarchData_Base.lua:35-59), so every end is sent radarFakeMarchWait after the last
// start. Everything else is left alone, by an allowlist: every march, rally, battle and mini-game
// type (2, 8, 16, 17, 20, 40, 46, the PvE types 7/10/12/15 whose result the client simulates, ...).
// RESCUE (11) is left alone too although its fake march is the same pair (FakeRescueMarchData.lua:
// 63, 90): its claim needs the client's troop-cap confirm, which radar-claims can't do, so a
// FINISHED rescue would hold its slot until the player claims it, while an unstarted one expires.
// Nothing here re-rolls a task (`reset.detect.event` costs diamonds, detect_config.k7), sends a
// battle result, or uses an item.
//
// On a radar-scoring day the tasks this feature finishes are claimed in the same run, and the run
// repeats on the tasks those claims draw from stock (the claim reply's newEvent,
// RadarCenterDataManager.lua:289-327). radar-claims can't do it: RunFeatures runs features in
// name order, so it runs before radar-execute. Static-only: not sent live yet.

//go:generate go run ./genradar -events ${LASTWAR_TABLES}/detect_event.json -levels ${LASTWAR_TABLES}/detect_level.json -version ${LASTWAR_TABLE_VERSION} -out radar_tables_gen.go

const (
	radarPlaceCmd       = "detect.event.batch.put.point.in.world"
	radarPickStartCmd   = "start.pick.garbage"
	radarSamplingEndCmd = "finish.sampling"
	radarVisitorEndCmd  = "finish.visitor"
	radarHelpStartCmd   = "detect.event.help.start"
	radarHelpEndCmd     = "detect.event.help.end"
	radarTalkStartCmd   = "start.detect.event.talk"
	radarTalkEndCmd     = "end.detect.event.talk"

	// DetectEventState (EnumType.lua:8465-8470); 1 FINISHED is radarStateFinished.
	radarStateNotFinish  = 0
	radarStateRewarded   = 2
	radarStateNotInWorld = 3

	// DetectEventType (EnumType.lua:8512-8552).
	radarTypePickGarbage   int32 = 6
	radarTypeRescue        int32 = 11
	radarTypePlot          int32 = 14
	radarTypeHelper        int32 = 18
	radarTypeDominatorCure int32 = 26
	radarTypeSeasonVisitor int32 = 44

	radarHelpStaminaCost = 10
	// radarHelpStaminaReserve is the stamina Help Teammates never spends below, kept for marches.
	radarHelpStaminaReserve = 60
	// car_stamina k1 (base stamina cap) and k2 (seconds per regenerated point), table item 1.0.364
	// (ArmyFormationDataManager.lua:156-161); the live cap and rate only add effects on top.
	radarStaminaBaseMax = 110
	radarStaminaRegen   = 300 * time.Second

	// radarFakeMarchWait is the client's longest fake march (3 s) plus a margin for rounding.
	radarFakeMarchWait = 3500 * time.Millisecond
	// radarExecuteMaxEvents caps the tasks started per run; radarExecuteMaxRounds caps the
	// execute-claim rounds on a radar-scoring day.
	radarExecuteMaxEvents = 30
	radarExecuteMaxRounds = 4
)

// radarSleep is the fake-march wait; tests replace it.
var radarSleep = time.Sleep

// radarTypeRange is a run of detect_event ids [lo, hi] sharing one type (radar_tables_gen.go).
type radarTypeRange struct {
	lo, hi int64
	typ    int32
}

// radarLevelRow is one detect_level row (radar_tables_gen.go). detect_level_B, the table the
// client reads instead when monopoly_ab == 1 (PlayerInfo.lua:1030-1031, 1448-1457), has identical
// rows in 1.0.364.
type radarLevelRow struct {
	show, max, refreshMin, refreshN int64
}

// radarEventType returns detect_event.type for eventID; ok is false for an id the table lacks.
func radarEventType(eventID int64) (int32, bool) {
	i := sort.Search(len(radarEventTypeRanges), func(i int) bool { return radarEventTypeRanges[i].hi >= eventID })
	if i < len(radarEventTypeRanges) && radarEventTypeRanges[i].lo <= eventID {
		return radarEventTypeRanges[i].typ, true
	}
	return 0, false
}

func init() {
	registerFeature(Feature{
		Name: "radar-execute",
		// A Help Teammates task names the ally it helps (helpInfo.targetUid, DetectEventInfo.lua:
		// 62-72), and a helped task records its helper (completeByHelper, :74-83): visible to
		// another player, so the feature is opt-in.
		Summary: "opt-in: run march-free radar tasks (sampling, treatment, season visitor, and Help Teammates for an ally above " +
			"a stamina reserve) so they wait finished; on radar-scoring duel days also talk tasks and claim what it finished",
		// It scores only through its own claims on a radar-scoring day, and must run every day.
		Duel: []DuelScore{{Type: DuelScoreRadarTask}},
		Run:  runRadarExecute,
	})
	registerFeature(Feature{
		Name:    "radar-inventory",
		Summary: "read-only: log every radar task with its type and state and what radar-execute and radar-overflow would do",
		Run:     runRadarInventory,
	})
}

// detectEvent is one entry of get.detect.info's events (DetectEventInfo.lua:36-120).
type detectEvent struct {
	uuid, eventID, state, cost, startTime, endTime int64
	typ                                            int32 // detect_event.type; 0 when the id isn't in the table
	sentType                                       int64 // the entry's own `type` field, -1 when absent
}

// detectSnapshot is a get.detect.info reply.
type detectSnapshot struct {
	level, eventNum, nextRefresh int64
	hasLevel, hasEventNum        bool
	events                       []detectEvent
}

func parseDetectEvent(o *sfs.SFSObject) detectEvent {
	ev := detectEvent{sentType: -1}
	ev.uuid, _ = claimInt(o, "uuid")
	ev.eventID, _ = claimInt(o, "eventId")
	if s, ok := claimInt(o, "state"); ok {
		ev.state = s
	} else {
		ev.state = -1
	}
	ev.cost, _ = claimInt(o, "cost")
	ev.startTime, _ = claimInt(o, "startTime")
	ev.endTime, _ = claimInt(o, "endTime")
	if t, ok := claimInt(o, "type"); ok {
		ev.sentType = t
	}
	ev.typ, _ = radarEventType(ev.eventID)
	return ev
}

// readDetect sends get.detect.info {openWnd: false}, as the client does after init
// (DetectInfoGetMessage.lua:4-11), and parses detectInfo {level, eventNum, nextRefreshTime, ...}
// (RadarCenterDataManager.lua:251-266) and events, an array or a map (table.walk, :39-54).
func readDetect(conn *session.GameConn) (*detectSnapshot, error) {
	params := sfs.NewSFSObject()
	params.PutBool("openWnd", false)
	msg, err := session.SendAndWait(conn, "radar info", radarInfoCmd, params)
	if err != nil {
		return nil, err
	}
	d := evObject(msg.Params, "detectInfo")
	s := &detectSnapshot{}
	s.level, s.hasLevel = claimInt(d, "level")
	s.eventNum, s.hasEventNum = claimInt(d, "eventNum")
	s.nextRefresh, _ = claimInt(d, "nextRefreshTime")
	for _, o := range claimObjectsOrMap(msg.Params, "events") {
		s.events = append(s.events, parseDetectEvent(o))
	}
	return s, nil
}

// radarAction is what radar-execute does with one task.
type radarAction int

const (
	radarSkip    radarAction = iota
	radarPick                // start.pick.garbage, finish.sampling
	radarVisitor             // as radarPick, then finish.visitor
	radarHelp                // detect.event.help.start/end {eventType: 18}
	radarTalk                // start/end.detect.event.talk; finishes and claims
)

func (a radarAction) String() string {
	return [...]string{"skip", "sample", "sample+visitor", "help", "talk"}[a]
}

// radarDecision is radar-execute's verdict on one task; why says why a skipped task is skipped.
type radarDecision struct {
	ev     detectEvent
	action radarAction
	why    string
}

// radarStamina is the stamina Help Teammates may spend, as a lower bound.
type radarStamina struct {
	have       int64
	known      bool
	inAlliance bool
}

// radarStaminaFloor reads init playerInfo {stamina, lastStaminaTime (ms)}: PlayerInfo:InitFromNet
// passes init playerInfo to UpdatePlayerInfo (PlayerInfo.lua:150-158, 425-429), whose
// UpdatePowerData calls SetStaminaData (:282, :321, :1158-1169). The client adds one point per
// FormationStaminaUpdateTime since lastStaminaTime up to FormationStaminaMax (GetCurStamina,
// :1186-1197); with the base car_stamina values, which effects only raise, this is a lower bound.
func radarStaminaFloor(in *Init, now time.Time) (int64, bool) {
	p := in.Object("playerInfo")
	st, ok := evNum(p, "stamina")
	if !ok {
		return 0, false
	}
	if last, ok := claimInt(p, "lastStaminaTime"); ok && last > 0 && st < radarStaminaBaseMax {
		if regen := (now.UnixMilli() - last) / radarStaminaRegen.Milliseconds(); regen > 0 {
			st = min(st+regen, radarStaminaBaseMax)
		}
	}
	return st, true
}

// radarExecuteDecide returns radar-execute's verdict on every task, the ones it runs first, in
// endTime order (soonest to expire first). It spends st for each Help Teammates task it picks and
// picks at most budget tasks.
func radarExecuteDecide(events []detectEvent, radarDay bool, st *radarStamina, budget int, nowMs int64) []radarDecision {
	ends := func(ev detectEvent) int64 {
		if ev.endTime <= 0 {
			return math.MaxInt64
		}
		return ev.endTime
	}
	sorted := slices.Clone(events)
	slices.SortStableFunc(sorted, func(a, b detectEvent) int {
		return cmp.Or(cmp.Compare(ends(a), ends(b)), cmp.Compare(a.uuid, b.uuid))
	})
	var run, skip []radarDecision
	for _, ev := range sorted {
		action, why := radarExecPlan(ev, radarDay, nowMs)
		if action != radarSkip && len(run) >= budget {
			action, why = radarSkip, "per-run cap reached"
		}
		if action == radarHelp {
			cost := int64(radarHelpStaminaCost)
			if ev.cost == 1 {
				cost = 0
			}
			switch {
			case !st.inAlliance:
				action, why = radarSkip, "Help Teammates needs an alliance"
			case !st.known:
				action, why = radarSkip, "init has no playerInfo.stamina"
			case st.have-cost < radarHelpStaminaReserve:
				action, why = radarSkip, fmt.Sprintf("stamina %d would drop below the %d reserve", st.have, radarHelpStaminaReserve)
			default:
				st.have -= cost
			}
		}
		if action == radarSkip {
			skip = append(skip, radarDecision{ev: ev, why: why})
		} else {
			run = append(run, radarDecision{ev: ev, action: action})
		}
	}
	return append(run, skip...)
}

// radarExecPlan decides, stamina aside, what radar-execute does with ev.
func radarExecPlan(ev detectEvent, radarDay bool, nowMs int64) (radarAction, string) {
	switch {
	case ev.uuid == 0:
		return radarSkip, "no uuid"
	case ev.state == radarStateFinished:
		return radarSkip, "finished (banked)"
	case ev.state == radarStateRewarded:
		return radarSkip, "rewarded"
	case ev.state != radarStateNotFinish && ev.state != radarStateNotInWorld:
		return radarSkip, "unknown state"
	case ev.typ == 0:
		return radarSkip, "eventId not in the detect_event table"
	case ev.sentType >= 0 && ev.sentType != int64(ev.typ):
		return radarSkip, fmt.Sprintf("its type field %d disagrees with detect_event.type %d", ev.sentType, ev.typ)
	case ev.endTime > 0 && ev.endTime <= nowMs:
		return radarSkip, "expired"
	}
	switch ev.typ {
	case radarTypePickGarbage, radarTypeDominatorCure:
		return radarPick, ""
	case radarTypeSeasonVisitor:
		return radarVisitor, ""
	case radarTypeHelper:
		return radarHelp, ""
	case radarTypePlot:
		if radarDay {
			return radarTalk, ""
		}
		return radarSkip, "a talk finishes and claims at once: held for a radar-scoring day"
	case radarTypeRescue:
		return radarSkip, "rescue: its claim needs the troop-cap check, so it is left to the player"
	}
	return radarSkip, "not march-free (march, rally, battle or mini-game)"
}

func runRadarExecute(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("radar-execute: no init push; skipping")
		return nil
	}
	radarDay := TodayDuel(conn, in).Scores(DuelScoreRadarTask, "")
	st := radarStamina{inAlliance: claimInAlliance(in)}
	st.have, st.known = radarStaminaFloor(in, claimNow(in))
	snap, err := readDetect(conn)
	if err != nil {
		return err
	}
	rounds := 1
	if radarDay {
		rounds = radarExecuteMaxRounds
	}
	budget := radarExecuteMaxEvents
	var errs []error
	for round := 1; round <= rounds && budget > 0; round++ {
		var todo []radarDecision
		for _, d := range radarExecuteDecide(snap.events, radarDay, &st, budget, claimNow(in).UnixMilli()) {
			if d.action != radarSkip {
				todo = append(todo, d)
			}
		}
		if len(todo) == 0 {
			if round == 1 {
				slog.Info("radar-execute: nothing to execute", "events", len(snap.events), "radarDay", radarDay)
			}
			break
		}
		budget -= len(todo)
		done, rerrs := radarExecute(conn, todo)
		errs = append(errs, rerrs...)
		if session.ContainsNonTimeoutNetError(errors.Join(rerrs...)) || !radarDay {
			break
		}
		if snap, err = readDetect(conn); err != nil {
			errs = append(errs, err)
			break
		}
		claimed, cerrs := radarClaimFinished(conn, snap, done)
		errs = append(errs, cerrs...)
		if claimed == 0 || session.ContainsNonTimeoutNetError(errors.Join(cerrs...)) {
			break
		}
		// The claims drew new tasks from stock into the freed slots.
		if snap, err = readDetect(conn); err != nil {
			errs = append(errs, err)
			break
		}
	}
	return errors.Join(errs...)
}

// radarSend sends one execute step. ok is false only for a reply carrying an errorCode or a dead
// connection: the client never reads these replies (FinishSamplingMessage.lua:7-9,
// DetectEventHelpEndMessage.lua:8-10) and drives the fake march by time, so a step that gets no
// reply in time still counts as sent, and the task's state in the next get.detect.info decides.
func radarSend(conn *session.GameConn, label, cmd string, p *sfs.SFSObject) (*session.ExtensionMessage, bool, error) {
	msg, err := session.SendAndWait(conn, label, cmd, p)
	switch {
	case err == nil:
		return msg, true, nil
	case session.ContainsNonTimeoutNetError(err):
		return nil, false, err
	case msg == nil:
		slog.Warn(label+": no reply; counting it as sent", "cmd", cmd)
		return nil, true, nil
	}
	return msg, false, err
}

func radarUUIDParams(uuid int64) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutLong("uuid", uuid)
	return p
}

func radarHelpParams(uuid int64) *sfs.SFSObject {
	p := radarUUIDParams(uuid)
	p.PutInt("eventType", radarTypeHelper)
	return p
}

// radarExecute places, starts and ends the tasks in todo and returns the uuids it ended (talks
// excluded: their end already claimed them). It stops at a dead connection.
func radarExecute(conn *session.GameConn, todo []radarDecision) (map[int64]bool, []error) {
	var errs []error
	ready, err := radarPlace(conn, todo)
	if err != nil {
		errs = append(errs, err)
		if session.ContainsNonTimeoutNetError(err) {
			return nil, errs
		}
	}
	var started []radarDecision
	for _, d := range ready {
		label := fmt.Sprintf("radar %s %d (event %d)", d.action, d.ev.uuid, d.ev.eventID)
		var cmd string
		p := radarUUIDParams(d.ev.uuid)
		switch d.action {
		case radarPick, radarVisitor:
			cmd = radarPickStartCmd
		case radarHelp:
			cmd, p = radarHelpStartCmd, radarHelpParams(d.ev.uuid)
		case radarTalk:
			cmd = radarTalkStartCmd
		}
		_, ok, err := radarSend(conn, label+" start", cmd, p)
		if err != nil {
			errs = append(errs, err)
			if session.ContainsNonTimeoutNetError(err) {
				return nil, errs
			}
		}
		if ok {
			started = append(started, d)
		}
	}
	if len(started) == 0 {
		return nil, errs
	}
	radarSleep(radarFakeMarchWait)

	done := map[int64]bool{}
	for _, d := range started {
		label := fmt.Sprintf("radar %s %d (event %d)", d.action, d.ev.uuid, d.ev.eventID)
		var steps []string
		p := radarUUIDParams(d.ev.uuid)
		switch d.action {
		case radarPick:
			steps = []string{radarSamplingEndCmd}
		case radarVisitor:
			steps = []string{radarSamplingEndCmd, radarVisitorEndCmd}
		case radarHelp:
			steps, p = []string{radarHelpEndCmd}, radarHelpParams(d.ev.uuid)
		case radarTalk:
			steps = []string{radarTalkEndCmd}
		}
		ok := true
		for _, cmd := range steps {
			msg, sent, err := radarSend(conn, label+" end", cmd, p)
			if err != nil {
				errs = append(errs, err)
				if session.ContainsNonTimeoutNetError(err) {
					return done, errs
				}
			}
			if cmd == radarTalkEndCmd && msg != nil && !msg.Params.Has("errorCode") {
				slog.Info(label+" granted "+describeGrants(msg.Params), "cmd", cmd)
			}
			ok = ok && sent
		}
		if ok && d.action != radarTalk {
			done[d.ev.uuid] = true
		}
	}
	slog.Info("radar-execute: executed", "started", len(started), "ended", len(done))
	return done, errs
}

// radarPlace returns the tasks of todo that are on the map: those already in state 0, and those in
// state 3 that the batch placement reply lists in state 0.
func radarPlace(conn *session.GameConn, todo []radarDecision) ([]radarDecision, error) {
	var ready, place []radarDecision
	var uuids []string
	for _, d := range todo {
		if d.ev.state == radarStateNotInWorld {
			place = append(place, d)
			uuids = append(uuids, strconv.FormatInt(d.ev.uuid, 10))
		} else {
			ready = append(ready, d)
		}
	}
	if len(place) == 0 {
		return ready, nil
	}
	p := sfs.NewSFSObject()
	p.PutUtfString("uuidList", strings.Join(uuids, "|"))
	msg, err := session.SendAndWait(conn, fmt.Sprintf("radar place %d tasks", len(place)), radarPlaceCmd, p)
	if err != nil {
		return ready, err
	}
	placed := map[int64]bool{}
	for _, o := range claimObjects(msg.Params, "array") {
		ev := parseDetectEvent(o)
		if ev.state == radarStateNotFinish {
			placed[ev.uuid] = true
		}
	}
	for _, d := range place {
		if placed[d.ev.uuid] {
			ready = append(ready, d)
		} else {
			slog.Warn("radar-execute: task not placed on the map; leaving it for the next run", "uuid", d.ev.uuid, "eventId", d.ev.eventID)
		}
	}
	return ready, nil
}

// radarClaimFinished claims the tasks in done that snap shows FINISHED (state 1), with the client's
// `receive.detect.event.reward {uuid}` (DetectEventCompleteOneClickBtnView.lua:262-279).
func radarClaimFinished(conn *session.GameConn, snap *detectSnapshot, done map[int64]bool) (int, []error) {
	var due []int64
	for _, ev := range snap.events {
		if !done[ev.uuid] {
			continue
		}
		if ev.state == radarStateFinished && !radarSkippedEvent(ev.eventID) {
			due = append(due, ev.uuid)
		} else {
			slog.Info("radar-execute: executed task not finished yet; radar-claims takes it later", "uuid", ev.uuid, "state", ev.state)
		}
	}
	claimed := 0
	errs := claimEach(due, func(uuid int64) error {
		_, err := claimAndLog(conn, fmt.Sprintf("radar event %d", uuid), radarEventCmd, radarUUIDParams(uuid))
		if err == nil {
			claimed++
		}
		return err
	})
	return claimed, errs
}

// runRadarInventory reads the radar state and logs what radar-execute and radar-overflow would do
// with it. It sends nothing but get.detect.info and the duel's hero.event.info.get.
func runRadarInventory(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		slog.Info("radar-inventory: no init push; skipping")
		return nil
	}
	day := TodayDuel(conn, in)
	radarDay := day.Scores(DuelScoreRadarTask, "")
	now := claimNow(in)
	st := radarStamina{inAlliance: claimInAlliance(in)}
	st.have, st.known = radarStaminaFloor(in, now)
	snap, err := readDetect(conn)
	if err != nil {
		return err
	}
	slog.Info("radar-inventory", "theme", day.ThemeName(), "radarDay", radarDay, "level", snap.level, "eventNum", snap.eventNum,
		"slotsUsed", len(snap.events), "nextRefresh", time.UnixMilli(snap.nextRefresh).UTC().Format(time.RFC3339),
		"staminaFloor", st.have, "staminaKnown", st.known, "inAlliance", st.inAlliance)

	overflowWhy := "radar-scoring day: radar-overflow does nothing, radar-claims claims"
	pick := map[int64]bool{}
	if !radarDay {
		plan, picks, err := radarOverflowPlan(snap)
		switch {
		case err != nil:
			overflowWhy = "radar-overflow would claim nothing: " + err.Error()
		default:
			overflowWhy = fmt.Sprintf("radar-overflow would claim %d of %d needed (eventNum %d + refresh %d - free slots %d - stock cap %d)",
				len(picks), max(plan.k, 0), plan.eventNum, plan.refreshN, plan.freeSlots, plan.maxNum)
			for _, ev := range picks {
				pick[ev.uuid] = true
			}
		}
	}
	slog.Info("radar-inventory: " + overflowWhy)
	for _, d := range radarExecuteDecide(snap.events, radarDay, &st, radarExecuteMaxEvents, now.UnixMilli()) {
		execute := d.action.String()
		if d.action == radarSkip {
			execute = "skip: " + d.why
		}
		left := "none"
		if d.ev.endTime > 0 {
			left = (time.Duration(d.ev.endTime-now.UnixMilli()) * time.Millisecond).Truncate(time.Second).String()
		}
		slog.Info("radar-inventory: task", "uuid", d.ev.uuid, "eventId", d.ev.eventID, "type", d.ev.typ, "sentType", d.ev.sentType,
			"state", d.ev.state, "endsIn", left, "cost", d.ev.cost, "radarExecute", execute, "radarOverflowClaims", pick[d.ev.uuid])
	}
	return nil
}
