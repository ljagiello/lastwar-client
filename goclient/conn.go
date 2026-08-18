package main

import (
	"bufio"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// SFS2X system-controller action ids actually used by this game (dossier §04.1).
// actionHandshake=0 is confirmed present in the game's own Smartfox2xLw.decompiled.cs
// (SendHandshakeRequest/HandshakeRequest) but was never exercised by this Go
// client until the sfs2x-api@1.8.6 comparison pass -- see DoHandshake.
const (
	actionHandshake     = 0
	actionLogin         = 1
	actionCallExtension = 13
	actionPingPong      = 29
)

const (
	controllerSystem    = 0
	controllerExtension = 1
)

// Envelope is the decoded outer {c,a,p} wrapper for both directions.
type Envelope struct {
	Controller byte
	Action     int16
	Content    *SFSObject
}

// GameConn is a raw (plaintext, connectionType 0) SFS2X socket connection.
type GameConn struct {
	conn   net.Conn
	reader *bufio.Reader
	wmu    sync.Mutex

	stopHeartbeat chan struct{}
}

func DialGame(addr string, timeout time.Duration) (*GameConn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetNoDelay(true)
	}
	return &GameConn{
		conn:   conn,
		reader: bufio.NewReaderSize(conn, 64*1024),
	}, nil
}

func (c *GameConn) Close() error {
	if c.stopHeartbeat != nil {
		close(c.stopHeartbeat)
		c.stopHeartbeat = nil
	}
	return c.conn.Close()
}

// SendEnvelope builds the outer {c,a,p} SFSObject, serializes, frames, and
// writes it to the socket. Safe for concurrent use (heartbeat + main flow).
func (c *GameConn) SendEnvelope(controller byte, action int16, content *SFSObject) error {
	outer := NewSFSObject()
	outer.PutByte("c", controller)
	outer.PutShort("a", action)
	outer.PutSFSObject("p", content)

	body := EncodeObject(outer)
	packet, err := EncodePacket(body)
	if err != nil {
		return fmt.Errorf("encode packet: %w", err)
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.conn.Write(packet)
	return err
}

// SendExtension sends a client->server `cmd` extension request, matching
// SFSNetwork.SendMessage(cmd, ...) -- dossier §04/§06.
func (c *GameConn) SendExtension(cmd string, params *SFSObject) error {
	if params == nil {
		params = NewSFSObject()
	}
	extContent := NewSFSObject()
	extContent.PutUtfString("c", cmd)
	extContent.PutInt("r", -1)
	extContent.PutSFSObject("p", params)
	return c.SendEnvelope(controllerExtension, actionCallExtension, extContent)
}

// ReadEnvelope blocks until the next framed packet arrives and decodes it.
func (c *GameConn) ReadEnvelope() (*Envelope, error) {
	body, err := ReadPacket(c.reader)
	if err != nil {
		return nil, err
	}
	obj, err := DecodeObject(body)
	if err != nil {
		return nil, fmt.Errorf("decode envelope: %w", err)
	}
	env := &Envelope{}
	if v, ok := obj.Get("c"); ok {
		if b, ok := v.Val.(byte); ok {
			env.Controller = b
		}
	}
	if v, ok := obj.Get("a"); ok {
		if s, ok := v.Val.(int16); ok {
			env.Action = s
		}
	}
	if v, ok := obj.Get("p"); ok {
		if p, ok := v.Val.(*SFSObject); ok {
			env.Content = p
		}
	}
	return env, nil
}

// ExtensionMessage is a decoded server->client `cmd` push/response.
type ExtensionMessage struct {
	Cmd    string
	Params *SFSObject
}

// AsExtension interprets an Envelope as an extension message (controller=1),
// mirroring MessageDispather's split of the "p" content into cmd + params.
func (e *Envelope) AsExtension() (*ExtensionMessage, bool) {
	if e.Controller != controllerExtension || e.Content == nil {
		return nil, false
	}
	cmd := e.Content.GetString("c")
	var params *SFSObject
	if v, ok := e.Content.Get("p"); ok {
		if p, ok := v.Val.(*SFSObject); ok {
			params = p
		}
	}
	if params == nil {
		params = NewSFSObject()
	}
	return &ExtensionMessage{Cmd: cmd, Params: params}, true
}

// DoHandshake sends the vanilla SFS2X pre-Login Handshake request
// (HandshakeRequest.KEY_API="api", KEY_CLIENT_TYPE="cl") and waits for the
// response. Confirmed present in this game's own decompiled SDK
// (Smartfox2xLw.decompiled.cs:4363 SendHandshakeRequest, called from
// OnSocketConnect before any Login) but this Go client never sent it --
// every login attempt so far (guest, email-bind, and every reconnect
// variant) skipped straight to Login. Experimental: guest/email-bind work
// fine without it, so it's not obviously required, but it's cheap to send
// and untested for the still-open init-push/reconnect-block questions.
// api="1.7.8" cl="Unity" match the game's own SDK defaults exactly
// (Smartfox2xLw.decompiled.cs:3613-3621).
func (c *GameConn) DoHandshake(timeout time.Duration) (*SFSObject, error) {
	req := NewSFSObject()
	req.PutUtfString("api", "1.7.8")
	req.PutUtfString("cl", "Unity")
	if err := c.SendEnvelope(controllerSystem, actionHandshake, req); err != nil {
		return nil, fmt.Errorf("send handshake: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, fmt.Errorf("timed out waiting for handshake response")
		}
		c.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := c.ReadEnvelope()
		if err != nil {
			return nil, fmt.Errorf("read handshake response: %w", err)
		}
		if env.Controller == controllerSystem && env.Action == actionHandshake {
			if ec, ok := env.Content.Get("ec"); ok {
				return nil, fmt.Errorf("HANDSHAKE FAILED: ec=%v full=%s", ec.Val, env.Content.String())
			}
			return env.Content, nil
		}
		// Anything else this early (unlikely, but be tolerant) is logged
		// and skipped rather than treated as a protocol violation.
		slog.Info("skipped envelope while waiting for handshake", "controller", env.Controller, "action", env.Action, "content", env.Content.String())
	}
}

// StartHeartbeat launches the PingPongRequest loop (dossier §04: every
// 4000ms, {clientTime: ms}), required to avoid the ~12s server-perceived
// timeout while we wait on slower steps (e.g. the user fetching an email
// verification code).
func (c *GameConn) StartHeartbeat(interval time.Duration, start time.Time) {
	c.stopHeartbeat = make(chan struct{})
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-c.stopHeartbeat:
				return
			case <-ticker.C:
				pp := NewSFSObject()
				pp.PutLong("clientTime", time.Since(start).Milliseconds())
				if err := c.SendEnvelope(controllerSystem, actionPingPong, pp); err != nil {
					slog.Error("heartbeat send failed", "error", err)
					return
				}
			}
		}
	}()
}
