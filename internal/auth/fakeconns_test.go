package auth

import (
	"errors"
	"net"
	"sync"
)

type countingCloseConn struct {
	net.Conn
	mu    sync.Mutex
	calls int
}

func (c *countingCloseConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls > 1 {
		return errors.New("use of closed network connection")
	}
	return nil
}

func (c *countingCloseConn) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

type writeFailConn struct {
	net.Conn
	err error
}

func (w *writeFailConn) Write([]byte) (int, error) { return 0, w.err }

// TestReadPacketGracefulCloseIsNonTimeoutNetError is the packet.go-level regression test for round
// 24's MAJOR finding: a peer's graceful close (a clean FIN, or the far end process simply exiting,
// with nothing sent) surfaces from sfs.ReadPacket's leading io.ReadFull(r, hb[:]) header read as bare
// io.EOF, which does not itself implement net.Error -- and fmt.Errorf's %w wrapping doesn't change
// that, since errors.As only succeeds if SOME error in the chain implements the target interface.
// Left unfixed, every one of the 5 "abort remaining independent work on a genuine dead connection"
// checks built across rounds 16-23 (buildings.go's CollectAll, mail.go's ClaimAllMail, visitors.go's
// GreetVisitors, alliance.go's ClaimAllianceGifts, interactive.go's handleInteractiveLine, all via
// containsNonTimeoutNetError or a direct net.Error check) silently never fires for this, the single
// most realistic real-world failure mode -- empirically reproduced during the audit: wiring an
// equivalent fake conn into CollectAll produced 9 separate wasted requests, each burning a full
// defaultCmdTimeout, instead of aborting after the first.
//
// bytes.NewReader(nil) is itself exactly the shape under test: an io.Reader whose very first Read
// call returns bare io.EOF and nothing else -- standing in for a socket a peer closed before
// sending anything at all (the between-packets case; see
// TestReadPacketMidFrameCloseIsNonTimeoutNetError below for the mid-frame variant).
