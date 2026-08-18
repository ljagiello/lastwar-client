package main

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
	}
	return filepath.Join(dir, name)
}

func deviceIDStatePath() string { return stateFilePath(".lastwar_goclient_device_id") }
func usernameStatePath() string { return stateFilePath(".lastwar_goclient_username") }
func gameUidStatePath() string  { return stateFilePath(".lastwar_goclient_gameuid") }
func loginKeyStatePath() string { return stateFilePath(".lastwar_goclient_loginkey") }

func loadOrCreateDeviceIdentity() (*deviceIdentity, error) {
	id := ""
	if b, err := os.ReadFile(deviceIDStatePath()); err == nil {
		id = strings.TrimSpace(string(b))
	}
	if id == "" {
		// Fallback path a real client would take if the native UDID call
		// failed: a random unique id + "_n3d" (release build suffix).
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		id = hex.EncodeToString(raw) + "_n3d"
		if err := os.WriteFile(deviceIDStatePath(), []byte(id), 0o600); err != nil {
			return nil, fmt.Errorf("persist device id: %w", err)
		}
	}
	readTrimmed := func(path string) string {
		if b, err := os.ReadFile(path); err == nil {
			return strings.TrimSpace(string(b))
		}
		return ""
	}
	return &deviceIdentity{
		DeviceID: id,
		Username: readTrimmed(usernameStatePath()),
		GameUid:  readTrimmed(gameUidStatePath()),
		LoginKey: readTrimmed(loginKeyStatePath()),
	}, nil
}

func (d *deviceIdentity) SaveLoginKey(key string) error {
	d.LoginKey = key
	return os.WriteFile(loginKeyStatePath(), []byte(key), 0o600)
}

func (d *deviceIdentity) SaveGameUid(uid string) error {
	d.GameUid = uid
	return os.WriteFile(gameUidStatePath(), []byte(uid), 0o600)
}

// SaveUsername persists the SFS login response's `un` field so the next run
// can present it instead of an empty username.
func (d *deviceIdentity) SaveUsername(un string) error {
	d.Username = un
	return os.WriteFile(usernameStatePath(), []byte(un), 0o600)
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

func BuildLoginParams(in LoginParamsInput) *SFSObject {
	now := time.Now().Unix()
	cmdBaseTime := strconv.FormatInt(now, 10)
	oneCode, coreV := oneCodeAndCoreV()

	p := NewSFSObject()
	p.PutInt("_id", in.FutureID)
	p.PutInt("netType", 2) // 2 = wifi, matches the common case
	ta := "{}"
	if in.IOSMode {
		// DIAGNOSTIC ONLY: reproduces the exact field structure/format of the
		// `ta` analytics blob from a real captured login (values below are
		// synthetic placeholders, not the real captured ones), to isolate
		// whether the server validates fields inside it (lw_zone/lw_device_id/
		// lw_shumei_id echoing the top-level request) as part of the same
		// token-binding check already confirmed for packageName/appVersion/versionCode.
		ta = `{"#os": "iOS", "#disk": "84.6/926.4", "#device_id": "00000000-0000-0000-0000-000000000000", "#os_version": "26.5", "#system_language": "en", "#bundle_id": "com.lastwar.ios", "#carrier": "", "#app_version": "1.0.344", "#fps": 3, "#ram": "4.3/64.0", "#simulator": false, "#zone_offset": -7, "#manufacturer": "Apple", "#screen_height": 1194, "#network_type": "WIFI", "#device_model": "iPad8,6", "#screen_width": 834, "#install_time": "2025-06-16 09:27:29.892", "lw_net": "wifi", "lw_res_version": "2131.1477_788F_37951.23413", "lw_buildcode": "786", "lw_line": "lastwar-game-cf.lastwarapp.net", "pd_dl": 3, "lw_ab": "1", "lw_platform": "AppStore", "lw_game_session_id": "00000000-0000-0000-0000-000000000000", "lw_first_launch": false, "lw_alliance_id": "00000000000000000000000000000000", "lw_device_id": "your-real-device-id_n3d", "lw_device_level": 2, "lw_main_level": 33, "lw_shumei_id": "your-real-shumei-fingerprint-token", "lw_airKey": "your-real-air-key-token", "lw_version": "1.0.344", "lw_power": 0, "lw_zone": "APS1234"}`
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
	platform := "1"       // Android
	pf := "market_global" // Android storefront
	if in.IOSMode {
		effectivePackageName = iosPackageName
		platform = "0"
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
	p.PutUtfString("platform", platform)
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
