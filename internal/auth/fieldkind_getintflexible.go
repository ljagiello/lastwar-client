package auth

import (
	"fmt"
	"log/slog"
	"math"
	"strconv"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// getIntFlexible reads a field that's usually an SFS numeric type but,
// confirmed live on serverInfo's `port`, is sometimes a UTF string
// instead (the response's other numeric-looking fields, like `id`, come
// through as real numbers -- this one specifically doesn't). Falls back
// to parsing the string form so a redirect doesn't silently resolve to
// port 0 depending on which type the server happened to send this time.
//
// Round 30 fix: the string-fallback path used to do a bare, unchecked int32(n) conversion on
// strconv.Atoi's result, mirroring the exact bug GetInt's own round-29 fix (sfsobject.go) closed
// for its int64 case -- on a 64-bit Platform Go's int is 64-bit, so Atoi happily parses a
// numeric string outside int32's range, and the bare conversion silently wraps it (e.g.
// "4294967301" -> 5) instead of rejecting it. Both real call sites (login.go's/crossserver.go's
// port-from-redirect reads) feed this straight into buildBaseZoneLoginAddr's redial, whose only
// port guard rejects non-positive values -- a wrapped small positive port would have sailed
// straight through. Now checked against math.MinInt32/MaxInt32 before converting, degrading to
// the same 0 fallback this function already uses for an absent/empty field, exactly like GetInt's
// own fix. See TestGetIntFlexibleRejectsOutOfInt32RangeString (redirect_helpers_test.go).
//
// Round 31 fix: this function used to have no diagnostic at all for a present-but-genuinely-
// anomalous field -- either a non-empty string that isn't a valid integer literal, or a value of
// some other Go type entirely (bool, float, nested object, ...) that neither the int-shaped nor
// string-shaped path above recognizes -- silently falling all the way through to the same 0
// fallback used for a merely-absent field, with zero signal distinguishing the two. This is the
// identical present-but-wrong-typed-vs-genuinely-absent distinction login.go's redirectIP/
// redirectZone (reading the ip/zone siblings on this SAME serverInfo object) already warn on --
// both functions' own doc comments cite this function's port-string precedent as the reason that
// distinction matters, but the precedent itself was never given the matching diagnostic. Added
// below, without changing either success path's existing behavior at all.
//
// Round 32 fix: round 31's enumeration of anomaly cases above omitted a THIRD one that already
// existed in this function at the time: the out-of-int32-range numeric string case round 30's own
// fix (above) guards against. That branch still silently returned 0 with zero diagnostic, exactly
// as anomalous as the two round 31 did add warnings for -- an out-of-range "port" string is just
// as real an anomaly as a non-numeric one, and was indistinguishable in the logs from a merely-
// absent field. Now warns here too, using the same message shape as its non-numeric-string sibling
// immediately below.
//
// Round 33 fix: after three straight rounds of incrementally discovering this function still had
// an uncovered anomaly branch, a final exhaustive re-check found a FOURTH: a present, correctly-
// typed int64 Long whose VALUE doesn't fit in int32's range (GetInt's own round-29 fix already
// degrades this to 0, but silently) was still indistinguishable from a merely-absent field, since
// neither the string-fallback path nor the final wrong-Go-type check (both below) can see a
// same-value-range problem on an already-int64-typed field. Checked directly against the raw
// decoded value, immediately after the initial GetInt() call.
//
// Round 41 fix: that round-33 check was REMOVED, not kept, once GetInt itself (sfsobject.go)
// gained its own out-of-int32-range-Long Warn in round 39, closing the exact "GetInt's own fix
// degrades this to 0, but silently" gap this comment describes. From round 39 onward, the initial
// `o.GetInt(key)` call above already warns and returns 0 for this anomaly before this function's
// own duplicate check could ever run, so the round-33 block had become dead weight: every
// triggering input produced two separate Warn log lines (GetInt's own, then this function's
// identical-in-substance one) for a single anomaly, confirmed by direct reproduction. Removed
// rather than left in place, since GetInt's diagnostic already carries the same key/redacted-value
// information this function's own would have.
func getIntFlexible(o *sfs.SFSObject, key string) int32 {
	if n := o.GetInt(key); n != 0 {
		return n
	}
	// Round-35 fix: redactedValue gates the three raw-scalar "value" log args below on
	// sfs.IsSensitiveSFSKey(key), matching every sibling wrong-typed-field guard in this codebase
	// (requireFieldType/warnIfWrongTypedField/redirectIP/redirectZone all log only
	// StringRedacted()/goType, never a field's own raw scalar). getIntFlexible is a generic,
	// key-parameterized helper -- today's only call sites hardcode key="port" (never sensitive),
	// but a future caller passing a sensitive key would otherwise leak its real value into these
	// three anomaly-diagnostic Warn calls with no redaction at all, unlike this function's own
	// fourth branch below, which already used the safe StringRedacted() pattern.
	redactedValue := func(v any) any {
		if sfs.IsSensitiveSFSKey(key) {
			return "[REDACTED]"
		}
		return v
	}
	if s := o.GetString(key); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			if n < math.MinInt32 || n > math.MaxInt32 {
				slog.Warn("serverInfo redirect: field present as an out-of-int32-range numeric string, falling back to 0",
					"key", key, "value", redactedValue(s))
				return 0
			}
			return int32(n)
		}
		slog.Warn("serverInfo redirect: field present as a non-numeric string, falling back to 0",
			"key", key, "value", redactedValue(s))
		return 0
	}
	// Neither GetInt nor GetString produced anything -- either key is genuinely absent (silent,
	// the ordinary case) or it's present with some other Go type entirely, which neither accessor
	// recognizes -- warn only for the latter.
	if v, ok := o.Get(key); ok && v.Val != nil &&
		!session.SFSFieldKindAccepts(session.SFSFieldKindInt, v.Val) && !session.SFSFieldKindAccepts(session.SFSFieldKindString, v.Val) {
		slog.Warn("serverInfo redirect: field present but wrong-typed (neither numeric nor string) -- falling back to 0",
			"key", key, "goType", fmt.Sprintf("%T", v.Val), "raw", o.StringRedacted())
	}
	return 0
}
