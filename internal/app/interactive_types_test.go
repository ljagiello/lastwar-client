package app

import (
	"encoding/json"
	"testing"

	"lastwar-client/internal/sfs"
)

// TestPutJSONValueTypes covers 1.0.370 audit B82: an integer that fits in 32 bits is an SFS Int
// (most integer params are, e.g. AccountLoginNewMessage.lua:27 PutInt("type", 0)), a larger one a
// Long, and a ":i"/":l"/":d"/":s"/":b" key suffix picks the type and is stripped from the key.
func TestPutJSONValueTypes(t *testing.T) {
	cases := []struct {
		key      string
		value    any
		wantKey  string
		wantType byte
		wantVal  any
	}{
		{"type", json.Number("0"), "type", sfs.SFSInt, int32(0)},
		{"count", json.Number("-2147483648"), "count", sfs.SFSInt, int32(-2147483648)},
		{"big", json.Number("2147483648"), "big", sfs.SFSLong, int64(2147483648)},
		{"uuid:l", json.Number("42"), "uuid", sfs.SFSLong, int64(42)},
		{"type:i", json.Number("7"), "type", sfs.SFSInt, int32(7)},
		{"rate:d", json.Number("3"), "rate", sfs.SFSDouble, float64(3)},
		{"name:s", "x", "name", sfs.SFSUtfString, "x"},
		{"flag:b", true, "flag", sfs.SFSBool, true},
	}
	for _, c := range cases {
		o := sfs.NewSFSObject()
		if !putJSONValue(o, c.key, c.value) {
			t.Errorf("%s: putJSONValue = false, want true", c.key)
			continue
		}
		v, ok := o.Get(c.wantKey)
		if !ok || v.Type != c.wantType || v.Val != c.wantVal {
			t.Errorf("%s: got %q = %+v (present %v), want type %d value %v", c.key, c.wantKey, v, ok, c.wantType, c.wantVal)
		}
		if len(o.Keys()) != 1 {
			t.Errorf("%s: keys = %v, want only %q", c.key, o.Keys(), c.wantKey)
		}
	}

	for _, bad := range []struct {
		key   string
		value any
	}{
		{"type:i", json.Number("2147483648")}, // out of Int range
		{"type:i", json.Number("1.5")},
		{"uuid:l", "123"},            // a string for a Long
		{"name:s", json.Number("1")}, // a number for a string
		{"flag:b", "true"},           // a string for a Bool
		{"type:x", json.Number("1")}, // unknown suffix
		{":i", json.Number("1")},     // no key
	} {
		o := sfs.NewSFSObject()
		if putJSONValue(o, bad.key, bad.value) || len(o.Keys()) != 0 {
			t.Errorf("%s=%v: accepted, want it refused with nothing set", bad.key, bad.value)
		}
	}
}
