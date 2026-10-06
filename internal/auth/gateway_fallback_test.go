package auth

import (
	"errors"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/testutil"
	"slices"
	"testing"
	"time"
)

func TestBaseZoneLoginAddrs(t *testing.T) {
	cases := []struct {
		name, ip string
		want     []string
	}{
		{"single host", "203.0.113.5", []string{"203.0.113.5:10783"}},
		{"fallback list, deduplicated and trimmed", "203.0.113.5| 198.51.100.7 ||203.0.113.5", []string{"203.0.113.5:10783", "198.51.100.7:10783"}},
		{"ipv6 gateway is bracketed", "203.0.113.5|2606:4700:90::1", []string{"203.0.113.5:10783", "[2606:4700:90::1]:10783"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := baseZoneLoginAddrs(c.ip, 10783)
			if err != nil {
				t.Fatalf("baseZoneLoginAddrs(%q): %v", c.ip, err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("baseZoneLoginAddrs(%q) = %v, want %v", c.ip, got, c.want)
			}
		})
	}
	// A malformed first entry still fails, exactly like buildBaseZoneLoginAddr.
	if _, err := baseZoneLoginAddrs("|203.0.113.5", 10783); err == nil {
		t.Error(`baseZoneLoginAddrs("|203.0.113.5"): expected an error for an empty first host`)
	}
}

// TestDoCrossServerLoginFallsBackToNextGateway checks that an unreachable first gateway no longer
// fails the whole run: the login goes through the next host in the configured "|" list, and the
// result reports the address actually used.
func TestDoCrossServerLoginFallsBackToNextGateway(t *testing.T) {
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		if _, err := server.ReadEnvelope(); err != nil {
			return
		}
		resp := sfs.NewSFSObject()
		resp.PutUtfString("zn", "APS1")
		_ = server.SendEnvelope(session.ControllerSystem, session.ActionLogin, resp)
	})
	host, port := testutil.SplitHostPortInt(t, addr)

	const deadHost = "192.0.2.1" // TEST-NET-1, never dialed for real: the fake dialer refuses it
	var dialed []string
	result, err := DoCrossServerLogin(CrossServerLoginParams{
		IP: deadHost + "|" + host, Port: port, Zone: "APS1", GameUid: "uid-1",
		DeviceID: "dev-1", AirKey: "airkey-1", AccessTok: "tok-1",
		DialGame: func(a string, timeout time.Duration) (*session.GameConn, error) {
			dialed = append(dialed, a)
			if a == addr {
				return session.DialGame(a, timeout)
			}
			return nil, errors.New("connection refused")
		},
	})
	if err != nil {
		t.Fatalf("DoCrossServerLogin: %v (dialed %v)", err, dialed)
	}
	defer func() { _ = result.Conn.Close() }()
	if len(dialed) != 2 || dialed[1] != addr {
		t.Errorf("dialed %v, want the dead gateway then %s", dialed, addr)
	}
	if result.Addr != addr {
		t.Errorf("result.Addr = %q, want the gateway that answered, %q", result.Addr, addr)
	}
}
