package game

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Duel days at evTestNow, with the 1.0.364 scoring ids.
const (
	duelTestHeroes      = "90101|90102|90103|90000"              // Thu: hero recruits (type 42)
	duelTestBase        = "90201|90202|90203|90204|90000|100910" // Tue: survivor recruits (type 120)
	duelTestRadar       = "90402|90401|90000"                    // Mon: no recruit, no training
	duelTestMobilize    = "90402|90201|90301|90501|90502|90505|90508|90510|90511|90512|90000"
	duelTestEnemyBuster = "90203|90201|90301|90501|90601|90602|90000"
)

func duelTestDay(theme int32, score string) *sfs.SFSObject {
	return duelEntry(theme, score, evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
}

// duelSpendMsg is one message the duel spend fake sends in answer to a request: a push when cmd
// is set, the reply (under the request's cmd) when it is empty.
type duelSpendMsg struct {
	cmd string
	p   *sfs.SFSObject
}

func duelReply(p *sfs.SFSObject) duelSpendMsg { return duelSpendMsg{p: p} }

func duelPush(cmd string, p *sfs.SFSObject) duelSpendMsg { return duelSpendMsg{cmd: cmd, p: p} }

// duelGoldReply is a hero.dispatch.list reply carrying the diamond balance.
func duelGoldReply(gold int64) duelSpendMsg {
	p := sfs.NewSFSObject()
	p.PutSFSArray("ls", sfs.NewSFSArray())
	p.PutLong("gold", gold)
	return duelReply(p)
}

func duelObj(kv ...any) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		switch v := kv[i+1].(type) {
		case string:
			o.PutUtfString(k, v)
		case int:
			o.PutInt(k, int32(v))
		case int64:
			o.PutLong(k, v)
		case float64:
			o.PutDouble(k, v)
		case *sfs.SFSObject:
			o.PutSFSObject(k, v)
		}
	}
	return o
}

// duelSpendFake is a scripted game server: it answers the duel info request with the given day
// entries, and every other request with handle's messages in order (pushes, then usually the
// reply). handle returning nothing means a plain success reply; a nil message closes the
// connection.
type duelSpendFake struct {
	mu   sync.Mutex
	reqs []evReq
}

func (f *duelSpendFake) requests() []evReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]evReq(nil), f.reqs...)
}

func (f *duelSpendFake) cmds() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.Cmd)
	}
	return out
}

func (f *duelSpendFake) only(cmd string) []evReq {
	var out []evReq
	for _, r := range f.requests() {
		if r.Cmd == cmd {
			out = append(out, r)
		}
	}
	return out
}

func startDuelSpendFake(t *testing.T, day *sfs.SFSObject, handle func(cmd string, p *sfs.SFSObject) []duelSpendMsg) (*session.GameConn, *duelSpendFake) {
	t.Helper()
	withEvNow(t, evTestNow)
	duelSpendHalted.Store(false)
	t.Cleanup(func() { duelSpendHalted.Store(false) })
	f := &duelSpendFake{}
	// A real TCP listener, not a pipe: pushes sent after a reply must not block the server while
	// the client is not reading, as the kernel buffers them live.
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		defer func() { _ = server.Close() }()
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, evReq{msg.Cmd, msg.Params})
			f.mu.Unlock()
			var out []duelSpendMsg
			if msg.Cmd == "hero.event.info.get" && msg.Params.GetString("activityId") == "70000" {
				var entries []*sfs.SFSObject
				if day != nil {
					entries = append(entries, day)
				}
				out = []duelSpendMsg{duelReply(duelInfo(entries...))}
			} else {
				out = handle(msg.Cmd, msg.Params)
			}
			if len(out) == 0 {
				out = []duelSpendMsg{duelReply(evOK())}
			}
			for _, m := range out {
				if m.p == nil {
					return
				}
				cmd := m.cmd
				if cmd == "" {
					cmd = msg.Cmd
				}
				if err := server.SendExtension(cmd, m.p); err != nil {
					return
				}
			}
		}
	})
	conn, err := session.DialGame(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial fake server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, f
}

// duelRecruitInit is an init with the duel activity, user.gold (gold < 0 omits it), the items held,
// officer 401100 and the hero pools.
func duelRecruitInit(gold int64, items map[string]int, pools ...*sfs.SFSObject) *Init {
	in := evInit(30, duelActivity())
	if gold >= 0 {
		in.Raw.PutSFSObject("user", duelObj("gold", gold, "allianceId", "a1"))
	}
	arr := sfs.NewSFSArray()
	for id, n := range items {
		arr.AddSFSObject(duelObj("uuid", "u"+id, "itemId", id, "count", n))
	}
	in.Raw.PutSFSArray("items", arr)
	pa := sfs.NewSFSArray()
	for _, p := range pools {
		pa.AddSFSObject(p)
	}
	in.Raw.PutSFSArray("lotteryFreeInfo", pa)
	ids := sfs.NewSFSArray()
	ids.AddInt(401100)
	in.Raw.PutSFSArray("workerOfficerIds", ids)
	return in
}

// duelPool is a lotteryFreeInfo pool with no free pull and no time limit.
func duelPool(id string, order int, item string) *sfs.SFSObject {
	return duelObj("id", id, "type", 1, "order", order, "item", item, "dailyFreeLimit", 0,
		"startTime", int64(0), "endTime", int64(0))
}

// duelRecruitServer answers the gold check with gold, the survivor info with nextFreeTime (ms) and
// every pull with a plain reply.
func duelRecruitServer(gold, nextFreeMs int64) func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
	return func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch cmd {
		case duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(gold)}
		case duelRecruitWorkerInfoCmd:
			return []duelSpendMsg{duelReply(duelObj("curNum", 0, "nextFreeTime", nextFreeMs))}
		}
		return nil
	}
}

// pullsOf summarises the pulls sent as "pool:isTen" (hero) or "w:isTen" (survivor), and checks the
// rules every pull must follow.
func pullsOf(t *testing.T, f *duelSpendFake) []string {
	t.Helper()
	var out []string
	for _, r := range f.requests() {
		switch r.Cmd {
		case duelRecruitHeroCmd:
			if r.Params.GetInt("useFree") != 0 || !r.Params.Has("aiPushStatus") || r.Params.Has("lotteryCount") {
				t.Errorf("hero pull %v: want useFree 0, aiPushStatus and no lotteryCount", r.Params)
			}
			if r.Params.GetString("itemId") == "" {
				t.Errorf("hero pull %v carries no ticket itemId", r.Params)
			}
			out = append(out, r.Params.GetString("id")+":"+string('0'+rune(r.Params.GetInt("isTen"))))
		case duelRecruitWorkerCmd:
			if r.Params.GetInt("useFree") != 0 || r.Params.GetInt("officerId") != 401100 {
				t.Errorf("survivor pull %v: want useFree 0 and officerId 401100", r.Params)
			}
			out = append(out, "w:"+string('0'+rune(r.Params.GetInt("isTen"))))
		}
		if isTen := r.Params.GetInt("isTen"); isTen != 0 && isTen != 1 {
			t.Errorf("%s sent isTen %d: only 0 and 1 may be sent", r.Cmd, isTen)
		}
	}
	return out
}

func TestDuelRecruitTicketsRegistration(t *testing.T) {
	f := featureRegistry["duel-recruit-tickets"]
	if f.DefaultOn || !f.Hold || !strings.HasPrefix(f.Summary, "OPT-IN") {
		t.Errorf("duel-recruit-tickets must be opt-in and duel-held: %+v", f)
	}
	want := []DuelScore{{Type: DuelScoreRecruitHero}, {Type: DuelScoreRecruitSurvivor}}
	if !slices.Equal(f.Duel, want) {
		t.Errorf("Duel = %v, want %v", f.Duel, want)
	}
	for _, c := range []struct {
		days []*sfs.SFSObject
		held bool
	}{
		{[]*sfs.SFSObject{duelTestDay(DuelThemeHeroes, duelTestHeroes)}, false},
		{[]*sfs.SFSObject{duelTestDay(DuelThemeBase, duelTestBase)}, false},
		{[]*sfs.SFSObject{duelTestDay(DuelThemeRadar, duelTestRadar)}, true},
		{nil, true},
	} {
		conn, in := duelFake(t, c.days...)
		if held, why := duelHeld(conn, in, f, nil); held != c.held {
			t.Errorf("held = %v (%s), want %v", held, why, c.held)
		}
	}
}

func TestDuelRecruitHeroSpendsEveryTicketOnTrainHeroes(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 23, duelRecruitWorkerTicket: 50},
		duelPool("221700", 2, "230006;1|230006;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	// 23 tickets: two ten pulls (23 -> 13 -> 3), then three singles. Survivor tickets are kept on
	// a day that doesn't score survivors.
	want := []string{"221700:1", "221700:1", "221700:0", "221700:0", "221700:0"}
	if got := pullsOf(t, f); !slices.Equal(got, want) {
		t.Fatalf("pulls %v, want %v", got, want)
	}
	// The balance is read before the first pull and after every pull.
	cmds := f.cmds()
	if got := len(f.only(duelGoldProbeCmd)); got != 6 {
		t.Errorf("sent %d gold checks, want 6: %v", got, cmds)
	}
	for i, c := range cmds[1:] {
		if c == duelRecruitHeroCmd && cmds[i] != duelGoldProbeCmd {
			t.Errorf("pull %d was not preceded by a gold check: %v", i, cmds)
		}
	}
	for _, r := range f.only(duelRecruitHeroCmd) {
		if r.Params.GetString("itemId") != "230006" {
			t.Errorf("pull itemId %q, want the pool's ticket 230006", r.Params.GetString("itemId"))
		}
	}
}

func TestDuelRecruitTenPullBoundary(t *testing.T) {
	for _, c := range []struct {
		held int
		want []string
	}{
		{9, []string{"p:0", "p:0", "p:0", "p:0", "p:0", "p:0", "p:0", "p:0", "p:0"}},
		{10, []string{"p:1"}},
		{1, []string{"p:0"}},
	} {
		in := duelRecruitInit(1000, map[string]int{"230006": c.held}, duelPool("p", 2, "230006;1|230006;10"))
		conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
		if err := runDuelRecruitTickets(conn, in); err != nil {
			t.Fatalf("held %d: %v", c.held, err)
		}
		if got := pullsOf(t, f); !slices.Equal(got, c.want) {
			t.Errorf("held %d: pulls %v, want %v", c.held, got, c.want)
		}
	}
	// A pool whose ten pull costs another item never gets isTen 1.
	in := duelRecruitInit(1000, map[string]int{"230006": 12}, duelPool("p", 2, "230006;1|999999;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := pullsOf(t, f); len(got) != 12 || slices.Contains(got, "p:1") {
		t.Errorf("pulls %v, want 12 singles", got)
	}
}

func TestDuelRecruitNeverPullsWithoutTickets(t *testing.T) {
	for name, items := range map[string]map[string]int{
		"none":             {},
		"other ticket":     {"640111": 5},
		"zero count":       {"230006": 0},
		"survivor tickets": {duelRecruitWorkerTicket: 30},
	} {
		in := duelRecruitInit(1000, items, duelPool("221700", 2, "230006;1|230006;10"))
		conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
		if err := runDuelRecruitTickets(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
			t.Errorf("%s: sent %v, want only the duel read", name, got)
		}
	}
}

func TestDuelRecruitPicksTheClientsPoolPerTicket(t *testing.T) {
	now := evTestNow.Unix()
	extra := duelPool("221800", 50, "230006;1|230006;10")
	extra.PutUtfString("extra_res", "15;100") // also costs a resource: never pulled
	limited := duelPool("221900", 40, "230006;1|230006;10")
	limited.PutLong("startTime", now-7200)
	limited.PutLong("endTime", now-3600) // closed
	typ2 := duelPool("221950", 60, "230006;1|230006;10")
	typ2.PutInt("type", 2) // not listed by the client
	in := duelRecruitInit(1000, map[string]int{"230006": 2, "640111": 1, "640112": 0},
		duelPool("221700", 2, "230006;1|230006;10"),
		duelPool("221702", 5, "230006;1|230006;10"), // same ticket, earlier tab: this one
		duelPool("301100", 100, "640111;1|640111;10"),
		duelPool("301101", 90, "640112;1|640112;10"),
		extra, limited, typ2)
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{"301100:0", "221702:0", "221702:0"}
	if got := pullsOf(t, f); !slices.Equal(got, want) {
		t.Errorf("pulls %v, want %v", got, want)
	}
}

func TestDuelRecruitLeavesSinglesWhileTheFreePullIsDue(t *testing.T) {
	pool := duelPool("221700", 2, "230006;1|230006;10")
	pool.PutInt("dailyFreeLimit", 1)
	pool.PutLong("dailyFreeNextFreshTime", evTestNow.Add(-time.Minute).Unix())
	in := duelRecruitInit(1000, map[string]int{"230006": 13}, pool)
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := pullsOf(t, f); !slices.Equal(got, []string{"221700:1"}) {
		t.Errorf("pulls %v, want one ten pull and no single while the free pull is due", got)
	}
}

func TestDuelRecruitPushedCountLowersTheLedger(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 30}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch cmd {
		case duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(1000)}
		case duelRecruitHeroCmd:
			// The server says only 2 are left after this pull, after the reply.
			return []duelSpendMsg{duelReply(evOK()), duelPush(duelPushItemUse, duelObj("uuid", "u1", "itemId", "230006", "count", 2))}
		}
		return nil
	})
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	// One ten pull, then the pushed 2 (not the ledger's 20) decides: two singles.
	want := []string{"221700:1", "221700:0", "221700:0"}
	if got := pullsOf(t, f); !slices.Equal(got, want) {
		t.Errorf("pulls %v, want %v", got, want)
	}

	// push.item.del without a count removes the stack.
	in = duelRecruitInit(1000, map[string]int{"230006": 30}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch cmd {
		case duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(1000)}
		case duelRecruitHeroCmd:
			return []duelSpendMsg{duelPush(duelPushItemDel, duelObj("uuid", "u1", "itemId", "230006")), duelReply(evOK())}
		}
		return nil
	})
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := pullsOf(t, f); !slices.Equal(got, []string{"221700:1"}) {
		t.Errorf("pulls %v, want one pull before the stack was deleted", got)
	}
}

func TestDuelRecruitDiamondTripwire(t *testing.T) {
	for name, drop := range map[string]func(cmd string, pulls int) []duelSpendMsg{
		"pushed balance": func(cmd string, pulls int) []duelSpendMsg {
			if cmd == duelRecruitHeroCmd {
				return []duelSpendMsg{duelPush(duelPushResourceInfo, duelObj("gold", int64(940))), duelReply(evOK())}
			}
			return []duelSpendMsg{duelGoldReply(1000)}
		},
		"gold check": func(cmd string, pulls int) []duelSpendMsg {
			if cmd == duelGoldProbeCmd && pulls > 0 {
				return []duelSpendMsg{duelGoldReply(999)}
			}
			return []duelSpendMsg{duelGoldReply(1000)}
		},
		"reply": func(cmd string, pulls int) []duelSpendMsg {
			if cmd == duelRecruitHeroCmd {
				return []duelSpendMsg{duelReply(duelObj("remainGold", int64(10)))}
			}
			return []duelSpendMsg{duelGoldReply(1000)}
		},
		"remain gold push": func(cmd string, pulls int) []duelSpendMsg {
			if cmd == duelRecruitHeroCmd {
				return []duelSpendMsg{duelReply(evOK()), duelPush(duelPushRemainGold, duelObj("remainGold", int64(1)))}
			}
			return []duelSpendMsg{duelGoldReply(1000)}
		},
	} {
		in := duelRecruitInit(1000, map[string]int{"230006": 40}, duelPool("221700", 2, "230006;1|230006;10"))
		pulls := 0
		conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
			out := drop(cmd, pulls)
			if cmd == duelRecruitHeroCmd {
				pulls++
			}
			return out
		})
		err := runDuelRecruitTickets(conn, in)
		if !errors.Is(err, errDuelSpendHalted) || !duelSpendHalted.Load() {
			t.Errorf("%s: err = %v, want the tripwire's error and the session halted", name, err)
		}
		if got := len(f.only(duelRecruitHeroCmd)); got != 1 {
			t.Errorf("%s: %d pulls sent, want none after the drop", name, got)
		}
		// Every duel spend stays halted for the session, without sending anything.
		before := len(f.requests())
		if err := runDuelRecruitTickets(conn, in); !errors.Is(err, errDuelSpendHalted) {
			t.Errorf("%s: a second run must refuse: %v", name, err)
		}
		if err := runDuelTroopTraining(conn, in); !errors.Is(err, errDuelSpendHalted) {
			t.Errorf("%s: duel-troop-training must refuse too: %v", name, err)
		}
		if after := len(f.requests()); after != before {
			t.Errorf("%s: halted runs sent %d requests", name, after-before)
		}
	}
}

func TestDuelRecruitRaisedBalanceIsTheNewBaseline(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 2}, duelPool("221700", 2, "230006;1|230006;10"))
	checks := 0
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelGoldProbeCmd {
			checks++
			// Claims earlier in the run raised it to 1050; the pulls keep it there.
			return []duelSpendMsg{duelGoldReply(1050)}
		}
		return nil
	})
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := pullsOf(t, f); len(got) != 2 || checks != 3 {
		t.Errorf("pulls %v with %d gold checks, want 2 pulls and 3 checks", got, checks)
	}
}

func TestDuelRecruitNeedsTheGoldBaseline(t *testing.T) {
	in := duelRecruitInit(-1, map[string]int{"230006": 20}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := f.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
		t.Errorf("without user.gold it sent %v", got)
	}

	// The gold check is unavailable: init gold and the pushes are the tripwire, with no check per pull.
	in = duelRecruitInit(1000, map[string]int{"230006": 2}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelGoldProbeCmd {
			return []duelSpendMsg{duelReply(evErr("E100001"))}
		}
		return nil
	})
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := len(f.only(duelGoldProbeCmd)); got != 1 || len(pullsOf(t, f)) != 2 {
		t.Errorf("sent %v, want one gold check and 2 pulls", f.cmds())
	}
}

func TestDuelRecruitStopsAPoolOnErrorCode(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 5, "640111": 1},
		duelPool("221700", 2, "230006;1|230006;10"), duelPool("301100", 100, "640111;1|640111;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		switch {
		case cmd == duelGoldProbeCmd:
			return []duelSpendMsg{duelGoldReply(1000)}
		case cmd == duelRecruitHeroCmd && p.GetString("id") == "301100":
			return []duelSpendMsg{duelReply(evErr("E120200"))}
		}
		return nil
	})
	err := runDuelRecruitTickets(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E120200") {
		t.Errorf("err = %v, want the pool's errorCode", err)
	}
	want := []string{"301100:0", "221700:0", "221700:0", "221700:0", "221700:0", "221700:0"}
	if got := pullsOf(t, f); !slices.Equal(got, want) {
		t.Errorf("pulls %v, want %v (the failing pool stops, the next one runs)", got, want)
	}
}

func TestDuelRecruitPullCap(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 150}, duelPool("221700", 2, "230006;1"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, duelTestHeroes), duelRecruitServer(1000, 0))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := len(f.only(duelRecruitHeroCmd)); got != duelRecruitMaxPulls {
		t.Errorf("sent %d pulls, want the cap %d", got, duelRecruitMaxPulls)
	}
}

func TestDuelRecruitSurvivorsOnBaseExpansion(t *testing.T) {
	future := evTestNow.Add(time.Hour).UnixMilli()
	in := duelRecruitInit(1000, map[string]int{"230006": 40, duelRecruitWorkerTicket: 12}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeBase, duelTestBase), duelRecruitServer(1000, future))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	// Hero tickets are kept on a survivor day; 12 survivor tickets: a ten pull, then two singles.
	if got := pullsOf(t, f); !slices.Equal(got, []string{"w:1", "w:0", "w:0"}) {
		t.Errorf("pulls %v, want w:1 w:0 w:0", got)
	}

	// While the free draw is due only ten pulls go out.
	in = duelRecruitInit(1000, map[string]int{duelRecruitWorkerTicket: 12})
	conn, f = startDuelSpendFake(t, duelTestDay(DuelThemeBase, duelTestBase), duelRecruitServer(1000, evTestNow.Add(-time.Minute).UnixMilli()))
	if err := runDuelRecruitTickets(conn, in); err != nil {
		t.Fatal(err)
	}
	if got := pullsOf(t, f); !slices.Equal(got, []string{"w:1"}) {
		t.Errorf("pulls %v, want only w:1 while the free draw is due", got)
	}
}

func TestDuelRecruitKeepsTicketsOnOtherDays(t *testing.T) {
	for name, day := range map[string]*sfs.SFSObject{
		"radar":       duelTestDay(DuelThemeRadar, duelTestRadar),
		"no duel day": nil,
		"stale day":   duelEntry(DuelThemeHeroes, duelTestHeroes, evTestNow.Add(-25*time.Hour), evTestNow.Add(-time.Hour)),
	} {
		in := duelRecruitInit(1000, map[string]int{"230006": 40, duelRecruitWorkerTicket: 40}, duelPool("221700", 2, "230006;1|230006;10"))
		conn, f := startDuelSpendFake(t, day, duelRecruitServer(1000, 0))
		if err := runDuelRecruitTickets(conn, in); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := f.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
			t.Errorf("%s: sent %v, want only the duel read", name, got)
		}
	}
	if err := runDuelRecruitTickets(nil, &Init{}); err != nil {
		t.Errorf("no init: %v", err)
	}
}

func TestDuelRecruitDeadConnection(t *testing.T) {
	in := duelRecruitInit(1000, map[string]int{"230006": 5, duelRecruitWorkerTicket: 5}, duelPool("221700", 2, "230006;1|230006;10"))
	conn, f := startDuelSpendFake(t, duelTestDay(DuelThemeHeroes, "90101|100910"), func(cmd string, p *sfs.SFSObject) []duelSpendMsg {
		if cmd == duelRecruitHeroCmd {
			return []duelSpendMsg{{}} // close
		}
		return []duelSpendMsg{duelGoldReply(1000)}
	})
	err := runDuelRecruitTickets(conn, in)
	if !session.ContainsNonTimeoutNetError(err) {
		t.Errorf("err = %v, want a dead-connection error", err)
	}
	if got := f.only(duelRecruitWorkerCmd); len(got) != 0 {
		t.Errorf("survivor pulls after a dead connection: %v", got)
	}
}
