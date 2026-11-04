package session

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// The sample rules are Timer.SyncServerTime's (Assembly-CSharp.decompiled.cs:66043-66060).
func TestServerClockSampleRules(t *testing.T) {
	var k serverClock
	start := time.Now().Add(-time.Minute)
	at := func(ms int64) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }

	k.sync(at(1000), 5_000, 700) // before StartHeartbeat: no basis for clientTime
	if _, ok := k.now(); ok || k.samples != 0 {
		t.Fatalf("a pong before setStart synced the clock (samples %d)", k.samples)
	}
	k.setStart(start)

	steps := []struct {
		name               string
		recv, server, sent int64
		wantBase, wantRTT  int64
	}{
		{"the first pong is taken", 1_000, 50_000, 700, 50_150, 300},
		{"a slower pong over 200 ms is not", 5_000, 54_000, 4_500, 50_150, 300},
		{"a faster one is", 9_000, 58_000, 8_750, 58_125, 250},
		{"so is any pong under 200 ms", 13_000, 62_000, 12_900, 62_050, 100},
		{"even one slower than the best", 17_000, 66_000, 16_850, 66_075, 150},
		{"a clientTime from the future is ignored", 21_000, 70_000, 21_500, 66_075, 150},
	}
	for _, s := range steps {
		k.sync(at(s.recv), s.server, s.sent)
		if k.base != s.wantBase || k.minRTT != s.wantRTT {
			t.Errorf("%s: base %d rtt %d, want %d and %d", s.name, k.base, k.minRTT, s.wantBase, s.wantRTT)
		}
	}
	if k.samples != 5 {
		t.Errorf("samples %d, want 5 (the future clientTime is not one)", k.samples)
	}
}

func TestServerClockRunsOnFromTheSample(t *testing.T) {
	var k serverClock
	if now, ok := k.now(); ok || time.Since(now).Abs() > time.Second {
		t.Fatalf("an unsynced clock is %v (ok %v), want the host clock and false", now, ok)
	}
	recv := time.Now()
	k.setStart(recv.Add(-10 * time.Second))
	// Sent 100 ms before it came back, from a server one minute ahead of the host.
	k.sync(recv, recv.Add(time.Minute).UnixMilli(), 10_000-100)
	now, ok := k.now()
	if !ok {
		t.Fatal("the clock is not synced after a pong")
	}
	want := time.Minute + 50*time.Millisecond
	if got := time.Until(now); (got - want).Abs() > 500*time.Millisecond {
		t.Errorf("server minus host is %v, want about %v", got, want)
	}
}

// A pong read off the connection syncs ServerNow; another envelope carrying the same keys doesn't.
func TestGameConnServerNowFromPongs(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	start := time.Now().Add(-5 * time.Second)
	client.clock.setStart(start)
	serverMs := time.Now().Add(-time.Hour).UnixMilli()
	times := func() *sfs.SFSObject {
		p := sfs.NewSFSObject()
		p.PutLong("serverTime", serverMs)
		p.PutLong("clientTime", time.Since(start).Milliseconds())
		return p
	}

	go func() { _ = server.SendEnvelope(ControllerSystem, ActionHandshake, times()) }()
	if _, err := client.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ServerNow(); ok {
		t.Fatal("a handshake envelope synced the clock")
	}

	go func() { _ = server.SendEnvelope(ControllerSystem, ActionPingPong, times()) }()
	if _, err := client.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	now, ok := client.ServerNow()
	if !ok {
		t.Fatal("a pong did not sync the clock")
	}
	if d := time.Since(now); (d - time.Hour).Abs() > 2*time.Second {
		t.Errorf("ServerNow is %v behind the host, want about 1h", d)
	}
}

func TestCloseLogsTheClockOffset(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	for _, c := range []struct {
		name   string
		ahead  time.Duration
		synced bool
		want   string
	}{
		{"close to the server", 300 * time.Millisecond, true, "level=INFO msg=\"server clock: offsetMs"},
		{"seconds off", -5 * time.Second, true, "level=WARN msg=\"server clock: offsetMs"},
		{"no pong", 0, false, "no pong carried serverTime"},
	} {
		buf.Reset()
		client, _ := NewPipeGameConnPair(t)
		recv := time.Now()
		client.clock.setStart(recv.Add(-time.Second))
		if c.synced {
			client.clock.sync(recv, recv.Add(c.ahead).UnixMilli(), 1000)
		}
		_ = client.Close()
		if !strings.Contains(buf.String(), c.want) {
			t.Errorf("%s: logged %q, want %q", c.name, buf.String(), c.want)
		}
	}

	// A connection that never ran a heartbeat (the fake-server side) logs nothing.
	buf.Reset()
	_, server := NewPipeGameConnPair(t)
	_ = server.Close()
	if buf.Len() != 0 {
		t.Errorf("a connection without a heartbeat logged %q", buf.String())
	}
}
