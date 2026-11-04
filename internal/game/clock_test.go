package game

import (
	"slices"
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
	t.Cleanup(func() { _ = client.Close(); clockConn.Store(nil) })
	go func() {
		ping, err := server.ReadEnvelope()
		if err != nil || ping.Content == nil {
			return
		}
		pong := sfs.NewSFSObject()
		pong.PutLong("serverTime", time.Now().Add(offset).UnixMilli())
		pong.PutLong("clientTime", evObject(ping.Content, "p").GetLong("clientTime")) // the ping nests it
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

// The tests below put a fixture time an hour after the host clock and an hour before the server
// clock (syncedPipe's +2 h), so only a gate that reads the server clock passes it.

// collectCore's own gates and GreetVisitors read the session clock: the Smelter's 30-minute tick
// and the visitor's arrival are past only on the server clock. It also points the game time at the
// session, since CollectAll reaches it without Collect.
func TestCollectCoreReadsTheServerClock(t *testing.T) {
	t.Cleanup(func() { clockConn.Store(nil) })
	clockConn.Store(nil)
	client, server := syncedPipe(t, 2*time.Hour)
	host := time.Now()
	raw := obj(
		"tomorrow", host.Add(3*time.Hour).Unix(),
		"allianceNewMail", 0,
		"user", obj("uid", "1001"),
		"vip", obj("vipInfo", obj("loginScoreState", 1, "everyDayReward", 1, "endTime", host.Add(24*time.Hour).Unix())),
	)
	smelter := productionBuilding(12, BuildingSmelter, 5, host.Add(-time.Minute), host.Add(-time.Minute), host.Add(time.Hour))
	visitor := newTimedTestVisitor(21, 2003, host.Add(time.Hour)) // a GIFT visitor
	in := &Init{Raw: raw, Buildings: []Building{smelter}, Visitors: []Visitor{visitor}}
	srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
		switch msg.Cmd {
		case "chat.get.system.mails":
			return "push.chat.get.system.mails", nil
		case "building.production.collect":
			return "", obj("status", 1)
		}
		return "", nil
	})
	if err := collectCore(client, in); err != nil {
		t.Fatalf("collectCore() = %v, want nil", err)
	}
	if got := srv.requests("building.production.collect"); len(got) != 1 || got[0].Params.GetLong("uuid") != 12 {
		t.Errorf("collected %v, want the Smelter, whose tick is done on the server clock", got)
	}
	if got := srv.requests("visitor.operate"); len(got) != 1 {
		t.Errorf("greeted %d visitors, want the one that has arrived on the server clock", len(got))
	}
	if clockConn.Load() != client {
		t.Error("collectCore did not point the game time at its session")
	}
}

// visitor-fresh waits while a queued visitor is still arriving; the queued one has arrived on the
// server clock.
func TestVisitorFreshReadsTheServerClock(t *testing.T) {
	client, server := syncedPipe(t, 2*time.Hour)
	in := freshTestInit(5, int32(1), queuedTestVisitor(1, 1001, 0, time.Now().Add(time.Hour)))
	srv := serveScripted(server, func(*session.ExtensionMessage) (string, *sfs.SFSObject) { return "", nil })
	if err := runVisitorFresh(client, in); err != nil {
		t.Fatalf("runVisitorFresh() = %v, want nil", err)
	}
	if got := srv.cmds(); !slices.Equal(got, []string{"visitor.fresh"}) {
		t.Errorf("sent %v, want visitor.fresh", got)
	}
}

// activity-visitors claims only while the event runs; it ended an hour ago on the server clock.
func TestActivityVisitorsReadTheServerClock(t *testing.T) {
	client, server := syncedPipe(t, 2*time.Hour)
	in := initWithActivityVisitors(map[string]*sfs.SFSObject{
		"94309": activityVisitorEntry(0, time.Now().Add(time.Hour), 4006),
	}, "94309")
	srv := serveScripted(server, func(*session.ExtensionMessage) (string, *sfs.SFSObject) { return "", nil })
	if err := runActivityVisitors(client, in); err != nil {
		t.Fatalf("runActivityVisitors() = %v, want nil", err)
	}
	if got := srv.cmds(); len(got) != 0 {
		t.Errorf("sent %v for an event that has ended on the server clock", got)
	}
}

// cmdBaseTime is a game time the server sees (the fireworks scan's timeStamp, from evNow, is the
// other): the client sends its server time (UIAllianceHelpCtrl.lua:119-120).
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
