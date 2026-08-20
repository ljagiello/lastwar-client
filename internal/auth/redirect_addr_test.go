package auth

import (
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"strings"
	"testing"
)

// TestBuildBaseZoneLoginAddrEmptyIP is the regression test for buildBaseZoneLoginAddr's empty-ip
// guard (login.go): an empty ip must produce a clear error rather than silently building a
// ":<port>"-shaped address, which Go's "host:port" dial syntax treats as the loopback interface
// (see main.go's equivalent gsl.FirstHost(ip) == "" guard on the cross-server login path, which this
// mirrors). Exercised directly against the small helper Login() calls -- rather than through a
// full Login() integration test with fake GSL/game servers -- since this is a pure function of its
// two arguments and doesn't need any network fakery to prove the guard fires.
func TestBuildBaseZoneLoginAddrEmptyIP(t *testing.T) {
	_, err := buildBaseZoneLoginAddr("", 9339)
	if err == nil {
		t.Fatal("buildBaseZoneLoginAddr(\"\", 9339): expected an error for an empty ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
}

// TestBuildBaseZoneLoginAddrNonEmptyIP is TestBuildBaseZoneLoginAddrEmptyIP's happy-path
// counterpart: a normal, non-empty ip must still build the expected "host:port" address and return
// no error, confirming the new guard doesn't reject valid input.
func TestBuildBaseZoneLoginAddrNonEmptyIP(t *testing.T) {
	addr, err := buildBaseZoneLoginAddr("203.0.113.5", 9339)
	if err != nil {
		t.Fatalf("buildBaseZoneLoginAddr: unexpected error for a valid ip: %v", err)
	}
	if want := "203.0.113.5:9339"; addr != want {
		t.Errorf("addr = %q, want %q", addr, want)
	}
}

// TestBuildBaseZoneLoginAddrFirstOfFallbackList confirms buildBaseZoneLoginAddr's guard checks
// gsl.FirstHost's result (the "|"-delimited list entry actually used to dial), not the raw ip string --
// a pipe-delimited list starting with an empty entry must still be caught, not let through just
// because the raw string itself is non-empty.
func TestBuildBaseZoneLoginAddrFirstOfFallbackList(t *testing.T) {
	if _, err := buildBaseZoneLoginAddr("|203.0.113.5", 9339); err == nil {
		t.Error("buildBaseZoneLoginAddr(\"|203.0.113.5\", 9339): expected an error (gsl.FirstHost of this list is empty), got nil")
	}
	addr, err := buildBaseZoneLoginAddr("203.0.113.5|198.51.100.7", 9339)
	if err != nil {
		t.Fatalf("buildBaseZoneLoginAddr: unexpected error: %v", err)
	}
	if want := "203.0.113.5:9339"; addr != want {
		t.Errorf("addr = %q, want %q (first entry of the fallback list)", addr, want)
	}
}

// TestBuildBaseZoneLoginAddrZeroPort is the round-19 regression test for
// buildBaseZoneLoginAddr's port guard: a zero (or negative) port must produce a clear error
// rather than silently building a "host:0"-shaped address. Mirrors
// TestBuildBaseZoneLoginAddrEmptyIP's structure for the port half of the same guard function.
func TestBuildBaseZoneLoginAddrZeroPort(t *testing.T) {
	_, err := buildBaseZoneLoginAddr("203.0.113.5", 0)
	if err == nil {
		t.Fatal("buildBaseZoneLoginAddr(\"203.0.113.5\", 0): expected an error for a zero port, got nil")
	}
	if strings.Contains(err.Error(), "203.0.113.5:0") {
		t.Errorf("err = %q, must not contain a \"host:0\"-shaped address (that's the footgun this guard exists to prevent)", err.Error())
	}
}

// TestLoginRedirectRejectsEmptyRedirectIP is the round-18 regression test for the same
// gsl.FirstHost-without-emptiness-check gap crossserver_test.go's
// TestDoCrossServerLoginRedirectRejectsEmptyRedirectIP covers on the DoCrossServerLogin side:
// Login()'s own serverInfo redirect branch only checked siObj.GetString("ip") != "", not
// gsl.FirstHost's resolved result -- so a pipe-malformed ip like "|1.2.3.4" (raw non-empty, but
// gsl.FirstHost resolves it down to "") built a ":<port>"-shaped dial address via a raw fmt.Sprintf,
// which Go's "host:port" dial syntax silently treats as the loopback interface, instead of
// failing clearly. Reuses login_integration_test.go's fake-GSL/fake-game-server infrastructure
// (testutil.NewFakeGSLServer/testutil.UseFakeGSLServer) and crossserver_test.go's fake game listener helpers
// (session.StartFakeGameServer/testutil.SplitHostPortInt) -- all in this same package -- to drive a real Login()
// call through a successful initial dial and into the redirect branch. Proves Login() now returns
// a clear error (routed through buildBaseZoneLoginAddr, same as the initial dial) instead of
// attempting the loopback dial.
func TestLoginRedirectRejectsEmptyRedirectIP(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldAddr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := sfs.NewSFSObject()
		si.PutUtfString("ip", "|1.2.3.4") // gsl.FirstHost("|1.2.3.4") == "" -- the malformed case
		si.PutInt("port", 9339)
		si.PutUtfString("zone", "APS2")
		resp := sfs.NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
	})
	oldHost, oldPort := testutil.SplitHostPortInt(t, oldAddr)

	gsl := testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(oldHost), Port: testutil.FlexPort(oldPort), Zone: "APS1", GameUid: "uid-1"}},
		At:         &gsl.LoginToken{Token: "tok-1"},
	})
	testutil.UseFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err == nil {
		if result != nil && result.Conn != nil {
			_ = result.Conn.Close()
		}
		t.Fatal("expected an error for a pipe-malformed redirect ip, got nil")
	}
	if strings.Contains(err.Error(), ":9339") {
		t.Errorf("err = %q, must not contain a \":<port>\"-shaped address (that's the loopback-dial footgun this guard exists to prevent)", err.Error())
	}
	if !strings.Contains(err.Error(), "serverInfo redirect") {
		t.Errorf("err = %q, want it to mention the serverInfo redirect context", err.Error())
	}
}

// TestLoginRedirectRejectsMissingRedirectPort is the round-19 counterpart to
// TestLoginRedirectRejectsEmptyRedirectIP, covering the port half of buildBaseZoneLoginAddr's
// guard instead of the host half: a serverInfo redirect payload that omits `port` entirely (the
// same shape gsl.go's getIntFlexible silently resolves to 0 for, whether the field is absent or
// present-but-unparseable) must make Login() return a clear error, not silently build and dial a
// "host:0"-shaped address. Mirrors TestLoginRedirectRejectsEmptyRedirectIP's fake-GSL/fake-game-
// server setup, just omitting the `port` field on the serverInfo payload instead of malforming
// `ip`.
func TestLoginRedirectRejectsMissingRedirectPort(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldAddr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		si := sfs.NewSFSObject()
		si.PutUtfString("ip", "203.0.113.9")
		// No "port" field at all -- getIntFlexible(si, "port") resolves this to 0, same as an
		// unparseable port value would.
		si.PutUtfString("zone", "APS2")
		resp := sfs.NewSFSObject()
		resp.PutSFSObject("serverInfo", si)
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
	})
	oldHost, oldPort := testutil.SplitHostPortInt(t, oldAddr)

	gsl := testutil.NewFakeGSLServer(t, gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: gsl.FlexString(oldHost), Port: testutil.FlexPort(oldPort), Zone: "APS1", GameUid: "uid-1"}},
		At:         &gsl.LoginToken{Token: "tok-1"},
	})
	testutil.UseFakeGSLServer(t, gsl)

	result, err := Login(LoginOptions{})
	if err == nil {
		if result != nil && result.Conn != nil {
			_ = result.Conn.Close()
		}
		t.Fatal("expected an error for a redirect payload with a missing port, got nil")
	}
	if strings.Contains(err.Error(), "203.0.113.9:0") {
		t.Errorf("err = %q, must not contain a \"host:0\"-shaped address (that's the footgun this guard exists to prevent)", err.Error())
	}
	if !strings.Contains(err.Error(), "serverInfo redirect") {
		t.Errorf("err = %q, want it to mention the serverInfo redirect context", err.Error())
	}
}
