package game

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// assistInit is an HQ-10 init with DispatchTask activity 94101, in an alliance when allied is set.
func assistInit(allied bool) *Init {
	return rewardInitWithDay(10, func(raw *sfs.SFSObject) {
		acts := sfs.NewSFSObject()
		acts.PutSFSObject("94101", sfs.NewSFSObject())
		raw.PutSFSObject("activity", acts)
		if allied {
			raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}
	})
}

// assistList is a hero.dispatch.alliance.list reply: ls[] entries {uuid, cfgId, completionTime,
// rewarded, targetServer} (DispatchGetAllianceTasksMessage.lua:9-17, DispatchTaskItem.lua:486).
func assistList(now time.Time) *sfs.SFSObject {
	ls := sfs.NewSFSArray()
	for _, e := range []struct {
		done     time.Duration
		rewarded int
	}{{-time.Hour, 0}, {-time.Minute, 0}, {-time.Hour, 1}, {time.Hour, 0}} {
		ls.AddSFSObject(duelObj("uuid", int64(100), "cfgId", 500101, "completionTime", now.Add(e.done).UnixMilli(),
			"rewarded", e.rewarded, "targetServer", 1234))
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("ls", ls)
	return r
}

func TestSecretTaskAssistPlanOnlyReads(t *testing.T) {
	f, ok := featureRegistry[secretTaskAssistPlanFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("%s registered as %+v; want an opt-in read-only plan", secretTaskAssistPlanFeature, f)
	}
	now := time.Now()
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case secretTasksListCmd:
			return duelObj("todayAssistNum", 4)
		case secretTaskAllianceListCmd:
			return assistList(now)
		}
		return rewardErr("999999")
	})
	if err := runSecretTaskAssistPlan(conn, assistInit(true)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{secretTasksListCmd, secretTaskAllianceListCmd}) {
		t.Errorf("sent %v, want only the two lists (never an assist)", got)
	}
	// The map gate: finished (completionTime in the past) and not rewarded.
	n := 0
	for _, e := range claimObjects(assistList(now), "ls") {
		if secretTaskAssistable(e, now.UnixMilli()) {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d assistable entries, want 2", n)
	}
}

func TestSecretTaskAssistPlanGates(t *testing.T) {
	for name, in := range map[string]*Init{
		"no alliance": assistInit(false),
		"no activity": rewardInitWithDay(10, func(raw *sfs.SFSObject) { raw.PutSFSObject("alliance", duelObj("uid", "a1")) }),
		"HQ 4": rewardInitWithDay(4, func(raw *sfs.SFSObject) {
			acts := sfs.NewSFSObject()
			acts.PutSFSObject("94101", sfs.NewSFSObject())
			raw.PutSFSObject("activity", acts)
			raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}),
	} {
		conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
		if err := runSecretTaskAssistPlan(conn, in); err != nil || len(fake.cmds()) != 0 {
			t.Errorf("%s: sent %v (err %v), want nothing", name, fake.cmds(), err)
		}
	}
}

// The assist tests use the client's literals: the commands (MsgDefines DispatchGetTasks,
// DispatchGetAllianceTasks, DispatchAssist), the ls[] field names (ActDispatchTaskDataManager.lua:
// 550-557; DispatchTaskItem.lua:342-372, 487), aid_count 5 (lw_dispatch_settings, all 123 rows,
// tables 40413) and the lw_dispatch_tasks colours of the cfgIds below (tables 40413).
const (
	assistOwnListCmd  = "hero.dispatch.list"
	assistAllyListCmd = "hero.dispatch.alliance.list"
	assistSendCmd     = "hero.dispatch.assist"
	assistOwnUID      = "777"

	cfgSR  = 300101   // lw_dispatch_tasks 300101: color 3
	cfgSSR = 400101   // 400101: color 4
	cfgUR  = 500101   // 500101: color 5
	cfgNew = 99999999 // no lw_dispatch_tasks row
)

// assistTestInit is assistInit(true) with the player's uid in init user.
func assistTestInit() *Init {
	return rewardInitWithDay(10, func(raw *sfs.SFSObject) {
		acts := sfs.NewSFSObject()
		acts.PutSFSObject("94101", sfs.NewSFSObject())
		raw.PutSFSObject("activity", acts)
		raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		raw.PutSFSObject("user", duelObj("uid", assistOwnUID))
	})
}

// assistEntry is one hero.dispatch.alliance.list ls[] entry; a negative targetServer or rewarded
// leaves that field out.
func assistEntry(uuid int64, cfgID int, done int64, rewarded, server int) *sfs.SFSObject {
	e := duelObj("uuid", uuid, "cfgId", cfgID, "completionTime", done)
	if rewarded >= 0 {
		e.PutInt("rewarded", int32(rewarded))
	}
	if server >= 0 {
		e.PutInt("targetServer", int32(server))
	}
	return e
}

func assistListReply(entries ...*sfs.SFSObject) *sfs.SFSObject {
	ls := sfs.NewSFSArray()
	for _, e := range entries {
		ls.AddSFSObject(e)
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("ls", ls)
	return r
}

// assistOwnReply is a hero.dispatch.list reply: todayAssistNum (left out when negative) and one own
// task, uuid 900.
func assistOwnReply(today int) *sfs.SFSObject {
	r := assistListReply(duelObj("uuid", int64(900), "cfgId", cfgUR, "completionTime", int64(0), "rewarded", 0))
	if today >= 0 {
		r.PutInt("todayAssistNum", int32(today))
	}
	return r
}

// assistServer answers the two lists and each assist with assist(n, params), n counting from 1.
func assistServer(t *testing.T, own, ally *sfs.SFSObject, assist func(n int, p *sfs.SFSObject) *sfs.SFSObject) (*rewardFake, func() error) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	conn, fake := startRewardFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case assistOwnListCmd:
			return own
		case assistAllyListCmd:
			return ally
		case assistSendCmd:
			mu.Lock()
			n++
			k := n
			mu.Unlock()
			return assist(k, p)
		}
		return rewardErr("999999")
	})
	return fake, func() error { return runSecretTaskAssist(conn, assistTestInit()) }
}

// stubAssistSleep records the assist pauses instead of sleeping.
func stubAssistSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	orig := secretTaskAssistSleep
	secretTaskAssistSleep = func(d time.Duration) { slept = append(slept, d) }
	t.Cleanup(func() { secretTaskAssistSleep = orig })
	return &slept
}

// assistSent returns the uuid and targetServer of every assist request, failing on any other key or
// wire type than the client's PutLong("uuid") and PutInt("targetServer") (DispatchAssistMessage.lua:
// 7-8).
func assistSent(t *testing.T, fake *rewardFake) (uuids []int64, servers []int32) {
	t.Helper()
	for _, r := range fake.requests() {
		if r.Cmd != assistSendCmd {
			continue
		}
		keys := slices.DeleteFunc(slices.Sorted(slices.Values(r.Params.Keys())), func(k string) bool { return k == "_id" })
		if !slices.Equal(keys, []string{"targetServer", "uuid"}) {
			t.Errorf("assist carries %v, want only targetServer and uuid", keys)
		}
		u, _ := r.Params.Get("uuid")
		s, _ := r.Params.Get("targetServer")
		if u.Type != sfs.SFSLong || s.Type != sfs.SFSInt {
			t.Errorf("assist uuid type %d, targetServer type %d; want Long (%d) and Int (%d)", u.Type, s.Type, sfs.SFSLong, sfs.SFSInt)
			continue
		}
		uuids = append(uuids, u.Val.(int64))
		servers = append(servers, s.Val.(int32))
	}
	return uuids, servers
}

func TestSecretTaskAssistRegisteredOptIn(t *testing.T) {
	f, ok := featureRegistry["secret-task-assist"]
	if !ok {
		t.Fatal("secret-task-assist is not registered")
	}
	if f.DefaultOn || f.Hold || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("secret-task-assist registered as %+v; want opt-in, no duel hold, a static-only summary", f)
	}
}

// TestSecretTaskAssistAssistsAlliesInClientOrder: of a list with every kind of entry, only other
// owners' finished, unrewarded entries with all fields and a known cfgId are assisted, UR first and
// the earliest finished first within a colour, each with its own targetServer, 1 s apart, at most
// 5 - todayAssistNum, and nothing but the two lists and the assists is sent.
func TestSecretTaskAssistAssistsAlliesInClientOrder(t *testing.T) {
	slept := stubAssistSleep(t)
	logs := captureRewardLogs(t)
	now := time.Now()
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixMilli() }
	ownedByMe := assistEntry(1009, cfgUR, ago(time.Hour), 0, 1309)
	ownedByMe.PutUtfString("ownerId", assistOwnUID)
	ally := assistListReply(
		assistEntry(1001, cfgSR, ago(3*time.Hour), 0, 1305),
		assistEntry(1002, cfgUR, ago(time.Hour), 0, 1302),
		assistEntry(1003, cfgSSR, ago(2*time.Hour), 0, 1303),
		assistEntry(1004, cfgUR, ago(2*time.Hour), 0, 1301),
		assistEntry(1005, cfgUR, ago(time.Hour), 1, 1306),                 // rewarded
		assistEntry(1006, cfgUR, now.Add(time.Hour).UnixMilli(), 0, 1307), // still running
		assistEntry(1007, cfgUR, 0, 0, 1308),                              // not started
		assistEntry(900, cfgUR, ago(time.Hour), 0, 1310),                  // the own list's task
		ownedByMe,
		assistEntry(1010, cfgUR, ago(time.Hour), 0, -1),    // no targetServer
		assistEntry(1011, cfgUR, ago(time.Hour), -1, 1311), // no rewarded
		assistEntry(1012, cfgNew, ago(time.Hour), 0, 1312),
		assistEntry(1004, cfgUR, ago(2*time.Hour), 0, 1301), // listed twice
		assistEntry(1013, cfgSR, ago(4*time.Hour), 0, 1304),
	)
	fake, run := assistServer(t, assistOwnReply(1), ally, func(n int, _ *sfs.SFSObject) *sfs.SFSObject {
		r := rewardOK()
		r.PutInt("todayAssistNum", int32(1+n))
		return r
	})
	if err := run(); err != nil {
		t.Fatal(err)
	}
	want := []string{assistOwnListCmd, assistAllyListCmd, assistSendCmd, assistSendCmd, assistSendCmd, assistSendCmd}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	uuids, servers := assistSent(t, fake)
	if !slices.Equal(uuids, []int64{1004, 1002, 1003, 1013}) || !slices.Equal(servers, []int32{1301, 1302, 1303, 1304}) {
		t.Errorf("assisted %v on servers %v, want [1004 1002 1003 1013] on [1301 1302 1303 1304]", uuids, servers)
	}
	if !slices.Equal(*slept, []time.Duration{time.Second, time.Second, time.Second}) {
		t.Errorf("paused %v between assists, want 1s before each but the first", *slept)
	}
	if out := logs.String(); !strings.Contains(out, "first hero.dispatch.assist reply") || !strings.Contains(out, "level=INFO") {
		t.Errorf("the first assist reply is not logged at Info:\n%s", out)
	}
}

// TestSecretTaskAssistReadsTodayFromEachReply: the reply's todayAssistNum (UpdateTodayNum,
// DispatchAssistMessage.lua:22) ends the run at the cap although more tasks are eligible.
func TestSecretTaskAssistReadsTodayFromEachReply(t *testing.T) {
	stubAssistSleep(t)
	done := time.Now().Add(-time.Hour).UnixMilli()
	ally := assistListReply(assistEntry(1, cfgUR, done, 0, 7), assistEntry(2, cfgUR, done, 0, 7), assistEntry(3, cfgUR, done, 0, 7))
	fake, run := assistServer(t, assistOwnReply(3), ally, func(int, *sfs.SFSObject) *sfs.SFSObject {
		return duelObj("todayAssistNum", 5)
	})
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if uuids, _ := assistSent(t, fake); !slices.Equal(uuids, []int64{1}) {
		t.Errorf("assisted %v, want only [1]: the first reply says today's 5 assists are used", uuids)
	}
}

// TestSecretTaskAssistCountsWithoutTodayInReply: a reply without todayAssistNum still counts, so a
// run never sends more than 5 - todayAssistNum.
func TestSecretTaskAssistCountsWithoutTodayInReply(t *testing.T) {
	stubAssistSleep(t)
	done := time.Now().Add(-time.Hour).UnixMilli()
	ally := assistListReply(assistEntry(1, cfgUR, done, 0, 7), assistEntry(2, cfgUR, done, 0, 7), assistEntry(3, cfgUR, done, 0, 7))
	fake, run := assistServer(t, assistOwnReply(3), ally, func(int, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if uuids, _ := assistSent(t, fake); !slices.Equal(uuids, []int64{1, 2}) {
		t.Errorf("assisted %v, want [1 2] (5 - 3 left today)", uuids)
	}
}

// TestSecretTaskAssistBoundsRunByOwnCount: the own list's todayAssistNum bounds the run at 5 -
// todayAssistNum whatever the assist replies say. Replies whose todayAssistNum stays at the starting
// value or goes back (a stale count, or another meaning than the used count) cannot stretch a run
// to every eligible entry.
func TestSecretTaskAssistBoundsRunByOwnCount(t *testing.T) {
	stubAssistSleep(t)
	done := time.Now().Add(-time.Hour).UnixMilli()
	var entries []*sfs.SFSObject
	for uuid := range int64(8) {
		entries = append(entries, assistEntry(uuid+1, cfgUR, done, 0, 7))
	}
	for name, c := range map[string]struct {
		today, reply int
		want         []int64
	}{
		"3 used, replies stay at 3": {3, 3, []int64{1, 2}},
		"3 used, replies say 0":     {3, 0, []int64{1, 2}},
		"0 used, replies stay at 0": {0, 0, []int64{1, 2, 3, 4, 5}},
		"4 used, replies stay at 4": {4, 4, []int64{1}},
	} {
		fake, run := assistServer(t, assistOwnReply(c.today), assistListReply(entries...),
			func(int, *sfs.SFSObject) *sfs.SFSObject { return duelObj("todayAssistNum", c.reply) })
		if err := run(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if uuids, _ := assistSent(t, fake); !slices.Equal(uuids, c.want) {
			t.Errorf("%s: assisted %v, want %v (5 - %d left today)", name, uuids, c.want, c.today)
		}
	}
}

// TestSecretTaskAssistStopsOnFirstErrorCode: an errorCode (here 456225, the client's cap tip) stops
// the run with an error; nothing after it is sent.
func TestSecretTaskAssistStopsOnFirstErrorCode(t *testing.T) {
	stubAssistSleep(t)
	done := time.Now().Add(-time.Hour).UnixMilli()
	ally := assistListReply(assistEntry(1, cfgUR, done, 0, 7), assistEntry(2, cfgUR, done, 0, 7), assistEntry(3, cfgUR, done, 0, 7))
	fake, run := assistServer(t, assistOwnReply(0), ally, func(n int, _ *sfs.SFSObject) *sfs.SFSObject {
		if n == 2 {
			return rewardErr("456225")
		}
		return rewardOK()
	})
	err := run()
	if err == nil || !strings.Contains(err.Error(), "456225") {
		t.Errorf("err = %v, want the errorCode", err)
	}
	if uuids, _ := assistSent(t, fake); !slices.Equal(uuids, []int64{1, 2}) {
		t.Errorf("assisted %v, want [1 2] and a stop at the refusal", uuids)
	}
}

// TestSecretTaskAssistFailsClosed: no assist without todayAssistNum in the own list, with today's 5
// used, or without ls in the alliance list; and nothing at all outside an alliance, without the
// DispatchTask activity or below HQ 5.
func TestSecretTaskAssistFailsClosed(t *testing.T) {
	stubAssistSleep(t)
	done := time.Now().Add(-time.Hour).UnixMilli()
	ally := assistListReply(assistEntry(1, cfgUR, done, 0, 7))
	for name, c := range map[string]struct{ own, ally *sfs.SFSObject }{
		"no todayAssistNum": {assistOwnReply(-1), ally},
		"5 used today":      {assistOwnReply(5), ally},
		"no ls":             {assistOwnReply(0), duelObj("todayAssistNum", 0)},
	} {
		fake, run := assistServer(t, c.own, c.ally, func(int, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
		if err := run(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if got := fake.cmds(); !slices.Equal(got, []string{assistOwnListCmd, assistAllyListCmd}) {
			t.Errorf("%s: sent %v, want only the two lists", name, got)
		}
	}
	for name, in := range map[string]*Init{
		"no alliance": assistInit(false),
		"no activity": rewardInitWithDay(10, func(raw *sfs.SFSObject) { raw.PutSFSObject("alliance", duelObj("uid", "a1")) }),
		"HQ 4": rewardInitWithDay(4, func(raw *sfs.SFSObject) {
			acts := sfs.NewSFSObject()
			acts.PutSFSObject("94101", sfs.NewSFSObject())
			raw.PutSFSObject("activity", acts)
			raw.PutSFSObject("alliance", duelObj("uid", "a1"))
		}),
	} {
		conn, fake := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return rewardOK() })
		if err := runSecretTaskAssist(conn, in); err != nil || len(fake.cmds()) != 0 {
			t.Errorf("%s: sent %v (err %v), want nothing", name, fake.cmds(), err)
		}
	}
}

// TestSecretTaskAssistTargetsSkipReasons pins each skip reason of the target filter.
func TestSecretTaskAssistTargetsSkipReasons(t *testing.T) {
	now := time.Now()
	done := now.Add(-time.Hour).UnixMilli()
	mine := assistEntry(6, cfgUR, done, 0, 7)
	mine.PutLong("ownerId", 777)
	ls := []*sfs.SFSObject{
		assistEntry(1, cfgUR, done, 0, 7),
		assistEntry(2, cfgUR, done, 0, -1),
		assistEntry(3, cfgNew, done, 0, 7),
		assistEntry(4, cfgUR, done, 0, 7),
		assistEntry(5, cfgUR, done, 1, 7),
		mine,
		assistEntry(1, cfgUR, done, 0, 7),
		assistEntry(7, cfgUR, done, 0, 0),
	}
	targets, skipped := secretTaskAssistTargets(ls, map[int64]bool{4: true}, assistOwnUID, now.UnixMilli())
	if len(targets) != 1 || targets[0].uuid != 1 {
		t.Errorf("targets %+v, want only uuid 1", targets)
	}
	want := map[string]int{"missingField": 2, "unknownCfgId": 1, "own": 2, "notFinishedOrRewarded": 1, "duplicate": 1}
	if !maps.Equal(skipped, want) {
		t.Errorf("skipped %v, want %v", skipped, want)
	}
}

// TestSecretTaskAssistColoursFromDispatchTable pins the lw_dispatch_tasks colours the assist order
// and the known-cfgId check read (secretTaskStartsCfgs; tables 40413: 328 rows).
func TestSecretTaskAssistColoursFromDispatchTable(t *testing.T) {
	if n := len(secretTaskStartsCfgs); n != 328 {
		t.Errorf("lw_dispatch_tasks has %d rows, want 328", n)
	}
	for id, color := range map[int64]int64{cfgSR: 3, cfgSSR: 4, cfgUR: 5, 60000701: 5, 70000403: 4, 302704: 3} {
		if got, ok := secretTaskStartsCfgs[id]; !ok || got.color != color {
			t.Errorf("cfgId %d: colour %d (known %v), want %d", id, got.color, ok, color)
		}
	}
	if _, ok := secretTaskStartsCfgs[cfgNew]; ok {
		t.Errorf("cfgId %d has a row; the unknown-cfgId fixture needs one without", cfgNew)
	}
}
