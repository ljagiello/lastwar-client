package game

import (
	"log/slog"
	"slices"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Survivor Pack free rewards (MASTER.md G22; cov-dc-social-city F3, cov-ui-city F1 and their
// verdicts), OPT-IN, claims only. Static-only, from the 1.0.370 Lua and tables 40413, not sent
// live yet. The only type-188 row in tables 40413 (1110002) is an internal-test row (onoff "0",
// serverId "9999"): the feature does nothing until the event appears in the init activity list.
//
// While a Survivor Pack activity (type 188, EnumType.lua:6517) is open, startTime <= now < endTime
// (SurvivorPackManager.lua:477-484), the client lists today's survivors: the activity_survivor_list
// rows of the config row's list_group with day <= actOpenDay, actOpenDay being the server days
// since the start's server midnight plus 1 (:74-139). The config row is activity_survivor
// [tableInfoType or subType] (:74-86). State is init activitySurvivorVisitor {receiveIdArr,
// receiveFreeArr, receivePayArr, receiveScoreArr} (:60-67), refreshed by every reply that carries
// those arrays (ParseInfo, :195-211), among them survivor.visitor.info {activityId: Int}
// (SurvivorVisitorInfoMessage.lua:4-7; UISurvivorPackPopupView.lua:489), which this feature reads
// first.
//
// A survivor whose free reward is not in receiveFreeArr shows the popup's Get button, which sends
// survivor.visitor.receive.free {activityId: Int, id: Int} (UISurvivorPackPopupView.lua:238,
// 496-503; SurvivorVisitorReceiveFreeMessage.lua:4-8). Closing the popup of a survivor that came as
// a city visitor sends survivor.visitor.receive.bubble {activityId: Int, id: Int}, which files it
// in receiveIdArr (SurvivorPackManager.lua:443-463; SurvivorVisitorReceiveBubbleMessage.lua:4-8);
// a survivor already there opens the same popup from the list (:401-441;
// UISurvivorPackListItem.lua:110-115). So for a new survivor this sends free then bubble, as the
// popup does, and for a listed one only free. Score-bar stage i (0-based, bar_reward sorted by
// score) is claimable when the score_item count reaches its score and i is not in receiveScoreArr
// (:157-178, 238-272), and sends survivor.visitor.receive.score {activityId: Int, index: Int}
// (UISurvivorPackRewardItem.lua:76-78; SurvivorVisitorReceiveScoreMessage.lua:4-8). The score item
// comes only with the paid packs, so that step pays nothing to a non-paying account. Never the pay
// half (receivePayArr, PayManager:CallPayment).
const (
	survivorPackFeature = "survivor-pack"
	survivorPackActType = 188
	survivorInfoCmd     = "survivor.visitor.info"
	survivorFreeCmd     = "survivor.visitor.receive.free"
	survivorBubbleCmd   = "survivor.visitor.receive.bubble"
	survivorScoreCmd    = "survivor.visitor.receive.score"
	survivorScoreItem   = "993113" // activity_survivor score_item, both rows, tables 40413
)

// survivorConfigs is table activity_survivor 40413: list_group and the bar_reward scores, sorted.
var survivorConfigs = map[int64]struct {
	group  string
	scores []int64
}{
	1: {"1", []int64{5, 10, 20, 40, 50}},
	2: {"2", []int64{5, 10, 20, 40}},
}

// survivorListRow is one activity_survivor_list row; every 40413 row has a free reward.
type survivorListRow struct {
	id    int64
	group string
	day   int64
}

// survivorList is table activity_survivor_list 40413: ids 1-30, group "1", day = id.
var survivorList = func() []survivorListRow {
	rows := make([]survivorListRow, 0, 30)
	for id := int64(1); id <= 30; id++ {
		rows = append(rows, survivorListRow{id: id, group: "1", day: id})
	}
	return rows
}()

func init() {
	registerFeature(Feature{
		Name:      survivorPackFeature,
		Summary:   "OPT-IN: claim the Survivor Pack's free survivor rewards and reached score-bar stages; never the paid half (static-only, not validated live)",
		DefaultOn: false,
		Run:       runSurvivorPack,
	})
}

// survivorState is the four receive arrays.
type survivorState struct {
	ids, free, pay, score []int64
}

// overlay copies the arrays o carries, as ParseInfo does.
func (st *survivorState) overlay(o *sfs.SFSObject) {
	if o == nil {
		return
	}
	for _, f := range []struct {
		key string
		dst *[]int64
	}{{"receiveIdArr", &st.ids}, {"receiveFreeArr", &st.free}, {"receivePayArr", &st.pay}, {"receiveScoreArr", &st.score}} {
		if _, ok := o.Get(f.key); ok {
			*f.dst = evNums(o, f.key)
		}
	}
}

func runSurvivorPack(conn *session.GameConn, in *Init) error {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	b := &evBatch{conn: conn}
	now := evNow()
	for _, a := range s.All(survivorPackActType) {
		if b.dead {
			break
		}
		if nowMs := now.UnixMilli(); nowMs < a.StartTime() || nowMs >= a.EndTime() {
			continue
		}
		cfgID, ok := survivorConfigID(a)
		cfg, known := survivorConfigs[cfgID]
		if !ok || !known {
			slog.Info(survivorPackFeature+": no activity_survivor row for the activity; skipped",
				"activity", a.ID, "cfgId", cfgID, "tableVersion", tableVersion)
			continue
		}
		day, ok := survivorOpenDay(in, a.StartTime(), now)
		if !ok {
			slog.Info(survivorPackFeature+": init has no tomorrow; the activity day is unknown", "activity", a.ID)
			continue
		}
		claimSurvivorPack(s, b, in, a, cfg.group, cfg.scores, day)
	}
	return b.err()
}

// survivorConfigID is actData.tableInfoType or subType: the entry's own field when it has one,
// else the activity row's tableInfoType (Activity.SubType, ActivityInfoData.lua:231), the
// activity_survivor row.
func survivorConfigID(a Activity) (int64, bool) {
	for _, k := range []string{"tableInfoType", "subType"} {
		if n, ok := evNum(a.Raw, k); ok && n > 0 {
			return n, true
		}
	}
	if st := a.SubType(); st > 0 {
		return int64(st), true
	}
	return 0, false
}

// survivorOpenDay is actOpenDay: max(1, server days from the start's server midnight to today's + 1).
func survivorOpenDay(in *Init, startMs int64, now time.Time) (int64, bool) {
	today, ok := in.ServerDayStart(now)
	if !ok {
		return 0, false
	}
	startDay, _ := in.ServerDayStart(time.UnixMilli(startMs))
	return max(1, int64(today.Sub(startDay)/(24*time.Hour))+1), true
}

func claimSurvivorPack(s *ActivitySweep, b *evBatch, in *Init, a Activity, group string, scores []int64, day int64) {
	var st survivorState
	st.overlay(in.Object("activitySurvivorVisitor"))
	p := sfs.NewSFSObject()
	p.PutInt("activityId", a.ID)
	info, err := s.Info(survivorPackFeature+" info", survivorInfoCmd, a, p)
	if err != nil {
		b.add(err)
		return
	}
	st.overlay(info)
	due := survivorDue(group, day, st)
	slog.Info(survivorPackFeature+": state", "activity", a.ID, "day", day, "receiveIdArr", st.ids,
		"receiveFreeArr", st.free, "receiveScoreArr", st.score, "freeDue", due)
	for _, id := range due {
		r := survivorSend(b, a, survivorFreeCmd, "id", id)
		if r == nil {
			return
		}
		st.overlay(r)
		if !slices.Contains(st.ids, id) {
			r := survivorSend(b, a, survivorBubbleCmd, "id", id)
			if r == nil {
				return
			}
			st.overlay(r)
		}
	}
	count := survivorItemCount(in, survivorScoreItem)
	for _, idx := range survivorScoresDue(scores, count, st.score) {
		if survivorSend(b, a, survivorScoreCmd, "index", idx) == nil {
			return
		}
	}
}

// survivorSend sends one claim {activityId, key: v} and logs the reply; nil when it failed.
func survivorSend(b *evBatch, a Activity, cmd, key string, v int64) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	p.PutInt("activityId", a.ID)
	p.PutInt(key, int32(v))
	r := b.send(survivorPackFeature+": "+cmd, cmd, p)
	if r == nil {
		return nil
	}
	evTaskLogReply(survivorPackFeature+": "+cmd, cmd, r.Params, "activity", a.ID, key, v)
	return r.Params
}

// survivorDue lists today's survivors (group, day <= actOpenDay) whose free reward is unclaimed.
func survivorDue(group string, day int64, st survivorState) []int64 {
	var due []int64
	for _, r := range survivorList {
		if r.group == group && r.day <= day && !slices.Contains(st.free, r.id) {
			due = append(due, r.id)
		}
	}
	return due
}

// survivorScoresDue lists the 0-based bar stages count reaches that receiveScoreArr lacks.
func survivorScoresDue(scores []int64, count int64, claimed []int64) []int64 {
	var due []int64
	for i, sc := range scores {
		if count >= sc && !slices.Contains(claimed, int64(i)) {
			due = append(due, int64(i))
		}
	}
	return due
}

// survivorItemCount is the bag count of item id in init items (the largest stack, as GetItemById
// returns one ItemInfo per id).
func survivorItemCount(in *Init, id string) int64 {
	var n int64
	for _, it := range claimObjectsOrMap(in.Raw, "items") {
		got := evString(it, "itemId")
		if got == "" {
			got = evString(it, "goodsId")
		}
		if got != id {
			continue
		}
		// ItemInfo:UpdateInfo applies count, num, then itemLeftCnt: the last present one is the
		// stack's count (ItemInfo.lua:52-66).
		if c, ok := claimFirstInt(it, "itemLeftCnt", "num", "count"); ok {
			n = max(n, c)
		}
	}
	return n
}
