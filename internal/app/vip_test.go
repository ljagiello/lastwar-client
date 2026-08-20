package app

import (
	"lastwar-client/internal/sfs"
	"testing"
)

// TestClaimVIPDailyLoginScoreSendsExactCmdAndEmptyParams checks the plain success path: per
// vip.go's own doc comment, ClaimVIPDailyLoginScore is genuinely parameterless on the wire (the
// decompiled OnCreate takes nothing beyond self), so this asserts both the exact cmd string sent
// and that no params are attached, mirroring TestGreetVisitorsAggregatesErrorsAndSkipsBenign's
// assert-the-exact-request-shape style (visitors_orchestration_test.go).
func TestClaimVIPDailyLoginScoreSendsExactCmdAndEmptyParams(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var gotCmd string
	var gotParamCount int
	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		gotCmd = msg.Cmd
		gotParamCount = len(msg.Params.Keys())
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	err := ClaimVIPDailyLoginScore(client)
	<-done

	if err != nil {
		t.Fatalf("ClaimVIPDailyLoginScore() = %v, want nil", err)
	}
	if gotCmd != "vip.add.login.score" {
		t.Errorf("cmd = %q, want vip.add.login.score", gotCmd)
	}
	if gotParamCount != 0 {
		t.Errorf("got %d params, want 0 (vip.add.login.score is genuinely parameterless on the wire)", gotParamCount)
	}
}

// TestClaimVIPDailyLoginScoreBenignAlreadyClaimedToday checks the documented benign cooldown
// path: per vip.go's own doc comment, replaying this call on an account that already claimed
// today gets errorCode=120289 ("no score"), which conn.go's benignErrorCodes map classifies as a
// non-fatal no-op -- ClaimVIPDailyLoginScore must return nil, not an error, for it.
func TestClaimVIPDailyLoginScoreBenignAlreadyClaimedToday(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := sfs.NewSFSObject()
		resp.PutUtfString("errorCode", "120289") // benignErrorCodes: "no score" -- already claimed today
		readAndReply(server, "", resp)
	}()

	if err := ClaimVIPDailyLoginScore(client); err != nil {
		t.Errorf("ClaimVIPDailyLoginScore() = %v, want nil (errorCode=120289 is a documented benign already-claimed-today no-op)", err)
	}
}

// TestClaimVIPDailyFreebieSendsExactCmdAndEmptyParams checks the plain success path: per vip.go's
// own doc comment, ClaimVIPDailyFreebie is also parameterless on the wire (the decompiled
// OnCreate declares an actId argument but never actually puts it on the sfs.SFSObject), so this
// asserts both the exact cmd string sent and that no params are attached.
func TestClaimVIPDailyFreebieSendsExactCmdAndEmptyParams(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	var gotCmd string
	var gotParamCount int
	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		gotCmd = msg.Cmd
		gotParamCount = len(msg.Params.Keys())
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	err := ClaimVIPDailyFreebie(client)
	<-done

	if err != nil {
		t.Fatalf("ClaimVIPDailyFreebie() = %v, want nil", err)
	}
	if gotCmd != "vip.get.every.day.reward" {
		t.Errorf("cmd = %q, want vip.get.every.day.reward", gotCmd)
	}
	if gotParamCount != 0 {
		t.Errorf("got %d params, want 0 (vip.get.every.day.reward is parameterless on the wire despite the decompiled actId argument)", gotParamCount)
	}
}

// TestClaimVIPDailyFreebieBenignAlreadyClaimedToday checks the documented benign cooldown path:
// per vip.go's own doc comment, replaying this call on an account that already claimed today gets
// errorCode=120289 ("no reward"), the same error code family as the login score above, which
// conn.go's benignErrorCodes map classifies as a non-fatal no-op -- ClaimVIPDailyFreebie must
// return nil, not an error, for it.
func TestClaimVIPDailyFreebieBenignAlreadyClaimedToday(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	go func() {
		resp := sfs.NewSFSObject()
		resp.PutUtfString("errorCode", "120289") // benignErrorCodes: "no reward" -- already claimed today
		readAndReply(server, "", resp)
	}()

	if err := ClaimVIPDailyFreebie(client); err != nil {
		t.Errorf("ClaimVIPDailyFreebie() = %v, want nil (errorCode=120289 is a documented benign already-claimed-today no-op)", err)
	}
}
