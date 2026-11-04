package game

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"lastwar-client/internal/sfs"
)

// overflowInfo is a pass info reply (the client's field names, ActBattlePassInfo.lua:27-71,
// 222-256) at level, with stateInfo rows 1..top all claimed, the given battlePass.exp, extraExp
// and infiniteNum, and a one-item extraReward.
func overflowInfo(level, top int32, exp, extraExp, infiniteNum int64) *sfs.SFSObject {
	normal := make([]int32, top)
	special := make([]int32, top)
	for i := range normal {
		normal[i], special[i] = 1, 1
	}
	info := battlePassInfo(level, 0, normal, special, nil)
	evObject(info, "battlePass").PutLong("exp", exp)
	info.PutLong("extraExp", extraExp)
	info.PutInt("infiniteNum", int32(infiniteNum))
	info.PutSFSArray("extraReward", freebieReward())
	return info
}

// overflowPass is a fake pass server. Each box claim runs claim on the state, and the next info
// read shows the result. edit, when set, rewrites every info reply.
type overflowPass struct {
	mu                        sync.Mutex
	level, top                int32
	exp, extra, inf           int64
	claim                     func(p *overflowPass)
	edit                      func(info *sfs.SFSObject) *sfs.SFSObject
	claimErr                  string
	infoCmd, extraCmd, allCmd string
	onAll                     func(p *overflowPass)
}

func v2OverflowPass() *overflowPass {
	return &overflowPass{infoCmd: "hero.event.info.get", extraCmd: battlePassV2ExtraCmd, allCmd: battlePassV2AllCmd}
}

func v1OverflowPass() *overflowPass {
	return &overflowPass{infoCmd: battlePassInfoCmd, extraCmd: battlePassExtraCmd, allCmd: battlePassAllCmd}
}

func (o *overflowPass) reply(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch cmd {
	case o.infoCmd:
		info := overflowInfo(o.level, o.top, o.exp, o.extra, o.inf)
		if o.edit != nil {
			info = o.edit(info)
		}
		return info
	case o.allCmd:
		if o.onAll != nil {
			o.onAll(o)
		}
	case o.extraCmd:
		if o.claimErr != "" {
			return evErr(o.claimErr)
		}
		if o.claim != nil {
			o.claim(o)
		}
		r := sfs.NewSFSObject()
		bp := sfs.NewSFSObject()
		bp.PutInt("level", o.level)
		bp.PutLong("exp", o.exp)
		r.PutSFSObject("battlePass", bp)
		r.PutInt("infiniteNum", int32(o.inf))
		r.PutSFSArray("reward", freebieReward())
		return r
	}
	return evOK()
}

// withoutKey copies o without key.
func withoutKey(o *sfs.SFSObject, key string) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	for _, k := range o.Keys() {
		if k != key {
			v, _ := o.Get(k)
			r.PutValue(k, v)
		}
	}
	return r
}

func TestBattlePassOverflowV2SubtractsClaimedBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v2OverflowPass()
	o.level, o.top, o.exp, o.extra, o.inf = 3, 3, 3500, 1000, 1 // (3500 - 1000*1) / 1000 = 2 boxes
	o.claim = func(p *overflowPass) { p.inf++ }                 // a claimed box shows up in infiniteNum
	conn, fake := startEvFake(t, o.reply)
	if err := runBattlePassOverflow(conn, evInit(30, evActivity(5038))); err != nil {
		t.Fatal(err)
	}
	want := []string{"hero.event.info.get", battlePassV2ExtraCmd, "hero.event.info.get", battlePassV2ExtraCmd, "hero.event.info.get"}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	for _, r := range fake.only(battlePassV2ExtraCmd) {
		if v, _ := r.Params.Get("activityId"); v.Type != sfs.SFSInt || r.Params.GetInt("activityId") != 5038 {
			t.Errorf("claim params = %v, want activityId Int 5038", r.Params)
		}
		for _, k := range r.Params.Keys() {
			if k != "activityId" && k != "_id" { // _id is the wire layer's request id
				t.Errorf("claim carries %q; the client sends activityId only", k)
			}
		}
	}
}

func TestBattlePassOverflowV1IgnoresInfiniteNum(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v1OverflowPass()
	o.level, o.top, o.exp, o.extra, o.inf = 2, 2, 2500, 1000, 7 // type 27: 2500 / 1000 = 2 boxes
	o.claim = func(p *overflowPass) { p.exp -= 1000 }
	conn, fake := startEvFake(t, o.reply)
	if err := runBattlePassOverflow(conn, evInit(30, evActivity(2001))); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.only(battlePassExtraCmd)); got != 2 {
		t.Fatalf("sent %d type-27 box claims (%v), want 2", got, fake.cmds())
	}
	if len(fake.only(battlePassV2ExtraCmd)) != 0 {
		t.Error("a type-27 pass must use receive.battlepass.extra.reward")
	}
	if v, _ := fake.only(battlePassInfoCmd)[0].Params.Get("activityId"); v.Type != sfs.SFSInt {
		t.Errorf("info activityId type %d, want Int", v.Type)
	}
}

// TestBattlePassOverflowSkipsLoginPasses: the seven-day login passes (activity showUiType 2, 6, 9)
// show no overflow box (ActBattlePassInfo.lua:357-369), so nothing is sent for them even when the
// info would count boxes; a normal type-27 pass next to them still claims.
func TestBattlePassOverflowSkipsLoginPasses(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v1OverflowPass()
	o.level, o.top, o.exp, o.extra = 2, 2, 2500, 1000
	o.claim = func(p *overflowPass) { p.exp -= 1000 }
	conn, fake := startEvFake(t, o.reply)
	if err := runBattlePassOverflow(conn, evInit(30, evActivity(13), evActivity(99022), evActivity(99044), evActivity(2001))); err != nil {
		t.Fatal(err)
	}
	for _, r := range fake.requests() {
		if id := r.Params.GetInt("activityId"); id != 2001 {
			t.Errorf("sent %s for login pass %d", r.Cmd, id)
		}
	}
	if got := len(fake.only(battlePassExtraCmd)); got != 2 {
		t.Errorf("sent %d box claims for pass 2001, want 2", got)
	}
}

func TestBattlePassOverflowGates(t *testing.T) {
	for _, c := range []struct {
		name  string
		level int32
		edit  func(info *sfs.SFSObject) *sfs.SFSObject
	}{
		{"below the top level", 2, nil},
		{"no extraExp", 3, func(i *sfs.SFSObject) *sfs.SFSObject { i.PutLong("extraExp", 0); return i }},
		{"empty extraReward", 3, func(i *sfs.SFSObject) *sfs.SFSObject { i.PutSFSArray("extraReward", sfs.NewSFSArray()); return i }},
		{"no extraReward", 3, func(i *sfs.SFSObject) *sfs.SFSObject { return withoutKey(i, "extraReward") }},
		{"a level missing from stateInfo", 3, func(i *sfs.SFSObject) *sfs.SFSObject {
			rows := sfs.NewSFSArray()
			for _, l := range []int32{0, 1, 3} { // level 0 is dropped as ParseStateInfo does; 2 is missing
				r := sfs.NewSFSObject()
				r.PutInt("level", l)
				r.PutInt("normalState", 1)
				rows.AddSFSObject(r)
			}
			i.PutSFSArray("stateInfo", rows)
			return i
		}},
		{"activityType disagrees with the table", 3, func(i *sfs.SFSObject) *sfs.SFSObject { i.PutInt("activityType", 27); return i }},
		{"less than one box after the subtraction", 3, func(i *sfs.SFSObject) *sfs.SFSObject {
			evObject(i, "battlePass").PutLong("exp", 1999) // 1999 - 1000*1 = 999 < 1000
			return i
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			withEvNow(t, evTestNow)
			o := v2OverflowPass()
			o.level, o.top, o.exp, o.extra, o.inf, o.edit = c.level, 3, 5000, 1000, 1, c.edit
			conn, fake := startEvFake(t, o.reply)
			if err := runBattlePassOverflow(conn, evInit(30, evActivity(5038))); err != nil {
				t.Fatal(err)
			}
			if got := fake.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
				t.Errorf("sent %v, want only the info read", got)
			}
		})
	}
}

func TestBattlePassOverflowStopsWhenTheCountDoesNotFall(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v2OverflowPass()
	o.level, o.top, o.exp, o.extra = 3, 3, 9000, 1000 // the server answers but the pass does not change
	conn, fake := startEvFake(t, o.reply)
	if err := runBattlePassOverflow(conn, evInit(30, evActivity(5038))); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.only(battlePassV2ExtraCmd)); got != 1 {
		t.Errorf("sent %d claims, want 1 then stop", got)
	}
}

func TestBattlePassOverflowStopsAtTheBound(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v1OverflowPass()
	o.level, o.top, o.exp, o.extra = 1, 1, 1000*(battlePassOverflowMaxClaims+5), 1000
	o.claim = func(p *overflowPass) { p.exp -= 1000 }
	conn, fake := startEvFake(t, o.reply)
	if err := runBattlePassOverflow(conn, evInit(30, evActivity(2001))); err != nil {
		t.Fatal(err)
	}
	if got := len(fake.only(battlePassExtraCmd)); got != battlePassOverflowMaxClaims {
		t.Errorf("sent %d claims, want the bound %d", got, battlePassOverflowMaxClaims)
	}
}

func TestBattlePassOverflowErrorStopsThatPass(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v1OverflowPass()
	o.level, o.top, o.exp, o.extra, o.claimErr = 1, 1, 5000, 1000, evAlreadyExecuted
	conn, fake := startEvFake(t, o.reply)
	err := runBattlePassOverflow(conn, evInit(30, evActivity(2001), evActivity(19)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s reply reported (no benign codes for this feature)", err, evAlreadyExecuted)
	}
	claims := fake.only(battlePassExtraCmd)
	if len(claims) != 2 || claims[0].Params.GetInt("activityId") != 19 || claims[1].Params.GetInt("activityId") != 2001 {
		t.Errorf("sent %v, want one claim per pass (19, 2001) and no retry", fake.cmds())
	}
}

// The ON battle-pass feature runs first and its claim-all can level the pass; the overflow
// feature reads the info again instead of reusing the sweep's cached reply.
func TestBattlePassOverflowRereadsAfterTheBattlePassFeature(t *testing.T) {
	withEvNow(t, evTestNow)
	o := v2OverflowPass()
	o.level, o.top, o.extra = 2, 3, 1000
	o.edit = func(i *sfs.SFSObject) *sfs.SFSObject {
		if evObject(i, "battlePass").GetInt("level") == 2 {
			evObjects(i, "stateInfo")[1].PutInt("normalState", 0) // level 2's free row is unclaimed
		}
		return i
	}
	o.onAll = func(p *overflowPass) { p.level, p.exp = 3, 1500 } // claim-all reaches the top with a box of exp
	o.claim = func(p *overflowPass) { p.inf++ }
	conn, fake := startEvFake(t, o.reply)
	in := evInit(30, evActivity(5038))
	if err := runBattlePass(conn, in); err != nil {
		t.Fatal(err)
	}
	if err := runBattlePassOverflow(conn, in); err != nil {
		t.Fatal(err)
	}
	want := []string{"hero.event.info.get", battlePassV2AllCmd, "hero.event.info.get", battlePassV2ExtraCmd, "hero.event.info.get"}
	if got := fake.cmds(); !slices.Equal(got, want) {
		t.Errorf("sent %v, want %v", got, want)
	}
}

func TestBattlePassOverflowState(t *testing.T) {
	for _, c := range []struct {
		name            string
		typ             int32
		exp, extra, inf int64
		level, top      int32
		want            int64
		wantSkip        bool
	}{
		{"142 subtracts claimed boxes", actTypeBattlePassNew, 4200, 1000, 2, 5, 5, 2, false},
		{"27 does not", actTypeBattlePass, 4200, 1000, 2, 5, 5, 4, false},
		{"142 under one box after the subtraction", actTypeBattlePassNew, 2999, 1000, 2, 5, 5, 0, false},
		{"above the listed top level", actTypeBattlePass, 1000, 1000, 0, 6, 5, 1, false},
		{"one below the top", actTypeBattlePass, 9000, 1000, 0, 4, 5, 0, true},
	} {
		o := battlePassOverflowState(overflowInfo(c.level, c.top, c.exp, c.extra, c.inf), c.typ)
		if o.boxes != c.want || (o.skip != "") != c.wantSkip {
			t.Errorf("%s: boxes %d skip %q, want %d (skip %v)", c.name, o.boxes, o.skip, c.want, c.wantSkip)
		}
	}
	if o := battlePassOverflowState(withoutKey(overflowInfo(5, 5, 9000, 1000, 0), "battlePass"), actTypeBattlePass); o.boxes != 0 || o.skip == "" {
		t.Errorf("no battlePass: boxes %d skip %q, want none", o.boxes, o.skip)
	}
}

func TestBattlePassOverflowIsOptIn(t *testing.T) {
	f, ok := featureRegistry[battlePassOverflowFeature]
	if !ok || f.DefaultOn || len(f.Duel) != 0 || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature %+v: want registered, DefaultOn false, no duel scores, static-only summary", f)
	}
}
