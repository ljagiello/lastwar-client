package app

import (
	"bytes"
	"encoding/json"
	"io"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/session"
	"lastwar-client/internal/shumei"
	"lastwar-client/internal/testutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// shumeiReply is a shumei.Client that answers every device-profile POST with body, counting calls.
type shumeiReply struct {
	body  string
	calls int
}

func (s *shumeiReply) Do(*http.Request) (*http.Response, error) {
	s.calls++
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(s.body)), Header: http.Header{}}, nil
}

const (
	shumeiSmid1 = `{"code":1100,"detail":{"deviceId":"smid-1"},"requestId":"r-1"}`
	shumeiSmid2 = `{"code":1100,"detail":{"deviceId":"smid-2"},"requestId":"r-2"}`
)

// writeTestSessionConfig saves cfg to a fresh session config file and returns its path.
func writeTestSessionConfig(t *testing.T, cfg *SessionConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveSessionConfig(cfg, path); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	return path
}

// sameInstall compares two installs field by field, times with Equal, so one read back from JSON
// matches the one that was saved.
func sameInstall(a, b shumei.Install) bool {
	return a.DeviceID == b.DeviceID && a.AndroidID == b.AndroidID && a.MediaDrmID == b.MediaDrmID &&
		a.PUUID == b.PUUID && a.BoxID == b.BoxID &&
		a.FirstInstallTime.Equal(b.FirstInstallTime) && a.LastUpdateTime.Equal(b.LastUpdateTime) &&
		a.BoxIDSavedAt.Equal(b.BoxIDSavedAt)
}

// TestMintShumeiBoxIDKeepsInstall checks the first -cs-shumei-gen run for a session fills its
// install values, binds them to the run's deviceId and saves them, with the minted box ID, to the
// session config without touching the file's other fields. A later run getting the same smid back
// leaves the file alone; one getting a new smid saves it.
func TestMintShumeiBoxIDKeepsInstall(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{
		DeviceID: "dev-1", AccessToken: "tok-1", RefreshToken: "rt-1", RefreshTokenTime: 42,
	})
	client := &shumeiReply{body: shumeiSmid1}

	boxID, install := mintShumeiBoxID(client, "dev-1", shumei.Device{}, shumei.Install{}, path)
	if boxID != "Bsmid-1" {
		t.Fatalf("boxID = %q, want Bsmid-1", boxID)
	}
	if install.DeviceID != "dev-1" || install.BoxID != "Bsmid-1" || install.BoxIDSavedAt.IsZero() || install.AndroidID == "" || install.PUUID == "" {
		t.Errorf("install = %+v, want filled values bound to dev-1, and the minted box ID with its save time", install)
	}
	saved, err := LoadSessionConfig(path)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if !sameInstall(saved.ShumeiInstall, install) {
		t.Errorf("saved shumeiInstall = %+v, want %+v", saved.ShumeiInstall, install)
	}
	if saved.AccessToken != "tok-1" || saved.RefreshToken != "rt-1" || saved.RefreshTokenTime != 42 || saved.ShumeiBoxId != "" {
		t.Errorf("saving the install changed other fields: %+v", *saved)
	}

	// Same phone, same smid back: nothing changed, so nothing is written. The file is rewritten
	// as compact JSON first; a save would indent it again, as the first one did.
	indented, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(indented, []byte("\n  ")) {
		t.Fatalf("first save wrote %q (read error %v), want indented JSON for the check below to mean anything", indented, err)
	}
	compact, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compact, 0o600); err != nil {
		t.Fatal(err)
	}
	boxID, again := mintShumeiBoxID(client, "dev-1", shumei.Device{}, saved.ShumeiInstall, path)
	if boxID != "Bsmid-1" || !sameInstall(again, install) {
		t.Errorf("second run = %q, %+v; want Bsmid-1 and the install unchanged", boxID, again)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(compact) {
		t.Errorf("second run rewrote the session config (read error %v); want it untouched", err)
	}

	// A new smid, what the live server returns on every registration, is saved with its time.
	client.body = shumeiSmid2
	boxID, next := mintShumeiBoxID(client, "dev-1", shumei.Device{}, again, path)
	if boxID != "Bsmid-2" || next.BoxID != "Bsmid-2" || next.BoxIDSavedAt.Before(install.BoxIDSavedAt) {
		t.Errorf("third run = %q, %+v; want Bsmid-2 saved no earlier than %v", boxID, next, install.BoxIDSavedAt)
	}
	if saved, err := LoadSessionConfig(path); err != nil || !sameInstall(saved.ShumeiInstall, next) {
		t.Errorf("after a new smid the config holds %+v (load error %v), want %+v", saved.ShumeiInstall, err, next)
	}
	if client.calls != 3 {
		t.Errorf("device-profile calls = %d, want 3: every run registers, as the SDK does on each launch", client.calls)
	}
}

// TestMintShumeiBoxIDOtherDeviceID checks one deviceId's install is never sent or saved under
// another. A run whose -cs-deviceid overrides the config's registers a fresh install and leaves the
// config's alone; a config whose deviceId itself changed gets a fresh install saved for it.
func TestMintShumeiBoxIDOtherDeviceID(t *testing.T) {
	_, d1Install := mintShumeiBoxID(&shumeiReply{body: shumeiSmid1}, "dev-1", shumei.Device{}, shumei.Install{}, "")

	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1", ShumeiInstall: d1Install})
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, got := mintShumeiBoxID(&shumeiReply{body: shumeiSmid2}, "dev-2", shumei.Device{}, d1Install, path)
	if got.DeviceID != "dev-2" || got.AndroidID == d1Install.AndroidID || got.PUUID == d1Install.PUUID || got.BoxID != "Bsmid-2" {
		t.Errorf("install for dev-2 = %+v, want a fresh one bound to dev-2, not dev-1's %+v", got, d1Install)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Errorf("the dev-2 run rewrote dev-1's session config (read error %v); want it untouched", err)
	}

	path = writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-2", ShumeiInstall: d1Install})
	_, got = mintShumeiBoxID(&shumeiReply{body: shumeiSmid2}, "dev-2", shumei.Device{}, d1Install, path)
	if saved, err := LoadSessionConfig(path); err != nil || saved.ShumeiInstall.DeviceID != "dev-2" || !sameInstall(saved.ShumeiInstall, got) {
		t.Errorf("config moved to dev-2 holds %+v (load error %v), want the fresh dev-2 install %+v", saved.ShumeiInstall, err, got)
	}
}

// TestMintShumeiBoxIDConfigWithoutDeviceID checks a session config without a top-level deviceId
// still keeps its install, and that the install already there then owns the slot: a run under
// another deviceId can't replace it.
func TestMintShumeiBoxIDConfigWithoutDeviceID(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{AccessToken: "tok-1"})
	_, install := mintShumeiBoxID(&shumeiReply{body: shumeiSmid1}, "dev-1", shumei.Device{}, shumei.Install{}, path)
	saved, err := LoadSessionConfig(path)
	if err != nil || saved.ShumeiInstall.DeviceID != "dev-1" || !sameInstall(saved.ShumeiInstall, install) {
		t.Fatalf("config without a deviceId holds %+v (load error %v), want the dev-1 install %+v", saved.ShumeiInstall, err, install)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, other := mintShumeiBoxID(&shumeiReply{body: shumeiSmid2}, "dev-2", shumei.Device{}, saved.ShumeiInstall, path)
	if other.DeviceID != "dev-2" || other.AndroidID == install.AndroidID {
		t.Errorf("install for dev-2 = %+v, want a fresh one, not dev-1's", other)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(before) {
		t.Errorf("the dev-2 run replaced dev-1's install in a config without a deviceId (read error %v); want it untouched", err)
	}

	// The install's own deviceId keeps saving: a later dev-1 run's new smid is stored.
	_, next := mintShumeiBoxID(&shumeiReply{body: shumeiSmid2}, "dev-1", shumei.Device{}, saved.ShumeiInstall, path)
	if again, err := LoadSessionConfig(path); err != nil || again.ShumeiInstall.BoxID != "Bsmid-2" || !sameInstall(again.ShumeiInstall, next) {
		t.Errorf("after dev-1's next run the config holds %+v (load error %v), want %+v with Bsmid-2", again.ShumeiInstall, err, next)
	}
}

// TestMintShumeiBoxIDFailure checks a failed registration falls back to an empty box ID but
// still saves the install values it filled, so the retry reports the same phone.
func TestMintShumeiBoxIDFailure(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1"})
	boxID, install := mintShumeiBoxID(&shumeiReply{body: `{"code":1902,"detail":{}}`}, "dev-1", shumei.Device{}, shumei.Install{}, path)
	if boxID != "" || install.BoxID != "" {
		t.Errorf("boxID = %q, install.BoxID = %q; want both empty after a 1902", boxID, install.BoxID)
	}
	saved, err := LoadSessionConfig(path)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if saved.ShumeiInstall.AndroidID == "" || !sameInstall(saved.ShumeiInstall, install) {
		t.Errorf("saved shumeiInstall = %+v, want the filled install %+v", saved.ShumeiInstall, install)
	}
}

// TestMintShumeiBoxIDSaveFails checks a session config that can't be written doesn't cost the
// run its box ID: saving is best effort.
func TestMintShumeiBoxIDSaveFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "session.json")
	boxID, install := mintShumeiBoxID(&shumeiReply{body: shumeiSmid1}, "dev-1", shumei.Device{}, shumei.Install{}, path)
	if boxID != "Bsmid-1" || install.BoxID != "Bsmid-1" {
		t.Errorf("mintShumeiBoxID = %q, %+v; want Bsmid-1 despite the failed save", boxID, install)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat %s = %v, want nothing written", path, err)
	}
}

// TestMintShumeiBoxIDNoConfig checks a run without a session config still mints, from a fresh
// install it can't keep.
func TestMintShumeiBoxIDNoConfig(t *testing.T) {
	boxID, install := mintShumeiBoxID(&shumeiReply{body: shumeiSmid1}, "dev-1", shumei.Device{}, shumei.Install{}, "")
	if boxID != "Bsmid-1" || install.AndroidID == "" || install.DeviceID != "dev-1" {
		t.Errorf("mintShumeiBoxID = %q, %+v; want Bsmid-1 and a fresh install for dev-1", boxID, install)
	}
}

// TestMintsShumeiBoxID pins when runCrossServerTest registers, which is also when Run checks the
// session config's shumeiInstall.
func TestMintsShumeiBoxID(t *testing.T) {
	for _, c := range []struct {
		name string
		o    crossServerTestOpts
		want bool
	}{
		{"-cs-shumei-gen", crossServerTestOpts{shumeiGen: true}, true},
		{"no -cs-shumei-gen", crossServerTestOpts{}, false},
		{"shumeiBoxId given", crossServerTestOpts{shumeiGen: true, shumeiBoxId: "Bsmid"}, false},
		{"iOS mode", crossServerTestOpts{shumeiGen: true, iosMode: true}, false},
	} {
		if got := c.o.mintsShumeiBoxID(); got != c.want {
			t.Errorf("%s: mintsShumeiBoxID() = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestRunShumeiInstallGate checks Run refuses a session config whose shumeiInstall a real install
// couldn't hold on exactly the runs that register (-cs-shumei-gen, no shumeiBoxId, not iOS), and
// lets every other run through. Run calls os.Exit, so each case re-executes the test binary as a
// subprocess (see TestMainFlagParseExitCodes). The child points check-version at a closed local
// port, so no case reaches a real server even if the gate regresses. Where Run stopped is read
// off the device-id file: runCrossServerTest creates it first thing, so it exists only for a run
// that got past the gate.
func TestRunShumeiInstallGate(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		gsl.CheckVersionHosts = []string{"http://127.0.0.1:1"}
		os.Args = append([]string{"lastwar-client", "-config", os.Getenv("LASTWAR_TEST_CONFIG")}, strings.Fields(os.Getenv("LASTWAR_TEST_ARGS"))...)
		Run()
		return
	}

	path := writeTestSessionConfig(t, &SessionConfig{
		DeviceID: "dev-1", AccessToken: "tok-1",
		ShumeiInstall: shumei.Install{AndroidID: "NOT-AN-ANDROID-ID"},
	})
	for _, c := range []struct {
		name, args string
		gated      bool
	}{
		{"mints", "-cs-shumei-gen", true},
		{"shumeiBoxId given", "-cs-shumei-gen -cs-shumei Bsmid", false},
		{"iOS mode", "-cs-shumei-gen -cs-ios", false},
		{"no -cs-shumei-gen", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			home := t.TempDir()
			cmd := exec.Command(os.Args[0], "-test.run=^TestRunShumeiInstallGate$")
			cmd.Env = append(os.Environ(), "LASTWAR_TEST_HELPER_PROCESS=1", "LASTWAR_TEST_CONFIG="+path,
				"LASTWAR_TEST_ARGS="+c.args, "HOME="+home, "LASTWAR_STATE_DIR=")
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			runErr := cmd.Run()

			// Every case exits 1: the gated one at the gate, the rest later, for want of an ip.
			if exitErr, ok := runErr.(*exec.ExitError); !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("subprocess = %v, want exit 1; stderr=%s", runErr, stderr.String())
			}
			gated := strings.Contains(stderr.String(), "invalid shumeiInstall")
			_, statErr := os.Stat(filepath.Join(home, ".lastwar_goclient_device_id"))
			pastGate := statErr == nil
			if gated != c.gated || pastGate == c.gated {
				t.Errorf("gate logged %v, run continued past it %v; want gated %v; stderr=%s", gated, pastGate, c.gated, stderr.String())
			}
			if strings.Contains(stderr.String(), "NOT-AN-ANDROID-ID") {
				t.Errorf("stderr = %s, which logs the androidId value itself", stderr.String())
			}
		})
	}
}

// runSaveBack drives runCrossServerTest through a -cs-rt refresh that changes the access token,
// so it saves the session config back (see TestRunCrossServerTestRtRefreshPersistsFreshAccessToken),
// with ShuMei's deviceprofile/v4 answering smid-1. It returns the config as saved.
func runSaveBack(t *testing.T, o crossServerTestOpts) SessionConfig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	gslClient := testutil.UseInMemoryGSL(t, gsl.LoginServerListRespon{
		Code:       "0",
		ServerList: []gsl.LoginServerInfo{{IP: "10.9.8.7", Port: testutil.FlexPort(20443), Zone: "APS-REAL", GameUid: "uid-real"}},
		At:         &gsl.LoginToken{Token: "fresh-access-token"},
	})
	inner := gslClient.Transport
	o.httpClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/deviceprofile/v4") {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(shumeiSmid1)), Header: http.Header{}}, nil
		}
		return inner.RoundTrip(r)
	})}
	o.ip, o.port, o.zone, o.gameUid, o.at, o.rt = "stale-host", 1, "APS-STALE", "uid-stale", "stale-token", "some-refresh-token"

	synctest.Test(t, func(t *testing.T) {
		o.dialGame = session.NewInMemoryGameDial(session.FakeInitPushServer(nil))
		runCrossServerTest(o)
		synctest.Wait()
	})
	saved, err := LoadSessionConfig(o.configSavePath)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if saved.AccessToken != "fresh-access-token" {
		t.Fatalf("saved accessToken = %q, want the refreshed one: the save-back didn't run", saved.AccessToken)
	}
	return *saved
}

// TestRunCrossServerTestSaveBackKeepsMintedInstall checks the redirect/refresh save-back stores
// the install as the mint left it, and keeps the minted box ID out of the top-level shumeiBoxId,
// where it would stop later runs registering.
func TestRunCrossServerTestSaveBackKeepsMintedInstall(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1"})
	saved := runSaveBack(t, crossServerTestOpts{deviceID: "dev-1", shumeiGen: true, configSavePath: path})
	if saved.ShumeiBoxId != "" {
		t.Errorf("saved shumeiBoxId = %q, want it left empty: the minted one belongs in shumeiInstall", saved.ShumeiBoxId)
	}
	if in := saved.ShumeiInstall; in.BoxID != "Bsmid-1" || in.DeviceID != "dev-1" || in.AndroidID == "" {
		t.Errorf("saved shumeiInstall = %+v, want the minted Bsmid-1 on a filled install bound to dev-1", in)
	}
}

// TestRunCrossServerTestShumeiAppVersion checks a -cs-shumei-gen run registers the build its Login
// claims as the fingerprint's app version (a38): the session config's appVersion when it sets one,
// otherwise empty, which shumei.BoxID fills with gsl.AppVersion, the default Login's build.
func TestRunCrossServerTestShumeiAppVersion(t *testing.T) {
	register := registerShumeiDevice
	t.Cleanup(func() { registerShumeiDevice = register })
	for _, appVersion := range []string{"1.0.399", ""} {
		var registered []shumei.Device
		registerShumeiDevice = func(client shumei.Client, deviceID string, dev shumei.Device) (string, error) {
			registered = append(registered, dev)
			return register(client, deviceID, dev)
		}
		path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1"})
		runSaveBack(t, crossServerTestOpts{deviceID: "dev-1", shumeiGen: true, appVersion: appVersion, configSavePath: path})
		if len(registered) != 1 || registered[0].AppVersion != appVersion {
			t.Errorf("appVersion %q: registered %+v, want one device with AppVersion %q", appVersion, registered, appVersion)
		}
	}
}

// TestRunCrossServerTestSaveBackDropsOtherDeviceInstall checks the save-back, which writes this
// run's deviceId, doesn't keep another deviceId's install next to it.
func TestRunCrossServerTestSaveBackDropsOtherDeviceInstall(t *testing.T) {
	d1Install := shumei.Install{DeviceID: "dev-1", AndroidID: "3f9c1e0a7b2d4c58", BoxID: "Bsmid-0", BoxIDSavedAt: time.UnixMilli(1789500000000)}
	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1", ShumeiInstall: d1Install})
	saved := runSaveBack(t, crossServerTestOpts{deviceID: "dev-2", shumeiInstall: d1Install, configSavePath: path})
	if saved.DeviceID != "dev-2" || saved.ShumeiInstall != (shumei.Install{}) {
		t.Errorf("saved deviceId %q with shumeiInstall %+v, want dev-2 with no install", saved.DeviceID, saved.ShumeiInstall)
	}
}

// TestRunCrossServerTestSaveBackWritesNoStubInstall checks a run that never had an install (no
// -cs-shumei-gen here; iOS and a given shumeiBoxId are the same) saves the config back without a
// shumeiInstall, rather than an empty one bound to its deviceId.
func TestRunCrossServerTestSaveBackWritesNoStubInstall(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1"})
	saved := runSaveBack(t, crossServerTestOpts{deviceID: "dev-1", configSavePath: path})
	if saved.ShumeiInstall != (shumei.Install{}) {
		t.Errorf("saved shumeiInstall = %+v, want none", saved.ShumeiInstall)
	}
	if raw, err := os.ReadFile(path); err != nil || bytes.Contains(raw, []byte("shumeiInstall")) {
		t.Errorf("saved config (read error %v) = %s, want no shumeiInstall key", err, raw)
	}
}

// TestRunCrossServerTestSaveBackKeepsUnboundInstall checks a run that doesn't mint still saves back
// an install pasted without a deviceId, bound to the run's, instead of dropping it.
func TestRunCrossServerTestSaveBackKeepsUnboundInstall(t *testing.T) {
	pasted := shumei.Install{AndroidID: "3f9c1e0a7b2d4c58"}
	path := writeTestSessionConfig(t, &SessionConfig{DeviceID: "dev-1", ShumeiInstall: pasted})
	saved := runSaveBack(t, crossServerTestOpts{deviceID: "dev-1", shumeiInstall: pasted, configSavePath: path})
	if saved.ShumeiInstall.AndroidID != pasted.AndroidID || saved.ShumeiInstall.DeviceID != "dev-1" {
		t.Errorf("saved shumeiInstall = %+v, want the pasted androidId bound to dev-1", saved.ShumeiInstall)
	}
}
