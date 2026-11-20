package auth

import (
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// WP-Z touch point 2 (MASTER B66, aud-transport/F1): a push.account.device.del that names this
// session's device ends the run (PushAccountDeviceDelMessage.lua:8-25), so the connection must know
// the deviceId the Login sent, the client's DEVICE_ID (Assembly-CSharp.decompiled.cs:73399-73416,
// 83092-83093). Without it no device push is terminal.

// deviceIDLoginServer accepts one Login, reports its p.deviceId on seen, replies with reply and,
// when init is set, pushes an empty init.
func deviceIDLoginServer(seen chan<- string, reply *sfs.SFSObject, init bool) func(*session.GameConn) {
	return func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		id := ""
		if pv, ok := env.Content.Get("p"); ok {
			if p, ok := pv.Val.(*sfs.SFSObject); ok {
				id = p.GetString("deviceId")
			}
		}
		seen <- id
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, reply); err != nil {
			return
		}
		if init {
			_ = server.SendExtension("init", sfs.NewSFSObject())
		}
	}
}

func TestDoCrossServerLoginRecordsTheDeviceID(t *testing.T) {
	seen := make(chan string, 1)
	ok := sfs.NewSFSObject()
	ok.PutBool("success", true)
	addr := session.StartFakeGameServer(t, deviceIDLoginServer(seen, ok, false))
	host, port := testutil.SplitHostPortInt(t, addr)
	result, err := DoCrossServerLogin(CrossServerLoginParams{
		IP: host, Port: port, Zone: "APS1", GameUid: "uid-1", DeviceID: "dev-1", AirKey: "airkey-1", AccessTok: "tok-1",
		IOSMode: true, AppVersion: "1.0.370",
	})
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()
	sent := <-seen
	if got := result.Conn.DeviceID(); got != sent || got != "dev-1" {
		t.Errorf("Conn.DeviceID() = %q, the Login sent deviceId %q; want both dev-1", got, sent)
	}
}

func TestLoginRecordsTheDeviceID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seen := make(chan string, 1)
	addr := session.StartFakeGameServer(t, deviceIDLoginServer(seen, loginReplyWithUpdate(3, "1.0.370", false), true))
	host, port := testutil.SplitHostPortInt(t, addr)
	fake := testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS1", GameUid: "uid-1"}},
		At:         &gsl.LoginToken{Token: "tok-1"},
	})
	testutil.UseFakeGSLServer(t, fake)

	result, err := Login(LoginOptions{})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()
	sent := <-seen
	if got := result.Conn.DeviceID(); sent == "" || got != sent {
		t.Errorf("Conn.DeviceID() = %q, the Login sent deviceId %q; want the same, non-empty", got, sent)
	}
}
