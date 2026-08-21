// Package testutil holds test helpers shared across the internal packages
// (packet encoders, an RSA pubkey generator, a fake net.Error, and small conversions).
package testutil

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"io"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// CaptureStdout redirects os.Stdout to a pipe for the duration of fn and
// returns everything written to it. DecodeStreamFile writes directly via
// fmt.Printf rather than accepting an io.Writer, so this is the only way to
// observe its per-packet output (as opposed to just its returned error).
//
// The drain runs in a goroutine concurrently with fn, not after it: an
// os.Pipe has a small fixed kernel buffer, and DecodeStreamFile can print
// more than that fits, so a synchronous "run fn, then read" would deadlock
// with fn blocked on a full pipe and nothing reading it yet.
func CaptureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	outCh := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		outCh <- buf.String()
	}()

	fn()

	os.Stdout = orig
	if err := w.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}
	out := <-outCh
	_ = r.Close()
	return out
}

// MustEncodeCorruptPacket builds a packet whose framing is entirely valid
// (correct length prefix, correctly round-trips through sfs.ReadPacket) but
// whose sfs.SFSObject body is not: the leading tag byte is overwritten so
// DecodeObject's "expected top-level tag 18" check fails. This is the
// packet-framing-succeeded-but-content-is-garbage case, as distinct from a
// truncated/corrupt frame (which sfs.ReadPacket itself rejects).
func MustEncodeCorruptPacket(t *testing.T, field, value string) []byte {
	t.Helper()
	o := sfs.NewSFSObject()
	o.PutUtfString(field, value)
	body, err := sfs.EncodeObject(o)
	if err != nil {
		t.Fatalf("sfs.EncodeObject: %v", err)
	}
	body[0] = 0xEE // not sfs.SFSObjectType (18) -> DecodeObject must error
	packet, err := sfs.EncodePacket(body)
	if err != nil {
		t.Fatalf("sfs.EncodePacket: %v", err)
	}
	return packet
}

// RSAPubKeyDER returns a fresh 2048-bit RSA public key, DER+base64 encoded, for the game
// package's fake-GSL-server tests. Duplicated from internal/gsl's own copy: test helpers can't be
// shared across package boundaries.
func RSAPubKeyDER(t *testing.T) string {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate RSA key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

// FlexPort converts a plain int port into gsl.go's gsl.FlexString shape, for building test
// gsl.LoginServerInfo/gsl.AccountServerInfo fixtures now that Port/WsPort are gsl.FlexString (round 35 --
// see gsl.FlexString.Int's own doc comment in gsl.go for why).
func FlexPort(n int) gsl.FlexString { return gsl.FlexString(strconv.Itoa(n)) }

// FakeTimeoutNetError is a minimal net.Error whose Timeout() reports true, standing in for the
// *net.OpError a real deadline-exceeded net.Conn.Write returns -- per net.Conn's documented
// contract, "a Write ... that exceeds the [write] deadline ... returns an error that wraps
// os.ErrDeadlineExceeded" and satisfies net.Error with Timeout() == true. Used by
// TestSendAndWaitWriteStageFailureIsNonTimeoutNetError below to prove sendAndWait's write-stage
// wrapper (sendStageError, conn.go) forces Timeout()==false even when the underlying cause is
// itself exactly this Timeout()==true shape.
type FakeTimeoutNetError struct{ Msg string }

func (e FakeTimeoutNetError) Error() string { return e.Msg }
func (FakeTimeoutNetError) Timeout() bool   { return true }
func (FakeTimeoutNetError) Temporary() bool { return true }

// SplitHostPortInt parses a "host:port" address (as returned by net.Listener.Addr().String())
// into DoCrossServerLogin's IP/Port shape.
func SplitHostPortInt(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split host/port %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port %q: %v", portStr, err)
	}
	return host, port
}

// NewFakeGSLServer stands up one httptest.Server answering both gsl.CheckVersion's
// getlsu3dversion.php (always the same canned response, carrying a throwaway RSA pubkey) and
// gsl.GetServerList's getserverlist.php (like TestGetServerListAgainstFakeServer, a plaintext --
// unencrypted -- gsl.LoginServerListRespon body, which gsl.GetServerList already falls back to when no
// "bin" field is present). gslResponses is consumed one entry per gsl.GetServerList POST, in order;
// once exhausted, the last entry repeats, so a test only needs to supply as many entries as it
// cares to distinguish (one per distinct gsl.GetServerList call it expects, e.g. the initial
// opt=new/fix/login call and, separately, any mid-redirect opt=fix refresh call).
func NewFakeGSLServer(t *testing.T, gslResponses ...gsl.LoginServerListRespon) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(gslHandler(t, gslResponses))
	t.Cleanup(server.Close)
	return server
}

// gslHandler is the shared request handler behind both NewFakeGSLServer (over a real httptest.Server)
// and UseInMemoryGSL (over an in-memory RoundTripper): it answers gsl.CheckVersion's
// getlsu3dversion.php with a canned throwaway RSA pubkey and gsl.GetServerList's getserverlist.php
// with the next gslResponses entry (repeating the last once exhausted -- see NewFakeGSLServer's doc
// comment for the per-call semantics).
func gslHandler(t *testing.T, gslResponses []gsl.LoginServerListRespon) http.HandlerFunc {
	t.Helper()
	pub := RSAPubKeyDER(t)
	var mu sync.Mutex
	call := 0
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			_ = json.NewEncoder(w).Encode(gsl.CheckVersionResponse{ResMsg: gsl.FlexString(pub)})
		case strings.HasSuffix(r.URL.Path, "getserverlist.php"):
			mu.Lock()
			idx := call
			if idx >= len(gslResponses) {
				idx = len(gslResponses) - 1
			}
			call++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(gslResponses[idx])
		default:
			http.NotFound(w, r)
		}
	}
}

// UseFakeGSLServer points gsl.CheckVersionHosts at server for the duration of the test, restoring the
// real list on cleanup -- same override pattern as gsl_http_test.go's gsl.CheckVersion tests.
func UseFakeGSLServer(t *testing.T, server *httptest.Server) {
	t.Helper()
	orig := gsl.CheckVersionHosts
	gsl.CheckVersionHosts = []string{server.URL}
	t.Cleanup(func() { gsl.CheckVersionHosts = orig })
}

// recorderRoundTripper serves each request synchronously by replaying it through an http.Handler and
// recording the response, with NO real socket and NO background goroutines -- unlike httptest.Server,
// which is a real loopback listener whose http.Transport client pool leaves keep-alive reader
// goroutines parked in the netpoller. That difference is what makes this usable inside a
// testing/synctest bubble: a plain RoundTripper call is an ordinary function call the bubble can run
// deterministically, whereas real HTTP would block in the netpoller (never durably, so time can't
// advance) and strand Transport goroutines that trip synctest's end-of-bubble "still running" check.
type recorderRoundTripper struct{ h http.Handler }

func (rt recorderRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// UseInMemoryGSL is the synctest-friendly sibling of NewFakeGSLServer+UseFakeGSLServer: it points
// gsl.CheckVersionHosts at a dummy URL (the RoundTripper ignores the host, dispatching purely on the
// request path) and returns an *http.Client whose transport answers CheckVersion/GetServerList
// entirely in memory -- same canned responses, no real network, no goroutines, no client Timeout
// (so no timer goroutine either). Pass the returned client as crossServerTestOpts.httpClient so a
// runCrossServerTest test's GSL round-trips run inside a synctest bubble under virtual time.
func UseInMemoryGSL(t *testing.T, gslResponses ...gsl.LoginServerListRespon) *http.Client {
	t.Helper()
	orig := gsl.CheckVersionHosts
	gsl.CheckVersionHosts = []string{"http://gsl.invalid"}
	t.Cleanup(func() { gsl.CheckVersionHosts = orig })
	return &http.Client{Transport: recorderRoundTripper{h: gslHandler(t, gslResponses)}}
}
