package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// handleInteractiveLine's pure JSON-decoding piece (putJSONValue) is covered directly in
// interactive_test.go. This file covers handleInteractiveLine itself over a net.Pipe-backed
// GameConn (newPipeGameConnPair, conn_wait_test.go), the same pattern conn_wait_test.go and
// visitors_orchestration_test.go use for their sendAndWait/GreetVisitors coverage.

// TestHandleInteractiveLineAbortsOnUnsupportedValue checks the "abort on unsupported JSON value"
// path: when putJSONValue rejects a key's value (here, a JSON array -- decoded by
// encoding/json into []any, which putJSONValue has no case for), handleInteractiveLine must log
// and return without ever calling conn.SendExtension. Proving that requires more than "the call
// returned" -- net.Pipe's Read/Write rendezvous synchronously with no buffering, so if
// handleInteractiveLine sent anything here it would block forever with nobody on the other end
// reading, and this test would hang instead of failing cleanly. Running it in a goroutine with a
// bounded wait turns that hang into a clean timeout failure.
func TestHandleInteractiveLineAbortsOnUnsupportedValue(t *testing.T) {
	client, _ := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `test.cmd {"bad":[1,2,3]}`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly -- it likely tried to send despite the unsupported param value")
	}
}

// TestHandleInteractiveLineSendsParsedCommand checks the normal path: a well-formed
// "cmd.name {json}" line is parsed into an SFSObject and sent as an Extension call with that exact
// cmd, and a matching reply within the window is read back without handleInteractiveLine erroring
// out (it just logs -- there's no return value to assert on, so we assert on what the fake server
// on the other end of the pipe actually received).
func TestHandleInteractiveLineSendsParsedCommand(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type got struct {
		cmd    string
		params *SFSObject
	}
	gotCh := make(chan got, 1)
	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		gotCh <- got{msg.Cmd, msg.Params}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `some.command {"key":"value","num":42}`)
	}()

	var g got
	select {
	case g = <-gotCh:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never received the Extension call")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return after receiving its reply")
	}

	if g.cmd != "some.command" {
		t.Errorf("Cmd = %q, want some.command", g.cmd)
	}
	if got := g.params.GetString("key"); got != "value" {
		t.Errorf(`params["key"] = %q, want "value"`, got)
	}
	if got := g.params.GetLong("num"); got != 42 {
		t.Errorf(`params["num"] = %d, want 42`, got)
	}
}

// TestHandleInteractiveLineRedactsCredentialFields is interactive.go's sibling of
// TestLoginEmailVerificationPushErrorDoesNotLeakLoginKey (login_integration_test.go):
// RunInteractive's whole purpose is trying arbitrary commands -- including, plausibly,
// account/login-family ones -- against a real authenticated session, so its "sending
// command"/"received response" log lines must never dump a live credential in cleartext, on
// either the outgoing params or the incoming response.
func TestHandleInteractiveLineRedactsCredentialFields(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const secretLoginKey = "sensitive-secret-loginkey-must-not-leak-1234567890"
	const secretPw = "sensitive-secret-outgoing-pw-must-not-leak-abcdef"

	go func() {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		resp := NewSFSObject()
		resp.PutUtfString("loginKey", secretLoginKey)
		resp.PutUtfString("gameUid", "g1")
		_ = server.SendExtension(msg.Cmd, resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, `account.login.new {"pw":"`+secretPw+`"}`)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleInteractiveLine did not return promptly")
	}

	logged := buf.String()
	if strings.Contains(logged, secretLoginKey) {
		t.Errorf("interactive log output leaks the response loginKey in cleartext:\n%s", logged)
	}
	if strings.Contains(logged, secretPw) {
		t.Errorf("interactive log output leaks the outgoing pw in cleartext:\n%s", logged)
	}
}
