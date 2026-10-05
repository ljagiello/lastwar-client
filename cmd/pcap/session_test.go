package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lastwar-client/internal/app"
	"lastwar-client/internal/sfs"
)

const (
	testToken  = "secret-access-token-0123456789abcdef"
	testShumei = "secret-shumei-box-id-0123456789"
	testDevice = "secret-device-id_n3d"
)

func framedEnvelope(t *testing.T, c byte, a int16, p *sfs.SFSObject) []byte {
	t.Helper()
	obj := sfs.NewSFSObject()
	obj.PutByte("c", c)
	obj.PutShort("a", a)
	obj.PutSFSObject("p", p)
	body, err := sfs.EncodeObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	framed, err := sfs.EncodePacket(body)
	if err != nil {
		t.Fatal(err)
	}
	return framed
}

// loginCapture writes a capture of one game connection: a real-app-shaped Login request and the
// server's reply (accepted, or rejected with {ec=28, ep=[E011]}).
func loginCapture(t *testing.T, accepted bool) string {
	t.Helper()
	params := sfs.NewSFSObject()
	params.PutUtfString("deviceId", testDevice)
	params.PutUtfString("gameUid", "1000000000000783")
	params.PutUtfString("appVersion", "1.0.344")
	params.PutUtfString("versionCode", "786")
	params.PutUtfString("platform", "0")
	params.PutUtfString("shumeiBoxId", testShumei)
	params.PutUtfString("at", testToken)
	req := sfs.NewSFSObject()
	req.PutUtfString("zn", "APS783")
	req.PutUtfString("un", "1000000000000783")
	req.PutUtfString("pw", "")
	req.PutSFSObject("p", params)

	resp := sfs.NewSFSObject()
	if accepted {
		resp.PutUtfString("zn", "APS783")
		resp.PutShort("rs", 0)
		resp.PutSFSObject("p", sfs.NewSFSObject())
	} else {
		resp.PutValue("ep", sfs.SFSValue{Type: 16, Val: []string{"E011"}}) // 16 = UTF_STRING_ARRAY
		resp.PutShort("ec", 28)
	}

	c2s := framedEnvelope(t, 0, 1, req)
	s2c := framedEnvelope(t, 0, 1, resp)
	const client, server = "192.168.1.9", "203.0.113.5"
	frames := [][]byte{
		ethV4TCP(t, client, server, 5000, 10783, 1000, 0x02, nil),
		ethV4TCP(t, client, server, 5000, 10783, 1001, 0x10, c2s),
		ethV4TCP(t, server, client, 10783, 5000, 7001, 0x10, s2c),
	}
	path := filepath.Join(t.TempDir(), "login.pcap")
	if err := os.WriteFile(path, classicPcap(t, frames...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	fn()
	os.Stdout = orig
	_ = f.Close()
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRunSessionOutWritesAcceptedLogin(t *testing.T) {
	in := loginCapture(t, true)
	dst := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(dst, []byte("{}"), 0o644); err != nil { // an existing, too-open config
		t.Fatal(err)
	}

	var runErr error
	out := captureStdout(t, func() { runErr = runSessionOut(in, -1, "", dst) })
	if runErr != nil {
		t.Fatalf("runSessionOut: %v", runErr)
	}
	for _, secret := range []string{testToken, testShumei, testDevice} {
		if strings.Contains(out, secret) {
			t.Errorf("output leaks a credential (%q): %s", secret, out)
		}
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode != 0o600 {
		t.Errorf("session config mode = %v, want 0600", mode)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got app.SessionConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := app.SessionConfig{
		IP: "203.0.113.5", Port: 10783, Zone: "APS783", GameUid: "1000000000000783",
		DeviceID: testDevice, ShumeiBoxId: testShumei, AccessToken: testToken, IOSMode: true,
		AppVersion: "1.0.344", VersionCode: "786",
	}
	if got != want {
		t.Errorf("session config mismatch:\n got %+v\nwant %+v", got, want) // test-only fake values
	}
}

// TestRunSessionOutRecordsProbedGateways mirrors the real client's connect race: an a=29 probe to a
// second gateway that answered but lost, plus one to a gateway that never answered. Only the one
// that answered joins the fallback list, after the gateway the accepted Login used.
func TestRunSessionOutRecordsProbedGateways(t *testing.T) {
	data, err := os.ReadFile(loginCapture(t, true))
	if err != nil {
		t.Fatal(err)
	}
	ping := sfs.NewSFSObject()
	ping.PutLong("clientTime", 532)
	pong := sfs.NewSFSObject()
	pong.PutLong("serverTime", 1791171496667)
	const client = "192.168.1.9"
	frames := [][]byte{
		ethV4TCP(t, client, "198.51.100.7", 5001, 10783, 2000, 0x02, nil),
		ethV4TCP(t, client, "198.51.100.7", 5001, 10783, 2001, 0x10, framedEnvelope(t, 0, 29, ping)),
		ethV4TCP(t, "198.51.100.7", client, 10783, 5001, 8001, 0x10, framedEnvelope(t, 0, 29, pong)),
		ethV4TCP(t, client, "192.0.2.44", 5002, 10783, 3000, 0x02, nil),
		ethV4TCP(t, client, "192.0.2.44", 5002, 10783, 3001, 0x10, framedEnvelope(t, 0, 29, ping)),
	}
	var extra []byte
	for _, f := range frames {
		rec := make([]byte, 16)
		binary.LittleEndian.PutUint32(rec[8:12], uint32(len(f)))
		binary.LittleEndian.PutUint32(rec[12:16], uint32(len(f)))
		extra = append(append(extra, rec...), f...)
	}
	in := filepath.Join(t.TempDir(), "race.pcap")
	if err := os.WriteFile(in, append(data, extra...), 0o644); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "session.json")
	var runErr error
	captureStdout(t, func() { runErr = runSessionOut(in, -1, "", dst) })
	if runErr != nil {
		t.Fatalf("runSessionOut: %v", runErr)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	var got app.SessionConfig
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if want := "203.0.113.5|198.51.100.7"; got.IP != want {
		t.Errorf("ip = %q, want %q", got.IP, want)
	}
}

// TestAcceptedLoginWithoutShumei: shumeiBoxId is optional (an empty one reconnected live), the
// bearer fields are not.
func TestAcceptedLoginWithoutShumei(t *testing.T) {
	login := func(withToken bool) []byte {
		params := sfs.NewSFSObject()
		params.PutUtfString("deviceId", testDevice)
		params.PutUtfString("gameUid", "1000000000000783")
		params.PutUtfString("platform", "0")
		if withToken {
			params.PutUtfString("at", testToken)
		}
		req := sfs.NewSFSObject()
		req.PutUtfString("zn", "APS783")
		req.PutSFSObject("p", params)
		return framedEnvelope(t, 0, 1, req)
	}
	ok := sfs.NewSFSObject()
	ok.PutShort("rs", 0)
	reply := framedEnvelope(t, 0, 1, ok)

	if cfg, reason := acceptedLogin(login(true), reply); cfg == nil || cfg.ShumeiBoxId != "" {
		t.Errorf("acceptedLogin without shumeiBoxId = %v (%s), want a config with an empty shumeiBoxId", cfg != nil, reason)
	}
	if cfg, reason := acceptedLogin(login(false), reply); cfg != nil || !strings.Contains(reason, "at") {
		t.Errorf("acceptedLogin without at: got config=%v reason=%q, want a rejection naming at", cfg != nil, reason)
	}
}

func TestRunSessionOutRejectsRefusedLogin(t *testing.T) {
	in := loginCapture(t, false)
	dst := filepath.Join(t.TempDir(), "session.json")
	var runErr error
	captureStdout(t, func() { runErr = runSessionOut(in, -1, "", dst) })
	if runErr == nil || !strings.Contains(runErr.Error(), "ec=28") {
		t.Fatalf("runSessionOut error = %v, want a rejected-Login error naming ec=28", runErr)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("session config was written for a rejected Login (stat err = %v)", err)
	}
}

func TestRunSessionOutErrors(t *testing.T) {
	in := loginCapture(t, true)
	dst := filepath.Join(t.TempDir(), "session.json")
	cases := []struct {
		name, in, client, path string
		stream                 int
	}{
		{name: "missing-in", path: dst, stream: -1},
		{name: "bad-stream", in: in, path: dst, stream: 9},
		{name: "bad-client", in: in, client: "nope", path: dst, stream: -1},
		{name: "no-login", in: synthCapture(t), path: dst, stream: -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var runErr error
			captureStdout(t, func() { runErr = runSessionOut(c.in, c.stream, c.client, c.path) })
			if runErr == nil {
				t.Errorf("%s: expected an error, got nil", c.name)
			}
		})
	}
}
