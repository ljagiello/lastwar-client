package auth

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// captureAuthLog routes slog to a buffer for one test.
func captureAuthLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return &buf
}

// loginReplyWithUpdate is a game-server Login reply carrying updateType (Int) and new_version
// (UtfString), the fields LoginMessage.CSHandleResponse reads (Assembly-CSharp.decompiled.cs:
// 82878-82896), on the reply itself or, underP, on its p object.
func loginReplyWithUpdate(updateType int32, newVersion string, underP bool) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutInt("updateType", updateType)
	o.PutUtfString("new_version", newVersion)
	o.PutUtfString("downloadurl", "https://apps.apple.com/app/id0")
	if !underP {
		return o
	}
	reply := sfs.NewSFSObject()
	reply.PutSFSObject("p", o)
	return reply
}

func TestReadLoginUpdateGate(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply *sfs.SFSObject
		want  loginUpdateGate
	}{
		{"top level", loginReplyWithUpdate(3, "1.0.350", false), loginUpdateGate{3, true, "1.0.350"}},
		{"under p", loginReplyWithUpdate(2, "1.0.367", true), loginUpdateGate{2, true, "1.0.367"}},
		{"absent", sfs.NewSFSObject(), loginUpdateGate{}},
		{"nil reply", nil, loginUpdateGate{}},
	} {
		if got := readLoginUpdateGate(c.reply, "test"); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
	zero := sfs.NewSFSObject()
	zero.PutInt("updateType", 0)
	if got := readLoginUpdateGate(zero, "test"); !got.hasUpdateType || got.updateType != 0 {
		t.Errorf("updateType 0: got %+v, want it present", got)
	}
}

func TestLogLoginUpdateGate(t *testing.T) {
	for _, c := range []struct {
		name     string
		reply    *sfs.SFSObject
		wantInfo []string
		wantWarn bool
	}{
		// The live 2026-10-04 Login for iOS 1.0.344 (docs/bootstrap.mdx): proceed, and a newer
		// new_version is no warning.
		{"proceed with a newer new_version", loginReplyWithUpdate(3, "1.0.350", false),
			[]string{"updateType=3", "newVersion=1.0.350", "appVersion=1.0.344"}, false},
		{"optional update", loginReplyWithUpdate(1, "1.0.350", false), []string{"updateType=1"}, false},
		{"forced update", loginReplyWithUpdate(2, "1.0.367", true), []string{"updateType=2", "newVersion=1.0.367"}, true},
		{"absent", sfs.NewSFSObject(), []string{"updateType=absent", "newVersion=absent"}, false},
	} {
		logs := captureAuthLog(t)
		logLoginUpdateGate(c.reply, "cross-server Login", "1.0.344")
		out := logs.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "login reply: update gate") {
			t.Errorf("%s: log %q, want the Info line", c.name, out)
		}
		for _, w := range c.wantInfo {
			if !strings.Contains(out, w) {
				t.Errorf("%s: log %q, want %s", c.name, out, w)
			}
		}
		if got := strings.Contains(out, "level=WARN"); got != c.wantWarn {
			t.Errorf("%s: warned = %v, want %v (log %q)", c.name, got, c.wantWarn, out)
		}
		if c.wantWarn && !strings.Contains(out, "real client stops here") {
			t.Errorf("%s: warning %q, want it to say the real client stops", c.name, out)
		}
	}
}

// TestDoCrossServerLoginLogsForcedUpdateAndCarriesOn: a Login reply with updateType 2 is logged and
// warned about, and the login still succeeds (no exit; enforcing it is a separate decision). The
// connection records the appVersion the Login claimed.
func TestDoCrossServerLoginLogsForcedUpdateAndCarriesOn(t *testing.T) {
	logs := captureAuthLog(t)
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, loginReplyWithUpdate(2, "1.0.367", true))
	})
	host, port := testutil.SplitHostPortInt(t, addr)
	result, err := DoCrossServerLogin(CrossServerLoginParams{
		IP: host, Port: port, Zone: "APS1", GameUid: "uid-1", DeviceID: "dev-1", AirKey: "airkey-1", AccessTok: "tok-1",
		IOSMode: true, AppVersion: "1.0.344",
	})
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()
	out := logs.String()
	for _, w := range []string{"updateType=2", "newVersion=1.0.367", "appVersion=1.0.344", "level=WARN"} {
		if !strings.Contains(out, w) {
			t.Errorf("log %q, want %s", out, w)
		}
	}
	if got := result.Conn.AppVersion(); got != "1.0.344" {
		t.Errorf("Conn.AppVersion() = %q, want the claimed 1.0.344", got)
	}
}

// TestLoginLogsUpdateGate: the base-zone Login reads the same fields, and its connection records the
// build the Login claimed (gsl.AppVersion for the default Android build).
func TestLoginLogsUpdateGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	logs := captureAuthLog(t)
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, loginReplyWithUpdate(3, "1.0.367", false)); err != nil {
			return
		}
		_ = server.SendExtension("init", sfs.NewSFSObject())
	})
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
	out := logs.String()
	for _, w := range []string{"base-zone Login", "updateType=3", "newVersion=1.0.367", "appVersion=" + gsl.AppVersion} {
		if !strings.Contains(out, w) {
			t.Errorf("log %q, want %s", out, w)
		}
	}
	if strings.Contains(out, "forced update") {
		t.Errorf("log %q: updateType 3 must not warn", out)
	}
	if got := result.Conn.AppVersion(); got != gsl.AppVersion {
		t.Errorf("Conn.AppVersion() = %q, want %q", got, gsl.AppVersion)
	}
}
