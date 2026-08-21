package session

import (
	"bufio"
	"net"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// NewPipeGameConnPair returns two GameConns wired to each other over an in-memory net.Pipe, with a
// t.Cleanup that closes both ends. Shared by tests across the internal packages that need a live,
// synchronous client/server GameConn pair without a real socket.
func NewPipeGameConnPair(t *testing.T) (client, server *GameConn) {
	t.Helper()
	c1, c2 := net.Pipe()
	t.Cleanup(func() {
		_ = c1.Close()
		_ = c2.Close()
	})
	return &GameConn{conn: c1, reader: bufio.NewReaderSize(c1, 4096)}, &GameConn{conn: c2, reader: bufio.NewReaderSize(c2, 4096)}
}

// readAndReply is the fake-server half used by the sendAndWait tests below: read one request off
// server, then reply to it with the given cmd/params. Run in a goroutine since net.Pipe is
// unbuffered/synchronous -- the client's send and this read rendezvous directly. Takes no
// *testing.T: it runs in a background goroutine that may still be alive after the test function
// returns (unblocked by testutil.NewPipeGameConnPair's t.Cleanup), and calling T methods from a goroutine
// after the test has completed is unsafe.
func ReadAndReply(server *GameConn, replyCmd string, replyParams *sfs.SFSObject) {
	env, err := server.ReadEnvelope()
	if err != nil {
		return
	}
	msg, ok := env.AsExtension()
	if !ok {
		return
	}
	cmd := replyCmd
	if cmd == "" {
		cmd = msg.Cmd
	}
	_ = server.SendExtension(cmd, replyParams)
}

// NewFakeGameListener opens a TCP listener on 127.0.0.1 for a fake game server and registers
// t.Cleanup to close it. Split out from ServeFakeGameServer (rather than one combined
// "listen and serve" call) so a test that needs to know an address before it can build the
// handler for a DIFFERENT listener -- e.g. a serverInfo redirect chain, where each server's
// response embeds the NEXT server's address -- can open every listener (addresses are known the
// instant Listen returns, no Accept required) before wiring up any handlers.
func NewFakeGameListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, ln.Addr().String()
}

// ServeFakeGameServer runs handler, each in its own goroutine, once per connection accepted on
// ln, until ln.Close() (via NewFakeGameListener's t.Cleanup) breaks the Accept loop at test end.
// Takes no *testing.T deliberately: like conn_wait_test.go's ReadAndReply, handler may still be
// running in the background after the test function itself has returned, and calling T methods
// from such a goroutine is unsafe.
func ServeFakeGameServer(ln net.Listener, handler func(*GameConn)) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			gc := NewGameConnForTest(c)
			go handler(gc)
		}
	}()
}

// StartFakeGameServer covers the common single-listener case: listen and serve immediately.
func StartFakeGameServer(t *testing.T, handler func(*GameConn)) string {
	t.Helper()
	ln, addr := NewFakeGameListener(t)
	ServeFakeGameServer(ln, handler)
	return addr
}

// NewInMemoryGameDial returns a dial function with DialGame's signature, suitable for
// CrossServerLoginParams.DialGame / crossServerTestOpts.dialGame, that opens the game socket over an
// in-memory net.Pipe instead of a real TCP connection. On each call (the serverInfo-redirect loop
// redials, so this can run more than once per login) it wires a fresh pipe, runs handler on the
// server end, and returns the client end wrapped as a GameConn -- ignoring addr/timeout entirely.
//
// After handler returns, the server goroutine DRAINS the pipe (reading and discarding envelopes)
// until the client closes its end. This is essential under net.Pipe, which is synchronous: the
// client's StartHeartbeat goroutine writes a pingpong every few seconds, and with nothing reading
// them those writes would block forever -- and inside a synctest bubble, every goroutine being
// blocked with time unable to advance is a deadlock panic. Draining absorbs the heartbeats so the
// client side proceeds normally; the drain (and handler) unblock and exit when the caller's
// deferred conn.Close() closes the client end. Intended to be called from inside a synctest bubble,
// where net.Pipe's channel-based reads/writes and time.Timer-based deadlines are all virtualized.
func NewInMemoryGameDial(handler func(*GameConn)) func(addr string, timeout time.Duration) (*GameConn, error) {
	return func(string, time.Duration) (*GameConn, error) {
		c1, c2 := net.Pipe()
		go func() {
			srv := NewGameConnForTest(c2)
			defer func() { _ = srv.Close() }()
			handler(srv)
			for {
				if _, err := srv.ReadEnvelope(); err != nil {
					return
				}
			}
		}()
		return NewGameConnForTest(c1), nil
	}
}

// FakeInitPushServer replies to a base zone Login (whatever content it receives) with a plain
// success response, then immediately follows up with the bare `init` bootstrap push
// waitForInitPush is waiting for -- so Login()'s step 5 completes almost instantly instead of
// waiting out its real 45s timeout (that timeout is a local const inside Login(), not overridable
// from a test, so the fake server sending `init` promptly is the only way to keep this test fast).
// zoneSeen, if non-nil, receives the zone (`zn`) the client actually logged in with, so a redirect
// test can confirm the redialed Login used the new zone, not the original one.
func FakeInitPushServer(zoneSeen chan<- string) func(*GameConn) {
	return func(server *GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if zoneSeen != nil && env.Content != nil {
			zoneSeen <- env.Content.GetString("zn")
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(ControllerSystem, ActionLogin, resp); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	}
}

// ReadNextExtension reads envelopes off server until one decodes as an extension message,
// silently skipping anything else -- in practice the client's own heartbeat pingpong (system
// controller, sent every 4s per conn.go's StartHeartbeat), which TestLoginEmailVerificationPath's
// fake server would otherwise misread as the next expected extension request if the client's real
// FIFO round-trip for the verification code ever took long enough for a heartbeat to land in
// between. Mirrors waitFor's own "skip anything that doesn't match" loop on the client side.
func ReadNextExtension(server *GameConn) (*ExtensionMessage, error) {
	for {
		env, err := server.ReadEnvelope()
		if err != nil {
			return nil, err
		}
		if msg, ok := env.AsExtension(); ok {
			return msg, nil
		}
	}
}
