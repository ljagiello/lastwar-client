package auth

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
// 83236-83254, 1.0.370), on the reply itself or, underP, on its p object.
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
	logs := captureAuthLog(t)
	zero := sfs.NewSFSObject()
	zero.PutInt("updateType", 0)
	if got := readLoginUpdateGate(zero, "test"); !got.hasUpdateType || got.updateType != 0 {
		t.Errorf("updateType 0: got %+v, want it present", got)
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Errorf("updateType 0 (proceed) warned: %s", logs.String())
	}
	str := sfs.NewSFSObject()
	str.PutUtfString("updateType", "2")
	if got := readLoginUpdateGate(str, "test"); got.hasUpdateType {
		t.Errorf("a UtfString updateType: got %+v, want it treated as absent (the client reads it with GetInt)", got)
	}
	if out := logs.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "updateType present but not an integer") {
		t.Errorf("a UtfString updateType: log %q, want a Warn", out)
	}
}

func TestCheckLoginUpdateGate(t *testing.T) {
	for _, c := range []struct {
		name       string
		reply      *sfs.SFSObject
		wantInfo   []string
		wantForced bool
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
		err := checkLoginUpdateGate(c.reply, "cross-server Login", "1.0.344")
		out := logs.String()
		if !strings.Contains(out, "level=INFO") || !strings.Contains(out, "login reply: update gate") {
			t.Errorf("%s: log %q, want the Info line", c.name, out)
		}
		for _, w := range c.wantInfo {
			if !strings.Contains(out, w) {
				t.Errorf("%s: log %q, want %s", c.name, out, w)
			}
		}
		if got := errors.Is(err, gsl.ErrForcedUpdate); got != c.wantForced {
			t.Errorf("%s: err = %v, want forced update %v", c.name, err, c.wantForced)
		}
		if !c.wantForced && err != nil {
			t.Errorf("%s: err = %v, want nil", c.name, err)
		}
		if got := strings.Contains(out, "level=ERROR"); got != c.wantForced {
			t.Errorf("%s: logged an error = %v, want %v (log %q)", c.name, got, c.wantForced, out)
		}
		if c.wantForced && !strings.Contains(out, "real client stops here") {
			t.Errorf("%s: error %q, want it to say the real client stops", c.name, out)
		}
	}
}

// TestDoCrossServerLoginStopsOnForcedUpdate: a Login reply with updateType 2 ends the run, as the
// client's loading error 9 does (Assembly-CSharp.decompiled.cs:71486-71494): no connection is
// handed back and the error wraps gsl.ErrForcedUpdate (1.0.370 audit B16).
func TestDoCrossServerLoginStopsOnForcedUpdate(t *testing.T) {
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
	if !errors.Is(err, gsl.ErrForcedUpdate) {
		t.Fatalf("DoCrossServerLogin err = %v, want ErrForcedUpdate", err)
	}
	if result != nil {
		t.Errorf("result = %v, want nil after a forced update", result)
	}
	out := logs.String()
	for _, w := range []string{"updateType=2", "newVersion=1.0.367", "appVersion=1.0.344", "level=ERROR"} {
		if !strings.Contains(out, w) {
			t.Errorf("log %q, want %s", out, w)
		}
	}
}

// TestLoginStopsOnForcedUpdateReply: the base-zone Login moves to AppUpdate on updateType 2
// (Assembly-CSharp.decompiled.cs:13666-13669) and never waits for init.
func TestLoginStopsOnForcedUpdateReply(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LASTWAR_STATE_DIR", "")
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, loginReplyWithUpdate(2, "1.0.371", false)); err != nil {
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
	if !errors.Is(err, gsl.ErrForcedUpdate) {
		t.Fatalf("Login err = %v, want ErrForcedUpdate", err)
	}
	if result != nil {
		t.Errorf("result = %v, want nil after a forced update", result)
	}
}

// TestLoginStopsOnForcedUpdateCheckVersion: a check-version reply with updateType 2 stops the client
// before GSL (Assembly-CSharp.decompiled.cs:9619-9626), so Login makes no getserverlist call.
func TestLoginStopsOnForcedUpdateCheckVersion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LASTWAR_STATE_DIR", "")
	var gslCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "getlsu3dversion.php"):
			_, _ = w.Write([]byte(`{"updateType":"2","downloadurl":"https://store.invalid/app"}`))
		default:
			gslCalls++
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	testutil.UseFakeGSLServer(t, server)

	if _, err := Login(LoginOptions{IOSMode: true}); !errors.Is(err, gsl.ErrForcedUpdate) {
		t.Fatalf("Login err = %v, want ErrForcedUpdate", err)
	}
	if gslCalls != 0 {
		t.Errorf("GSL was called %d times after a forced update", gslCalls)
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
