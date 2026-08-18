package main

import (
	"encoding/json"
	"fmt"
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
	return os.WriteFile(path, data, 0600)
}

// applyOverride returns override if it's non-zero, else base.
func applyOverride(base, override string) string {
	if override != "" {
		return override
	}
	return base
}

// loadEffectiveConfig resolves which session config file (if any) to use:
// an explicit -config path if given, else the default path if it exists.
// Returns (nil, "") if neither applies -- not an error, since running from
// bare -cs-* flags (or the plain email/loginKey flow) is still valid.
func loadEffectiveConfig(explicitPath string) (*SessionConfig, string) {
	path := explicitPath
	if path == "" {
		path = defaultSessionConfigPath()
		if _, err := os.Stat(path); err != nil {
			return nil, ""
		}
	}
	cfg, err := LoadSessionConfig(path)
	if err != nil {
		if explicitPath != "" {
			// only explicitly-requested configs are fatal if unreadable;
			// a missing/broken default file just falls through silently
			fmt.Fprintf(os.Stderr, "load session config %s: %v\n", path, err)
			os.Exit(1)
		}
		return nil, ""
	}
	return cfg, path
}
