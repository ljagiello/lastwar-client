package app

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// shumeiExitPushed is the made-up shumeiId every shumeiExitCases server pushes.
const shumeiExitPushed = "made-up-pushed-9"

// sendShumeiPush sends the push.shumei.id the server sends about 0.5 s after the Login
// (PushShumeiIdMessage.lua:8-18).
func sendShumeiPush(server *session.GameConn) bool {
	p := sfs.NewSFSObject()
	p.PutUtfString("shumeiId", shumeiExitPushed)
	return server.SendExtension("push.shumei.id", p) == nil
}

// failEveryRequest answers each request with a non-benign errorCode under its own cmd and its
// push. form, as fakeInitPushThenFailAllServer does, until the connection closes.
func failEveryRequest(server *session.GameConn) {
	for {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			continue
		}
		fail := sfs.NewSFSObject()
		fail.PutUtfString("errorCode", "999999")
		if server.SendExtension(msg.Cmd, fail) != nil || server.SendExtension("push."+msg.Cmd, fail) != nil {
			return
		}
	}
}

// loginInitPush logs the client in, then sends init and the shumeiId push in the given order.
func loginInitPush(server *session.GameConn, pushFirst bool) bool {
	if !readLoginRequest(server) || !sendLoginOK(server) {
		return false
	}
	if pushFirst && !sendShumeiPush(server) {
		return false
	}
	if server.SendExtension("init", sfs.NewSFSObject()) != nil {
		return false
	}
	return pushFirst || sendShumeiPush(server)
}

// shumeiExitCases are runs that get a push.shumei.id and then end each way a run can end after
// the Login: the exit code, the server, and the run's -collect, -interactive and -run. held: the
// config (and so the Login) already has the pushed id, as one recaptured with pcap -session-out.
var shumeiExitCases = map[string]struct {
	code        int
	server      func(*session.GameConn)
	collect     bool
	interactive bool
	runFeature  string
	held        bool
}{
	// A kick while FetchInit waits for init: exit 3 (exitIfSessionEnded).
	"kicked during init": {code: exitCodeSessionEnded, collect: true, server: func(server *session.GameConn) {
		if !readLoginRequest(server) || !sendLoginOK(server) || !sendShumeiPush(server) {
			return
		}
		kick := sfs.NewSFSObject()
		kick.PutUtfString("errorCode", "E100083")
		_ = server.SendExtension("push.user.off", kick)
	}},
	// The cron's run: -collect without -interactive, and a refused request is enough for exit 1
	// (shouldAbortBeforeInteractive(err, false)).
	"collect with a failed request": {code: 1, collect: true, server: func(server *session.GameConn) {
		if loginInitPush(server, false) {
			failEveryRequest(server)
		}
	}},
	// -interactive keeps the run going past the failures; RunInteractive then exits 1 itself (no
	// control pipe), without returning to runCrossServerTest.
	"collect, then -interactive exits": {code: 1, collect: true, interactive: true, server: func(server *session.GameConn) {
		if loginInitPush(server, false) {
			failEveryRequest(server)
		}
	}},
	// The cron's run on a config that already holds the pushed id: the push repeats it, nothing
	// is written, and the run still logs a line for it.
	"collect with a failed request, the id already saved": {code: 1, collect: true, held: true, server: func(server *session.GameConn) {
		if loginInitPush(server, false) {
			failEveryRequest(server)
		}
	}},
	// The connection drops at the first collect request: exit 1.
	"connection lost during collect": {code: 1, collect: true, server: func(server *session.GameConn) {
		if !loginInitPush(server, false) {
			return
		}
		_, _ = session.ReadNextExtension(server)
		_ = server.Close()
	}},
	// A maintenance stop answers the first collect request: exit 3.
	"server stop during collect": {code: exitCodeSessionEnded, collect: true, server: func(server *session.GameConn) {
		if !loginInitPush(server, false) {
			return
		}
		if _, err := session.ReadNextExtension(server); err != nil {
			return
		}
		_ = server.SendExtension("push.server.stop", nil)
		for {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
		}
	}},
	// -run of a feature that has nothing to do on this init: exit 0 through the -run return.
	"-run that succeeds": {code: 0, runFeature: "finished-timers", server: func(server *session.GameConn) {
		if loginInitPush(server, true) {
			failEveryRequest(server)
		}
	}},
	// -run that fails: exit 1.
	"-run that fails": {code: 1, runFeature: "no-such-feature", server: func(server *session.GameConn) {
		if loginInitPush(server, true) {
			failEveryRequest(server)
		}
	}},
}

// TestRunCrossServerTestSavesThePushedShumeiIDHoweverTheRunEnds: the client saves a pushed
// shumeiId in the push handler (PushShumeiIdMessage.lua:16-17, Setting:SetString then
// Setting:Save()), so a session that then ends badly keeps it. A run that gets the push keeps it
// in the session config whichever way it ends: exit 3 on a terminal push, exit 1 on a failed
// -collect (the cron's case), on RunInteractive's own exit or on a lost connection, and through
// both ends of -run. Each case runs runCrossServerTest in a re-exec'd subprocess, since it calls
// os.Exit (see TestRunCrossServerTestExitsCode3WhenServerEndsSession). A push that repeats the
// saved id writes nothing and logs that it is already saved, so the config-copy test, which waits
// for one of the two lines, holds for a recaptured config too.
func TestRunCrossServerTestSavesThePushedShumeiIDHoweverTheRunEnds(t *testing.T) {
	cfgShumeiID := func(held bool) string {
		if held {
			return shumeiExitPushed
		}
		return "pushed-1"
	}
	if name := os.Getenv("LASTWAR_TEST_SHUMEI_EXIT_CASE"); name != "" {
		c := shumeiExitCases[name]
		t.Setenv("HOME", t.TempDir())
		gameAddr := session.StartFakeGameServer(t, c.server)
		gameHost, gamePort := testutil.SplitHostPortInt(t, gameAddr)
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{Code: "0"}))
		o := crossServerTestOpts{
			ip: gameHost, port: gamePort, zone: "APS1", gameUid: "uid-1", deviceID: "dev-1_n3d", at: "tok-1",
			iosMode: true, shumeiID: cfgShumeiID(c.held), configSavePath: os.Getenv("LASTWAR_TEST_SHUMEI_EXIT_CFG"),
			collect: c.collect, runFeature: c.runFeature,
		}
		if c.interactive {
			o.interactive = t.TempDir() + "/does-not-exist-fifo"
		}
		runCrossServerTest(o)
		return // exit 0
	}

	for name, c := range shumeiExitCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := writeSessionConfig(t, SessionConfig{IP: "127.0.0.1", Port: 1, Zone: "APS1", GameUid: "uid-1",
				DeviceID: "dev-1_n3d", AccessToken: "tok-1", IOSMode: true, ShumeiID: cfgShumeiID(c.held)})
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestSavesThePushedShumeiIDHoweverTheRunEnds$")
			cmd.Env = append(os.Environ(), "LASTWAR_TEST_SHUMEI_EXIT_CASE="+name, "LASTWAR_TEST_SHUMEI_EXIT_CFG="+path)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				exitErr, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatalf("subprocess: %v; stderr=%s", err, stderr.String())
				}
				code = exitErr.ExitCode()
			}
			if code != c.code {
				t.Errorf("exit code %d, want %d; stderr=%s", code, c.code, stderr.String())
			}
			if got := readSessionConfig(t, path).ShumeiID; got != shumeiExitPushed {
				t.Errorf("exit %d: the session config's shumeiId is %q, want the pushed %q; stderr=%s", code, got, shumeiExitPushed, stderr.String())
			}
			wantLine, notLine := "saved the pushed shumeiId", "pushed shumeiId already saved"
			if c.held {
				wantLine, notLine = notLine, wantLine
				if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
					t.Errorf("a repeated push rewrote the session config (err %v)", err)
				}
			}
			if !strings.Contains(stderr.String(), wantLine) || strings.Contains(stderr.String(), notLine) {
				t.Errorf("want a %q line and no %q line; stderr=%s", wantLine, notLine, stderr.String())
			}
		})
	}
}
