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

// EnumActivity type numbers (Global/EnumType.lua:6260 onwards) the event features act on. An
// activity's type is not on the wire: the client looks it up in table `activity` by id
// (ActivityInfoData.FetchActivityConfigData, ActivityInfoData.lua:218-241), and so does
// activityTypeOf below.
const (
	actTypePersonalArms     int32 = 12
	actTypeAllianceCompete  int32 = 14
	actTypeBattlePass       int32 = 27
	actTypeSevenDay         int32 = 34
	actTypeLuckyRoll        int32 = 124
	actTypePersonalArmsNew  int32 = 125
	actTypeGiftBox          int32 = 132
	actTypeLuckyShop        int32 = 134
	actTypeBattlePassNew    int32 = 142
	actTypeCooking          int32 = 150
	actTypeBanquet          int32 = 151
	actTypeMonopoly         int32 = 153
	actTypeBargainShop      int32 = 155
	actTypeBlueStore        int32 = 156
	actTypeSignIn           int32 = 158
	actTypeSlotMachine      int32 = 159
	actTypeDecorationGacha  int32 = 173
	actTypeSevenDayV2       int32 = 174
	actTypeWinterStorm      int32 = 218
	actTypeTorchRelay       int32 = 309
	actTypeHeroMonthCard    int32 = 313
	actTypeBountyHunter     int32 = 350
	actTypeSurvivalVipGift  int32 = 380
	actTypeMineCave         int32 = 25
	actTypeArena            int32 = 26
	actTypeTruckActivity    int32 = 110
	actTypeTrainActivity    int32 = 203
	activityHQBuildingID    int32 = 10100000 // BuildingTypes.FUN_BUILD_MAIN (EnumType.lua:3481)
	activityOpenToleranceMs int64 = 1000     // CheckIsSend's timeDevintion
)

// evNow is the clock the event features read; tests replace it.
var evNow = time.Now

// Activity is one running event from the init push's `activity` map (ActivityListDataManager:
// InitActivityListData, ActivityListDataManager.lua:100-124). Each entry carries the id and its
// window (startTime, endTime or lastTime, endViewTime, all ms); Type and NeedHQ come from the
// 1.0.364 table, keyed by id.
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

// IDString is the id as hero.event.info.get wants it (UtfString).
func (a Activity) IDString() string { return strconv.FormatInt(int64(a.ID), 10) }

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
// key. Ids the 1.0.364 table doesn't know are skipped: their type, and so the right detail and
// claim commands, are unknown.
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
	var unknown []int32
	for _, e := range entries {
		id, ok := activityEntryID(e.obj, e.key)
		if !ok {
			continue
		}
		typ, known := activityTypeOf(id)
		if !known {
			unknown = append(unknown, id)
			continue
		}
		out = append(out, Activity{ID: id, Type: typ, NeedHQ: activityNeedHQ[id], Raw: e.obj})
	}
	slices.SortFunc(out, func(a, b Activity) int { return int(a.ID) - int(b.ID) })
	if len(unknown) > 0 {
		slog.Debug("activity sweep: ids outside the event features' types", "count", len(unknown))
	}
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
// string (the Lua tonumber()s them all alike). ok is false when it is absent or not a number.
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

// activityTypeOf returns the EnumActivity type of activity id from table `activity`.
func activityTypeOf(id int32) (int32, bool) {
	activityTypeOnce.Do(func() {
		activityTypeByID = make(map[int32]int32)
		for typ, ids := range activityIDsByType {
			for _, id := range ids {
				activityTypeByID[id] = typ
			}
		}
	})
	t, ok := activityTypeByID[id]
	return t, ok
}

var (
	activityTypeOnce sync.Once
	activityTypeByID map[int32]int32
)

// activityIDsByType and activityNeedHQ are generated from table `activity` in the 1.0.364 tables
// (39432; identical in live 39516 per coverage/reports/tables.md): the id, type and needMainCityLevel
// columns, for the types the event features handle (types: 14,27,34,124,125,132,134,142,150,151,153,155,156,158,159,173,174,218,309,313,350,380).
// Regenerate after a table update that adds event ids.
var activityIDsByType = map[int32][]int32{
	14: {55000},
	27: {
		13, 19, 2001, 2002, 2003, 2004, 2005, 2006, 2007, 2008, 2009, 2010,
		2011, 2101, 3014, 3015, 4003, 4006, 4008, 4009, 4010, 4011, 4012, 4013,
		5001, 5002, 5004, 5006, 5008, 5011, 5012, 5013, 5014, 5015, 5018, 5019,
		5023, 5025, 5026, 5027, 5028, 5034, 5035, 5040, 5092, 5094, 14004, 50341,
		50342, 91001, 98004, 98007, 98502, 98506, 98510, 99022, 99044, 99093, 99107, 99114,
		99164,
	},
	34: {3013, 94001, 95001, 95002, 95003, 95005, 99024, 99116},
	124: {
		14, 15, 1002, 1003, 1004, 1005, 4004, 4005, 4007, 5005, 5007, 5032,
		5033, 5036, 5037, 5041, 5106, 5133, 99017, 99032, 99056, 1000022, 1000024, 1000026,
		1000029, 1000031, 1010033, 1010035, 1010037, 1010039, 1010041, 1010043, 1020002, 1020006, 1020010, 1031002,
		1031004, 1031006, 1041002, 1041004, 1041006, 1051002, 1051004, 1051006,
	},
	125: {17, 20, 24, 26, 27, 29},
	132: {
		98001, 98005, 98008, 98009, 98010, 98011, 98012, 98013, 98111, 1000217, 1000218, 1000219,
		1000231, 1000232, 1000265, 1000504, 1000513, 1010217, 1010218, 1010219, 1010220, 1020213, 1030213, 1030232,
		1030238, 1030239, 1030240, 1030241,
	},
	134: {7001},
	142: {
		5038, 5039, 5405, 70002, 70003, 70004, 70005, 70006, 70007, 70008, 70009, 70010,
		70011, 70012, 70013, 70014, 70015, 70016, 70017, 70018, 70019, 70020, 70021, 70022,
		70023, 70024, 70025, 70026, 70027, 70028, 70029, 70030, 70031, 70032, 98512, 99002,
		99013, 99021, 99029, 99043, 99049, 99053, 99060, 99062, 99074, 99075, 99083, 99091,
		99098, 99105, 99109, 99123, 99133, 99140, 99148, 99158, 99173, 99184, 99188, 99198,
		1000001, 1000011, 1000023, 1000025, 1000027, 1000028, 1000030, 1000033, 1000066, 1000235, 1000236, 1000237,
		1000238, 1000239, 1000240, 1000241, 1000242, 1000243, 1000260, 1000277, 1000278, 1000279, 1000280, 1000281,
		1000283, 1000284, 1000310, 1000311, 1000317, 1000320, 1000321, 1000322, 1000506, 1000507, 1000508, 1000514,
		1000607, 1010032, 1010034, 1010036, 1010038, 1010040, 1010042, 1010245, 1010246, 1010247, 1010248, 1010250,
		1010251, 1010252, 1010253, 1010255, 1010256, 1010257, 1010258, 1010295, 1010296, 1010297, 1010298, 1010300,
		1010301, 1010302, 1020001, 1020005, 1020009, 1020217, 1020218, 1020219, 1020220, 1030217, 1030218, 1030219,
		1030220, 1031001, 1031003, 1031005, 1041001, 1041003, 1041005, 1041015, 1041016, 1041017, 1041018, 1051001,
		1051003, 1051005, 1051015, 1051016, 1051017, 1051018, 1200006, 1200032, 1200050, 1200110, 10202181,
	},
	150: {99003},
	151: {99004, 99014, 99030, 99054, 99084, 99099, 99126, 99149},
	153: {99011, 99026, 99087, 99102, 99120, 99159, 99174},
	155: {99020, 99040, 99067, 99115, 99183},
	156: {
		5301, 5302, 5303, 5304, 5305, 5306, 5307, 5308, 5309, 5310, 1000220, 1000221,
		1000222, 1000262, 1000268, 1000269, 1000270, 1000316, 1000505, 1000516, 1010230, 1010231, 1010232, 1010233,
		1020214, 1030214, 1030233, 1030261, 1030262, 1030263, 1030264, 1041011, 1041012, 1051011, 1051012,
	},
	158: {5401, 5402, 5403, 5404, 5411, 5412, 5413, 99193, 99194},
	159: {99047, 99050, 99070, 99071, 99080, 99095, 99130, 99138, 99145, 99186, 99196},
	173: {98629, 98630, 98631, 98632, 98633, 98634, 98635, 98636, 98637},
	174: {95101, 95102, 95103, 98672, 98673, 98674, 98675, 98676, 98677, 99180},
	218: {40039},
	309: {99086, 99101, 99113, 99162},
	313: {
		98704, 98705, 98706, 98707, 98708, 98709, 98710, 98711, 98712, 98713, 98714, 98715,
		98716, 98717,
	},
	350: {
		98800, 98802, 98803, 98804, 98805, 98806, 1030242, 1030251, 1030252, 1030253, 1030254, 1041009,
		1041010, 1051009, 1051010, 10202131, 10202171, 10202191, 10205041,
	},
	380: {98801},
}

var activityNeedHQ = map[int32]int32{
	20: 5, 24: 7, 26: 7, 27: 7, 29: 7, 3013: 4, 40039: 10, 50341: 4,
	50342: 4, 55000: 10, 94001: 4, 95001: 4, 95002: 4, 95003: 4, 95005: 4, 95101: 4,
	95102: 4, 95103: 4, 98672: 4, 98673: 4, 98674: 4, 98675: 4, 98676: 4, 98677: 4,
	99024: 4, 99116: 4,
}
