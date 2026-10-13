package session

import (
	"errors"
	"net"
	"os"
	"runtime"
	"testing"
	"time"
)

// TestServeFakeGameServerKeepsConnOpenAfterHandlerReturns pins ServeFakeGameServer's conns slice: a
// handler that returns while the client is still connected (FakeInitPushServer's shape) must not
// leave its conn for a GC to finalize, which closes the server socket under the client. The client
// must see its own read deadline expire, not EOF.
func TestServeFakeGameServerKeepsConnOpenAfterHandlerReturns(t *testing.T) {
	returned := make(chan struct{})
	addr := StartFakeGameServer(t, func(*GameConn) { close(returned) })

	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	<-returned
	for range 5 {
		runtime.GC()
		time.Sleep(10 * time.Millisecond) // let the finalizer goroutine run what GC queued
	}

	if err := c.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("client read err = %v, want a deadline timeout (the server socket closed under a live client)", err)
	}
}
