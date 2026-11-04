package game

import (
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// syncedPipe is a client/server pair whose client clock is synced from one pong, from a server
// whose clock is offset from the host's.
func syncedPipe(t *testing.T, offset time.Duration) (client, server *session.GameConn) {
	t.Helper()
	client, server = session.NewPipeGameConnPair(t)
	t.Cleanup(func() { _ = client.Close() })
	go func() {
		ping, err := server.ReadEnvelope()
		if err != nil || ping.Content == nil {
			return
		}
		pong := sfs.NewSFSObject()
		pong.PutLong("serverTime", time.Now().Add(offset).UnixMilli())
		pong.PutLong("clientTime", ping.Content.GetLong("clientTime"))
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionPingPong, pong)
	}()
	client.StartHeartbeat(time.Hour, time.Now())
	if _, err := client.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ServerNow(); !ok {
		t.Fatal("the pong did not sync the client's clock")
	}
	return client, server
}

func TestGameTimeIsTheSessionsServerClock(t *testing.T) {
	t.Cleanup(func() { clockConn.Store(nil) })
	near := func(name string, got time.Time, want time.Duration) {
		t.Helper()
		if d := time.Until(got); (d - want).Abs() > 2*time.Second {
			t.Errorf("%s is %v from the host clock, want %v", name, d, want)
		}
	}
	clockConn.Store(nil)
	near("gameNow without a session", gameNow(), 0)

	client, _ := syncedPipe(t, -time.Hour)
	near("gameNow before useServerClock", gameNow(), 0)
	useServerClock(client)
	near("gameNow", gameNow(), -time.Hour)
	near("claimNow", claimNow(nil), -time.Hour)
	near("evNow", evNow(), -time.Hour)
	near("connNow", connNow(client), -time.Hour)
	near("connNow without a conn", connNow(nil), 0)

	unsynced, _ := session.NewPipeGameConnPair(t)
	near("connNow before the first pong", connNow(unsynced), 0)
}

func TestRunFeaturesUsesTheSessionClock(t *testing.T) {
	t.Cleanup(func() { clockConn.Store(nil) })
	clockConn.Store(nil)
	client, _ := session.NewPipeGameConnPair(t)
	_ = RunFeatures(client, &Init{}, FeatureConfig{}, "no-such-feature")
	if clockConn.Load() != client {
		t.Error("RunFeatures did not point the game time at its session")
	}
}

// cmdBaseTime is the one game time on the wire: the client sends its server time
// (UIAllianceHelpCtrl.lua:119-120).
func TestHelpAllianceMembersSendsTheServerTime(t *testing.T) {
	client, server := syncedPipe(t, 2*time.Hour)
	got := make(chan int64, 1)
	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			got <- 0
			return
		}
		msg, _ := env.AsExtension()
		got <- msg.Params.GetLong("cmdBaseTime")
		_ = server.SendExtension(msg.Cmd, sfs.NewSFSObject())
	}()
	if err := HelpAllianceMembers(client); err != nil {
		t.Fatal(err)
	}
	want := time.Now().Add(2 * time.Hour).UnixMilli()
	if d := <-got - want; d < -2000 || d > 2000 {
		t.Errorf("cmdBaseTime is %d ms from the server's clock", d)
	}
}
