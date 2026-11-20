package chat

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests never touch the network: discovery goes through a RoundTripper and the WebSocket
// through net.Pipe.

const (
	testUID      = "812345678901"
	testNowMs    = 1763500000123
	testRoom     = "alliance_321_abc"
	testLinksOK  = `{"code":1,"data":[{"protocol":"ws","ip":"10.0.0.1","port":8080}]}`
	testChatTokn = "chat-token-value"
)

func testIdentity() Identity {
	return Identity{UID: testUID, SelfServerID: 321, SourceServerID: 321, AllianceID: "abc", ChatToken: testChatTokn}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}},
		Body: io.NopCloser(strings.NewReader(body))}
}

// linksOK answers every discovery request with one ws line.
func linksOK(*http.Request) (*http.Response, error) { return jsonResponse(200, testLinksOK), nil }

func testOptions(rt roundTripFunc, dial DialFunc) Options {
	o := DefaultOptions()
	o.HTTP = &http.Client{Transport: rt}
	o.Dial = dial
	o.Now = func() time.Time { return time.UnixMilli(testNowMs) }
	o.Sleep = func(time.Duration) {}
	o.Timeout = 5 * time.Second
	o.StepWait = 2 * time.Second
	o.Listen = 150 * time.Millisecond
	o.PingEvery = time.Hour
	return o
}

// fakeServer is the server end of one chat WebSocket.
type fakeServer struct {
	c  net.Conn
	br *bufio.Reader

	mu      sync.Mutex
	headers map[string]string // handshake headers as spelled on the wire
	reqLine string
	frames  []map[string]json.RawMessage
	raw     [][]byte
	pongs   int
	closed  bool
}

// fakeDial returns a DialFunc whose connection is served by script after the handshake; the
// fakeServer records what the client sent. The returned channel yields the server once dialled.
func fakeDial(t *testing.T, script func(s *fakeServer)) (DialFunc, <-chan *fakeServer) {
	t.Helper()
	ch := make(chan *fakeServer, 1)
	dial := func(ctx context.Context, network, addr string) (net.Conn, error) {
		client, server := net.Pipe()
		s := &fakeServer{c: server, br: bufio.NewReader(server), headers: map[string]string{}}
		go func() {
			defer func() { _ = server.Close() }()
			if err := s.handshake(); err != nil {
				t.Errorf("fake handshake: %v", err)
				return
			}
			ch <- s
			script(s)
			s.drain()
		}()
		return client, nil
	}
	return dial, ch
}

func (s *fakeServer) handshake() error {
	line, err := s.br.ReadString('\n')
	if err != nil {
		return err
	}
	s.reqLine = strings.TrimSpace(line)
	for {
		l, err := s.br.ReadString('\n')
		if err != nil {
			return err
		}
		l = strings.TrimRight(l, "\r\n")
		if l == "" {
			break
		}
		k, v, _ := strings.Cut(l, ": ")
		s.headers[k] = v
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + acceptKey(s.headers["Sec-WebSocket-Key"]) + "\r\n\r\n"
	_, err = io.WriteString(s.c, resp)
	return err
}

// writeFrame writes one unmasked server frame.
func (s *fakeServer) writeFrame(op byte, fin bool, p []byte) {
	b := []byte{op, 0}
	if fin {
		b[0] |= 0x80
	}
	switch n := len(p); {
	case n < 126:
		b[1] = byte(n)
	case n <= 0xFFFF:
		b[1] = 126
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b[1] = 127
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	_, _ = s.c.Write(append(b, p...))
}

func (s *fakeServer) sendJSON(v string) { s.writeFrame(opText, true, []byte(v)) }

// readFrame reads one client frame, which must be masked.
func (s *fakeServer) readFrame() (op byte, p []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(s.br, h[:]); err != nil {
		return 0, nil, err
	}
	if h[1]&0x80 == 0 {
		return 0, nil, errors.New("client frame not masked")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var l [2]byte
		_, _ = io.ReadFull(s.br, l[:])
		n = uint64(binary.BigEndian.Uint16(l[:]))
	case 127:
		var l [8]byte
		_, _ = io.ReadFull(s.br, l[:])
		n = binary.BigEndian.Uint64(l[:])
	}
	var mask [4]byte
	if _, err = io.ReadFull(s.br, mask[:]); err != nil {
		return 0, nil, err
	}
	p = make([]byte, n)
	if _, err = io.ReadFull(s.br, p); err != nil {
		return 0, nil, err
	}
	for i := range p {
		p[i] ^= mask[i%4]
	}
	return h[0] & 0x0F, p, nil
}

// recv returns the next client text frame, decoded and recorded; nil on close or error.
func (s *fakeServer) recv() map[string]json.RawMessage {
	for {
		op, p, err := s.readFrame()
		if err != nil {
			return nil
		}
		switch op {
		case opPong:
			s.mu.Lock()
			s.pongs++
			s.mu.Unlock()
			continue
		case opPing:
			continue
		case opClose:
			s.mu.Lock()
			s.closed = true
			s.mu.Unlock()
			return nil
		}
		var m map[string]json.RawMessage
		_ = json.Unmarshal(p, &m)
		s.mu.Lock()
		s.frames = append(s.frames, m)
		s.raw = append(s.raw, p)
		s.mu.Unlock()
		return m
	}
}

// drain reads until the client closes.
func (s *fakeServer) drain() {
	for s.recv() != nil {
	}
}

func (s *fakeServer) cmds() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, f := range s.frames {
		var c string
		_ = json.Unmarshal(f["cmd"], &c)
		out = append(out, c)
	}
	return out
}

func cmdOf(f map[string]json.RawMessage) string {
	var c string
	_ = json.Unmarshal(f["cmd"], &c)
	return c
}
