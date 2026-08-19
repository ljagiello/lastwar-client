package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// SessionConfig holds everything DoCrossServerLogin needs to reconnect
// directly to an already-known role, without re-running the
// email-verification flow or retyping every -cs-* flag by hand.
//
// These values come from a live packet capture of a real client login (see
// the dossier's §14 "Solved: a field-by-field identity mismatch" section
// for the full methodology) -- there's no way to derive them offline.
// AccessToken in particular is tied to whatever platform identity
// (IOSMode) it was issued under; it is not single-use, but it will
// eventually need refreshing from a fresh capture.
type SessionConfig struct {
	IP          string `json:"ip"`
	Port        int    `json:"port"`
	Zone        string `json:"zone"`
	GameUid     string `json:"gameUid"`
	DeviceID    string `json:"deviceId"`
	ShumeiBoxId string `json:"shumeiBoxId"`
	AccessToken string `json:"accessToken"`
	IOSMode     bool   `json:"iosMode"`
}

func defaultSessionConfigPath() string {
	return stateFilePath(".lastwar_goclient_session.json")
}

// LoadSessionConfig reads a SessionConfig from path.
func LoadSessionConfig(path string) (*SessionConfig, error) {
	if fi, err := os.Stat(path); err == nil {
		if mode := fi.Mode().Perm(); mode&0077 != 0 {
			slog.Warn("session config file is more permissive than 0600 -- it holds a real access token", "path", path, "mode", mode)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg SessionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &cfg, nil
}

// SaveSessionConfig writes cfg to path as indented JSON via identity.go's
// atomicWriteStateFile (write to a fresh 0600 temp file in the same
// directory, fsync, then os.Rename into place) instead of a plain
// os.WriteFile+os.Chmod sequence. Used to persist a serverInfo redirect's
// resolved address/zone (see CrossServerLoginResult) so future runs connect
// directly instead of re-following the same redirect every time -- and, via
// main.go's runCrossServerTest, to persist a -cs-rt access-token refresh.
// Both call sites tend to land near the end of a cron-driven run, exactly
// the kind of moment a SIGKILL/timeout/power-loss is plausible.
//
// A plain os.WriteFile does an open(O_TRUNC)+write+close as separate
// syscalls with no fsync: a crash/OOM-kill/power-loss mid-write could leave
// a zero-length or torn/truncated session config file behind. That's
// especially bad here because loadEffectiveConfig only treats
// os.IsNotExist as the benign "no config yet" case -- a torn file's
// json.Unmarshal failure does NOT satisfy os.IsNotExist, so it takes the
// fatal os.Exit(1) branch instead, repeating on every subsequent run until
// a human intervenes. Routing through atomicWriteStateFile closes that gap
// the same way it already closed it for identity.go's loginKey/gameUid/
// username state files: the rename either publishes the complete new
// content or leaves the previous complete content in place, never
// something in between.
//
// As a side effect of always writing through a fresh 0600 temp file and
// renaming it into place, this also settles the "existing file left
// world/group-readable" gotcha os.WriteFile's mode argument has (it only
// applies on file creation, not when overwriting an existing file): rename
// replaces the target's inode outright, so the destination always ends up
// with the temp file's 0600 bits regardless of what permissions the file
// being replaced had -- preserving the 0600 permissions a session config
// (it holds a real access token) should always have.
func SaveSessionConfig(cfg *SessionConfig, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteStateFile(path, string(data))
}

// applyOverride returns override if it's non-zero, else base.
func applyOverride(base, override string) string {
	if override != "" {
		return override
	}
	return base
}

// loadEffectiveConfig resolves which session config file (if any) to use:
// an explicit -config path if given, else the default path. Returns (nil,
// "") when the resolved path genuinely has no file yet -- not an error,
// since running from bare -cs-* flags (or the plain email/loginKey flow) is
// still valid for a brand-new setup.
//
// This calls LoadSessionConfig(path) directly for the default path too
// (previously it did a separate os.Stat(path) pre-check first, and treated
// every stat error -- ENOENT or not -- as "nothing to load"). That pre-check
// is gone: checking os.IsNotExist(err) on LoadSessionConfig's own result
// covers "file genuinely doesn't exist" just as well, without a second
// syscall or the TOCTOU gap between a passing stat and the read that
// followed it.
func loadEffectiveConfig(explicitPath string) (*SessionConfig, string) {
	path := explicitPath
	if path == "" {
		path = defaultSessionConfigPath()
	}
	cfg, err := LoadSessionConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Genuinely no config at this path yet -- an expected, common
			// case for both an explicit -config path and the default path.
			// Continue silently; the caller falls back to bare -cs-* flags
			// or the plain email/loginKey flow.
			return nil, ""
		}
		// Any other error -- permission denied, I/O failure, a directory
		// sitting where the file was expected, corrupt JSON, an NFS/network-
		// home glitch, etc. -- must never be treated the same as "absent".
		// Doing so used to let a stat/read hiccup on the default session-
		// config path get silently swallowed (with, at best, an
		// easy-to-miss WARN) and fall through to (nil, ""), which the
		// caller (main.go) can't tell apart from the legitimate first-run
		// case -- silently diverting what was meant to be a session-based
		// reconnect into a completely different guest/email login flow
		// under a different, unrelated device identity. This is the exact
		// class of bug round 16 fixed in identity.go's
		// loadOrCreateDeviceIdentity/readTrimmed: a non-ENOENT failure
		// reading an EXISTING file is never silently treated as "nothing
		// there yet".
		//
		// Both the explicit -config path and the default path fail loudly
		// here via os.Exit(1), not just the explicit one:
		//   - -config is a deliberate, explicit request, so a real failure
		//     reading it obviously shouldn't be swallowed.
		//   - The default path is consulted unconditionally on every run
		//     (unless -no-config is passed), exactly like identity.go's
		//     device-id/gameUid/loginKey state files that loadOrCreateDeviceIdentity
		//     reads unconditionally and fails hard (via its caller's
		//     os.Exit(1)) on any non-ENOENT error. This function's own
		//     signature -- (*SessionConfig, string), no error return -- gives
		//     it no way to hand a distinguishable failure back to main.go for
		//     the caller to decide on; silently returning (nil, "") here
		//     would be indistinguishable from the legitimate first-run case
		//     and let main.go proceed straight into a wrong-identity login.
		//     An operator who genuinely doesn't want default session-config
		//     handling at all already has an explicit, deliberate opt-out
		//     (-no-config) that skips this function entirely -- so failing
		//     hard here on a persistent glitch doesn't trap them without a
		//     way out; it just means -no-config is what they should be
		//     passing instead of relying on this to silently no-op.
		slog.Error("load session config failed", "path", path, "error", err)
		os.Exit(1)
	}
	return cfg, path
}
