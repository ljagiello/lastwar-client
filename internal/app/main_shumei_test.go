package app

import (
	"encoding/json"
	"io"
	"lastwar-client/internal/shumei"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

const shumeiSmid1 = `{"code":1100,"detail":{"deviceId":"smid-1"},"requestId":"r-1"}`

// writeTestSessionConfig saves cfg to a fresh session config file and returns its path.
func writeTestSessionConfig(t *testing.T, cfg *SessionConfig) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.json")
	if err := SaveSessionConfig(cfg, path); err != nil {
		t.Fatalf("SaveSessionConfig: %v", err)
	}
	return path
}

// TestMintShumeiBoxIDKeepsInstall checks the first -cs-shumei-gen run for a session fills its
// install values and saves them, with the minted box ID, to the session config without touching
// the file's other fields, and that a later run reusing them leaves the file alone.
func TestMintShumeiBoxIDKeepsInstall(t *testing.T) {
	path := writeTestSessionConfig(t, &SessionConfig{
		DeviceID: "dev-1", AccessToken: "tok-1", RefreshToken: "rt-1", RefreshTokenTime: 42,
	})
	client := &shumeiReply{body: shumeiSmid1}

	boxID, install := mintShumeiBoxID(client, "dev-1", shumei.Device{}, shumei.Install{}, path)
	if boxID != "Bsmid-1" {
		t.Fatalf("boxID = %q, want Bsmid-1", boxID)
	}
	if install.BoxID != "Bsmid-1" || install.BoxIDSavedAt.IsZero() || install.AndroidID == "" || install.PUUID == "" {
		t.Errorf("install = %+v, want filled values and the minted box ID with its save time", install)
	}
	saved, err := LoadSessionConfig(path)
	if err != nil {
		t.Fatalf("LoadSessionConfig: %v", err)
	}
	if saved.ShumeiInstall.AndroidID != install.AndroidID || saved.ShumeiInstall.BoxID != "Bsmid-1" ||
		!saved.ShumeiInstall.BoxIDSavedAt.Equal(install.BoxIDSavedAt) {
		t.Errorf("saved shumeiInstall = %+v, want %+v", saved.ShumeiInstall, install)
	}
	if saved.AccessToken != "tok-1" || saved.RefreshToken != "rt-1" || saved.RefreshTokenTime != 42 || saved.ShumeiBoxId != "" {
		t.Errorf("saving the install changed other fields: %+v", *saved)
	}

	// Same phone, same smid back: nothing changed, so nothing is written. The file is rewritten
	// as compact JSON first; a save would indent it again.
	compact, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compact, 0o600); err != nil {
		t.Fatal(err)
	}
	boxID, again := mintShumeiBoxID(client, "dev-1", shumei.Device{}, install, path)
	if boxID != "Bsmid-1" || again != install {
		t.Errorf("second run = %q, %+v; want Bsmid-1 and the install unchanged", boxID, again)
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(compact) {
		t.Errorf("second run rewrote the session config (read error %v); want it untouched", err)
	}
	if client.calls != 2 {
		t.Errorf("device-profile calls = %d, want 2: every run registers, as the SDK does on each launch", client.calls)
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
	if saved.ShumeiInstall.AndroidID == "" || saved.ShumeiInstall.AndroidID != install.AndroidID {
		t.Errorf("saved shumeiInstall = %+v, want the filled install %+v", saved.ShumeiInstall, install)
	}
}

// TestMintShumeiBoxIDNoConfig checks a run without a session config still mints, from a fresh
// install it can't keep.
func TestMintShumeiBoxIDNoConfig(t *testing.T) {
	boxID, install := mintShumeiBoxID(&shumeiReply{body: shumeiSmid1}, "dev-1", shumei.Device{}, shumei.Install{}, "")
	if boxID != "Bsmid-1" || install.AndroidID == "" {
		t.Errorf("mintShumeiBoxID = %q, %+v; want Bsmid-1 and a fresh install", boxID, install)
	}
}
