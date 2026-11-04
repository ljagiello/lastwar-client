package session

import (
	"context"
	"log/slog"
	"math"
	"sync"
	"time"

	"lastwar-client/internal/sfs"
)

// serverClock is the client's server-time sync. Every pong carries serverTime (ms) and echoes the
// clientTime its ping sent (BaseUtils.decompiled.cs:36404-36409), and Timer.SyncServerTime
// (Assembly-CSharp.decompiled.cs:66043-66060) turns that pair into a server clock:
//
//   - the round trip is the arrival time minus clientTime, both on the heartbeat's elapsed-ms basis;
//   - a sample replaces the current one when its round trip is the shortest yet, under 200 ms, or
//     the current one's was 0;
//   - the server time at arrival is serverTime plus half the round trip, and the clock runs on
//     from there with the monotonic timer (__GetServerTime, :66062-66068).
//
// Lua's UITimeManager:GetServerTime() reads that clock every frame (UITimeManager.lua:48-55), and
// the client's time gates and cmdBaseTime come from it.
type serverClock struct {
	mu       sync.Mutex
	start    time.Time // the heartbeat's clientTime basis; zero until StartHeartbeat
	minRTT   int64     // ms; math.MaxInt64 until the first sample
	base     int64     // server Unix ms at syncAt
	syncAt   time.Time // host reading (with its monotonic part) when base was taken
	synced   bool
	samples  int  // pongs seen with both times
	unusable bool // a pong without them was seen (logged once)
}

// clockReplaceRTT is SyncServerTime's "num < 200": a round trip this short always replaces the
// current sample, so the clock follows a fresh fast pong over an older, slightly faster one.
const clockReplaceRTT = 200

// clockWarnOffset is the host-to-server offset from which Close's summary line is a Warn.
const clockWarnOffset = 2 * time.Second

func (k *serverClock) setStart(t time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.start = t
	k.minRTT = math.MaxInt64
}

// sync records one pong that arrived at recv.
func (k *serverClock) sync(recv time.Time, serverMs, clientMs int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.start.IsZero() {
		return
	}
	rtt := recv.Sub(k.start).Milliseconds() - clientMs
	if rtt < 0 {
		return // not a clientTime this heartbeat sent
	}
	k.samples++
	if rtt < k.minRTT || rtt < clockReplaceRTT || k.minRTT == 0 {
		k.minRTT = rtt
		k.base = serverMs + rtt/2
		k.syncAt = recv
		k.synced = true
	}
}

// now is the server time, or the host time and false before the first pong.
func (k *serverClock) now() (time.Time, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.synced {
		return time.Now(), false
	}
	return time.UnixMilli(k.base).Add(time.Since(k.syncAt)), true
}

// ServerNow is the server's current time as the client keeps it: synced from the pongs on this
// connection, running on the host's monotonic clock in between. Before the first pong it is the
// host clock, and ok is false; so is a nil connection.
func (c *GameConn) ServerNow() (now time.Time, ok bool) {
	if c == nil {
		return time.Now(), false
	}
	return c.clock.now()
}

// clockMinServerMs is 2020-01-01 in Unix ms: a pong whose serverTime is earlier is not a server
// clock reading and is ignored.
const clockMinServerMs = 1577836800000

// notePong feeds a pong's serverTime and clientTime to the clock, as of recv, when the packet was
// read off the socket. Pings the client sends carry no serverTime, so the fake-server side of a
// test pair, which reads them, records nothing.
func (c *GameConn) notePong(env *Envelope, recv time.Time) {
	if env.Controller != ControllerSystem || env.Action != ActionPingPong || env.Content == nil {
		return
	}
	serverMs, ok1 := pongMs(env.Content, "serverTime")
	clientMs, ok2 := pongMs(env.Content, "clientTime")
	if !ok1 || !ok2 || serverMs < clockMinServerMs {
		if c.clock.firstUnusable() {
			slog.Debug("server clock: a pong without a usable serverTime and clientTime", "keys", env.Content.Keys())
		}
		return
	}
	c.clock.sync(recv, serverMs, clientMs)
}

// firstUnusable reports whether this is the connection's first pong the clock can't use, so its
// shape is logged once.
func (k *serverClock) firstUnusable() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	first := !k.unusable
	k.unusable = true
	return first
}

// pongMs reads a pong's ms field: a Long, as both the client (GetLong, BaseUtils.decompiled.cs:
// 36406-36407) and Go's ping send it, or an Int.
func pongMs(o *sfs.SFSObject, key string) (int64, bool) {
	v, ok := o.Get(key)
	if !ok {
		return 0, false
	}
	switch n := v.Val.(type) {
	case int64:
		return n, true
	case int32:
		return int64(n), true
	}
	return 0, false
}

// logClock logs how far the host clock is from the server's, once, as the connection closes.
// rttMs is the round trip of the sample in use, which a later pong under 200 ms replaces even when
// slower.
func (c *GameConn) logClock() {
	k := &c.clock
	k.mu.Lock()
	started, synced, rtt, samples := !k.start.IsZero(), k.synced, k.minRTT, k.samples
	k.mu.Unlock()
	if !started {
		return // no heartbeat ran on this connection
	}
	if !synced {
		slog.Debug("server clock: no pong carried serverTime and clientTime on this connection")
		return
	}
	server, _ := k.now()
	offset := time.Until(server)
	level := slog.LevelInfo
	if offset.Abs() >= clockWarnOffset {
		level = slog.LevelWarn
	}
	slog.Log(context.Background(), level, "server clock: offsetMs is server time minus host time",
		"offsetMs", offset.Milliseconds(), "rttMs", rtt, "pongs", samples)
}
