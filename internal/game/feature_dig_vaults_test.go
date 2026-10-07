package game

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// vaultFake is a scripted vault server for one game: every list or info request answers from the
// vaults map; an open marks the stone as the player's own and, when it lies on a relic, sets
// rewardState 1 (else 2); a claim sets rewardState 2.
type vaultFake struct {
	mu     sync.Mutex
	vaults map[int64]*vaultState
	noAct  bool // alliance.boss.act.info answers E100172
}

type vaultState struct {
	uuid, cfg, start, end, rewardState int64
	typ                                int32
	uid                                string
	opened                             map[int64]string
	blocks                             [][2]int64 // {bid, pos}
}

func newVault(uuid, cfg int64) *vaultState {
	return &vaultState{
		uuid: uuid, cfg: cfg, opened: map[int64]string{},
		start: evTestNow.Add(-time.Hour).UnixMilli(), end: evTestNow.Add(time.Hour).UnixMilli(),
	}
}

func (v *vaultState) info() *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutLong("uuid", v.uuid)
	r.PutInt("mapConfigId", int32(v.cfg))
	if v.typ != 0 {
		r.PutInt("type", v.typ)
	}
	if v.uid != "" {
		r.PutUtfString("uid", v.uid)
	}
	r.PutInt("rewardState", int32(v.rewardState))
	open := sfs.NewSFSArray()
	for _, pos := range slices.Sorted(func(yield func(int64) bool) {
		for p := range v.opened {
			if !yield(p) {
				return
			}
		}
	}) {
		e := sfs.NewSFSObject()
		e.PutInt("pos", int32(pos))
		pi := sfs.NewSFSObject()
		pi.PutUtfString("uid", v.opened[pos])
		e.PutSFSObject("playerInfo", pi)
		open.AddSFSObject(e)
	}
	r.PutSFSArray("allianceOpenInfo", open)
	blocks := sfs.NewSFSArray()
	for _, b := range v.blocks {
		e := sfs.NewSFSObject()
		e.PutInt("bid", int32(b[0]))
		e.PutInt("pos", int32(b[1]))
		blocks.AddSFSObject(e)
	}
	r.PutSFSArray("blockInfo", blocks)
	return r
}

func (f *vaultFake) find(p *sfs.SFSObject) *vaultState {
	if p.Has("uuid") {
		return f.vaults[p.GetLong("uuid")]
	}
	for _, v := range f.vaults {
		return v
	}
	return nil
}

func (f *vaultFake) reply(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch cmd {
	case seasonDigListCmd, offSeasonDigListCmd:
		r := sfs.NewSFSObject()
		l := sfs.NewSFSArray()
		for _, id := range slices.Sorted(func(yield func(int64) bool) {
			for k := range f.vaults {
				if !yield(k) {
					return
				}
			}
		}) {
			v := f.vaults[id]
			e := sfs.NewSFSObject()
			e.PutLong("uuid", v.uuid)
			e.PutInt("mapConfigId", int32(v.cfg))
			e.PutLong("startTime", v.start)
			e.PutLong("endTime", v.end)
			e.PutInt("rewardState", int32(v.rewardState))
			l.AddSFSObject(e)
		}
		r.PutSFSArray("mapList", l)
		return r
	case allianceBossActCmd:
		if f.noAct {
			return evErr(allianceBossNoActCode)
		}
		r := sfs.NewSFSObject()
		if v := f.find(sfs.NewSFSObject()); v != nil {
			dg := sfs.NewSFSObject()
			dg.PutLong("uuid", v.uuid)
			dg.PutLong("endTime", v.end)
			dg.PutInt("rewardState", int32(v.rewardState))
			r.PutSFSObject("digGameInfo", dg)
		}
		return r
	case seasonDigInfoCmd, offSeasonDigInfoCmd, allianceBossInfoCmd:
		if v := f.find(p); v != nil {
			return v.info()
		}
		return evOK()
	case seasonDigOpenCmd, offSeasonDigOpenCmd, allianceBossOpenCmd:
		v := f.find(p)
		pos := int64(p.GetInt("pos"))
		v.opened[pos] = "1001"
		v.rewardState = 2
		vv := &digVault{layout: digVaultMap{alliance: true, width: 10, height: 12}}
		for _, b := range v.blocks {
			if slices.Contains(vv.cells(digVaultBlock{b[0], b[1]}), pos) {
				v.rewardState = 1
			}
		}
		return sfs.NewSFSObject()
	case seasonDigBlockCmd, offSeasonDigBlockCmd, allianceBossRewardCmd, seasonDigPersonalCmd:
		if v := f.find(p); v != nil {
			v.rewardState = 2
		}
		r := sfs.NewSFSObject()
		rw := sfs.NewSFSArray()
		rw.AddSFSObject(sfs.NewSFSObject())
		r.PutSFSArray("reward", rw)
		return r
	}
	return evOK()
}

func TestOffSeasonDigOpensRevealedRelicAndClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	v := newVault(7001, 15001)
	v.opened[1] = "2002"
	v.opened[50] = "2003"
	v.blocks = [][2]int64{{60013, 50}, {60011, 1}} // 1x1 at 50 (complete); 2x2 at 1: stones 1, 2, 11, 12
	f := &vaultFake{vaults: map[int64]*vaultState{7001: v}}
	conn, fake := startEvFake(t, f.reply)
	if err := runOffSeasonDigs(conn, digsInit(0, evActivity(80052)), false); err != nil {
		t.Fatal(err)
	}
	want := "offseason.dig.activity.info,offseason.dig.game.info,offseason.dig.game.open,offseason.dig.game.info," +
		"offseason.dig.game.get.alliance.block.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	open := fake.only(offSeasonDigOpenCmd)[0].Params
	if open.GetLong("uuid") != 7001 || open.GetInt("pos") != 2 {
		t.Errorf("open = %v, want uuid 7001 pos 2 (the relic's first unopened stone)", open)
	}
	claim := fake.only(offSeasonDigBlockCmd)[0].Params
	if claim.GetLong("uuid") != 7001 || claim.GetInt("pos") != 2 {
		t.Errorf("claim = %v, want uuid 7001 pos 2 (the own stone)", claim)
	}
	digsAssertSafe(t, fake)
}

func TestDigVaultSkipsAndOnlyClaimsWhatTheClientShows(t *testing.T) {
	withEvNow(t, evTestNow)
	dugNoReward := newVault(1, 15002) // own stone open, nothing to claim: no second dig
	dugNoReward.opened[5] = "1001"
	finished := newVault(2, 15003) // listed rewardState 2: not even read
	finished.rewardState = 2
	offRelic := newVault(3, 15004) // rewardState 1 but the own stone is on no revealed relic: no claim
	offRelic.rewardState = 1
	offRelic.opened[100] = "1001"
	offRelic.blocks = [][2]int64{{60013, 1}}
	expired := newVault(4, 15005)
	expired.end = evTestNow.Add(-time.Minute).UnixMilli()
	notYet := newVault(5, 15006)
	notYet.start = evTestNow.Add(time.Minute).UnixMilli()
	f := &vaultFake{vaults: map[int64]*vaultState{1: dugNoReward, 2: finished, 3: offRelic, 4: expired, 5: notYet}}
	conn, fake := startEvFake(t, f.reply)
	if err := runOffSeasonDigs(conn, digsInit(0, evActivity(80052)), false); err != nil {
		t.Fatal(err)
	}
	want := "offseason.dig.activity.info,offseason.dig.game.info,offseason.dig.game.info"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	for _, r := range fake.only(offSeasonDigInfoCmd) {
		if u := r.Params.GetLong("uuid"); u != 1 && u != 3 {
			t.Errorf("read vault %d", u)
		}
	}
}

func TestDigVaultBlindPickWithNoRevealedRelic(t *testing.T) {
	withEvNow(t, evTestNow)
	v := newVault(9, 15001)
	v.opened[1] = "2002"
	f := &vaultFake{vaults: map[int64]*vaultState{9: v}}
	conn, fake := startEvFake(t, f.reply)
	if err := runOffSeasonDigs(conn, digsInit(0, evActivity(80052)), false); err != nil {
		t.Fatal(err)
	}
	if pos := fake.only(offSeasonDigOpenCmd)[0].Params.GetInt("pos"); pos != 2 {
		t.Errorf("opened stone %d, want 2 (the first unopened)", pos)
	}
	if n := len(fake.only(offSeasonDigBlockCmd)); n != 0 {
		t.Errorf("claimed %d times after a miss", n)
	}
}

func TestSeasonDigClaimsPersonalAndOpensAllianceVault(t *testing.T) {
	withEvNow(t, evTestNow)
	single := newVault(11, 10001)
	single.rewardState = 1
	single.typ, single.uid = digVaultSingle, "1001"
	singleOpen := newVault(12, 10002) // a personal vault still being dug: hammers are never spent
	alliance := newVault(13, 10043)
	alliance.typ, alliance.uid = digVaultAlliance, "3003"
	alliance.blocks = [][2]int64{{10013, 7}}
	f := &vaultFake{vaults: map[int64]*vaultState{11: single, 12: singleOpen, 13: alliance}}
	conn, fake := startEvFake(t, f.reply)
	in := evWithSeason(digsInit(0, evActivity(1000070)), 3)
	if err := runSeasonDigs(conn, in, false); err != nil {
		t.Fatal(err)
	}
	want := "season.dig.activity.info,season.dig.game.info,season.dig.game.get.personal.reward," +
		"season.dig.game.info,season.dig.game.open,season.dig.game.info,season.dig.game.get.alliance.block.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	infos := fake.only(seasonDigInfoCmd)
	if p := infos[0].Params; p.GetInt("type") != digVaultSingle || p.GetLong("uuid") != 11 || p.GetString("uid") != "1001" {
		t.Errorf("personal info params = %v", p)
	}
	if p := infos[1].Params; p.GetInt("type") != digVaultAlliance || p.GetLong("uuid") != 13 || p.GetString("uid") != "1001" {
		t.Errorf("alliance info params = %v, want type 2 and the player's uid", p)
	}
	open := fake.only(seasonDigOpenCmd)[0].Params
	if open.GetInt("type") != digVaultAlliance || open.GetString("uid") != "3003" || open.GetInt("pos") != 7 {
		t.Errorf("open = %v, want the reply's type and uid and the revealed relic's stone 7", open)
	}
	if p := fake.only(seasonDigBlockCmd)[0].Params; p.GetLong("uuid") != 13 || p.GetInt("pos") != 7 {
		t.Errorf("relic claim = %v", p)
	}
	digsAssertSafe(t, fake)
}

func TestSeasonDigNeedsSeasonAndActivity(t *testing.T) {
	withEvNow(t, evTestNow)
	f := &vaultFake{vaults: map[int64]*vaultState{1: newVault(1, 10043)}}
	conn, fake := startEvFake(t, f.reply)
	ended := evActivity(1000070)
	ended.PutLong("endTime", evTestNow.Add(-time.Hour).UnixMilli())
	for _, in := range []*Init{
		digsInit(0, evActivity(1000070)),    // no season
		evWithSeason(digsInit(0), 3),        // no activity
		evWithSeason(digsInit(0, ended), 3), // activity over
		nil,
	} {
		if err := runSeasonDigs(conn, in, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(fake.requests()); n != 0 {
		t.Errorf("sent %v", fake.cmds())
	}
	if err := runOffSeasonDigs(conn, digsInit(0), false); err != nil || len(fake.requests()) != 0 {
		t.Errorf("off-season without its activity: err %v, sent %v", err, fake.cmds())
	}
}

func TestAllianceBossDigOpensAndClaims(t *testing.T) {
	withEvNow(t, evTestNow)
	v := newVault(21, 16001)
	v.blocks = [][2]int64{{70003, 33}} // 2x2 at 33: 33, 34, 43, 44
	v.opened[33] = "2002"
	f := &vaultFake{vaults: map[int64]*vaultState{21: v}}
	conn, fake := startEvFake(t, f.reply)
	if err := runAllianceBossDigs(conn, digsInit(0, evActivity(94301)), false); err != nil {
		t.Fatal(err)
	}
	want := "alliance.boss.act.info,alliance.boss.dig.game.info,alliance.boss.dig.game.open,alliance.boss.dig.game.info," +
		"alliance.boss.dig.game.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
	if s := fake.only(allianceBossActCmd)[0].Params.GetInt("serverId"); s != 777 {
		t.Errorf("serverId = %d, want init user.serverId", s)
	}
	if p := fake.only(allianceBossRewardCmd)[0].Params; p.GetLong("uuid") != 21 || p.GetInt("pos") != 34 {
		t.Errorf("reward = %v, want uuid 21 pos 34", p)
	}
	digsAssertSafe(t, fake)
}

func TestAllianceBossDigGates(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, (&vaultFake{vaults: map[int64]*vaultState{}, noAct: true}).reply)
	noAlliance := digsInit(0, evActivity(94301))
	noAlliance.Object("user").PutUtfString("allianceId", "")
	if err := runAllianceBossDigs(conn, noAlliance, false); err != nil {
		t.Fatal(err)
	}
	if err := runAllianceBossDigs(conn, digsInit(0), false); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.requests()); n != 0 {
		t.Fatalf("no alliance / no Alliance Drill: sent %v", fake.cmds())
	}
	if err := runAllianceBossDigs(conn, digsInit(0, evActivity(94301)), false); err != nil {
		t.Fatalf("E100172 is benign: %v", err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "alliance.boss.act.info" {
		t.Errorf("E100172: sent %s", got)
	}
	// A vault past its end, or no digGameInfo at all: only the act info.
	ended := newVault(5, 16001)
	ended.end = evTestNow.Add(-time.Second).UnixMilli()
	for _, f := range []*vaultFake{{vaults: map[int64]*vaultState{5: ended}}, {vaults: map[int64]*vaultState{}}} {
		conn, fake := startEvFake(t, f.reply)
		if err := runAllianceBossDigs(conn, digsInit(0, evActivity(94301)), false); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(fake.cmds(), ","); got != "alliance.boss.act.info" {
			t.Errorf("sent %s", got)
		}
	}
}

func TestDigVaultPlansAreReadOnly(t *testing.T) {
	withEvNow(t, evTestNow)
	mk := func(uuid, cfg int64) *vaultState {
		v := newVault(uuid, cfg)
		v.blocks = [][2]int64{{60011, 1}, {10011, 1}, {70001, 1}}
		return v
	}
	claimable := func(uuid, cfg int64) *vaultState {
		v := mk(uuid, cfg)
		v.rewardState = 1
		v.opened[1] = "1001"
		return v
	}
	for name, run := range map[string]func(*vaultFake) *evFake{
		"season": func(f *vaultFake) *evFake {
			conn, fake := startEvFake(t, f.reply)
			if err := runSeasonDigs(conn, evWithSeason(digsInit(0, evActivity(1000070)), 3), true); err != nil {
				t.Fatal(err)
			}
			return fake
		},
		"offseason": func(f *vaultFake) *evFake {
			conn, fake := startEvFake(t, f.reply)
			if err := runOffSeasonDigs(conn, digsInit(0, evActivity(80052)), true); err != nil {
				t.Fatal(err)
			}
			return fake
		},
		"boss": func(f *vaultFake) *evFake {
			conn, fake := startEvFake(t, f.reply)
			if err := runAllianceBossDigs(conn, digsInit(0, evActivity(94301)), true); err != nil {
				t.Fatal(err)
			}
			return fake
		},
	} {
		cfg := map[string]int64{"season": 10043, "offseason": 15001, "boss": 16001}[name]
		for _, f := range []*vaultFake{
			{vaults: map[int64]*vaultState{1: mk(1, cfg)}},
			{vaults: map[int64]*vaultState{1: claimable(1, cfg)}},
		} {
			fake := run(f)
			if len(fake.requests()) == 0 {
				t.Errorf("%s plan sent nothing", name)
			}
			digsAssertReadOnly(t, fake)
		}
	}
}

func TestDigVaultGeometry(t *testing.T) {
	v := &digVault{layout: digVaultMap{alliance: true, width: 10, height: 12}, opened: map[int64]string{}}
	if got := v.cells(digVaultBlock{10019, 9}); !slices.Equal(got, []int64{9, 10, 19, 20, 29, 30, 39, 40}) {
		t.Errorf("3x4 relic at 9 clipped to the 10-wide grid: %v", got)
	}
	if got := v.cells(digVaultBlock{99999, 1}); got != nil {
		t.Errorf("unknown relic: %v", got)
	}
	v.opened[12] = "1001"
	v.blocks = []digVaultBlock{{10011, 1}}
	if v.claimPos("1001") != 12 || v.claimPos("2002") != 0 {
		t.Error("claimPos must be the own stone on a revealed relic")
	}
	v.opened[1], v.opened[2], v.opened[11] = "x", "x", "x"
	if pos, _ := v.pickStone(); pos != 3 {
		t.Errorf("pickStone = %d, want 3 (relic complete, first unopened)", pos)
	}
}

func TestDigVaultClaimFailureKeepsGoing(t *testing.T) {
	withEvNow(t, evTestNow)
	mk := func(uuid int64) *vaultState {
		v := newVault(uuid, 15001)
		v.rewardState = 1
		v.opened[3] = "1001"
		v.blocks = [][2]int64{{60013, 3}}
		return v
	}
	f := &vaultFake{vaults: map[int64]*vaultState{1: mk(1), 2: mk(2)}}
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == offSeasonDigBlockCmd && p.GetLong("uuid") == 1 {
			return evErr("E9")
		}
		return f.reply(cmd, p)
	})
	err := runOffSeasonDigs(conn, digsInit(0, evActivity(80052)), false)
	if err == nil || !strings.Contains(err.Error(), "E9") {
		t.Fatalf("err = %v, want the failed claim reported", err)
	}
	claims := fake.only(offSeasonDigBlockCmd)
	if len(claims) != 2 || claims[1].Params.GetLong("uuid") != 2 {
		t.Errorf("sent %v, want vault 2 claimed after vault 1 failed", fake.cmds())
	}
}
