package game

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// EnumActivity type numbers (Global/EnumType.lua:6260-6489) the event features act on. An
// activity's type is not on the wire: the client looks it up in table `activity` by id
// (ActivityInfoData.FetchActivityConfigData, ActivityInfoData.lua:224-247), and so does
// activityTypeOf below.
const (
	actTypeAllianceCompete    int32 = 14
	actTypeMineCave           int32 = 25
	actTypeArena              int32 = 26
	actTypeBattlePass         int32 = 27
	actTypeSevenDay           int32 = 34
	actTypeTruckActivity      int32 = 110
	actTypeLuckyRoll          int32 = 124
	actTypeTreasureHunt       int32 = 130
	actTypeTreasureHuntNew    int32 = 139
	actTypePersonalArmsNew    int32 = 125
	actTypeGiftBox            int32 = 132
	actTypeLuckyShop          int32 = 134
	actTypeBattlePassNew      int32 = 142
	actTypeCooking            int32 = 150
	actTypeBanquet            int32 = 151
	actTypeMonopoly           int32 = 153
	actTypeActTask            int32 = 154
	actTypeBargainShop        int32 = 155
	actTypeBlueStore          int32 = 156
	actTypeSignIn             int32 = 158
	actTypeSlotMachine        int32 = 159
	actTypeDecorationGacha    int32 = 173
	actTypeSevenDayV2         int32 = 174
	actTypeAllyDrill          int32 = 202
	actTypeTrainActivity      int32 = 203
	actTypeWinterStorm        int32 = 218
	actTypeSeasonPeriodicCard int32 = 228 // SeasonPeriodicCard (EnumType.lua:6390), the season week card
	actTypeSeasonFarmer       int32 = 239
	actTypeDiggingGame        int32 = 241
	actTypeSeasonPreview      int32 = 248
	actTypeOffSeasonDig       int32 = 260
	actTypeGhostRecon         int32 = 308 // Ghostrecon (EnumType.lua:6405)
	actTypeTorchRelay         int32 = 309
	actTypeBountyHunter       int32 = 350
	actTypeMonthCardInsure    int32 = 373
	actTypeSurvivalVipGift    int32 = 380
	actTypeSeasonMilitary     int32 = 389
	actTypeCampScience        int32 = 392
	actTypeCrystalBoss        int32 = 412
	activityHQBuildingID      int32 = 10100000 // BuildingTypes.FUN_BUILD_MAIN (EnumType.lua:3481)
	activityOpenToleranceMs   int64 = 1000     // CheckIsSend's timeDevintion
)

// evNow is the clock the event features read: the game time (gameNow). Tests replace it.
var evNow = gameNow

// Activity is one running event from the init push's `activity` map (ActivityListDataManager:
// InitActivityListData, ActivityListDataManager.lua:100-124). Each entry carries the id and its
// window (startTime, endTime or lastTime, endViewTime, all ms); Type and NeedHQ come from table
// `activity` (activityTable, generated), keyed by id.
type Activity struct {
	ID     int32
	Type   int32
	NeedHQ int32
	Raw    *sfs.SFSObject
}

// StartTime is the activity's start in unix ms.
func (a Activity) StartTime() int64 { return a.Raw.GetLong("startTime") }

// EndTime is the activity's end in unix ms. ActivityInfoData:ParseActivityData lets lastTime
// override endTime (ActivityInfoData.lua:139-147).
func (a Activity) EndTime() int64 {
	if a.Raw.Has("lastTime") {
		return a.Raw.GetLong("lastTime")
	}
	return a.Raw.GetLong("endTime")
}

// EndViewTime is the end of the activity's display window in unix ms (0 when absent).
func (a Activity) EndViewTime() int64 { return a.Raw.GetLong("endViewTime") }

// IDString is the id as hero.event.info.get wants it (UtfString): the entry's activityid when it
// has one, else its id, as ActivityInfoData:ParseActivityData sets the activityId that
// AddOneActivity sends (ActivityInfoData.lua:126-128, 160-161; ActivityListDataManager.lua:162).
// Only the Alliance Duel's entry carries one live (id 55000, activityid "70000", its heroactivity
// row); asked for "55000" the server answers a bare {success=true} with no eventList.
func (a Activity) IDString() string {
	if v, ok := a.Raw.Get("activityid"); ok && v.Val != nil {
		if s := fmt.Sprintf("%v", v.Val); s != "" && s != "0" {
			return s
		}
	}
	return strconv.FormatInt(int64(a.ID), 10)
}

// open mirrors ActivityListDataManager:CheckIsSend (ActivityListDataManager.lua:954-966): the
// client fetches an activity's details only while now is within [startTime-1s, endTime+1s] (a few
// types with endTime 0 count as always open) and the HQ level reaches needMainCityLevel.
func (a Activity) open(nowMs int64, hq int32) bool {
	inWindow := nowMs >= a.StartTime()-activityOpenToleranceMs && nowMs <= a.EndTime()+activityOpenToleranceMs
	if a.EndTime() == 0 {
		switch a.Type {
		case actTypeMineCave, actTypeArena, actTypeTruckActivity, actTypeTrainActivity, actTypeSurvivalVipGift:
			inWindow = true
		}
	}
	return inWindow && hq >= a.NeedHQ
}

// ActivitySweep is one run's view of the open activities plus the detail replies fetched for
// them, so every event feature in a run shares one hero.event.info.get per activity, the way
// ActivityListDataManager:AddOneActivity sends it once at activity-list load
// (ActivityListDataManager.lua:154-932). Not safe for concurrent use; features run one at a time.
type ActivitySweep struct {
	conn    *session.GameConn
	in      *Init
	all     []Activity
	hq      int32
	replies map[string]activityReply
}

type activityReply struct {
	obj *sfs.SFSObject
	err error
}

var (
	activitySweepMu    sync.Mutex
	activitySweepCache *ActivitySweep
)

// Activities returns the activity sweep for this run, building it from in on first use. The cache
// holds one sweep, keyed by (conn, in): a new login or init replaces it.
func Activities(conn *session.GameConn, in *Init) *ActivitySweep {
	activitySweepMu.Lock()
	defer activitySweepMu.Unlock()
	if s := activitySweepCache; s != nil && s.conn == conn && s.in == in {
		return s
	}
	s := &ActivitySweep{conn: conn, in: in, all: parseActivities(in), hq: activityHQLevel(in), replies: map[string]activityReply{}}
	activitySweepCache = s
	return s
}

// parseActivities reads init `activity`. The client walks it with table.walk, so it accepts both
// an object keyed by id and an array; an entry's own `id` (Int, Long or UtfString) wins over its
// key. Every id table `activity` knows is kept, whatever its type (features pick theirs through
// Open and All). An id the generated table lacks is skipped, since its type and so the right
// detail and claim commands are unknown, and logged at Info: it is an event newer than the
// generated table, so the table needs regenerating.
func parseActivities(in *Init) []Activity {
	if in == nil || in.Raw == nil {
		return nil
	}
	v, ok := in.Raw.Get("activity")
	if !ok {
		return nil
	}
	type entry struct {
		key string
		obj *sfs.SFSObject
	}
	var entries []entry
	switch c := v.Val.(type) {
	case *sfs.SFSObject:
		for _, k := range c.Keys() {
			if ev, ok := c.Get(k); ok {
				if o, ok := ev.Val.(*sfs.SFSObject); ok {
					entries = append(entries, entry{k, o})
				}
			}
		}
	case *sfs.SFSArray:
		for _, it := range c.Items() {
			if o, ok := it.Val.(*sfs.SFSObject); ok {
				entries = append(entries, entry{"", o})
			}
		}
	default:
		slog.Warn("init activity is neither an object nor an array; skipping the activity sweep", "type", fmt.Sprintf("%T", v.Val))
		return nil
	}
	var out []Activity
	missing := 0
	for _, e := range entries {
		id, ok := activityEntryID(e.obj, e.key)
		if !ok {
			continue
		}
		typ, known := activityTypeOf(id)
		if !known {
			missing++
			a := Activity{ID: id, Raw: e.obj}
			slog.Info("activity sweep: id not in table activity; skipped until the table is regenerated",
				"id", id, "startTime", a.StartTime(), "endTime", a.EndTime(), "tableVersion", activityTableVersion)
			continue
		}
		out = append(out, Activity{ID: id, Type: typ, NeedHQ: activityNeedHQ(id), Raw: e.obj})
	}
	slices.SortFunc(out, func(a, b Activity) int { return int(a.ID) - int(b.ID) })
	slog.Debug("activity sweep", "parsed", len(out), "notInTable", missing)
	return out
}

func activityEntryID(o *sfs.SFSObject, key string) (int32, bool) {
	for _, k := range []string{"id", "activityid"} {
		v, ok := o.Get(k)
		if !ok {
			continue
		}
		switch n := v.Val.(type) {
		case string:
			if id, err := strconv.ParseInt(n, 10, 32); err == nil && id > 0 {
				return int32(id), true
			}
		default:
			if id := o.GetInt(k); id > 0 {
				return id, true
			}
		}
	}
	if id, err := strconv.ParseInt(key, 10, 32); err == nil && id > 0 {
		return int32(id), true
	}
	return 0, false
}

// activityHQLevel is DataCenter.BuildManager.MainLv: the HQ building's level, 0 when absent.
func activityHQLevel(in *Init) int32 {
	if in == nil {
		return 0
	}
	var lv int32
	for _, b := range in.Buildings {
		if b.BId() == activityHQBuildingID && b.Level() > lv {
			lv = b.Level()
		}
	}
	return lv
}

// Open returns the open activities (CheckIsSend) of the given types, by id; no types means all.
func (s *ActivitySweep) Open(types ...int32) []Activity {
	if s == nil {
		return nil
	}
	now := evNow().UnixMilli()
	var out []Activity
	for _, a := range s.all {
		if len(types) > 0 && !slices.Contains(types, a.Type) {
			continue
		}
		if a.open(now, s.hq) {
			out = append(out, a)
		}
	}
	return out
}

// All returns every parsed activity of the given types, open or not, by id.
func (s *ActivitySweep) All(types ...int32) []Activity {
	if s == nil {
		return nil
	}
	var out []Activity
	for _, a := range s.all {
		if len(types) == 0 || slices.Contains(types, a.Type) {
			out = append(out, a)
		}
	}
	return out
}

// Forget drops the cached cmd reply for a, so the next Info call fetches it again (after a claim
// changed it).
func (s *ActivitySweep) Forget(cmd string, a Activity) {
	delete(s.replies, cmd+"#"+a.IDString())
}

// EventInfo returns the activity's hero.event.info.get reply, the detail request
// ActivityListDataManager:AddOneActivity sends for most event types. Its activityId is a
// UtfString, unlike the Int every claim takes (ActivityEventInfoGetMessage.lua:4-6). A reply with
// errorCode set is an error.
func (s *ActivitySweep) EventInfo(a Activity) (*sfs.SFSObject, error) {
	params := sfs.NewSFSObject()
	params.PutUtfString("activityId", a.IDString())
	return s.Info("activity info response", "hero.event.info.get", a, params)
}

// Info sends a detail request for a once per sweep and caches the reply (or the failure) by cmd
// and activity id. A dead connection fails every later request without sending.
func (s *ActivitySweep) Info(label, cmd string, a Activity, params *sfs.SFSObject) (*sfs.SFSObject, error) {
	key := cmd + "#" + a.IDString()
	if r, ok := s.replies[key]; ok {
		return r.obj, r.err
	}
	for _, r := range s.replies {
		if r.err != nil && session.ContainsNonTimeoutNetError(r.err) {
			return nil, r.err
		}
	}
	msg, err := session.SendAndWait(s.conn, label, cmd, params)
	var r activityReply
	switch {
	case err != nil:
		r.err = fmt.Errorf("activity %d: %w", a.ID, err)
	case msg.Params.Has("errorCode"):
		r.err = fmt.Errorf("activity %d: %s returned errorCode=%v", a.ID, cmd, msg.Params.GetString("errorCode"))
	default:
		r.obj = msg.Params
	}
	s.replies[key] = r
	return r.obj, r.err
}

// evBatch runs one feature's sends: it keeps going past per-item failures, keeps every error, and
// stops sending once the connection is dead (like collectCore and RunFeatures).
type evBatch struct {
	conn *session.GameConn
	errs []error
	dead bool
}

// add records err; a non-timeout net error marks the connection dead.
func (b *evBatch) add(err error) {
	if err == nil {
		return
	}
	b.errs = append(b.errs, err)
	if session.ContainsNonTimeoutNetError(err) {
		b.dead = true
	}
}

// send sends one claim and returns its reply, or nil when it failed or the connection is dead. A
// benign errorCode (session.RegisterBenignErrorCode) returns the reply with no error recorded.
func (b *evBatch) send(label, cmd string, params *sfs.SFSObject) *session.ExtensionMessage {
	if b.dead {
		return nil
	}
	msg, err := session.SendAndWait(b.conn, label, cmd, params)
	if err != nil {
		b.add(err)
		return nil
	}
	return msg
}

func (b *evBatch) err() error { return errors.Join(b.errs...) }

// evUnix converts a server timestamp that may be seconds or milliseconds: the client stores some
// "last claimed" times in seconds (Monopoly, Bargain Shop, Blue Store multiply by 1000) and others
// in ms. Anything below 1e11 is taken as seconds (1e11 ms is 1973, 1e11 s is year 5138).
func evUnix(ts int64) time.Time {
	if ts < 1e11 {
		return time.Unix(ts, 0)
	}
	return time.UnixMilli(ts)
}

// evClaimedToday reports whether ts (s or ms) falls in the current server day, the client's
// UITimeManager:IsSameDayForServer(ts, now). A timestamp after today's start counts as today. ok
// is false when init carries no `tomorrow`; callers then skip the claim rather than guess the day.
func evClaimedToday(in *Init, ts int64) (claimed, ok bool) {
	start, ok := in.ServerDayStart(evNow())
	if !ok {
		return false, false
	}
	if ts <= 0 {
		return false, true
	}
	return !evUnix(ts).Before(start), true
}

// evNum reads a numeric field the server may send as any integer type, a double or a decimal
// string (the Lua tonumber()s them all alike). ok is false when it is absent or not a number. An
// integer string is parsed as one, so a Long uuid above 2^53 sent as a string keeps every digit
// (the flower-train uuid is kept as received and PutLong'd back, BaseSingleFlowerTrainData.lua:54,
// FlowerTrainReceiveRewardMessage.lua:5-8).
func evNum(o *sfs.SFSObject, key string) (int64, bool) {
	v, ok := o.Get(key)
	if !ok {
		return 0, false
	}
	switch n := v.Val.(type) {
	case int64, int32, int16, byte:
		return o.GetLong(key), true
	case float64:
		return int64(n), true
	case float32:
		return int64(n), true
	case string:
		if i, err := strconv.ParseInt(n, 10, 64); err == nil {
			return i, true
		}
		f, err := strconv.ParseFloat(n, 64)
		if err != nil {
			return 0, false
		}
		return int64(f), true
	}
	return 0, false
}

// evString reads a field the server may send as a string or a number, as a string.
func evString(o *sfs.SFSObject, key string) string {
	v, ok := o.Get(key)
	if !ok {
		return ""
	}
	if s, ok := v.Val.(string); ok {
		return s
	}
	if n, ok := evNum(o, key); ok {
		return strconv.FormatInt(n, 10)
	}
	return ""
}

// evNums reads a list of numbers sent as an SFSArray of numbers, a typed number array, or a
// string array of decimals; anything else in it is skipped.
func evNums(o *sfs.SFSObject, key string) []int64 {
	v, ok := o.Get(key)
	if !ok {
		return nil
	}
	var out []int64
	switch a := v.Val.(type) {
	case *sfs.SFSArray:
		for _, it := range a.Items() {
			e := sfs.NewSFSObject()
			e.PutValue("v", it)
			if n, ok := evNum(e, "v"); ok {
				out = append(out, n)
			}
		}
	case []int16:
		for _, n := range a {
			out = append(out, int64(n))
		}
	case []int32:
		for _, n := range a {
			out = append(out, int64(n))
		}
	case []int64:
		out = append(out, a...)
	case []string:
		for _, s := range a {
			if n, err := strconv.ParseInt(s, 10, 64); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// evObjects returns the object elements of the array under key, skipping anything else.
func evObjects(o *sfs.SFSObject, key string) []*sfs.SFSObject {
	v, ok := o.Get(key)
	if !ok {
		return nil
	}
	a, _ := v.Val.(*sfs.SFSArray)
	if a == nil {
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

// evObject returns the object under key, or nil.
func evObject(o *sfs.SFSObject, key string) *sfs.SFSObject {
	v, ok := o.Get(key)
	if !ok {
		return nil
	}
	e, _ := v.Val.(*sfs.SFSObject)
	return e
}

// evInitEffect is init effect[id] (EffectData:InitFromNet reads `effect` as id -> value,
// EffectData.lua:51-55, 254-257). The client's GetGameEffect adds alliance, city, server and
// season sources on top, so this is a lower bound.
func evInitEffect(in *Init, id int) int64 {
	n, _ := evNum(in.Object("effect"), strconv.Itoa(id))
	return n
}

//go:generate go run ./genactivities -in ${LASTWAR_TABLES}/activity.json -version ${LASTWAR_TABLE_VERSION} -out activity_tables_gen.go

// activityRow is one row of table `activity` as the client reads it in
// ActivityInfoData.FetchActivityConfigData: obj.type = tonumber(tabData.type) and
// obj.needMainCityLevel = tonumber(tabData.needMainCityLevel) or 0 (ActivityInfoData.lua:224-247).
// activityTable (activity_tables_gen.go) holds every row, whatever its type.
type activityRow struct {
	typ    int32
	needHQ int32
}

// activityTypeOf returns the EnumActivity type of activity id from table `activity`; false when
// the generated table lacks the id.
func activityTypeOf(id int32) (int32, bool) {
	r, ok := activityTable[id]
	return r.typ, ok
}

// activityNeedHQ returns the HQ level table `activity` requires for id (needMainCityLevel), 0 when
// the row has none or the table lacks the id.
func activityNeedHQ(id int32) int32 { return activityTable[id].needHQ }
