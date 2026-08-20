package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"strings"
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
// TestOsExitInInteractiveCallsConnCloseExplicitlyFirst is the round-41 regression test for the
// MINOR finding that RunInteractive's four os.Exit(1) sites (control-pipe stat/non-FIFO/open
// failures, persistent scan-error give-up) and handleInteractiveLine's two (SendExtension
// failure, non-timeout waitForCmd failure) never called conn.Close() first -- the identical
// defer-skipped-cleanup gap round 40's TestOsExitAfterDeferredConnCloseCallsCloseExplicitlyFirst
// (main_test.go) closed for main.go's own 4 sites, left unaddressed here even though main() and
// runCrossServerTest() both register `defer conn.Close()` before calling RunInteractive (which
// blocks until the process exits), so os.Exit from inside it skips that defer identically.
// Source-scanning is the honest way to pin this down (see that test's own doc comment for why: no
// black-box test can observe a behavioral difference, since killing the process also closes the
// socket either way).
func TestOsExitInInteractiveCallsConnCloseExplicitlyFirst(t *testing.T) {
	src, err := os.ReadFile("interactive.go")
	if err != nil {
		t.Fatalf("read interactive.go: %v", err)
	}

	re := regexp.MustCompile(`conn\.Close\(\)\s*\n\s*os\.Exit\(1\)`)
	matches := re.FindAll(src, -1)
	const want = 6 // RunInteractive's 4 sites + handleInteractiveLine's 2
	if len(matches) != want {
		t.Errorf("found %d conn.Close()-immediately-before-os.Exit(1) sites in interactive.go, want %d -- every os.Exit(1) reached while conn is in scope must call conn.Close() explicitly first, since os.Exit skips the caller's deferred conn.Close()", len(matches), want)
	}
}

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

// TestPutJSONValueRedactsSensitiveKeyOnUnparseableNumber is the round-41 regression test for the
// MAJOR finding that putJSONValue's json.Number error branch logged the raw operator-typed value
// unconditionally, bypassing every redaction layer this file otherwise enforces for exactly this
// scenario (see handleInteractiveLine's own JSON-decode-error/trailing-data branches, which
// explicitly avoid echoing raw operator text for the identical reason: it could carry a credential
// the operator meant to pass as params). An out-of-both-int64-and-float64-range JSON number
// literal (e.g. 1e400, which strconv.ParseFloat documents as returning +Inf with a non-nil range
// error) under a sensitive key name must now redact the logged value; a non-sensitive key must
// keep logging the real value, matching every other wrong-typed-field Warn/Error in this codebase.
func TestPutJSONValueRedactsSensitiveKeyOnUnparseableNumber(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantValue string // "" means the raw value must NOT appear in the log at all
	}{
		{name: "sensitive key redacts", key: "loginKey", wantValue: ""},
		{name: "non-sensitive key stays visible", key: "someField", wantValue: "1e400"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			orig := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			defer slog.SetDefault(orig)

			o := NewSFSObject()
			got := putJSONValue(o, tt.key, json.Number("1e400"))
			if got {
				t.Fatalf("putJSONValue() = true, want false for an out-of-range number")
			}

			logged := buf.String()
			if !strings.Contains(logged, "unparseable JSON number") {
				t.Fatalf("expected a Warn/Error mentioning the unparseable number, got log:\n%s", logged)
			}
			if tt.wantValue == "" {
				if strings.Contains(logged, "1e400") {
					t.Errorf("expected the raw value to be redacted for sensitive key %q, got log:\n%s", tt.key, logged)
				}
				if !strings.Contains(logged, "[REDACTED]") {
					t.Errorf("expected [REDACTED] in the log for sensitive key %q, got log:\n%s", tt.key, logged)
				}
			} else if !strings.Contains(logged, tt.wantValue) {
				t.Errorf("expected the real value %q to stay visible for non-sensitive key %q, got log:\n%s", tt.wantValue, tt.key, logged)
			}
		})
	}
}
