package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func t11Main(pool bool) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	info := sfs.NewSFSObject()
	if pool {
		info.PutSFSArray("rewardPool", freebieReward())
	} else {
		info.PutSFSArray("rewardPool", sfs.NewSFSArray())
	}
	r.PutSFSObject("idleGameIdleInfo", info)
	return r
}

func TestT11IdleExpeditionCollectsOnlyANonEmptyPool(t *testing.T) {
	for _, c := range []struct {
		name string
		pool bool
		want string
	}{
		{"pool", true, "idle.game.main,idle.game.reward.receive"},
		{"empty pool", false, "idle.game.main"},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == t11MainCmd {
				return t11Main(c.pool)
			}
			return evOK()
		})
		if err := runT11IdleExpedition(conn, evAllianceInit("t11_idle_game_open")); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(fake.cmds(), ","); got != c.want {
			t.Errorf("%s: sent %s, want %s", c.name, got, c.want)
		}
		for _, cmd := range fake.cmds() {
			if cmd == "idle.game.end" || cmd == "idle.game.start" {
				t.Errorf("%s: sent %s; the feature must never end or start a run", c.name, cmd)
			}
		}
	}
}

func TestT11IdleExpeditionSwitchOffLockedAndFailure(t *testing.T) {
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E_LOCKED") })
	if err := runT11IdleExpedition(conn, evAllianceInit()); err != nil || len(fake.requests()) != 0 {
		t.Errorf("switch off: err %v, sent %v", err, fake.cmds())
	}
	if err := runT11IdleExpedition(conn, evAllianceInit("t11_idle_game_open")); err != nil {
		t.Errorf("an errorCode on main means not available, got %v", err)
	}
	conn2, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == t11MainCmd {
			return t11Main(true)
		}
		return evErr("E7")
	})
	if err := runT11IdleExpedition(conn2, evAllianceInit("t11_idle_game_open")); err == nil || !strings.Contains(err.Error(), "E7") {
		t.Errorf("receive failure: err = %v", err)
	}
	conn3, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == t11MainCmd {
			return t11Main(true)
		}
		return evErr(evAlreadyExecuted)
	})
	if err := runT11IdleExpedition(conn3, evAllianceInit("t11_idle_game_open")); err != nil {
		t.Errorf("already collected must be benign, got %v", err)
	}
}
