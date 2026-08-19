package main

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestAsExtension(t *testing.T) {
	t.Run("non-extension controller", func(t *testing.T) {
		env := &Envelope{Controller: controllerSystem, Content: NewSFSObject()}
		if _, ok := env.AsExtension(); ok {
			t.Fatal("expected ok=false for a non-extension controller")
		}
	})
	t.Run("nil content", func(t *testing.T) {
		env := &Envelope{Controller: controllerExtension, Content: nil}
		if _, ok := env.AsExtension(); ok {
			t.Fatal("expected ok=false for nil Content")
		}
	})
	t.Run("well-formed extension message", func(t *testing.T) {
		content := NewSFSObject()
		content.PutUtfString("c", "test.cmd")
		p := NewSFSObject()
		p.PutInt("foo", 42)
		content.PutSFSObject("p", p)
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok {
			t.Fatal("expected ok=true")
		}
		if msg.Cmd != "test.cmd" {
			t.Errorf("Cmd = %q, want test.cmd", msg.Cmd)
		}
		if msg.Params.GetInt("foo") != 42 {
			t.Errorf("Params.foo = %d, want 42", msg.Params.GetInt("foo"))
		}
	})
	t.Run("missing p defaults to empty params, not nil", func(t *testing.T) {
		content := NewSFSObject()
		content.PutUtfString("c", "test.cmd")
		env := &Envelope{Controller: controllerExtension, Content: content}
		msg, ok := env.AsExtension()
		if !ok || msg.Params == nil {
			t.Fatalf("expected ok=true and non-nil Params, got ok=%v params=%v", ok, msg.Params)
		}
	})
}

func newTestExtMsg(cmd string, errorCode any) *ExtensionMessage {
	params := NewSFSObject()
	if errorCode != nil {
		switch v := errorCode.(type) {
		case string:
			params.PutUtfString("errorCode", v)
		case int:
			params.PutInt("errorCode", int32(v))
		}
	}
	return &ExtensionMessage{Cmd: cmd, Params: params}
}

// newTestExtMsgWithStatus is newTestExtMsg plus an optional "status" int field, for exercising
// classifyResponse's status=0 benign heuristic (which, unlike errorCode, only fires for a
// specific cmd -- see TestClassifyResponse). hasStatus distinguishes "no status field at all"
// from "status=0" -- both need to be tested separately, and a bare int can't represent "absent".
func newTestExtMsgWithStatus(cmd string, errorCode any, status int32, hasStatus bool) *ExtensionMessage {
	msg := newTestExtMsg(cmd, errorCode)
	if hasStatus {
		msg.Params.PutInt("status", status)
	}
	return msg
}

func TestLogCommandResultClassification(t *testing.T) {
	// logCommandResult only logs; this smoke-tests all four branches its switch/if-else can
	// actually take (round 26: the previous "all three" wording only covered outcomeSuccess,
	// outcomeBenign-with-a-code, and outcomeFailure -- missing logCommandResult's inner else,
	// the outcomeBenign-with-an-EMPTY-errorCode case, i.e. the building.production.collect
	// status=0-with-no-errorCode log line -- which was untested by this test, or any other test
	// in the suite):
	//   1. outcomeSuccess
	//   2. outcomeBenign with a non-empty errorCode
	//   3. outcomeBenign with no errorCode (the status=0 heuristic, via newTestExtMsgWithStatus)
	//   4. outcomeFailure
	// Classification logic itself is asserted precisely by TestClassifyResponse above; this just
	// runs each branch through the real logger without panicking. Both benign cases must use their
	// actual documented cmd (building.production.collect) -- since both the errorCode scoping and
	// the status=0 heuristic are cmd-scoped, pairing either with an arbitrary "test.cmd" would
	// silently exercise a different branch than intended.
	logCommandResult("test success", newTestExtMsg("test.cmd", nil))
	logCommandResult("test benign", newTestExtMsg("building.production.collect", "602026"))
	logCommandResult("test benign no code", newTestExtMsgWithStatus("building.production.collect", nil, 0, true))
	logCommandResult("test real failure", newTestExtMsg("test.cmd", "999999"))
}

// TestClassifyResponse asserts classifyResponse's actual (outcome, code) return value directly,
// including that the status=0-with-no-errorCode benign heuristic is scoped to
// building.production.collect only -- for every other command a status=0 response with no
// errorCode is a real success, not a no-op (see classifyResponse's doc comment in conn.go) -- and
// that every benignErrorCodes entry is likewise scoped to its own documented cmd(s): the same
// numeric/string errorCode value on an unrelated cmd must fall through to outcomeFailure rather
// than being silently reclassified as outcomeBenign.
func TestClassifyResponse(t *testing.T) {
	tests := []struct {
		name        string
		cmd         string
		errorCode   any
		status      int32
		hasStatus   bool
		wantOutcome commandOutcome
		wantCode    string
	}{
		{
			name:        "no errorCode, no status field, any cmd -> success",
			cmd:         "some.other.cmd",
			hasStatus:   false,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=1, any cmd -> success",
			cmd:         "some.other.cmd",
			status:      1,
			hasStatus:   true,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=0, cmd=building.production.collect -> benign",
			cmd:         "building.production.collect",
			status:      0,
			hasStatus:   true,
			wantOutcome: outcomeBenign,
			wantCode:    "",
		},
		{
			name:        "no errorCode, status=0, other cmd -> success (heuristic must not apply globally)",
			cmd:         "mail.reward.batch",
			status:      0,
			hasStatus:   true,
			wantOutcome: outcomeSuccess,
			wantCode:    "",
		},
		{
			name:        "602026 on its documented cmd (building.production.collect) -> benign",
			cmd:         "building.production.collect",
			errorCode:   "602026",
			wantOutcome: outcomeBenign,
			wantCode:    "602026",
		},
		{
			name:        "602026 on an unrelated cmd -> failure (errorCode scoping must not leak across commands)",
			cmd:         "vip.reward.get",
			errorCode:   "602026",
			wantOutcome: outcomeFailure,
			wantCode:    "602026",
		},
		{
			name:        "120289 on vip.add.login.score (one of its two documented cmds) -> benign",
			cmd:         "vip.add.login.score",
			errorCode:   "120289",
			wantOutcome: outcomeBenign,
			wantCode:    "120289",
		},
		{
			name:        "120289 on vip.get.every.day.reward (the other documented cmd) -> benign",
			cmd:         "vip.get.every.day.reward",
			errorCode:   "120289",
			wantOutcome: outcomeBenign,
			wantCode:    "120289",
		},
		{
			name:        "visitor_err_coming on its documented cmd (visitor.operate) -> benign",
			cmd:         "visitor.operate",
			errorCode:   "visitor_err_coming",
			wantOutcome: outcomeBenign,
			wantCode:    "visitor_err_coming",
		},
		{
			name:        "120471 on its documented cmd (al.science.donate) -> benign",
			cmd:         "al.science.donate",
			errorCode:   "120471",
			wantOutcome: outcomeBenign,
			wantCode:    "120471",
		},
		{
			name:        "errorCode is not in benignErrorCodes at all -> failure",
			cmd:         "vip.reward.get",
			errorCode:   "999999",
			wantOutcome: outcomeFailure,
			wantCode:    "999999",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := newTestExtMsgWithStatus(tt.cmd, tt.errorCode, tt.status, tt.hasStatus)
			gotOutcome, gotCode := classifyResponse(msg)
			if gotOutcome != tt.wantOutcome || gotCode != tt.wantCode {
				t.Errorf("classifyResponse() = (%v, %q), want (%v, %q)", gotOutcome, gotCode, tt.wantOutcome, tt.wantCode)
			}
		})
	}
}

func TestGameConnSendReceiveRoundTrip(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	client := &GameConn{conn: c1, reader: bufio.NewReaderSize(c1, 4096)}
	server := &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}

	params := NewSFSObject()
	params.PutUtfString("hello", "world")

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- client.SendExtension("test.roundtrip", params)
	}()

	c2.SetReadDeadline(time.Now().Add(5 * time.Second))
	env, readErr := server.ReadEnvelope()
	if sendErr := <-sendDone; sendErr != nil {
		t.Fatalf("SendExtension: %v", sendErr)
	}
	if readErr != nil {
		t.Fatalf("ReadEnvelope: %v", readErr)
	}

	msg, ok := env.AsExtension()
	if !ok {
		t.Fatal("expected a well-formed extension message")
	}
	if msg.Cmd != "test.roundtrip" {
		t.Errorf("Cmd = %q, want test.roundtrip", msg.Cmd)
	}
	if got := msg.Params.GetString("hello"); got != "world" {
		t.Errorf("Params.hello = %q, want world", got)
	}
}
