package game

import (
	"time"

	"lastwar-client/internal/sfs"
)

// Init is the server's post-login `init` push: Raw is the whole object (247 top-level keys in a
// 2026-10-04 capture), Buildings/Visitors are the parts FetchBuildings already decoded. Every
// feature reads its eligibility from Raw -- e.g. vip.vipInfo, collect_reward, compensateArr,
// freeRewardArr -- through the nil-safe accessors below, so a missing or wrong-typed field reads
// as absent instead of panicking. Raw is nil when no init push arrived (FetchBuildings fell back to
// push.init.build), and features must then skip themselves.
type Init struct {
	Raw       *sfs.SFSObject
	Buildings []Building
	Visitors  []Visitor
}

// Object returns the nested object under key, or nil.
func (in *Init) Object(key string) *sfs.SFSObject {
	if in == nil {
		return nil
	}
	v, ok := in.Raw.Get(key)
	if !ok {
		return nil
	}
	o, _ := v.Val.(*sfs.SFSObject)
	return o
}

// Array returns the items of the array under key, or nil.
func (in *Init) Array(key string) []sfs.SFSValue {
	if in == nil {
		return nil
	}
	v, ok := in.Raw.Get(key)
	if !ok {
		return nil
	}
	a, _ := v.Val.(*sfs.SFSArray)
	if a == nil {
		return nil
	}
	return a.Items()
}

// Objects returns the object elements of the array under key, skipping anything else.
func (in *Init) Objects(key string) []*sfs.SFSObject {
	var out []*sfs.SFSObject
	for _, it := range in.Array(key) {
		if o, ok := it.Val.(*sfs.SFSObject); ok {
			out = append(out, o)
		}
	}
	return out
}

// ServerDayStart returns the start of the server day containing now. The server day does not
// follow the host clock: init.tomorrow is the unix second of the next reset (02:00 UTC for the
// account tested live, real_timezone_offset -7200), the anchor the 1.0.364 client uses for its
// day-change refresh. ok is false when init carried no usable tomorrow.
func (in *Init) ServerDayStart(now time.Time) (start time.Time, ok bool) {
	if in == nil {
		return time.Time{}, false
	}
	t := in.Raw.GetLong("tomorrow")
	if t <= 0 {
		return time.Time{}, false
	}
	start = time.Unix(t, 0).Add(-24 * time.Hour)
	for now.Before(start) {
		start = start.Add(-24 * time.Hour)
	}
	for !now.Before(start.Add(24 * time.Hour)) {
		start = start.Add(24 * time.Hour)
	}
	return start, true
}
