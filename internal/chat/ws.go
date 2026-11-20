package chat

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal RFC 6455 client, enough for the chat server's JSON text frames. The real client uses
// BestHTTP's WebSocket (WebSocketItem.Connect, Assembly-CSharp.decompiled.cs:70372-70403): the URL
// is "{protocol}://{ip}:{port}" with no path, five custom handshake headers, a ping every 3000 ms
// and a close after 9 s without a frame. go.mod has no WebSocket library, so this is hand-written:
// no extensions are offered (whether BestHTTP offers permessage-deflate is not visible in the
// dump; the BestHTTP assembly is not in it), so the server must not compress frames.

const (
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11" // RFC 6455 section 1.3

	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA

	maxMessageBytes = 8 << 20 // a history page is far smaller; this bounds a hostile frame
	maxControlBytes = 125     // RFC 6455 section 5.5
	wsWriteTimeout  = 5 * time.Second
)

// DialFunc opens the TCP connection; tests pass an in-memory one.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// header is one handshake header in the client's spelling: net/http would canonicalize APPID to
// Appid, so the request is written by hand.
type header struct{ name, value string }

// closeError is a close frame from the server.
type closeError struct{ code int }

func (e *closeError) Error() string {
	return fmt.Sprintf("websocket closed by the server (code %d)", e.code)
}

// wsConn is one client connection. readMessage is called from a single goroutine; writes are
// serialized, so the ping goroutine can write concurrently.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader

	wmu    sync.Mutex
	closed bool
}

// dialWS opens rawURL (ws or wss) and performs the opening handshake with hdrs, all within ctx.
func dialWS(ctx context.Context, dial DialFunc, tlsConf *tls.Config, rawURL string, hdrs []header) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket url: %v", err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return nil, fmt.Errorf("websocket url: scheme %q is neither ws nor wss", u.Scheme)
	}
	if u.Port() == "" || u.Hostname() == "" {
		return nil, errors.New("websocket url: no host or port")
	}
	for _, h := range hdrs {
		if strings.ContainsAny(h.name+h.value, "\r\n") || h.name == "" {
			return nil, fmt.Errorf("websocket header %q: empty or holds a line break", h.name)
		}
	}
	c, err := dial(ctx, "tcp", u.Host)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = c.Close()
		}
	}()
	if dl, has := ctx.Deadline(); has {
		_ = c.SetDeadline(dl)
	}
	if u.Scheme == "wss" {
		cfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if tlsConf != nil {
			cfg = tlsConf.Clone()
		}
		if cfg.ServerName == "" {
			cfg.ServerName = u.Hostname()
		}
		tc := tls.Client(c, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("tls handshake: %v", err)
		}
		c = tc
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(nonce[:])
	var b strings.Builder
	fmt.Fprintf(&b, "GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n", u.RequestURI(), u.Host, key)
	for _, h := range hdrs {
		fmt.Fprintf(&b, "%s: %s\r\n", h.name, h.value)
	}
	b.WriteString("\r\n")
	if _, err := io.WriteString(c, b.String()); err != nil {
		return nil, fmt.Errorf("websocket handshake write: %v", err)
	}
	br := bufio.NewReaderSize(c, 64<<10)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return nil, fmt.Errorf("websocket handshake read: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("websocket handshake: HTTP %d", resp.StatusCode)
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || !headerHasToken(resp.Header, "Connection", "upgrade") {
		return nil, errors.New("websocket handshake: no Upgrade: websocket / Connection: Upgrade in the reply")
	}
	if got, want := resp.Header.Get("Sec-WebSocket-Accept"), acceptKey(key); got != want {
		return nil, errors.New("websocket handshake: wrong Sec-WebSocket-Accept")
	}
	if ext := resp.Header.Get("Sec-WebSocket-Extensions"); ext != "" {
		return nil, fmt.Errorf("websocket handshake: server chose extension %q that was never offered", ext)
	}
	_ = c.SetDeadline(time.Time{})
	ok = true
	return &wsConn{conn: c, br: br}, nil
}

// acceptKey is Sec-WebSocket-Accept for a Sec-WebSocket-Key (RFC 6455 section 4.2.2).
func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// readFrame reads one frame. Server frames are never masked (RFC 6455 section 5.1).
func (w *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	if h[0]&0x70 != 0 {
		return false, 0, nil, errors.New("websocket frame: reserved bits set, but no extension was negotiated")
	}
	if h[1]&0x80 != 0 {
		return false, 0, nil, errors.New("websocket frame: masked frame from the server")
	}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var l [2]byte
		if _, err = io.ReadFull(w.br, l[:]); err != nil {
			return false, 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(l[:]))
	case 127:
		var l [8]byte
		if _, err = io.ReadFull(w.br, l[:]); err != nil {
			return false, 0, nil, err
		}
		n = binary.BigEndian.Uint64(l[:])
	}
	if op >= opClose && (!fin || n > maxControlBytes) {
		return false, 0, nil, errors.New("websocket frame: fragmented or oversized control frame")
	}
	if n > maxMessageBytes {
		return false, 0, nil, fmt.Errorf("websocket frame: %d bytes is over the %d byte limit", n, maxMessageBytes)
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// readMessage returns the next text message, reassembling fragments, answering pings and
// skipping pongs and binary messages (the client ignores binary, Socket_OnBinary,
// Assembly-CSharp.decompiled.cs:70473-70475). A close frame is answered and returned as
// *closeError.
func (w *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	inMsg, isText := false, false
	for {
		fin, op, p, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := w.writeFrame(opPong, p); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code := 1005
			if len(p) >= 2 {
				code = int(binary.BigEndian.Uint16(p[:2]))
			}
			_ = w.writeFrame(opClose, p[:min(len(p), 2)])
			return nil, &closeError{code: code}
		case opText, opBinary:
			if inMsg {
				return nil, errors.New("websocket frame: new message inside a fragmented one")
			}
			inMsg, isText, msg = true, op == opText, p
		case opContinuation:
			if !inMsg {
				return nil, errors.New("websocket frame: continuation without a message")
			}
			if len(msg)+len(p) > maxMessageBytes {
				return nil, fmt.Errorf("websocket message over the %d byte limit", maxMessageBytes)
			}
			msg = append(msg, p...)
		default:
			return nil, fmt.Errorf("websocket frame: unknown opcode %d", op)
		}
		if !fin {
			continue
		}
		if isText {
			return msg, nil
		}
		msg, inMsg = nil, false
	}
}

// writeFrame writes one masked frame (client frames are always masked, RFC 6455 section 5.3).
func (w *wsConn) writeFrame(op byte, payload []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	if w.closed {
		return net.ErrClosed
	}
	if op == opClose {
		w.closed = true
	}
	n := len(payload)
	hdr := make([]byte, 2, 14+n)
	hdr[0] = 0x80 | op
	switch {
	case n < 126:
		hdr[1] = 0x80 | byte(n)
	case n <= 0xFFFF:
		hdr[1] = 0x80 | 126
		hdr = binary.BigEndian.AppendUint16(hdr, uint16(n))
	default:
		hdr[1] = 0x80 | 127
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	buf := append(hdr, mask[:]...)
	for i, c := range payload {
		buf = append(buf, c^mask[i%4])
	}
	_ = w.conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout))
	_, err := w.conn.Write(buf)
	return err
}

// writeText sends one text message.
func (w *wsConn) writeText(msg []byte) error { return w.writeFrame(opText, msg) }

// startPings pings every interval until the returned stop is called or a write fails; BestHTTP
// pings every 3000 ms (PingFrequency, Assembly-CSharp.decompiled.cs:70382).
func (w *wsConn) startPings(every time.Duration) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if w.writeFrame(opPing, nil) != nil {
					return
				}
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

// close sends a normal close frame (1000) when none was sent yet, then closes the connection.
func (w *wsConn) close() error {
	_ = w.writeFrame(opClose, []byte{0x03, 0xE8})
	return w.conn.Close()
}
