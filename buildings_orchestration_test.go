package main

import (
	"bufio"
	"bytes"
	"errors"
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

// fakeNetErrConn is a minimal net.Conn whose every Read fails with a fakeNetError -- a genuine
// connection-level failure (Timeout()==true), standing in for what a real silently-dead/blackholed
// TCP connection's eventual read-deadline expiry would produce, as opposed to a well-formed response
// carrying a decoded (possibly non-benign) errorCode. Writes succeed and are counted, so a test can
// prove exactly how many requests were sent before CollectAll's net.Error early-abort (buildings.go)
// fired.
type fakeNetErrConn struct {
	mu     sync.Mutex
	writes int
}

func (c *fakeNetErrConn) Read([]byte) (int, error) { return 0, fakeNetError{} }

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
// Temporary()), simulating a genuine connection-level failure rather than a decoded errorCode
// business-logic failure -- the same distinction FetchBuildings' own net.Error check (buildings.go)
// already makes, and login.go's waitForInitPush/TestWaitForTimeout (conn_wait_test.go) already rely
// on for the same purpose.
type fakeNetError struct{}

func (fakeNetError) Error() string   { return "fake net.Error: simulated dead connection" }
func (fakeNetError) Timeout() bool   { return true }
func (fakeNetError) Temporary() bool { return false }

// TestCollectAllAbortsRemainingActionsOnNetError is the round-16 regression test for CollectAll's
// (buildings.go) net.Error early-abort. TestCollectAllAggregatesErrorsWithoutShortCircuiting above
// proves ordinary decoded errorCode failures must NOT short-circuit the run; this test proves the
// opposite must happen for a genuine connection-level failure, mirroring FetchBuildings' own
// errors.As-against-net.Error check.
//
// The fake connection's Read always fails with fakeNetError, so CollectAll's very first sub-action
// -- CollectIdleReward's initial peek call -- fails immediately with a wrapped net.Error, before it
// ever gets to its own second (claim) call. Only that one request should ever be sent: if CollectAll
// didn't break early, GreetVisitors would be a no-op (visitors is nil) but ClaimAllMail,
// HelpAllianceMembers, ClaimAllianceGifts, DonateRecommendedAllianceTech, both VIP claims, and the
// one collectible building below would each still attempt a real request of their own.
//
// Mutation check: reverting CollectAll's actions/net.Error-break loop in buildings.go back to the
// old flat sequence of unconditional `errs = append(errs, ...)` calls makes this test fail with
// writeCount() == 9 (one per fixed sub-action plus the one building collect) instead of 1.
func TestCollectAllAbortsRemainingActionsOnNetError(t *testing.T) {
	fake := &fakeNetErrConn{}
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
