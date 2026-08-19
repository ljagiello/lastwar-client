package main

import (
	"encoding/json"
	"testing"
)

// putJSONValue is the only piece of interactive.go's JSON-decoding logic
// with no I/O dependency (everything else needs a live control FIFO), so
// it's exercised directly here instead of through RunInteractive.
//
// The json.Number cases are the ones worth getting right: handleInteractiveLine
// decodes with UseNumber specifically because uuids routinely exceed
// float64's 53-bit exact-integer range, so putJSONValue must prefer an
// exact int64 (PutLong) and only fall back to a lossy float64 (PutDouble)
// when the number can't be represented as one. We check the *type tag*
// SFSObject actually stored, not just the value, since a wrong-but-close
// float64 would otherwise pass a value-only comparison.
func TestPutJSONValue(t *testing.T) {
	cases := []struct {
		name   string
		value  any
		wantOK bool
		check  func(t *testing.T, o *SFSObject) // only run when wantOK
	}{
		{
			name:   "string",
			value:  "hello",
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				if got := o.GetString("k"); got != "hello" {
					t.Errorf("GetString() = %q, want %q", got, "hello")
				}
			},
		},
		{
			name:   "bool true",
			value:  true,
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				v, _ := o.Get("k")
				if v.Type != sfsBool || v.Val != true {
					t.Errorf("got %+v, want sfsBool(true)", v)
				}
			},
		},
		{
			name:   "bool false",
			value:  false,
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				v, _ := o.Get("k")
				if v.Type != sfsBool || v.Val != false {
					t.Errorf("got %+v, want sfsBool(false)", v)
				}
			},
		},
		{
			// 19 digits: past float64's exact-integer range but still
			// within int64, i.e. an ordinary uuid. Must land as sfsLong.
			name:   "json.Number that fits int64 uses PutLong",
			value:  json.Number("1234567890123456789"),
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				v, _ := o.Get("k")
				if v.Type != sfsLong {
					t.Fatalf("got type %d, want sfsLong (%d)", v.Type, sfsLong)
				}
				if got := o.GetLong("k"); got != 1234567890123456789 {
					t.Errorf("GetLong() = %d, want 1234567890123456789", got)
				}
			},
		},
		{
			// A fractional value can never be an Int64(); this is the
			// ordinary PutDouble fallback path.
			name:   "fractional json.Number falls back to PutDouble",
			value:  json.Number("123.45"),
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				v, _ := o.Get("k")
				if v.Type != sfsDouble {
					t.Fatalf("got type %d, want sfsDouble (%d)", v.Type, sfsDouble)
				}
				if got, ok := v.Val.(float64); !ok || got != 123.45 {
					t.Errorf("got %v, want 123.45", v.Val)
				}
			},
		},
		{
			// 20 digits: too large for int64 (max ~9.22e18, 19 digits) but
			// still plain digits, so Int64() fails and Float64() succeeds --
			// the exact precision-losing case the UseNumber comment warns
			// about, reached here instead of avoided.
			name:   "int64-overflowing json.Number falls back to PutDouble",
			value:  json.Number("99999999999999999999"),
			wantOK: true,
			check: func(t *testing.T, o *SFSObject) {
				v, _ := o.Get("k")
				if v.Type != sfsDouble {
					t.Fatalf("got type %d, want sfsDouble (%d)", v.Type, sfsDouble)
				}
			},
		},
		{
			name:   "unparseable json.Number is rejected",
			value:  json.Number("not-a-number"),
			wantOK: false,
		},
		{
			// What json.Unmarshal with UseNumber actually produces for a
			// JSON object value.
			name:   "unsupported nested map is rejected",
			value:  map[string]any{"a": 1},
			wantOK: false,
		},
		{
			// What json.Unmarshal with UseNumber actually produces for a
			// JSON array value.
			name:   "unsupported slice is rejected",
			value:  []any{1, 2, 3},
			wantOK: false,
		},
		{
			name:   "unsupported nil is rejected",
			value:  nil,
			wantOK: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := NewSFSObject()
			got := putJSONValue(o, "k", c.value)
			if got != c.wantOK {
				t.Fatalf("putJSONValue() = %v, want %v", got, c.wantOK)
			}
			if !c.wantOK {
				if o.Has("k") {
					t.Error("putJSONValue returned false but still set the key")
				}
				return
			}
			if c.check != nil {
				c.check(t, o)
			}
		})
	}
}
