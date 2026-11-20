package shumei

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"regexp"
	"strings"
	"time"
)

// Install holds what a phone keeps for the life of one Last War install: the
// IDs the SDK reads or generates once, the app's install times, and the smid
// the SDK cached from its last registration. A session config keeps one, so
// every registration for that session reports the same phone. It belongs with
// one Login deviceId: a different deviceId is a different install (see For).
type Install struct {
	// DeviceID is the Login deviceId this install belongs to. The SDK never
	// sends it (a2 and a97 carry the stored smid); it only keeps one deviceId's
	// install from being sent under another. Empty means it isn't bound to one
	// yet, as in a hand-written config; For binds it.
	DeviceID string `json:"deviceId,omitempty"`

	// MediaDrmID is a84: the base64 deviceUniqueId of the Widevine, ClearKey
	// (W3C and DASH-IF UUIDs) and PlayReady MediaDrm schemes, in that order,
	// joined with "_" (l1111l111111Il.l111l1111lIl). A scheme the phone lacks,
	// or that has no deviceUniqueId, leaves its part empty. The SDK caches the
	// joined string in its "auc" prefs.
	MediaDrmID string `json:"mediaDrmId,omitempty"`

	// PUUID is a163, the SDK's own install ID: java.util.UUID.randomUUID()
	// without dashes, kept in its "seq" prefs as "puuid" (l1l11I11l).
	PUUID string `json:"puuid,omitempty"`

	// AppUID is a60, the app's Linux uid as Android formats it on API 27+:
	// "u0_a" + (uid - 10000) (l11l1l1l1Il.l1111l111111Il(int)). The uid is
	// assigned per install, so this is a per-install value like androidId.
	AppUID string `json:"appUid,omitempty"`

	// FirstInstallTime and LastUpdateTime are a125's fit and lut, the app's
	// PackageInfo.firstInstallTime and lastUpdateTime.
	FirstInstallTime time.Time `json:"firstInstallTime,omitzero"`
	LastUpdateTime   time.Time `json:"lastUpdateTime,omitzero"`

	// BoxID is the "B" + smid the SDK cached from this install's last
	// registration, which the next one sends back as a126. Empty until the
	// first registration.
	BoxID string `json:"boxId,omitempty"`

	// BoxIDSavedAt is when the SDK cached BoxID. a167 is the cache file's
	// timestamp (SMSDK.u2) and a168 the copy the SDK records right after
	// writing it ("bfic"), so both carry this time. Before the first
	// registration the file doesn't exist and both are 0.
	BoxIDSavedAt time.Time `json:"boxIdSavedAt,omitzero"`

	// Collections and AppStarts are the last a4 and a130 this install
	// reported: the SDK's collection count, kept in its "seq" prefs and raised
	// by one on every collection (l1l11I11l.java:134-146), and its app-start
	// count, kept in its "auc" prefs and raised once per process
	// (l111l11111I1l.java:13-29). Zero means none yet, so a first registration
	// reports 1 for both.
	Collections int `json:"collections,omitempty"`
	AppStarts   int `json:"appStarts,omitempty"`

	// BootCount is the last a127 this install reported: the phone's
	// Settings.Global "boot_count" (l1l11I111ll.java:38-47). Fill gives a
	// fresh install the count a phone in use has reached. LastBoot is the boot
	// that count belongs to, so a collection after a later boot counts one
	// more; zero means none reported yet.
	BootCount int       `json:"bootCount,omitempty"`
	LastBoot  time.Time `json:"lastBoot,omitzero"`

	// CloudConfMD5 and CloudConfExcluded describe the cloud config the SDK
	// cached from its last /v3/cloudconf reply ("cloudms.conf" prefs,
	// l11l111l1Il.java): the md5 of its text, which the next request sends
	// back, and the names its "sensitive" switches turn off, comma-separated
	// in document order, which join notCollect in a85 and turn off the slots
	// they gate (gateSlots). An empty CloudConfMD5
	// means nothing is cached, so the SDK's built-in config is in force.
	CloudConfMD5      string `json:"cloudConfMd5,omitempty"`
	CloudConfExcluded string `json:"cloudConfExcluded,omitempty"`
}

// cloudExcluded returns the names the cloud config in force turns off: the
// cached one's, or the built-in config's when none is cached.
func (in Install) cloudExcluded() []string {
	switch {
	case in.CloudConfMD5 == "":
		return builtinCloudConfExcluded
	case in.CloudConfExcluded == "":
		return nil
	}
	return strings.Split(in.CloudConfExcluded, ",")
}

// withCloudConf returns in with c cached as the cloud config in force.
func (in Install) withCloudConf(c cloudConf) Install {
	in.CloudConfMD5, in.CloudConfExcluded = c.MD5, strings.Join(c.Excluded, ",")
	return in
}

// rebootGap is how much later than LastBoot a boot time must be to count as a
// new boot: a40's samples of one boot differ by milliseconds, not minutes.
const rebootGap = time.Minute

// collected returns in as one more app start and collection on a phone booted
// at boot leaves it: Collections and AppStarts one higher, and BootCount one
// higher when boot is a later boot than LastBoot.
func (in Install) collected(boot time.Time) Install {
	in.Collections++
	in.AppStarts++
	if !in.LastBoot.IsZero() && boot.Sub(in.LastBoot) > rebootGap {
		in.BootCount++
	}
	in.LastBoot = boot.Truncate(time.Millisecond)
	return in
}

// widevineIDLen is the length of the deviceUniqueId Widevine reports.
const widevineIDLen = 32

// NewInstall returns the values of a fresh install on one of the catalog's
// Android 12 phones: random IDs, the app installed 30 to 400 days before now
// and last updated within the past 20 days, a boot count of 10 to 99, and no
// smid or SDK counters yet. Of the four DRM schemes only Widevine has a
// deviceUniqueId: Android's ClearKey plugin keeps its test ID under
// "deviceId", and the phone has no PlayReady.
func NewInstall(now time.Time) Install {
	installed := now.Add(-randDuration(30*24*time.Hour, 400*24*time.Hour)).Truncate(time.Millisecond)
	updated := now.Add(-randDuration(time.Hour, 20*24*time.Hour)).Truncate(time.Millisecond)
	return Install{
		MediaDrmID:       base64.StdEncoding.EncodeToString(randBytes(widevineIDLen)) + "___",
		PUUID:            randomUUID(),
		AppUID:           newAppUID(),
		FirstInstallTime: installed,
		LastUpdateTime:   updated,
		BootCount:        10 + mrand.N(90),
	}
}

// Fill returns in with each empty ID, install time and the boot count taken
// from a fresh install, and whether it set any. Values already there stay, so
// a config can pin some of them, such as a real phone's Android ID. The SDK
// counters (Collections, AppStarts) start at zero and only collected moves
// them.
func (in Install) Fill(now time.Time) (Install, bool) {
	fresh := NewInstall(now)
	filled := false
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst, filled = v, true
		}
	}
	fill(&in.MediaDrmID, fresh.MediaDrmID)
	fill(&in.PUUID, fresh.PUUID)
	fill(&in.AppUID, fresh.AppUID)
	if in.BootCount == 0 {
		in.BootCount, filled = fresh.BootCount, true
	}
	if in.FirstInstallTime.IsZero() {
		in.FirstInstallTime, filled = fresh.FirstInstallTime, true
		if !in.LastUpdateTime.IsZero() && in.LastUpdateTime.Before(in.FirstInstallTime) {
			in.FirstInstallTime = in.LastUpdateTime
		}
	}
	if in.LastUpdateTime.IsZero() {
		in.LastUpdateTime, filled = fresh.LastUpdateTime, true
		if in.LastUpdateTime.Before(in.FirstInstallTime) {
			in.LastUpdateTime = in.FirstInstallTime
		}
	}
	return in, filled
}

// For returns in as the install of deviceID: unchanged if it already is,
// bound to deviceID if it isn't bound yet. An install made for a different
// deviceId is a different phone, so For returns the zero Install and false
// rather than let two deviceIds share one fingerprint.
func (in Install) For(deviceID string) (Install, bool) {
	switch in.DeviceID {
	case deviceID:
		return in, true
	case "":
		in.DeviceID = deviceID
		return in, true
	}
	return Install{}, false
}

// Registered returns in after the SDK cached boxID at t, and whether that
// changed it. Android writes a prefs file only when a value changes, so
// caching the same smid again keeps the earlier time.
func (in Install) Registered(boxID string, t time.Time) (Install, bool) {
	if boxID == in.BoxID {
		return in, false
	}
	in.BoxID, in.BoxIDSavedAt = boxID, t.Truncate(time.Millisecond)
	return in, true
}

var (
	puuidRe  = regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
	appUIDRe = regexp.MustCompile(`^u0_a[0-9]+$`)
	md5HexRe = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// Validate reports the first set value a real install couldn't hold. Empty
// values pass: Fill supplies them. The errors give lengths, never the values,
// since they end up in logs.
func (in Install) Validate() error {
	if in.MediaDrmID != "" {
		parts := strings.Split(in.MediaDrmID, "_")
		if len(parts) != 4 {
			return fmt.Errorf("mediaDrmId has %d \"_\"-separated parts, want 4 (Widevine, two ClearKey, PlayReady)", len(parts))
		}
		for i, p := range parts {
			if _, err := base64.StdEncoding.DecodeString(p); err != nil {
				return fmt.Errorf("mediaDrmId part %d is not base64: %w", i+1, err)
			}
		}
	}
	if in.PUUID != "" && !puuidRe.MatchString(in.PUUID) {
		return fmt.Errorf("puuid (%d characters) is not a random (version 4) UUID as 32 lowercase hex digits", len(in.PUUID))
	}
	if in.AppUID != "" && !appUIDRe.MatchString(in.AppUID) {
		return fmt.Errorf("appUid (%d characters) is not the \"u0_a<uid>\" form a real app uid takes", len(in.AppUID))
	}
	if !in.FirstInstallTime.IsZero() && !in.LastUpdateTime.IsZero() && in.LastUpdateTime.Before(in.FirstInstallTime) {
		return errors.New("lastUpdateTime is before firstInstallTime")
	}
	if in.BoxID != "" && !strings.HasPrefix(in.BoxID, "B") {
		return errors.New(`boxId must start with "B", as getDeviceId() returns it`)
	}
	if (in.BoxID == "") != in.BoxIDSavedAt.IsZero() {
		return errors.New("boxId and boxIdSavedAt must be set together")
	}
	if in.Collections < 0 || in.AppStarts < 0 || in.BootCount < 0 {
		return errors.New("collections, appStarts and bootCount can't be negative")
	}
	if in.CloudConfMD5 != "" && !md5HexRe.MatchString(in.CloudConfMD5) {
		return fmt.Errorf("cloudConfMd5 (%d characters) is not an MD5 as 32 lowercase hex digits", len(in.CloudConfMD5))
	}
	if in.CloudConfMD5 == "" && in.CloudConfExcluded != "" {
		return errors.New("cloudConfExcluded needs the cloudConfMd5 of the config it came from")
	}
	return nil
}

// randomUUID returns a version 4 UUID as 32 lowercase hex digits, what
// UUID.randomUUID().toString().replaceAll("-", "") gives.
func randomUUID() string {
	b := randBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return hex.EncodeToString(b)
}

// newAppUID returns a60, the app's Linux uid as Android formats it on API 27+
// (l11l1l1l1Il.l1111l111111Il(int)): "u0_a" + (uid - 10000). Android assigns
// the uid per install, in install order, so a fresh install sits somewhere in
// the low u0_aNN range.
func newAppUID() string {
	return fmt.Sprintf("u0_a%d", mrand.N(200))
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return b
}

// randDuration returns a random duration in [lo, hi).
func randDuration(lo, hi time.Duration) time.Duration {
	return lo + mrand.N(hi-lo)
}
