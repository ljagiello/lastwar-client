package main

import (
	"bufio"
	"bytes"
	"errors"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// TestHelpAllianceMembersSuccess checks the plain success path: HelpAllianceMembers sends
// `al.help.all` and returns nil once it gets back a normal (no errorCode) response.
func TestHelpAllianceMembersSuccess(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := NewSFSObject()
		resp.PutLong("accPoint", 42)
		readAndReply(server, "", resp)
	}()

	if err := HelpAllianceMembers(client); err != nil {
		t.Errorf("HelpAllianceMembers() = %v, want nil", err)
	}
}

// TestClaimAllianceGiftsSendsBothTypes checks that ClaimAllianceGifts sends one
// `alliance.reward.allreceive` per gift type -- Premium (1) then Regular (2), in that order, per
// the loop in alliance.go -- and returns nil once both get a real success response.
func TestClaimAllianceGiftsSendsBothTypes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var gotTypes []int32
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
			if msg.Cmd != "alliance.reward.allreceive" {
				t.Errorf("Cmd = %q, want alliance.reward.allreceive", msg.Cmd)
			}
			gotTypes = append(gotTypes, msg.Params.GetInt("type"))
			resp := NewSFSObject()
			resp.PutInt("receiveResult", 1)
			_ = server.SendExtension(msg.Cmd, resp)
		}
	}()

	err := ClaimAllianceGifts(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading both requests")
	}

	if err != nil {
		t.Fatalf("ClaimAllianceGifts() = %v, want nil", err)
	}
	if len(gotTypes) != 2 || gotTypes[0] != allianceGiftPremium || gotTypes[1] != allianceGiftRegular {
		t.Errorf("got types %v, want [%d %d] (Premium then Regular, in order)", gotTypes, allianceGiftPremium, allianceGiftRegular)
	}
}

// TestClaimAllianceGiftsAbortsRemainingTypesOnNetError is the round-18 regression test for
// ClaimAllianceGifts' net.Error early-abort, updated in round 21 to use a genuine (non-timeout)
// net.Error: the loop over the 2 gift types (alliance.go) mirrors CollectAll's identical
// errors.As-against-net.Error-and-!Timeout() early-abort (buildings.go) and ClaimAllMail's
// (mail.go) -- append the triggering error to errs, then break, rather than unconditionally
// attempting the Regular (type=2) request after the Premium (type=1) request already failed with
// a genuine connection-level net.Error. The underlying connection is known-dead at that point, so
// the second request is already doomed to independently burn a full defaultCmdTimeout before
// failing the exact same way.
//
// It reuses fakeNetErrConn/fakeNetError (buildings_orchestration_test.go, same package) with the
// default timeout: false, so the fake connection's every Read fails with a fakeNetError whose
// Timeout() is false (a stand-in for connection reset/broken pipe/DNS failure/TLS error, not an
// ordinary per-request timeout), so ClaimAllianceGifts' very first request -- the Premium (type=1)
// claim -- fails immediately with a wrapped, non-timeout net.Error. Only that one request should
// ever be sent.
//
// Mutation check: reverting ClaimAllianceGifts' loop back to the old flat
// `errs = append(errs, err)`-with-no-break shape makes this test fail with writeCount() == 2
// instead of 1.
func TestClaimAllianceGiftsAbortsRemainingTypesOnNetError(t *testing.T) {
	fake := &fakeNetErrConn{}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllianceGifts(client)

	if err == nil {
		t.Fatal("ClaimAllianceGifts() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("ClaimAllianceGifts() error = %v, want it to wrap a net.Error (the failure that triggered the break)", err)
	} else if netErr.Timeout() {
		t.Errorf("ClaimAllianceGifts() error's net.Error has Timeout()==true, want false (this test simulates a genuine dead connection, not an ordinary timeout)")
	}
	if got := fake.writeCount(); got != 1 {
		t.Errorf("fake connection saw %d writes, want exactly 1 (only the Premium/type=1 request -- ClaimAllianceGifts should have aborted before the Regular/type=2 request)", got)
	}
}

// TestClaimAllianceGiftsContinuesAfterNetErrorTimeoutOnFirstType is the round-21 regression test
// proving the net.Error early-abort in ClaimAllianceGifts (alliance.go) only fires for a genuine
// (non-timeout) net.Error: sendAndWait's ordinary "no matching response within
// defaultCmdTimeout" outcome is ITSELF a net.Error with Timeout()==true (confirmed via
// conn_wait_test.go's TestWaitForTimeout) -- an expected result on a perfectly healthy
// connection, not evidence the connection is dead. A Timeout()==true net.Error on the Premium
// (type=1) request must fall through like any other per-request failure and NOT stop the Regular
// (type=2) request from still being attempted, and the timeout error itself must still show up in
// the aggregated result.
//
// It reuses fakeNetErrConn/fakeNetError (buildings_orchestration_test.go, same package) with
// timeout: true, so the fake connection's every Read fails with a fakeNetError whose Timeout() is
// true, so both requests fail the same way; both should still be sent.
//
// Mutation check: reverting the alliance.go fix back to the bare
// `if errors.As(err, &netErr) { break }` (no !netErr.Timeout()) makes this test fail with
// writeCount() == 1 instead of 2.
func TestClaimAllianceGiftsContinuesAfterNetErrorTimeoutOnFirstType(t *testing.T) {
	fake := &fakeNetErrConn{timeout: true}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllianceGifts(client)

	if err == nil {
		t.Fatal("ClaimAllianceGifts() = nil, want a non-nil error (the fake connection's every Read fails)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("ClaimAllianceGifts() error = %v, want it to wrap a net.Error (the timeout that must still be recorded)", err)
	} else if !netErr.Timeout() {
		t.Errorf("ClaimAllianceGifts() error's net.Error has Timeout()==false, want true (this test simulates an ordinary per-request timeout)")
	}
	if got := fake.writeCount(); got != 2 {
		t.Errorf("fake connection saw %d writes, want exactly 2 (both the Premium/type=1 and Regular/type=2 requests -- an ordinary timeout on the first must not abort the second)", got)
	}
}

// TestClaimAllianceGiftsContinuesAfterBusinessErrorOnFirstType proves the round-18 net.Error
// early-abort fix above did not regress the pre-existing no-short-circuit-on-business-errors
// behavior: an ordinary decoded errorCode failure on the Premium (type=1) claim must not stop the
// Regular (type=2) claim from still being attempted -- only a genuine net.Error should do that
// (see TestClaimAllianceGiftsAbortsRemainingTypesOnNetError above).
func TestClaimAllianceGiftsContinuesAfterBusinessErrorOnFirstType(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var gotTypes []int32
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
			if msg.Cmd != "alliance.reward.allreceive" {
				t.Errorf("Cmd = %q, want alliance.reward.allreceive", msg.Cmd)
			}
			gotTypes = append(gotTypes, msg.Params.GetInt("type"))
			resp := NewSFSObject()
			if i == 0 {
				resp.PutUtfString("errorCode", "999999") // genuine failure, not benign
			} else {
				resp.PutInt("receiveResult", 1)
			}
			_ = server.SendExtension(msg.Cmd, resp)
		}
	}()

	err := ClaimAllianceGifts(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished reading both requests")
	}

	if err == nil {
		t.Fatal("ClaimAllianceGifts() = nil, want a non-nil error (the Premium/type=1 request got a genuine failure)")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("ClaimAllianceGifts() error = %v, want it to mention the type=1 failure's errorCode 999999", err)
	}
	if len(gotTypes) != 2 || gotTypes[0] != allianceGiftPremium || gotTypes[1] != allianceGiftRegular {
		t.Errorf("got types %v, want [%d %d] (Premium then Regular, in order) -- an ordinary business error must not abort the loop", gotTypes, allianceGiftPremium, allianceGiftRegular)
	}
}

// allianceScienceEntry builds one well-formed allianceScience-array entry, matching the
// scienceId/state pair science.data.refresh returns per findRecommendedTech's doc comment.
func allianceScienceEntry(scienceId, state int32) *SFSObject {
	e := NewSFSObject()
	e.PutInt("scienceId", scienceId)
	e.PutInt("state", state)
	return e
}

// allianceScienceEntryNullScienceId builds a state==1 entry whose scienceId field is present but
// explicitly null on the wire -- SFSValue{sfsNull, nil} -- rather than simply absent.
// o.put is unexported but same-package, so this constructs the same shape DecodeObject would
// produce for a real explicit-null field (sfsobject.go:553-554), which Has() alone can't tell
// apart from a genuine value.
func allianceScienceEntryNullScienceId(state int32) *SFSObject {
	e := NewSFSObject()
	e.put("scienceId", SFSValue{sfsNull, nil})
	e.PutInt("state", state)
	return e
}

// allianceScienceRefreshResponse builds a science.data.refresh response carrying the given
// allianceScience entries.
func allianceScienceRefreshResponse(entries ...*SFSObject) *SFSObject {
	arr := NewSFSArray()
	for _, e := range entries {
		arr.AddSFSObject(e)
	}
	resp := NewSFSObject()
	resp.PutSFSArray("allianceScience", arr)
	return resp
}

// TestFindRecommendedTech directly exercises findRecommendedTech -- pulled out of
// DonateRecommendedAllianceTech, per its own doc comment, specifically so it can be unit tested
// without a live connection.
func TestFindRecommendedTech(t *testing.T) {
	t.Run("state==1 entry's scienceId is returned", func(t *testing.T) {
		arr := NewSFSArray()
		arr.AddSFSObject(allianceScienceEntry(111, 0))
		arr.AddSFSObject(allianceScienceEntry(555, 1))

		id, found := findRecommendedTech(arr)
		if !found || id != 555 {
			t.Errorf("findRecommendedTech() = (%d, %v), want (555, true)", id, found)
		}
	})
	t.Run("no state==1 entry", func(t *testing.T) {
		arr := NewSFSArray()
		arr.AddSFSObject(allianceScienceEntry(111, 0))
		arr.AddSFSObject(allianceScienceEntry(222, 0))

		if _, found := findRecommendedTech(arr); found {
			t.Error("findRecommendedTech() found=true, want false (no entry has state==1)")
		}
	})
	t.Run("state==1 entry missing scienceId entirely is skipped", func(t *testing.T) {
		bad := NewSFSObject()
		bad.PutInt("state", 1) // no scienceId field
		arr := NewSFSArray()
		arr.AddSFSObject(bad)

		if _, found := findRecommendedTech(arr); found {
			t.Error("findRecommendedTech() found=true, want false (scienceId field is entirely missing)")
		}
	})
	t.Run("state==1 entry with explicit-null scienceId is skipped, not returned as 0", func(t *testing.T) {
		arr := NewSFSArray()
		arr.AddSFSObject(allianceScienceEntryNullScienceId(1))

		if id, found := findRecommendedTech(arr); found {
			t.Errorf("findRecommendedTech() = (%d, true), want found=false (explicit-null scienceId must not fall through to scienceId=0)", id)
		}
	})
	t.Run("non-object array item is skipped, not fatal", func(t *testing.T) {
		arr := NewSFSArray()
		arr.AddInt(12345) // malformed: not an SFSObject at all
		arr.AddSFSObject(allianceScienceEntry(777, 1))

		id, found := findRecommendedTech(arr)
		if !found || id != 777 {
			t.Errorf("findRecommendedTech() = (%d, %v), want (777, true) (the non-object item should be skipped, not stop the scan)", id, found)
		}
	})
	t.Run("empty array", func(t *testing.T) {
		arr := NewSFSArray()
		if _, found := findRecommendedTech(arr); found {
			t.Error("findRecommendedTech() found=true, want false for an empty array")
		}
	})
}

// TestDonateRecommendedAllianceTechNoAllianceScienceField checks the first documented no-op
// branch: a science.data.refresh response with no allianceScience field at all must return nil,
// not an error, and must not go on to send a donate request. No second fake-server reader is set
// up below, so a regression that did send one would hang inside SendExtension and this test would
// time out rather than silently pass -- the same tradeoff TestGreetVisitorsEmpty
// (visitors_orchestration_test.go) already accepts for its own short-circuit check.
func TestDonateRecommendedAllianceTechNoAllianceScienceField(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		readAndReply(server, "", NewSFSObject())
	}()

	if err := DonateRecommendedAllianceTech(client); err != nil {
		t.Errorf("DonateRecommendedAllianceTech() = %v, want nil (missing allianceScience field is a documented no-op)", err)
	}
}

// TestDonateRecommendedAllianceTechWrongFieldType checks the second documented no-op branch: an
// allianceScience field present but not an SFSArray must return nil, not an error, and must not
// go on to send a donate request (same no-second-reader reasoning as the test above).
//
// It's also the regression test for this round's fix: before it, this branch returned nil with
// zero logging, unlike the sibling branch two lines above (allianceScience field entirely
// missing) which already logs an explanatory Info message -- if the server response shape ever
// changed, -collect runs would silently stop donating alliance tech with no trace in the logs to
// explain why. The fake server below sends allianceScience as a string instead of an array, and
// the test asserts a Warn fires mentioning the field.
func TestDonateRecommendedAllianceTechWrongFieldType(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := NewSFSObject()
		resp.PutUtfString("allianceScience", "not-an-array")
		readAndReply(server, "", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	if err := DonateRecommendedAllianceTech(client); err != nil {
		t.Errorf("DonateRecommendedAllianceTech() = %v, want nil (non-array allianceScience field is tolerated)", err)
	}
	if logged := buf.String(); !strings.Contains(logged, "allianceScience") {
		t.Errorf("expected a warning mentioning allianceScience when the field is present but not an array, got log:\n%s", logged)
	}
}

// TestDonateRecommendedAllianceTechNoRecommendedEntry checks the third documented no-op branch: a
// well-formed allianceScience array with no state==1 entry must return nil, not an error, and must
// not go on to send a donate request (same no-second-reader reasoning as above).
func TestDonateRecommendedAllianceTechNoRecommendedEntry(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		readAndReply(server, "", allianceScienceRefreshResponse(
			allianceScienceEntry(111, 0),
			allianceScienceEntry(222, 0),
		))
	}()

	if err := DonateRecommendedAllianceTech(client); err != nil {
		t.Errorf("DonateRecommendedAllianceTech() = %v, want nil (no state==1 entry is a documented no-op)", err)
	}
}

// TestDonateRecommendedAllianceTechNullScienceIdSkipped is the regression test for this round's
// fix: findRecommendedTech now guards scienceId with requirePresentField instead of a bare
// Has("scienceId") check. Before that fix, an explicit-null scienceId on the state==1 entry still
// passed Has() (it only reflects key presence), so GetInt("scienceId") fell through to its zero
// value and DonateRecommendedAllianceTech would go on to send a real al.science.donate for
// scienceId=0 instead of skipping the malformed entry. No second fake-server reader is set up
// here, so a regression back to that behavior would hang inside SendExtension and this test would
// time out rather than silently pass.
func TestDonateRecommendedAllianceTechNullScienceIdSkipped(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		readAndReply(server, "", allianceScienceRefreshResponse(allianceScienceEntryNullScienceId(1)))
	}()

	if err := DonateRecommendedAllianceTech(client); err != nil {
		t.Errorf("DonateRecommendedAllianceTech() = %v, want nil (explicit-null scienceId must be skipped, not donated to as scienceId=0)", err)
	}
}

// TestDonateRecommendedAllianceTechDonates checks the real donation path: given a state==1 entry
// with a well-formed scienceId, DonateRecommendedAllianceTech must send al.science.donate with
// that exact scienceId and option=1, then return nil once it gets a real success response.
func TestDonateRecommendedAllianceTechDonates(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var donateCmd string
	var gotScienceID, gotOption int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		readAndReply(server, "", allianceScienceRefreshResponse(
			allianceScienceEntry(111, 0),
			allianceScienceEntry(555, 1), // the recommended one
		))

		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		donateCmd = msg.Cmd
		gotScienceID = msg.Params.GetInt("scienceId")
		gotOption = msg.Params.GetInt("option")
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	err := DonateRecommendedAllianceTech(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished the refresh+donate round trip")
	}

	if err != nil {
		t.Fatalf("DonateRecommendedAllianceTech() = %v, want nil", err)
	}
	if donateCmd != "al.science.donate" {
		t.Errorf("donate cmd = %q, want al.science.donate", donateCmd)
	}
	if gotScienceID != 555 {
		t.Errorf("donate scienceId = %d, want 555 (the state==1 entry)", gotScienceID)
	}
	if gotOption != 1 {
		t.Errorf("donate option = %d, want 1", gotOption)
	}
}

// TestDonateRecommendedAllianceTechBenignCooldown checks the realistic, commonly-hit outcome
// documented extensively in DonateRecommendedAllianceTech's own doc comment: al.science.donate
// gates repeat donations within a ~20-minute cooldown window, confirmed live via
// errorCode=120471 ("Donate science CD time is not finish"), which conn.go's benignErrorCodes map
// classifies as a non-fatal no-op. DonateRecommendedAllianceTech must return nil, not an error,
// when the fake server replies to al.science.donate with that errorCode -- same
// TestGreetVisitorsAggregatesErrorsAndSkipsBenign-style benign-errorCode pattern
// (visitors_orchestration_test.go) used for the success-path test above.
func TestDonateRecommendedAllianceTechBenignCooldown(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var donateCmd string
	done := make(chan struct{})
	go func() {
		defer close(done)
		readAndReply(server, "", allianceScienceRefreshResponse(
			allianceScienceEntry(555, 1), // the recommended one
		))

		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		donateCmd = msg.Cmd
		resp := NewSFSObject()
		resp.PutUtfString("errorCode", "120471") // benignErrorCodes: al.science.donate cooldown
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	err := DonateRecommendedAllianceTech(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished the refresh+donate round trip")
	}

	if err != nil {
		t.Fatalf("DonateRecommendedAllianceTech() = %v, want nil (errorCode=120471 is a documented benign donate-cooldown no-op)", err)
	}
	if donateCmd != "al.science.donate" {
		t.Errorf("donate cmd = %q, want al.science.donate", donateCmd)
	}
}
