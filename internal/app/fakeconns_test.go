package app

import (
	"io"
	"net"
	"sync"
	"time"
)

type fakeNetErrConn struct {
	mu      sync.Mutex
	writes  int
	timeout bool // Timeout() of the fakeNetError every Read fails with; see the doc comment above.
}

func (c *fakeNetErrConn) Read([]byte) (int, error) { return 0, fakeNetError{timeout: c.timeout} }

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
// Temporary()), simulating either of the two kinds of net.Error CollectAll's (and FetchBuildings',
// ClaimAllMail's, GreetVisitors', ClaimAllianceGifts') early-abort checks care about -- the
// distinction is entirely carried by the timeout field:
//
//   - timeout: false (the zero value -- what a bare fakeNetError{} literal gets, including every
//     direct `fakeNetError{}` use elsewhere in this package's other _test.go files) is a genuine
//     connection-level failure: connection reset, broken pipe, DNS failure, TLS error, etc. This is
//     the ONLY kind of net.Error that should still trigger an early abort of remaining independent
//     actions -- every subsequent action really is doomed to fail the same way.
//   - timeout: true is sendAndWait's ordinary "no matching response within defaultCmdTimeout (8s)"
//     outcome (confirmed by TestWaitForTimeout in conn_wait_test.go) -- a normal, expected timeout on
//     one action's response on an otherwise-healthy connection. It must NOT abort remaining actions.
type fakeNetError struct {
	timeout bool
}

func (e fakeNetError) Error() string {
	if e.timeout {
		return "fake net.Error: simulated per-action response timeout"
	}
	return "fake net.Error: simulated dead connection"
}
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return false }

type realEOFConn struct {
	mu     sync.Mutex
	writes int
}

func (c *realEOFConn) Read([]byte) (int, error) { return 0, io.EOF }

func (c *realEOFConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(b), nil
}

func (c *realEOFConn) Close() error                     { return nil }
func (c *realEOFConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *realEOFConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *realEOFConn) SetDeadline(time.Time) error      { return nil }
func (c *realEOFConn) SetReadDeadline(time.Time) error  { return nil }
func (c *realEOFConn) SetWriteDeadline(time.Time) error { return nil }

type writeFailConn struct {
	net.Conn
	err error
}

func (w *writeFailConn) Write([]byte) (int, error) { return 0, w.err }

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
