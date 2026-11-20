package game

import (
	"maps"
	"slices"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// dParam is one request param a wire test expects: its SFS type tag and value.
type dParam struct {
	typ byte
	val any
}

func dInt(v int32) dParam      { return dParam{sfs.SFSInt, v} }
func dLong(v int64) dParam     { return dParam{sfs.SFSLong, v} }
func dStr(v string) dParam     { return dParam{sfs.SFSUtfString, v} }
func dBool(v bool) dParam      { return dParam{sfs.SFSBool, v} }
func dNone() map[string]dParam { return nil }

// dWire is one request a wire test expects, every part a literal from the 1.0.370 client.
type dWire struct {
	cmd    string
	params map[string]dParam
}

// dCheckWire asserts that fake received exactly want, in order: the command literals, the exact
// param key set (without the session's _id), and each param's type tag and value.
func dCheckWire(t *testing.T, fake *evFake, want []dWire) {
	t.Helper()
	got := fake.requests()
	var gotCmds, wantCmds []string
	for _, r := range got {
		gotCmds = append(gotCmds, r.Cmd)
	}
	for _, w := range want {
		wantCmds = append(wantCmds, w.cmd)
	}
	if !slices.Equal(gotCmds, wantCmds) {
		t.Fatalf("sent\n%q\nwant\n%q", gotCmds, wantCmds)
	}
	for i, w := range want {
		p := got[i].Params
		keys := session.ParamKeysWithoutID(p)
		slices.Sort(keys)
		if wantKeys := slices.Sorted(maps.Keys(w.params)); !slices.Equal(keys, wantKeys) {
			t.Errorf("request %d %s: param keys %q, want %q", i, w.cmd, keys, wantKeys)
		}
		for k, wp := range w.params {
			v, ok := p.Get(k)
			if !ok || v.Type != wp.typ || v.Val != wp.val {
				t.Errorf("request %d %s: %s = %#v (type %d, present %v), want %#v of type %d", i, w.cmd, k, v.Val, v.Type, ok,
					wp.val, wp.typ)
			}
		}
	}
}

// dThroneSchedule is crossThroneSchedule of cfg type typ whose initServerGroup.group holds only
// the given camps ("a", "b"; a nil list is an empty array).
func dThroneSchedule(typ int32, camps map[string][]int32) *sfs.SFSObject {
	s := crossThroneSchedule(typ, nil, nil)
	group := sfs.NewSFSObject()
	for k, servers := range camps {
		group.PutIntArray(k, servers)
	}
	s.PutSFSObject("initServerGroup", obj("group", group))
	return s
}

// TestSeasonWarBoxesWire pins what season-outpost-boxes and cross-throne-rewards send, as
// literals from the 1.0.370 client:
//   - outpost.battle.info {} (MsgDefines.lua:334; FetchOutpostBattleInfoMessage.lua:5-7, no params;
//     sent at WorldAllianceCityDataManager.lua:75-78 and FetchOutpostBattleRewardMessage.lua:15)
//   - outpost.claim.box.reward {index: Int i-1} (MsgDefines.lua:335; FetchOutpostBattleRewardMessage.lua:4-7;
//     UILWSeasonOutpostAttackS6PlayerReward.lua:253)
//   - cross.throne.schedule {} (MsgDefines.lua:1359; CrossKingScheduleMessage.lua:4-6; ZoneWarManager.lua:60)
//   - cross.throne.fight.info {} (MsgDefines.lua:1363; CrossKingFightInfoMessage.lua:4-6; ZoneWarManager.lua:118)
//   - cross.throne.claim.server.win.reward {} (MsgDefines.lua:1366; CrossKingServerRewardWinMessage.lua:4-6;
//     UIServerBattleLastKingZoneReward.lua:83)
//   - cross.throne.claim.person.score.reward {index: Int i-1} (MsgDefines.lua:1364;
//     CrossKingPersonScoreRewardMessage.lua:4-7; UIServerBattleLastKingPlayerReward.lua:234)
//
// Activity 1200108 is type 266 (SeasonWarZoneOutpostAttack) in table activity 40413.
func TestSeasonWarBoxesWire(t *testing.T) {
	withEvNow(t, evTestNow)
	t.Run("season-outpost-boxes", func(t *testing.T) {
		conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "outpost.battle.info" {
				return obj("user", seasonScoreUser(500, []int64{100, 300, 500, 900}, []int32{0}))
			}
			return obj("reward", []*sfs.SFSObject{})
		})
		if err := runSeasonOutpostBoxes(conn, evWithSeason(evInit(30, evActivity(1200108)), 6)); err != nil {
			t.Fatal(err)
		}
		dCheckWire(t, fake, []dWire{
			{"outpost.battle.info", dNone()},
			{"outpost.claim.box.reward", map[string]dParam{"index": dInt(1)}},
			{"outpost.claim.box.reward", map[string]dParam{"index": dInt(2)}},
		})
	})
	t.Run("cross-throne-rewards", func(t *testing.T) {
		conn, fake := startEvFake(t, func(cmd string, _ *sfs.SFSObject) *sfs.SFSObject {
			switch cmd {
			case "cross.throne.schedule":
				return crossThroneSchedule(4, nil, nil)
			case "cross.throne.fight.info":
				return obj("curVsRound", []*sfs.SFSObject{obj("serverId", 900, "win", 0), obj("serverId", 812, "win", 1)},
					"user", seasonScoreUser(250, []int64{100, 200, 300}, []int32{0}, "winServerReward", 0))
			}
			return obj("reward", []*sfs.SFSObject{})
		})
		in := evInit(30)
		in.Raw.PutSFSObject("user", obj("serverId", 812))
		if err := runCrossThroneRewards(conn, in); err != nil {
			t.Fatal(err)
		}
		dCheckWire(t, fake, []dWire{
			{"cross.throne.schedule", dNone()},
			{"cross.throne.fight.info", dNone()},
			{"cross.throne.claim.server.win.reward", dNone()},
			{"cross.throne.claim.person.score.reward", map[string]dParam{"index": dInt(1)}},
		})
	})
}
