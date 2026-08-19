package main

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyOverride(t *testing.T) {
	if got := applyOverride("base", "override"); got != "override" {
		t.Errorf("applyOverride(base, override) = %q, want %q", got, "override")
	}
	if got := applyOverride("base", ""); got != "base" {
		t.Errorf("applyOverride(base, \"\") = %q, want %q", got, "base")
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
