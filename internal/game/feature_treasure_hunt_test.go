package game

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// digsInit is an Init for the dig-feature tests: the given activities, a player (uid 1001, 500
// diamonds, server 777, in an alliance) and pickaxes owned of item 620002.
func digsInit(pickaxes int64, acts ...*sfs.SFSObject) *Init {
	in := evInit(30, acts...)
	user := sfs.NewSFSObject()
	user.PutUtfString("uid", "1001")
	user.PutLong("gold", 500)
	user.PutInt("serverId", 777)
	user.PutUtfString("allianceId", "a1")
	in.Raw.PutSFSObject("user", user)
	items := sfs.NewSFSArray()
	if pickaxes > 0 {
		it := sfs.NewSFSObject()
		it.PutUtfString("itemId", "620002")
		it.PutLong("count", pickaxes)
		items.AddSFSObject(it)
	}
	in.Raw.PutSFSArray("items", items)
	return in
}

// digsAssertSafe fails when a request the fake saw is outside the allowlist, a purchase or carries
// a gold key.
func digsAssertSafe(t *testing.T, f *evFake) {
	t.Helper()
	for _, r := range f.requests() {
		if strings.Contains(r.Cmd, "buy") {
			t.Errorf("sent a purchase command %s", r.Cmd)
		}
		if _, ok := digsAllowedKeys[r.Cmd]; !ok {
			t.Errorf("sent %s, which is not in the dig allowlist", r.Cmd)
		}
		for _, k := range r.Params.Keys() {
			if strings.Contains(strings.ToLower(k), "gold") {
				t.Errorf("%s carried key %q", r.Cmd, k)
			}
		}
	}
}

// digsAssertReadOnly fails when a plan run sent anything but an info request.
func digsAssertReadOnly(t *testing.T, f *evFake) {
	t.Helper()
	for _, c := range f.cmds() {
		if !digsReadOnly[c] {
			t.Errorf("plan sent a write: %s (all: %v)", c, f.cmds())
		}
	}
}

// thFake is a scripted Treasure Hunt server: one activity, the grand prize on the grandAt-th card
// of each level.
type thFake struct {
	mu        sync.Mutex
	v2        bool
	id        int32
	level     int64
	big       int64
	dug       []int64
	claimed   []int64
	stored    int
	grandAt   int
	prize     string
	digErr    string
	tierEmpty bool
}

func (f *thFake) reply(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := sfs.NewSFSObject()
	switch cmd {
	case treasureHuntInfoCmd, treasureHuntInfoV2Cmd:
		r.PutInt("activityId", f.id)
		r.PutInt("level", int32(f.level))
		r.PutInt("bigRewardIndex", int32(f.big))
		rec := sfs.NewSFSArray()
		for _, d := range f.dug {
			e := sfs.NewSFSObject()
			e.PutInt("digIndex", int32(d))
			e.PutInt("goodsIndex", 2)
			rec.AddSFSObject(e)
		}
		r.PutSFSArray("digRecord", rec)
		if f.v2 {
			got := sfs.NewSFSArray()
			for _, c := range f.claimed {
				got.AddInt(int32(c))
			}
			r.PutSFSArray("bigRewardReceive", got)
			if f.prize != "" {
				r.PutUtfString("primaryPrize", f.prize)
			}
			tr := sfs.NewSFSArray()
			for range f.stored {
				tr.AddSFSObject(sfs.NewSFSObject())
			}
			r.PutSFSArray("totalReward", tr)
		}
	case treasureHuntChooseCmd, treasureHuntChooseV2Cmd:
		f.big = int64(p.GetInt("bigRewardIndex"))
		r.PutInt("activityId", f.id)
		r.PutInt("bigRewardIndex", int32(f.big))
	case treasureHuntDigCmd, treasureHuntDigV2Cmd:
		if f.digErr != "" {
			return evErr(f.digErr)
		}
		idx := int64(p.GetInt("digIndex"))
		f.dug = append(f.dug, idx)
		r.PutInt("digIndex", int32(idx))
		if len(f.dug) == f.grandAt {
			r.PutInt("goodsIndex", 0)
			f.level++
			f.big = 0
			f.dug = nil
		} else {
			r.PutInt("goodsIndex", 2)
		}
		if f.v2 {
			f.stored++
		}
	case treasureHuntTierCmd:
		if !f.tierEmpty {
			for _, lv := range treasureHuntPrimaryPrize(f.prize) {
				if lv <= f.level+1 && !slices.Contains(f.claimed, lv) {
					f.claimed = append(f.claimed, lv)
					break
				}
			}
			rw := sfs.NewSFSArray()
			rw.AddSFSObject(sfs.NewSFSObject())
			r.PutSFSArray("reward", rw)
		}
		r.PutInt("aid", f.id)
	case treasureHuntStoredCmd:
		f.stored = 0
		rw := sfs.NewSFSArray()
		rw.AddSFSObject(sfs.NewSFSObject())
		r.PutSFSArray("reward", rw)
	default:
		return evOK()
	}
	return r
}

func TestTreasureHuntV2ClaimsChoosesDigsAndClaimsStored(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{v2: true, id: 1000289, level: 4, grandAt: 3, prize: "5;510026;2000|10;510026;4000"}
	conn, fake := startEvFake(t, f.reply)
	if err := runTreasureHunt(conn, digsInit(4, evActivity(1000289))); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"get.dig.v2.activity.info", "dig.receive.big.reward", "get.dig.v2.activity.info",
		"choose.dig.v2.big.reward", "start.activity.dig.v2", "start.activity.dig.v2", "start.activity.dig.v2",
		"get.dig.v2.activity.info", "choose.dig.v2.big.reward", "start.activity.dig.v2",
		"get.dig.v2.activity.info", "dig.extra.reward",
	}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v\nwant %v", got, want)
	}
	for _, c := range fake.only(treasureHuntChooseV2Cmd) {
		if c.Params.GetInt("bigRewardIndex") != treasureHuntRecommended || c.Params.GetInt("activityId") != 1000289 {
			t.Errorf("choose params = %v, want the recommended index 1", c.Params)
		}
	}
	var idx []int32
	for _, d := range fake.only(treasureHuntDigV2Cmd) {
		idx = append(idx, d.Params.GetInt("digIndex"))
	}
	if !slices.Equal(idx, []int32{1, 2, 3, 1}) {
		t.Errorf("dug cards %v, want 1 2 3 then 1 on the next level (4 pickaxes)", idx)
	}
	if aid := fake.only(treasureHuntStoredCmd)[0].Params.GetInt("aid"); aid != 1000289 {
		t.Errorf("stored claim aid = %d", aid)
	}
	digsAssertSafe(t, fake)
}

func TestTreasureHuntV1DigsOnlyUndugCardsWithOwnedPickaxes(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{id: 3009, level: 2, big: 2, dug: []int64{1, 3}, grandAt: 99}
	conn, fake := startEvFake(t, f.reply)
	if err := runTreasureHunt(conn, digsInit(3, evActivity(3009))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.dig.activity.info,start.activity.dig,start.activity.dig,start.activity.dig" {
		t.Fatalf("sent %s", got)
	}
	var idx []int32
	for _, d := range fake.only(treasureHuntDigCmd) {
		idx = append(idx, d.Params.GetInt("digIndex"))
	}
	if !slices.Equal(idx, []int32{2, 4, 5}) {
		t.Errorf("dug %v, want the undug cards 2 4 5 (3 pickaxes; 1 and 3 already dug)", idx)
	}
	digsAssertSafe(t, fake)
}

func TestTreasureHuntNeverDigsWithoutPickaxes(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, act := range []int32{3009, 1000289} {
		f := &thFake{v2: act == 1000289, id: act, level: 1, grandAt: 3}
		conn, fake := startEvFake(t, f.reply)
		if err := runTreasureHunt(conn, digsInit(0, evActivity(act))); err != nil {
			t.Fatal(err)
		}
		for _, c := range fake.cmds() {
			if c != treasureHuntInfoCmd && c != treasureHuntInfoV2Cmd {
				t.Errorf("activity %d with no pickaxes sent %s", act, c)
			}
		}
	}
}

func TestTreasureHuntStopsOnNotEnoughItems(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{id: 3009, level: 1, big: 1, grandAt: 99, digErr: digsNoItemsCode}
	conn, fake := startEvFake(t, f.reply)
	if err := runTreasureHunt(conn, digsInit(10, evActivity(3009))); err != nil {
		t.Fatalf("120021 is benign: %v", err)
	}
	if n := len(fake.only(treasureHuntDigCmd)); n != 1 {
		t.Errorf("sent %d digs after 120021, want 1", n)
	}
}

func TestTreasureHuntLevelLimits(t *testing.T) {
	withEvNow(t, evTestNow)
	cases := []struct {
		name string
		f    *thFake
		act  int32
	}{
		{"v2 at stop_level 100", &thFake{v2: true, id: 1000289, level: 99, grandAt: 3}, 1000289},
		{"v1 sub-type 2 past its 50 levels", &thFake{id: 14003, level: 50, grandAt: 3}, 14003},
	}
	for _, c := range cases {
		conn, fake := startEvFake(t, c.f.reply)
		if err := runTreasureHunt(conn, digsInit(10, evActivity(c.act))); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range fake.cmds() {
			if strings.HasPrefix(cmd, "choose.") || strings.HasPrefix(cmd, "start.") {
				t.Errorf("%s: sent %s", c.name, cmd)
			}
		}
	}
}

func TestTreasureHuntInactiveOrNoData(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	ended := evActivity(1000289)
	ended.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	for _, in := range []*Init{nil, {}, digsInit(10), digsInit(10, ended)} {
		if err := runTreasureHunt(conn, in); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake.requests()); n != 0 {
		t.Fatalf("inactive event sent %v", fake.cmds())
	}
	// A bare success reply (no activityId) is no data: nothing after the info request.
	if err := runTreasureHunt(conn, digsInit(10, evActivity(1000289))); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "get.dig.v2.activity.info" {
		t.Errorf("no-data reply: sent %s", got)
	}
}

func TestTreasureHuntTierClaimStopsOnEmptyReward(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{v2: true, id: 1000289, level: 30, big: 1, grandAt: 99, prize: "5;1;1|10;1;1|20;1;1", tierEmpty: true}
	conn, fake := startEvFake(t, f.reply)
	if err := runTreasureHunt(conn, digsInit(0, evActivity(1000289))); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only(treasureHuntTierCmd)); n != 1 {
		t.Errorf("sent %d tier claims, want 1: an empty reward ends the claims", n)
	}
}

func TestTreasureHuntPlanIsReadOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{v2: true, id: 1000289, level: 4, grandAt: 3, prize: "5;510026;2000", stored: 2}
	conn, fake := startEvFake(t, f.reply)
	if err := runTreasureHuntPlan(conn, digsInit(4, evActivity(1000289), evActivity(3009))); err != nil {
		t.Fatal(err)
	}
	if len(fake.requests()) == 0 {
		t.Fatal("plan sent no info requests")
	}
	digsAssertReadOnly(t, fake)
}

func TestTreasureHuntDiamondTripwire(t *testing.T) {
	withEvNow(t, evTestNow)
	t.Cleanup(func() { digsTrip.Lock(); digsTrip.reason = ""; digsTrip.Unlock() })
	f := &thFake{id: 3009, level: 1, big: 1, grandAt: 99}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := f.reply(cmd, p)
		if cmd == treasureHuntDigCmd {
			r.PutLong("gold", 400) // below the 500 in init
		}
		return r
	})
	err := runTreasureHunt(conn, digsInit(10, evActivity(3009)))
	if err == nil || !strings.Contains(err.Error(), "tripwire") {
		t.Fatalf("err = %v, want the diamond tripwire", err)
	}
	if n := len(fake.only(treasureHuntDigCmd)); n != 1 {
		t.Errorf("sent %d digs, want the run to stop after the first", n)
	}
	if err := runTreasureHunt(conn, digsInit(10, evActivity(3009))); err == nil {
		t.Error("a tripped session must refuse further dig writes")
	}
	if n := len(fake.only(treasureHuntDigCmd)); n != 1 {
		t.Errorf("tripped session sent another dig (%d total)", n)
	}
}

func TestTreasureHuntPrimaryPrize(t *testing.T) {
	if got := treasureHuntPrimaryPrize("10;1;2|5;3;4|bad|20;5"); !slices.Equal(got, []int64{5, 10}) {
		t.Errorf("got %v", got)
	}
	info := sfs.NewSFSObject()
	info.PutInt("level", 9)
	got := sfs.NewSFSArray()
	got.AddInt(5)
	info.PutSFSArray("bigRewardReceive", got)
	if due := treasureHuntTiersDue(info, treasureHuntDigs[5]); !slices.Equal(due, []int64{10}) {
		t.Errorf("due = %v, want [10] (level 10 reached, 5 claimed)", due)
	}
}

func TestTreasureHuntDigFailureStopsThatActivityOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &thFake{id: 3009, level: 1, big: 1, grandAt: 99, digErr: "E9"}
	conn, fake := startEvFake(t, f.reply)
	err := runTreasureHunt(conn, digsInit(10, evActivity(3009), evActivity(5104)))
	if err == nil || !strings.Contains(err.Error(), "E9") {
		t.Fatalf("err = %v, want the failed dig reported", err)
	}
	if n := len(fake.only(treasureHuntDigCmd)); n != 2 {
		t.Errorf("sent %d digs, want one per activity: %v", n, fake.cmds())
	}
}
