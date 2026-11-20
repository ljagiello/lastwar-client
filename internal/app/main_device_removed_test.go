package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
)

// deviceRemovedServer accepts the Login and then, while the run waits for init, pushes
// push.account.device.del naming the deviceId that Login sent (PushAccountDeviceDelMessage.lua:
// 8-25), or another device when other is set.
func deviceRemovedServer(other bool) func(*session.GameConn) {
	return func(server *session.GameConn) {
		var deviceID string
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			if env.Controller == session.ControllerSystem && env.Action == session.ActionLogin {
				if pv, ok := env.Content.Get("p"); ok {
					if p, ok := pv.Val.(*sfs.SFSObject); ok {
						deviceID = p.GetString("deviceId")
					}
				}
				break
			}
		}
		if !sendLoginOK(server) {
			return
		}
		push := sfs.NewSFSObject()
		if other {
			push.PutUtfString("deviceId", deviceID+"-other")
		} else {
			push.PutUtfString("deviceId", deviceID)
		}
		_ = server.SendExtension("push.account.device.del", push)
		if other {
			_ = server.SendExtension("init", sfs.NewSFSObject())
		}
		for {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
		}
	}
}

// TestRunCrossServerTestExitsCode2WhenTheDeviceIsRemoved is WP-Z touch point 9 (MASTER B66): a
// push.account.device.del naming this run's device ends the session for good, so the run exits 2,
// the "session is stale, bind or recapture" code, not 3 (session.ErrDeviceRemoved wraps
// ErrSessionEnded and ErrAuthRejected). A push naming another device is not terminal.
func TestRunCrossServerTestExitsCode2WhenTheDeviceIsRemoved(t *testing.T) {
	if mode := os.Getenv("LASTWAR_TEST_DEVICE_REMOVED"); mode != "" {
		t.Setenv("HOME", t.TempDir())
		gameAddr := session.StartFakeGameServer(t, deviceRemovedServer(mode == "other"))
		gameHost, gamePort := testutil.SplitHostPortInt(t, gameAddr)
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{Code: "0"}))
		runCrossServerTest(crossServerTestOpts{ip: gameHost, port: gamePort, zone: "APS1", gameUid: "uid-1", at: "tok-1"})
		return // only reached if runCrossServerTest did not exit
	}
	for _, c := range []struct {
		mode     string
		wantCode int
		wantLog  string
	}{
		{"own", 2, "removed from the account"}, // the documented code, not the constant
		{"other", 0, ""},
	} {
		t.Run(c.mode, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestExitsCode2WhenTheDeviceIsRemoved$")
			cmd.Env = append(os.Environ(), "LASTWAR_TEST_DEVICE_REMOVED="+c.mode)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			err := cmd.Run()
			code := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				code = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("subprocess: %v", err)
			}
			if code != c.wantCode {
				t.Errorf("exit code = %d, want %d; stderr=%s", code, c.wantCode, stderr.String())
			}
			if c.wantLog != "" && !strings.Contains(stderr.String(), c.wantLog) {
				t.Errorf("stderr = %s\nwant %q", stderr.String(), c.wantLog)
			}
		})
	}
}

// TestExitCodesMatchTheREADME pins each exit code to the number README.md ("Exit code 2" to "Exit
// code 5") gives cron wrappers, so a changed constant fails here and not only in the docs.
func TestExitCodesMatchTheREADME(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want int
	}{
		{"device removed (bind again or recapture)", exitCodeDeviceRemoved, 2},
		{"session ended by the server", exitCodeSessionEnded, 3},
		{"account, role, device or IP blocked", exitCodeAccountBlocked, 4},
		{"forced update", exitCodeForcedUpdate, 5},
	} {
		if c.got != c.want {
			t.Errorf("%s: exit code %d, README documents %d", c.name, c.got, c.want)
		}
	}
}

// TestExitIfSessionEndedDeviceRemovedIsNotCode3 pins the order inside exitIfSessionEnded: the
// device-removed check comes first, so the error never reaches the code-3 exit.
func TestExitIfSessionEndedDeviceRemovedIsNotCode3(t *testing.T) {
	err := fmt.Errorf("wait: %w", session.ErrDeviceRemoved)
	if !errors.Is(err, session.ErrSessionEnded) || !errors.Is(err, session.ErrAuthRejected) {
		t.Fatalf("ErrDeviceRemoved must wrap ErrSessionEnded and ErrAuthRejected, as WP1 defines it")
	}
	if exitCodeDeviceRemoved == exitCodeSessionEnded {
		t.Errorf("exitCodeDeviceRemoved = %d, the session-ended code", exitCodeDeviceRemoved)
	}
}
