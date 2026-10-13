package auth

import (
	"os"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// emailFlowServer starts a fake game server that walks Login's email path (base Login, init,
// send-code ack, account.login.new ack) and then pushes push.account.login.new with push, and
// points GSL at it. It returns the FIFO to write the code to.
func emailFlowServer(t *testing.T, push *sfs.SFSObject) string {
	t.Helper()
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		ok := sfs.NewSFSObject()
		ok.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, ok); err != nil {
			return
		}
		if err := server.SendExtension("init", sfs.NewSFSObject()); err != nil {
			return
		}
		for _, cmd := range []string{"account.login.send.verify.code", "account.login.new"} {
			if _, err := session.ReadNextExtension(server); err != nil {
				return
			}
			if err := server.SendExtension(cmd, ok); err != nil {
				return
			}
		}
		_ = server.SendExtension("push.account.login.new", push)
	})
	host, port := testutil.SplitHostPortInt(t, addr)
	testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(host), Port: testutil.FlexPort(port), Zone: "APS1"}},
		At:         &gsl.LoginToken{Token: "tok-1"},
	}))
	pipe := mkfifoT(t, t.TempDir(), "code.pipe")
	go func() {
		f, err := os.OpenFile(pipe, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString("123456\n")
	}()
	return pipe
}

func readState(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// TestLoginEmailBindsRequestedServer: with Server, the email flow keeps the account's loginKey and
// binds the requested role's gameUid and name instead of the top-level ones.
func TestLoginEmailBindsRequestedServer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	push := pushWithRoles(liveShapeRole(783, "APS783", "uid-783", "Main"), liveShapeRole(72, "APS72", "uid-72", "Alt"))
	push.PutUtfString("gameUserName", "Main")
	pipe := emailFlowServer(t, push)

	result, err := Login(LoginOptions{Email: "player@example.com", CodePipe: pipe, Server: "APS72"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()

	if got := readState(t, loginKeyStatePath()); got != "account-login-key" {
		t.Errorf("persisted loginKey = %q, want the account's", got)
	}
	if got := readState(t, gameUidStatePath()); got != "uid-72" {
		t.Errorf("persisted gameUid = %q, want the requested role's uid-72", got)
	}
	if got := readState(t, usernameStatePath()); got != "Alt" {
		t.Errorf("persisted username = %q, want the requested role's name", got)
	}
}

// TestLoginEmailWithoutServerKeepsTopLevel: without Server the top-level gameUid is bound, as
// before.
func TestLoginEmailWithoutServerKeepsTopLevel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pipe := emailFlowServer(t, pushWithRoles(liveShapeRole(783, "APS783", "uid-783", "Main"), liveShapeRole(72, "APS72", "uid-72", "Alt")))

	result, err := Login(LoginOptions{Email: "player@example.com", CodePipe: pipe})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()
	if got := readState(t, gameUidStatePath()); got != "uid-783" {
		t.Errorf("persisted gameUid = %q, want the top-level uid-783", got)
	}
}

// TestLoginEmailUnknownServerSavesNoLoginKey: a Server the account has no role on fails after
// verification without saving the loginKey, so the device isn't bound to a role nobody asked for.
// The code is spent by then, and the error says so and what to do. (The guest's own state from the
// base Login may be on disk; only the binding is withheld.)
func TestLoginEmailUnknownServerSavesNoLoginKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pipe := emailFlowServer(t, pushWithRoles(liveShapeRole(783, "APS783", "uid-783", "Main")))

	_, err := Login(LoginOptions{Email: "player@example.com", CodePipe: pipe, Server: "72"})
	if err == nil || !strings.Contains(err.Error(), "no role on server 72") || !strings.Contains(err.Error(), "fresh LASTWAR_STATE_DIR") {
		t.Fatalf("err = %v, want the no-role error with the next step", err)
	}
	if lk := readState(t, loginKeyStatePath()); lk != "" {
		t.Errorf("persisted loginKey %q, want none", lk)
	}
	if uid := readState(t, gameUidStatePath()); uid == "uid-783" {
		t.Errorf("persisted the account role's gameUid %q", uid)
	}
}

// TestLoginEmailBindsAnEntrysOwnLoginKey: a role entry that carries its own loginKey (the role
// picker reads param.loginKey) is bound with it rather than the account's.
func TestLoginEmailBindsAnEntrysOwnLoginKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	alt := liveShapeRole(72, "APS72", "uid-72", "Alt")
	alt.PutUtfString("loginKey", "lk-role-72")
	pipe := emailFlowServer(t, pushWithRoles(liveShapeRole(783, "APS783", "uid-783", "Main"), alt))

	result, err := Login(LoginOptions{Email: "player@example.com", CodePipe: pipe, Server: "72"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	defer func() { _ = result.Conn.Close() }()
	if got := readState(t, loginKeyStatePath()); got != "lk-role-72" {
		t.Errorf("persisted loginKey = %q, want the entry's lk-role-72", got)
	}
}

// TestLoginRejectsABadServerBeforeAnyRequest: a -server that names no server number fails before
// check-version, the guest Login, or the email code it would waste.
func TestLoginRejectsABadServerBeforeAnyRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orig := gsl.CheckVersionHosts
	gsl.CheckVersionHosts = []string{"http://gsl.invalid"}
	defer func() { gsl.CheckVersionHosts = orig }()
	for _, bad := range []string{"APS", "State#783"} {
		if _, err := Login(LoginOptions{Email: "player@example.com", Server: bad}); err == nil || !strings.Contains(err.Error(), "-server") {
			t.Errorf("Server %q: err = %v, want the -server error", bad, err)
		}
	}
	if _, err := os.Stat(deviceIDStatePath()); !os.IsNotExist(err) {
		t.Errorf("a device identity was created before the -server check (stat err %v)", err)
	}
}
