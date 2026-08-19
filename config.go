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

// SaveSessionConfig writes cfg to path as indented JSON, preserving the
// 0600 permissions a session config (it holds a real access token) should
// always have -- matters because os.WriteFile only applies its mode bit
// when actually creating the file; on an existing file the previous mode
// wins, so a config file created some other way with looser permissions
// wouldn't get tightened by this call alone. Used to persist a
// serverInfo redirect's resolved address/zone (see
// CrossServerLoginResult) so future runs connect directly instead of
// re-following the same redirect every time.
func SaveSessionConfig(cfg *SessionConfig, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	// os.WriteFile's mode argument only applies when the file is newly created; on an existing
	// file its previous mode wins. Chmod explicitly so the 0600 invariant this file needs (it
	// holds a real access token) actually holds on every save, not just at creation.
	return os.Chmod(path, 0600)
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
