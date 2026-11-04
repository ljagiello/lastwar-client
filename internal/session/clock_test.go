package session

import (
	"bytes"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// The sample rules are Timer.SyncServerTime's (Assembly-CSharp.decompiled.cs:66043-66060): a pong
// replaces the sample in use when its round trip is shorter, under 200 ms, or the one in use took 0.
func TestServerClockSampleRules(t *testing.T) {
	var k serverClock
	start := time.Now().Add(-time.Minute)
	at := func(ms int64) time.Time { return start.Add(time.Duration(ms) * time.Millisecond) }

	k.sync(at(1000), 1_700_000_050_000, 700) // before StartHeartbeat: no basis for clientTime
	if _, ok := k.now(); ok || k.samples != 0 {
		t.Fatalf("a pong before setStart synced the clock (samples %d)", k.samples)
	}
	k.setStart(start)

	const s0 = 1_700_000_000_000
	steps := []struct {
		name               string
		recv, server, sent int64
		wantBase, wantRTT  int64
	}{
		{"the first pong is taken", 1_000, s0 + 50_000, 700, s0 + 50_150, 300},
		{"a slower pong over 200 ms is not", 5_000, s0 + 54_000, 4_500, s0 + 50_150, 300},
		{"a faster one is", 9_000, s0 + 58_000, 8_750, s0 + 58_125, 250},
		{"an equally fast one over 200 ms is not", 11_000, s0 + 60_000, 10_750, s0 + 58_125, 250},
		{"a faster one under 200 ms is", 13_000, s0 + 62_000, 12_900, s0 + 62_050, 100},
		{"a slower one at 200 ms is not", 15_000, s0 + 64_000, 14_800, s0 + 62_050, 100},
		{"nor is one at 220 ms", 17_000, s0 + 66_000, 16_780, s0 + 62_050, 100},
		{"a slower one under 200 ms is", 19_000, s0 + 68_000, 18_850, s0 + 68_075, 150},
		{"a 0 ms one is", 21_000, s0 + 70_000, 21_000, s0 + 70_000, 0},
		{"after a 0 ms one, any is", 23_000, s0 + 72_000, 22_600, s0 + 72_200, 400},
		{"a clientTime from the future is ignored", 25_000, s0 + 74_000, 25_500, s0 + 72_200, 400},
	}
	for _, s := range steps {
		k.sync(at(s.recv), s.server, s.sent)
		if k.base != s.wantBase || k.minRTT != s.wantRTT {
			t.Errorf("%s: base %d rtt %d, want %d and %d", s.name, k.base-s0, k.minRTT, s.wantBase-s0, s.wantRTT)
		}
	}
	if k.samples != 10 {
		t.Errorf("samples %d, want 10 (the future clientTime is not one)", k.samples)
	}
}

// After a sample the clock runs on with the host's monotonic clock (__GetServerTime,
// Assembly-CSharp.decompiled.cs:66062-66068), so a pong 30 s old still gives the current time.
func TestServerClockRunsOnFromTheSample(t *testing.T) {
	var k serverClock
	if now, ok := k.now(); ok || time.Since(now).Abs() > time.Second {
		t.Fatalf("an unsynced clock is %v (ok %v), want the host clock and false", now, ok)
	}
	recv := time.Now().Add(-30 * time.Second)
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

// pongEnvelope is a server pong for a connection whose heartbeat started at start.
func pongEnvelope(start time.Time, serverTime any) *Envelope {
	p := sfs.NewSFSObject()
	switch v := serverTime.(type) {
	case int64:
		p.PutLong("serverTime", v)
	case int32:
		p.PutInt("serverTime", v)
	}
	p.PutLong("clientTime", time.Since(start).Milliseconds())
	return &Envelope{Controller: ControllerSystem, Action: ActionPingPong, Content: p}
}

// A pong read off the connection syncs ServerNow; another envelope carrying the same keys doesn't.
func TestGameConnServerNowFromPongs(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	start := time.Now().Add(-5 * time.Second)
	client.clock.setStart(start)
	serverMs := time.Now().Add(-time.Hour).UnixMilli()

	hs := pongEnvelope(start, serverMs)
	go func() { _ = server.SendEnvelope(ControllerSystem, ActionHandshake, hs.Content) }()
	if _, err := client.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	if _, ok := client.ServerNow(); ok {
		t.Fatal("a handshake envelope synced the clock")
	}

	pong := pongEnvelope(start, serverMs)
	go func() { _ = server.SendEnvelope(ControllerSystem, ActionPingPong, pong.Content) }()
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

// A serverTime that can't be a clock reading (an Int, or before 2020) is ignored, and a nil
// connection reads as the host clock.
func TestServerClockIgnoresImplausiblePongs(t *testing.T) {
	client, _ := NewPipeGameConnPair(t)
	start := time.Now().Add(-time.Second)
	client.clock.setStart(start)
	for _, st := range []any{int32(1_000_000), int64(1_000_000), int64(clockMinServerMs - 1)} {
		client.notePong(pongEnvelope(start, st), time.Now())
		if _, ok := client.ServerNow(); ok {
			t.Errorf("serverTime %v synced the clock", st)
		}
	}
	var none *GameConn
	if now, ok := none.ServerNow(); ok || time.Since(now).Abs() > time.Second {
		t.Errorf("a nil connection's ServerNow is %v (ok %v), want the host clock and false", now, ok)
	}
}

func TestCloseLogsTheClockOffset(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)
	offsetRe := regexp.MustCompile(`offsetMs=(-?[0-9]+)`)

	for _, c := range []struct {
		name   string
		ahead  time.Duration
		synced bool
		want   string
	}{
		{"close to the server", 300 * time.Millisecond, true, "level=INFO msg=\"server clock: offsetMs"},
		{"seconds off", -5 * time.Second, true, "level=WARN msg=\"server clock: offsetMs"},
		{"no pong", 0, false, "level=DEBUG msg=\"server clock: no pong carried serverTime"},
	} {
		buf.Reset()
		client, _ := NewPipeGameConnPair(t)
		recv := time.Now()
		client.clock.setStart(recv.Add(-time.Second))
		if c.synced {
			client.clock.sync(recv, recv.Add(c.ahead).UnixMilli(), 1000)
		}
		_ = client.Close()
		logged := buf.String()
		if !strings.Contains(logged, c.want) {
			t.Errorf("%s: logged %q, want %q", c.name, logged, c.want)
		}
		if !c.synced {
			continue
		}
		m := offsetRe.FindStringSubmatch(logged)
		if m == nil {
			t.Errorf("%s: no offsetMs in %q", c.name, logged)
			continue
		}
		// offsetMs is server time minus host time: negative when the server is behind.
		if got, _ := strconv.ParseInt(m[1], 10, 64); (got-c.ahead.Milliseconds()) < -100 || got-c.ahead.Milliseconds() > 100 {
			t.Errorf("%s: offsetMs %d, want about %d", c.name, got, c.ahead.Milliseconds())
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
