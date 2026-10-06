package app

import (
	"bytes"
	"encoding/json"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
)

// handlerTransport answers HTTP requests in memory through h (no socket, no goroutine), so GSL
// round-trips can run inside a synctest bubble; see testutil.UseInMemoryGSL.
type handlerTransport struct{ h http.Handler }

func (rt handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	rt.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// TestRunCrossServerTestIOSResVersionAndCheckDeviceChange drives the cron path in iOS mode:
// check-version goes out as Android with the session zone (gate host and RSA key), then once more
// as the iOS build for resVersion; the Login carries the iOS-derived resVersion; and
// check.device.change follows init.
func TestRunCrossServerTestIOSResVersionAndCheckDeviceChange(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	pub := testutil.RSAPubKeyDER(t)

	var mu sync.Mutex
	var cvQueries []url.Values
	var gslCalls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(r.URL.Path, "getlsu3dversion.php") {
			gslCalls++
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		cvQueries = append(cvQueries, q)
		body := map[string]any{"resMsg": pub, "updateType": "3", "lwfile2": "826|1", "table_version": "39516,1,x", "locale": "23570,en",
			"hotUpdateMsg": "GameRes,2343,1,1;DllRes,1502,1,1"}
		if q.Get("platform") == "iOS" {
			body["lwfile2"], body["hotUpdateMsg"] = "821|1", "GameRes,2159,1,1;DllRes,1505,1,1"
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	orig := gsl.CheckVersionHosts
	gsl.CheckVersionHosts = []string{"http://gsl.invalid"}
	t.Cleanup(func() { gsl.CheckVersionHosts = orig })

	var loginResVersion string
	var afterInit []string
	dial := session.NewInMemoryGameDial(func(server *session.GameConn) {
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if pv, ok := env.Content.Get("p"); ok {
			if p, ok := pv.Val.(*sfs.SFSObject); ok {
				mu.Lock()
				loginResVersion = p.GetString("resVersion")
				mu.Unlock()
			}
		}
		resp := sfs.NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp); err != nil {
			return
		}
		if err := server.SendExtension("init", sfs.NewSFSObject()); err != nil {
			return
		}
		msg, err := session.ReadNextExtension(server)
		if err != nil {
			return
		}
		mu.Lock()
		afterInit = append(afterInit, msg.Cmd)
		mu.Unlock()
	})

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "gw.invalid", port: 10783, zone: "APS783", gameUid: "uid-1", at: "tok-1", iosMode: true,
			httpClient: &http.Client{Transport: handlerTransport{handler}},
			dialGame:   dial,
		})
		synctest.Wait()
	})

	mu.Lock()
	defer mu.Unlock()
	if loginResVersion != "2159.1505_821F_39516.23570" {
		t.Errorf("Login p.resVersion = %q, want the iOS-derived 2159.1505_821F_39516.23570", loginResVersion)
	}
	if len(cvQueries) != 2 {
		t.Fatalf("check-version calls = %d, want 2 (Android for the gate host, iOS for resVersion)", len(cvQueries))
	}
	if q := cvQueries[0]; q.Get("platform") != "Android" || q.Get("server") != "APS783" || q.Get("table_env") != "table_online" {
		t.Errorf("first check-version = %v, want Android, server=APS783, table_env=table_online", q)
	}
	if q := cvQueries[1]; q.Get("platform") != "iOS" || q.Get("packageName") != "com.lastwar.ios" || q.Get("appVersion") != "1.0.344" ||
		q.Get("buildId") != "786" || q.Get("server") != "APS783" || q.Get("uid") != "" || q.Get("deviceId") != "" {
		t.Errorf("second check-version = %v, want the anonymous iOS 1.0.344/786 query", q)
	}
	if gslCalls != 0 {
		t.Errorf("getserverlist.php calls = %d, want 0 without -cs-rt", gslCalls)
	}
	if len(afterInit) != 1 || afterInit[0] != "check.device.change" {
		t.Errorf("first command after init = %v, want check.device.change", afterInit)
	}
}

// runReauthSubprocess re-runs this test binary as a subprocess with helperEnv set and returns its
// exit code and stderr; the subprocess side calls os.Exit through the code under test.
func runReauthSubprocess(t *testing.T, testName, helperEnv string) (int, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+testName+"$")
	cmd.Env = append(os.Environ(), helperEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not exit with an error: err=%v stderr=%s", err, stderr.String())
	}
	return exitErr.ExitCode(), stderr.String()
}

// TestRunCrossServerTestRefreshReauthNeededExits2 covers MASTER §5.1 #4 on the -cs-rt path: GSL
// code 211 exits 2 with a re-auth message, even though the reply carries a server list.
func TestRunCrossServerTestRefreshReauthNeededExits2(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_REAUTH_CSRT") == "1" {
		t.Setenv("HOME", t.TempDir())
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
			Code:       "211",
			ServerList: []gsl.LoginServerInfo{{IP: "127.0.0.1", Port: "1", Zone: "APS1", GameUid: "uid-1"}},
		}))
		runCrossServerTest(crossServerTestOpts{ip: "1.2.3.4", port: 18888, gameUid: "uid-1", rt: "some-refresh-token"})
		return
	}
	code, stderr := runReauthSubprocess(t, "TestRunCrossServerTestRefreshReauthNeededExits2", "LASTWAR_TEST_HELPER_REAUTH_CSRT")
	if code != 2 || !strings.Contains(stderr, "re-auth needed") {
		t.Errorf("exit code %d, want 2 with a re-auth message; stderr=%s", code, stderr)
	}
}

// TestRunCrossServerTestRefreshOtherGSLCodeExits1: any other non-zero code is a plain failure.
func TestRunCrossServerTestRefreshOtherGSLCodeExits1(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_GSL201_CSRT") == "1" {
		t.Setenv("HOME", t.TempDir())
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{Code: "201"}))
		runCrossServerTest(crossServerTestOpts{ip: "1.2.3.4", port: 18888, gameUid: "uid-1", rt: "some-refresh-token"})
		return
	}
	code, stderr := runReauthSubprocess(t, "TestRunCrossServerTestRefreshOtherGSLCodeExits1", "LASTWAR_TEST_HELPER_GSL201_CSRT")
	if code != 1 || !strings.Contains(stderr, "GSL refresh rejected") || strings.Contains(stderr, "re-auth needed") {
		t.Errorf("exit code %d, want 1 with \"GSL refresh rejected\" and no re-auth message; stderr=%s", code, stderr)
	}
}

// TestMainLoginReauthNeededExits2 covers MASTER §5.1 #4 on the main guest/email path: GSL code 212
// from Login() exits 2 with a re-auth message.
func TestMainLoginReauthNeededExits2(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_REAUTH_MAIN") == "1" {
		t.Setenv("HOME", t.TempDir())
		testutil.UseFakeGSLServer(t, testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{Code: "212"}))
		os.Args = []string{"lastwar-client", "-no-config"}
		Run()
		return
	}
	code, stderr := runReauthSubprocess(t, "TestMainLoginReauthNeededExits2", "LASTWAR_TEST_HELPER_REAUTH_MAIN")
	if code != 2 || !strings.Contains(stderr, "re-auth needed") {
		t.Errorf("exit code %d, want 2 with a re-auth message; stderr=%s", code, stderr)
	}
}
