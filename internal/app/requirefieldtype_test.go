package app

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestRequireFieldTypeAcceptsOutOfRangeLongButGetIntReturnsZero is the round-30 regression test for
// the testing-rigor finding: no test previously combined an out-of-int32-range int64 value with
// requireFieldType(...) (buildings.go) returning true -- since the Go TYPE int64 is one
// sfsFieldKindInt accepts (see sfsFieldKindAccepts) -- followed by the corresponding accessor
// (GetInt, sfsobject.go) returning 0 on that same field. Only each half was tested in isolation
// before this: TestGetIntRejectsOutOfInt32RangeLong above proves GetInt's own zero-value fallback,
// while buildings_visitors_test.go's requireFieldType tests only exercise wrong-Go-TYPE fields, not
// a correctly-int64-typed-but-out-of-range one. This locks in the intentional "type-valid but
// value-invalid pass-through" design GetInt's own doc comment documents: requireFieldType/
// sfsFieldKindAccepts is a pure Go-type check, not a value-range check, so a present,
// correctly-int64-typed, but out-of-int32-range field passes requireFieldType's guard and then
// GetInt on that same field still degrades safely to its documented zero-value fallback rather than
// silently wrapping.
func TestRequireFieldTypeAcceptsOutOfRangeLongButGetIntReturnsZero(t *testing.T) {
	o := sfs.NewSFSObject()
	// 1<<32 + 5 is comfortably out of int32's range (and wraps to 5 under a naive int32(n)
	// truncation, the exact bug TestGetIntRejectsOutOfInt32RangeLong guards against) while still
	// being a plain int64 -- the Go type sfsFieldKindInt accepts.
	o.PutLong("bId", int64(1)<<32+5)

	if !requireFieldType(o, "bId", "test-context", sfsFieldKindInt) {
		t.Fatal("requireFieldType should accept an int64-typed field for sfsFieldKindInt even when its value is out of int32's range -- sfsFieldKindAccepts is a pure Go-type check, not a value-range check")
	}
	if got := o.GetInt("bId"); got != 0 {
		t.Errorf("GetInt(bId) = %d, want 0 (the documented zero-value fallback for an out-of-int32-range Long) even though requireFieldType passed", got)
	}
}
