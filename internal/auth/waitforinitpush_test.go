package auth

import (
	"errors"
	"lastwar-client/internal/game"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"net"
	"strings"
	"testing"
	"time"
)

// TestWaitForInitPushHalfwayActivePull checks waitForInitPush's two-phase deadline scheme: when
// the server stays completely silent (no `init` push ever arrives), the login.init active-pull
// fallback still gets sent roughly at the halfway point of the window, not at the very start and
// not saved for the very end -- which is the entire point of capping the first read's deadline at
// the halfway mark instead of the full window (see waitForInitPush's doc comment in login.go).
func TestWaitForInitPushHalfwayActivePull(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)

	const window = 200 * time.Millisecond
	activePullAt := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				continue
			}
			if msg.Cmd == "login.init" {
				activePullAt <- time.Since(start)
				return // stay silent for the rest of the window -- the `init` push is never sent
			}
		}
	}()

	buildings, visitors, gotInit, err := waitForInitPush(client, window)

	if gotInit {
		t.Fatalf("expected gotInit=false (server never sends the init push), got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err != nil {
		t.Errorf("err = %v, want nil (a genuine silence-until-deadline timeout is not an error)", err)
	}

	select {
	case elapsed := <-activePullAt:
		// login.init should fire roughly at the halfway point (window/2 = 100ms here), not
		// essentially immediately and not saved for the very end. These bounds are deliberately
		// wide (window/8 to window*7/8, i.e. 25ms-175ms of the 200ms window here) rather than a
		// tight quarter/three-quarter band: this is a wall-clock assertion with no injectable
		// clock, so it's inherently susceptible to scheduler/CI jitter, and this test is only
		// trying to prove the active pull fired somewhere in the middle of the window -- not pin
		// down precisely when.
		if elapsed < window/8 || elapsed > window*7/8 {
			t.Errorf("login.init sent at %v, want roughly the halfway point (~%v of a %v window)", elapsed, window/2, window)
		}
	default:
		t.Fatal("expected waitForInitPush to send login.init as an active-pull fallback partway through the window, but it never arrived")
	}
}

// TestWaitForInitPushConnectionFailure is the regression test for waitForInitPush's terminal-error
// return: a genuine connection failure (here: the peer closing, giving ReadEnvelope a real EOF/
// closed-pipe error) must be visible to the caller as a non-nil, non-timeout error -- not silently
// collapsed into the exact same (nil, nil, false, nil) outcome as a plain silence-until-deadline
// timeout, which is what waitForInitPush used to return unconditionally (discarding the real
// ReadEnvelope error entirely; see login.go's call site and doc comment). Mirrors
// TestWaitForInitPushHalfwayActivePull's structure, but forces a real error instead of letting the
// server just stay silent.
func TestWaitForInitPushConnectionFailure(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	_ = server.RawConn().Close() // simulate a real connection failure (EOF/reset), not silence

	const window = 200 * time.Millisecond
	start := time.Now()
	buildings, visitors, gotInit, err := waitForInitPush(client, window)
	elapsed := time.Since(start)

	if gotInit {
		t.Fatalf("expected gotInit=false, got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err == nil {
		t.Fatal("expected a non-nil error for a genuine connection failure, got nil (indistinguishable from a plain timeout)")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Errorf("err = %v, want a genuine connection-failure error, not a timeout error", err)
	}
	if elapsed > window {
		t.Errorf("waitForInitPush took %v, want it to return promptly on connection failure rather than waiting out the full %v window", elapsed, window)
	}
}

// TestWaitForInitPushSendExtensionFailure is the round-19 regression test for waitForInitPush's
// handling of a failed login.init active-pull send: previously, a SendExtension error here was
// only logged, and execution fell through unconditionally into the next blocking ReadEnvelope --
// so a local write failure (a plausible half-open-connection symptom, since a write error can
// surface fast while a peer that never actually closes the connection leaves the read blocking
// until the deadline) got silently downgraded into an ordinary silence-until-deadline timeout
// instead of the definite initErr!=nil connection-failure result Login() is built to fail-fast
// on. Forces this deterministically with writeFailConn rather than relying on a race: the read
// side is left as a genuinely-blocking, non-EOF net.Pipe (no peer close at all), so the *only*
// way this test's error can surface is via the send failure itself, and only a fix that returns
// immediately on that failure -- not one that just logs and keeps waiting -- can make this test
// pass promptly instead of timing out the full window.
//
// Round 31 fix: this test used to inject a plain errors.New(...) (not a net.Error) as writeErr,
// and its only net.Error-related assertion was "if errors.As(err, &netErr) && netErr.Timeout()"
// with no t.Fatalf requiring errors.As to succeed first -- so with sendStageError's wrap REMOVED,
// the raw errors.New(...) doesn't satisfy net.Error at all, errors.As fails, the && short-circuits
// false, and the whole Timeout() check is silently skipped rather than failing the test: the test
// passed identically whether or not the round-30 sendStageError wrap actually existed. Now injects
// fakeTimeoutNetError{} (a genuine net.Error with Timeout()==true, mirroring
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError's own technique above) and requires
// errors.As to succeed via t.Fatalf before asserting Timeout()==false -- a pattern that WOULD catch
// the wrap's removal, since removing it would leave the raw fakeTimeoutNetError's Timeout()==true
// value visible straight through errors.As.
func TestWaitForInitPushSendExtensionFailure(t *testing.T) {
	client, _ := session.NewPipeGameConnPair(t) // server intentionally left idle: no reply, no close
	writeErr := testutil.FakeTimeoutNetError{Msg: "simulated write failure (e.g. half-open connection)"}
	client.SetRawConn(&writeFailConn{Conn: client.RawConn(), err: writeErr})

	const window = 200 * time.Millisecond
	start := time.Now()
	buildings, visitors, gotInit, err := waitForInitPush(client, window)
	elapsed := time.Since(start)

	if gotInit {
		t.Fatalf("expected gotInit=false, got true (buildings=%v visitors=%v)", buildings, visitors)
	}
	if err == nil {
		t.Fatal("expected a non-nil error when the login.init active-pull send fails, got nil (indistinguishable from a plain timeout)")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap/equal the SendExtension failure %v", err, writeErr)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("err = %v (%T), want it to satisfy net.Error (via sendStageError's wrap)", err, err)
	}
	if netErr.Timeout() {
		t.Errorf("netErr.Timeout() = true, want false -- a send-stage failure must be distinguishable from a benign wait-stage timeout, even though the underlying write failure itself reports Timeout()==true (mirroring a real deadline-exceeded net.Conn.Write)")
	}
	// The active pull only fires at the halfway point (window/2), so the earliest this can
	// possibly return is ~window/2, not immediately from start -- but it must return well before
	// the full window elapses, proving it didn't fall through into the blocking read-and-wait
	// path after logging the send failure.
	if elapsed > window*3/4 {
		t.Errorf("waitForInitPush took %v, want it to return promptly after the failed send rather than waiting out the full %v window", elapsed, window)
	}
}

// TestWaitForInitPushConsecutiveDecodeFailuresBoundary is the round-50 regression test for the
// MAJOR finding that waitForInitPush's round-48 fix (tolerating a corrupt push instead of aborting
// the whole login) removed the only thing that used to bound how long a hostile peer could keep
// this loop reading: nothing capped how many consecutive malformed/undecodable frames it would
// silently tolerate before the caller-supplied wall-clock timeout eventually fired. Proves
// maxConsecutiveDecodeFailures (login.go) is a strict `>` bound here too: exactly that many corrupt
// frames in a row still lets the following valid init push be parsed, one more makes the function
// give up early with a wrapped error instead of continuing to tolerate them.
func TestWaitForInitPushConsecutiveDecodeFailuresBoundary(t *testing.T) {
	sendCorruptThenInit := func(t *testing.T, n int) (buildings []game.Building, visitors []game.Visitor, gotInit bool, err error) {
		client, server := session.NewPipeGameConnPair(t)
		// Deliberately not gated on the server goroutine finishing: once waitForInitPush gives up
		// early (the cap+1 case), the client stops reading mid-stream and net.Pipe's
		// unbuffered/synchronous Write blocks forever waiting for a reader that will never come
		// back -- NewPipeGameConnPair's own t.Cleanup closes both ends of the pipe at test end,
		// which unblocks that pending write with an error and lets the goroutine exit; there's
		// nothing else worth synchronizing on here.
		go func() {
			for range n {
				if _, werr := server.RawConn().Write(testutil.MustEncodeCorruptPacket(t, "field", "value")); werr != nil {
					return
				}
			}
			params := sfs.NewSFSObject()
			arr := sfs.NewSFSArray()
			arr.AddSFSObject(game.NewTestBuildingSFS(111, game.BuildingFarmland, 3))
			params.PutSFSArray("building_new", arr)
			_ = server.SendExtension("init", params)
		}()

		// A window well short of the halfway-point active-pull fallback, so this test only ever
		// exercises the plain read loop above it, not the login.init active-pull send path.
		return waitForInitPush(client, 300*time.Millisecond)
	}

	t.Run("exactly cap consecutive corrupt frames: init push still parsed", func(t *testing.T) {
		buildings, _, gotInit, err := sendCorruptThenInit(t, session.MaxConsecutiveDecodeFailures)
		if err != nil {
			t.Fatalf("waitForInitPush() error = %v, want nil (exactly the cap must still be tolerated)", err)
		}
		if !gotInit {
			t.Fatal("gotInit = false, want true (the init push must still be reached and parsed)")
		}
		if len(buildings) != 1 || buildings[0].Uuid() != 111 {
			t.Fatalf("buildings = %+v, want exactly one building with uuid 111", buildings)
		}
	})

	t.Run("cap+1 consecutive corrupt frames: gives up", func(t *testing.T) {
		buildings, visitors, gotInit, err := sendCorruptThenInit(t, session.MaxConsecutiveDecodeFailures+1)
		if err == nil {
			t.Fatal("waitForInitPush() error = nil, want an error once the consecutive-failure cap is exceeded")
		}
		if gotInit {
			t.Fatalf("gotInit = true, want false (buildings=%v visitors=%v)", buildings, visitors)
		}
		if !strings.Contains(err.Error(), "consecutive malformed/undecodable pushes") {
			t.Errorf("err = %v, want it to mention the consecutive-failure cap being exceeded", err)
		}
		// Round-51 regression assertion: see TestWaitForConsecutiveDecodeFailuresBoundary's
		// identical assertion above for the full MAJOR-finding rationale -- this give-up error
		// must satisfy net.Error with Timeout()==false (via sfs.DeadConnError, login.go), not just
		// be a non-nil error, so Login()'s own containsNonTimeoutNetError-based callers treat
		// it as the fatal, connection-is-dead condition it represents.
		var netErr net.Error
		if !errors.As(err, &netErr) {
			t.Fatalf("err = %v (%T), want it to satisfy net.Error (via sfs.DeadConnError's wrap)", err, err)
		}
		if netErr.Timeout() {
			t.Errorf("netErr.Timeout() = true, want false")
		}
		if !session.ContainsNonTimeoutNetError(err) {
			t.Errorf("containsNonTimeoutNetError(err) = false, want true")
		}
	})
}

// TestWaitForInitPushNonMatchingEnvelopeCapBoundary is the round-53 regression test for the MAJOR
// finding that maxConsecutiveDecodeFailures only ever bounded consecutive DECODE FAILURES, not a
// stream of well-formed-but-irrelevant pushes -- see TestWaitForNonMatchingEnvelopeCapBoundary
// above for the full rationale. Proves maxNonMatchingEnvelopesPerWait (login.go) is a strict `>`
// bound for waitForInitPush too: exactly that many irrelevant pushes in a row still lets the
// following init push be parsed, one more makes it give up -- benignly (gotInit=false, err=nil),
// matching this function's own pre-existing silence-until-deadline convention, not a fatal error.
func TestWaitForInitPushNonMatchingEnvelopeCapBoundary(t *testing.T) {
	sendNoiseThenInit := func(t *testing.T, n int) (buildings []game.Building, gotInit bool, err error) {
		client, server := session.NewPipeGameConnPair(t)
		go func() {
			noise := sfs.NewSFSObject()
			noise.PutUtfString("irrelevant", "noise")
			for range n {
				if err := server.SendExtension("irrelevant.cmd", noise); err != nil {
					return
				}
			}
			params := sfs.NewSFSObject()
			arr := sfs.NewSFSArray()
			arr.AddSFSObject(game.NewTestBuildingSFS(111, game.BuildingFarmland, 3))
			params.PutSFSArray("building_new", arr)
			_ = server.SendExtension("init", params)
		}()

		// A window well short of the halfway-point active-pull fallback, matching this file's
		// other waitForInitPush boundary test's own reasoning.
		buildings, _, gotInit, err = waitForInitPush(client, 5*time.Second)
		return buildings, gotInit, err
	}

	t.Run("exactly cap non-matching pushes: init push still parsed", func(t *testing.T) {
		buildings, gotInit, err := sendNoiseThenInit(t, session.MaxNonMatchingEnvelopesPerWait)
		if err != nil {
			t.Fatalf("waitForInitPush() error = %v, want nil (exactly the cap must still be tolerated)", err)
		}
		if !gotInit {
			t.Fatal("gotInit = false, want true (the init push must still be reached and parsed)")
		}
		if len(buildings) != 1 || buildings[0].Uuid() != 111 {
			t.Fatalf("buildings = %+v, want exactly one building with uuid 111", buildings)
		}
	})

	t.Run("cap+1 non-matching pushes: gives up benignly", func(t *testing.T) {
		buildings, gotInit, err := sendNoiseThenInit(t, session.MaxNonMatchingEnvelopesPerWait+1)
		if err != nil {
			t.Errorf("waitForInitPush() error = %v, want nil (giving up on too many non-matching pushes is a benign give-up, not an error)", err)
		}
		if gotInit {
			t.Errorf("gotInit = true, want false (buildings=%v)", buildings)
		}
	})
}

// TestWaitForInitPushSurvivesNonExtensionEnvelope is the round-51 regression test for the MAJOR
// finding that waitForInitPush's `msg, ok := env.AsExtension(); if !ok { continue }` guard
// (login.go) had zero test coverage -- the structurally identical sibling of buildings.go's
// FetchBuildings guard (see TestFetchBuildingsSurvivesNonExtensionEnvelope,
// buildings_orchestration_test.go, for the full rationale): no existing test ever sends a
// non-extension (controllerSystem) envelope during waitForInitPush's wait window, even though the
// client's own background heartbeat sends exactly this shape every ~4s once running. Sends a raw
// controllerSystem envelope before a valid init push, proving waitForInitPush skips it silently
// instead of panicking on a nil msg dereference.
func TestWaitForInitPushSurvivesNonExtensionEnvelope(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)

	go func() {
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionPingPong, sfs.NewSFSObject()); err != nil {
			return
		}
		params := sfs.NewSFSObject()
		arr := sfs.NewSFSArray()
		arr.AddSFSObject(game.NewTestBuildingSFS(111, game.BuildingFarmland, 3))
		params.PutSFSArray("building_new", arr)
		_ = server.SendExtension("init", params)
	}()

	buildings, _, gotInit, err := waitForInitPush(client, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("waitForInitPush() error = %v, want nil", err)
	}
	if !gotInit {
		t.Fatal("gotInit = false, want true (the init push must still be reached and parsed)")
	}
	if len(buildings) != 1 || buildings[0].Uuid() != 111 {
		t.Fatalf("buildings = %+v, want exactly one building with uuid 111 (the non-extension envelope must be skipped, not panic)", buildings)
	}
}
