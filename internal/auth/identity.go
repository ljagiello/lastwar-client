package auth

import (
	"bytes"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lastwar-client/internal/gsl"
	"lastwar-client/internal/sfs"
	"lastwar-client/internal/shumei"
	"log/slog"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// packageSignHex reproduces SDKManager.GetPackageSign() exactly:
// SHA1(PackageName), lowercase hex, no separators. (Confirmed from
// decompiled/Assembly-CSharp.decompiled.cs:93244 -- it hashes the package
// NAME string, not the certificate, despite the field's name.) Confirmed
// live for the iOS package too: SHA1("com.lastwar.ios") reproduces exactly
// the packageSign a real iOS client sent, captured via packet inspection.
func packageSignHex(forPackageName string) string {
	return sha1Hex(forPackageName)
}

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pshField is the Login's psh for the platform the Login claims (iosMode).
//
// Android (SdkManager.LW_PSH): MD5(cmdBaseTime + hex(signing-cert DER)), the base.apk certificate
// shumei.AppCertHex (signature.toCharsString(), extracted from the APK's signing block; shumei's
// TestAppCert pins its subject O=LCYD and hash).
// iOS (rg_getPsh:): MD5(cmdBaseTime + "7" + bundle id). rg_getPsh: puts "7" there when the app
// bundle has no embedded.mobileprovision, as in an App Store install, and "3" when it has one
// (static analysis only). goclient presents as the App Store app, so it sends "7". That matches
// two captures of the real App Store app (cmdBaseTime 1791877000 and 1792390017, 1.0.364/900),
// and a Login sending it into the real account was accepted on 2026-10-18 (confirmed live).
func pshField(iosMode bool, cmdBaseTime string) string {
	if iosMode {
		return md5Hex(cmdBaseTime + "7" + iosPackageName)
	}
	return md5Hex(cmdBaseTime + shumei.AppCertHex)
}

// securityCode reproduces the login field of the same name:
// MD5(cmdBaseTime + <hardcoded salt> + gameUid).
func securityCode(cmdBaseTime, gameUid string) string {
	const salt = "4d1c383ccbedf3d98320d6ea06d8dedc"
	return md5Hex(cmdBaseTime + salt + gameUid)
}

// randomDigitString reproduces StringUtils.GenerateRandomStr(n): n ASCII
// digit characters, not full alphanumeric.
func randomDigitString(n int) string {
	var b strings.Builder
	for range n {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(d.String())
	}
	return b.String()
}

// oneCodeAndCoreV reproduces the OneCode/CoreV construction from
// LoginMessage.CSSetData -- both derived from the same 32-digit rand32.
func oneCodeAndCoreV() (oneCode, coreV string) {
	return oneCodeAndCoreVFrom(randomDigitString(32))
}

// oneCodeAndCoreVFrom is oneCodeAndCoreV for a caller-supplied 32-digit seed.
// OneCode interleaves MD5(seed) with the seed. CoreV is MD5(reversed Base64(seed))
// interleaved with MD5(that digest + seed).
func oneCodeAndCoreVFrom(rand32 string) (oneCode, coreV string) {
	md5Rand := md5Hex(rand32)

	oneBytes := make([]byte, 0, 64)
	for i := range 32 {
		oneBytes = append(oneBytes, md5Rand[i], rand32[i])
	}
	oneCode = string(oneBytes)

	b64 := base64.StdEncoding.EncodeToString([]byte(rand32))
	b64Rev := reverseString(b64)
	md2 := md5Hex(b64Rev)
	md3 := md5Hex(md2 + rand32)

	coreBytes := make([]byte, 0, 64)
	for j := range 32 {
		coreBytes = append(coreBytes, md3[j], md2[j])
	}
	coreV = string(coreBytes)
	return
}

func reverseString(s string) string {
	r := []rune(s)
	for i, j := 0, len(r)-1; i < j; i, j = i+1, j-1 {
		r[i], r[j] = r[j], r[i]
	}
	return string(r)
}

// deviceIdentity is a stable, locally-persisted device identity so repeated
// runs present the same deviceId/airKey the server already associated with
// this "device" at the GSL step. Username, once known (the SFS login
// response's own `un` field -- a different identifier space from GSL's
// composite `gameUid`, see main.go), is persisted too so a returning run
// can present it instead of an empty username. GameUid (GSL's own field) is
// persisted so subsequent runs can correctly select opt=fix instead of
// opt=new -- dossier §02.2's opt table is keyed off "is uid empty", and
// re-sending opt=new for a device GSL already recognizes appears to cause
// the E011 rejection observed empirically.
type deviceIdentity struct {
	DeviceID  string
	AndroidID string // Settings.Secure.ANDROID_ID (16 lowercase hex), one per device, like DeviceID
	Username  string
	GameUid   string
	LoginKey  string
	// Zone is the zone of the role the -email flow bound ("APS783"), which the client caches as
	// SERVER_ZONE and sends as GSL `zone` (Assembly-CSharp.decompiled.cs:77411, 77437, 1.0.370);
	// BootstrapOwnDevice sends it. "" until a bind records one.
	Zone string
	// Android is the Android install's locally created values (install time, afuid, distinct_id);
	// zero for an identity loaded in iOS mode, which never creates them.
	Android AndroidInstall
}

// String/GoString are the round-48 regression fix for the MAJOR finding that deviceIdentity --
// which carries LoginKey, the single most sensitive credential in this client (alone sufficient to
// re-authenticate the account via opt=login, per this file's own doc comments) -- had no
// redaction-by-construction the way gsl.go's gsl.LoginToken got in round 47 for the identical class of
// risk. Login() keeps a live *deviceIdentity in scope for its entire multi-hundred-line body and
// hands it out further via LoginResult.Ident to every caller; every CURRENT call site already
// extracts and wraps LoginKey in sfs.Redact() before logging, so this is defense-in-depth for whatever
// comes next, not a fix for an actively-triggered leak -- a future slog.Error("login failed",
// "ident", ident) or fmt.Errorf("state: %+v", result) would otherwise fall through to Go's default
// reflection-based struct formatter and print LoginKey/GameUid/Username in clear text. Value
// receiver (not pointer) so both deviceIdentity and *deviceIdentity -- the type used throughout
// this codebase -- satisfy fmt.Stringer, mirroring gsl.LoginToken's identical choice.
func (d deviceIdentity) String() string   { return "[REDACTED deviceIdentity]" }
func (d deviceIdentity) GoString() string { return d.String() }

// LogValue makes deviceIdentity satisfy slog.LogValuer -- see gsl.go's gsl.LoginToken.LogValue for the
// full round-53 rationale. Value receiver, same reasoning as String()/GoString() above.
func (d deviceIdentity) LogValue() slog.Value { return slog.StringValue(d.String()) }

// StateFilePath is where a persisted credential/state file lives: $LASTWAR_STATE_DIR when set (so
// several independent device identities -- e.g. the cron's own device next to a config captured
// from the real app -- can coexist on one machine), otherwise the home directory.
func StateFilePath(name string) string {
	if dir := os.Getenv("LASTWAR_STATE_DIR"); dir != "" {
		return filepath.Join(dir, name)
	}
	dir, err := os.UserHomeDir()
	if err != nil {
		dir = "."
		// Credential state files (loginKey among them) would otherwise silently land in
		// whatever the current working directory happens to be -- e.g. $HOME unset in a
		// minimal container or misconfigured cron environment -- with no record of why.
		slog.Warn("could not determine home directory; persisting credential state files in the current working directory instead", "error", err, "dir", dir)
	}
	return filepath.Join(dir, name)
}

func deviceIDStatePath() string  { return StateFilePath(".lastwar_goclient_device_id") }
func androidIDStatePath() string { return StateFilePath(".lastwar_goclient_android_id") }
func usernameStatePath() string  { return StateFilePath(".lastwar_goclient_username") }
func gameUidStatePath() string   { return StateFilePath(".lastwar_goclient_gameuid") }
func loginKeyStatePath() string  { return StateFilePath(".lastwar_goclient_loginkey") }
func zoneStatePath() string      { return StateFilePath(".lastwar_goclient_zone") }

// readTrimmed reads path and returns its trimmed contents. A missing file (os.IsNotExist) is not
// an error -- it just means that piece of state hasn't been persisted yet (e.g. first run, or a
// GameUid/LoginKey not learned yet), and callers should treat it as "" the same as before. Any
// OTHER read error -- permission denied, I/O failure, a directory sitting where a file was
// expected, etc. -- is now returned as an error instead of being silently swallowed as "absent":
// treating a transient failure to read an EXISTING, valid state file the same as "no state yet"
// is exactly the bug that let LoadOrCreateDeviceIdentity fabricate and persist a brand-new random
// device ID over a real one the server already recognizes, and let a Username/GameUid/LoginKey
// read hiccup silently return "" and make login.go's gslOptFor pick the wrong opt value even
// though the real value was sitting on disk the whole time.
func readTrimmed(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// writeTempStateFile writes content to a brand-new, fsync'd temp file in the same directory as
// finalPath (so a subsequent same-directory Rename/Link is both atomic and guaranteed to stay on
// one filesystem), at 0600, and returns its path -- it never touches finalPath itself. This is
// the shared write+fsync durability step behind both ways this package publishes a state file:
//   - AtomicWriteStateFile (below) publishes via os.Rename, which silently overwrites an existing
//     destination -- correct for saveStateFile (loginKey/gameUid/username) and the device-id
//     empty-file self-heal path in LoadOrCreateDeviceIdentity, where clobbering whatever
//     (possibly stale/empty) content was already there is exactly the point.
//   - createDeviceIDStateFile (below) publishes via os.Link instead, which fails with an
//     os.IsExist-shaped error if the destination already exists rather than overwriting it --
//     required there to preserve the O_CREATE|O_EXCL-equivalent race-detection
//     LoadOrCreateDeviceIdentity's os.IsExist(err) handling depends on.
//
// Either way, because the content is fully written and durable on disk *before* the publish step
// even runs, a crash/OOM-kill/power-loss between this call returning and the caller's
// Rename/Link can only ever leave the temp file behind -- finalPath itself is never partially
// written to, so no torn/truncated content can ever become visible there.
func writeTempStateFile(finalPath, content string) (tmpPath string, err error) {
	dir := filepath.Dir(finalPath)
	tmp, err := os.CreateTemp(dir, filepath.Base(finalPath)+".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath = tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return "", err
	}
	cleanup = false
	return tmpPath, nil
}

// createDeviceIDStateFile persists a freshly-generated device id, but only if the file doesn't
// already exist. Exclusivity closes the TOCTOU gap between LoadOrCreateDeviceIdentity's earlier
// read-not-exists check and this write: if some other invocation created the file in between (the
// documented deployment is a single cron job per machine, so concurrent invocations aren't the
// primary realistic trigger here, but this is a basic safety margin at negligible cost), this
// fails with an os.IsExist error instead of silently clobbering whatever that other invocation
// already wrote -- the caller re-reads rather than overwrites in that case.
//
// The id is written to a temp file first (fsync'd, via the writeTempStateFile helper above, the
// same one AtomicWriteStateFile uses) and published with os.Link rather than a direct
// O_CREATE|O_EXCL+WriteString straight to path. The old direct-write approach had a window
// between O_CREATE|O_EXCL succeeding and WriteString completing where a crash/OOM-kill/ENOSPC
// left a partial, non-empty, non-zero-length device-id file behind -- which the empty-file
// self-heal logic in LoadOrCreateDeviceIdentity does NOT catch (it only self-heals a
// confirmed-empty leftover, never an already-non-empty-but-truncated one), so a truncated id
// could silently become the permanent device identity on every subsequent run. os.Link preserves
// the exact exclusivity os.O_EXCL provided -- it fails with an os.IsExist-shaped error if path
// already exists, exactly like the old O_EXCL failure, unlike os.Rename which would silently
// overwrite an existing destination and defeat the whole race-detection purpose this function
// exists for -- while still avoiding the torn-write window, since by the time Link runs the
// linked-to content is already fully written and fsync'd, so Link itself can only ever succeed
// outright or fail outright, never publish a partial file. The temp file is removed after a
// successful Link (the data itself survives at path via the surviving hard link; only the extra
// temp-name directory entry goes away) and also removed if Link fails, so no stray "<base>.tmp-*"
// file is ever left behind either way.
func createDeviceIDStateFile(path, id string) error {
	tmpPath, err := writeTempStateFile(path, id)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpPath) }()
	return os.Link(tmpPath, path)
}

// deviceIDEmptyRetryAttempts/deviceIDEmptyRetryDelay bound how long LoadOrCreateDeviceIdentity
// waits, after an O_CREATE|O_EXCL failure on the device-id state file, to tell a genuine
// concurrent writer (which will finish within milliseconds on the same machine) apart from a
// stale empty leftover from a prior crashed/OOM-killed/disk-full process (which never will). Kept
// small and bounded on purpose -- this must never become an unbounded or silent retry loop.
const (
	deviceIDEmptyRetryAttempts = 3
	deviceIDEmptyRetryDelay    = 25 * time.Millisecond
)

// AtomicWriteStateFile writes content to path via write-temp-then-rename instead of a plain
// O_WRONLY/O_TRUNC reopen (what os.WriteFile does under the hood). A plain truncate-then-write
// leaves a window -- open+truncate, then the write itself, as separate syscalls with no fsync --
// where a crash/OOM-kill/power-loss mid-write leaves a zero-length or partially-written file
// behind, or (for a concurrent writer instead of a crash) could race and clobber a
// slow-but-genuine concurrent writer's partial content. Rename within the same directory is
// atomic on POSIX filesystems, so any reader sees either the old complete content or the new
// complete content, never a torn write. Used by saveStateFile (loginKey/gameUid/username) and the
// device-id empty-file self-heal path in LoadOrCreateDeviceIdentity, where overwriting whatever
// was already at path is the desired behavior; createDeviceIDStateFile needs os.Link's stricter
// exclusivity instead (see writeTempStateFile's doc comment above), so it doesn't go through this
// function -- but it gets the same torn-write protection via the shared writeTempStateFile step.
func AtomicWriteStateFile(path, content string) error {
	tmpPath, err := writeTempStateFile(path, content)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmpPath) }() // no-op once the rename below succeeds -- the path no longer exists
	return os.Rename(tmpPath, path)
}

// loadOrCreateAndroidID returns the device's persisted Settings.Secure.ANDROID_ID (16 lowercase hex
// digits), generating and persisting one on first use, the way a real device has an ANDROID_ID from
// install. It is one value for the life of the device, used by both the ShuMei fingerprint's a24 and
// ThinkingData's ta #device_id, so a guest login and a session reconnect report the same ID.
func loadOrCreateAndroidID() (string, error) {
	id, err := readTrimmed(androidIDStatePath())
	if err != nil {
		return "", fmt.Errorf("read android id state: %w", err)
	}
	if id != "" {
		return id, nil
	}
	raw := make([]byte, 8) // 8 bytes -> 16 lowercase hex digits
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id = hex.EncodeToString(raw)
	if err := AtomicWriteStateFile(androidIDStatePath(), id); err != nil {
		return "", fmt.Errorf("persist android id: %w", err)
	}
	return id, nil
}

// LoadOrCreateDeviceIdentity loads this device's persisted identity, creating what is missing.
// iosMode is the platform a Login will claim. It shapes only what is created: a new Android
// deviceId is the device's Android ID + "_n3d", what Udid.getUid returns without an advertising id
// (com/sdkmanager/utils/Udid.java:75-83; DeviceManager.GetDeviceUid,
// Assembly-CSharp.decompiled.cs:73491-73506 of the Android build), and an Android identity gets its
// AndroidInstall. An existing deviceId is never rewritten: the server already knows it.
func LoadOrCreateDeviceIdentity(iosMode bool) (*deviceIdentity, error) {
	return LoadOrCreateDeviceIdentityInstalledAt(iosMode, nil)
}

// LoadOrCreateDeviceIdentityInstalledAt is LoadOrCreateDeviceIdentity with installedAt giving, for
// the identity's deviceId, the app install time another record of this install already reports
// (the session's ShuMei install, a125 fit). An AndroidInstall this call creates takes that time, so
// ta #install_time, the afuid's first launch just after it and that record describe one install
// (1.0.370 audit B31, B95). A nil installedAt, or a zero time from it, leaves a new install's time to
// the identity's creation. An AndroidInstall that already exists is never changed: its afuid has
// been sent.
func LoadOrCreateDeviceIdentityInstalledAt(iosMode bool, installedAt func(deviceID string) time.Time) (*deviceIdentity, error) {
	// created is when this identity was made: now for a new one, else when its device-id file was
	// written (the anchor of a new AndroidInstall, see loadOrCreateAndroidInstall).
	created := time.Now()
	if fi, err := os.Stat(deviceIDStatePath()); err == nil && fi.Size() > 0 {
		created = fi.ModTime()
	}
	id, err := readTrimmed(deviceIDStatePath())
	if err != nil {
		// A non-ENOENT failure reading an EXISTING device-id file must never be treated as "no
		// identity yet" -- doing so would fabricate and persist a brand-new random device ID
		// over the real one the server already recognizes, permanently losing it (and leaving
		// the fabricated deviceId/airKey -- AirKey() derives from DeviceID -- mismatched against
		// a still-valid persisted loginKey on the next login). Surface the real problem instead
		// of silently replacing the identity.
		slog.Warn("failed to read device id state file; refusing to fabricate a replacement identity", "path", deviceIDStatePath(), "error", err)
		return nil, fmt.Errorf("read device id state: %w", err)
	}
	if id == "" {
		if iosMode {
			// No 1.0.370 source gives the iOS native UDID (PM_getDeviceUDID): a random unique id
			// + "_n3d" (release build suffix), as this client has always created.
			raw := make([]byte, 16)
			if _, err := rand.Read(raw); err != nil {
				return nil, err
			}
			id = hex.EncodeToString(raw) + "_n3d"
		} else {
			androidID, err := loadOrCreateAndroidID()
			if err != nil {
				return nil, err
			}
			id = androidID + "_n3d"
		}
		if err := createDeviceIDStateFile(deviceIDStatePath(), id); err != nil {
			if os.IsExist(err) {
				// Another invocation created the file between our read-not-exists check above
				// and this write (see createDeviceIDStateFile's doc comment) -- OR the file is a
				// stale 0-byte leftover from a prior process that crashed/was OOM-killed/hit a
				// full disk between its own O_CREATE|O_EXCL and WriteString, with zero
				// concurrency actually involved this time. Both look identical right now
				// (os.IsExist(err) == true, file empty); the only way to tell them apart is to
				// give a genuine concurrent writer a brief window to finish, since on the same
				// machine that takes milliseconds. Re-read a few times with a short delay
				// between attempts: a real concurrent writer's content shows up well within that
				// window, while a stale empty leftover stays empty across every attempt.
				var reread string
				for attempt := range deviceIDEmptyRetryAttempts {
					if attempt > 0 {
						time.Sleep(deviceIDEmptyRetryDelay)
					}
					var rereadErr error
					reread, rereadErr = readTrimmed(deviceIDStatePath())
					if rereadErr != nil {
						return nil, fmt.Errorf("device id file appeared concurrently and could not be re-read: %w", rereadErr)
					}
					if reread != "" {
						break
					}
				}
				if reread == "" {
					// Still empty after giving a genuine concurrent writer several chances to
					// finish -- this is a stale leftover, not an in-progress write. Self-heal by
					// writing a fresh identity atomically instead of returning a permanent error
					// that every subsequent run would repeat forever until a human intervened by
					// hand.
					slog.Warn("device id state file exists but is empty after retries; self-healing with a fresh identity", "path", deviceIDStatePath(), "attempts", deviceIDEmptyRetryAttempts)
					if err := AtomicWriteStateFile(deviceIDStatePath(), id); err != nil {
						return nil, fmt.Errorf("self-heal empty device id file: %w", err)
					}
				} else {
					id = reread
				}
			} else {
				return nil, fmt.Errorf("persist device id: %w", err)
			}
		}
	}
	// deviceId+gameUid alone (no loginKey) are sufficient to attempt an
	// account-resolving GSL call via login.go's gslOptFor (opt=fix case), so
	// every persisted state file gets the same loose-permissions check, not
	// just loginKey.
	warnIfLoosePermissions(deviceIDStatePath())
	warnIfLoosePermissions(androidIDStatePath())
	warnIfLoosePermissions(usernameStatePath())
	warnIfLoosePermissions(gameUidStatePath())
	warnIfLoosePermissions(loginKeyStatePath())
	warnIfLoosePermissions(zoneStatePath())
	warnIfLoosePermissions(androidInstallStatePath())
	androidID, err := loadOrCreateAndroidID()
	if err != nil {
		slog.Warn("failed to load or create the android id state file", "path", androidIDStatePath(), "error", err)
		return nil, err
	}
	var android AndroidInstall
	if !iosMode {
		var installed time.Time
		if installedAt != nil {
			installed = installedAt(id)
		}
		if android, err = loadOrCreateAndroidInstall(created, installed); err != nil {
			slog.Warn("failed to load or create the android install state file", "path", androidInstallStatePath(), "error", err)
			return nil, err
		}
	}
	username, err := readTrimmed(usernameStatePath())
	if err != nil {
		slog.Warn("failed to read username state file", "path", usernameStatePath(), "error", err)
		return nil, fmt.Errorf("read username state: %w", err)
	}
	gameUid, err := readTrimmed(gameUidStatePath())
	if err != nil {
		slog.Warn("failed to read gameUid state file", "path", gameUidStatePath(), "error", err)
		return nil, fmt.Errorf("read gameUid state: %w", err)
	}
	loginKey, err := readTrimmed(loginKeyStatePath())
	if err != nil {
		slog.Warn("failed to read loginKey state file", "path", loginKeyStatePath(), "error", err)
		return nil, fmt.Errorf("read loginKey state: %w", err)
	}
	zone, err := readTrimmed(zoneStatePath())
	if err != nil {
		slog.Warn("failed to read zone state file", "path", zoneStatePath(), "error", err)
		return nil, fmt.Errorf("read zone state: %w", err)
	}
	return &deviceIdentity{
		DeviceID:  id,
		AndroidID: androidID,
		Username:  username,
		GameUid:   gameUid,
		LoginKey:  loginKey,
		Zone:      zone,
		Android:   android,
	}, nil
}

// warnIfLoosePermissions logs a warning if path exists and is readable/writable by group or
// other -- mirrors the equivalent check config.go's LoadSessionConfig already does for the
// session config file, applied here to all four persisted device-identity files. loginKey is
// the most sensitive (alone sufficient to log back into the real account via opt=login), but
// deviceId+gameUid together are enough to attempt an account-resolving GSL call (opt=fix, see
// login.go's gslOptFor), so they get the same treatment.
func warnIfLoosePermissions(path string) {
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	if mode := fi.Mode().Perm(); mode&0077 != 0 {
		slog.Warn("state file is more permissive than 0600", "path", path, "mode", mode)
	}
}

// saveStateFile persists data to path (used by SaveLoginKey/SaveGameUid/SaveUsername) via the
// same write-temp-then-rename AtomicWriteStateFile helper the device-id state file already uses
// -- a plain os.WriteFile does a truncate-open + write + close as separate syscalls with no
// fsync and no rename, so a crash/OOM-kill/power-loss mid-write could leave a zero-length or
// partially-written loginKey/gameUid/username file behind, the same class of bug the device-id
// file was hardened against. As a side effect of always writing through a fresh 0600 temp file
// and renaming it into place, this also settles the "existing file left world/group-readable"
// gotcha os.WriteFile has (its mode argument only applies on file creation): rename replaces the
// target's inode outright, so the destination always ends up with the temp file's 0600 bits
// regardless of what permissions the file being replaced had.
func saveStateFile(path, data string) error {
	return AtomicWriteStateFile(path, data)
}

// maxIdentityFieldLen bounds how long a value SaveLoginKey/SaveGameUid/SaveUsername will accept --
// round-46 fix, the same wire-tag-equivalence gap round 45 closed for mail.go's uid field:
// GetString (sfsobject.go) can't distinguish the 65535-byte-capped sfs.SFSUtfString wire tag from the
// ~64MiB-capped sfs.SFSText tag, both of which decode to the same Go string, so a server response
// tagging loginKey/gameUid/un/gameUserName as sfs.SFSText could previously smuggle an arbitrarily large
// value straight into d.LoginKey/d.GameUid/d.Username and onto disk. Two concrete harms: an
// unbounded-size write to a credential state file (saveStateFile below has no size check of its
// own), and -- more seriously -- BuildLoginParams/DoCrossServerLogin unconditionally re-embed
// these persisted values into every future login/cross-server-login request via PutUtfString,
// whose underlying sfs.WriteUtfString (sfsobject.go) hard-rejects any string over 32767 bytes at
// encode time -- so an oversized persisted value would permanently break every subsequent login
// attempt using that identity, in-memory for the rest of the current session and on disk for every
// future run, until an operator manually intervened. Rejecting before either the in-memory field
// assignment or the disk write (keeping the previous, presumably-valid value) closes both harms at
// once, exactly mirroring mail.go's "skip a malformed/hostile field, don't let it corrupt state"
// discipline. Set at the encoder's own hard limit (sfs.MaxUtfStringBytes, 32767 since the SFS2X
// WriteUTF cap was adopted), matching maxMailUidLen's own rationale: any value this accepts is
// guaranteed re-encodable later.
const maxIdentityFieldLen = sfs.MaxUtfStringBytes

func (d *deviceIdentity) SaveLoginKey(key string) error {
	if len(key) > maxIdentityFieldLen {
		return fmt.Errorf("identity: loginKey too long to persist (%d bytes, max %d)", len(key), maxIdentityFieldLen)
	}
	d.LoginKey = key
	return saveStateFile(loginKeyStatePath(), key)
}

func (d *deviceIdentity) SaveGameUid(uid string) error {
	if len(uid) > maxIdentityFieldLen {
		return fmt.Errorf("identity: gameUid too long to persist (%d bytes, max %d)", len(uid), maxIdentityFieldLen)
	}
	d.GameUid = uid
	return saveStateFile(gameUidStatePath(), uid)
}

// SaveZone persists the bound role's zone (deviceIdentity.Zone).
func (d *deviceIdentity) SaveZone(zone string) error {
	if len(zone) > maxIdentityFieldLen {
		return fmt.Errorf("identity: zone too long to persist (%d bytes, max %d)", len(zone), maxIdentityFieldLen)
	}
	d.Zone = zone
	return saveStateFile(zoneStatePath(), zone)
}

// SaveUsername persists the SFS login response's `un` field so the next run
// can present it instead of an empty username.
func (d *deviceIdentity) SaveUsername(un string) error {
	if len(un) > maxIdentityFieldLen {
		return fmt.Errorf("identity: username too long to persist (%d bytes, max %d)", len(un), maxIdentityFieldLen)
	}
	d.Username = un
	return saveStateFile(usernameStatePath(), un)
}

func (d *deviceIdentity) AirKey() string {
	return "lwDid_" + base64.StdEncoding.EncodeToString([]byte(d.DeviceID))
}

// LoginParamsInput holds the fields BuildLoginParams (below) needs to build the base SFS zone
// login request. uid is empty on a brand-new device (the server assigns one back via GSL's
// serverList[].gameUid, which the caller should pass in as gameUid once known).
type LoginParamsInput struct {
	FutureID    int32
	DeviceID    string
	AirKey      string
	GameUid     string
	AccessTok   string // "at" from the GSL response, if any
	ServerID    string // zone minus "APS" prefix
	ShumeiBoxId string // anti-fraud device-fingerprint token, if a real one is known
	AndroidID   string // Settings.Secure.ANDROID_ID (16 hex), ThinkingData's ta #device_id

	// ShumeiID is the ShuMei id the server pushed to this device (push.shumei.id), which the
	// client caches as LW_CacheShumeiDeviceId and reports as ta lw_shumei_id in every later Login
	// (Net/Msgs/PushShumeiIdMessage.lua:14-18; Assembly-CSharp.decompiled.cs:59117-59125, 1.0.370).
	// It is not the shumeiBoxId. "" is what a client sends before its first push.
	ShumeiID string

	// IOSMode switches PackageName/packageSign/Platform/pf/psh (and adds the
	// iOS-only idfa/idfv/phone_native_screen fields) to match a real iOS
	// client instead of the default Android identity. Confirmed live: an
	// `at` access token is bound to the PackageName/Platform it was issued
	// for -- reusing a token obtained under one Platform's identity while
	// claiming a different Platform/package gets ec=28/E005, even though
	// the token itself, un, and every other field are valid and accepted.
	// (Those Android Logins also sent ta as "{}", which on its own gets
	// E005 for an established account; see BuildLoginParams and #22.)
	IOSMode bool

	// AppVersion/VersionCode, when non-empty, replace the build identity BuildLoginParams would
	// otherwise send (gsl.AppVersion/gsl.VersionCode, or iosDefaultAppVersion/iosDefaultVersionCode
	// under IOSMode). A token is bound to the build it was issued under (confirmed live, see
	// IOSMode's comment), so once the real app updates, a token captured from it only works if these
	// carry that capture's appVersion/versionCode -- cmd/pcap -session-out records both.
	AppVersion  string
	VersionCode string

	// ResVersion is the hot-update state to report (see ResVersionFrom); "" sends "0", the value
	// this client sent for months. It also fills ta's lw_res_version.
	ResVersion string

	// NoZstd leaves isUseLz4 out of the Login, as the client does once check-version turns
	// ClientSwitch 4 or 5 on (gsl.CheckVersionResponse.NewPacketZstd). The zero value sends it.
	NoZstd bool

	// Device, under IOSMode, replaces the placeholder device fields with the ones a capture of
	// the real iOS app recorded (see CapturedDevice). Ignored on Android, whose device fields come
	// from the hardware profile the deviceId selects.
	Device CapturedDevice

	// Android is the Android install's afuid, distinct_id and install time (ta #install_time);
	// ignored under IOSMode. A zero value sends what this client sent before it had them.
	Android AndroidInstall
}

// The audited iOS build, 1.0.370/918 (Info.plist). A GSL-issued `at` access token is bound to the
// exact AppVersion/VersionCode it was obtained under, same as PackageName/Platform -- confirmed
// live: our hardcoded Android 1.0.351/1835 build numbers, sent alongside a real captured iOS token
// issued to 1.0.344/786, still got ec=28/E005 even after PackageName/Platform/pf matched. There is
// no known way to derive these generically the way packageSign derives from PackageName, so a
// session config carries the capture's own appVersion/versionCode once the real app moves on.
const (
	iosDefaultAppVersion  = "1.0.370"
	iosDefaultVersionCode = "918"
	iosPlatformName       = "iOS" // GameUtility.GetPlatformName() on iOS (BU:1945-1961)
)

// The builds a reconnect claimed before the defaults moved to 1.0.370: Android 1.0.364/1892 and the
// first captured iOS session's 1.0.344/786. A session config that names no appVersion/versionCode
// was made, and its token issued, under these, so a reconnect from one keeps claiming them
// (LegacyConfigBuild); every fresh bind claims the current defaults.
const (
	legacyAndroidAppVersion, legacyAndroidVersionCode = "1.0.364", "1892"
	legacyIOSAppVersion, legacyIOSVersionCode         = "1.0.344", "786"
)

// LegacyConfigBuild is the build a reconnect from a session config without appVersion/versionCode
// claims: the defaults that config's token was issued under before 1.0.370.
func LegacyConfigBuild(iosMode bool) (appVersion, versionCode string) {
	if iosMode {
		return legacyIOSAppVersion, legacyIOSVersionCode
	}
	return legacyAndroidAppVersion, legacyAndroidVersionCode
}

// loginBuild is the client build a Login claims: gsl.AndroidBuild, or the iOS build under iosMode,
// with a session config's appVersion/versionCode (when non-empty) replacing the defaults.
func loginBuild(iosMode bool, appVersion, versionCode string) gsl.ClientBuild {
	b := gsl.AndroidBuild
	if iosMode {
		b = gsl.ClientBuild{PackageName: iosPackageName, Platform: iosPlatformName,
			AppVersion: iosDefaultAppVersion, VersionCode: iosDefaultVersionCode}
	}
	if appVersion != "" {
		b.AppVersion = appVersion
	}
	if versionCode != "" {
		b.VersionCode = versionCode
	}
	return b
}

// storefrontPF is the `pf` storefront a client of this platform sends, on Login and on
// account.login.new alike: "AppStore" on iOS, "market_global" on Android.
func storefrontPF(iosMode bool) string {
	if iosMode {
		return "AppStore"
	}
	return "market_global"
}

// String/GoString are the round-48 regression fix for the MINOR finding that LoginParamsInput --
// which carries AccessTok/GameUid, live credentials -- had no redaction-by-construction, the same
// class of gap round 47/48 closed for gsl.LoginToken/deviceIdentity/SessionConfig. Every current call
// site (login.go/crossserver.go) constructs and immediately passes this struct into
// BuildLoginParams without logging it directly, so this is defense-in-depth, not an active leak
// fix -- rated minor rather than major since LoginParamsInput is short-lived (constructed and
// consumed within a single BuildLoginParams call) rather than held across a whole
// multi-hundred-line flow.
func (in LoginParamsInput) String() string   { return "[REDACTED LoginParamsInput]" }
func (in LoginParamsInput) GoString() string { return in.String() }

// LogValue makes LoginParamsInput satisfy slog.LogValuer -- see gsl.go's gsl.LoginToken.LogValue for
// the full round-53 rationale.
func (in LoginParamsInput) LogValue() slog.Value { return slog.StringValue(in.String()) }

const iosPackageName = "com.lastwar.ios"

// analyticsBlob mirrors the JSON shape of the `ta` analytics blob a real login sends (captured
// from iOS here, and from Android by C485, discussion #22), so BuildLoginParams can build it
// field-by-field instead of as a giant hand-maintained JSON string literal.
type analyticsBlob struct {
	OS              string `json:"#os"`
	Disk            string `json:"#disk"`
	DeviceID        string `json:"#device_id"`
	OSVersion       string `json:"#os_version"`
	SystemLanguage  string `json:"#system_language"`
	BundleID        string `json:"#bundle_id"`
	Carrier         string `json:"#carrier"`
	AppVersion      string `json:"#app_version"`
	FPS             int    `json:"#fps"`
	RAM             string `json:"#ram"`
	Simulator       bool   `json:"#simulator"`
	ZoneOffset      int    `json:"#zone_offset"`
	Manufacturer    string `json:"#manufacturer"`
	ScreenHeight    int    `json:"#screen_height"`
	NetworkType     string `json:"#network_type"`
	DeviceModel     string `json:"#device_model"`
	ScreenWidth     int    `json:"#screen_width"`
	InstallTime     string `json:"#install_time"`
	DeviceType      string `json:"#device_type,omitempty"` // Android only (BuildLoginParams)
	LwNet           string `json:"lw_net"`
	LwResVersion    string `json:"lw_res_version"`
	LwBuildcode     string `json:"lw_buildcode"`
	LwLine          string `json:"lw_line"`
	PdDl            int    `json:"pd_dl"`
	LwAb            string `json:"lw_ab"`
	LwPlatform      string `json:"lw_platform"`
	LwGameSessionID string `json:"lw_game_session_id"`
	LwFirstLaunch   bool   `json:"lw_first_launch"`
	LwAllianceID    string `json:"lw_alliance_id"`
	LwDeviceID      string `json:"lw_device_id"`
	LwDeviceLevel   int    `json:"lw_device_level"`
	LwMainLevel     int    `json:"lw_main_level"`
	LwShumeiID      string `json:"lw_shumei_id"`
	LwAirKey        string `json:"lw_airKey"`
	LwVersion       string `json:"lw_version"`
	LwPower         int    `json:"lw_power"`
	LwZone          string `json:"lw_zone"`
}

// androidSystemGiB is what the system partitions of a catalog phone take off its sold storage,
// so ta #disk's total is the data volume StatFs reports.
const androidSystemGiB = 9.0

// availOfTotal formats ThinkingData's "avail/total" GiB pair: each value rounded half up to one
// decimal (utils/q.java:111-113) and printed with one.
func availOfTotal(avail, total float64) string {
	round := func(d float64) float64 { return math.Floor(d*10+0.5) / 10 }
	return fmt.Sprintf("%.1f/%.1f", round(avail), round(total))
}

// thinkingDataCarriers is ThinkingData's own carrier name table, keyed by getSimOperator()
// (cn/thinkingdata/android/j.java:51-65): mainland China operators only.
var thinkingDataCarriers = map[string]string{
	"46000": "中国移动", "46002": "中国移动", "46007": "中国移动", "46008": "中国移动",
	"46001": "中国联通", "46006": "中国联通", "46009": "中国联通",
	"46003": "中国电信", "46005": "中国电信", "46011": "中国电信",
	"46004": "中国卫通", "46020": "中国铁通",
}

// thinkingDataCarrier is the ta #carrier ThinkingData reports on Android (j.java:225-238): its
// table's name for the SIM operator, else getSimOperatorName(), else "" (1.0.370 audit B91).
func thinkingDataCarrier(simOperator, simOperatorName string) string {
	if name, ok := thinkingDataCarriers[simOperator]; ok && simOperator != "" {
		return name
	}
	return simOperatorName
}

// taKey is one ta key a replayed capture takes from this Login (taRefresh).
type taKey struct{ key, val string }

// taRefresh lists the ta keys a replayed iOS capture takes from this Login instead of the
// capture, each with the 1.0.370 client line that sets it (Assembly-CSharp.decompiled.cs). The
// Login's ta is ThinkingData's preset properties followed by the game's super properties
// (GetTaPresetProp, :59270-59279, sent at :83072). Every other key keeps the captured value.
//   - #app_version: the ThinkingData preset app version, the build this Login claims.
//   - lw_version, lw_res_version, lw_buildcode: GameEntry.Sdk.Version, Resource.GetResVersion()
//     and Sdk.VersionCode, which LoginTracking writes after every Login (:59190-59201).
//   - lw_device_id, lw_airKey, lw_shumei_id: SetDeviceIdSuperProperty writes them on entering
//     check-version, before every Login (:9334, 59117-59125): GetDeviceUid() and
//     GetDeviceUid_Transcoding(), the Login's own deviceId and airKey (:83092-83093), and the
//     cached push.shumei.id value (PlayerPrefs LW_CacheShumeiDeviceId, :59119).
//
// An empty val keeps the captured value: this Login doesn't know it (no resVersion from
// check-version, no shumeiId pushed yet), and the client's cache keeps the last one it had.
func taRefresh(appVersion, versionCode, resVersion, deviceID, airKey, shumeiID string) []taKey {
	return []taKey{
		{"#app_version", appVersion},
		{"lw_version", appVersion},
		{"lw_res_version", resVersion},
		{"lw_buildcode", versionCode},
		{"lw_device_id", deviceID},
		{"lw_airKey", airKey},
		{"lw_shumei_id", shumeiID},
	}
}

// replayTa returns the captured ta blob with each refresh key's value replaced, keeping the
// captured key order and the raw bytes of every other value. A refresh key the capture lacks is
// appended (a super property the client sets after the capture's Login). It fails when captured
// is not one JSON object.
func replayTa(captured string, refresh []taKey) (string, error) {
	enc := func(s string) []byte {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		_ = e.Encode(s) // a string always encodes
		return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
	}
	byKey := make(map[string]int, len(refresh))
	for i, r := range refresh {
		if r.val != "" {
			byKey[r.key] = i
		}
	}
	dec := json.NewDecoder(strings.NewReader(captured))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", errors.New("the captured ta is not a JSON object")
	}
	var out bytes.Buffer
	out.WriteByte('{')
	put := func(key string, val []byte) {
		if out.Len() > 1 {
			out.WriteByte(',')
		}
		out.Write(enc(key))
		out.WriteByte(':')
		out.Write(val)
	}
	seen := make(map[string]bool, len(byKey))
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("the captured ta: %w", err)
		}
		key, _ := tok.(string) // an object's keys are always strings
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return "", fmt.Errorf("the captured ta: %w", err)
		}
		if i, ok := byKey[key]; ok {
			raw, seen[key] = enc(refresh[i].val), true
		}
		put(key, raw)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return "", errors.New("the captured ta is not a JSON object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("the captured ta has data after its object")
	}
	for _, r := range refresh {
		if _, ok := byKey[r.key]; ok && !seen[r.key] {
			put(r.key, enc(r.val))
		}
	}
	out.WriteByte('}')
	return out.String(), nil
}

// BuildLoginParams builds the full ~50-field sfs.SFSObject for the base SFS zone login (dossier §05 /
// §2.3). uid is empty on a brand-new device (the server assigns one back via GSL's
// serverList[].gameUid, which the caller should pass in as gameUid once known).
func BuildLoginParams(in LoginParamsInput) *sfs.SFSObject {
	now := time.Now().Unix()
	cmdBaseTime := strconv.FormatInt(now, 10)
	oneCode, coreV := oneCodeAndCoreV()

	p := sfs.NewSFSObject()
	p.PutInt("_id", in.FutureID)
	// DeviceManager.GetNetworkStatus (Assembly-CSharp.decompiled.cs:73295-73303, 1.0.370): 1 is
	// ReachableViaLocalAreaNetwork (wifi), 2 carrier data. ta (lw_net "wifi") and Android mt
	// (NetWork:Wifi) describe wifi, so netType does too.
	p.PutInt("netType", 1)
	build := loginBuild(in.IOSMode, in.AppVersion, in.VersionCode)
	effectiveAppVersion, effectiveVersionCode := build.AppVersion, build.VersionCode
	// The Android device model the Login's device fields describe, matched to
	// the ShuMei fingerprint the same deviceId selects (shumei.HardwareFor), so
	// phone_model, osVersion, device_string, mt, AndroidID, IMEI and the ta
	// blob all agree with the aNN profile. iOS Logins carry iOS fields
	// (idfa/idfv) instead of a build/hardware dump, so hw stays zero there.
	var hw shumei.Hardware
	// blobCarrier is ta #carrier: "" on iOS as before; on Android ThinkingData's own carrier name
	// for the SIM (thinkingDataCarrier), set once hw is known.
	var blobCarrier string
	if !in.IOSMode {
		hw = shumei.HardwareFor(in.DeviceID)
		blobCarrier = thinkingDataCarrier(hw.SimOperator, hw.SimOperatorName)
	}
	// resVersion "0" and ta's lw_res_version "0.0" are what this client sent for months, and the
	// server accepted them. A derived value goes into both, as in the real client's ta.
	resVersion, lwResVersion := "0", "0.0"
	if in.ResVersion != "" {
		resVersion, lwResVersion = in.ResVersion, in.ResVersion
	}
	// The ta blob's per-platform values. lw_version is GameEntry.Sdk.Version, the claimed
	// appVersion, on both platforms (the super properties, Assembly-CSharp.decompiled.cs:59189-59193,
	// 1.0.370); the iOS blob used to send "0.0.0". A real Android ta reports the device
	// model/manufacturer/screen/RAM/OS from Build; the iOS blob keeps the "unknown"/"0.0"/"0.0/0.0"
	// the cron's iOS session has logged in with since August 2026 (no 1.0.370 source gives the iOS
	// ThinkingData preset values).
	osName, lwVersion := "Android", effectiveAppVersion
	var manufacturer, deviceModel string
	blobOSVersion, blobRAM, blobInstallTime, blobDeviceID := "0.0", "0.0/0.0", "2000-01-01 00:00:00.000", "00000000-0000-0000-0000-000000000000"
	blobDisk, deviceType := "50.0/100.0", ""
	blobFPS, screenWidth, screenHeight := 0, 0, 0
	// dev is the captured iOS device (CapturedDevice); Android takes its device from hw.
	var dev CapturedDevice
	if in.IOSMode {
		osName, manufacturer, deviceModel = "iOS", "Apple", "unknown"
		dev = in.Device.encodable()
	} else {
		manufacturer = hw.Build["manufacturer"]
		deviceModel = hw.Model()
		blobOSVersion = hw.AndroidVersion
		screenWidth, screenHeight = hw.ScreenWH()
		// #ram and #disk are avail/total in GiB, each rounded to one decimal
		// (cn/thinkingdata/android/j.java:374-378, 399-402; utils/q.java:111-113), so the 8 GiB
		// catalog phone reports a total of 8.0, as ShuMei a48 describes the same RAMBytes. #disk is
		// the data volume: the GB the phone is sold with, in GiB, less what the system partitions
		// take.
		const gib = 1 << 30
		ramTotal := float64(hw.RAMBytes) / gib
		blobRAM = availOfTotal(ramTotal*0.35, ramTotal)
		diskTotal := float64(hw.StorageGB)*1e9/gib - androidSystemGiB
		blobDisk = availOfTotal(diskTotal*0.45, diskTotal)
		blobFPS = 60
		blobDeviceID = in.AndroidID
		// #install_time is PackageInfo.firstInstallTime as "yyyy-MM-dd HH:mm:ss.SSS" in the zone
		// #zone_offset names, UTC here (j.java:78-79, 282-283).
		if !in.Android.InstallTime.IsZero() {
			blobInstallTime = in.Android.InstallTime.UTC().Format("2006-01-02 15:04:05.000")
		}
		// #device_type is the Android SDK's 19th preset key, "Phone" for every catalog phone
		// (cn/thinkingdata/android/ThinkingAnalyticsSDK.java:916-918, utils/q.java:707-709). The
		// iOS blob has none.
		deviceType = "Phone"
	}

	// The ta analytics blob is part of the login gate for an established account. Confirmed live
	// on 2026-10-13 (found by C485, discussion #22): an Android Login into the real account with an
	// Android-issued token and ta="{}" gets ec=28/E005, and the same Login with this blob is
	// accepted. A guest Login was accepted with "{}" and is with this blob too. Which keys the
	// server reads isn't isolated; the platform markers (#os, #bundle_id, lw_platform) are the
	// likely ones. Every Android E005 this project recorded before also sent "{}", which the
	// platform-binding reading in crossserver.go never separated out.
	//
	// lw_device_id, lw_airKey and lw_shumei_id: before every Login the client sets them to its
	// deviceId, its airKey and the shumeiId the server last pushed (SetDeviceIdSuperProperty,
	// Assembly-CSharp.decompiled.cs:9334, 59117-59125, 1.0.370; see taRefresh). The iOS blob
	// fills them that way (1.0.370 audit B15, the user's decision of 2026-11-19). They were blank
	// because the raw blob once reached a log: ta is now itself a redacted key
	// (sfs.SensitiveSFSKeys), and no code logs or stores the raw value. lw_shumei_id is never the
	// shumeiBoxId. The Android blob keeps them blank: the same lines run on Android
	// (AND-CS:59196-59204), but the decision covers the iOS identity.
	var lwDeviceID, lwAirKey, lwShumeiID string
	if in.IOSMode {
		lwDeviceID, lwAirKey, lwShumeiID = in.DeviceID, in.AirKey, in.ShumeiID
	}
	blob := analyticsBlob{
		OS: osName, Disk: blobDisk, DeviceID: blobDeviceID,
		OSVersion: blobOSVersion, SystemLanguage: "en", BundleID: build.PackageName, Carrier: blobCarrier,
		AppVersion: effectiveAppVersion, FPS: blobFPS, RAM: blobRAM, Simulator: false, ZoneOffset: 0,
		Manufacturer: manufacturer, ScreenHeight: screenHeight, NetworkType: "WIFI", DeviceModel: deviceModel,
		ScreenWidth: screenWidth, InstallTime: blobInstallTime, DeviceType: deviceType,
		LwNet: "wifi", LwResVersion: lwResVersion, LwBuildcode: effectiveVersionCode, LwLine: "",
		PdDl: 0, LwAb: "0", LwPlatform: storefrontPF(in.IOSMode),
		LwGameSessionID: "00000000-0000-0000-0000-000000000000", LwFirstLaunch: false,
		LwAllianceID: "00000000000000000000000000000000",
		LwDeviceID:   lwDeviceID, LwDeviceLevel: 0, LwMainLevel: 0,
		LwShumeiID: lwShumeiID, LwAirKey: lwAirKey,
		LwVersion: lwVersion, LwPower: 0, LwZone: "APS" + in.ServerID,
	}
	// lw_zone repeats the zone, which DoCrossServerLogin accepts up to the full string cap; a blob
	// that would no longer encode falls back to "{}" rather than making the whole Login unsendable.
	// An established account rejected an Android Login with "{}" (E005), so it is logged. The blob
	// holds only strings, ints and bools, so Marshal can't fail.
	ta := "{}"
	if b, _ := json.Marshal(blob); len(b) <= sfs.MaxUtfStringBytes {
		ta = string(b)
	} else {
		slog.Warn("ta: the analytics blob is too long to encode; sending {}, which an established account rejected with E005 (seen on Android)",
			"len", len(b), "max", sfs.MaxUtfStringBytes, "zoneLen", len(in.ServerID))
	}
	// An iOS session captured from the real app replays that app's own ta, with the keys the client
	// rewrites per build or per Login taken from this one (taRefresh). A captured blob this can't
	// use leaves the one built above.
	if dev.Ta != "" {
		refresh := taRefresh(effectiveAppVersion, effectiveVersionCode, in.ResVersion, in.DeviceID, in.AirKey, in.ShumeiID)
		switch replayed, err := replayTa(dev.Ta, refresh); {
		case err != nil:
			slog.Warn("ta: the session config's captured ta can't be replayed; sending the built blob. Recapture with cmd/pcap -session-out", "error", err)
		case len(replayed) > sfs.MaxUtfStringBytes:
			slog.Warn("ta: the replayed capture is too long to encode; sending the built blob", "len", len(replayed), "max", sfs.MaxUtfStringBytes)
		default:
			ta = replayed
		}
	}
	p.PutUtfString("ta", ta)
	// distinct_id and afuid are local SDK values a real Android install always has (AndroidInstall);
	// iOS sends the captured ones, if any.
	distinctID, afuid := dev.DistinctID, dev.Afuid
	if !in.IOSMode {
		distinctID, afuid = in.Android.DistinctID, in.Android.Afuid
	}
	p.PutUtfString("distinct_id", distinctID)
	// phone_screen is Screen.width*Screen.height. The game's activity is locked
	// to portrait, so that's width*height. Android reports the selected device's
	// screen; iOS sends the captured one, else the 1920*1080 the first captured session sent.
	phoneScreen := orDefault(dev.PhoneScreen, "1920*1080")
	if !in.IOSMode {
		phoneScreen = fmt.Sprintf("%d*%d", screenWidth, screenHeight)
	}
	p.PutUtfString("phone_screen", phoneScreen)
	p.PutSFSObject("configVersion", sfs.NewSFSObject())

	// Dossier §05 documented these as "only if uid empty", inferred from
	// static analysis. Confirmed live: a real reconnect (non-empty uid)
	// omits all five entirely -- sending them unconditionally, as this
	// client did before, was an untested guess that turned out to differ
	// from every real reconnect this server has seen.
	//
	// gcmRegisterId and referrer are Android-only: the iOS block has the first three keys alone
	// (Assembly-CSharp.decompiled.cs:83078-83087, 1.0.370; Android :83185-83196). timeoffset is the
	// device's UTC offset in seconds ((DateTime.Now - DateTime.UtcNow).TotalSeconds); "0" is the
	// UTC device the ta blob describes (#zone_offset 0).
	if in.GameUid == "" {
		p.PutInt("country", 1)
		p.PutBool("suggestCountry", false)
		p.PutUtfString("timeoffset", "0")
		if !in.IOSMode {
			p.PutUtfString("gcmRegisterId", "")
			p.PutUtfString("referrer", "")
		}
	}

	// AndroidID/IMEI are Android-only identifiers; a real iOS client's
	// captured request omits both entirely rather than sending them empty. The
	// names are the game's own: "AndroidID" is really Build.SERIAL (which reads
	// "unknown" on a SDK 36 target), and "IMEI" is Udid.getDeviceInfo()'s
	// space-separated build dump, both from the selected device.
	if !in.IOSMode {
		p.PutUtfString("AndroidID", "unknown")
		p.PutUtfString("IMEI", hw.DeviceInfo())
	}
	p.PutUtfString("psh", pshField(in.IOSMode, cmdBaseTime))
	mt := dev.HandSetInfo
	if !in.IOSMode {
		mt = hw.HandSetInfo()
	}
	p.PutUtfString("mt", mt)
	p.PutUtfString("deviceId", in.DeviceID)
	p.PutUtfString("airKey", in.AirKey)
	p.PutUtfString("cmdBaseTime", cmdBaseTime)
	p.PutUtfString("SecurityCode", securityCode(cmdBaseTime, in.GameUid))
	p.PutUtfString("OneCode", oneCode)
	p.PutUtfString("CoreV", coreV)
	p.PutUtfString("googlePlay", "")
	androidDid := ""
	if !in.IOSMode {
		androidDid = hw.AndroidDid()
	}
	p.PutUtfString("androidDid", androidDid)
	p.PutUtfString("googleName", "")
	p.PutUtfString("deeplinkParams", "")
	p.PutUtfString("pfId", "")
	// google_available: GooglePlayManager.isAvailable, true on a Play-store phone with Play
	// services (com/googleplayservice/GooglePlayManager.java:77-82; Android
	// Assembly-CSharp.decompiled.cs:83239), as every catalog phone is. The iOS Login has no such key.
	if !in.IOSMode {
		p.PutInt("google_available", 1)
	}
	p.PutUtfString("fromCountry", orDefault(dev.FromCountry, "US"))
	// Confirmed via live packet capture against production (real Last War
	// app, iOS/Mac Catalyst build): this field was entirely absent from
	// our Login request before -- not just empty, the key itself was
	// never sent. The real client's value is a client-side config-bundle
	// hash we have no way to legitimately compute
	// ("<gameUid>_<hash>_<hash>_<version>"); sending the key with an
	// empty value is the closest honest approximation and untested
	// whether it alone matters, but omitting the key entirely is now
	// known to differ from every real login this server has ever seen.
	p.PutUtfString("dataConfigMd5", "")
	effectivePackageName := build.PackageName
	platformCode := "1" // Android
	pf := storefrontPF(in.IOSMode)
	if in.IOSMode {
		platformCode = "0"
		// iOS-only identifiers a real client always sends; captured live
		// with real (non-secret, ad-tracking-scoped) values. idfv is the
		// captured one when the session config has it; idfa is never replayed.
		p.PutUtfString("idfa", "")
		p.PutUtfString("idfv", dev.Idfv)
		p.PutUtfString("phone_native_screen", "720*1280")
	}
	p.PutUtfString("packageName", effectivePackageName)
	p.PutUtfString("packageSign", packageSignHex(effectivePackageName))
	p.PutUtfString("platform", platformCode)
	// lat is a constant per platform: 1 on iOS, 0 on Android (Assembly-CSharp.decompiled.cs:83132;
	// Android :83246, 1.0.370).
	lat := int32(0)
	if in.IOSMode {
		lat = 1
	}
	p.PutInt("lat", lat)
	// device_string: a real Android Login sends DeviceManager.GetDeviceString()
	// (Model/Memory/Vendor/Processor/Graphics/DeviceType); a real iOS Login
	// doesn't carry it, so it stays Android-only.
	if !in.IOSMode {
		p.PutUtfString("device_string", hw.DeviceString())
	}
	p.PutUtfString("pf", pf)
	p.PutUtfString("firebaseId", dev.FirebaseID)
	p.PutUtfString("afuid", afuid)
	// Without a captured device, iOS keeps the placeholders the cron's session has logged in with
	// since August 2026: no 1.0.370 source gives an iOS SystemInfo.deviceModel/operatingSystem to
	// put in their place, and runCrossServerTest warns to recapture.
	phoneModel := orDefault(dev.PhoneModel, "GoClient")
	if !in.IOSMode {
		phoneModel = hw.Model()
	}
	p.PutUtfString("phone_model", phoneModel)
	p.PutInt("configNumber", 0)
	p.PutUtfString("gaid", "")
	osVersion := orDefault(dev.OSVersion, "GoClient/1.0")
	if !in.IOSMode {
		osVersion = hw.OSVersionString()
	}
	p.PutUtfString("osVersion", osVersion)
	p.PutUtfString("parseRegisterId", "")
	p.PutUtfString("gameUid", in.GameUid)
	p.PutUtfString("appVersion", effectiveAppVersion)
	p.PutUtfString("resVersion", resVersion)
	p.PutUtfString("versionCode", effectiveVersionCode)
	p.PutUtfString("lang", orDefault(dev.Lang, "en"))
	p.PutUtfString("serverId", in.ServerID)
	p.PutInt("gmLogin", 0)
	p.PutUtfString("KCPMode", "0")
	p.PutInt("forbidden_froce_merge", 1)
	p.PutUtfString("shumeiBoxId", in.ShumeiBoxId)
	// simOp and simOpName are TelephonyManager.getSimOperator() and getSimOperatorName() of the
	// ready SIM the phone's mt already names (com/sdkmanager/AppUtilManager.java:639-667; Android
	// Assembly-CSharp.decompiled.cs:83269-83270). iOS sends both empty as before.
	simOp, simOpName := "", ""
	if !in.IOSMode {
		simOp, simOpName = hw.SimOperator, hw.SimOperatorName
	}
	p.PutUtfString("simOp", simOp)
	p.PutUtfString("simOpName", simOpName)
	p.PutInt("delete_account_status", 0)
	// Real client sent true; we now actually implement Zstd decode
	// (packet.go), so this is both truthful and matches production. The client sends it only
	// while the new packet class and Zstd are both on (Assembly-CSharp.decompiled.cs:83176-83179).
	if !in.NoZstd {
		p.PutBool("isUseLz4", true)
	}
	if in.AccessTok != "" {
		p.PutUtfString("at", in.AccessTok)
	}
	return p
}
