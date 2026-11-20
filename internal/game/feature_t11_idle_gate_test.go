package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// t11OpenInit is evAllianceInit plus what IsT11IdleGameFunctionOn needs past the switch: a
// military camp at level 27 and season 5 (T11IdleGameManager.lua:64-82, idle_game_para k1 "27;4;64").
func t11OpenInit(switches ...string) *Init {
	return t11GateInit(27, 5, 0, switches...)
}

// t11GateInit is evAllianceInit with a military camp at campLevel (none when 0), init
// playerServerSeasonInfo for season (none when < 0) and seasonStartTime seasonDay days back (none
// when 0).
func t11GateInit(campLevel int32, season int32, seasonDay int, switches ...string) *Init {
	in := evAllianceInit(switches...)
	if campLevel > 0 {
		in.Buildings = append(in.Buildings, NewTestBuilding(2, t11MilitaryCampID, campLevel-1), NewTestBuilding(3, t11MilitaryCampID, campLevel))
	}
	if season >= 0 {
		p := sfs.NewSFSObject()
		p.PutInt("seasonId", season)
		if seasonDay > 0 {
			// day d is (now - start) // 1 day + 1, so start sits d-1 days and an hour back
			p.PutLong("seasonStartTime", evTestNow.Add(-time.Duration(seasonDay-1)*24*time.Hour-time.Hour).UnixMilli())
		}
		in.Raw.PutSFSObject("playerServerSeasonInfo", p)
	}
	return in
}

// B60: below the camp/season gate the client never sends idle.game.main (T11IdleGameManager.lua:
// 64-82); a gate input missing from init keeps the old behaviour (send, errorCode = unavailable).
func TestT11GateBeforeTheMainRead(t *testing.T) {
	withEvNow(t, evTestNow)
	noBuildings := t11GateInit(0, 5, 0, "t11_idle_game_open")
	noBuildings.Buildings = nil
	for _, c := range []struct {
		name string
		in   *Init
		send bool
	}{
		{"camp 26", t11GateInit(26, 5, 0, "t11_idle_game_open"), false},
		{"no camp", t11GateInit(0, 5, 0, "t11_idle_game_open"), false},
		{"season 3", t11GateInit(27, 3, 0, "t11_idle_game_open"), false},
		{"season 4 day 63", t11GateInit(27, 4, 63, "t11_idle_game_open"), false},
		{"season 4 day 64", t11GateInit(27, 4, 64, "t11_idle_game_open"), true},
		{"season 5", t11GateInit(27, 5, 0, "t11_idle_game_open"), true},
		{"unknown: no playerServerSeasonInfo", t11GateInit(27, -1, 0, "t11_idle_game_open"), true},
		{"unknown: season 4 without a start", t11GateInit(27, 4, 0, "t11_idle_game_open"), true},
		{"unknown: no buildings", noBuildings, true},
		{"switch off", t11GateInit(27, 5, 0), false},
	} {
		for _, run := range []func(*Init) (string, error){
			func(in *Init) (string, error) {
				conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return t11Main(false) })
				err := runT11IdleExpedition(conn, in)
				return strings.Join(fake.cmds(), ","), err
			},
			func(in *Init) (string, error) {
				conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return sfs.NewSFSObject() })
				err := runT11IdleEvents(conn, in)
				return strings.Join(fake.cmds(), ","), err
			},
		} {
			got, err := run(c.in)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if sent := got == t11MainCmd; sent != c.send {
				t.Errorf("%s: sent %q, want main read %v", c.name, got, c.send)
			}
		}
	}
}

// B60: the pool is read from changeLevel.idleGameMain.idleGameIdleInfo when the reply has one, as
// the client applies it after the top level (T11IdleGameDataManager.lua:97-112).
func TestT11IdleExpeditionPrefersTheChangeLevelPool(t *testing.T) {
	withEvNow(t, evTestNow)
	withChange := func(top, changed bool) *sfs.SFSObject {
		r := t11Main(top)
		info := sfs.NewSFSObject()
		if changed {
			info.PutSFSArray("rewardPool", freebieReward())
		} else {
			info.PutSFSArray("rewardPool", sfs.NewSFSArray())
		}
		m := sfs.NewSFSObject()
		m.PutSFSObject("idleGameIdleInfo", info)
		cl := sfs.NewSFSObject()
		cl.PutSFSObject("idleGameMain", m)
		r.PutSFSObject("changeLevel", cl)
		return r
	}
	for _, c := range []struct {
		name  string
		reply *sfs.SFSObject
		want  string
	}{
		{"top empty, changeLevel pool", withChange(false, true), "idle.game.main,idle.game.reward.receive"},
		{"top pool, changeLevel empty", withChange(true, false), "idle.game.main"},
		{"top pool, no changeLevel", t11Main(true), "idle.game.main,idle.game.reward.receive"},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == t11MainCmd {
				return c.reply
			}
			return evOK()
		})
		if err := runT11IdleExpedition(conn, t11OpenInit("t11_idle_game_open")); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(fake.cmds(), ","); got != c.want {
			t.Errorf("%s: sent %s, want %s", c.name, got, c.want)
		}
	}
}
