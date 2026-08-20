package auth

import (
	"bytes"
	"fmt"
	"lastwar-client/internal/gsl"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDeviceIdentityStringGoStringRedact is the round-48 regression test for the MAJOR finding
// that deviceIdentity -- which carries LoginKey, the single most sensitive credential in this
// client -- had no String()/GoString() redaction, unlike gsl.go's gsl.LoginToken (round 47). Proves
// both the bare value and a value/pointer nested inside a containing struct's %+v are redacted;
// deviceIdentity is used as *deviceIdentity throughout the codebase (LoginResult.Ident), so both
// receiver shapes are checked.
func TestDeviceIdentityStringGoStringRedact(t *testing.T) {
	const liveLoginKey = "FAKE-LIVE-LOGIN-KEY-must-not-leak-xyz789"
	d := deviceIdentity{DeviceID: "dev-1", Username: "user-1", GameUid: "uid-1", LoginKey: liveLoginKey}

	t.Run("String", func(t *testing.T) {
		s := d.String()
		if strings.Contains(s, liveLoginKey) {
			t.Errorf("String() = %q, must not contain the live loginKey", s)
		}
	})
	t.Run("GoString", func(t *testing.T) {
		s := d.GoString()
		if strings.Contains(s, liveLoginKey) {
			t.Errorf("GoString() = %q, must not contain the live loginKey", s)
		}
	})
	t.Run("nested pointer inside a containing struct's %+v", func(t *testing.T) {
		wrapper := struct{ Ident *deviceIdentity }{Ident: &d}
		formatted := fmt.Sprintf("%+v", wrapper)
		if strings.Contains(formatted, liveLoginKey) {
			t.Errorf("fmt.Sprintf(%%+v, wrapper) = %q, must not contain the live loginKey nested in .Ident", formatted)
		}
	})
	t.Run("via fmt.Errorf %v", func(t *testing.T) {
		err := fmt.Errorf("login failed: %v", d)
		if strings.Contains(err.Error(), liveLoginKey) {
			t.Errorf("err = %q, must not contain the live loginKey", err.Error())
		}
	})
}

// TestLoginParamsInputStringGoStringRedact is the round-48 regression test for the MINOR finding
// that LoginParamsInput -- which carries AccessTok/GameUid, live credentials -- had no
// String()/GoString() redaction, the same class of gap round 47/48 closed for
// gsl.LoginToken/deviceIdentity/SessionConfig.
func TestLoginParamsInputStringGoStringRedact(t *testing.T) {
	const liveToken = "FAKE-LIVE-ACCESS-TOKEN-must-not-leak-ghi789"
	in := LoginParamsInput{DeviceID: "dev-1", GameUid: "uid-1", AccessTok: liveToken}

	if s := in.String(); strings.Contains(s, liveToken) {
		t.Errorf("String() = %q, must not contain the live token", s)
	}
	if s := in.GoString(); strings.Contains(s, liveToken) {
		t.Errorf("GoString() = %q, must not contain the live token", s)
	}
	if s := fmt.Sprintf("%+v", struct{ In LoginParamsInput }{In: in}); strings.Contains(s, liveToken) {
		t.Errorf("fmt.Sprintf(%%+v, wrapper) = %q, must not contain the live token nested in .In", s)
	}
}

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

			wantPackageName := gsl.PackageName
			wantPlatform := "1"
			wantPf := "market_global"
			wantAppVersion := gsl.AppVersion
			wantVersionCode := gsl.VersionCode
			if c.iosMode {
				wantPackageName = iosPackageName
				wantPlatform = "0"
				wantPf = "AppStore"
				wantAppVersion = "1.0.344"
				wantVersionCode = "786"
			}
			if got := p.GetString("packageName"); got != wantPackageName {
				t.Errorf("PackageName = %q, want %q", got, wantPackageName)
			}
			if got := p.GetString("platform"); got != wantPlatform {
				t.Errorf("Platform = %q, want %q", got, wantPlatform)
			}
			if got := p.GetString("pf"); got != wantPf {
				t.Errorf("pf = %q, want %q", got, wantPf)
			}
			if got := p.GetString("appVersion"); got != wantAppVersion {
				t.Errorf("AppVersion = %q, want %q", got, wantAppVersion)
			}
			if got := p.GetString("versionCode"); got != wantVersionCode {
				t.Errorf("VersionCode = %q, want %q", got, wantVersionCode)
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

// TestSaveIdentityFieldRejectsOversizedValue is the round-46 regression test for the MAJOR finding
// that SaveLoginKey/SaveGameUid/SaveUsername persisted a server-supplied value with no length cap
// at all -- the same wire-tag-equivalence gap round 45 closed for mail.go's uid field: GetString
// (sfsobject.go) can't distinguish the 65535-byte-capped sfs.SFSUtfString wire tag from the far larger
// sfs.SFSText tag, both of which decode to the same Go string, so a server response tagging
// loginKey/gameUid/un/gameUserName as sfs.SFSText could previously smuggle an oversized value straight
// into the in-memory identity AND onto disk -- and since BuildLoginParams/DoCrossServerLogin
// unconditionally re-embed these persisted values into every future login request via
// PutUtfString (hard-capped at 65535 bytes by sfs.WriteUtfString), an oversized value would then
// permanently break every subsequent login attempt until an operator manually intervened. Proves
// all three Save* methods reject a one-byte-over-cap value with an error, leaving BOTH the
// in-memory field and the on-disk state file untouched (still holding whatever value, if any, was
// there before) rather than corrupting either.
func TestSaveIdentityFieldRejectsOversizedValue(t *testing.T) {
	oversized := strings.Repeat("a", maxIdentityFieldLen+1)

	cases := []struct {
		name   string
		save   func(d *deviceIdentity, v string) error
		get    func(d *deviceIdentity) string
		path   func() string
		preset string
	}{
		{"loginKey", (*deviceIdentity).SaveLoginKey, func(d *deviceIdentity) string { return d.LoginKey }, loginKeyStatePath, "previous-good-key"},
		{"gameUid", (*deviceIdentity).SaveGameUid, func(d *deviceIdentity) string { return d.GameUid }, gameUidStatePath, "previous-good-uid"},
		{"username", (*deviceIdentity).SaveUsername, func(d *deviceIdentity) string { return d.Username }, usernameStatePath, "previous-good-name"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)

			d := &deviceIdentity{LoginKey: tt.preset, GameUid: tt.preset, Username: tt.preset}

			err := tt.save(d, oversized)
			if err == nil {
				t.Fatal("expected an error for a value one byte over maxIdentityFieldLen, got nil")
			}

			if got := tt.get(d); got != tt.preset {
				t.Errorf("in-memory field = %q (len %d), want it to stay at the previous value %q -- an oversized value must not corrupt the in-memory identity", got, len(got), tt.preset)
			}
			if _, statErr := os.Stat(tt.path()); !os.IsNotExist(statErr) {
				t.Errorf("state file exists at %s, want no file written for a rejected oversized value", tt.path())
			}
		})
	}
}

// TestSaveIdentityFieldAcceptsValueExactlyAtCap is the round-47 regression test for the MINOR
// finding that TestSaveIdentityFieldRejectsOversizedValue above only tests the reject side of
// maxIdentityFieldLen's boundary (a strict len > maxIdentityFieldLen guard): it never proves a
// value of exactly 65535 bytes -- the wire format's own hard limit, and thus the largest value
// that's still guaranteed re-encodable later via PutUtfString -- is accepted. Without this, a
// future edit tightening any of the three Save* methods' `>` comparisons to `>=` would wrongly
// reject a legitimate maximal-length loginKey/gameUid/username while every existing test kept
// passing. Proves all three Save* methods accept an exactly-at-cap value, storing it in BOTH the
// in-memory field and the on-disk state file.
func TestSaveIdentityFieldAcceptsValueExactlyAtCap(t *testing.T) {
	atCap := strings.Repeat("a", maxIdentityFieldLen)

	cases := []struct {
		name string
		save func(d *deviceIdentity, v string) error
		get  func(d *deviceIdentity) string
		path func() string
	}{
		{"loginKey", (*deviceIdentity).SaveLoginKey, func(d *deviceIdentity) string { return d.LoginKey }, loginKeyStatePath},
		{"gameUid", (*deviceIdentity).SaveGameUid, func(d *deviceIdentity) string { return d.GameUid }, gameUidStatePath},
		{"username", (*deviceIdentity).SaveUsername, func(d *deviceIdentity) string { return d.Username }, usernameStatePath},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)

			d := &deviceIdentity{}

			if err := tt.save(d, atCap); err != nil {
				t.Fatalf("expected a value of exactly maxIdentityFieldLen bytes to be accepted, got error: %v", err)
			}

			if got := tt.get(d); got != atCap {
				t.Errorf("in-memory field len = %d, want %d (the unmodified at-cap value)", len(got), len(atCap))
			}
			onDisk, err := os.ReadFile(tt.path())
			if err != nil {
				t.Fatalf("expected the state file to be written for an at-cap value: %v", err)
			}
			if string(onDisk) != atCap {
				t.Errorf("state file content len = %d, want %d (the unmodified at-cap value)", len(onDisk), len(atCap))
			}
		})
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

	if _, err := LoadOrCreateDeviceIdentity(); err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity: %v", err)
	}
	if !strings.Contains(buf.String(), "more permissive than 0600") {
		t.Errorf("expected a permission warning in the log output, got: %s", buf.String())
	}
}

// TestLoadOrCreateDeviceIdentityWarnsOnLooseGameUidPermissions confirms the loose-permissions
// warning isn't limited to the loginKey file -- gameUid is just as capable of driving an
// account-resolving GSL call (opt=fix, see login.go's gslOptFor) once a deviceId is known, so it
// must get the same check on every load.
func TestLoadOrCreateDeviceIdentityWarnsOnLooseGameUidPermissions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	if err := os.WriteFile(gameUidStatePath(), []byte("stale-gameuid"), 0644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	if _, err := LoadOrCreateDeviceIdentity(); err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity: %v", err)
	}
	if !strings.Contains(buf.String(), "more permissive than 0600") {
		t.Errorf("expected a permission warning in the log output, got: %s", buf.String())
	}
}

// TestStateFilePathWarnsAndFallsBackWhenHomeDirUnavailable confirms stateFilePath's
// os.UserHomeDir() fallback -- used for all four persisted device-identity state files, loginKey
// included -- is no longer silent. On darwin/linux, os.UserHomeDir() reads $HOME directly (see
// Go's os package: only windows/plan9/nacl/android/ios get special-cased env vars or hardcoded
// paths), so an empty $HOME reproduces the same "home directory not determined" failure a minimal
// container or misconfigured cron environment would hit for real. Before this fix, that fallback
// silently redirected credential state files to the current working directory with no logged
// trace; reverting the slog.Warn call in stateFilePath would make this test fail (the location
// assertion would still pass) while every other identity_test.go test keeps passing, since none
// of them exercise the os.UserHomeDir() error path.
func TestStateFilePathWarnsAndFallsBackWhenHomeDirUnavailable(t *testing.T) {
	t.Setenv("HOME", "")

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	const name = ".lastwar_goclient_test_marker"
	got := StateFilePath(name)
	want := filepath.Join(".", name)
	if got != want {
		t.Errorf("stateFilePath(%q) = %q, want %q (current-working-directory fallback)", name, got, want)
	}

	logOutput := buf.String()
	if !strings.Contains(logOutput, "could not determine home directory") {
		t.Errorf("expected a home-directory-fallback warning in the log output, got: %s", logOutput)
	}
	if !strings.Contains(logOutput, "dir=.") {
		t.Errorf("expected the warning to record the fallback dir, got: %s", logOutput)
	}
}

// TestLoadOrCreateDeviceIdentityDoesNotClobberOnReadFailure confirms the fix for the bug where
// loadOrCreateDeviceIdentity treated ANY device-id read error (not just os.IsNotExist) as "no
// identity yet" -- silently fabricating and persisting a brand-new random device ID over an
// EXISTING, valid one, permanently losing the identity the server already recognizes. A directory
// sitting where the state file is expected reproduces a reliable non-ENOENT read failure (unlike
// permission bits, this also works when tests run as root) without needing to fabricate a
// genuinely unreadable-but-present regular file. Before the fix, this test's second assertion
// would fail: loadOrCreateDeviceIdentity would return success with a freshly-generated DeviceID
// instead of surfacing the read error.
func TestLoadOrCreateDeviceIdentityDoesNotClobberOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// Put a directory where the device-id state file is expected, so os.ReadFile fails with a
	// non-ENOENT error ("is a directory") instead of the plain "file doesn't exist yet" case.
	if err := os.Mkdir(deviceIDStatePath(), 0700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	_, err := LoadOrCreateDeviceIdentity()
	if err == nil {
		t.Fatal("loadOrCreateDeviceIdentity: got nil error for a non-ENOENT device-id read failure, want an error -- it must not silently fabricate a replacement identity")
	}
	if !strings.Contains(buf.String(), "refusing to fabricate a replacement identity") {
		t.Errorf("expected a warning distinguishing this from the genuine first-run case, got: %s", buf.String())
	}

	// The directory must be left untouched -- no plain file should have been written over/into
	// it, confirming nothing was clobbered.
	fi, err := os.Stat(deviceIDStatePath())
	if err != nil {
		t.Fatalf("stat device id path: %v", err)
	}
	if !fi.IsDir() {
		t.Errorf("device id path is no longer a directory -- something wrote over it")
	}
}

// TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropGameUidOnReadFailure confirms readTrimmed's
// fix applies to GameUid/Username/LoginKey too, not just the device id: a non-ENOENT read failure
// on an EXISTING gameUid state file must not be silently treated as "" (which, per gslOptFor in
// login.go, would pick opt=new instead of the correct opt=fix even though the real gameUid was on
// disk the whole time). Before the fix this returned (identity, nil) with GameUid == ""; now it
// must surface the error instead.
func TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropGameUidOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// A directory in place of the gameUid state file reproduces a reliable non-ENOENT read
	// failure on an otherwise-legitimate device id (created fresh on this same call).
	if err := os.Mkdir(gameUidStatePath(), 0700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	_, err := LoadOrCreateDeviceIdentity()
	if err == nil {
		t.Fatal("loadOrCreateDeviceIdentity: got nil error for a non-ENOENT gameUid read failure, want an error rather than a silently-empty GameUid")
	}
	if !strings.Contains(buf.String(), "failed to read gameUid state file") {
		t.Errorf("expected a warning naming the failed gameUid read, got: %s", buf.String())
	}
}

// TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropUsernameOnReadFailure mirrors
// TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropGameUidOnReadFailure above, but for the
// username state file -- confirming readTrimmed's fix applies there too, not just to gameUid.
// Before the fix this returned (identity, nil) with Username == ""; now it must surface the
// error instead of silently discarding a real, already-persisted username.
func TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropUsernameOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// A directory in place of the username state file reproduces a reliable non-ENOENT read
	// failure on an otherwise-legitimate device id (created fresh on this same call).
	if err := os.Mkdir(usernameStatePath(), 0700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	_, err := LoadOrCreateDeviceIdentity()
	if err == nil {
		t.Fatal("loadOrCreateDeviceIdentity: got nil error for a non-ENOENT username read failure, want an error rather than a silently-empty Username")
	}
	if !strings.Contains(buf.String(), "failed to read username state file") {
		t.Errorf("expected a warning naming the failed username read, got: %s", buf.String())
	}
}

// TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropLoginKeyOnReadFailure mirrors
// TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropGameUidOnReadFailure above, but for the
// loginKey state file -- confirming readTrimmed's fix applies there too, not just to gameUid.
// Before the fix this returned (identity, nil) with LoginKey == ""; now it must surface the
// error instead of silently discarding a real, already-persisted loginKey.
func TestLoadOrCreateDeviceIdentityDoesNotSilentlyDropLoginKeyOnReadFailure(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	// A directory in place of the loginKey state file reproduces a reliable non-ENOENT read
	// failure on an otherwise-legitimate device id (created fresh on this same call).
	if err := os.Mkdir(loginKeyStatePath(), 0700); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	_, err := LoadOrCreateDeviceIdentity()
	if err == nil {
		t.Fatal("loadOrCreateDeviceIdentity: got nil error for a non-ENOENT loginKey read failure, want an error rather than a silently-empty LoginKey")
	}
	if !strings.Contains(buf.String(), "failed to read loginKey state file") {
		t.Errorf("expected a warning naming the failed loginKey read, got: %s", buf.String())
	}
}

// TestLoadOrCreateDeviceIdentitySelfHealsStaleEmptyDeviceIDFile confirms the fix for the bug
// where loadOrCreateDeviceIdentity permanently failed whenever the persisted device-id file
// exists on disk but is empty -- e.g. a prior process crashed/was OOM-killed/hit a full disk
// between createDeviceIDStateFile's O_CREATE|O_EXCL and its following WriteString, leaving a
// 0-byte file behind, with zero concurrency actually involved. Before the fix, readTrimmed
// correctly returned ("", nil) for this existing-but-empty file, the id=="" branch's
// O_CREATE|O_EXCL then failed with os.IsExist(err)==true purely because the empty file was
// already there, and the code unconditionally treated that as "a concurrent invocation raced us
// and finished first" -- re-reading, finding the same empty content, and returning a hard
// permanent error with no self-healing path, so every subsequent run repeated the identical
// failure forever. This test pre-creates an empty device-id file with no concurrency involved at
// all (just an empty file sitting there from the start) and asserts loadOrCreateDeviceIdentity
// now self-heals by writing a fresh identity into that same path instead of returning the old
// "device id file appeared concurrently but is empty" error.
func TestLoadOrCreateDeviceIdentitySelfHealsStaleEmptyDeviceIDFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := deviceIDStatePath()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	got, err := LoadOrCreateDeviceIdentity()
	if err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity: got error %v, want a self-healed identity instead of the old permanent failure", err)
	}
	if got.DeviceID == "" {
		t.Fatal("got empty DeviceID after self-heal, want a freshly generated one")
	}
	if !strings.HasSuffix(got.DeviceID, "_n3d") {
		t.Errorf("got DeviceID %q, want the release-build suffix _n3d", got.DeviceID)
	}
	if !strings.Contains(buf.String(), "self-healing with a fresh identity") {
		t.Errorf("expected a self-heal warning in the log output, got: %s", buf.String())
	}

	// The self-healed id must actually be persisted to disk (not just held in memory) and at the
	// same 0600 permissions the rest of the state-file lifecycle uses, so a subsequent run picks
	// up the same identity instead of repeating the self-heal (or worse, re-fabricating) forever.
	onDisk, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed after self-heal: %v", err)
	}
	if onDisk != got.DeviceID {
		t.Errorf("on-disk device id %q does not match returned DeviceID %q", onDisk, got.DeviceID)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("got mode %v for %s, want 0600", fi.Mode().Perm(), path)
	}

	// A second load must reuse the self-healed id rather than self-healing (or fabricating)
	// again.
	second, err := LoadOrCreateDeviceIdentity()
	if err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity (reload): %v", err)
	}
	if second.DeviceID != got.DeviceID {
		t.Errorf("got DeviceID %q on reload, want %q (should reuse the self-healed id)", second.DeviceID, got.DeviceID)
	}
}

// TestLoadOrCreateDeviceIdentityUsesGenuineConcurrentWriterContent confirms the os.IsExist
// re-read/retry path still does the right thing for the case it was originally built for:
// another invocation wins the O_CREATE|O_EXCL race and finishes writing real (non-empty) content
// shortly afterwards -- well within the retry window -- rather than the file staying empty
// forever. The device-id file is pre-created empty (so the initial readTrimmed sees "" and takes
// the id=="" branch, and createDeviceIDStateFile's O_CREATE|O_EXCL deterministically fails with
// os.IsExist since the file already exists), and a background goroutine populates it with real
// content 3ms later. That's long enough after the goroutine starts that the initial readTrimmed
// (which happens within microseconds, before the goroutine's sleep even elapses) still reliably
// observes "" and takes the retry path, but it leaves a wide (~22ms) margin before the retry
// loop's first post-sleep re-read at deviceIDEmptyRetryDelay (~25ms) -- unlike a delay close to
// 25ms, real-world scheduling/CI/-race jitter can't plausibly eat a 22ms margin, so this isn't a
// race against the production timing the way a tighter delay would be. Unlike
// TestLoadOrCreateDeviceIdentitySelfHealsStaleEmptyDeviceIDFile's file (which stays empty and
// must self-heal), content that shows up mid-retry must win outright -- the self-heal path added
// by that fix must never fire here, let alone overwrite it.
func TestLoadOrCreateDeviceIdentityUsesGenuineConcurrentWriterContent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	path := deviceIDStatePath()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	const wantID = "concurrent-writer-device-id_n3d"
	go func() {
		time.Sleep(3 * time.Millisecond)
		_ = os.WriteFile(path, []byte(wantID), 0o600)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	got, err := LoadOrCreateDeviceIdentity()
	if err != nil {
		t.Fatalf("loadOrCreateDeviceIdentity: %v", err)
	}
	if got.DeviceID != wantID {
		t.Errorf("got DeviceID %q, want %q (the genuine concurrent writer's content, not a self-healed replacement)", got.DeviceID, wantID)
	}
	if strings.Contains(buf.String(), "self-healing with a fresh identity") {
		t.Errorf("self-heal path fired for a genuine concurrent writer, want it to only adopt the existing content: %s", buf.String())
	}
}

// TestAirKeyMatchesKnownValue is AirKey()'s counterpart to selftest_test.go's
// TestPackageSignMatchesKnownValue -- AirKey() is just as wire-format-critical (it's the
// `lw_airKey`/`airKey` value the server echoes back and validates, per identity.go's AirKey doc
// comment and BuildLoginParams' usage), so it deserves the same golden-value rigor rather than
// only a round-trip/shape check.
func TestAirKeyMatchesKnownValue(t *testing.T) {
	d := &deviceIdentity{DeviceID: "abcdef0123456789abcdef0123456789_n3d"}
	// base64.StdEncoding of the DeviceID above, computed independently.
	const want = "lwDid_YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlfbjNk"
	if got := d.AirKey(); got != want {
		t.Errorf("AirKey() = %q, want %q", got, want)
	}
}

// TestLoadOrCreateDeviceIdentityRoundTrip confirms the full persisted-state lifecycle: a fresh
// HOME creates a new device identity, SaveGameUid/SaveUsername persist their values to disk at
// 0600, and a second load picks up exactly what was saved -- the same guarantee config_test.go's
// tests already confirm for SessionConfig.
func TestLoadOrCreateDeviceIdentityRoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	first, err := LoadOrCreateDeviceIdentity()
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

	second, err := LoadOrCreateDeviceIdentity()
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

// TestCreateDeviceIDStateFileNormalCreation confirms createDeviceIDStateFile's switch from a
// direct O_CREATE|O_EXCL+WriteString to write-temp-then-os.Link still produces exactly the same
// end result for the ordinary first-run case: the id lands at path, readable back verbatim, at
// 0600, with no stray "<base>.tmp-*" file left behind once Link has published it. Link (unlike
// Rename) doesn't remove the source name on its own -- it just adds a second directory entry
// pointing at the same inode -- so this also confirms createDeviceIDStateFile's explicit
// post-Link os.Remove cleanup actually runs, not just that the content made it to path.
func TestCreateDeviceIDStateFileNormalCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-id")

	const wantID = "fresh-device-id_n3d"
	if err := createDeviceIDStateFile(path, wantID); err != nil {
		t.Fatalf("createDeviceIDStateFile: %v", err)
	}

	got, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed: %v", err)
	}
	if got != wantID {
		t.Errorf("got %q, want %q", got, wantID)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("got mode %v for %s, want 0600", fi.Mode().Perm(), path)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("got directory entries %v, want exactly [%s] (no leftover temp file after Link)", names, filepath.Base(path))
	}
}

// TestCreateDeviceIDStateFileExclusivityOnExistingFile confirms the switch from
// O_CREATE|O_EXCL to write-temp-then-os.Link preserved the exact exclusivity semantics
// loadOrCreateDeviceIdentity's os.IsExist(err) race-detection depends on (see
// createDeviceIDStateFile's doc comment): calling it against a path that already has real content
// must fail with an os.IsExist error -- exactly like the old O_EXCL failure -- rather than
// silently overwriting it the way a naive write-temp-then-os.Rename fix would have (os.Rename
// doesn't fail when the destination already exists on POSIX -- it just clobbers it, which would
// defeat the whole point of the exclusivity check). The pre-existing content must survive
// untouched, and no stray temp file should be left behind.
func TestCreateDeviceIDStateFileExclusivityOnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-id")

	const existingID = "already-here-device-id_n3d"
	if err := os.WriteFile(path, []byte(existingID), 0o600); err != nil {
		t.Fatal(err)
	}

	err := createDeviceIDStateFile(path, "would-be-new-device-id_n3d")
	if err == nil {
		t.Fatal("createDeviceIDStateFile: got nil error against a pre-existing file, want an os.IsExist error")
	}
	if !os.IsExist(err) {
		t.Errorf("got error %v, want one satisfying os.IsExist -- loadOrCreateDeviceIdentity's race-detection branches on exactly this", err)
	}

	got, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed: %v", err)
	}
	if got != existingID {
		t.Errorf("existing content = %q after a failed createDeviceIDStateFile, want it left untouched at %q (os.Link must not clobber an existing destination, unlike os.Rename)", got, existingID)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("got directory entries %v, want exactly [%s] (temp file cleaned up after a failed Link)", names, filepath.Base(path))
	}
}

// TestWriteTempStateFileDoesNotTouchFinalPathBeforePublish simulates a crash in the window
// createDeviceIDStateFile's fix was specifically designed to close: after the temp file's content
// is fully written and fsync'd (durable), but before the Link call that publishes it ever runs.
// writeTempStateFile itself never touches finalPath -- it only creates and populates a
// separately-named temp file -- so a crash in exactly that window can only ever leave the temp
// file behind; finalPath is never partially written to, truncated, or otherwise touched. This is
// the property that makes publishing via os.Link safe: by the time Link runs, there is no way for
// it to expose anything other than the already-complete content, so it can only succeed outright
// (publishing the full content) or fail outright (leaving finalPath exactly as it was before) --
// never leave a torn/partial file visible at finalPath, unlike the old direct
// O_CREATE|O_EXCL+WriteString path this replaced, where a crash between those two syscalls left a
// truncated, non-empty device-id file sitting at the real path.
func TestWriteTempStateFileDoesNotTouchFinalPathBeforePublish(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-id-crash-sim")

	tmpPath, err := writeTempStateFile(path, "fully-written-and-durable-content")
	if err != nil {
		t.Fatalf("writeTempStateFile: %v", err)
	}
	// Simulate a crash right here -- after the content is fully durable on disk, but before any
	// caller has run Rename/Link to publish it.

	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("final path exists (or a stat error other than not-exist occurred) after a simulated crash before publish, want it completely untouched: err=%v", statErr)
	}

	got, err := readTrimmed(tmpPath)
	if err != nil {
		t.Fatalf("readTrimmed(tmpPath): %v", err)
	}
	if got != "fully-written-and-durable-content" {
		t.Errorf("temp file content = %q, want the content to already be fully durable at the temp path before publish is even attempted", got)
	}

	_ = os.Remove(tmpPath)
}

// TestSaveStateFileRoundTrip confirms saveStateFile's normal write-then-read round trip still
// works correctly after switching it from a plain os.WriteFile to the write-temp-then-rename
// atomicWriteStateFile helper (the same one atomicWriteDeviceIDStateFile's self-heal path already
// used): the target file must end up existing at the given path, at 0600, with exactly the
// written content -- not the temp file, not something left behind under a ".tmp-*" name.
func TestSaveStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state-roundtrip")

	const want = "some-persisted-value"
	if err := saveStateFile(path, want); err != nil {
		t.Fatalf("saveStateFile: %v", err)
	}

	got, err := readTrimmed(path)
	if err != nil {
		t.Fatalf("readTrimmed: %v", err)
	}
	if got != want {
		t.Errorf("got %q after round trip, want %q", got, want)
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

// TestSaveStateFileLeavesTargetUntouchedOnFailedWrite proves saveStateFile's switch to
// write-temp-then-rename actually protects an existing target from a torn write, not just that
// the happy path still works: if the final rename step fails, the target must be left completely
// untouched rather than ending up empty, truncated, or partially overwritten -- the exact class of
// bug a plain os.WriteFile (truncate-open + write, no rename) is exposed to on a crash mid-write.
//
// Forcing rename(2) to fail this way -- destination path is an existing directory -- is a
// type-mismatch failure the kernel reports the same way regardless of privilege, so (like
// identity_test.go's TestLoadOrCreateDeviceIdentityDoesNotClobberOnReadFailure and config_test.go's
// TestLoadEffectiveConfigExitsOnDefaultPathReadFailure) it also works when tests run as root,
// unlike a permission-bits-based failure trick would.
func TestSaveStateFileLeavesTargetUntouchedOnFailedWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state-target")

	// Put a directory where the state file is expected, so the temp file gets written and
	// fsync'd successfully (proving the write itself completed) but the final os.Rename fails
	// with a directory/type-mismatch error instead of silently replacing/merging into it.
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}

	if err := saveStateFile(path, "new-content"); err == nil {
		t.Fatal("saveStateFile: got nil error for a rename onto an existing directory, want an error")
	}

	// The target must still be exactly what it was before the failed save -- a directory, not a
	// regular file (torn, empty, or otherwise) written over/into it.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if !fi.IsDir() {
		t.Errorf("target path is no longer a directory after a failed saveStateFile -- something wrote over it, want it left completely untouched")
	}

	// The temp file (written and fsync'd before the failed rename) must be cleaned up rather than
	// leaked into the directory permanently.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(path) {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("got directory entries %v, want exactly [%s] (temp file cleaned up, target untouched)", names, filepath.Base(path))
	}
}
