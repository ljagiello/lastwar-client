package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// This file covers buildings.go's three network-driving entry points -- FetchBuildings,
// CollectIdleReward, and CollectAll -- over a net.Pipe-backed GameConn, the same fake-server
// pattern conn_wait_test.go, interactive_orchestration_test.go, and visitors_orchestration_test.go
// all use (newPipeGameConnPair/readAndReply, conn_wait_test.go). Pure, network-free logic
// (ParseInitBuildings, collectCmdFor) is already covered directly in buildings_visitors_test.go;
// this file is only about the orchestration built on top of it.

// newTestBuildingSFS builds a minimal building_new-shaped SFSObject with the fields Building's
// accessors read -- uuid/bId/lv -- mirroring what a real init push carries (see ParseInitBuildings'
// doc comment in buildings.go).
func newTestBuildingSFS(uuid int64, bId, lv int32) *SFSObject {
	b := NewSFSObject()
	b.PutLong("uuid", uuid)
	b.PutInt("bId", bId)
	b.PutInt("lv", lv)
	return b
}

func newTestBuilding(uuid int64, bId, lv int32) Building {
	return Building{Raw: newTestBuildingSFS(uuid, bId, lv)}
}

// TestFetchBuildingsInitPushParsesBuildingsAndVisitors covers FetchBuildings' main documented
// path: a bare `init` bootstrap push carrying `building_new` and `visitor` -- ParseInitBuildings'
// doc comment explains this, not push.init.build/defaultBuilds, is the field that actually
// matters.
//
// After processing any init/push.init.build push, FetchBuildings deliberately resets its own
// deadline to a fresh `time.Now().Add(3 * time.Second)` (see the "push.init.build" case's doc
// comment) to opportunistically observe trailing push.queue.add/push.build.queue.info traffic --
// so a *graceful* (err==nil, reached by that window's own timeout firing) return from this exact
// scenario needs a real 3-second sleep. That's too slow for this file's fast/deterministic bar,
// and buildings.go isn't ours to touch this round to make that window configurable. Instead, the
// fake server closes its end of the pipe immediately after delivering the init push:
// FetchBuildings' *next* read then fails immediately with io.EOF (confirmed via net.Pipe's
// close-then-read semantics -- that's not a net.Error/Timeout(), so it takes the "real I/O error"
// return path, not the graceful-timeout one) instead of waiting out the 3s window, keeping the
// test fast. This still exercises exactly what the audit finding asks for -- the parsed result of
// a normal init-push response -- the trailing non-nil error is a fake-server test artifact (proven
// via errors.Is), not a claim about real server behavior, and the already-accumulated
// buildings/visitors are asserted directly rather than discarded.
func TestFetchBuildingsInitPushParsesBuildingsAndVisitors(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		params := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(newTestBuildingSFS(111, BuildingFarmland, 3))
		arr.AddSFSObject(newTestBuildingSFS(222, BuildingIronMine, 5))
		params.PutSFSArray("building_new", arr)

		visitorList := NewSFSArray()
		v := NewSFSObject()
		v.PutLong("uid", 999)
		v.PutInt("eventId", 2001)
		v.PutInt("visitorId", 6)
		visitorList.AddSFSObject(v)
		visitor := NewSFSObject()
		visitor.PutSFSArray("list", visitorList)
		params.PutSFSObject("visitor", visitor)

		if err := server.SendExtension("init", params); err != nil {
			return
		}
		server.conn.Close() // see doc comment above: ends the test fast instead of waiting out the post-init 3s window
	}()

	buildings, visitors, err := FetchBuildings(client, 2*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact, see doc comment)", err)
	}
	if len(buildings) != 2 {
		t.Fatalf("got %d buildings, want 2", len(buildings))
	}
	if buildings[0].Uuid() != 111 || buildings[0].BId() != BuildingFarmland || buildings[0].Level() != 3 {
		t.Errorf("buildings[0] = uuid=%d bId=%d lv=%d, want uuid=111 bId=%d lv=3", buildings[0].Uuid(), buildings[0].BId(), buildings[0].Level(), BuildingFarmland)
	}
	if buildings[1].Uuid() != 222 || buildings[1].BId() != BuildingIronMine {
		t.Errorf("buildings[1] = uuid=%d bId=%d, want uuid=222 bId=%d", buildings[1].Uuid(), buildings[1].BId(), BuildingIronMine)
	}
	if len(visitors) != 1 || visitors[0].Uid() != 999 {
		t.Fatalf("got visitors=%v, want exactly one with uid 999", visitors)
	}
}

// TestFetchBuildingsNoPushWithinTimeoutReturnsEmpty covers the plain "nothing ever arrived"
// timeout branch (the `remaining <= 0` break / the netErr.Timeout() break, whichever fires first)
// directly: pass a short timeout and never touch the fake server's end of the pipe at all.
// FetchBuildings must return promptly once its deadline elapses, with gotInitBuild still false --
// buildings.go's own doc comment says that's only a Warn log, never a returned error -- and empty
// results.
func TestFetchBuildingsNoPushWithinTimeoutReturnsEmpty(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	const window = 100 * time.Millisecond
	start := time.Now()
	buildings, visitors, err := FetchBuildings(client, window)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a silent timeout with nothing received is not itself an error)", err)
	}
	if len(buildings) != 0 || len(visitors) != 0 {
		t.Errorf("got buildings=%v visitors=%v, want both empty", buildings, visitors)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("FetchBuildings took %v, want close to the %v timeout", elapsed, window)
	}
}

// TestFetchBuildingsTimeoutKeepsPartialResults covers the "timeout after partial results" branch
// the audit finding asks about, chosen specifically to stay fast: push.add.building is the one
// case in FetchBuildings' switch that does NOT reset the deadline to a fresh 3-second window
// (unlike init/push.init.build -- see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc
// comment above), so sending exactly one of those and then falling silent lets the original short
// outer timeout govern the whole test, no 3-second tail involved.
//
// Deliberately NOT covered here: partial results left over when an init/push.init.build push
// itself is what times out mid-window (rather than being read to completion). Reaching that would
// need the same real ~3-second wait as the doc comment above explains, for the same
// buildings.go-internal reason, so it's skipped as not reasonably fast/deterministic rather than
// forced.
func TestFetchBuildingsTimeoutKeepsPartialResults(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		params := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(newTestBuildingSFS(333, BuildingGoldMine, 1))
		params.PutSFSArray("buildings", arr)
		_ = server.SendExtension("push.add.building", params)
	}()

	buildings, visitors, err := FetchBuildings(client, 150*time.Millisecond)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending push.add.building")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a plain timeout after partial data is not itself an error)", err)
	}
	if len(visitors) != 0 {
		t.Errorf("got visitors=%v, want none (push.add.building carries no visitor data)", visitors)
	}
	if len(buildings) != 1 || buildings[0].Uuid() != 333 || buildings[0].BId() != BuildingGoldMine {
		t.Fatalf("got buildings=%v, want exactly one uuid=333 bId=%d (BuildingGoldMine)", buildings, BuildingGoldMine)
	}
}

// TestFetchBuildingsRedactsCredentialFieldsInUnrecognizedPush is the round-12 regression test for
// FetchBuildings' push-observer switch (buildings.go's "push.queue.add"/"push.build.queue.info"/
// default cases): all three previously dumped msg.Params via the raw, unredacted String(), so any
// credential-bearing push landing in this switch while FetchBuildings is listening -- e.g. an
// unrecognized cmd carrying a sensitiveSFSKeys field like loginKey -- would leak it into the log.
// No currently-reachable path is known to route such a push through here (the fix's own comment in
// buildings.go explains why that's an unenforced assumption, not a proven invariant), but this test
// doesn't rely on that: it sends an arbitrary unrecognized cmd carrying a live loginKey directly, so
// it lands in the default case, and asserts the raw secret never appears in the captured log
// output. Mirrors TestWaitForCmdSkipRedactsCredentialFields' (conn_wait_test.go) capture pattern.
//
// Mutation check: reverting buildings.go's three .StringRedacted() calls back to .String() makes
// this test fail, since msg.Params.String() would print the loginKey in cleartext.
func TestFetchBuildingsRedactsCredentialFieldsInUnrecognizedPush(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-buildings-1234567890"

	done := make(chan struct{})
	go func() {
		defer close(done)
		push := NewSFSObject()
		push.PutUtfString("loginKey", secretLoginKey)
		push.PutUtfString("someField", "ordinary gameplay data")
		_ = server.SendExtension("push.some.unrecognized.event", push)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	// push.some.unrecognized.event doesn't reset FetchBuildings' deadline (only init/
	// push.init.build do -- see those cases' doc comments), so this short outer timeout governs
	// the whole test, no 3-second tail involved (same reasoning as
	// TestFetchBuildingsTimeoutKeepsPartialResults above).
	_, _, err := FetchBuildings(client, 150*time.Millisecond)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished sending the unrecognized push")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a plain timeout is not itself an error)", err)
	}
	if logged := buf.String(); strings.Contains(logged, secretLoginKey) {
		t.Errorf("FetchBuildings' push-observer switch leaks the raw loginKey in cleartext:\n%s", logged)
	}
}

// TestFetchBuildingsDedupesBuildingUUIDAcrossSources is the round-12 regression test for
// FetchBuildings' seenBuildingUUIDs dedupe: if the same building uuid arrives via more than one of
// the three population sources within a single fetch window -- here, both the init push's
// building_new and a following push.init.build/defaultBuilds carrying the same uuid -- it must
// appear exactly once in the returned slice, not duplicated (a duplicate would otherwise cause
// CollectAll to issue a redundant building.production.collect for it).
//
// Mutation check: reverting the three appendBuilding call sites in buildings.go back to plain
// `buildings = append(buildings, ...)` makes this test fail with 2 buildings instead of 1.
func TestFetchBuildingsDedupesBuildingUUIDAcrossSources(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const dupeUUID = int64(777)

	done := make(chan struct{})
	go func() {
		defer close(done)
		initParams := NewSFSObject()
		initArr := NewSFSArray()
		initArr.AddSFSObject(newTestBuildingSFS(dupeUUID, BuildingFarmland, 1))
		initParams.PutSFSArray("building_new", initArr)
		if err := server.SendExtension("init", initParams); err != nil {
			return
		}

		wrapper := NewSFSObject()
		wrapper.PutSFSObject("buildInfo", newTestBuildingSFS(dupeUUID, BuildingFarmland, 1))
		defaultBuilds := NewSFSArray()
		defaultBuilds.AddSFSObject(wrapper)
		pibParams := NewSFSObject()
		pibParams.PutSFSArray("defaultBuilds", defaultBuilds)
		if err := server.SendExtension("push.init.build", pibParams); err != nil {
			return
		}
		server.conn.Close() // see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment: ends the test fast instead of waiting out the post-push 3s window
	}()

	buildings, _, err := FetchBuildings(client, 2*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact, see doc comment)", err)
	}
	if len(buildings) != 1 {
		t.Fatalf("got %d buildings, want exactly 1 (uuid %d arrived via both init and push.init.build)", len(buildings), dupeUUID)
	}
	if buildings[0].Uuid() != dupeUUID {
		t.Errorf("got uuid %d, want %d", buildings[0].Uuid(), dupeUUID)
	}
}

// TestFetchBuildingsWaitsForAuthoritativeInitDespiteEarlyPushInitBuild is the round-23 regression
// test for the "push.init.build" case's deadline-shrink gate (buildings.go's gotAuthoritativeInit).
// Contrast with TestFetchBuildingsDedupesBuildingUUIDAcrossSources above, which sends init THEN
// push.init.build -- the already-correct ordering, where init has already arrived by the time
// push.init.build's shrink fires, so shrinking to a short trailing window is fine. This test covers
// the reverse, previously-buggy ordering: push.init.build arrives FIRST, with nothing else seen yet,
// followed by a DELAYED authoritative init arriving more than 3 seconds later but still comfortably
// inside the caller's original timeout.
//
// Before this fix, push.init.build's deadline-shrink applied unconditionally, identical to init's
// own shrink -- so this exact sequence would have capped the deadline to ~3s after push.init.build,
// well before the authoritative init at ~3.5s ever arrived, and FetchBuildings would have given up
// early with only push.init.build's inferior defaultBuilds data (buildings.go's own doc comments are
// emphatic that init/building_new, not push.init.build/defaultBuilds, is "the field that actually
// matters"). Fixed by gating that shrink on gotAuthoritativeInit: push.init.build only gets to act as
// a short trailing window once the authoritative init has already been captured, never before.
//
// docs/live-validation.mdx notes push.init.build has never actually fired once across roughly a
// dozen real live sessions, so this ordering can't happen against the real production server today
// -- but this file's own doc comments explicitly treat an arbitrary/hostile -cs-ip peer as in-scope
// (see originalDeadline's doc comment), and nothing stops a non-standard or adversarial peer from
// sending push.init.build before init.
//
// Mutation check: reverting the "push.init.build" case's `if gotAuthoritativeInit { ... }` gate in
// buildings.go back to an unconditional `deadline = capDeadline(...)` makes this test fail: the
// authoritative building (uuid 777) would be missing from the result, and err would be a plain nil
// timeout instead of one wrapping io.EOF, since FetchBuildings would have given up at ~3s, before the
// fake server ever got to send (or close the connection after) the delayed init.
func TestFetchBuildingsWaitsForAuthoritativeInitDespiteEarlyPushInitBuild(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const (
		callerTimeout = 6 * time.Second
		// delayBeforeInit is deliberately > the 3-second push.init.build shrink window (see
		// buildings.go's gotAuthoritativeInit doc comment) -- comfortably enough to absorb
		// scheduling jitter -- while staying well under callerTimeout, so the fixed behavior
		// (waiting the full original budget) still has room to actually read it.
		delayBeforeInit   = 3500 * time.Millisecond
		inferiorUUID      = int64(555)
		authoritativeUUID = int64(777)
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		// push.init.build FIRST, with nothing else seen yet -- the previously-buggy ordering.
		wrapper := NewSFSObject()
		wrapper.PutSFSObject("buildInfo", newTestBuildingSFS(inferiorUUID, BuildingIronMine, 1))
		defaultBuilds := NewSFSArray()
		defaultBuilds.AddSFSObject(wrapper)
		pibParams := NewSFSObject()
		pibParams.PutSFSArray("defaultBuilds", defaultBuilds)
		if err := server.SendExtension("push.init.build", pibParams); err != nil {
			return
		}

		time.Sleep(delayBeforeInit)

		// The DELAYED authoritative init, arriving well after the 3s shrink window would have
		// already expired the pre-fix deadline.
		initParams := NewSFSObject()
		initArr := NewSFSArray()
		initArr.AddSFSObject(newTestBuildingSFS(authoritativeUUID, BuildingFarmland, 5))
		initParams.PutSFSArray("building_new", initArr)
		if err := server.SendExtension("init", initParams); err != nil {
			return
		}
		server.conn.Close() // see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment: ends the test fast instead of waiting out the post-init 3s window
	}()

	start := time.Now()
	buildings, _, err := FetchBuildings(client, callerTimeout)
	elapsed := time.Since(start)

	select {
	case <-done:
	case <-time.After(delayBeforeInit + 3*time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact once the delayed authoritative init was read; see doc comment) -- a plain nil/timeout here means FetchBuildings gave up before the authoritative init ever arrived, exactly the round-23 bug this test guards against", err)
	}
	if elapsed >= callerTimeout {
		t.Fatalf("FetchBuildings took %v, want well under the %v caller timeout (should have returned shortly after reading the delayed authoritative init at ~%v, not run out the full budget)", elapsed, callerTimeout, delayBeforeInit)
	}

	if len(buildings) != 2 {
		t.Fatalf("got %d buildings, want 2 (push.init.build's inferior uuid=%d plus the authoritative init's uuid=%d)", len(buildings), inferiorUUID, authoritativeUUID)
	}
	if buildings[0].Uuid() != inferiorUUID || buildings[0].BId() != BuildingIronMine {
		t.Errorf("buildings[0] = uuid=%d bId=%d, want push.init.build's inferior uuid=%d bId=%d", buildings[0].Uuid(), buildings[0].BId(), inferiorUUID, BuildingIronMine)
	}
	if buildings[1].Uuid() != authoritativeUUID || buildings[1].BId() != BuildingFarmland || buildings[1].Level() != 5 {
		t.Errorf("buildings[1] = uuid=%d bId=%d lv=%d, want the authoritative init/building_new uuid=%d bId=%d lv=5 -- push.init.build's early arrival must not shrink the deadline before this delayed-but-in-budget authoritative data is read", buildings[1].Uuid(), buildings[1].BId(), buildings[1].Level(), authoritativeUUID, BuildingFarmland)
	}
}

// TestFetchBuildingsDedupesVisitorUIDAcrossInitPushes is the round-16 regression test for
// FetchBuildings' seenVisitorUUIDs dedupe (buildings.go): the visitor list's sole population
// source is the bare `init` push's `visitor` field (ParseInitVisitors), but a redundant resend of
// that same init push within one fetch window -- e.g. a duplicate bootstrap/init resend, the exact
// scenario TestFetchBuildingsDedupesBuildingUUIDAcrossSources above covers for buildings -- must
// not double-append every visitor it carries. A duplicate would otherwise cause GreetVisitors
// (visitors.go) to issue two real visitor.operate network calls for the same uid.
//
// Mutation check: reverting appendVisitor's call site in buildings.go back to plain
// `visitors = append(visitors, ParseInitVisitors(msg.Params)...)` makes this test fail with 2
// visitors instead of 1.
func TestFetchBuildingsDedupesVisitorUIDAcrossInitPushes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const dupeUID = int64(888)
	newInitParamsWithVisitor := func() *SFSObject {
		visitorList := NewSFSArray()
		v := NewSFSObject()
		v.PutLong("uid", dupeUID)
		v.PutInt("eventId", 2001)
		v.PutInt("visitorId", 6)
		visitorList.AddSFSObject(v)
		visitor := NewSFSObject()
		visitor.PutSFSArray("list", visitorList)
		params := NewSFSObject()
		params.PutSFSObject("visitor", visitor)
		return params
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.SendExtension("init", newInitParamsWithVisitor()); err != nil {
			return
		}
		// A redundant resend of the same bootstrap/init push -- the identical scenario
		// TestFetchBuildingsDedupesBuildingUUIDAcrossSources exercises for buildings, applied to
		// visitors instead (buildings.go's own FIX-1 doc comment on seenVisitorUUIDs).
		if err := server.SendExtension("init", newInitParamsWithVisitor()); err != nil {
			return
		}
		server.conn.Close() // see TestFetchBuildingsInitPushParsesBuildingsAndVisitors' doc comment: ends the test fast instead of waiting out the post-init 3s window
	}()

	_, visitors, err := FetchBuildings(client, 2*time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if !errors.Is(err, io.EOF) {
		t.Fatalf("FetchBuildings() error = %v, want an error wrapping io.EOF (expected fake-server-hangup artifact, see doc comment)", err)
	}
	if len(visitors) != 1 {
		t.Fatalf("got %d visitors, want exactly 1 (uid %d arrived via two separate init pushes)", len(visitors), dupeUID)
	}
	if visitors[0].Uid() != dupeUID {
		t.Errorf("got uid %d, want %d", visitors[0].Uid(), dupeUID)
	}
}

// TestFetchBuildingsDeadlineCappedAgainstRepeatedInitPushes is the round-20 regression test for
// FetchBuildings' per-push deadline extension (the "init"/"push.init.build" cases' unconditional
// `deadline = time.Now().Add(3 * time.Second)`, now `deadline = capDeadline(time.Now().Add(3*
// time.Second), originalDeadline)`). Before this fix, every qualifying push reset the deadline
// with no cap against the caller-supplied timeout at all, so a peer -- this client supports
// connecting to arbitrary hosts via -cs-ip, which this project's threat model already treats as
// untrusted/hostile-capable -- that kept re-sending "init" faster than the 3-second window could
// keep this call (and therefore the whole synchronous main() flow) hanging indefinitely, a
// materially worse outcome than a bounded timeout for the cron-wrapper usage main.go's own
// comments describe as a first-class use case.
//
// The fake server sends 5 qualifying "init" pushes spaced 400ms apart (comfortably under the
// 3-second reset window, so every single one would extend the deadline if uncapped) against a
// short 2-second caller timeout. If the deadline were still being reset unconditionally, the last
// push (at ~1.6s) would push the effective deadline out to ~4.6s -- well past the 2-second budget
// the caller actually asked for. This test asserts FetchBuildings returns within a bounded window
// close to that original 2-second budget instead.
//
// Unlike TestFetchBuildingsDedupesVisitorUIDAcrossInitPushes/
// TestFetchBuildingsInitPushParsesBuildingsAndVisitors above, which each send only 2 pushes and
// then close the connection (exiting via a wrapped io.EOF, never actually exercising the deadline
// logic at all), this test's fake server never closes its end of the pipe -- it just stops sending
// after the 5th push and returns. The only way FetchBuildings can terminate here is the deadline
// itself elapsing and the loop's `remaining <= 0` / SetReadDeadline-timeout break firing, which is
// exactly the code path this fix targets.
//
// Mutation check: reverting either capDeadline call site in buildings.go back to the old plain
// `deadline = time.Now().Add(3 * time.Second)` makes this test fail (or hang past its own 6-second
// safety timeout), since the 5th push's reset would push the effective deadline to ~4.6s -- outside
// this test's asserted bound.
func TestFetchBuildingsDeadlineCappedAgainstRepeatedInitPushes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const (
		callerTimeout = 2 * time.Second
		pushSpacing   = 400 * time.Millisecond
		numPushes     = 5 // spaced well under the 3s reset window; last push lands at ~1.6s
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < numPushes; i++ {
			if i > 0 {
				time.Sleep(pushSpacing)
			}
			params := NewSFSObject()
			arr := NewSFSArray()
			arr.AddSFSObject(newTestBuildingSFS(int64(1000+i), BuildingFarmland, 1))
			params.PutSFSArray("building_new", arr)
			if err := server.SendExtension("init", params); err != nil {
				return
			}
		}
		// Deliberately does NOT close the connection: the point of this test is that
		// FetchBuildings must give up on its own, governed by the capped deadline, not because
		// the peer hung up (contrast the io.EOF-driven tests above).
	}()

	start := time.Now()
	buildings, _, err := FetchBuildings(client, callerTimeout)
	elapsed := time.Since(start)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server goroutine never finished sending its 5 init pushes")
	}

	if err != nil {
		t.Fatalf("FetchBuildings() error = %v, want nil (a plain deadline-elapsed timeout is not itself an error, per the existing convention)", err)
	}
	// Uncapped, the 5th push (sent at ~1.6s) would reset the deadline to ~4.6s. Capped, the
	// deadline can never move past callerTimeout (2s) regardless of how many pushes arrive.
	// Bounded generously above callerTimeout to absorb scheduling jitter while staying well
	// under what the uncapped bug would produce.
	if maxWant := callerTimeout + 1*time.Second; elapsed > maxWant {
		t.Errorf("FetchBuildings took %v, want at most ~%v (deadline must be capped at the caller's original %v timeout despite repeated qualifying init pushes -- an uncapped reset from the 5th push alone would run to ~%v)",
			elapsed, maxWant, callerTimeout, pushSpacing*(numPushes-1)+3*time.Second)
	}
	if len(buildings) != numPushes {
		t.Errorf("got %d buildings, want %d (one distinct uuid per init push, all processed before the capped deadline fired)", len(buildings), numPushes)
	}
}

// TestCollectIdleRewardSuccess covers CollectIdleReward's documented two-call sequence -- a peek
// (action=0) immediately followed by a claim (action=1), both against `lw.pve.idle.reward` (see
// its doc comment in buildings.go) -- against a fake server that answers both with a plain
// success.
func TestCollectIdleRewardSuccess(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var gotActions []int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 2; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			if msg.Cmd != "lw.pve.idle.reward" {
				t.Errorf("Cmd = %q, want lw.pve.idle.reward", msg.Cmd)
			}
			gotActions = append(gotActions, msg.Params.GetInt("action"))
			resp := NewSFSObject()
			resp.PutBool("success", true)
			_ = server.SendExtension(msg.Cmd, resp)
		}
	}()

	err := CollectIdleReward(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading both idle-reward requests")
	}

	if err != nil {
		t.Fatalf("CollectIdleReward() = %v, want nil", err)
	}
	if len(gotActions) != 2 || gotActions[0] != 0 || gotActions[1] != 1 {
		t.Errorf("got actions %v, want [0 1] (peek then claim, in order)", gotActions)
	}
}

// TestCollectAllAggregatesErrorsWithoutShortCircuiting is the main point of this file: CollectAll
// appends every sub-action's error to a slice and only errors.Join's them at the very end (see its
// doc comment/body in buildings.go), so one real failure partway through must not stop the rest of
// the run. To prove that rather than just assert a non-nil error, the fake server tracks how many
// requests it actually receives for each command, and a genuine (non-benign) failure is injected
// on `al.help.all` -- roughly in the middle of CollectAll's fixed call sequence -- while every
// command after it (alliance gifts, the alliance-tech fetch, both VIP dailies, and both building
// collects) still gets a request. If CollectAll stopped at the first error, those later counts
// would be zero instead of matching.
//
// visitors is passed as nil and the fake mail-list response is empty specifically so
// GreetVisitors/ClaimAllMail short-circuit to a single (or zero) request each -- keeping the fake
// server's response table to the handful of command shapes below, rather than needing to also
// simulate per-visitor greets or mail batching, which isn't this test's point (see
// visitors_orchestration_test.go and a future mail-specific test for that coverage instead).
func TestCollectAllAggregatesErrorsWithoutShortCircuiting(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	buildings := []Building{
		newTestBuilding(501, BuildingFarmland, 2),
		newTestBuilding(502, BuildingIronMine, 4),
	}
	const wantRequests = 11 // idle(2) + mail-list(1) + help(1) + gifts(2) + tech-refresh(1) + vip(2) + collects(2)

	counts := make(map[string]int)
	var collectedUUIDs []int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < wantRequests; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("request %d: ReadEnvelope: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				t.Errorf("request %d: not a well-formed extension message", i)
				return
			}
			counts[msg.Cmd]++

			resp := NewSFSObject()
			replyCmd := msg.Cmd
			switch msg.Cmd {
			case "chat.get.system.mails":
				// ListMail (mail.go) waits for the response under a different cmd name, not an
				// echo of the request -- see sendAndWait's waitCmds parameter. Left empty (no
				// "msg", no "more"), it reads as zero mail found, one page, done.
				replyCmd = "push.chat.get.system.mails"
			case "al.help.all":
				// The one genuine (non-benign) failure this test injects.
				resp.PutUtfString("errorCode", "999999")
			case "science.data.refresh":
				// Left with no "allianceScience" field: DonateRecommendedAllianceTech reads that
				// as "no tech tree data" and returns nil without a second al.science.donate call.
			case "building.production.collect":
				collectedUUIDs = append(collectedUUIDs, msg.Params.GetLong("uuid"))
				resp.PutBool("success", true)
			default: // lw.pve.idle.reward, alliance.reward.allreceive, vip.add.login.score, vip.get.every.day.reward
				resp.PutBool("success", true)
			}
			_ = server.SendExtension(replyCmd, resp)
		}
	}()

	err := CollectAll(client, buildings, nil)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading all expected requests")
	}

	if err == nil {
		t.Fatal("CollectAll() = nil, want a non-nil error (al.help.all got a genuine failure)")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("aggregated error = %v, want it to mention the al.help.all failure's errorCode 999999", err)
	}

	// The heart of the test: every command scheduled after the failing al.help.all call in
	// CollectAll's fixed sequence must still have been requested exactly as many times as a clean
	// run would -- proof the aggregation didn't short-circuit.
	wantCounts := map[string]int{
		"lw.pve.idle.reward":          2,
		"chat.get.system.mails":       1,
		"al.help.all":                 1,
		"alliance.reward.allreceive":  2,
		"science.data.refresh":        1,
		"vip.add.login.score":         1,
		"vip.get.every.day.reward":    1,
		"building.production.collect": 2,
	}
	for cmd, want := range wantCounts {
		if got := counts[cmd]; got != want {
			t.Errorf("counts[%q] = %d, want %d", cmd, got, want)
		}
	}
	if len(collectedUUIDs) != 2 || collectedUUIDs[0] != 501 || collectedUUIDs[1] != 502 {
		t.Errorf("collected building uuids = %v, want [501 502] in order", collectedUUIDs)
	}
}

// TestCollectAllCapsCollectibleBuildingsAndLogsWarning is the round-24 regression test for
// CollectAll's maxCollectibleBuildingsPerRun sanity cap (buildings.go): unlike GreetVisitors
// (visitors.go), which now enforces the init push's own server-sent `maxNum` field, there is no
// server-sent "expected building count" anywhere in this protocol, so CollectAll's per-building
// collect loop needed a purely defensive, client-side ceiling instead -- closing the same
// unbounded-hang threat model (an arbitrary/hostile -cs-ip peer that simply never responds, each
// collect call costing up to a full defaultCmdTimeout) as buildings.go's own capDeadline (round 20)
// and ParseInitVisitors' maxVisitorsDefensiveCeiling (visitors.go), just for a different loop.
//
// The fake account here owns maxCollectibleBuildingsPerRun+50 collectible buildings (all
// BuildingFarmland, distinct uuids assigned in ascending order) -- comfortably over the cap. The
// fake server answers exactly fixedSubActionRequests (the 8 non-building sub-actions' own request
// count, mirroring TestCollectAllAggregatesErrorsWithoutShortCircuiting's wantRequests formula with
// visitors=nil so GreetVisitors contributes 0) plus maxCollectibleBuildingsPerRun
// building.production.collect requests, with plain success responses throughout (no injected
// failures -- this test is only about the cap, not error aggregation). If CollectAll didn't cap the
// list, it would attempt one collect per building (350 of them) and the fake server -- which only
// answers maxCollectibleBuildingsPerRun of them before its goroutine returns -- would leave
// CollectAll's 301st collect call hanging with nobody to answer it, and this test's own done-channel
// wait would time out instead of completing quickly.
//
// Mutation check: reverting the `else if len(toCollect) > maxCollectibleBuildingsPerRun { ... }`
// truncation in buildings.go's CollectAll back out makes this test fail: either the done-channel
// wait times out (the fake server starves waiting for a 301st collect request nobody sent, while
// CollectAll is itself blocked on a request nobody will ever answer), or -- if the fake server were
// instead sized to answer all 350 -- collectedUUIDs would have 350 entries instead of exactly
// maxCollectibleBuildingsPerRun and no truncation warning would be logged.
func TestCollectAllCapsCollectibleBuildingsAndLogsWarning(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const numBuildings = maxCollectibleBuildingsPerRun + 50
	buildings := make([]Building, 0, numBuildings)
	for i := 0; i < numBuildings; i++ {
		buildings = append(buildings, newTestBuilding(int64(10000+i), BuildingFarmland, 1))
	}

	// Mirrors TestCollectAllAggregatesErrorsWithoutShortCircuiting's wantRequests formula: idle(2) +
	// mail-list(1) + help(1) + gifts(2) + tech-refresh(1) + vip(2) = 9, with visitors=nil so
	// GreetVisitors contributes 0 requests of its own.
	const fixedSubActionRequests = 9
	const wantRequests = fixedSubActionRequests + maxCollectibleBuildingsPerRun

	var collectedUUIDs []int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < wantRequests; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("request %d: ReadEnvelope: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				t.Errorf("request %d: not a well-formed extension message", i)
				return
			}

			resp := NewSFSObject()
			replyCmd := msg.Cmd
			switch msg.Cmd {
			case "chat.get.system.mails":
				// ListMail (mail.go) waits under a different cmd name -- see
				// TestCollectAllAggregatesErrorsWithoutShortCircuiting's identical case.
				replyCmd = "push.chat.get.system.mails"
			case "science.data.refresh":
				// No "allianceScience" field: DonateRecommendedAllianceTech reads that as "no tech
				// tree data" and returns nil without a second al.science.donate call.
			case "building.production.collect":
				collectedUUIDs = append(collectedUUIDs, msg.Params.GetLong("uuid"))
				resp.PutBool("success", true)
			default: // lw.pve.idle.reward, al.help.all, alliance.reward.allreceive, vip.add.login.score, vip.get.every.day.reward
				resp.PutBool("success", true)
			}
			_ = server.SendExtension(replyCmd, resp)
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := CollectAll(client, buildings, nil)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished reading all expected requests (a missing/broken cap would leave it waiting for more building.production.collect requests than CollectAll ever sends)")
	}

	if err != nil {
		t.Fatalf("CollectAll() = %v, want nil (no genuine failure was injected -- this test is only about the collectible-building sanity cap)", err)
	}
	if len(collectedUUIDs) != maxCollectibleBuildingsPerRun {
		t.Fatalf("fake server received %d building.production.collect requests, want exactly %d (the sanity cap, not one per owned building -- account owns %d)",
			len(collectedUUIDs), maxCollectibleBuildingsPerRun, numBuildings)
	}
	for i, uuid := range collectedUUIDs {
		if want := int64(10000 + i); uuid != want {
			t.Errorf("collectedUUIDs[%d] = %d, want %d (the first %d buildings, in order)", i, uuid, want, maxCollectibleBuildingsPerRun)
			break
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, "collectible building count exceeds sanity cap") {
		t.Errorf("expected a truncation warning log, got:\n%s", logged)
	}
	if wantCount := fmt.Sprintf("count=%d", numBuildings); !strings.Contains(logged, wantCount) {
		t.Errorf("truncation warning log missing %q, got:\n%s", wantCount, logged)
	}
	if wantCap := fmt.Sprintf("cap=%d", maxCollectibleBuildingsPerRun); !strings.Contains(logged, wantCap) {
		t.Errorf("truncation warning log missing %q, got:\n%s", wantCap, logged)
	}
}

// fakeNetErrConn is a minimal net.Conn whose every Read fails with a fakeNetError. By default
// (timeout: false, the zero value -- what every bare fakeNetErrConn{} literal across this package's
// other _test.go files gets) that's a genuine connection-level failure (Timeout()==false), standing
// in for what a real dead/reset/blackholed TCP connection would produce. Set timeout: true instead to
// simulate sendAndWait's ordinary "no matching response within defaultCmdTimeout" per-call outcome
// (Timeout()==true, confirmed by TestWaitForTimeout in conn_wait_test.go) -- a normal, expected
// timeout on an otherwise-healthy connection. Either way this is as opposed to a well-formed response
// carrying a decoded (possibly non-benign) errorCode. Writes succeed and are counted, so a test can
// prove exactly how many requests were sent before CollectAll's net.Error early-abort (buildings.go)
// fired -- or, for the timeout:true case, prove it did NOT fire and every action still ran.
type fakeNetErrConn struct {
	mu      sync.Mutex
	writes  int
	timeout bool // Timeout() of the fakeNetError every Read fails with; see the doc comment above.
}

func (c *fakeNetErrConn) Read([]byte) (int, error) { return 0, fakeNetError{timeout: c.timeout} }

func (c *fakeNetErrConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}

func (c *fakeNetErrConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *fakeNetErrConn) Close() error                     { return nil }
func (c *fakeNetErrConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *fakeNetErrConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *fakeNetErrConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeNetErrConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeNetErrConn) SetWriteDeadline(time.Time) error { return nil }

type fakeNetAddr struct{}

func (fakeNetAddr) Network() string { return "fake" }
func (fakeNetAddr) String() string  { return "fake" }

// fakeNetError implements net.Error directly (error + Timeout() + the deprecated-but-still-required
// Temporary()), simulating either of the two kinds of net.Error CollectAll's (and FetchBuildings',
// ClaimAllMail's, GreetVisitors', ClaimAllianceGifts') early-abort checks care about -- the
// distinction is entirely carried by the timeout field:
//
//   - timeout: false (the zero value -- what a bare fakeNetError{} literal gets, including every
//     direct `fakeNetError{}` use elsewhere in this package's other _test.go files) is a genuine
//     connection-level failure: connection reset, broken pipe, DNS failure, TLS error, etc. This is
//     the ONLY kind of net.Error that should still trigger an early abort of remaining independent
//     actions -- every subsequent action really is doomed to fail the same way.
//   - timeout: true is sendAndWait's ordinary "no matching response within defaultCmdTimeout (8s)"
//     outcome (confirmed by TestWaitForTimeout in conn_wait_test.go) -- a normal, expected timeout on
//     one action's response on an otherwise-healthy connection. It must NOT abort remaining actions.
type fakeNetError struct {
	timeout bool
}

func (e fakeNetError) Error() string {
	if e.timeout {
		return "fake net.Error: simulated per-action response timeout"
	}
	return "fake net.Error: simulated dead connection"
}
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return false }

// TestCollectAllAbortsRemainingActionsOnNetError is the round-16 regression test for CollectAll's
// (buildings.go) net.Error early-abort. TestCollectAllAggregatesErrorsWithoutShortCircuiting above
// proves ordinary decoded errorCode failures must NOT short-circuit the run; this test proves the
// opposite must happen for a genuine (non-timeout) connection-level failure, mirroring
// FetchBuildings' own errors.As-against-net.Error check.
//
// timeout: false makes the fake connection's every Read fail with a fakeNetError standing in for a
// genuine dead connection (Timeout()==false; round-21 fix -- see fakeNetError's doc comment above for
// why Timeout()==true must NOT trigger this same abort, covered instead by
// TestCollectAllContinuesRemainingActionsOnNetErrorTimeout below). So CollectAll's very first
// sub-action -- CollectIdleReward's initial peek call -- fails immediately with a wrapped net.Error,
// before it ever gets to its own second (claim) call. Only that one request should ever be sent: if
// CollectAll didn't break early, GreetVisitors would be a no-op (visitors is nil) but ClaimAllMail,
// HelpAllianceMembers, ClaimAllianceGifts, DonateRecommendedAllianceTech, both VIP claims, and the
// one collectible building below would each still attempt a real request of their own.
//
// Mutation check: reverting CollectAll's actions/net.Error-break loop in buildings.go back to the
// old flat sequence of unconditional `errs = append(errs, ...)` calls makes this test fail with
// writeCount() == 9 (one per fixed sub-action plus the one building collect) instead of 1.
func TestCollectAllAbortsRemainingActionsOnNetError(t *testing.T) {
	fake := &fakeNetErrConn{timeout: false}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	buildings := []Building{newTestBuilding(501, BuildingFarmland, 2)}

	err := CollectAll(client, buildings, nil)

	if err == nil {
		t.Fatal("CollectAll() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("CollectAll() error = %v, want it to wrap a net.Error (the failure that triggered the break)", err)
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only CollectIdleReward's first request -- CollectAll should have aborted before any other sub-action or building collect)", got)
	}
}

// TestCollectAllContinuesRemainingActionsOnNetErrorTimeout is the round-21 regression test for the
// fix to CollectAll's net.Error early-abort (buildings.go): a net.Error whose Timeout() is true is
// sendAndWait's ordinary "no matching response within defaultCmdTimeout (8s)" outcome (confirmed by
// TestWaitForTimeout in conn_wait_test.go) -- a normal, expected timeout on one action's response on
// an otherwise-healthy connection, not evidence the connection is dead. It must NOT abort the
// remaining independent actions, exactly like TestCollectAllAggregatesErrorsWithoutShortCircuiting
// already proves for an ordinary decoded errorCode failure.
//
// timeout: true makes the fake connection's every Read fail with a fakeNetError whose Timeout() is
// true, so every one of CollectAll's 8 fixed sub-actions (visitors is nil, so GreetVisitors is a
// no-op) fails the same way in turn. If CollectAll incorrectly still broke early on this net.Error,
// only CollectIdleReward's first request would ever be sent; with the round-21 fix, every sub-action
// runs and gets a chance to write its own request(s) to the wire.
//
// Mutation check: reverting the !netErr.Timeout() guard in buildings.go back to the old bare
// errors.As(err, &netErr) check makes this test fail with writeCount() == 1 instead of > 1, and the
// aggregated error would then read as CollectIdleReward's lone failure instead of mentioning multiple
// simulated per-action timeouts.
func TestCollectAllContinuesRemainingActionsOnNetErrorTimeout(t *testing.T) {
	fake := &fakeNetErrConn{timeout: true}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := CollectAll(client, nil, nil)

	if err == nil {
		t.Fatal("CollectAll() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("CollectAll() error = %v, want it to still wrap a net.Error (every sub-action failed the same way)", err)
	}
	if !netErr.Timeout() {
		t.Errorf("CollectAll() error = %v, want the wrapped net.Error's Timeout() to be true", err)
	}
	// CollectIdleReward issues one request per attempt, so if it alone ran that would also produce
	// writeCount() == 1 -- indistinguishable from an incorrect early-abort on just that one action.
	// Asserting > 1 confirms sub-actions past CollectIdleReward (GreetVisitors is skipped: visitors
	// is nil) were actually attempted, not just that CollectIdleReward ran once.
	if got := fake.writeCount(); got <= 1 {
		t.Errorf("fake connection saw %d writes, want more than 1 (every one of CollectAll's fixed sub-actions should have been attempted, not just the first)", got)
	}
}

// TestContainsNonTimeoutNetError directly exercises containsNonTimeoutNetError (buildings.go) --
// the round-22 helper CollectAll's outer loop now uses in place of a plain `errors.As(err, &netErr)
// && !netErr.Timeout()` check. Covers the crux of the fix: errors.As itself only returns the FIRST
// net.Error match its own depth-first walk finds through a joined-error tree, so a bare
// errors.Join(timeout, non-timeout) and its reverse errors.Join(non-timeout, timeout) must BOTH
// report a non-timeout net.Error present -- order must not matter, unlike errors.As.
func TestContainsNonTimeoutNetError(t *testing.T) {
	timeoutErr := fakeNetError{timeout: true}
	nonTimeoutErr := fakeNetError{timeout: false}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"bare Timeout()==true net.Error", timeoutErr, false},
		{"bare Timeout()==false net.Error", nonTimeoutErr, true},
		{"errors.Join(timeout, timeout)", errors.Join(timeoutErr, timeoutErr), false},
		{"errors.Join(timeout, non-timeout)", errors.Join(timeoutErr, nonTimeoutErr), true},
		// Order must not matter -- this is the crux of the round-22 fix: a plain errors.As would
		// find whichever net.Error its depth-first walk hits first, which for
		// errors.Join(non-timeout, timeout) IS the genuine non-timeout net.Error (so a naive
		// errors.As-based check happens to get this particular ordering right by accident), but
		// errors.Join(timeout, non-timeout) above is the ordering where a plain errors.As gets it
		// wrong -- both must report true regardless of which position the non-timeout error is in.
		{"errors.Join(non-timeout, timeout)", errors.Join(nonTimeoutErr, timeoutErr), true},
		// mail.go's ClaimAllMail wraps its ListMail failure via fmt.Errorf("list mail: %w", err)
		// before appending it to errs and returning errors.Join(errs...) -- a different tree shape
		// than GreetVisitors/ClaimAllianceGifts, which append per-item errors unwrapped (see
		// TestCollectAllAbortsOnMaskedNonTimeoutNetErrorInJoinedSubActionError, which deliberately
		// uses ClaimAllianceGifts, not ClaimAllMail, as its repro vehicle). These two cases prove
		// containsNonTimeoutNetError correctly unwraps through that intermediate fmt.Errorf node --
		// via its `interface{ Unwrap() error }` branch -- before reaching the net.Error, for both the
		// genuine (non-timeout) and benign (timeout) cases, not just when a bare net.Error sits
		// directly in the Join like the cases above.
		{"errors.Join(fmt.Errorf-wrapped non-timeout)", errors.Join(fmt.Errorf("wrap: %w", nonTimeoutErr)), true},
		{"errors.Join(fmt.Errorf-wrapped timeout)", errors.Join(fmt.Errorf("wrap: %w", timeoutErr)), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsNonTimeoutNetError(tt.err); got != tt.want {
				t.Errorf("containsNonTimeoutNetError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// sequencedNetErrConn is a minimal net.Conn whose Read calls fail with a scripted sequence of
// net.Errors, one per call -- unlike fakeNetErrConn above, whose every Read fails identically
// (always timeout or always non-timeout, never a mix within one connection's lifetime). Calls past
// the end of the script repeat the script's last entry, so a test doesn't need to predict exactly
// how many extra reads a not-yet-fixed bug might cause before failing its own assertions.
//
// This is what TestCollectAllAbortsOnMaskedNonTimeoutNetErrorInJoinedSubActionError needs to
// reproduce the round-22 audit's own repro technique: a single sub-action (ClaimAllianceGifts, per
// the audit's suggestion) must see its FIRST item fail with a Timeout()==true net.Error (benign,
// its own round-21-fixed inner loop keeps going) and its SECOND item fail with a Timeout()==false
// net.Error (genuine dead connection, its own inner loop correctly breaks) -- producing exactly the
// errors.Join(timeout, non-timeout) tree that a plain errors.As-based check in CollectAll's outer
// loop could miss (see CollectAll's round-22 doc comment in buildings.go). Every other sub-action
// scheduled before it in CollectAll's fixed sequence also needs a scripted Timeout()==true failure
// of its own, purely so CollectAll's outer loop doesn't already have a legitimate reason to keep
// going or stop before ever reaching the sub-action under test -- their own returned errors are not
// otherwise interesting to this test.
//
// Writes always succeed and are counted, mirroring fakeNetErrConn, so a test can prove exactly how
// many requests were sent -- and, critically, that no MORE were sent after CollectAll's outer loop
// should have aborted.
type sequencedNetErrConn struct {
	mu       sync.Mutex
	writes   int
	reads    int
	timeouts []bool // Timeout() for the Nth Read call (0-indexed); the last entry repeats past the end
}

func (c *sequencedNetErrConn) Read([]byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	idx := c.reads
	if idx >= len(c.timeouts) {
		idx = len(c.timeouts) - 1
	}
	c.reads++
	return 0, fakeNetError{timeout: c.timeouts[idx]}
}

func (c *sequencedNetErrConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}

func (c *sequencedNetErrConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *sequencedNetErrConn) Close() error                     { return nil }
func (c *sequencedNetErrConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *sequencedNetErrConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *sequencedNetErrConn) SetDeadline(time.Time) error      { return nil }
func (c *sequencedNetErrConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sequencedNetErrConn) SetWriteDeadline(time.Time) error { return nil }

// TestCollectAllAbortsOnMaskedNonTimeoutNetErrorInJoinedSubActionError is the round-22 regression
// test for CollectAll's outer net.Error check (buildings.go): reproduces the audit's own repro
// technique against ClaimAllianceGifts (alliance.go), one of the three sub-actions whose own inner
// loop, per round 21, appends every per-item error -- including any earlier benign
// Timeout()==true -- to a local slice and returns one errors.Join(errs...) tree rather than a
// single error.
//
// The scripted read sequence below makes every sub-action scheduled before ClaimAllianceGifts in
// CollectAll's fixed order fail with a benign Timeout()==true net.Error (so CollectAll's outer loop
// has no reason to stop before reaching it), then makes ClaimAllianceGifts' own two gift-type
// requests fail Premium (type=1, first) with Timeout()==true and Regular (type=2, second) with
// Timeout()==false -- exactly the "starts flaking with timeouts, then actually dies" tree
// CollectAll's round-22 fix (containsNonTimeoutNetError) exists to still catch. ClaimAllianceGifts
// itself behaves correctly per round 21 (its own inner loop appends both errors and breaks after
// the second), returning errors.Join(timeoutErr, nonTimeoutErr) as ONE error to CollectAll.
//
// CollectAll's own fixed sequence (buildings.go's `actions` slice) is: CollectIdleReward,
// GreetVisitors, ClaimAllMail, HelpAllianceMembers, ClaimAllianceGifts,
// DonateRecommendedAllianceTech, ClaimVIPDailyLoginScore, ClaimVIPDailyFreebie, then one closure per
// collectible building. visitors and buildings are both passed nil/empty so GreetVisitors is a
// no-op (0 requests) and there are no building-collect closures, keeping the scripted read sequence
// short: CollectIdleReward's peek (1), ClaimAllMail's ListMail first page (1, short-circuits since
// zero mail was "found"), HelpAllianceMembers (1), then ClaimAllianceGifts' Premium and Regular
// requests (2) -- 5 reads/writes total before the mixed error is returned. If CollectAll correctly
// aborts right there, DonateRecommendedAllianceTech, both VIP claims, and (if the fix broke count
// still didn't skip them) any building collects must NEVER be attempted -- exactly what this test
// checks by asserting the write count stops at 5, not 8.
//
// Mutation check: reverting CollectAll's containsNonTimeoutNetError(err) check back to the old
// `errors.As(err, &netErr) && !netErr.Timeout()` makes this test fail with writeCount() == 8
// instead of 5, since errors.As would find ClaimAllianceGifts' first (benign, Timeout()==true)
// joined error before ever reaching its second (genuine) one, and CollectAll would incorrectly keep
// going through DonateRecommendedAllianceTech and both VIP claims.
func TestCollectAllAbortsOnMaskedNonTimeoutNetErrorInJoinedSubActionError(t *testing.T) {
	fake := &sequencedNetErrConn{
		timeouts: []bool{
			true,  // 1: CollectIdleReward's peek
			true,  // 2: ClaimAllMail's ListMail (first page)
			true,  // 3: HelpAllianceMembers
			true,  // 4: ClaimAllianceGifts' Premium (type=1) -- benign, its own loop keeps going
			false, // 5: ClaimAllianceGifts' Regular (type=2) -- genuine dead connection
		},
	}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := CollectAll(client, nil, nil)

	if err == nil {
		t.Fatal("CollectAll() = nil, want a non-nil error (every scripted read fails)")
	}
	if !containsNonTimeoutNetError(err) {
		t.Errorf("CollectAll() error = %v, want containsNonTimeoutNetError to still find the genuine non-timeout net.Error buried in ClaimAllianceGifts' joined result", err)
	}
	const wantWrites = 5 // idle-peek(1) + mail-list(1) + help(1) + gifts(2); see doc comment above
	if got := fake.writeCount(); got != wantWrites {
		t.Errorf("fake connection saw %d writes, want exactly %d (CollectAll should have aborted immediately after ClaimAllianceGifts' mixed timeout/non-timeout result, never attempting DonateRecommendedAllianceTech or either VIP claim)", got, wantWrites)
	}
}
