package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionConfigStringGoStringRedact is the round-48 regression test for the MAJOR finding that
// SessionConfig -- whose own doc comment calls AccessToken "a real access token" -- had no
// String()/GoString() redaction, unlike gsl.go's LoginToken (round 47). Proves both the bare value
// and, critically, a SessionConfig nested inside a containing value's %+v are redacted, and that
// the actual JSON persistence path (SaveSessionConfig/LoadSessionConfig) still round-trips the
// real, unredacted token -- confirming String()/GoString() don't interfere with encoding/json the
// way a MarshalJSON override would have (see this fix's own doc comment in config.go for why
// MarshalJSON was deliberately NOT added).
func TestSessionConfigStringGoStringRedact(t *testing.T) {
	const liveToken = "FAKE-LIVE-SESSION-ACCESS-TOKEN-must-not-leak-abc123"
	cfg := SessionConfig{IP: "203.0.113.9", Port: 9339, Zone: "APS1", GameUid: "uid-1", AccessToken: liveToken}

	t.Run("String", func(t *testing.T) {
		s := cfg.String()
		if strings.Contains(s, liveToken) {
			t.Errorf("String() = %q, must not contain the live token", s)
		}
	})
	t.Run("GoString", func(t *testing.T) {
		s := cfg.GoString()
		if strings.Contains(s, liveToken) {
			t.Errorf("GoString() = %q, must not contain the live token", s)
		}
	})
	t.Run("nested inside a containing value's %+v", func(t *testing.T) {
		wrapper := struct{ Cfg SessionConfig }{Cfg: cfg}
		formatted := fmt.Sprintf("%+v", wrapper)
		if strings.Contains(formatted, liveToken) {
			t.Errorf("fmt.Sprintf(%%+v, wrapper) = %q, must not contain the live token nested in .Cfg", formatted)
		}
	})
	t.Run("JSON round trip still carries the real token", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "session.json")
		if err := SaveSessionConfig(&cfg, path); err != nil {
			t.Fatalf("SaveSessionConfig: %v", err)
		}
		loaded, err := LoadSessionConfig(path)
		if err != nil {
			t.Fatalf("LoadSessionConfig: %v", err)
		}
		if loaded.AccessToken != liveToken {
			t.Errorf("loaded.AccessToken = %q, want the real token %q -- String()/GoString() must not corrupt the actual JSON persistence path", loaded.AccessToken, liveToken)
		}
	})
}

func TestApplyOverride(t *testing.T) {
	if got := applyOverride("base", "override"); got != "override" {
		t.Errorf("applyOverride(base, override) = %q, want %q", got, "override")
	}
	if got := applyOverride("base", ""); got != "base" {
		t.Errorf("applyOverride(base, \"\") = %q, want %q", got, "base")
	}
}

// TestMergeExplicitOrConfigString is the round-33 regression test for the MAJOR finding that
// main()'s -cs-ip/-cs-gameuid config-merge used to silently replace an explicitly-passed-but-empty
// flag with the session config's value, with zero diagnostic -- contradicting -config's own
// documented override contract and defeating the dedicated "given but empty" diagnostics further
// down in runCrossServerTest entirely (by the time those ran, the flag already held the config's
// non-empty value). mergeExplicitOrConfigString is the extracted, directly-testable decision logic
// behind that fix.
func TestMergeExplicitOrConfigString(t *testing.T) {
	cases := []struct {
		name                string
		flagVal             string
		explicit            bool
		cfgVal              string
		wantEffective       string
		wantExplicitlyEmpty bool
	}{
		{"never mentioned, empty, config has a value: falls back to config", "", false, "cfg-ip", "cfg-ip", false},
		{"never mentioned, empty, config also empty: stays empty", "", false, "", "", false},
		{"explicitly set to a real value: kept, config ignored", "flag-ip", true, "cfg-ip", "flag-ip", false},
		{"explicitly set but empty: stays empty, does NOT fall back to config", "", true, "cfg-ip", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEffective, gotExplicitlyEmpty := mergeExplicitOrConfigString(c.flagVal, c.explicit, c.cfgVal)
			if gotEffective != c.wantEffective {
				t.Errorf("effective = %q, want %q", gotEffective, c.wantEffective)
			}
			if gotExplicitlyEmpty != c.wantExplicitlyEmpty {
				t.Errorf("explicitlyEmpty = %v, want %v", gotExplicitlyEmpty, c.wantExplicitlyEmpty)
			}
		})
	}
}

// TestMergeExplicitOrConfigPort is mergeExplicitOrConfigPort's TestMergeExplicitOrConfigString
// counterpart -- same round-33 finding, same fix, applied to -cs-port's int-vs-"not given" shape
// instead of a string's empty-vs-"not given" shape.
func TestMergeExplicitOrConfigPort(t *testing.T) {
	cases := []struct {
		name               string
		flagVal            int
		explicit           bool
		cfgVal             int
		wantEffective      int
		wantExplicitlyZero bool
	}{
		{"never mentioned, zero, config has a value: falls back to config", 0, false, 9999, 9999, false},
		{"never mentioned, zero, config also zero: stays zero", 0, false, 0, 0, false},
		{"explicitly set to a real value: kept, config ignored", 1234, true, 9999, 1234, false},
		{"explicitly set to 0: stays zero, does NOT fall back to config", 0, true, 9999, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotEffective, gotExplicitlyZero := mergeExplicitOrConfigPort(c.flagVal, c.explicit, c.cfgVal)
			if gotEffective != c.wantEffective {
				t.Errorf("effective = %d, want %d", gotEffective, c.wantEffective)
			}
			if gotExplicitlyZero != c.wantExplicitlyZero {
				t.Errorf("explicitlyZero = %v, want %v", gotExplicitlyZero, c.wantExplicitlyZero)
			}
		})
	}
}

// TestMergeExplicitOrConfigBool is the round-35 regression test for the MAJOR finding that -cs-ios
// (main.go), the one field in the SessionConfig merge family left as an inline `if
// !csIOSSetExplicitly { *csIOS = cfg.IOSMode }` block, had zero test coverage of its merge
// decision -- confirmed via mutation testing (inverting the condition passed the full suite
// unchanged). Now extracted into this pure function, mirroring mergeExplicitOrConfigString/Port's
// own extraction, so the decision itself is directly testable independent of main()'s wiring.
func TestMergeExplicitOrConfigBool(t *testing.T) {
	cases := []struct {
		name     string
		flagVal  bool
		explicit bool
		cfgVal   bool
		want     bool
	}{
		{"never mentioned, flag default false, config true: falls back to config", false, false, true, true},
		{"never mentioned, flag default false, config false: stays false", false, false, false, false},
		{"explicitly set to true: kept, config ignored (even if config is false)", true, true, false, true},
		{"explicitly set to false: kept, config ignored (even if config is true) -- false is a legitimate explicit choice, not an absent one", false, true, true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := mergeExplicitOrConfigBool(c.flagVal, c.explicit, c.cfgVal)
			if got != c.want {
				t.Errorf("mergeExplicitOrConfigBool(%v, %v, %v) = %v, want %v", c.flagVal, c.explicit, c.cfgVal, got, c.want)
			}
		})
	}
}

func TestLoadEffectiveConfigExplicitPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"ip":"1.2.3.4","port":123,"zone":"APS1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, src := loadEffectiveConfig(path)
	if cfg == nil {
		t.Fatal("got nil cfg for a valid explicit path")
	}
	if cfg.IP != "1.2.3.4" || cfg.Port != 123 || cfg.Zone != "APS1" {
		t.Errorf("got cfg=%+v, want IP=1.2.3.4 Port=123 Zone=APS1", cfg)
	}
	if src != path {
		t.Errorf("got source path %q, want %q", src, path)
	}
}

func TestLoadSessionConfigWarnsOnLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"ip":"1.2.3.4","port":123,"zone":"APS1"}`), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	if _, err := LoadSessionConfig(path); err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if !strings.Contains(buf.String(), "more permissive than 0600") {
		t.Errorf("expected a permission warning in the log output, got: %s", buf.String())
	}
}

// TestLoadSessionConfigRejectsMalformedJSON is the round-50 regression test for LoadSessionConfig's
// json.Unmarshal error path (config.go), which had zero direct test coverage: every existing
// LoadSessionConfig test above only ever hands it well-formed JSON. A session config file can be
// corrupted by a partial write surviving a crash/SIGKILL between SaveSessionConfig's os.Rename and
// the next read, or by a user hand-editing it -- LoadSessionConfig must return a clear, path-
// prefixed error in that case, not panic or silently return a zero-value config that would then
// get treated as a legitimate (if empty) prior session.
func TestLoadSessionConfigRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte(`{"ip":"1.2.3.4","port":`), 0600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadSessionConfig(path)
	if err == nil {
		t.Fatal("expected an error for malformed JSON, got nil")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("err = %v, want it to mention the file path %q so a corrupted config is identifiable", err, path)
	}
}

// TestLoadEffectiveConfigDefaultPathAbsentReturnsNil confirms the genuine first-run case still
// works correctly after dropping loadEffectiveConfig's separate os.Stat pre-check: with no
// -config flag and nothing at all at the default session config path, loadEffectiveConfig must
// return (nil, "") -- silently, without exiting -- exactly as before, so a bare -cs-* / plain
// email/loginKey run isn't blocked on a config file that was never expected to exist yet.
func TestLoadEffectiveConfigDefaultPathAbsentReturnsNil(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	cfg, src := loadEffectiveConfig("")
	if cfg != nil {
		t.Errorf("got cfg=%+v, want nil for a genuinely absent default session config", cfg)
	}
	if src != "" {
		t.Errorf("got source path %q, want \"\" for a genuinely absent default session config", src)
	}
}

// TestLoadEffectiveConfigExitsOnDefaultPathReadFailure is the regression test for the fix to the
// bug where loadEffectiveConfig's default-path branch treated ANY os.Stat/read error the same as
// "no config file yet" -- a transient/permission/I-O failure (EIO, a permission hiccup, ENOTDIR,
// an NFS/network-home glitch) on the default session config path silently fell through to the
// exact same (nil, "") a genuine first run produces, diverting what was meant to be a
// session-config-based reconnect into a completely different guest/email login flow under an
// unrelated device identity, with only an easy-to-miss WARN log line as a trace.
//
// A directory sitting where the default session config file is expected reproduces a reliable
// non-ENOENT read failure ("is a directory") without needing to fabricate a genuinely
// unreadable-but-present regular file -- mirrors identity_test.go's
// TestLoadOrCreateDeviceIdentityDoesNotClobberOnReadFailure technique, which (unlike fiddling with
// permission bits) also works when tests run as root.
//
// Per loadEffectiveConfig's own doc comment, a non-ENOENT error on the default path now fails
// loudly via os.Exit(1) (its (*SessionConfig, string) signature has no error return for main.go to
// inspect, so exiting is the only way to keep this from silently looking like the legitimate
// first-run case) -- so, like main_crossserver_test.go's TestRunCrossServerTestExitsWhenIPEmpty,
// this drives it via the standard re-exec-the-test-binary-as-a-subprocess idiom rather than
// calling it in-process, and asserts on both the exit code and the stderr message (the message is
// what actually distinguishes this from any other unrelated exit(1)).
func TestLoadEffectiveConfigExitsOnDefaultPathReadFailure(t *testing.T) {
	if os.Getenv("LASTWAR_TEST_HELPER_PROCESS") == "1" {
		dir := t.TempDir()
		t.Setenv("HOME", dir)

		// Put a directory where the default session config file is expected, so
		// LoadSessionConfig's os.ReadFile fails with a non-ENOENT error instead of the plain
		// "file doesn't exist yet" case.
		if err := os.Mkdir(defaultSessionConfigPath(), 0700); err != nil {
			t.Fatal(err)
		}

		cfg, src := loadEffectiveConfig("")
		// Only reached if loadEffectiveConfig fails to exit -- the outer assertions below will
		// then see a clean (non-error) subprocess exit and fail with a clear message instead of
		// this silently reproducing the exact pre-fix (nil, "") bug.
		if cfg != nil || src != "" {
			t.Fatalf("expected loadEffectiveConfig to exit before returning, got cfg=%+v src=%q", cfg, src)
		}
		return
	}

	cmd := exec.Command(os.Args[0], "-test.run=^TestLoadEffectiveConfigExitsOnDefaultPathReadFailure$")
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
	const wantMsg = "load session config failed"
	if !strings.Contains(stderr.String(), wantMsg) {
		t.Errorf("subprocess stderr = %s\nwant it to contain %q -- the pre-fix behavior instead silently returned (nil, \"\"), indistinguishable from the genuine first-run \"no config yet\" case", stderr.String(), wantMsg)
	}
}

// TestSaveSessionConfigRoundTrip confirms SaveSessionConfig's normal write-then-read round trip
// still works correctly after switching it from a plain os.WriteFile+os.Chmod sequence to the
// write-temp-then-rename atomicWriteStateFile helper (the same one identity.go's saveStateFile --
// see identity_test.go's TestSaveStateFileRoundTrip -- already uses for loginKey/gameUid/
// username): the target file must end up existing at the given path, at 0600, with exactly the
// written content round-tripping intact through LoadSessionConfig -- not the temp file, not
// something left behind under a ".tmp-*" name.
func TestSaveSessionConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")

	want := &SessionConfig{
		IP:          "1.2.3.4",
		Port:        9527,
		Zone:        "APS1",
		GameUid:     "some-game-uid",
		DeviceID:    "some-device-id",
		ShumeiBoxId: "some-shumei-box-id",
		AccessToken: "some-access-token",
		IOSMode:     true,
	}
	if err := SaveSessionConfig(want, path); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}

	got, err := LoadSessionConfig(path)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if *got != *want {
		t.Errorf("got %+v after round trip, want %+v", got, want)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("got mode %v for %s, want 0600", fi.Mode().Perm(), path)
	}

	// No stray temp file (atomicWriteStateFile's "<base>.tmp-*" pattern) should be left behind in
	// the directory alongside the real target -- confirms the rename actually happened rather
	// than leaving both a temp file and (somehow) the target.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("got directory entries %v, want exactly [%s] (no leftover temp file)", names, filepath.Base(path))
	}
}

func TestSaveSessionConfigTightensExistingFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := &SessionConfig{IP: "1.2.3.4", Port: 123, Zone: "APS1"}
	if err := SaveSessionConfig(cfg, path); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("got mode %v, want 0600 -- SaveSessionConfig should tighten an existing file's permissions, not just set them on creation", fi.Mode().Perm())
	}
}
