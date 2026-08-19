package main

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// packageSignHex reproduces SDKManager.GetPackageSign() exactly:
// SHA1(packageName), lowercase hex, no separators. (Confirmed from
// decompiled/Assembly-CSharp.decompiled.cs:93244 -- it hashes the package
// NAME string, not the certificate, despite the field's name.) Confirmed
// live for the iOS package too: SHA1("com.lastwar.ios") reproduces exactly
// the packageSign a real iOS client sent, captured via packet inspection.
func packageSignHex(forPackageName string) string {
	return sha1Hex(forPackageName)
}

// apkCertHex is the raw DER-encoded signing certificate, hex-encoded --
// exactly what Android's android.content.pm.Signature.toCharsString()
// returns. Extracted directly from com.fun.lastwar.gp.apk's
// META-INF/BNDLTOOL.RSA (a PKCS#7 SignedData blob) via:
//
//	openssl pkcs7 -inform DER -in BNDLTOOL.RSA -print_certs | openssl x509 -outform DER | xxd -p
//
// This is public information (anyone can extract it from the APK), used
// only for the `psh` field's MD5 input -- dossier §11 confirms this is not
// secret/derived-from-native-computation.
const apkCertHex = "3082029c30820184a00302010202046fdfb4c4300d06092a864886f70d0101050500300f310d300b060355040a0c044c4359443020170d3233303131363133323131335a180f32303733303130333133323131335a300f310d300b060355040a0c044c43594430820122300d06092a864886f70d01010105000382010f003082010a028201010089ef3aa54cf5a75c83d18c37694482552227620de97dbce3747d0d820b94400a322a65ad61b8360deaf036a9755a4058a6eb01388f6fcb3b061bea3730bc401022471739ebcbe3337c3a67e211c5f7a909397198a6824a0f71a9d8c715575b6529055d922fe7e007ce71b16098c491bdea6f4767ac2e2e7e6cbaa5fbb11c55ffb6561a895c3e7437cb41697db019005473d9f77f4430b163848231ec4903259949b489d5eb436f9cbf4206fcbf040d8c6da6300e30edfeecb5619abfb79dbab2837b4adddba51582a5c85e50568ae32355eba1f974f5567dbb4ac8afdd9ce474f6555e9f5f1085b82072052a08a772db1ee943615b38833dd1e95f0c827ffeed0203010001300d06092a864886f70d01010505000382010100488695eee865bc3992e9dcfa86a19719eb753b7abcd7a5d7ddfd9471229136b37bf08bce8520a98280c0915202ea5a6530f12f81fc6b85769685d8049e73f94d7584ad5e62babc1f9c8c34617f2856d40f7592dad213a0b29edcb5c6ef9179e2d220fd4d83ed42bd6b4f0e430bbcb970db89b352aac9a41f90b15dcb1f76f3efd8e6d104fc23c71c77d50448d9b7666ca182210a33d95b7786a2400f20ff6b9bb33a464bd1134f14eb6a4d6c169d526c2875a5ba0dca2f1f8b8920d1eb3dabfa7977f35fd348eba2a72ee7b052dd8618d5a50b84c25a5e7afdeba271c5eff310d49b934bd8b80724dd7784f35550cfe72c6e0a00140bf50f45acbe1a7eafc2ff"

func md5Hex(s string) string {
	sum := md5.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

// pshField reproduces SdkManager.LW_PSH: MD5(cmdBaseTime + certHex).
func pshField(cmdBaseTime string) string {
	return md5Hex(cmdBaseTime + apkCertHex)
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
	for i := 0; i < n; i++ {
		d, _ := rand.Int(rand.Reader, big.NewInt(10))
		b.WriteString(d.String())
	}
	return b.String()
}

// oneCodeAndCoreV reproduces the OneCode/CoreV construction from
// LoginMessage.CSSetData -- both derived from the same 32-digit rand32.
func oneCodeAndCoreV() (oneCode, coreV string) {
	rand32 := randomDigitString(32)
	md5Rand := md5Hex(rand32)

	oneBytes := make([]byte, 0, 64)
	for i := 0; i < 32; i++ {
		oneBytes = append(oneBytes, md5Rand[i], rand32[i])
	}
	oneCode = string(oneBytes)

	b64 := base64.StdEncoding.EncodeToString([]byte(rand32))
	b64Rev := reverseString(b64)
	md2 := md5Hex(b64Rev)
	md3 := md5Hex(md2 + rand32)

	coreBytes := make([]byte, 0, 64)
	for j := 0; j < 32; j++ {
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
	DeviceID string
	Username string
	GameUid  string
	LoginKey string
}

func stateFilePath(name string) string {
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

func deviceIDStatePath() string { return stateFilePath(".lastwar_goclient_device_id") }
func usernameStatePath() string { return stateFilePath(".lastwar_goclient_username") }
func gameUidStatePath() string  { return stateFilePath(".lastwar_goclient_gameuid") }
func loginKeyStatePath() string { return stateFilePath(".lastwar_goclient_loginkey") }

// readTrimmed reads path and returns its trimmed contents. A missing file (os.IsNotExist) is not
// an error -- it just means that piece of state hasn't been persisted yet (e.g. first run, or a
// GameUid/LoginKey not learned yet), and callers should treat it as "" the same as before. Any
// OTHER read error -- permission denied, I/O failure, a directory sitting where a file was
// expected, etc. -- is now returned as an error instead of being silently swallowed as "absent":
// treating a transient failure to read an EXISTING, valid state file the same as "no state yet"
// is exactly the bug that let loadOrCreateDeviceIdentity fabricate and persist a brand-new random
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
//   - atomicWriteStateFile (below) publishes via os.Rename, which silently overwrites an existing
//     destination -- correct for saveStateFile (loginKey/gameUid/username) and the device-id
//     empty-file self-heal path in loadOrCreateDeviceIdentity, where clobbering whatever
//     (possibly stale/empty) content was already there is exactly the point.
//   - createDeviceIDStateFile (below) publishes via os.Link instead, which fails with an
//     os.IsExist-shaped error if the destination already exists rather than overwriting it --
//     required there to preserve the O_CREATE|O_EXCL-equivalent race-detection
//     loadOrCreateDeviceIdentity's os.IsExist(err) handling depends on.
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
			os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
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
// already exist. Exclusivity closes the TOCTOU gap between loadOrCreateDeviceIdentity's earlier
// read-not-exists check and this write: if some other invocation created the file in between (the
// documented deployment is a single cron job per machine, so concurrent invocations aren't the
// primary realistic trigger here, but this is a basic safety margin at negligible cost), this
// fails with an os.IsExist error instead of silently clobbering whatever that other invocation
// already wrote -- the caller re-reads rather than overwrites in that case.
//
// The id is written to a temp file first (fsync'd, via the writeTempStateFile helper above, the
// same one atomicWriteStateFile uses) and published with os.Link rather than a direct
// O_CREATE|O_EXCL+WriteString straight to path. The old direct-write approach had a window
// between O_CREATE|O_EXCL succeeding and WriteString completing where a crash/OOM-kill/ENOSPC
// left a partial, non-empty, non-zero-length device-id file behind -- which the empty-file
// self-heal logic in loadOrCreateDeviceIdentity does NOT catch (it only self-heals a
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
	defer os.Remove(tmpPath)
	return os.Link(tmpPath, path)
}

// deviceIDEmptyRetryAttempts/deviceIDEmptyRetryDelay bound how long loadOrCreateDeviceIdentity
// waits, after an O_CREATE|O_EXCL failure on the device-id state file, to tell a genuine
// concurrent writer (which will finish within milliseconds on the same machine) apart from a
// stale empty leftover from a prior crashed/OOM-killed/disk-full process (which never will). Kept
// small and bounded on purpose -- this must never become an unbounded or silent retry loop.
const (
	deviceIDEmptyRetryAttempts = 3
	deviceIDEmptyRetryDelay    = 25 * time.Millisecond
)

// atomicWriteStateFile writes content to path via write-temp-then-rename instead of a plain
// O_WRONLY/O_TRUNC reopen (what os.WriteFile does under the hood). A plain truncate-then-write
// leaves a window -- open+truncate, then the write itself, as separate syscalls with no fsync --
// where a crash/OOM-kill/power-loss mid-write leaves a zero-length or partially-written file
// behind, or (for a concurrent writer instead of a crash) could race and clobber a
// slow-but-genuine concurrent writer's partial content. Rename within the same directory is
// atomic on POSIX filesystems, so any reader sees either the old complete content or the new
// complete content, never a torn write. Used by saveStateFile (loginKey/gameUid/username) and the
// device-id empty-file self-heal path in loadOrCreateDeviceIdentity, where overwriting whatever
// was already at path is the desired behavior; createDeviceIDStateFile needs os.Link's stricter
// exclusivity instead (see writeTempStateFile's doc comment above), so it doesn't go through this
// function -- but it gets the same torn-write protection via the shared writeTempStateFile step.
func atomicWriteStateFile(path, content string) error {
	tmpPath, err := writeTempStateFile(path, content)
	if err != nil {
		return err
	}
	defer os.Remove(tmpPath) // no-op once the rename below succeeds -- the path no longer exists
	return os.Rename(tmpPath, path)
}

func loadOrCreateDeviceIdentity() (*deviceIdentity, error) {
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
		// Fallback path a real client would take if the native UDID call
		// failed: a random unique id + "_n3d" (release build suffix).
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		id = hex.EncodeToString(raw) + "_n3d"
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
				for attempt := 0; attempt < deviceIDEmptyRetryAttempts; attempt++ {
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
					if err := atomicWriteStateFile(deviceIDStatePath(), id); err != nil {
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
	warnIfLoosePermissions(usernameStatePath())
	warnIfLoosePermissions(gameUidStatePath())
	warnIfLoosePermissions(loginKeyStatePath())
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
	return &deviceIdentity{
		DeviceID: id,
		Username: username,
		GameUid:  gameUid,
		LoginKey: loginKey,
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
// same write-temp-then-rename atomicWriteStateFile helper the device-id state file already uses
// -- a plain os.WriteFile does a truncate-open + write + close as separate syscalls with no
// fsync and no rename, so a crash/OOM-kill/power-loss mid-write could leave a zero-length or
// partially-written loginKey/gameUid/username file behind, the same class of bug the device-id
// file was hardened against. As a side effect of always writing through a fresh 0600 temp file
// and renaming it into place, this also settles the "existing file left world/group-readable"
// gotcha os.WriteFile has (its mode argument only applies on file creation): rename replaces the
// target's inode outright, so the destination always ends up with the temp file's 0600 bits
// regardless of what permissions the file being replaced had.
func saveStateFile(path, data string) error {
	return atomicWriteStateFile(path, data)
}

func (d *deviceIdentity) SaveLoginKey(key string) error {
	d.LoginKey = key
	return saveStateFile(loginKeyStatePath(), key)
}

func (d *deviceIdentity) SaveGameUid(uid string) error {
	d.GameUid = uid
	return saveStateFile(gameUidStatePath(), uid)
}

// SaveUsername persists the SFS login response's `un` field so the next run
// can present it instead of an empty username.
func (d *deviceIdentity) SaveUsername(un string) error {
	d.Username = un
	return saveStateFile(usernameStatePath(), un)
}

func (d *deviceIdentity) AirKey() string {
	return "lwDid_" + base64.StdEncoding.EncodeToString([]byte(d.DeviceID))
}

// LoginParams builds the full ~50-field SFSObject for the base SFS zone
// login (dossier §05 / §2.3). uid is empty on a brand-new device (the
// server assigns one back via GSL's serverList[].gameUid, which the caller
// should pass in as gameUid once known).
type LoginParamsInput struct {
	FutureID    int32
	DeviceID    string
	AirKey      string
	GameUid     string
	AccessTok   string // "at" from the GSL response, if any
	ServerID    string // zone minus "APS" prefix
	ShumeiBoxId string // anti-fraud device-fingerprint token, if a real one is known

	// IOSMode switches packageName/packageSign/platform/pf (and adds the
	// iOS-only idfa/idfv/phone_native_screen fields) to match a real iOS
	// client instead of the default Android identity. Confirmed live: an
	// `at` access token is bound to the packageName/platform it was issued
	// for -- reusing a token obtained under one platform's identity while
	// claiming a different platform/package gets ec=28/E005, even though
	// the token itself, un, and every other field are valid and accepted.
	IOSMode bool
}

const iosPackageName = "com.lastwar.ios"

// iosAnalyticsBlob mirrors the JSON shape of the `ta` analytics blob a real
// captured iOS login sends, so the diagnostic copy built in BuildLoginParams
// can be constructed field-by-field instead of as a giant hand-maintained
// JSON string literal.
type iosAnalyticsBlob struct {
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

func BuildLoginParams(in LoginParamsInput) *SFSObject {
	now := time.Now().Unix()
	cmdBaseTime := strconv.FormatInt(now, 10)
	oneCode, coreV := oneCodeAndCoreV()

	p := NewSFSObject()
	p.PutInt("_id", in.FutureID)
	p.PutInt("netType", 2) // 2 = wifi, matches the common case
	effectiveAppVersion, effectiveVersionCode := appVersion, versionCode
	if in.IOSMode {
		// A GSL-issued `at` access token is bound to the exact
		// appVersion/versionCode it was obtained under, same as
		// packageName/platform above -- confirmed live: our hardcoded
		// Android 1.0.351/1835 build numbers, sent alongside a real
		// captured iOS token issued to 1.0.344/786, still got ec=28/E005
		// even after packageName/platform/pf matched. These iOS values
		// are what that capture actually showed; there is no known way
		// to derive them generically the way packageSign derives from
		// packageName, so a fresh capture is needed if the real client's
		// build ever moves on.
		effectiveAppVersion, effectiveVersionCode = "1.0.344", "786"
	}
	ta := "{}"
	if in.IOSMode {
		// DIAGNOSTIC ONLY: reproduces the field structure/format of the ta analytics blob a
		// real captured iOS login sends, to isolate whether the server validates fields inside
		// it (lw_zone/lw_device_id/lw_shumei_id echoing the top-level request) as part of the
		// same token-binding check already confirmed for packageName/appVersion/versionCode.
		//
		// LwDeviceID/LwShumeiID/LwAirKey were originally set from the same in.DeviceID/
		// in.ShumeiBoxId/in.AirKey values used for the top-level request fields elsewhere in
		// this function. That leaked those live secrets in cleartext: this whole blob gets
		// JSON-marshaled and stored as a single opaque string under the "ta" key, and
		// SFSObject.StringRedacted() only masks known-sensitive *keys* -- it has no way to see
		// or redact secrets embedded inside another field's string value. sensitiveSFSKeys now
		// also lists "ta" itself (see sfsobject.go) as defense-in-depth, but the real fix is
		// here: never put a live credential into a field that isn't itself a redacted key.
		// These three fields are placeholders (empty string, matching this struct's existing
		// pattern for telemetry we have no way to observe, e.g. Carrier/LwLine below) until/
		// unless a live capture confirms the server actually validates their content -- their
		// mere presence in the JSON shape is preserved in case some undocumented check depends
		// on the keys existing at all.
		blob := iosAnalyticsBlob{
			OS: "iOS", Disk: "50.0/100.0", DeviceID: "00000000-0000-0000-0000-000000000000",
			OSVersion: "0.0", SystemLanguage: "en", BundleID: iosPackageName, Carrier: "",
			AppVersion: effectiveAppVersion, FPS: 0, RAM: "0.0/0.0", Simulator: false, ZoneOffset: 0,
			Manufacturer: "Apple", ScreenHeight: 0, NetworkType: "WIFI", DeviceModel: "unknown",
			ScreenWidth: 0, InstallTime: "2000-01-01 00:00:00.000",
			LwNet: "wifi", LwResVersion: "0.0", LwBuildcode: effectiveVersionCode, LwLine: "",
			PdDl: 0, LwAb: "0", LwPlatform: "AppStore",
			LwGameSessionID: "00000000-0000-0000-0000-000000000000", LwFirstLaunch: false,
			LwAllianceID: "00000000000000000000000000000000",
			LwDeviceID:   "", LwDeviceLevel: 0, LwMainLevel: 0,
			LwShumeiID: "", LwAirKey: "",
			LwVersion: "0.0.0", LwPower: 0, LwZone: "APS" + in.ServerID,
		}
		if b, err := json.Marshal(blob); err == nil {
			ta = string(b)
		}
	}
	p.PutUtfString("ta", ta)
	p.PutUtfString("distinct_id", "")
	p.PutUtfString("phone_screen", "1920*1080")
	p.PutSFSObject("configVersion", NewSFSObject())

	// Dossier §05 documented these as "only if uid empty", inferred from
	// static analysis. Confirmed live: a real reconnect (non-empty uid)
	// omits all five entirely -- sending them unconditionally, as this
	// client did before, was an untested guess that turned out to differ
	// from every real reconnect this server has seen.
	if in.GameUid == "" {
		p.PutInt("country", 1)
		p.PutBool("suggestCountry", false)
		p.PutUtfString("timeoffset", "0")
		p.PutUtfString("gcmRegisterId", "")
		p.PutUtfString("referrer", "")
	}

	// AndroidID/IMEI are Android-only identifiers; a real iOS client's
	// captured request omits both entirely rather than sending them empty.
	if !in.IOSMode {
		p.PutUtfString("AndroidID", "")
		p.PutUtfString("IMEI", "")
	}
	p.PutUtfString("psh", pshField(cmdBaseTime))
	p.PutUtfString("mt", "")
	p.PutUtfString("deviceId", in.DeviceID)
	p.PutUtfString("airKey", in.AirKey)
	p.PutUtfString("cmdBaseTime", cmdBaseTime)
	p.PutUtfString("SecurityCode", securityCode(cmdBaseTime, in.GameUid))
	p.PutUtfString("OneCode", oneCode)
	p.PutUtfString("CoreV", coreV)
	p.PutUtfString("googlePlay", "")
	p.PutUtfString("androidDid", "")
	p.PutUtfString("googleName", "")
	p.PutUtfString("deeplinkParams", "")
	p.PutUtfString("pfId", "")
	if !in.IOSMode {
		p.PutInt("google_available", 0) // Google Play Services is Android-only; a real iOS capture omits this key
	}
	p.PutUtfString("fromCountry", "US")
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
	effectivePackageName := packageName
	platformCode := "1"   // Android
	pf := "market_global" // Android storefront
	if in.IOSMode {
		effectivePackageName = iosPackageName
		platformCode = "0"
		pf = "AppStore"
		// iOS-only identifiers a real client always sends; captured live
		// with real (non-secret, ad-tracking-scoped) values, sent empty
		// here since we have none of our own.
		p.PutUtfString("idfa", "")
		p.PutUtfString("idfv", "")
		p.PutUtfString("phone_native_screen", "720*1280")
	}
	p.PutUtfString("packageName", effectivePackageName)
	p.PutUtfString("packageSign", packageSignHex(effectivePackageName))
	p.PutUtfString("platform", platformCode)
	p.PutInt("lat", 1) // real client sent 1 (location-authorized); we hardcoded 0 before
	// device_string: not present in any real capture (guest or reconnect)
	// -- dropped rather than guessed at.
	p.PutUtfString("pf", pf)
	p.PutUtfString("firebaseId", "")
	p.PutUtfString("afuid", "")
	p.PutUtfString("phone_model", "GoClient")
	p.PutInt("configNumber", 0)
	p.PutUtfString("gaid", "")
	p.PutUtfString("osVersion", "GoClient/1.0")
	p.PutUtfString("parseRegisterId", "")
	p.PutUtfString("gameUid", in.GameUid)
	p.PutUtfString("appVersion", effectiveAppVersion)
	p.PutUtfString("resVersion", "0")
	p.PutUtfString("versionCode", effectiveVersionCode)
	p.PutUtfString("lang", "en")
	p.PutUtfString("serverId", in.ServerID)
	p.PutInt("gmLogin", 0)
	p.PutUtfString("KCPMode", "0")
	p.PutInt("forbidden_froce_merge", 1)
	p.PutUtfString("shumeiBoxId", in.ShumeiBoxId)
	p.PutUtfString("simOp", "")
	p.PutUtfString("simOpName", "")
	p.PutInt("delete_account_status", 0)
	// Real client sent true; we now actually implement Zstd decode
	// (packet.go), so this is both truthful and matches production.
	p.PutBool("isUseLz4", true)
	if in.AccessTok != "" {
		p.PutUtfString("at", in.AccessTok)
	}
	return p
}
