package chat

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// wsPair is a client wsConn and the raw server end of a pipe, past the handshake.
func wsPair(t *testing.T) (*wsConn, *fakeServer) {
	t.Helper()
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return &wsConn{conn: client, br: bufio.NewReader(client)}, &fakeServer{c: server, br: bufio.NewReader(server)}
}

func TestReadMessageReassemblesFragmentsAndAnswersPings(t *testing.T) {
	w, s := wsPair(t)
	go func() {
		s.writeFrame(opText, false, []byte(`{"cmd":`))
		s.writeFrame(opPing, true, []byte("hi"))
		s.writeFrame(opContinuation, false, []byte(`"x",`))
		s.writeFrame(opPong, true, nil)
		s.writeFrame(opContinuation, true, []byte(`"a":1}`))
		s.writeFrame(opBinary, true, []byte{1, 2, 3}) // ignored, as Socket_OnBinary does
		s.writeFrame(opText, true, []byte(strings.Repeat("y", 70000)))
	}()
	pong := make(chan []byte, 1)
	go func() {
		op, p, err := s.readFrame()
		if err == nil && op == opPong {
			pong <- p
		}
	}()
	msg, err := w.readMessage()
	if err != nil || string(msg) != `{"cmd":"x","a":1}` {
		t.Fatalf("got %q, %v", msg, err)
	}
	select {
	case p := <-pong:
		if string(p) != "hi" {
			t.Errorf("pong payload %q", p)
		}
	case <-time.After(time.Second):
		t.Error("ping not answered")
	}
	msg, err = w.readMessage()
	if err != nil || len(msg) != 70000 {
		t.Errorf("64-bit length frame: %d bytes, %v", len(msg), err)
	}
}

func TestReadMessageRejectsBadFrames(t *testing.T) {
	cases := map[string][]byte{
		"masked server frame":  {0x81, 0x81, 1, 2, 3, 4, 'x'},
		"reserved bits":        {0xC1, 0x01, 'x'},
		"fragmented control":   {0x09, 0x00},
		"oversized control":    append([]byte{0x89, 126, 0, 200}, make([]byte, 200)...),
		"stray continuation":   {0x80, 0x01, 'x'},
		"unknown opcode":       {0x83, 0x00},
		"over the size limit":  binary.BigEndian.AppendUint64([]byte{0x81, 127}, maxMessageBytes+1),
		"message inside other": {0x01, 0x01, 'a', 0x81, 0x01, 'b'},
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			w, s := wsPair(t)
			go func() { _, _ = s.c.Write(raw) }()
			_ = w.conn.SetReadDeadline(time.Now().Add(time.Second))
			if msg, err := w.readMessage(); err == nil {
				t.Errorf("accepted %q", msg)
			}
		})
	}
}

func TestCloseFrameIsAnsweredAndReported(t *testing.T) {
	w, s := wsPair(t)
	go s.writeFrame(opClose, true, []byte{0x03, 0xE9, 'b', 'y', 'e'})
	got := make(chan []byte, 1)
	go func() {
		op, p, _ := s.readFrame()
		if op == opClose {
			got <- p
		}
	}()
	_, err := w.readMessage()
	var ce *closeError
	if !errors.As(err, &ce) || ce.code != 1001 {
		t.Fatalf("err %v", err)
	}
	if p := <-got; len(p) != 2 || binary.BigEndian.Uint16(p) != 1001 {
		t.Errorf("close echo %v", p)
	}
	if err := w.writeText([]byte("x")); err == nil {
		t.Error("wrote after close")
	}
}

func TestWriteFrameMasksAndUsesExtendedLengths(t *testing.T) {
	for _, n := range []int{0, 125, 126, 65535, 65536} {
		w, s := wsPair(t)
		payload := []byte(strings.Repeat("z", n))
		go func() { _ = w.writeText(payload) }()
		op, p, err := s.readFrame()
		if err != nil || op != opText || string(p) != string(payload) {
			t.Errorf("n=%d: op %d, %d bytes, %v", n, op, len(p), err)
		}
	}
}

func TestDialWSRejectsABadHandshake(t *testing.T) {
	reply := func(resp string) DialFunc {
		return func(context.Context, string, string) (net.Conn, error) {
			c, s := net.Pipe()
			go func() {
				br := bufio.NewReader(s)
				for {
					l, err := br.ReadString('\n')
					if err != nil || l == "\r\n" {
						break
					}
				}
				_, _ = io.WriteString(s, resp)
			}()
			return c, nil
		}
	}
	for name, resp := range map[string]string{
		"not 101":        "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n",
		"wrong accept":   "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: AAAA\r\n\r\n",
		"no upgrade":     "HTTP/1.1 101 Switching Protocols\r\nConnection: keep-alive\r\n\r\n",
		"unknown scheme": "",
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			u := "ws://h:1"
			if name == "unknown scheme" {
				u = "http://h:1"
			}
			if _, err := dialWS(ctx, reply(resp), nil, u, nil); err == nil {
				t.Error("handshake accepted")
			}
		})
	}
	ctx := context.Background()
	if _, err := dialWS(ctx, reply(""), nil, "ws://h:1", []header{{"UID", "a\r\nX: y"}}); err == nil {
		t.Error("a header with a line break was written")
	}
}
