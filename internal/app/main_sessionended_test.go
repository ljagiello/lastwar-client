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

// readLoginRequest reads envelopes until the client's Login request, skipping its heartbeat pings.
func readLoginRequest(server *session.GameConn) bool {
	for {
		env, err := server.ReadEnvelope()
		if err != nil {
			return false
		}
		if env.Controller == session.ControllerSystem && env.Action == session.ActionLogin {
			return true
		}
	}
}

func sendLoginOK(server *session.GameConn) bool {
	resp := sfs.NewSFSObject()
	resp.PutBool("success", true)
	return server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp) == nil
}

// sessionEndedServers are fake game servers that each end the session with a terminal push at a
// different point of a cross-server run.
var sessionEndedServers = map[string]func(*session.GameConn){
	// Kicked while FetchInit waits for the init push.
	"kicked-during-init": func(server *session.GameConn) {
		if !readLoginRequest(server) || !sendLoginOK(server) {
			return
		}
		kick := sfs.NewSFSObject()
		kick.PutUtfString("errorCode", "E100083")
		_ = server.SendExtension("push.user.off", kick)
	},
	// init.error in place of the Login response.
	"init-error-at-login": func(server *session.GameConn) {
		if !readLoginRequest(server) {
			return
		}
		_ = server.SendExtension("init.error", nil)
	},
	// Maintenance stop answering the first collect request.
	"server-stop-during-collect": func(server *session.GameConn) {
		if !readLoginRequest(server) || !sendLoginOK(server) {
			return
		}
		if server.SendExtension("init", sfs.NewSFSObject()) != nil {
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
	},
}

// TestRunCrossServerTestExitsCode3WhenServerEndsSession covers MASTER.md 5.1 #2 end to end: a
// terminal push at login, during the init wait, or during -collect makes the run exit with code 3
// and the "ended this session" log line, instead of the generic 1 (or carrying on). Each case runs
// runCrossServerTest in a re-exec'd subprocess, since it calls os.Exit (see
// TestRunCrossServerTestExitsCode2WhenRefreshHasNoUsableData).
func TestRunCrossServerTestExitsCode3WhenServerEndsSession(t *testing.T) {
	if name := os.Getenv("LASTWAR_TEST_SESSION_ENDED_CASE"); name != "" {
		t.Setenv("HOME", t.TempDir())
		gameAddr := session.StartFakeGameServer(t, sessionEndedServers[name])
		gameHost, gamePort := testutil.SplitHostPortInt(t, gameAddr)
		fakeGSL := testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{Code: "0"})
		testutil.UseFakeGSLServer(t, fakeGSL)

		runCrossServerTest(crossServerTestOpts{
			ip: gameHost, port: gamePort, zone: "APS1", gameUid: "uid-1", at: "tok-1",
			collect: true,
		})
		return // only reached if runCrossServerTest did not exit
	}

	for name := range sessionEndedServers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsCode3WhenServerEndsSession$")
			cmd.Env = append(os.Environ(), "LASTWAR_TEST_SESSION_ENDED_CASE="+name)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			exitErr, ok := runErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("subprocess did not exit with an error: err=%v, stderr=%s", runErr, stderr.String())
			}
			if exitErr.ExitCode() != exitCodeSessionEnded {
				t.Errorf("exit code = %d, want %d; stderr=%s", exitErr.ExitCode(), exitCodeSessionEnded, stderr.String())
			}
			if !strings.Contains(stderr.String(), "the server ended this session") {
				t.Errorf("stderr = %s\nwant the session-ended log line", stderr.String())
			}
		})
	}
}

// TestExitIfSessionEndedReturnsForOtherErrors: every other error, including an auth rejection and
// an ordinary dead connection, falls through to the caller's existing exit-code logic.
func TestExitIfSessionEndedReturnsForOtherErrors(t *testing.T) {
	for _, err := range []error{nil, session.ErrAuthRejected, session.ErrTokenRejected, sfs.NewDeadConnError(os.ErrClosed)} {
		exitIfSessionEnded(err, nil) // would kill the test binary if it exited
	}
}
