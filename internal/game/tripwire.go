package game

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Diamond tripwires that outlive the process (aud-duel F3). duelSpeedupsTrip, duelSpendHalted and
// digsTrip live in memory, and cron starts a new -collect process every 3 h, so a trip used to be
// forgotten by the next run and a cause that persists would repeat one diamond-charging call per
// run. The client spends diamonds only on an explicit, diamond-priced action of the player (the
// speed panel's useGold sends, UISpeedView.lua:951-970, behind UIUtil.TryShowDiamondConfirm,
// Util/UIUtil.lua:3292-3299), so once a headless run has seen the balance fall nothing spends
// again until a person has looked.
//
// So every observed diamond drop is also written to its own file next to the session config,
// <config>.tripwire.json (never the session config itself), 0600, through a temp file and a
// rename. Reply-shape failures (duel-speedups' missing itemCostArr or gold) halt only the process
// that saw them. While that file exists, every duel spend (duel-speedups, duel-recruit-tickets,
// duel-troop-training, secret-tasks-start), Treasure Hunt's pickaxe digs (digsSpendCmds) and
// treasure-digs' brick opens refuse, with a Warn naming the file. Free claims, the free vault-stone
// opens and the read-only -plan twins still run. Only the operator clears it, with
// -clear-tripwire. An unreadable or malformed file counts as a trip. With no path set (tests, a
// run without a session config) trips stay in memory only, as before.

// TripwireRecord is one persisted trip.
type TripwireRecord struct {
	Time    time.Time `json:"time"`
	Feature string    `json:"feature"`
	Command string    `json:"command"`
	Reason  string    `json:"reason"`
}

// tripwireClearFlag is the CLI flag that removes the file (internal/app).
const tripwireClearFlag = "-clear-tripwire"

var tripwireState struct {
	sync.Mutex
	path string
}

// errTripwire is what a spend feature returns while the tripwire file exists.
var errTripwire = errors.New("the diamond tripwire file is set: a duel or dig spend saw the diamond balance fall")

// TripwireFile is the tripwire file for a session config path, or "" when there is none.
func TripwireFile(sessionConfigPath string) string {
	if sessionConfigPath == "" {
		return ""
	}
	return sessionConfigPath + ".tripwire.json"
}

// SetTripwirePath sets the file trips persist to; internal/app calls it at startup with
// TripwireFile of the loaded session config. "" keeps trips in memory only.
func SetTripwirePath(path string) {
	tripwireState.Lock()
	defer tripwireState.Unlock()
	tripwireState.path = path
}

func tripwirePath() string {
	tripwireState.Lock()
	defer tripwireState.Unlock()
	return tripwireState.path
}

// tripwireTrip persists a trip. The first one wins: a file that already exists is left as it is.
// A failed write is logged; the in-process halt that called this still applies.
func tripwireTrip(feature, cmd, reason string) {
	path := tripwirePath()
	if path == "" {
		return
	}
	if _, err := os.Lstat(path); err == nil {
		return
	}
	rec := TripwireRecord{Time: time.Now().UTC(), Feature: feature, Command: cmd, Reason: reason}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		err = tripwireWrite(path, data)
	}
	if err != nil {
		slog.Error("DIAMOND TRIPWIRE: could not persist the trip; the next run will not know about it",
			"path", path, "feature", feature, "error", err)
		return
	}
	slog.Error("DIAMOND TRIPWIRE persisted: every duel and dig spend refuses to run until the file is removed",
		"path", path, "feature", feature, "command", cmd, "clear", tripwireClearFlag)
}

// tripwireWrite writes data to path atomically: a 0600 temp file in the same directory, synced,
// then renamed over path.
func tripwireWrite(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }() // a no-op once the rename succeeds
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// tripwireActive reports whether the tripwire file exists, with its record when it parses.
func tripwireActive() (rec TripwireRecord, path string, active bool, readErr error) {
	path = tripwirePath()
	if path == "" {
		return rec, "", false, nil
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return rec, path, false, nil
	case err != nil:
		return rec, path, true, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, path, true, err
	}
	return rec, path, true, nil
}

// tripwireLogArgs are the Warn attributes naming the file, its record and how to clear it.
func tripwireLogArgs(rec TripwireRecord, path string, readErr error) []any {
	args := []any{"path", path, "clear", "remove the file or run with " + tripwireClearFlag}
	if readErr != nil {
		return append(args, "readError", readErr)
	}
	return append(args, "trippedAt", rec.Time.Format(time.RFC3339), "trippedBy", rec.Feature,
		"command", rec.Command, "reason", rec.Reason)
}

// tripwireErr returns an error, after a Warn naming the file and how to clear it, while the
// tripwire file exists; nil otherwise. feature is the one refusing.
func tripwireErr(feature string) error {
	rec, path, active, readErr := tripwireActive()
	if !active {
		return nil
	}
	slog.Warn(feature+": not spending while the diamond tripwire file exists",
		append([]any{"feature", feature}, tripwireLogArgs(rec, path, readErr)...)...)
	return fmt.Errorf("%s: %w (%s; remove it or run with %s)", feature, errTripwire, path, tripwireClearFlag)
}

// WarnIfTripwireSet logs a Warn at startup while the tripwire file exists, so every run's log
// shows it, not only the days a spend feature runs. internal/app calls it after SetTripwirePath.
func WarnIfTripwireSet() {
	rec, path, active, readErr := tripwireActive()
	if active {
		slog.Warn("the diamond tripwire file is set: duel and dig spends refuse until it is cleared",
			tripwireLogArgs(rec, path, readErr)...)
	}
}

// ClearTripwire removes the tripwire file at path (-clear-tripwire). It reports whether there
// was one.
func ClearTripwire(path string) (bool, error) {
	if path == "" {
		return false, errors.New("no tripwire file path")
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		slog.Info("no diamond tripwire file to clear", "path", path)
		return false, nil
	}
	var rec TripwireRecord
	if err == nil {
		_ = json.Unmarshal(data, &rec)
	}
	if err := os.Remove(path); err != nil {
		return false, err
	}
	slog.Warn("cleared the diamond tripwire: duel and dig spends run again from the next run", "path", path,
		"trippedAt", rec.Time.Format(time.RFC3339), "trippedBy", rec.Feature, "command", rec.Command, "reason", rec.Reason)
	return true, nil
}
