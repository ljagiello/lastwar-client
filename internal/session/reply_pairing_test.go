package session

import (
	"bytes"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"lastwar-client/internal/sfs"
)

// replyWithID is a reply the way the server sends it: the caller's fields plus the request's
// `_id` echoed back (Assembly-CSharp.decompiled.cs:82052-82056 reads it off every reply).
func replyWithID(id int32, kv ...string) *sfs.SFSObject {
	p := pushParams(kv...)
	p.PutInt("_id", id)
	return p
}

// TestSendAndWaitSkipsLateReplyToEarlierRequest (B11): after one wait on a cmd times out, its late
// reply is still on the socket. The next request with the same cmd used to take that reply as its
// own (pairing by cmd alone), and every later reply of the cmd was off by one. The reply echoing
// the timed-out request's `_id` must be skipped and the one echoing the pending `_id` returned.
func TestSendAndWaitSkipsLateReplyToEarlierRequest(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	const cmd = "building.production.collect"

	first := make(chan *ExtensionMessage, 1)
	go func() {
		msg, err := ReadNextExtension(server)
		if err != nil {
			return
		}
		first <- msg
	}()
	staleID, err := client.SendExtensionID(cmd, pushParams("uuid", "a"))
	if err != nil {
		t.Fatalf("SendExtensionID: %v", err)
	}
	req := <-first
	if got := req.Params.GetInt("_id"); got != staleID {
		t.Fatalf("request _id on the wire = %d, SendExtensionID returned %d", got, staleID)
	}
	// The first wait gives up before the server answers.
	if _, err := WaitForReply(client, 50*time.Millisecond, staleID, cmd); err == nil {
		t.Fatal("first wait = nil error, want a timeout")
	}

	go func() {
		next, err := ReadNextExtension(server)
		if err != nil {
			return
		}
		// The late reply to the first request arrives first, then the reply to this one.
		_ = server.SendExtension(cmd, replyWithID(staleID, "which", "late reply to the first request"))
		_ = server.SendExtension(cmd, replyWithID(next.Params.GetInt("_id"), "which", "reply to the second request"))
	}()
	msg, err := SendAndWait(client, "collect", cmd, pushParams("uuid", "b"))
	if err != nil {
		t.Fatalf("SendAndWait: %v", err)
	}
	if got := msg.Params.GetString("which"); got != "reply to the second request" {
		t.Errorf("SendAndWait returned %q, want the reply to its own request", got)
	}
}

// TestStaleReplyNeedsPositiveEvidence: only the first message echoing the `_id` of one of this
// connection's own earlier requests is stale. A reply without `_id` (pushes, possibly error
// replies), one echoing the pending `_id`, one echoing an `_id` this connection never sent or one
// already answered, and anything with an unknown pending id are taken by cmd as before.
func TestStaleReplyNeedsPositiveEvidence(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	const cmd = "al.help.all"

	// Two requests (_id 2 and 3); the server answers neither yet.
	go func() {
		for range 2 {
			if _, err := ReadNextExtension(server); err != nil {
				return
			}
		}
	}()
	id2, _ := client.SendExtensionID(cmd, nil)
	id3, _ := client.SendExtensionID(cmd, nil)
	if id2 != 2 || id3 != 3 {
		t.Fatalf("ids = %d, %d, want 2, 3", id2, id3)
	}

	read := func(params *sfs.SFSObject) *ExtensionMessage {
		t.Helper()
		go func() { _ = server.SendExtension(cmd, params) }()
		env, err := client.ReadEnvelope()
		if err != nil {
			t.Fatalf("ReadEnvelope: %v", err)
		}
		msg, _ := env.AsExtension()
		return msg
	}
	wrongTyped := sfs.NewSFSObject()
	wrongTyped.PutUtfString("_id", "2")
	cases := []struct {
		name    string
		params  *sfs.SFSObject
		pending int32
		want    bool
	}{
		{"no _id", sfs.NewSFSObject(), id3, false},
		{"wrong-typed _id", wrongTyped, id3, false},
		{"an _id never sent", replyWithID(40), id3, false},
		{"the pending _id", replyWithID(id3), id3, false},
		{"the earlier request's late reply", replyWithID(id2), id3 + 1, true},
		{"a second message echoing the same _id", replyWithID(id2), id3 + 1, false},
		{"pending id unknown", replyWithID(id3), 0, false},
	}
	for _, c := range cases {
		if got := StaleReply(read(c.params), c.pending); got != c.want {
			t.Errorf("%s: StaleReply = %v, want %v", c.name, got, c.want)
		}
	}
	if StaleReply(&ExtensionMessage{Cmd: cmd, Params: replyWithID(1)}, id3) {
		t.Error("an ExtensionMessage not read off a connection was judged stale")
	}
}

// TestSendExtensionIDReturnsTheWireID: SendExtensionID returns the `_id` that went out, the caller's
// own when it set one (login.go's login.init fallback sends 2), and a fake server's sends carry no
// `_id` of their own counter's making.
func TestSendExtensionIDReturnsTheWireID(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	got := make(chan []int32, 1)
	go func() { got <- readIDs(t, server, 2) }()

	custom := sfs.NewSFSObject()
	custom.PutInt("_id", 77)
	a, err := client.SendExtensionID("operator.cmd", custom)
	if err != nil {
		t.Fatal(err)
	}
	b, err := client.SendExtensionID("visitor.list", nil)
	if err != nil {
		t.Fatal(err)
	}
	if wire := <-got; a != wire[0] || b != wire[1] || a != 77 || b != 3 {
		t.Errorf("returned ids %d, %d; wire ids %v; want 77 and 3 on both", a, b, wire)
	}

	go func() { _, _ = server.SendExtensionID("push.something", nil) }()
	env, err := client.ReadEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if msg, _ := env.AsExtension(); msg.Params.Has("_id") {
		t.Errorf("a fake server push carried _id %v of its own", msg.Params.GetInt("_id"))
	}
}

// TestReadAndReplyEchoesRequestID: the fake server answers with the request's `_id`, as the real
// server does, so SendAndWait tests exercise the pairing rather than pass by coincidence.
func TestReadAndReplyEchoesRequestID(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	go ReadAndReply(server, "", pushParams("ok", "1"))
	msg, err := SendAndWait(client, "test", "test.cmd", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := msg.Params.GetInt("_id"); got != 2 {
		t.Errorf("reply _id = %d, want the request's 2", got)
	}
	if msg.repliesTo != 2 {
		t.Errorf("reply not marked as answering request 2 (repliesTo %d)", msg.repliesTo)
	}
}

// TestSendAndWaitDonationResetCodeIsBenign (B33): al.science.donate's 120836, live on every
// 02:00 UTC run, no longer fails the run; the same code on another cmd still does.
func TestSendAndWaitDonationResetCodeIsBenign(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	go ReadAndReply(server, "", pushParams("errorCode", "120836"))
	if _, err := SendAndWait(client, "alliance tech donate", "al.science.donate", nil); err != nil {
		t.Errorf("SendAndWait(al.science.donate) with 120836 = %v, want nil", err)
	}
	go ReadAndReply(server, "", pushParams("errorCode", "120836"))
	if _, err := SendAndWait(client, "refresh", "al.science.refreshNum", nil); err == nil {
		t.Error("SendAndWait(al.science.refreshNum) with 120836 = nil, want the failure")
	}
}

// sendPong has server send a pong for a heartbeat that started at start.
func sendPong(server *GameConn, start time.Time) {
	pong := pongEnvelope(start, int64(1_800_000_000_000)).Content
	go func() { _ = server.SendEnvelope(ControllerSystem, ActionPingPong, pong) }()
}

// TestWaitForWatchdogEndsSilentConnection (B68): the client disconnects after 12 s without a
// pong (offMaxTime, BaseUtils.decompiled.cs:35935, 36017-36031). Go only reads while it waits, so
// it counts blocked time alone: a full DefaultCmdTimeout wait with no frame at all is two missed
// pongs, and a second one in a row passes 12 s, so it must come back as a dead connection, which
// stops every collect loop, instead of one more benign timeout.
func TestWaitForWatchdogEndsSilentConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := NewPipeGameConnPair(t)
		client.clock.setStart(time.Now())
		sendPong(server, time.Now())
		if _, err := client.ReadEnvelope(); err != nil {
			t.Fatal(err)
		}

		never := func(*Envelope) bool { return false }
		_, err := WaitFor(client, DefaultCmdTimeout, never)
		var netErr net.Error
		if !errors.As(err, &netErr) || !netErr.Timeout() {
			t.Fatalf("first silent wait = %v, want a benign timeout", err)
		}
		_, err = WaitFor(client, DefaultCmdTimeout, never)
		if !ContainsNonTimeoutNetError(err) {
			t.Fatalf("second silent wait (16 s without a frame) = %v, want a dead connection", err)
		}
		var dead sfs.DeadConnError
		if !errors.As(err, &dead) {
			t.Errorf("err = %T, want an sfs.DeadConnError", err)
		}
	})
}

// TestWaitForWatchdogCountsOnlySilence: any frame read resets the count, pongs included, and a
// connection that never ponged keeps the old behaviour (the client's watchdog is off until
// lastPingPongTime is first set, BaseUtils.decompiled.cs:36021-36024). Fake servers never pong,
// so this also keeps their long silent tests benign.
func TestWaitForWatchdogCountsOnlySilence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		never := func(*Envelope) bool { return false }
		benign := func(t *testing.T, client *GameConn, what string) {
			t.Helper()
			_, err := WaitFor(client, DefaultCmdTimeout, never)
			if ContainsNonTimeoutNetError(err) {
				t.Fatalf("%s = %v, want a benign timeout", what, err)
			}
		}

		client, _ := NewPipeGameConnPair(t)
		for i := range 3 {
			benign(t, client, "silent wait before any pong #"+string(rune('1'+i)))
		}

		client, server := NewPipeGameConnPair(t)
		client.clock.setStart(time.Now())
		sendPong(server, time.Now())
		if _, err := client.ReadEnvelope(); err != nil {
			t.Fatal(err)
		}
		benign(t, client, "first silent wait")
		sendPong(server, time.Now()) // read inside the next wait, which then times out
		benign(t, client, "a wait that read a pong")
		benign(t, client, "the first silent wait after it")
	})
}

// TestPongTimedAtArrival (B71): a pong that came off the socket together with an earlier frame is
// timed when it arrived, not when the flow got round to it, so a delay in decoding or between
// waits does not lengthen its round trip and let it replace a better clock sample (the client
// stamps in its receive thread, BaseUtils.decompiled.cs:32810-32812).
func TestPongTimedAtArrival(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client, server := NewPipeGameConnPair(t)
		start := time.Now()
		client.clock.setStart(start)

		frame := func(controller byte, action int16, content *sfs.SFSObject) []byte {
			outer := sfs.NewSFSObject()
			outer.PutByte("c", controller)
			outer.PutShort("a", action)
			outer.PutSFSObject("p", content)
			body, err := sfs.EncodeObject(outer)
			if err != nil {
				t.Fatal(err)
			}
			packet, err := sfs.EncodePacket(body)
			if err != nil {
				t.Fatal(err)
			}
			return packet
		}
		push := sfs.NewSFSObject()
		push.PutUtfString("c", "push.something")
		push.PutSFSObject("p", sfs.NewSFSObject())
		// The ping went out at start (clientTime 0); its pong comes back 40 ms later. (A bubble's
		// clock starts in 2000, before the clock's plausibility floor, so serverTime is fixed.)
		pong := pongEnvelope(time.Now(), int64(1_800_000_000_000)).Content
		time.Sleep(40 * time.Millisecond)
		both := append(frame(ControllerExtension, ActionCallExtension, push), frame(ControllerSystem, ActionPingPong, pong)...)
		go func() { _, _ = server.RawConn().Write(both) }()

		if _, err := client.ReadEnvelope(); err != nil { // the push; the pong now sits in the buffer
			t.Fatal(err)
		}
		time.Sleep(300 * time.Millisecond) // the flow is busy elsewhere
		if _, err := client.ReadEnvelope(); err != nil {
			t.Fatal(err)
		}
		if rtt := client.clock.minRTT; rtt != 40 {
			t.Errorf("pong round trip = %d ms, want 40 (timed at arrival, not 300 ms later when read)", rtt)
		}
	})
}

// TestForwardServerIDFraming (B69): off by default, so the production frames keep header 0xC0;
// once set, every frame, pings included, carries bit 0x04 and the zone number right after the
// header byte (BaseUtils.decompiled.cs:33557-33567), and still decodes.
func TestForwardServerIDFraming(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	raw := func(send func()) []byte {
		t.Helper()
		go send()
		buf := make([]byte, 4096)
		n, err := server.RawConn().Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		return buf[:n]
	}

	plain := raw(func() { _ = client.SendExtension("al.help.all", nil) })
	if plain[0] != sfs.HdrBinary|sfs.HdrEncrypted {
		t.Errorf("default header = %#x, want 0xc0 (no forward sid)", plain[0])
	}

	client.SetForwardServerID(783)
	for name, send := range map[string]func(){
		"request": func() { _ = client.SendExtension("al.help.all", nil) },
		"ping":    func() { _ = client.sendHeartbeat(time.Now()) },
	} {
		b := raw(send)
		if b[0] != sfs.HdrBinary|sfs.HdrEncrypted|0x04 || b[1] != 0x03 || b[2] != 0x0F {
			t.Errorf("%s frame starts %x, want c4 030f (forward bit, sid 783)", name, b[:3])
		}
		if _, err := sfs.ReadPacket(bytes.NewReader(b)); err != nil {
			t.Errorf("%s frame does not decode: %v", name, err)
		}
	}
}
