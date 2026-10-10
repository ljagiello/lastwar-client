package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"lastwar-client/internal/auth"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The game server a lookup returns in these tests: the zone's three gateways on its current port.
const (
	lookupGateways = "gw-a.example|gw-b.example|gw-c.example"
	lookupPort     = 10783
	lookupZone     = "APS783"
	lookupGameUid  = "uid-1"
)

func lookupReply() gsl.LoginServerListRespon {
	return gsl.LoginServerListRespon{
		Code: "0",
		ServerList: []gsl.LoginServerInfo{
			{ID: "783", IP: lookupGateways, Port: testutil.FlexPort(lookupPort), Zone: lookupZone, GameUid: lookupGameUid},
		},
	}
}

// countGetServerList wraps client's transport to count getserverlist.php requests.
func countGetServerList(client *http.Client) func() int {
	var mu sync.Mutex
	n := 0
	base := client.Transport
	client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "getserverlist.php") {
			mu.Lock()
			n++
			mu.Unlock()
		}
		return base.RoundTrip(r)
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// recordingDial serves every address with an in-memory game server: handlers[addr] when present,
// else FakeInitPushServer, and records the addresses dialed. A nil handler refuses the dial the way
// an unreachable gateway does.
func recordingDial(handlers map[string]func(*session.GameConn)) (dial func(string, time.Duration) (*session.GameConn, error), dialed func() []string) {
	var mu sync.Mutex
	var addrs []string
	return func(addr string, timeout time.Duration) (*session.GameConn, error) {
			mu.Lock()
			addrs = append(addrs, addr)
			mu.Unlock()
			handler, ok := handlers[addr]
			if !ok {
				handler = session.FakeInitPushServer(nil)
			} else if handler == nil {
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			}
			return session.NewInMemoryGameDial(handler)(addr, timeout)
		}, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), addrs...)
		}
}

func readSessionConfig(t *testing.T, path string) SessionConfig {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("session config was not written to %s: %v", path, err)
	}
	var cfg SessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse persisted session config: %v", err)
	}
	return cfg
}

// TestRunCrossServerTestLooksUpMissingAddress: a session with no ip/port asks GSL getserverlist for
// the role's game server, dials its first gateway, logs in with the zone the lookup named, and
// persists the whole gateway list and port, keeping the configured access token.
func TestRunCrossServerTestLooksUpMissingAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	lookups := countGetServerList(client)
	zoneSeen := make(chan string, 1)
	dial, dialed := recordingDial(map[string]func(*session.GameConn){
		"gw-a.example:10783": session.FakeInitPushServer(zoneSeen),
	})
	cfgPath := t.TempDir() + "/session.json"

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			zone: "APS1", gameUid: lookupGameUid, at: "at-1",
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if n := lookups(); n != 1 {
		t.Errorf("getserverlist requests = %d, want 1", n)
	}
	if got := dialed(); len(got) != 1 || got[0] != "gw-a.example:10783" {
		t.Errorf("dialed %v, want [gw-a.example:10783]", got)
	}
	if z := <-zoneSeen; z != lookupZone {
		t.Errorf("Login zone = %q, want the looked-up %q", z, lookupZone)
	}
	cfg := readSessionConfig(t, cfgPath)
	if cfg.IP != lookupGateways || cfg.Port != lookupPort || cfg.Zone != lookupZone {
		t.Errorf("persisted %s %s:%d, want %s %s:%d", cfg.Zone, cfg.IP, cfg.Port, lookupZone, lookupGateways, lookupPort)
	}
	if cfg.AccessToken != "at-1" || cfg.GameUid != lookupGameUid {
		t.Errorf("persisted accessToken/gameUid changed: tokenLen=%d gameUid=%q", len(cfg.AccessToken), cfg.GameUid)
	}
}

// TestRunCrossServerTestDoesNotLookUpAWorkingAddress: a configured address that answers needs no
// GSL call, so a working cron run sends exactly what it sent before the lookup existed.
func TestRunCrossServerTestDoesNotLookUpAWorkingAddress(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	client := testutil.UseInMemoryGSL(t, lookupReply())
	lookups := countGetServerList(client)
	dial, dialed := recordingDial(nil)
	cfgPath := t.TempDir() + "/session.json"

	synctest.Test(t, func(t *testing.T) {
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			configSavePath: cfgPath, httpClient: client, dialGame: dial,
		})
		synctest.Wait()
	})

	if n := lookups(); n != 0 {
		t.Errorf("getserverlist requests = %d, want 0", n)
	}
	if got := dialed(); len(got) != 1 || got[0] != "10.0.0.1:17783" {
		t.Errorf("dialed %v, want [10.0.0.1:17783]", got)
	}
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("session config written although nothing changed (stat err = %v)", err)
	}
}

// TestRunCrossServerTestLooksUpAfterUnreachableAddress: when the configured address refuses every
// dial, or accepts the connection but never answers the Login (the August 2026 port move), one GSL
// lookup finds the current server, the Login is retried there, and the new address is persisted.
func TestRunCrossServerTestLooksUpAfterUnreachableAddress(t *testing.T) {
	cases := map[string]func(*session.GameConn){
		"dial refused":     nil,
		"login unanswered": func(*session.GameConn) {}, // the drain reads the Login and never replies
	}
	for name, stale := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			client := testutil.UseInMemoryGSL(t, lookupReply())
			lookups := countGetServerList(client)
			dial, dialed := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": stale})
			cfgPath := t.TempDir() + "/session.json"

			synctest.Test(t, func(t *testing.T) {
				runCrossServerTest(crossServerTestOpts{
					ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
					configSavePath: cfgPath, httpClient: client, dialGame: dial,
				})
				synctest.Wait()
			})

			if n := lookups(); n != 1 {
				t.Errorf("getserverlist requests = %d, want 1", n)
			}
			want := []string{"10.0.0.1:17783", "gw-a.example:10783"}
			if got := dialed(); fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("dialed %v, want %v", got, want)
			}
			cfg := readSessionConfig(t, cfgPath)
			if cfg.IP != lookupGateways || cfg.Port != lookupPort {
				t.Errorf("persisted %s:%d, want %s:%d", cfg.IP, cfg.Port, lookupGateways, lookupPort)
			}
		})
	}
}

// TestRunCrossServerTestNoLookupOnTokenRejection: an E011 rejection comes from a server that
// answered, so another address can't help, and a GSL call is the last thing a token problem should
// trigger. The run exits 2 without looking anything up. Re-executes the test binary because
// runCrossServerTest exits the process on this path.
func TestRunCrossServerTestNoLookupOnTokenRejection(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, lookupReply())
		reject := func(server *session.GameConn) {
			if _, err := server.ReadEnvelope(); err != nil {
				return
			}
			resp := sfs.NewSFSObject()
			resp.PutValue("ep", sfs.SFSValue{Type: 16, Val: []string{"E011"}}) // 16 = UTF_STRING_ARRAY
			resp.PutShort("ec", 28)
			_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
		}
		dial, _ := recordingDial(map[string]func(*session.GameConn){"10.0.0.1:17783": reject})
		runCrossServerTest(crossServerTestOpts{
			ip: "10.0.0.1", port: 17783, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			httpClient: client, dialGame: dial,
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoLookupOnTokenRejection$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 2 {
		t.Errorf("subprocess exit code = %d, want 2; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	if strings.Contains(stderr.String(), "GSL getserverlist") {
		t.Errorf("an E011 rejection triggered a GSL lookup; stderr=%s", stderr.String())
	}
}

// TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer: when GSL lists the very address that
// just failed, a second dial can't help, so the run reports the original error and exits 1 after
// one dial. Re-executes the test binary because runCrossServerTest exits the process on this path.
func TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		t.Setenv("HOME", t.TempDir())
		client := testutil.UseInMemoryGSL(t, lookupReply())
		// The configured gateways are the looked-up ones in another order, and all refuse.
		runCrossServerTest(crossServerTestOpts{
			ip: "gw-c.example|gw-b.example|gw-a.example", port: lookupPort, zone: lookupZone, gameUid: lookupGameUid, at: "at-1",
			httpClient: client, dialGame: func(addr string, _ time.Duration) (*session.GameConn, error) {
				fmt.Fprintf(os.Stderr, "dial %s\n", addr)
				return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
			},
		})
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestRunCrossServerTestNoRetryWhenLookupNamesTheSameServer$")
	cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("subprocess did not fail as expected: err=%v, stderr=%s", runErr, stderr.String())
	}
	if exitErr.ExitCode() != 1 {
		t.Errorf("subprocess exit code = %d, want 1; stderr=%s", exitErr.ExitCode(), stderr.String())
	}
	log := stderr.String()
	if !strings.Contains(log, "GSL lists the game server that just failed; not retrying") {
		t.Errorf("want the not-retrying warning; stderr=%s", log)
	}
	// One pass over the configured gateways, none after the lookup.
	if n := strings.Count(log, "dial gw-"); n != 3 {
		t.Errorf("dialed %d times, want 3 (one pass over the configured gateways); stderr=%s", n, log)
	}
}

func TestConnectFailed(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"dial refused", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}, true},
		{"every gateway refused", errors.Join(&net.OpError{Op: "dial"}, &net.OpError{Op: "dial"}), true},
		{"login unanswered", session.DeadlineExceededError{}, true},
		{"connection dropped", session.SendStageError{Err: errors.New("broken pipe")}, true},
		{"auth rejected", fmt.Errorf("CROSS-SERVER LOGIN FAILED: %w", session.ErrAuthRejected), false},
		{"token rejected", fmt.Errorf("CROSS-SERVER LOGIN FAILED: %w", session.ErrTokenRejected), false},
		{"kicked", &session.SessionEndedError{Err: session.ErrKicked}, false},
		{"plain error", errors.New("cross-server login: no access token given"), false},
	}
	for _, c := range cases {
		if got := connectFailed(c.err); got != c.want {
			t.Errorf("%s: connectFailed = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSameGameServer(t *testing.T) {
	srv := auth.GameServer{IP: "a|b", Port: 10783, Zone: "APS783"}
	cases := []struct {
		name string
		ip   string
		port int
		zone string
		want bool
	}{
		{"same", "a|b", 10783, "APS783", true},
		{"same hosts, other order", "b|a", 10783, "APS783", true},
		{"other port", "a|b", 17783, "APS783", false},
		{"other zone", "a|b", 10783, "APS8092", false},
		{"captured IPs vs hostnames", "1.2.3.4|5.6.7.8", 10783, "APS783", false},
	}
	for _, c := range cases {
		if got := sameGameServer(srv, c.ip, c.port, c.zone); got != c.want {
			t.Errorf("%s: sameGameServer = %v, want %v", c.name, got, c.want)
		}
	}
}
