package session

import (
	"errors"
	"net"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// assertSessionEnded checks err is the terminal-push error for want: errors.Is matches both the
// specific sentinel and ErrSessionEnded, and every existing dead-connection check treats it as
// fatal (a net.Error with Timeout()==false, found by ContainsNonTimeoutNetError).
func assertSessionEnded(t *testing.T, err, want error) {
	t.Helper()
	if err == nil {
		t.Fatalf("err = nil, want %v", want)
	}
	if !errors.Is(err, want) {
		t.Errorf("errors.Is(%v, %v) = false", err, want)
	}
	if !errors.Is(err, ErrSessionEnded) {
		t.Errorf("errors.Is(%v, ErrSessionEnded) = false", err)
	}
	if !ContainsNonTimeoutNetError(err) {
		t.Errorf("ContainsNonTimeoutNetError(%v) = false; collect loops would keep sending after the server ended the session", err)
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || netErr.Timeout() {
		t.Errorf("err %v is not a non-timeout net.Error; the errors.As(&netErr) && !netErr.Timeout() abort checks would miss it", err)
	}
}

// ownDevice is a session's Login deviceId, in the client's "<udid>_n3d" shape (GetDeviceUid,
// Assembly-CSharp.decompiled.cs:73414); ownAirKey is its SetDeviceUidToTranscoding form,
// "lwDid_" + base64 of the id (73443-73447), computed outside Go with base64(1).
const (
	ownDevice = "6F1C2D3E-4A5B-6C7D-8E9F-0A1B2C3D4E5F_n3d"
	ownAirKey = "lwDid_NkYxQzJEM0UtNEE1Qi02QzdELThFOUYtMEExQjJDM0Q0RTVGX24zZA=="
)

// pushParams builds a push's params from key/value pairs, all UtfStrings.
func pushParams(kv ...string) *sfs.SFSObject {
	p := sfs.NewSFSObject()
	for i := 0; i+1 < len(kv); i += 2 {
		p.PutUtfString(kv[i], kv[i+1])
	}
	return p
}

func TestTerminalPushError(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		params *sfs.SFSObject
		want   error
	}{
		{"kicked", "push.user.off", nil, ErrKicked},
		{"server stop", "push.server.stop", nil, ErrServerStopped},
		{"init error", "init.error", nil, ErrInitFailed},
		// PushUserGMOffMessage.lua:9-16 quits or reloads whatever the push carries.
		{"GM kick", "push.user.gm.off", nil, ErrGMKicked},
		{"GM kick with a code", "push.user.gm.off", pushParams("errorCode", "E130023"), ErrGMKicked},
		{"refund restart", "push.refund.restart", sfs.NewSFSObject(), ErrRefundRestart},
		{"refund restart, no params", "push.refund.restart", nil, ErrRefundRestart},
		{"this device removed", "push.account.device.del", pushParams("deviceId", ownDevice), ErrDeviceRemoved},
		{"this device removed, by airKey", "push.account.device.del", pushParams("airKey", ownAirKey), ErrDeviceRemoved},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := TerminalPushError(c.cmd, c.params, ownDevice)
			assertSessionEnded(t, err, c.want)
			var ended *SessionEndedError
			if !errors.As(err, &ended) || ended.Cmd != c.cmd {
				t.Errorf("TerminalPushError(%q) = %#v, want a *SessionEndedError with Cmd %q", c.cmd, err, c.cmd)
			}
		})
	}

	notTerminal := []struct {
		name, cmd   string
		params      *sfs.SFSObject
		ownDeviceID string
	}{
		// push.server.tostop is the maintenance countdown toast, not the stop itself.
		{"init", "init", nil, ownDevice},
		{"maintenance countdown", "push.server.tostop", nil, ownDevice},
		{"lookalike cmd", "push.user.off.extra", nil, ownDevice},
		{"empty cmd", "", nil, ownDevice},
		// With an errorCode the refund and device pushes only show a tip (lua :10-12 of each).
		{"refund restart with a code", "push.refund.restart", pushParams("errorCode", "120001"), ownDevice},
		{"device push with a code", "push.account.device.del", pushParams("errorCode", "120001", "deviceId", ownDevice), ownDevice},
		// Another device: the client refreshes its device list (PushAccountDeviceDelMessage.lua:22).
		{"another device", "push.account.device.del", pushParams("deviceId", "11111111-2222_n3d"), ownDevice},
		{"another device's airKey", "push.account.device.del", pushParams("airKey", "lwDid_MTExMTExMTEtMjIyMl9uM2Q="), ownDevice},
		{"no device named", "push.account.device.del", sfs.NewSFSObject(), ownDevice},
		{"empty device id", "push.account.device.del", pushParams("deviceId", ""), ownDevice},
		{"own device unknown", "push.account.device.del", pushParams("deviceId", ownDevice), ""},
		{"own device unknown, empty pushed id", "push.account.device.del", pushParams("deviceId", ""), ""},
	}
	for _, c := range notTerminal {
		if err := TerminalPushError(c.cmd, c.params, c.ownDeviceID); err != nil {
			t.Errorf("%s: TerminalPushError(%q, %s, %q) = %v, want nil", c.name, c.cmd, c.params.StringRedacted(), c.ownDeviceID, err)
		}
	}
}

// TestDeviceRemovedIsAlsoAnAuthRejection: an own-device removal ends the session like a kick
// (ErrSessionEnded, checked by assertSessionEnded above) and also voids the stored login, so
// main.go can send the operator to a new bind or capture (exit 2) by checking it first.
func TestDeviceRemovedIsAlsoAnAuthRejection(t *testing.T) {
	err := TerminalPushError("push.account.device.del", pushParams("deviceId", ownDevice), ownDevice)
	if !errors.Is(err, ErrDeviceRemoved) || !errors.Is(err, ErrAuthRejected) || !errors.Is(err, ErrSessionEnded) {
		t.Errorf("err = %v, want ErrDeviceRemoved, ErrAuthRejected and ErrSessionEnded", err)
	}
	for _, other := range []error{ErrGMKicked, ErrRefundRestart, ErrKicked} {
		if errors.Is(other, ErrAuthRejected) {
			t.Errorf("%v wraps ErrAuthRejected; only a removed device voids the login", other)
		}
	}
}

// sendPush has server send cmd with params from a goroutine (net.Pipe is synchronous).
func sendPush(server *GameConn, cmd string, params *sfs.SFSObject) {
	go func() { _ = server.SendExtension(cmd, params) }()
}

// TestReadEnvelopeEndsSessionOnTerminalPush covers MASTER.md 5.1 #2 at the single read choke
// point: each terminal push comes back from ReadEnvelope as its SessionEndedError, carrying the
// push's errorCode, and the connection stays ended -- later reads return the same error without
// touching the socket, and later sends write nothing.
func TestReadEnvelopeEndsSessionOnTerminalPush(t *testing.T) {
	cases := []struct {
		cmd      string
		code     string
		want     error
		wantCode string
	}{
		{cmd: "push.user.off", code: "E100083", want: ErrKicked, wantCode: "E100083"},
		{cmd: "push.user.off", want: ErrKicked},
		{cmd: "push.server.stop", want: ErrServerStopped},
		{cmd: "init.error", want: ErrInitFailed},
	}
	for _, tc := range cases {
		t.Run(tc.cmd+"/"+tc.code, func(t *testing.T) {
			client, server := NewPipeGameConnPair(t)
			params := sfs.NewSFSObject()
			if tc.code != "" {
				params.PutUtfString("errorCode", tc.code)
			}
			sendPush(server, tc.cmd, params)

			env, err := client.ReadEnvelope()
			if env != nil {
				t.Errorf("ReadEnvelope returned an envelope (%+v) along with the terminal error", env)
			}
			assertSessionEnded(t, err, tc.want)
			var ended *SessionEndedError
			if !errors.As(err, &ended) {
				t.Fatalf("err %T is not a *SessionEndedError", err)
			}
			if ended.Cmd != tc.cmd || ended.Code != tc.wantCode {
				t.Errorf("SessionEndedError{Cmd: %q, Code: %q}, want {%q, %q}", ended.Cmd, ended.Code, tc.cmd, tc.wantCode)
			}

			// Nothing is written after the end: the server's read must time out.
			if err := client.SendExtension("al.help.all", nil); !errors.Is(err, tc.want) {
				t.Errorf("SendExtension after the end = %v, want %v", err, tc.want)
			}
			_ = server.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			if env, err := server.ReadEnvelope(); err == nil {
				t.Errorf("server received %+v after the session ended, want nothing", env)
			}

			// A second read returns the same error at once, without blocking on the socket.
			done := make(chan error, 1)
			go func() {
				_, err := client.ReadEnvelope()
				done <- err
			}()
			select {
			case err := <-done:
				assertSessionEnded(t, err, tc.want)
			case <-time.After(2 * time.Second):
				t.Fatal("second ReadEnvelope blocked instead of returning the recorded end")
			}
		})
	}
}

// TestReadEnvelopeEndsSessionOnClientQuitPushes (B66): the GM kick, a refund restart and the
// removal of this session's device end the session at the read choke point, like push.user.off,
// so the run stops sending instead of carrying on until the server drops the socket.
func TestReadEnvelopeEndsSessionOnClientQuitPushes(t *testing.T) {
	cases := []struct {
		cmd    string
		params *sfs.SFSObject
		want   error
	}{
		{"push.user.gm.off", sfs.NewSFSObject(), ErrGMKicked},
		{"push.refund.restart", sfs.NewSFSObject(), ErrRefundRestart},
		{"push.account.device.del", pushParams("deviceId", ownDevice, "airKey", ownAirKey), ErrDeviceRemoved},
	}
	for _, tc := range cases {
		t.Run(tc.cmd, func(t *testing.T) {
			client, server := NewPipeGameConnPair(t)
			client.SetDeviceID(ownDevice)
			sendPush(server, tc.cmd, tc.params)

			env, err := client.ReadEnvelope()
			if env != nil {
				t.Errorf("ReadEnvelope returned an envelope (%+v) along with the terminal error", env)
			}
			assertSessionEnded(t, err, tc.want)
			if err := client.SendExtension("al.help.all", nil); !errors.Is(err, tc.want) {
				t.Errorf("SendExtension after the end = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestReadEnvelopePassesConditionalPushesThrough: a refund or device push with an errorCode, a
// device push naming another device, and any device push before SetDeviceID are ordinary pushes.
func TestReadEnvelopePassesConditionalPushesThrough(t *testing.T) {
	cases := []struct {
		name, cmd string
		params    *sfs.SFSObject
		ownDevice string
	}{
		{"refund with a code", "push.refund.restart", pushParams("errorCode", "120001"), ownDevice},
		{"device push with a code", "push.account.device.del", pushParams("errorCode", "120001", "deviceId", ownDevice), ownDevice},
		{"another device", "push.account.device.del", pushParams("deviceId", "11111111-2222_n3d"), ownDevice},
		{"own device not yet known", "push.account.device.del", pushParams("deviceId", ownDevice), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, server := NewPipeGameConnPair(t)
			if tc.ownDevice != "" {
				client.SetDeviceID(tc.ownDevice)
			}
			sendPush(server, tc.cmd, tc.params)
			env, err := client.ReadEnvelope()
			if err != nil {
				t.Fatalf("ReadEnvelope = %v, want the %s envelope", err, tc.cmd)
			}
			if msg, ok := env.AsExtension(); !ok || msg.Cmd != tc.cmd {
				t.Fatalf("got %+v, want %s", env, tc.cmd)
			}
			if e := client.ended.Load(); e != nil {
				t.Errorf("connection marked ended (%v) by a non-terminal push", e)
			}
		})
	}
}

// TestReadEnvelopePassesOrdinaryPushesThrough: only the terminal cmds end the session; the
// maintenance countdown (push.server.tostop) and everything else arrive as normal envelopes.
func TestReadEnvelopePassesOrdinaryPushesThrough(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	tostop := sfs.NewSFSObject()
	tostop.PutLong("time", 1_900_000_000_000)
	sendPush(server, "push.server.tostop", tostop)

	env, err := client.ReadEnvelope()
	if err != nil {
		t.Fatalf("ReadEnvelope = %v, want the push.server.tostop envelope", err)
	}
	if msg, ok := env.AsExtension(); !ok || msg.Cmd != "push.server.tostop" {
		t.Fatalf("got %+v, want push.server.tostop", env)
	}
	if e := client.ended.Load(); e != nil {
		t.Errorf("connection marked ended (%v) by a non-terminal push", e)
	}
}

// TestSendAndWaitStopsOnKick: a kick arriving while a claim waits for its response ends that wait
// with ErrKicked, and the next claim fails at the send stage without writing anything, so a
// collect loop that ignored the first error still cannot keep talking to the server.
func TestSendAndWaitStopsOnKick(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	go func() {
		if _, err := ReadNextExtension(server); err != nil {
			return
		}
		kick := sfs.NewSFSObject()
		kick.PutUtfString("errorCode", "E100083")
		_ = server.SendExtension("push.user.off", kick)
	}()

	_, err := SendAndWait(client, "alliance help-all", "al.help.all", nil)
	assertSessionEnded(t, err, ErrKicked)

	_, err = SendAndWait(client, "vip login score", "vip.add.login.score", nil)
	assertSessionEnded(t, err, ErrKicked)
	var stage SendStageError
	if !errors.As(err, &stage) {
		t.Errorf("second SendAndWait error = %v, want a SendStageError (refused before writing)", err)
	}
}

// TestWaitForInitFailsFastOnInitError: init.error used to be skipped as an unrelated push, so the
// init wait sat out its whole window (45 s in login.go). It must now return at once.
func TestWaitForInitFailsFastOnInitError(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	sendPush(server, "init.error", nil)

	start := time.Now()
	_, err := WaitForCmd(client, 45*time.Second, "init")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("WaitForCmd took %v, want an immediate return on init.error", elapsed)
	}
	assertSessionEnded(t, err, ErrInitFailed)
}

// TestDoHandshakeStopsOnTerminalPush: the handshake read loop gets the same treatment, through
// its existing dead-connection branch.
func TestDoHandshakeStopsOnTerminalPush(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	go func() {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendExtension("push.server.stop", nil)
	}()
	_, err := client.DoHandshake(10 * time.Second)
	assertSessionEnded(t, err, ErrServerStopped)
}

// TestHeartbeatStopsAfterSessionEnded: once the session has ended the heartbeat stops pinging
// instead of logging a send failure every tick.
func TestHeartbeatStopsAfterSessionEnded(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	pings := make(chan struct{}, 64)
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			if env.Controller == ControllerSystem && env.Action == ActionPingPong {
				pings <- struct{}{}
			}
		}
	}()

	const interval = 5 * time.Millisecond
	client.StartHeartbeat(interval, time.Now())
	select {
	case <-pings:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat never pinged")
	}

	client.ended.Store(&SessionEndedError{Cmd: "push.server.stop", Err: ErrServerStopped})
	time.Sleep(4 * interval) // let a tick already past the ended check finish
	for len(pings) > 0 {
		<-pings
	}
	time.Sleep(10 * interval)
	if n := len(pings); n != 0 {
		t.Errorf("heartbeat sent %d pings after the session ended, want 0", n)
	}
}
