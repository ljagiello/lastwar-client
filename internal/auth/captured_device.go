package auth

import (
	"log/slog"

	"lastwar-client/internal/sfs"
)

// CapturedDevice is what a real iOS Login said about its device, recorded from a capture of the
// real app (cmd/pcap -session-out) and replayed by BuildLoginParams in iOS mode, so the Login
// describes the device the session came from instead of this client's placeholders. The 1.0.370
// iOS client asks Unity and native code for each one (LoginMessage.CSSetData,
// Assembly-CSharp.decompiled.cs:83053-83185), so none can be derived offline. An empty field keeps
// what this client sends without a capture. The JSON names are the Login keys.
//
// Ta is the captured Login's whole ta blob. The Login replays it with the keys that change per
// build or per Login taken from this run (taRefresh, identity.go), as the client's own super
// properties would have them.
type CapturedDevice struct {
	PhoneModel  string `json:"phone_model,omitempty"`  // SystemInfo.deviceModel (:83149, 73386)
	OSVersion   string `json:"osVersion,omitempty"`    // SystemInfo.operatingSystem (:83152, 73351)
	HandSetInfo string `json:"mt,omitempty"`           // native PM_getHandSetInfo (:83090)
	Idfv        string `json:"idfv,omitempty"`         // native Get_IDFV (:83136-83138)
	DistinctID  string `json:"distinct_id,omitempty"`  // ThinkingData's distinct id (:83073)
	FromCountry string `json:"fromCountry,omitempty"`  // GlobalData.fromCountry, also GSL `country` (:83128, 77433)
	Lang        string `json:"lang,omitempty"`         // Localization.GetLanguageName(), also GSL `lang` (:83158, 77435)
	PhoneScreen string `json:"phone_screen,omitempty"` // Screen.width*Screen.height (:83074)
	Afuid       string `json:"afuid,omitempty"`        // AppsFlyer uid (:83146-83148)
	FirebaseID  string `json:"firebaseId,omitempty"`   // native FireBase_GetFireBaseId (:83139-83140)
	Ta          string `json:"ta,omitempty"`           // SDKManager.GetTaPresetProp() (:83072, 59270-59279)
}

// String/GoString/LogValue redact: idfv, distinct_id, afuid, firebaseId and ta carry device
// identifiers (sfs.SensitiveSFSKeys lists them), like every other identity-bearing type here.
func (d CapturedDevice) String() string       { return "[REDACTED CapturedDevice]" }
func (d CapturedDevice) GoString() string     { return d.String() }
func (d CapturedDevice) LogValue() slog.Value { return slog.StringValue(d.String()) }

// IsZero reports whether no field was captured.
func (d CapturedDevice) IsZero() bool { return d == CapturedDevice{} }

// encodable returns d with every field too long for an SFS UtfString blanked, so a hand-edited or
// corrupt session config can't make the Login unsendable; each one dropped is logged by name.
func (d CapturedDevice) encodable() CapturedDevice {
	for _, f := range []struct {
		name string
		v    *string
	}{
		{"phone_model", &d.PhoneModel}, {"osVersion", &d.OSVersion}, {"mt", &d.HandSetInfo}, {"idfv", &d.Idfv},
		{"distinct_id", &d.DistinctID}, {"fromCountry", &d.FromCountry}, {"lang", &d.Lang},
		{"phone_screen", &d.PhoneScreen}, {"afuid", &d.Afuid}, {"firebaseId", &d.FirebaseID}, {"ta", &d.Ta},
	} {
		if len(*f.v) > sfs.MaxUtfStringBytes {
			slog.Warn("captured device field too long to send; leaving it out", "field", f.name, "len", len(*f.v), "max", sfs.MaxUtfStringBytes)
			*f.v = ""
		}
	}
	return d
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
