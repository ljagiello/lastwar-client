package game

import (
	"bytes"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// evTestNow is the fixed clock the event-feature tests run at: 12:00 UTC, ten hours into the
// server day that starts at 02:00 UTC (init tomorrow = evTestTomorrow).
var (
	evTestNow      = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	evTestTomorrow = time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC)
)

// withEvNow pins evNow for one test.
func withEvNow(t *testing.T, now time.Time) {
	t.Helper()
	saved := evNow
	evNow = func() time.Time { return now }
	t.Cleanup(func() { evNow = saved })
}

// evReq is one request the fake server saw.
type evReq struct {
	Cmd    string
	Params *sfs.SFSObject
}

// evFake is a scripted game server on an in-memory pipe: reply maps each request to its reply
// params, or nil to close the connection (a dead-connection failure for the client).
type evFake struct {
	mu   sync.Mutex
	reqs []evReq
}

func (f *evFake) requests() []evReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]evReq(nil), f.reqs...)
}

// cmds lists the commands the fake server received, in order.
func (f *evFake) cmds() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.Cmd)
	}
	return out
}

// only returns the requests for cmd.
func (f *evFake) only(cmd string) []evReq {
	var out []evReq
	for _, r := range f.requests() {
		if r.Cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

func startEvFake(t *testing.T, reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	client, server := session.NewPipeGameConnPair(t)
	f := &evFake{}
	go func() {
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, evReq{msg.Cmd, msg.Params})
			f.mu.Unlock()
			r := reply(msg.Cmd, msg.Params)
			if r == nil {
				_ = server.Close()
				return
			}
			if err := server.SendExtension(msg.Cmd, r); err != nil {
				return
			}
		}
	}()
	return client, f
}

// evOK is a plain success reply.
func evOK() *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutBool("success", true)
	return r
}

// evErr is an errorCode reply.
func evErr(code string) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutUtfString("errorCode", code)
	return r
}

// evActivity is an init `activity` entry open around evTestNow.
func evActivity(id int32) *sfs.SFSObject {
	a := sfs.NewSFSObject()
	a.PutInt("id", id)
	a.PutLong("startTime", evTestNow.Add(-48*time.Hour).UnixMilli())
	a.PutLong("endTime", evTestNow.Add(48*time.Hour).UnixMilli())
	a.PutLong("endViewTime", evTestNow.Add(72*time.Hour).UnixMilli())
	return a
}

// evInit builds an Init with init tomorrow, an HQ at hqLevel and the given activities, keyed by
// id like the live push.
func evInit(hqLevel int32, acts ...*sfs.SFSObject) *Init {
	raw := sfs.NewSFSObject()
	raw.PutLong("tomorrow", evTestTomorrow.Unix())
	m := sfs.NewSFSObject()
	for _, a := range acts {
		key := a.GetString("id")
		if key == "" {
			key = strconv.Itoa(int(a.GetInt("id")))
		}
		m.PutSFSObject(key, a)
	}
	raw.PutSFSObject("activity", m)
	return &Init{Raw: raw, Buildings: []Building{NewTestBuilding(1, activityHQBuildingID, hqLevel)}}
}

func TestParseActivitiesResolvesTypeAndWindow(t *testing.T) {
	withEvNow(t, evTestNow)
	strID := evActivity(0)
	strID.PutUtfString("id", "99047") // slot machine, id as UtfString
	lastTime := evActivity(5401)      // sign-in: lastTime overrides endTime
	lastTime.PutLong("lastTime", evTestNow.Add(-time.Hour).UnixMilli())
	unknown := evActivity(424242)
	in := evInit(30, evActivity(99011), strID, lastTime, unknown)

	acts := parseActivities(in)
	if len(acts) != 3 {
		t.Fatalf("parsed %d activities, want 3 (the unknown id is skipped): %+v", len(acts), acts)
	}
	if acts[0].ID != 5401 || acts[0].Type != actTypeSignIn || acts[1].ID != 99011 || acts[1].Type != actTypeMonopoly ||
		acts[2].ID != 99047 || acts[2].Type != actTypeSlotMachine {
		t.Errorf("activities = %+v", acts)
	}
	s := Activities(nil, in)
	open := s.Open()
	if len(open) != 2 || open[0].ID != 99011 || open[1].ID != 99047 {
		t.Errorf("Open() = %+v, want 99011 and 99047 (5401 ended an hour ago)", open)
	}
	if got := s.Open(actTypeSlotMachine); len(got) != 1 || got[0].ID != 99047 {
		t.Errorf("Open(slot) = %+v", got)
	}
}

// captureActivityLog routes slog to a buffer, Debug and up, for one test.
func captureActivityLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

func TestParseActivitiesLogsIDsMissingFromTable(t *testing.T) {
	withEvNow(t, evTestNow)
	logs := captureActivityLog(t)
	unknown := evActivity(424242)
	dragon := evActivity(40038) // ActDragon, type 201: in the table, asked for by no feature
	acts := parseActivities(evInit(30, unknown, dragon, evActivity(99011)))

	if len(acts) != 2 || acts[0].ID != 40038 || acts[0].Type != 201 || acts[0].NeedHQ != 15 || acts[1].ID != 99011 {
		t.Fatalf("activities = %+v, want 40038 (type 201, HQ 15) and 99011; the id missing from the table skipped", acts)
	}
	var info []string
	for _, l := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if strings.Contains(l, "level=INFO") {
			info = append(info, l)
		}
	}
	if len(info) != 1 {
		t.Fatalf("INFO lines = %q, want one, for 424242 only (a known id of an unhandled type stays quiet)", info)
	}
	for _, want := range []string{
		"id=424242",
		"startTime=" + strconv.FormatInt(unknown.GetLong("startTime"), 10),
		"endTime=" + strconv.FormatInt(unknown.GetLong("endTime"), 10),
		"tableVersion=" + activityTableVersion,
	} {
		if !strings.Contains(info[0], want) {
			t.Errorf("INFO line %q lacks %q", info[0], want)
		}
	}
	if !strings.Contains(logs.String(), "level=DEBUG msg=\"activity sweep\" parsed=2 notInTable=1") {
		t.Errorf("missing the Debug summary:\n%s", logs)
	}
}

func TestParseActivitiesLogsLastTimeAsEnd(t *testing.T) {
	withEvNow(t, evTestNow)
	logs := captureActivityLog(t)
	a := evActivity(424243)
	end := evTestNow.Add(time.Hour).UnixMilli()
	a.PutLong("lastTime", end) // overrides endTime, as ActivityInfoData.lua:148-150
	if got := parseActivities(evInit(30, a)); len(got) != 0 {
		t.Fatalf("parsed %+v, want the id missing from the table skipped", got)
	}
	if !strings.Contains(logs.String(), "endTime="+strconv.FormatInt(end, 10)) {
		t.Errorf("INFO must log the lastTime end:\n%s", logs)
	}
}

func TestParseActivitiesKnownIDsStayQuiet(t *testing.T) {
	withEvNow(t, evTestNow)
	logs := captureActivityLog(t)
	if got := parseActivities(evInit(30, evActivity(5401), evActivity(40038))); len(got) != 2 {
		t.Fatalf("parsed %+v, want both", got)
	}
	if strings.Contains(logs.String(), "level=INFO") {
		t.Errorf("known ids must not log at INFO:\n%s", logs)
	}
}

// handMapTypeCounts are the per-type id counts of the hand-copied activityIDsByType this table
// replaced (table 39432, the 33 types the event features handled), and handMapSamples one of its
// ids per type. Every later table must keep them: a table that drops or retypes one of these ids
// changes what an ON feature sees.
var (
	handMapTypeCounts = map[int32]int{
		14: 1, 27: 61, 34: 8, 124: 44, 125: 6, 130: 14, 132: 28, 134: 1, 139: 15, 142: 155, 150: 1, 151: 8,
		153: 7, 154: 6, 155: 5, 156: 35, 158: 9, 159: 11, 173: 9, 174: 10, 202: 5, 218: 1, 239: 1, 241: 1,
		248: 6, 260: 6, 309: 4, 350: 19, 373: 1, 380: 1, 389: 1, 392: 1, 412: 1,
	}
	handMapSamples = map[int32]int32{
		55000: 14, 13: 27, 99164: 27, 3013: 34, 14: 124, 1051006: 124, 17: 125, 29: 125, 3009: 130, 98001: 132,
		7001: 134, 1000289: 139, 5038: 142, 10202181: 142, 99003: 150, 99004: 151, 99011: 153, 99010: 154,
		99020: 155, 5301: 156, 5401: 158, 99194: 158, 99047: 159, 98629: 173, 95101: 174, 94301: 202,
		40039: 218, 1000058: 239, 1000070: 241, 1000079: 248, 80052: 260, 99086: 309, 98800: 350,
		10205041: 350, 1210000: 373, 98801: 380, 1200101: 389, 1200104: 392, 80152: 412,
	}
	// handMapNeedHQ is the hand-copied activityNeedHQ, verbatim: every nonzero needMainCityLevel
	// among those ids. The rest of them had none.
	handMapNeedHQ = map[int32]int32{
		20: 5, 24: 7, 26: 7, 27: 7, 29: 7, 3013: 4, 40039: 10, 50341: 4,
		50342: 4, 55000: 10, 94001: 4, 95001: 4, 95002: 4, 95003: 4, 95005: 4, 95101: 4,
		95102: 4, 95103: 4, 98672: 4, 98673: 4, 98674: 4, 98675: 4, 98676: 4, 98677: 4,
		99024: 4, 99116: 4, 1210000: 4,
	}
)

func TestActivityTableKeepsTheHandMap(t *testing.T) {
	counts := map[int32]int{}
	hqByType := map[int32]map[int32]int32{}
	for id, r := range activityTable {
		counts[r.typ]++
		if r.needHQ != 0 {
			if hqByType[r.typ] == nil {
				hqByType[r.typ] = map[int32]int32{}
			}
			hqByType[r.typ][id] = r.needHQ
		}
	}
	for typ, n := range handMapTypeCounts {
		if counts[typ] < n {
			t.Errorf("type %d: %d ids in the table, the hand map had %d", typ, counts[typ], n)
		}
	}
	for id, typ := range handMapSamples {
		if got, ok := activityTypeOf(id); !ok || got != typ {
			t.Errorf("activityTypeOf(%d) = %d, %v; the hand map had type %d", id, got, ok, typ)
		}
	}
	for id, lv := range handMapNeedHQ {
		if got := activityNeedHQ(id); got != lv {
			t.Errorf("activityNeedHQ(%d) = %d, the hand map had %d", id, got, lv)
		}
	}
	// No other id of the handled types gained an HQ gate (39879 added none).
	for typ := range handMapTypeCounts {
		for id, lv := range hqByType[typ] {
			if handMapNeedHQ[id] != lv {
				t.Errorf("activity %d (type %d): needHQ %d, the hand map had %d", id, typ, lv, handMapNeedHQ[id])
			}
		}
	}
	if r, ok := activityTable[99202]; !ok || r != (activityRow{typ: actTypeSignIn}) {
		t.Errorf("99202 = %+v, %v; want the 2026 Halloween sign-in, type 158 with no HQ gate (new in 39879)", r, ok)
	}
	if activityNeedHQ(424242) != 0 {
		t.Error("an id the table lacks must have no HQ gate")
	}
}

func TestActivityTypesOtherFeaturesUseAreInTheTable(t *testing.T) {
	for _, typ := range []int32{actTypeSeasonPeriodicCard, actTypeGhostRecon} {
		n := 0
		for _, r := range activityTable {
			if r.typ == typ {
				n++
			}
		}
		if n == 0 {
			t.Errorf("type %d has no activity rows", typ)
		}
	}
}

func TestEvNumKeepsLongStringsExact(t *testing.T) {
	o := sfs.NewSFSObject()
	o.PutUtfString("uuid", "9007199254740993") // 2^53 + 1: float64 rounds it to 2^53
	o.PutUtfString("neg", "-9007199254740993")
	o.PutUtfString("float", "12.0")
	o.PutUtfString("frac", "12.9")
	o.PutUtfString("word", "abc")
	o.PutLong("long", 9007199254740993)
	cases := []struct {
		key  string
		want int64
		ok   bool
	}{
		{"uuid", 9007199254740993, true},
		{"neg", -9007199254740993, true},
		{"float", 12, true},
		{"frac", 12, true},
		{"word", 0, false},
		{"long", 9007199254740993, true},
		{"absent", 0, false},
	}
	for _, c := range cases {
		if got, ok := evNum(o, c.key); got != c.want || ok != c.ok {
			t.Errorf("evNum(%s) = %d, %v; want %d, %v", c.key, got, ok, c.want, c.ok)
		}
	}
}

func TestActivityOpenGate(t *testing.T) {
	now := evTestNow.UnixMilli()
	mk := func(start, end int64, typ, need int32) Activity {
		raw := sfs.NewSFSObject()
		raw.PutLong("startTime", start)
		raw.PutLong("endTime", end)
		return Activity{ID: 1, Type: typ, NeedHQ: need, Raw: raw}
	}
	cases := []struct {
		name string
		a    Activity
		hq   int32
		want bool
	}{
		{"inside", mk(now-1, now+1, actTypeMonopoly, 0), 1, true},
		{"starts within the 1s tolerance", mk(now+1000, now+5000, actTypeMonopoly, 0), 1, true},
		{"not started", mk(now+1001, now+5000, actTypeMonopoly, 0), 1, false},
		{"ended within the tolerance", mk(now-5000, now-1000, actTypeMonopoly, 0), 1, true},
		{"ended", mk(now-5000, now-1001, actTypeMonopoly, 0), 1, false},
		{"HQ too low", mk(now-1, now+1, actTypeAllianceCompete, 10), 9, false},
		{"HQ reached", mk(now-1, now+1, actTypeAllianceCompete, 10), 10, true},
		{"survival VIP gift with no end", mk(now-5000, 0, actTypeSurvivalVipGift, 0), 1, true},
		{"other type with no end", mk(now-5000, 0, actTypeMonopoly, 0), 1, false},
	}
	for _, c := range cases {
		if got := c.a.open(now, c.hq); got != c.want {
			t.Errorf("%s: open = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestActivitiesNeedHQFromTable(t *testing.T) {
	withEvNow(t, evTestNow)
	in := evInit(9, evActivity(55000)) // Alliance Duel: needMainCityLevel 10
	if got := Activities(nil, in).Open(); len(got) != 0 {
		t.Errorf("HQ 9 opened %+v; table needMainCityLevel is 10", got)
	}
	in = evInit(10, evActivity(55000))
	if got := Activities(nil, in).Open(); len(got) != 1 || got[0].NeedHQ != 10 {
		t.Errorf("HQ 10: Open() = %+v", got)
	}
}

func TestActivityIDStringPrefersActivityid(t *testing.T) {
	acts := parseActivities(evInit(30, duelActivity(), evActivity(5401)))
	got := map[int32]string{}
	for _, a := range acts {
		got[a.ID] = a.IDString()
	}
	if got[55000] != "70000" {
		t.Errorf("duel IDString = %q, want the entry's activityid \"70000\" (ActivityInfoData.lua:160-161)", got[55000])
	}
	if got[5401] != "5401" {
		t.Errorf("an entry without activityid must use its id, got %q", got[5401])
	}
}

func TestActivitiesArrayAndMissing(t *testing.T) {
	withEvNow(t, evTestNow)
	raw := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()
	arr.AddSFSObject(evActivity(99011))
	arr.AddInt(5)
	raw.PutSFSArray("activity", arr)
	if got := parseActivities(&Init{Raw: raw}); len(got) != 1 || got[0].ID != 99011 {
		t.Errorf("array form: %+v", got)
	}
	if got := parseActivities(&Init{Raw: sfs.NewSFSObject()}); got != nil {
		t.Errorf("no activity key: %+v", got)
	}
	if got := parseActivities(nil); got != nil {
		t.Errorf("nil init: %+v", got)
	}
	var nilSweep *ActivitySweep
	if nilSweep.Open() != nil {
		t.Error("nil sweep must have no open activities")
	}
}

func TestActivitiesCachedPerConnAndInit(t *testing.T) {
	in := evInit(30, evActivity(99011))
	a, b := Activities(nil, in), Activities(nil, in)
	if a != b {
		t.Error("same (conn, init) must share one sweep")
	}
	if c := Activities(nil, evInit(30)); c == a {
		t.Error("a new init must get a new sweep")
	}
}

func TestEventInfoSendsUtfIDOnceAndCaches(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := sfs.NewSFSObject()
		r.PutInt("activityType", actTypeMonopoly)
		return r
	})
	in := evInit(30, evActivity(99011))
	s := Activities(conn, in)
	act := s.Open()[0]
	for range 2 {
		info, err := s.EventInfo(act)
		if err != nil || info.GetInt("activityType") != actTypeMonopoly {
			t.Fatalf("EventInfo = %v, %v", info, err)
		}
	}
	reqs := fake.only("hero.event.info.get")
	if len(reqs) != 1 || len(fake.requests()) != 1 {
		t.Fatalf("requests = %v, want one hero.event.info.get", fake.cmds())
	}
	v, _ := reqs[0].Params.Get("activityId")
	if v.Type != sfs.SFSUtfString || v.Val != "99011" {
		t.Errorf("activityId = %#v, want UtfString \"99011\"", v)
	}
}

func TestEventInfoErrorCodeAndDeadConnection(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if p.GetString("activityId") == "99011" {
			return evErr("E000000")
		}
		return nil // close: the next request finds a dead connection
	})
	in := evInit(30, evActivity(99011), evActivity(99047), evActivity(99020))
	s := Activities(conn, in)
	open := s.Open()
	if _, err := s.EventInfo(open[0]); err == nil {
		t.Error("errorCode reply must be an error")
	}
	if _, err := s.EventInfo(open[1]); err == nil {
		t.Error("closed connection must be an error")
	}
	if _, err := s.EventInfo(open[2]); !session.ContainsNonTimeoutNetError(err) {
		t.Errorf("after a dead connection EventInfo = %v, want the dead-connection error", err)
	}
	if got := len(fake.requests()); got != 2 {
		t.Errorf("sent %d requests, want 2 (none after the connection died)", got)
	}
}

func TestEvClaimedToday(t *testing.T) {
	withEvNow(t, evTestNow)
	in := evInit(1)
	dayStart := evTestTomorrow.Add(-24 * time.Hour)
	cases := []struct {
		name string
		ts   int64
		want bool
	}{
		{"never", 0, false},
		{"today in ms", dayStart.Add(time.Minute).UnixMilli(), true},
		{"today in seconds", dayStart.Add(time.Minute).Unix(), true},
		{"yesterday in ms", dayStart.Add(-time.Minute).UnixMilli(), false},
		{"yesterday in seconds", dayStart.Add(-time.Minute).Unix(), false},
		{"host-clock today but server yesterday", time.Date(2026, 10, 5, 1, 0, 0, 0, time.UTC).UnixMilli(), false},
	}
	for _, c := range cases {
		got, ok := evClaimedToday(in, c.ts)
		if !ok || got != c.want {
			t.Errorf("%s: evClaimedToday = %v, %v; want %v", c.name, got, ok, c.want)
		}
	}
	if _, ok := evClaimedToday(&Init{Raw: sfs.NewSFSObject()}, 1); ok {
		t.Error("no tomorrow must report !ok")
	}
}

func TestEvBatchStopsOnDeadConnection(t *testing.T) {
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "a":
			return evOK()
		case "b":
			return evErr("boom")
		}
		return nil
	})
	b := &evBatch{conn: conn}
	if b.send("a", "a", sfs.NewSFSObject()) == nil {
		t.Error("success must return the reply")
	}
	if b.send("b", "b", sfs.NewSFSObject()) != nil {
		t.Error("an errorCode failure must return nil")
	}
	b.send("c", "c", sfs.NewSFSObject())
	b.send("d", "d", sfs.NewSFSObject())
	if got := strings.Join(fake.cmds(), ","); got != "a,b,c" {
		t.Errorf("sent %s, want a,b,c (nothing after the dead connection)", got)
	}
	if err := b.err(); err == nil || !b.dead {
		t.Errorf("err = %v, dead = %v", err, b.dead)
	}
	b2 := &evBatch{}
	b2.add(errors.New("x"))
	if b2.dead || b2.err() == nil {
		t.Error("a plain error must not mark the connection dead")
	}
}
