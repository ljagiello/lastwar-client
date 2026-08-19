package main

import (
	"bytes"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// Confirms BuildLoginParams' Android/iOS and empty-vs-set-GameUid conditional field logic --
// exactly the static-vs-dynamic field set whose mismatch caused the documented "reconnect wall"
// identity-mismatch production bug (see docs/live-validation.mdx).
func TestBuildLoginParamsConditionalFields(t *testing.T) {
	cases := []struct {
		name    string
		iosMode bool
		gameUid string
	}{
		{"android, empty gameUid", false, ""},
		{"ios, empty gameUid", true, ""},
		{"android, set gameUid", false, "12345"},
		{"ios, set gameUid", true, "12345"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := BuildLoginParams(LoginParamsInput{
				FutureID: 1,
				DeviceID: "dev-1",
				AirKey:   "air-1",
				GameUid:  c.gameUid,
				ServerID: "1234",
				IOSMode:  c.iosMode,
			})

			for _, key := range []string{"AndroidID", "IMEI"} {
				if got := p.Has(key); got != !c.iosMode {
					t.Errorf("Has(%q) = %v, want %v (IOSMode=%v)", key, got, !c.iosMode, c.iosMode)
				}
			}
			if got := p.Has("google_available"); got != !c.iosMode {
				t.Errorf("Has(google_available) = %v, want %v (IOSMode=%v)", got, !c.iosMode, c.iosMode)
			}

			for _, key := range []string{"idfa", "idfv", "phone_native_screen"} {
				if got := p.Has(key); got != c.iosMode {
					t.Errorf("Has(%q) = %v, want %v (IOSMode=%v)", key, got, c.iosMode, c.iosMode)
				}
			}

			wantEmptyUidFields := c.gameUid == ""
			for _, key := range []string{"country", "suggestCountry", "timeoffset", "gcmRegisterId", "referrer"} {
				if got := p.Has(key); got != wantEmptyUidFields {
					t.Errorf("Has(%q) = %v, want %v (GameUid=%q)", key, got, wantEmptyUidFields, c.gameUid)
				}
			}

			wantPackageName := packageName
			wantPlatform := "1"
			wantPf := "market_global"
			wantAppVersion := appVersion
			wantVersionCode := versionCode
			if c.iosMode {
				wantPackageName = iosPackageName
				wantPlatform = "0"
				wantPf = "AppStore"
				wantAppVersion = "1.0.344"
				wantVersionCode = "786"
			}
			if got := p.GetString("packageName"); got != wantPackageName {
				t.Errorf("packageName = %q, want %q", got, wantPackageName)
			}
			if got := p.GetString("platform"); got != wantPlatform {
				t.Errorf("platform = %q, want %q", got, wantPlatform)
			}
			if got := p.GetString("pf"); got != wantPf {
				t.Errorf("pf = %q, want %q", got, wantPf)
			}
			if got := p.GetString("appVersion"); got != wantAppVersion {
				t.Errorf("appVersion = %q, want %q", got, wantAppVersion)
			}
			if got := p.GetString("versionCode"); got != wantVersionCode {
				t.Errorf("versionCode = %q, want %q", got, wantVersionCode)
			}
		})
	}
}

// TestSaveLoginKeyTightensExistingFilePermissions mirrors config_test.go's
// TestSaveSessionConfigTightensExistingFilePermissions -- the loginKey file is even more
// sensitive than the session config (see warnIfLoosePermissions' doc comment: a loginKey alone
// is sufficient to log back into the real account), so SaveLoginKey must also tighten an
// existing file's permissions on every save, not just at creation.
func TestSaveLoginKeyTightensExistingFilePermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := loginKeyStatePath()
	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}

	d := &deviceIdentity{}
	if err := d.SaveLoginKey("fresh-key"); err != nil {
		t.Fatalf("SaveLoginKey: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("got mode %v, want 0600 -- SaveLoginKey should tighten an existing file's permissions, not just set them on creation", fi.Mode().Perm())
	}
}

// TestLoadOrCreateDeviceIdentityWarnsOnLoosePermissions mirrors config_test.go's
// TestLoadSessionConfigWarnsOnLoosePermissions -- the persisted loginKey is at least as sensitive
// as the session config (see warnIfLoosePermissions' doc comment), so loading the device identity
// must surface the same loose-permission warning.
func TestLoadOrCreateDeviceIdentityWarnsOnLoosePermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := os.WriteFile(loginKeyStatePath(), []byte("stale-key"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	if _, err := loadOrCreateDeviceIdentity(); err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity: %v", err)
	}
	if !strings.Contains(buf.String(), "more permissive than 0600") {
		t.Errorf("expected a permission warning in the log output, got: %s", buf.String())
	}
}

// TestLoadOrCreateDeviceIdentityRoundTrip confirms the full persisted-state lifecycle: a fresh
// HOME creates a new device identity, SaveGameUid/SaveUsername persist their values to disk at
// 0600, and a second load picks up exactly what was saved -- the same guarantee config_test.go's
// tests already confirm for SessionConfig.
func TestLoadOrCreateDeviceIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	first, err := loadOrCreateDeviceIdentity()
	if err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity (create): %v", err)
	}
	if first.DeviceID == "" {
		t.Fatal("got empty DeviceID on fresh identity")
	}
	if first.GameUid != "" || first.Username != "" {
		t.Errorf("got GameUid=%q Username=%q on fresh identity, want both empty", first.GameUid, first.Username)
	}

	const wantGameUid = "test-gameuid-123"
	const wantUsername = "test-username"
	if err := first.SaveGameUid(wantGameUid); err != nil {
		t.Fatalf("SaveGameUid: %v", err)
	}
	if err := first.SaveUsername(wantUsername); err != nil {
		t.Fatalf("SaveUsername: %v", err)
	}

	second, err := loadOrCreateDeviceIdentity()
	if err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity (reload): %v", err)
	}
	if second.DeviceID != first.DeviceID {
		t.Errorf("got DeviceID %q on reload, want %q (should reuse the persisted id, not regenerate)", second.DeviceID, first.DeviceID)
	}
	if second.GameUid != wantGameUid {
		t.Errorf("got GameUid %q on reload, want %q", second.GameUid, wantGameUid)
	}
	if second.Username != wantUsername {
		t.Errorf("got Username %q on reload, want %q", second.Username, wantUsername)
	}

	for _, path := range []string{deviceIDStatePath(), gameUidStatePath(), usernameStatePath()} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Errorf("got mode %v for %s, want 0600", fi.Mode().Perm(), path)
		}
	}
}
