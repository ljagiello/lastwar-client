package auth

import (
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"strconv"
	"time"
)

// AndroidInstall is what a synthetic Android device keeps for the life of its Last War install,
// beyond its Android ID: the values the Android client's SDKs create locally on the first launch and
// a real Login always carries. It is created once, with the identity, and persisted next to it.
// Neither id is ever registered with AppsFlyer or ThinkingData: this client only sends them in the
// Login, as the real one does.
type AndroidInstall struct {
	// InstallTime is PackageInfo.firstInstallTime, ThinkingData's ta #install_time
	// (cn/thinkingdata/android/j.java:78-79, 282-283).
	InstallTime time.Time `json:"installTime"`
	// Afuid is the AppsFlyer uid, the Login's afuid: "<first-launch ms>-<Math.abs(SecureRandom
	// .nextLong())>" (com/appsflyer/internal/AFb1lSDK.java:70-101), the first launch shortly after
	// the install.
	Afuid string `json:"afuid"`
	// DistinctID is ThinkingData's random distinct id, the Login's distinct_id: a lowercase
	// UUID.randomUUID() (cn/thinkingdata/android/ThinkingAnalyticsSDK.java:857-860, p/j.java:10-16).
	DistinctID string `json:"distinctId"`
}

func (a AndroidInstall) String() string       { return "[REDACTED AndroidInstall]" }
func (a AndroidInstall) GoString() string     { return a.String() }
func (a AndroidInstall) LogValue() slog.Value { return slog.StringValue(a.String()) }

func androidInstallStatePath() string { return StateFilePath(".lastwar_goclient_android_install") }

// newAndroidInstall is an install made at installed, first launched seconds to minutes after that,
// never at the same millisecond: the AppsFlyer uid's prefix is the time of the SDK's first start,
// right after the install (1.0.370 audit B31, B95).
func newAndroidInstall(installed time.Time) AndroidInstall {
	installed = installed.Truncate(time.Millisecond)
	firstLaunch := installed.Add(randomDuration(5*time.Second, 90*time.Second))
	n := binary.BigEndian.Uint64(randomBytes(8)) &^ (1 << 63) // Math.abs of a long: 63 bits
	return AndroidInstall{
		InstallTime: installed,
		Afuid:       strconv.FormatInt(firstLaunch.UnixMilli(), 10) + "-" + strconv.FormatUint(n, 10),
		DistinctID:  lowerUUID4(),
	}
}

// loadOrCreateAndroidInstall returns the persisted AndroidInstall, creating and persisting one on
// first use. A new one is installed at installedAt when that is set: the time another record of
// this install already reports (the session's ShuMei install, a125 fit), so both describe one
// install. Otherwise it is installed a few minutes before created, the time this device's identity
// was created, so an identity older than this state file (one made before it existed) doesn't
// report an install after Logins the server has already seen. An unreadable or corrupt file is an
// error rather than a silent replacement: the values describe an install the server has already
// seen.
func loadOrCreateAndroidInstall(created, installedAt time.Time) (AndroidInstall, error) {
	raw, err := readTrimmed(androidInstallStatePath())
	if err != nil {
		return AndroidInstall{}, fmt.Errorf("read android install state: %w", err)
	}
	if raw != "" {
		var a AndroidInstall
		if err := json.Unmarshal([]byte(raw), &a); err != nil || a.InstallTime.IsZero() || a.Afuid == "" || a.DistinctID == "" {
			return AndroidInstall{}, fmt.Errorf("android install state %s is incomplete or not JSON", androidInstallStatePath())
		}
		return a, nil
	}
	if installedAt.IsZero() {
		installedAt = created.Add(-randomDuration(2*time.Minute, 10*time.Minute))
	}
	a := newAndroidInstall(installedAt)
	b, _ := json.Marshal(a) // a time and two strings: Marshal can't fail
	if err := AtomicWriteStateFile(androidInstallStatePath(), string(b)); err != nil {
		return AndroidInstall{}, fmt.Errorf("persist android install state: %w", err)
	}
	return a, nil
}

// lowerUUID4 is java.util.UUID.randomUUID().toString(): a version-4 UUID, lowercase, dashed.
func lowerUUID4() string {
	b := randomBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error
	return b
}

// randomDuration returns a random duration in [lo, hi).
func randomDuration(lo, hi time.Duration) time.Duration {
	n, _ := rand.Int(rand.Reader, big.NewInt(int64(hi-lo)))
	return lo + time.Duration(n.Int64())
}
