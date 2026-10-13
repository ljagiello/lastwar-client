package shumei

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestNewInstall checks a fresh install's values have the formats a real phone
// reports, and that two installs don't share them.
func TestNewInstall(t *testing.T) {
	now := time.UnixMilli(1790882444713)
	in := NewInstall(now)
	if err := in.Validate(); err != nil {
		t.Fatalf("NewInstall(...).Validate() = %v", err)
	}
	// Only Widevine has a deviceUniqueId: 32 bytes, then three empty parts.
	parts := strings.Split(in.MediaDrmID, "_")
	if len(parts) != 4 || parts[1] != "" || parts[2] != "" || parts[3] != "" {
		t.Errorf("MediaDrmID = %q, want Widevine's ID and three empty parts", in.MediaDrmID)
	}
	if b, err := base64.StdEncoding.DecodeString(parts[0]); err != nil || len(b) != widevineIDLen {
		t.Errorf("Widevine ID decodes to %d bytes (error %v), want %d", len(b), err, widevineIDLen)
	}
	if lo, hi := now.Add(-400*24*time.Hour), now.Add(-30*24*time.Hour); in.FirstInstallTime.Before(lo) || in.FirstInstallTime.After(hi) {
		t.Errorf("FirstInstallTime = %v, want 30 to 400 days before %v", in.FirstInstallTime, now)
	}
	if in.LastUpdateTime.Before(in.FirstInstallTime) || in.LastUpdateTime.After(now) {
		t.Errorf("LastUpdateTime = %v, want between FirstInstallTime %v and now", in.LastUpdateTime, in.FirstInstallTime)
	}
	if in.BoxID != "" || !in.BoxIDSavedAt.IsZero() {
		t.Errorf("BoxID = %q, BoxIDSavedAt = %v, want neither on a fresh install", in.BoxID, in.BoxIDSavedAt)
	}

	other := NewInstall(now)
	if other.AndroidID == in.AndroidID || other.MediaDrmID == in.MediaDrmID || other.PUUID == in.PUUID {
		t.Errorf("two fresh installs share an ID: %+v and %+v", in, other)
	}
}

// TestInstallFill checks Fill sets only the empty values and reports whether
// it set any.
func TestInstallFill(t *testing.T) {
	now := time.UnixMilli(1790882444713)

	if got, filled := testInstall.Fill(now); filled || got != testInstall {
		t.Errorf("Fill on a full install = %+v, %v; want it unchanged, false", got, filled)
	}

	got, filled := Install{}.Fill(now)
	if !filled || got.AndroidID == "" || got.MediaDrmID == "" || got.PUUID == "" || got.FirstInstallTime.IsZero() || got.LastUpdateTime.IsZero() {
		t.Errorf("Fill on an empty install = %+v, %v; want every value set, true", got, filled)
	}

	// A pinned Android ID stays; the rest are filled in.
	got, filled = Install{AndroidID: "0123456789abcdef"}.Fill(now)
	if !filled || got.AndroidID != "0123456789abcdef" || got.PUUID == "" {
		t.Errorf("Fill = %+v, %v; want the pinned androidId kept and the rest filled", got, filled)
	}

	// A pinned update time earlier than any fresh install time pulls the
	// filled install time back to it.
	early := now.Add(-500 * 24 * time.Hour)
	got, _ = Install{LastUpdateTime: early}.Fill(now)
	if got.LastUpdateTime != early || got.FirstInstallTime.After(early) {
		t.Errorf("Fill = fit %v, lut %v; want lut kept and fit no later", got.FirstInstallTime, got.LastUpdateTime)
	}
	// And a pinned install time later than any fresh update time pushes the
	// filled update time up to it.
	late := now.Add(-time.Minute)
	got, _ = Install{FirstInstallTime: late}.Fill(now)
	if got.FirstInstallTime != late || got.LastUpdateTime.Before(late) {
		t.Errorf("Fill = fit %v, lut %v; want fit kept and lut no earlier", got.FirstInstallTime, got.LastUpdateTime)
	}

	// Filling only the times still counts, so the caller saves them.
	ids := Install{AndroidID: testInstall.AndroidID, MediaDrmID: testInstall.MediaDrmID, PUUID: testInstall.PUUID}
	if got, filled := ids.Fill(now); !filled || got.FirstInstallTime.IsZero() || got.LastUpdateTime.IsZero() {
		t.Errorf("Fill with only the IDs set = %+v, %v; want both times set, true", got, filled)
	}

	// Fill leaves the owner to For.
	if got, _ := (Install{}).Fill(now); got.DeviceID != "" {
		t.Errorf("Fill set DeviceID %q, want it left empty", got.DeviceID)
	}
}

// TestInstallFor checks an install binds to the first deviceId it's used
// with, and refuses any other.
func TestInstallFor(t *testing.T) {
	got, ok := testInstall.For("dev-1")
	if !ok || got.DeviceID != "dev-1" || got.AndroidID != testInstall.AndroidID {
		t.Fatalf("For on an unbound install = %+v, %v; want it bound to dev-1, true", got, ok)
	}
	if again, ok := got.For("dev-1"); !ok || again != got {
		t.Errorf("For with its own deviceId = %+v, %v; want it unchanged, true", again, ok)
	}
	if other, ok := got.For("dev-2"); ok || other != (Install{}) {
		t.Errorf("For with another deviceId = %+v, %v; want the zero Install, false", other, ok)
	}
}

// TestInstallRegistered checks the save time moves only when the box ID does.
func TestInstallRegistered(t *testing.T) {
	first, second := time.UnixMilli(1790000000123), time.UnixMilli(1790000999999)
	in, changed := testInstall.Registered("Bsmid-1", first)
	if !changed || in.BoxID != "Bsmid-1" || !in.BoxIDSavedAt.Equal(first) {
		t.Fatalf("Registered = %q at %v (changed %v), want Bsmid-1 at %v, true", in.BoxID, in.BoxIDSavedAt, changed, first)
	}
	if again, changed := in.Registered("Bsmid-1", second); changed || !again.BoxIDSavedAt.Equal(first) {
		t.Errorf("same box ID = save time %v, changed %v; want %v kept, false", again.BoxIDSavedAt, changed, first)
	}
	if next, changed := in.Registered("Bsmid-2", second); !changed || next.BoxID != "Bsmid-2" || !next.BoxIDSavedAt.Equal(second) {
		t.Errorf("Registered = %q at %v (changed %v), want Bsmid-2 at %v, true", next.BoxID, next.BoxIDSavedAt, changed, second)
	}
}

// TestInstallValidate checks each value a real install couldn't hold is
// refused, and that empty values pass.
func TestInstallValidate(t *testing.T) {
	if err := (Install{}).Validate(); err != nil {
		t.Errorf("empty Install.Validate() = %v, want nil", err)
	}
	registered, _ := testInstall.Registered("Bsmid", time.UnixMilli(1789500000000))
	if err := registered.Validate(); err != nil {
		t.Errorf("registered Install.Validate() = %v, want nil", err)
	}
	for name, mutate := range map[string]func(*Install){
		"uppercase androidId":    func(in *Install) { in.AndroidID = "3F9C1E0A7B2D4C58" },
		"short androidId":        func(in *Install) { in.AndroidID = "3f9c1e0a" },
		"mediaDrmId three parts": func(in *Install) { in.MediaDrmID = "AAEC__" },
		"mediaDrmId not base64":  func(in *Install) { in.MediaDrmID = "not base64!___" },
		"puuid with dashes":      func(in *Install) { in.PUUID = "6f1c2b3a-4d5e-4f60-a7b8-c9d0e1f20314" },
		"puuid not version 4":    func(in *Install) { in.PUUID = "ffeeddccbbaa99887766554433221100" },
		"puuid wrong variant":    func(in *Install) { in.PUUID = "6f1c2b3a4d5e4f6077b8c9d0e1f20314" },
		"update before install":  func(in *Install) { in.LastUpdateTime = in.FirstInstallTime.Add(-time.Second) },
		"boxId without B":        func(in *Install) { in.BoxID, in.BoxIDSavedAt = "smid", time.UnixMilli(1) },
		"boxId without saved at": func(in *Install) { in.BoxID = "Bsmid" },
		"saved at without boxId": func(in *Install) { in.BoxIDSavedAt = time.UnixMilli(1) },
	} {
		in := testInstall
		mutate(&in)
		err := in.Validate()
		if err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
			continue
		}
		// The error reaches the logs, so it must not carry the identifiers.
		for _, id := range []string{in.AndroidID, in.PUUID} {
			if strings.Contains(err.Error(), id) {
				t.Errorf("%s: Validate() = %q, which contains the value %q", name, err, id)
			}
		}
	}
}

// TestInstallJSON checks an install survives the session config's JSON, and
// that a fresh install's empty box ID fields stay out of it.
func TestInstallJSON(t *testing.T) {
	b, err := json.Marshal(testInstall)
	if err != nil {
		t.Fatalf("json.Marshal = %v", err)
	}
	if strings.Contains(string(b), "boxId") {
		t.Errorf("JSON = %s, want no boxId fields before a registration", b)
	}
	var got Install
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("json.Unmarshal = %v", err)
	}
	if got.AndroidID != testInstall.AndroidID || !got.FirstInstallTime.Equal(testInstall.FirstInstallTime) ||
		!got.LastUpdateTime.Equal(testInstall.LastUpdateTime) {
		t.Errorf("round trip = %+v, want %+v", got, testInstall)
	}
}
