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

func TestLogCommandResultClassification(t *testing.T) {
	// logCommandResult only logs; this smoke-tests all three classification branches
	// (success, benign errorCode, real-failure errorCode) run without panicking.
	logCommandResult("test success", newTestExtMsg("test.cmd", nil))
	logCommandResult("test benign", newTestExtMsg("test.cmd", "602026"))
	logCommandResult("test real failure", newTestExtMsg("test.cmd", "999999"))
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
