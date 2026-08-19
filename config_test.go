package main

import (
	"bytes"
	"log/slog"
	"os"
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
