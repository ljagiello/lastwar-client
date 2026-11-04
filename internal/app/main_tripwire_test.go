package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"lastwar-client/internal/game"
)

// TestRunPointsTheTripwireAtTheSessionConfig runs -list-features with -config in a subprocess: Run
// must point the tripwire at <config>.tripwire.json (so a trip outlives the process) and warn while
// that file exists. It fails if Run stops calling SetTripwirePath.
func TestRunPointsTheTripwireAtTheSessionConfig(t *testing.T) {
	if cfg := os.Getenv("LASTWAR_TEST_TRIPWIRE_PATH"); cfg != "" {
		os.Args = []string{"lastwar-client", "-config", cfg, "-list-features"}
		Run()
		return
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "session.json")
	if err := os.WriteFile(cfg, []byte(`{"ip":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestRunPointsTheTripwireAtTheSessionConfig$")
		cmd.Env = append(os.Environ(), "LASTWAR_TEST_TRIPWIRE_PATH="+cfg, "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("-list-features: %v\n%s", err, out)
		}
		return string(out)
	}
	const warn = "the diamond tripwire file is set"
	if out := run(); strings.Contains(out, warn) {
		t.Errorf("warned without a tripwire file:\n%s", out)
	}
	if err := os.WriteFile(game.TripwireFile(cfg), []byte(`{"feature":"duel-speedups","command":"build.ccd.m.new","reason":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := run(); !strings.Contains(out, warn) || !strings.Contains(out, "duel-speedups") {
		t.Errorf("no startup Warn naming the trip while the file exists:\n%s", out)
	}
}

// TestClearTripwireFlag runs -clear-tripwire in a subprocess: it removes the tripwire file next to
// the -config session config, leaves the config alone, sends nothing and exits 0, and says so
// when there is nothing to clear.
func TestClearTripwireFlag(t *testing.T) {
	if cfg := os.Getenv("LASTWAR_TEST_CLEAR_TRIPWIRE"); cfg != "" {
		os.Args = []string{"lastwar-client", "-config", cfg, "-clear-tripwire"}
		Run()
		return
	}
	dir := t.TempDir()
	cfg := filepath.Join(dir, "session.json")
	if err := os.WriteFile(cfg, []byte(`{"ip":"127.0.0.1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	trip := game.TripwireFile(cfg)
	if err := os.WriteFile(trip, []byte(`{"feature":"duel-speedups","command":"build.ccd.m.new","reason":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestClearTripwireFlag$")
		cmd.Env = append(os.Environ(), "LASTWAR_TEST_CLEAR_TRIPWIRE="+cfg, "HOME="+dir)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("-clear-tripwire: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := run(); !strings.Contains(out, "cleared the diamond tripwire") || !strings.Contains(out, "duel-speedups") {
		t.Errorf("output lacks the cleared record:\n%s", out)
	}
	if _, err := os.Stat(trip); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tripwire file still there: %v", err)
	}
	if _, err := os.Stat(cfg); err != nil {
		t.Errorf("the session config must be left alone: %v", err)
	}
	if out := run(); !strings.Contains(out, "no diamond tripwire file to clear") {
		t.Errorf("second run output:\n%s", out)
	}
}
