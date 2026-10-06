package session

import (
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// TestStartHeartbeatPingsBeforeReturning covers MASTER.md 5.1 #20: the first keepalive goes out
// at once, before StartHeartbeat returns -- so before Login, as on the real client -- rather than
// one interval later. The payload stays the flat {clientTime} the server has always accepted.
func TestStartHeartbeatPingsBeforeReturning(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	envs := make(chan *Envelope, 4)
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			envs <- env
		}
	}()

	returned := make(chan struct{})
	go func() {
		client.StartHeartbeat(time.Hour, time.Now()) // no tick can fire during this test
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("StartHeartbeat did not return")
	}
	t.Cleanup(func() { _ = client.Close() })

	// The ping was written before StartHeartbeat returned, so it precedes anything sent after.
	if err := client.SendExtension("after.heartbeat.start", nil); err != nil {
		t.Fatalf("SendExtension: %v", err)
	}

	first := <-envs
	if first.Controller != ControllerSystem || first.Action != ActionPingPong {
		t.Fatalf("first envelope = c%d a%d, want the PingPong (c%d a%d)", first.Controller, first.Action, ControllerSystem, ActionPingPong)
	}
	if keys := first.Content.Keys(); len(keys) != 1 || keys[0] != "clientTime" {
		t.Errorf("ping payload keys = %v, want the flat [clientTime]", keys)
	}
	if v, ok := first.Content.Get("clientTime"); !ok || v.Type != sfs.SFSLong {
		t.Errorf("clientTime = %+v, want a Long", v)
	}

	select {
	case second := <-envs:
		if msg, ok := second.AsExtension(); !ok || msg.Cmd != "after.heartbeat.start" {
			t.Errorf("second envelope = %+v, want the extension sent after StartHeartbeat", second)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("extension sent after StartHeartbeat never arrived")
	}
}

// TestStartHeartbeatFirstPingFailureClosesConn: if even the first ping cannot be written, the
// connection is closed and no loop is started, the same handling a failed tick gets.
func TestStartHeartbeatFirstPingFailureClosesConn(t *testing.T) {
	client, _ := NewPipeGameConnPair(t)
	_ = client.conn.Close()

	client.StartHeartbeat(time.Millisecond, time.Now())

	select {
	case <-client.stopHeartbeat:
	default:
		t.Fatal("StartHeartbeat returned without closing the connection after its first ping failed")
	}
}

// TestFakeGameServerDropsClientPings: handlers behind StartFakeGameServer read the client's first
// request, not the heartbeat ping now sent ahead of it.
func TestFakeGameServerDropsClientPings(t *testing.T) {
	gotCmd := make(chan string, 1)
	addr := StartFakeGameServer(t, func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			gotCmd <- "read error: " + err.Error()
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			gotCmd <- "non-extension envelope"
			return
		}
		gotCmd <- msg.Cmd
	})

	client, err := DialGame(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("DialGame: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	client.StartHeartbeat(time.Hour, time.Now())
	if err := client.SendExtension("first.request", nil); err != nil {
		t.Fatalf("SendExtension: %v", err)
	}

	select {
	case cmd := <-gotCmd:
		if cmd != "first.request" {
			t.Errorf("fake server's first envelope = %q, want first.request", cmd)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never read a request")
	}
}
