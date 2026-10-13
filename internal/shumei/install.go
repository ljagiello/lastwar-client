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
	// DeviceID is the Login deviceId (a2, a97) this install registers under.
	// Empty means it isn't bound to one yet, as in a hand-written config; For
	// binds it.
	DeviceID string `json:"deviceId,omitempty"`

	// AndroidID is a24, Settings.Secure.ANDROID_ID (l1l11I111ll): 16
	// lowercase hex digits, which Android 8 and later derive per app signing
	// key. Last War doesn't set needUsingMD5, so the SDK sends it unhashed.
	AndroidID string `json:"androidId,omitempty"`

	// MediaDrmID is a84: the base64 deviceUniqueId of the Widevine, ClearKey
	// (W3C and DASH-IF UUIDs) and PlayReady MediaDrm schemes, in that order,
	// joined with "_" (l1111l111111Il.l111l1111lIl). A scheme the phone lacks,
	// or that has no deviceUniqueId, leaves its part empty. The SDK caches the
	// joined string in its "auc" prefs.
	MediaDrmID string `json:"mediaDrmId,omitempty"`

	// PUUID is a163, the SDK's own install ID: java.util.UUID.randomUUID()
	// without dashes, kept in its "seq" prefs as "puuid" (l1l11I11l).
	PUUID string `json:"puuid,omitempty"`

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
}

// widevineIDLen is the length of the deviceUniqueId Widevine reports.
const widevineIDLen = 32

// NewInstall returns the values of a fresh install on the profile's Samsung
// SM-G988N (Android 12): random IDs, the app installed 30 to 400 days before
// now and last updated within the past 20 days, and no smid yet. Of the four
// DRM schemes only Widevine has a deviceUniqueId: Android's ClearKey plugin
// keeps its test ID under "deviceId", and the phone has no PlayReady.
func NewInstall(now time.Time) Install {
	installed := now.Add(-randDuration(30*24*time.Hour, 400*24*time.Hour)).Truncate(time.Millisecond)
	updated := now.Add(-randDuration(time.Hour, 20*24*time.Hour)).Truncate(time.Millisecond)
	return Install{
		AndroidID:        hex.EncodeToString(randBytes(8)),
		MediaDrmID:       base64.StdEncoding.EncodeToString(randBytes(widevineIDLen)) + "___",
		PUUID:            randomUUID(),
		FirstInstallTime: installed,
		LastUpdateTime:   updated,
	}
}

// Fill returns in with each empty ID and install time taken from a fresh
// install, and whether it set any. Values already there stay, so a config can
// pin some of them, such as a real phone's Android ID.
func (in Install) Fill(now time.Time) (Install, bool) {
	fresh := NewInstall(now)
	filled := false
	fill := func(dst *string, v string) {
		if *dst == "" {
			*dst, filled = v, true
		}
	}
	fill(&in.AndroidID, fresh.AndroidID)
	fill(&in.MediaDrmID, fresh.MediaDrmID)
	fill(&in.PUUID, fresh.PUUID)
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
	androidIDRe = regexp.MustCompile(`^[0-9a-f]{16}$`)
	puuidRe     = regexp.MustCompile(`^[0-9a-f]{12}4[0-9a-f]{3}[89ab][0-9a-f]{15}$`)
)

// Validate reports the first set value a real install couldn't hold. Empty
// values pass: Fill supplies them. The errors give lengths, never the values,
// since they end up in logs.
func (in Install) Validate() error {
	if in.AndroidID != "" && !androidIDRe.MatchString(in.AndroidID) {
		return fmt.Errorf("androidId (%d characters) is not 16 lowercase hex digits", len(in.AndroidID))
	}
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
	if !in.FirstInstallTime.IsZero() && !in.LastUpdateTime.IsZero() && in.LastUpdateTime.Before(in.FirstInstallTime) {
		return errors.New("lastUpdateTime is before firstInstallTime")
	}
	if in.BoxID != "" && !strings.HasPrefix(in.BoxID, "B") {
		return errors.New(`boxId must start with "B", as getDeviceId() returns it`)
	}
	if (in.BoxID == "") != in.BoxIDSavedAt.IsZero() {
		return errors.New("boxId and boxIdSavedAt must be set together")
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

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return b
}

// randDuration returns a random duration in [lo, hi).
func randDuration(lo, hi time.Duration) time.Duration {
	return lo + mrand.N(hi-lo)
}
