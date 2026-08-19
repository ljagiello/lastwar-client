package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// SFS2X SFSDataType tags, per the reverse-engineered wire format (see dossier §04).
const (
	sfsNull           = 0
	sfsBool           = 1
	sfsByte           = 2
	sfsShort          = 3
	sfsInt            = 4
	sfsLong           = 5
	sfsFloat          = 6
	sfsDouble         = 7
	sfsUtfString      = 8
	sfsBoolArray      = 9
	sfsByteArray      = 10
	sfsShortArray     = 11
	sfsIntArray       = 12
	sfsLongArray      = 13
	sfsFloatArray     = 14
	sfsDoubleArray    = 15
	sfsUtfStringArray = 16
	sfsArrayType      = 17
	sfsObjectType     = 18
	sfsClass          = 19 // unused/unimplemented by the game
	sfsText           = 20
)

// SFSValue is a single tagged field value.
type SFSValue struct {
	Type byte
	Val  interface{}
}

// String makes SFSValue satisfy fmt.Stringer safely, mirroring *SFSObject/*SFSArray's own
// String()/StringRedacted() treatment (rounds 14-15). Every current .Get() call site type-asserts
// straight through .Val, or hands the whole SFSObject/SFSArray recursively to StringRedacted() --
// none formats a bare SFSValue itself -- so this is currently latent, not actively exploited. But
// it's the same shape of gap those rounds closed: without this, an idiomatic future call site like
// `fmt.Sprintf("...: %v", someVal)` on an SFSValue extracted via o.Get("loginKey") would fall
// through to Go's default reflection-based struct formatter and print v.Val -- a live
// secret -- in full cleartext, since Go's struct-field printer has no notion of "this field is
// sensitive."
//
// Unlike *SFSObject/*SFSArray, a bare SFSValue carries no key/field-name context at all to check
// against sensitiveSFSKeys (that check only ever happens one level up, in the parent SFSObject
// that held this value under a specific key) -- so, mirroring the bare *SFSArray.StringRedacted()
// method's own reasoning for the identical "no key context to lean on" situation, this
// blanket-masks unconditionally rather than risk ever printing a value that turns out to be
// sensitive.
func (v SFSValue) String() string {
	return "[REDACTED SFSValue]"
}

// GoString makes SFSValue satisfy fmt.GoStringer, mirroring String() above the same way
// *SFSObject.GoString()/*SFSArray.GoString() mirror their own String() methods: without this,
// %#v on a bare SFSValue falls through to Go's default reflection-based formatter, dumping its
// Val field (and Type) raw.
func (v SFSValue) GoString() string {
	return v.String()
}

// SFSObject is an ordered string-keyed map, matching the client's own
// "insert order doesn't matter for lookup, but we preserve it for wire
// determinism" behavior.
type SFSObject struct {
	keys   []string
	values map[string]SFSValue
}

func NewSFSObject() *SFSObject {
	return &SFSObject{values: make(map[string]SFSValue)}
}

func (o *SFSObject) put(key string, v SFSValue) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = v
}

func (o *SFSObject) PutUtfString(key, val string)      { o.put(key, SFSValue{sfsUtfString, val}) }
func (o *SFSObject) PutBool(key string, val bool)      { o.put(key, SFSValue{sfsBool, val}) }
func (o *SFSObject) PutByte(key string, val byte)      { o.put(key, SFSValue{sfsByte, val}) }
func (o *SFSObject) PutShort(key string, val int16)    { o.put(key, SFSValue{sfsShort, val}) }
func (o *SFSObject) PutInt(key string, val int32)      { o.put(key, SFSValue{sfsInt, val}) }
func (o *SFSObject) PutLong(key string, val int64)     { o.put(key, SFSValue{sfsLong, val}) }
func (o *SFSObject) PutDouble(key string, val float64) { o.put(key, SFSValue{sfsDouble, val}) }
func (o *SFSObject) PutSFSObject(key string, val *SFSObject) {
	o.put(key, SFSValue{sfsObjectType, val})
}
func (o *SFSObject) PutSFSArray(key string, val *SFSArray) { o.put(key, SFSValue{sfsArrayType, val}) }

// Has reports whether key is present in the decoded object.
func (o *SFSObject) Has(key string) bool { _, ok := o.values[key]; return ok }

// Get returns the raw SFSValue stored under key, and whether it was present.
func (o *SFSObject) Get(key string) (SFSValue, bool) { v, ok := o.values[key]; return v, ok }

// GetString reads a field as string, returning "" if the field is absent or its concrete decoded
// Go type isn't string -- the same "treat as absent/zero-value" fallback GetInt/GetLong use for a
// wrong-typed field, not a panic or an error. Both sfsUtfString and sfsText wire tags decode to
// Go's plain string type (see the decode switch below), so there is no further wire-tag-level
// distinction to make here.
func (o *SFSObject) GetString(key string) string {
	if v, ok := o.values[key]; ok {
		if s, ok := v.Val.(string); ok {
			return s
		}
	}
	return ""
}

// GetInt reads a field as int32, accepting the same narrower integer types GetLong does (int16,
// byte) plus int64 -- SFS2X's Long tag often carries a value this repo reads through the narrower
// Int-shaped accessor anyway (buildings.go's Building.BId()/Level()/PointId(), mail.go's
// Mail.Type()/RewardStatus(), visitors.go's Visitor.EventId()/VisitorId(), alliance.go's
// findRecommendedTech via tech.GetInt("state")/GetInt("scienceId")) -- but an int64 value that does
// NOT actually fit in int32's range is treated the same way this function already treats a wrong-
// Go-typed or absent field: as a zero value, never as a silently wrapped one.
//
// Round 29 fix: the int64 case used to do a bare, unchecked int32(n) conversion, which Go
// truncates/wraps (modulo 2^32) rather than errors on -- an out-of-int32-range Long used to come out
// as a small, unrelated, possibly-negative int32 instead of being rejected. That defeated
// requireFieldType's (buildings.go) own documented guarantee that "requireFieldType's check and the
// accessor's own behavior can never disagree": requireFieldType/sfsFieldKindAccepts only checks that
// the field's concrete Go TYPE is one GetInt accepts (int64/int32/int16/byte), not that an int64
// VALUE actually fits in int32's range -- so a present, correctly-int64-typed, but out-of-range field
// used to sail straight past requireFieldType's guard and then come out of GetInt corrupted, for
// every one of the real call sites listed above (e.g. a wrapped bId colliding with an unrelated
// building in buildings.go's map-keyed lookups, or findRecommendedTech donating to the wrong tech).
//
// Returning 0 here -- the same zero-value fallback this function already uses for a wrong-Go-typed
// field -- is the fix, consistent with this codebase's existing "treat as absent/zero-value rather
// than corrupt" philosophy (see requireFieldType's own doc comment). Whether sfsFieldKindAccepts
// (buildings.go) should ALSO learn to reject an out-of-range int64 value -- making requireFieldType
// itself catch this case the same way it already catches a wrong Go type -- was considered and
// deliberately left alone: sfsFieldKindAccepts documents itself as a pure TYPE-family check
// ("identifies which family of concrete decoded Go types"), and every field this repo actually reads
// via GetInt is, in every real captured payload, a small in-range identifier/counter -- an
// out-of-range Long under one of these keys is itself a sign of a decode desync or a hostile/corrupt
// payload, not a legitimate large ID this client needs to preserve. GetInt's own fix already
// degrades that case the same safe way a wrong-typed field degrades (a dedup-map/lookup collision
// risk, not memory corruption or a crash), so duplicating a value-range check into
// sfsFieldKindAccepts too would add real complexity (a kind-specific range check inside what is
// today a pure, kind-only type switch, and one only GetInt/sfsFieldKindInt would ever need --
// sfsFieldKindLong's own accessor, GetLong, never narrows and so has no equivalent gap) for no
// behavioral gain beyond what GetInt's own fix already provides. See
// TestGetIntRejectsOutOfInt32RangeLong (sfsobject_array_test.go) for the regression coverage proving
// the wrap no longer happens.
func (o *SFSObject) GetInt(key string) int32 {
	if v, ok := o.values[key]; ok {
		switch n := v.Val.(type) {
		case int32:
			return n
		case int16:
			return int32(n)
		case byte:
			return int32(n)
		case int64:
			if n < math.MinInt32 || n > math.MaxInt32 {
				return 0
			}
			return int32(n)
		}
	}
	return 0
}

// GetLong reads a field as int64, accepting the same narrower integer types GetInt does (int32,
// int16, byte). Unlike GetInt, it never needs a range check against its own return type: int64 is
// GetLong's native return type, so every accepted narrower type widens into it without any
// possibility of overflow -- see GetInt's own doc comment above for why ITS int64 case needs one.
func (o *SFSObject) GetLong(key string) int64 {
	if v, ok := o.values[key]; ok {
		switch n := v.Val.(type) {
		case int64:
			return n
		case int32:
			return int64(n)
		case int16:
			return int64(n)
		case byte:
			return int64(n)
		}
	}
	return 0
}

// String makes *SFSObject satisfy fmt.Stringer safely: it delegates to StringRedacted() rather
// than a raw, unredacted dump, so any code path that hands a *SFSObject to fmt's %v/%s verbs, a
// Print-family function, or slog's Any-kind attribute formatting -- all of which auto-invoke
// Stringer with zero literal ".String()" text in the source, a pattern
// credential_leak_lint_test.go's text-scanning approach structurally cannot see -- is redacted by
// construction instead of leaking a live loginKey/accessToken/airKey/etc. This means an ordinary,
// idiomatic fmt.Errorf("...: %v", someSFSObject) or slog.Info("resp", "params", someSFSObject) is
// safe by default, closing the gap for good rather than only for today's known call sites.
//
// Round 14 introduced this delegation (the pre-round-14 String() was itself the raw, unredacted
// dump, later renamed to the unexported unsafeRawString()). Round 15 deleted unsafeRawString() and
// its formatSFSValue() recursion helper entirely, once the round-15 audit (via `go run
// golang.org/x/tools/cmd/deadcode@latest .`) confirmed nothing called them anymore -- String() has
// delegated straight to StringRedacted() since round 14, so the raw-dump path had been unreachable
// dead code (0% test coverage) ever since.
func (o *SFSObject) String() string {
	return o.StringRedacted()
}

// GoString makes *SFSObject satisfy fmt.GoStringer -- the interface Go's %#v verb uses -- the same
// way String() above satisfies fmt.Stringer. Without this, %#v on a *SFSObject falls through to
// Go's default reflection-based struct formatter, which dumps every internal field (including the
// unexported values map) completely raw, printing a live loginKey/accessToken/etc. in full
// cleartext. No live %#v usage exists anywhere in this repo today, but it's an extremely common,
// idiomatic Go debugging verb any future contributor might reach for without realizing SFSObject
// needs special handling -- this closes that gap the same way round 14 closed the %v/Stringer one.
func (o *SFSObject) GoString() string {
	return o.StringRedacted()
}

// sensitiveSFSKeys lists the field names this protocol is known to carry live credentials/tokens
// under, across every login/session response and request this repo decodes or builds (see
// login.go's redact() call sites, identity.go's BuildLoginParams, and gsl.go's
// LoginServerListRespon.At/Rt) -- loginKey/accountArr's sibling, gameUid, is deliberately not
// included: it identifies an account but isn't a bearer credential by itself.
var sensitiveSFSKeys = map[string]bool{
	"loginKey":    true,
	"at":          true,
	"rt":          true,
	"accessToken": true,
	"airKey":      true,
	"shumeiBoxId": true,
	"pw":          true,
	"password":    true,
	// verifyCode is the live one-time email-verification code account.login.new sends to
	// complete login (see login.go's finishParams.PutUtfString("verifyCode", code)).
	"verifyCode": true,
	// deviceId is, together with airKey (already above), the actual SFS-layer bearer credential
	// for the base zone Login (see login.go's/identity.go's BuildLoginParams doc comments) -- it
	// always appears alongside airKey in loginParams, so it must redact the same way.
	"deviceId": true,
	// chatToken is documented (docs/auth.mdx, docs/alliance-chat-mail.mdx) as a live bearer
	// credential for the separate chat WebSocket, carried in the `init` push's params. Not yet
	// consumed by any Go code (chat isn't implemented) -- added pre-emptively/defense-in-depth.
	"chatToken": true,
	// tk is the vanilla SFS2X Handshake response's session token -- docs/wire-protocol.mdx
	// documents a real captured Handshake response shape `{ct=3072, ms=1000000, tk=<32-hex>}`
	// from the live production server, explicitly calling tk a session token.
	"tk": true,
	// ta is the iOS login's analytics blob (identity.go's BuildLoginParams/iosAnalyticsBlob):
	// a JSON-marshaled string, not a scalar, so StringRedacted() has no way to redact secrets
	// nested inside it field-by-field -- it can only mask the whole "ta" value or none of it.
	// identity.go no longer puts real DeviceID/ShumeiBoxId/AirKey values into that blob, but
	// this stays as defense-in-depth in case a future field embeds something sensitive inside
	// this or another opaque string value.
	"ta": true,
	// mail is the operator's own real email address, put there by login.go's email-verification
	// flow (PutUtfString("mail", opts.Email), both the account.login.send.verify.code and
	// account.login.new call sites). It's PII -- the account operator's own email address -- not
	// a bearer credential, but is added defensively so any current/future StringRedacted() dump
	// of a request/response carrying this field masks it instead of printing a real email
	// address in cleartext.
	"mail": true,
	// un is the classic SFS2X username field -- the server's real returned account username
	// (env.Content.GetString("un") on the base zone Login response, checked in login.go). Same PII
	// class as mail immediately above: the operator's own real account name, not a bearer
	// credential, but must be masked so any current/future StringRedacted() dump of a decoded
	// response doesn't print it in cleartext. crossserver.go's LWDEBUG_DUMP_LOGIN debug dump of
	// loginContent.StringRedacted() documents itself, in its own comment, as "Redacted, not a raw
	// dump" -- this entry is what makes that claim actually true for "un".
	"un": true,
	// gameUserName is "un"'s exact sibling on the OTHER login path: login.go's Login() reads two
	// distinct server-returned username fields carrying the identical real-account-username value
	// in the same flow -- "un" on the base-zone Login response, and "gameUserName" on the
	// post-email-verification push.account.login.new response (msg2.Params.GetString("gameUserName")),
	// both persisted via the identical ident.SaveUsername() call. Round 31 fix: this key was missing
	// here despite "un"'s own doc comment above making the identical PII argument for it -- unlike
	// the placeholder-only device/tracking-identifier cluster below, gameUserName carries a genuine
	// value from a real server response in this client's own normal operation today, and
	// result.Account (msg2.Params, unredacted) is handed back to every caller of Login(). Without
	// this entry, decode.go's -decode-stream tool (which calls StringRedacted() unconditionally on
	// every decoded packet) would print a real captured account's username in cleartext.
	"gameUserName": true,
	// The following are the device/advertising-identifier PII cluster documented in
	// docs/live-validation.mdx's "complete Login params field list" section (IMEI, AndroidID,
	// androidDid, gaid, afuid, firebaseId, distinct_id) and its iOS reconnect-fix section (idfa,
	// idfv) as real fields a genuine (non-Go-client) Login request carries. identity.go's own
	// BuildLoginParams currently only ever sends these as empty-string placeholders (this Go
	// client doesn't have real values for them), so today's Go-client-originated traffic leaks
	// nothing under these keys -- but decode.go's -decode-stream tool (this repo's own documented
	// tool for decoding a REAL captured non-Go-client login) would print real values for these
	// fields in cleartext, since StringRedacted() has no way to mask a key that isn't in this
	// map. Not bearer credentials -- device/tracking identifiers -- added so a real captured
	// login decoded via -decode-stream doesn't leak them.
	"IMEI":        true,
	"AndroidID":   true,
	"androidDid":  true,
	"idfa":        true,
	"idfv":        true,
	"gaid":        true,
	"afuid":       true,
	"firebaseId":  true,
	"distinct_id": true,
	// gcmRegisterId/parseRegisterId are push-notification device tokens (Android FCM/legacy GCM
	// and Parse push, respectively) -- confirmed real fields identity.go's BuildLoginParams
	// constructs (PutUtfString("gcmRegisterId", ...) / PutUtfString("parseRegisterId", ...)).
	// Same actionable-device-targeting risk class as firebaseId above, and the same "only ever
	// sent as an empty-string placeholder by this Go client today, but a real captured
	// non-Go-client login decoded via -decode-stream would leak it" reasoning applies.
	"gcmRegisterId":   true,
	"parseRegisterId": true,
	// googleName is a Google account display name -- confirmed real field identity.go's
	// BuildLoginParams constructs (PutUtfString("googleName", "")). More directly PII than a
	// device/tracking identifier: it's a real person's name, not just an identifier for one.
	"googleName": true,
	// googlePlay sits in the same Google-identity field cluster identity.go's BuildLoginParams
	// constructs consecutively with googleName/gcmRegisterId immediately above/below (set in this
	// order: googlePlay, androidDid, googleName, deeplinkParams, pfId) -- same "only ever sent as
	// an empty-string placeholder by this Go client today, but a real captured non-Go-client login
	// decoded via -decode-stream would leak it in cleartext" reasoning as its neighbors.
	"googlePlay": true,
	// mt sits in the same field cluster per docs/live-validation.mdx's "complete Login params
	// field list" (`AndroidID, IMEI, psh, mt, deviceId, airKey, ...`) -- undocumented meaning, but
	// the same "not yet leaking from this Go client's own placeholder traffic, but a real captured
	// non-Go-client login decoded via -decode-stream would leak it" reasoning already applied to
	// its neighbors applies here too. Confirmed real field identity.go constructs
	// (PutUtfString("mt", "")).
	"mt": true,
	// simOp/simOpName/phone_model/osVersion/phone_screen/phone_native_screen round out the
	// device/carrier-identifier cluster for full consistency with the rationale above -- all
	// confirmed real fields identity.go's BuildLoginParams constructs (PutUtfString calls for
	// each).
	"simOp":               true,
	"simOpName":           true,
	"phone_model":         true,
	"osVersion":           true,
	"phone_screen":        true,
	"phone_native_screen": true,
	// SecurityCode/OneCode/CoreV/packageSign/psh were considered and deliberately excluded: per
	// docs/auth.mdx, they're reproducible MD5/SHA1 hashes over non-secret/publicly-extractable
	// inputs (cmdBaseTime, gameUid, packageName, a hardcoded salt, and -- for psh specifically --
	// the APK's own signing-cert DER bytes, which are themselves not secret) rather than live
	// bearer tokens or PII, so this decision doesn't need re-deriving next round.
}

// sensitiveSFSKeysLower is a case-insensitive lookup mirror of sensitiveSFSKeys, built once (at
// package init, from sensitiveSFSKeys' own literal keys) rather than lowercasing sensitiveSFSKeys
// itself in place -- see isSensitiveSFSKey's doc comment for why a case-insensitive check is
// needed, and why sensitiveSFSKeys' own keys stay exact-case for any other/future reader of that
// map.
var sensitiveSFSKeysLower = buildSensitiveSFSKeysLower()

func buildSensitiveSFSKeysLower() map[string]bool {
	m := make(map[string]bool, len(sensitiveSFSKeys))
	for k := range sensitiveSFSKeys {
		m[strings.ToLower(k)] = true
	}
	return m
}

// isSensitiveSFSKey reports whether k names a known-sensitive field, case-insensitively against
// sensitiveSFSKeys' registered (exact-case) key names. A plain `sensitiveSFSKeys[k]` map lookup is
// exact-case only: interactive.go's putJSONValue takes a JSON object key from the operator's
// control-FIFO line verbatim, with no case normalization, so a casing variant of a known-sensitive
// key (e.g. an operator typing "LoginKey" instead of the registered "loginKey") would bypass
// redactSFSValue entirely and fall through to formatSFSValueRedacted's plain
// fmt.Sprintf("%v", val) -- printing a secret typed under a mis-cased key in full cleartext in
// local logs. Comparing against sensitiveSFSKeysLower closes that gap while leaving
// sensitiveSFSKeys' own keys unchanged.
func isSensitiveSFSKey(k string) bool {
	return sensitiveSFSKeysLower[strings.ToLower(k)]
}

// maxFormattedNodes bounds the total number of key/item nodes a single top-level StringRedacted()
// call (and the String()/GoString() methods that delegate to it) will examine before truncating
// with a visible marker -- independent of maxDecodedNodes below, which only bounds DECODE-time cost
// for one wire payload, not a later format/log walk of an object that's already sitting in memory.
// Two real gaps this closes:
//
//  1. requirePresentField (buildings.go/mail.go/alliance.go/visitors.go) calls o.String() (->
//     StringRedacted()) on a SINGLE array item when a required field is missing. That one item's
//     own internal node count is bounded only by the overall maxDecodedNodes=300,000 decode budget
//     for the WHOLE payload -- none of this repo's own raw-item-scan-count caps (buildings.go's
//     maxRawBuildingItemsPerPush, mail.go's mailListRawItemCap, alliance.go's
//     allianceScienceRawItemCap, ...) bound a single item's own internal subtree size, only the
//     OUTER array's item count. So a single call to String() on one such item could still walk up
//     to ~300,000 nodes.
//  2. conn.go's logCommandResult calls msg.Params.String()/.StringRedacted() unconditionally on
//     EVERY command response, as an eager Go function-call argument to slog -- executed regardless
//     of the configured log level (slog only skips emitting the log LINE; it does not skip
//     evaluating arguments already passed to it).
//
// Both mean an already-decoded SFSObject/SFSArray, however large, can reach a String()/
// StringRedacted() call with no format-time cost bound of its own. This budget is deliberately
// independent of maxDecodedNodes -- it exists purely to keep the cost of ONE format call bounded,
// including for an object built programmatically via Put*/Add* (which has no decode-time bound
// applied to it at all, since maxDecodedNodes/chargeNodes only run inside DecodeObject's read
// path).
//
// Round 29 addition: the two gaps above only motivated bounding the total number of KEYS/ITEMS
// walked -- they didn't account for a single key/item's own VALUE being unboundedly large, if that
// value is a bare string/sfsText field or one of the 8 primitive-array types (as opposed to a nested
// SFSObject/SFSArray, whose own internal keys/items were already correctly charged one unit each).
// formatSFSValueRedacted's default case now charges additional budget proportional to such a
// value's own real size (chargeUpTo/primitiveArrayPrefix, sfsobject.go), so a single huge string or
// primitive-array field can't exempt itself from this same budget.
const maxFormattedNodes = 50_000

// formatTruncatedMarker is appended to StringRedacted()'s output, in place of the remaining
// keys/items, once a single top-level call's maxFormattedNodes budget runs out -- an explicit,
// visible marker rather than silently dropping the rest of the data, matching this file's existing
// fail-safe conventions (e.g. redactSFSValue's "[REDACTED N items]"/"[REDACTED N fields]" shapes).
const formatTruncatedMarker = "...[truncated: exceeded maxFormattedNodes format budget]"

// formatBudget tracks the remaining format-time node budget across one top-level StringRedacted()
// call's full recursive descent through nested SFSObject/SFSArray values. It is a single counter
// threaded through every recursive call reached from that one top-level call
// (stringRedactedBudgeted below and formatSFSValueRedacted), deliberately NOT reset at each nested
// level: an object made of many small nested objects/arrays (rather than one large flat one) must
// still cost no more in total than maxFormattedNodes, or the budget wouldn't actually bound the
// cost of the call as a whole.
type formatBudget struct {
	remaining int
	// truncated becomes true the first time ANY nesting level (stringRedactedBudgeted's key loop,
	// formatSFSValueRedacted's *SFSArray item loop, or formatSFSValueRedacted's default case for an
	// oversized primitive array/string -- see chargeUpTo/noteTruncation below) notices the shared
	// budget is exhausted and appends formatTruncatedMarker to its own output.
	//
	// Round 29 fix: without this flag, EVERY still-in-progress enclosing nesting level independently
	// re-checks charge() (still false, per its own doc comment) once the budget is exhausted and
	// appends its OWN formatTruncatedMarker as the call stack unwinds -- so a single StringRedacted()
	// output could contain multiple redundant truncation markers, one per enclosing level that was
	// still mid-loop at the moment exhaustion was first noticed, instead of just one. noteTruncation()
	// below is the single choke point that lets only the FIRST caller to notice actually append the
	// marker; every later caller, at any nesting level, sees truncated already true and stays silent.
	truncated bool
}

func newFormatBudget() *formatBudget {
	return &formatBudget{remaining: maxFormattedNodes}
}

// charge consumes one unit of budget for one key/item about to be formatted, and reports whether
// budget remains. Once exhausted, every subsequent charge() call keeps returning false (remaining
// is never decremented below the point it first hit zero), so a caller doesn't need to special-case
// "already truncated" separately from "just ran out".
func (fb *formatBudget) charge() bool {
	if fb.remaining <= 0 {
		return false
	}
	fb.remaining--
	return true
}

// chargeUpTo consumes up to n units of budget for a SINGLE value whose real formatting cost is
// proportional to n (a primitive array's element count, or a string's byte length) -- unlike
// charge(), which always spends exactly one unit per key/item, this lets formatSFSValueRedacted's
// default case (see its doc comment) charge proportional to a bare string/primitive-array value's
// actual size, rather than the flat single unit its caller already paid just for the field/item
// existing at all.
//
// Returns allowed (how many of the n units actually fit, 0 <= allowed <= n) and ranOut (whether the
// budget ran out before allowed reached n) -- a caller uses allowed to format only a size-bounded
// prefix of the value when ranOut is true, instead of either formatting the whole (potentially huge)
// value regardless of budget, or refusing to format any of it.
func (fb *formatBudget) chargeUpTo(n int) (allowed int, ranOut bool) {
	if n <= 0 {
		return 0, false
	}
	if fb.remaining <= 0 {
		return 0, true
	}
	if n <= fb.remaining {
		fb.remaining -= n
		return n, false
	}
	allowed = fb.remaining
	fb.remaining = 0
	return allowed, true
}

// noteTruncation reports whether THIS call is the first, across the whole shared budget's recursive
// descent, to notice the budget is exhausted -- see the truncated field's doc comment above for why
// this exists. A caller that just observed exhaustion (charge() returned false, or chargeUpTo
// reported ranOut) should append formatTruncatedMarker to its own output only when this returns
// true; every subsequent call (from this or any other nesting level) returns false and must NOT
// append a second marker.
func (fb *formatBudget) noteTruncation() bool {
	if fb.truncated {
		return false
	}
	fb.truncated = true
	return true
}

// StringRedacted is *SFSObject's safe-to-log dump (and, since String()/GoString() delegate to this
// method, is the real implementation behind both too): a decoded server response or outgoing
// request can carry a live loginKey/accessToken/airKey/shumeiBoxId in cleartext (this protocol has
// no separate "credentials" envelope -- they're ordinary fields mixed in with gameplay data).
// StringRedacted walks o's fields and masks any key matching sensitiveSFSKeys (checked
// case-insensitively via isSensitiveSFSKey -- see its doc comment), recursing into nested
// SFSObject/SFSArray values via formatSFSValueRedacted instead of printing its value, so a call
// site that wants to log/error-wrap a full decoded object for debugging can do so without risking a
// credential leak.
//
// A nil receiver (e.g. from a hypothetical future PutSFSObject(key, nil) call reached recursively,
// or a bare nil *SFSObject handed straight to fmt) returns the safe literal "<nil>" instead of
// dereferencing o.keys/o.values and panicking.
//
// Each top-level call gets its own fresh maxFormattedNodes budget (see formatBudget's doc comment
// for why that budget must then stay shared, not reset, across the whole recursive descent this
// call kicks off) -- so this call's own cost is bounded regardless of how large or deeply nested o
// (or anything reachable from it) turns out to be.
func (o *SFSObject) StringRedacted() string {
	if o == nil {
		return "<nil>"
	}
	return o.stringRedactedBudgeted(newFormatBudget())
}

// stringRedactedBudgeted is StringRedacted's real implementation, parameterized over a formatBudget
// shared across the whole recursive descent from one top-level StringRedacted()/String()/GoString()
// call. formatSFSValueRedacted's *SFSObject case calls back into this (not the public
// StringRedacted()) for exactly this reason: StringRedacted() itself always allocates a brand-new
// budget, which would let a nested object reset the counter instead of continuing to spend down the
// same one.
func (o *SFSObject) stringRedactedBudgeted(fb *formatBudget) string {
	if o == nil {
		return "<nil>"
	}
	var b bytes.Buffer
	b.WriteString("{")
	wrote := false
	for _, k := range o.keys {
		if !fb.charge() {
			// noteTruncation: only the FIRST nesting level to notice exhaustion appends the marker --
			// see formatBudget.truncated's doc comment (round 29 fix).
			if fb.noteTruncation() {
				if wrote {
					b.WriteString(", ")
				}
				b.WriteString(formatTruncatedMarker)
			}
			break
		}
		if wrote {
			b.WriteString(", ")
		}
		v := o.values[k]
		// The key name itself is server-controlled (readUtfString decodes it straight off the
		// wire, same as any string value), so it goes through sanitizeForTerminal too -- not just
		// the value -- before being embedded in the output. See sanitizeForTerminal's doc comment.
		safeKey := sanitizeForTerminal(k)
		if isSensitiveSFSKey(k) {
			fmt.Fprintf(&b, "%s=%s", safeKey, redactSFSValue(v))
		} else {
			fmt.Fprintf(&b, "%s=%s", safeKey, formatSFSValueRedacted(v, fb))
		}
		wrote = true
	}
	b.WriteString("}")
	return b.String()
}

// sanitizeForTerminal makes s safe to write to a terminal by escaping every C0 control byte
// (0x00-0x1F) other than a plain newline/tab, plus DEL (0x7F), as a visible "\xHH" sequence
// instead of passing it through raw.
//
// This protocol has no separate "trusted" vs "untrusted" string channel -- every decoded field
// value (and, per readUtfString/readValuePayload's sfsObjectType case, every decoded field KEY
// too) is either produced by DecodeObject from bytes a live server or a captured/replayed capture
// file supplied, both of which are adversarial from this client's point of view. Two real call
// sites write StringRedacted()'s output straight to a terminal with zero escaping of their own
// (decode.go's -decode-stream tool, directly reachable from a crafted capture file, and
// buildings.go's PrintBuildings, hit during ordinary -collect/-list-buildings runs against a live
// server) -- without this, a malicious server/capture file could embed a raw ESC (0x1b) followed
// by a CSI/OSC sequence in any decoded string and spoof terminal output on the operator's screen
// (fake error text, title-bar spoofing, cursor manipulation) the moment that value flows into
// StringRedacted()'s output. Every slog.* call site is already safe (main.go's JSON handler
// escapes control bytes through encoding/json), so this is scoped to this file's own
// formatting/redaction output construction rather than duplicated at each terminal-writing call
// site, closing the gap for every current AND future consumer of StringRedacted() at once.
//
// Deliberately byte-oriented rather than rune-oriented: a raw ESC byte is always 0x1b regardless
// of what UTF-8 sequence surrounds it (and readUtfString's `string(b)` conversion doesn't
// guarantee b is even valid UTF-8 to begin with, since it comes straight off the wire), so
// scanning byte-by-byte catches it without needing to decode runes first -- and every ordinary
// multi-byte UTF-8 sequence's bytes are all >= 0x80, so they pass through this scan untouched.
// Newline and tab are deliberately left alone so ordinary multi-line/tab-formatted string values
// stay human-readable; every other C0 control code (including BEL 0x07 and ESC 0x1b) and DEL are
// escaped.
func sanitizeForTerminal(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 0x20 && c != '\n' && c != '\t') || c == 0x7f {
			fmt.Fprintf(&b, "\\x%02x", c)
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// formatSFSValueRedacted recurses into nested SFSObject/SFSArray values (rather than printing
// their Go pointer) so StringRedacted's output is actually useful for inspecting arrays-of-objects
// like `accountArr`/`defaultBuilds`, while staying redacted at every level via *SFSObject's own
// stringRedactedBudgeted logic for nested objects (each carries its own keys, so recursing into it
// correctly re-applies the sensitiveSFSKeys check at that level).
//
// fb is the calling top-level StringRedacted() call's shared formatBudget (see its doc comment) --
// threaded through every recursive call this function makes, including into the nested *SFSObject
// case below, so a single top-level call's total formatting cost stays bounded by maxFormattedNodes
// regardless of how the nodes are distributed across nesting levels.
//
// The *SFSArray case is deliberately NOT delegated to (*SFSArray).StringRedacted() -- that method
// is the bare/standalone entry point (used when an *SFSArray is reached with no enclosing key at
// all, e.g. a future call site that extracts and logs an array value directly) and blanket-masks
// every item defensively for exactly that reason. An array reached HERE, though, is always a value
// already sitting under a specific key inside a parent SFSObject whose StringRedacted() has
// already checked that key against sensitiveSFSKeys (a sensitive key's array value never reaches
// this function at all -- SFSObject.StringRedacted() routes those through redactSFSValue's own
// *SFSArray case instead, which blanket-masks). So an array reached here is known-non-sensitive by
// construction, and recursing item-by-item (rather than blanket-masking) is what keeps
// StringRedacted's output actually useful for inspecting ordinary gameplay arrays-of-objects like
// `accountArr`/`defaultBuilds`/`buildingList` -- blanket-masking here would make those always print
// "[REDACTED N items]" regardless of content, gutting the tool for the overwhelming common case.
//
// A nil *SFSObject/*SFSArray is handled explicitly here, ahead of the delegated/recursive call, as
// defense-in-depth alongside StringRedacted()'s own nil guard on each type -- neither path
// dereferences a nil pointer.
func formatSFSValueRedacted(v SFSValue, fb *formatBudget) string {
	switch val := v.Val.(type) {
	case *SFSObject:
		if val == nil {
			return "<nil>"
		}
		return val.stringRedactedBudgeted(fb)
	case *SFSArray:
		if val == nil {
			return "<nil>"
		}
		var b bytes.Buffer
		b.WriteString("[")
		wrote := false
		for _, item := range val.items {
			if !fb.charge() {
				// noteTruncation: only the FIRST nesting level to notice exhaustion appends the
				// marker -- see formatBudget.truncated's doc comment (round 29 fix).
				if fb.noteTruncation() {
					if wrote {
						b.WriteString(", ")
					}
					b.WriteString(formatTruncatedMarker)
				}
				break
			}
			if wrote {
				b.WriteString(", ")
			}
			b.WriteString(formatSFSValueRedacted(item, fb))
			wrote = true
		}
		b.WriteString("]")
		return b.String()
	default:
		// val may be a bare string/sfsText value, one of the 8 primitive-array types
		// (readValuePayload's array-tag cases) whose elements can themselves be server-controlled
		// strings ([]string), or any other plain scalar (int32/int64/int16/byte/float32/float64/
		// bool/nil).
		//
		// Round 29 fix: the first two shapes can be arbitrarily large -- a decoded string's byte
		// length or a primitive array's element count is bounded only by maxDecodedNodes (the
		// DECODE-time budget for the whole payload), not by this format-time budget -- yet this case
		// used to charge a flat ONE formatBudget unit for the entire value regardless of its real
		// size (e.g. a 40,000-element string array cost the same single unit as an empty one), so
		// maxFormattedNodes didn't actually bound a single StringRedacted() call's real formatting
		// cost for either shape, only for nested SFSObject/SFSArray structures (whose own per-key/
		// per-item charge() calls in stringRedactedBudgeted/the *SFSArray case above already scale
		// correctly). The caller (stringRedactedBudgeted's key loop, or the *SFSArray item loop
		// above) already charged exactly one unit for "this field/item exists at all" before ever
		// reaching this function -- chargeUpTo below charges ADDITIONAL budget proportional to the
		// value's actual size on top of that, and truncates the formatted output (via
		// primitiveArrayPrefix, or a direct byte-slice for a string) to whatever fits when it
		// doesn't. Every other scalar type here is intrinsically O(1) to format, so it keeps the
		// flat, already-paid 1-unit cost with no extra charge.
		if n, ok := primitiveArrayLen(val); ok {
			allowed, ranOut := fb.chargeUpTo(n)
			out := sanitizeForTerminal(primitiveArrayPrefix(val, allowed))
			if ranOut && fb.noteTruncation() {
				out += " " + formatTruncatedMarker
			}
			return out
		}
		if s, ok := val.(string); ok {
			allowed, ranOut := fb.chargeUpTo(len(s))
			out := sanitizeForTerminal(s[:allowed])
			if ranOut && fb.noteTruncation() {
				out += " " + formatTruncatedMarker
			}
			return out
		}
		return sanitizeForTerminal(fmt.Sprintf("%v", val))
	}
}

// redactSFSValue masks a sensitive-keyed field's value. It is FAIL-CLOSED by design: every shape
// it doesn't explicitly recognize as safe falls through to a fixed "[REDACTED]" placeholder rather
// than to any formatter that might print real content, so a future SFSValue shape this repo adds
// (or a value shape this function's author didn't anticipate for a given key) is masked by default
// instead of leaking.
//
// Every known sensitive key (sensitiveSFSKeys) carries a plain string on the wire in every case
// this repo has decoded; a non-string value under one of those keys would be unexpected, but is
// still masked -- this closes the gap where a sensitive key's value reached via PutInt/PutLong/
// PutBool/PutDouble/PutByte/PutShort (any scalar Go type other than string) used to fall through
// to the final fallback, which used to be formatSFSValueRedacted(v) -- the ORDINARY, non-redacting
// recursive formatter, whose own default case is the naive fmt.Sprintf("%v", val) -- printing the
// raw scalar in full cleartext.
//
// A primitive array (one of the 8 types readValuePayload's array-tag cases decode into --
// []bool/[]byte/[]int16/[]int32/[]int64/[]float32/[]float64/[]string) still gets masked explicitly
// below, since formatSFSValueRedacted's fallback for those types is the same naive
// fmt.Sprintf("%v", val) String() uses -- printing the raw slice contents with no redaction at all.
//
// An *SFSArray (the wrapper type sfsArrayType decodes into, as opposed to a primitive array) also
// gets masked explicitly below, for the same reason one level deeper: formatSFSValueRedacted's own
// *SFSArray case recurses via formatSFSValueRedacted (not redactSFSValue) on each item, so a raw
// scalar item inside the array would lose the "sensitive" context and print via the naive
// fmt.Sprintf("%v", val) default -- no current PutSFSArray call site puts a sensitive key's value
// in an *SFSArray of scalars, but a future decoded server response could represent a sensitive
// field that way. A nil *SFSArray (e.g. PutSFSArray(sensitiveKey, (*SFSArray)(nil))) is checked
// explicitly too: the type assertion below succeeds (ok=true) for a nil pointer of the right
// dynamic type, so without this check, `arr.items` would dereference a nil pointer and panic.
//
// A *SFSObject also gets masked explicitly below -- blanket, by field count, mirroring the
// *SFSArray case's style exactly -- rather than delegating to formatSFSValueRedacted, whose own
// *SFSObject case calls the NESTED object's own StringRedacted(). That would only re-check the
// nested object's OWN key names against sensitiveSFSKeys, completely losing the fact that the
// OUTER key was already known-sensitive -- so a secret sitting under an ordinary-looking sub-key
// name inside that nested object (e.g. {loginKey: {value: "the-real-secret"}}) would print in
// full. A nil *SFSObject is checked explicitly for the same nil-pointer-panic reason as *SFSArray
// above.
func redactSFSValue(v SFSValue) string {
	if s, ok := v.Val.(string); ok {
		// redact() (login.go) only shortens s to a first4...last4 shape -- it doesn't strip
		// control bytes, so a secret whose first/last 4 bytes happen to contain a raw ESC/BEL
		// (e.g. an attacker padding a crafted "loginKey" value specifically to smuggle one into
		// the visible slice) would still reach the terminal unescaped without this. See
		// sanitizeForTerminal's doc comment.
		return sanitizeForTerminal(redact(s))
	}
	if n, ok := primitiveArrayLen(v.Val); ok {
		return fmt.Sprintf("[REDACTED %d items]", n)
	}
	if arr, ok := v.Val.(*SFSArray); ok {
		if arr == nil {
			return "<nil>"
		}
		return fmt.Sprintf("[REDACTED %d items]", len(arr.items))
	}
	if obj, ok := v.Val.(*SFSObject); ok {
		if obj == nil {
			return "<nil>"
		}
		return fmt.Sprintf("[REDACTED %d fields]", len(obj.keys))
	}
	// Fail-closed fallback: any value shape under a sensitive key that isn't one of the
	// explicitly-recognized-safe forms above is masked by a fixed placeholder, rather than falling
	// through to formatSFSValueRedacted -- a formatter that doesn't know it's operating inside a
	// sensitive context and will happily print raw content (see the doc comment above).
	return "[REDACTED]"
}

// primitiveArrayLen reports the length of val and true if val is one of the 8 primitive array
// types readValuePayload's array-tag cases (sfsBoolArray..sfsUtfStringArray) decode into -- plain
// unwrapped Go slices, as opposed to the *SFSArray wrapper type. Used by redactSFSValue to mask a
// sensitive key's array value without dumping its raw contents.
func primitiveArrayLen(val interface{}) (int, bool) {
	switch a := val.(type) {
	case []bool:
		return len(a), true
	case []byte:
		return len(a), true
	case []int16:
		return len(a), true
	case []int32:
		return len(a), true
	case []int64:
		return len(a), true
	case []float32:
		return len(a), true
	case []float64:
		return len(a), true
	case []string:
		return len(a), true
	}
	return 0, false
}

// primitiveArrayPrefix formats val's first n elements the same way Go's default fmt.Sprintf("%v",
// val) would format the WHOLE slice (e.g. "[1 2 3]"), for a primitive array value (one of the 8
// types primitiveArrayLen recognizes). Used by formatSFSValueRedacted's default case (round 29 fix)
// to format only as many elements as fit within the remaining formatBudget when the array's real
// size doesn't fit in full -- n is always <= the value's real length (formatSFSValueRedacted only
// ever passes chargeUpTo's own `allowed` return, which is capped at the value's real length), so the
// slice expression below can never panic.
func primitiveArrayPrefix(val interface{}, n int) string {
	switch a := val.(type) {
	case []bool:
		return fmt.Sprintf("%v", a[:n])
	case []byte:
		return fmt.Sprintf("%v", a[:n])
	case []int16:
		return fmt.Sprintf("%v", a[:n])
	case []int32:
		return fmt.Sprintf("%v", a[:n])
	case []int64:
		return fmt.Sprintf("%v", a[:n])
	case []float32:
		return fmt.Sprintf("%v", a[:n])
	case []float64:
		return fmt.Sprintf("%v", a[:n])
	case []string:
		return fmt.Sprintf("%v", a[:n])
	}
	return ""
}

// SFSArray is a sequential list of tagged values.
type SFSArray struct {
	items []SFSValue
}

func NewSFSArray() *SFSArray { return &SFSArray{} }

func (a *SFSArray) add(v SFSValue)              { a.items = append(a.items, v) }
func (a *SFSArray) AddInt(val int32)            { a.add(SFSValue{sfsInt, val}) }
func (a *SFSArray) AddSFSObject(val *SFSObject) { a.add(SFSValue{sfsObjectType, val}) }

// String makes *SFSArray satisfy fmt.Stringer safely, mirroring SFSObject.String(): it delegates
// to StringRedacted() rather than printing raw item contents, so handing a bare *SFSArray (one not
// wrapped in a parent SFSObject -- every current call site instead ranges over .items directly, so
// this was a latent rather than actively exploited gap) to fmt's %v/%s verbs, a Print-family
// function, or slog's Any-kind attribute formatting can't leak a sensitive item in cleartext.
func (a *SFSArray) String() string {
	return a.StringRedacted()
}

// GoString makes *SFSArray satisfy fmt.GoStringer, mirroring SFSObject.GoString(): without this,
// %#v on a bare *SFSArray falls through to Go's default reflection-based formatter, dumping its
// unexported items slice raw.
func (a *SFSArray) GoString() string {
	return a.StringRedacted()
}

// StringRedacted is *SFSArray's safe-to-log dump for the bare/standalone case -- called directly
// on an *SFSArray with no enclosing SFSObject key to check against sensitiveSFSKeys (String()/
// GoString() above both delegate here). Unlike formatSFSValueRedacted's *SFSArray case (used when
// an array is reached as a value already sitting under a specific, already-checked key inside a
// parent object), this method has no key context at all to lean on -- a caller could be logging an
// array extracted from anywhere, including a sensitive field's value. With no way to tell, it
// blanket-masks every item unconditionally, the same conservative "[REDACTED N items]" shape
// redactSFSValue already uses for a sensitive key's *SFSArray value, rather than risk printing an
// item that turns out to be sensitive.
//
// A nil receiver returns the safe literal "<nil>" instead of dereferencing a.items and panicking.
func (a *SFSArray) StringRedacted() string {
	if a == nil {
		return "<nil>"
	}
	return fmt.Sprintf("[REDACTED %d items]", len(a.items))
}

// ---- Encoding ----

// EncodeObject serializes a top-level SFSObject to its self-describing wire
// form: tag(18) + i16 key count + per-key (UTF_STRING key + typed value).
// Returns an error (rather than panicking) if any key/string/collection
// along the way is too large to represent on the wire -- see int16Count and
// writeUtfString.
//
// A nil o returns a clean error instead of panicking on the o.keys dereference below --
// mirroring this file's existing nil-guard hardening on writeValuePayload's nested
// sfsObjectType/sfsArrayType cases and on StringRedacted/formatSFSValueRedacted/redactSFSValue,
// all of which handle a nil *SFSObject/*SFSArray gracefully rather than crashing the process.
func EncodeObject(o *SFSObject) ([]byte, error) {
	if o == nil {
		return nil, fmt.Errorf("sfsobject: cannot encode a nil *SFSObject")
	}
	var buf bytes.Buffer
	buf.WriteByte(sfsObjectType)
	n, err := int16Count(len(o.keys), "keys")
	if err != nil {
		return nil, err
	}
	writeInt16(&buf, n)
	for _, k := range o.keys {
		if err := writeUtfString(&buf, k); err != nil {
			return nil, err
		}
		v := o.values[k]
		if err := writeTaggedValue(&buf, v); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func writeTaggedValue(buf *bytes.Buffer, v SFSValue) error {
	buf.WriteByte(v.Type)
	return writeValuePayload(buf, v)
}

func writeValuePayload(buf *bytes.Buffer, v SFSValue) error {
	switch v.Type {
	case sfsNull:
		// no payload
		return nil
	case sfsBool:
		if v.Val.(bool) {
			buf.WriteByte(1)
		} else {
			buf.WriteByte(0)
		}
		return nil
	case sfsByte:
		buf.WriteByte(v.Val.(byte))
		return nil
	case sfsShort:
		writeInt16(buf, v.Val.(int16))
		return nil
	case sfsInt:
		writeInt32(buf, v.Val.(int32))
		return nil
	case sfsLong:
		writeInt64(buf, v.Val.(int64))
		return nil
	case sfsFloat:
		binary.Write(buf, binary.BigEndian, math.Float32bits(v.Val.(float32)))
		return nil
	case sfsDouble:
		binary.Write(buf, binary.BigEndian, math.Float64bits(v.Val.(float64)))
		return nil
	case sfsUtfString:
		return writeUtfString(buf, v.Val.(string))
	case sfsText:
		// Same underlying representation as sfsUtfString (a Go string), but tagged sfsText on the
		// wire and length-prefixed with a 4-byte count instead of 2 (mirrors readValuePayload's
		// sfsText case).
		b := []byte(v.Val.(string))
		n, err := int32Count(len(b), "text bytes")
		if err != nil {
			return err
		}
		writeInt32(buf, n)
		buf.Write(b)
		return nil
	case sfsBoolArray:
		arr := v.Val.([]bool)
		n, err := int16Count(len(arr), "bool array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			if e {
				buf.WriteByte(1)
			} else {
				buf.WriteByte(0)
			}
		}
		return nil
	case sfsByteArray:
		// Unlike every other array type (which use a 2-byte count), ByteArray uses a bare 4-byte
		// int count (mirrors readValuePayload's sfsByteArray case -- see the comment there).
		b := v.Val.([]byte)
		n, err := int32Count(len(b), "byte array items")
		if err != nil {
			return err
		}
		writeInt32(buf, n)
		buf.Write(b)
		return nil
	case sfsShortArray:
		arr := v.Val.([]int16)
		n, err := int16Count(len(arr), "short array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt16(buf, e)
		}
		return nil
	case sfsIntArray:
		arr := v.Val.([]int32)
		n, err := int16Count(len(arr), "int array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt32(buf, e)
		}
		return nil
	case sfsLongArray:
		arr := v.Val.([]int64)
		n, err := int16Count(len(arr), "long array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			writeInt64(buf, e)
		}
		return nil
	case sfsFloatArray:
		arr := v.Val.([]float32)
		n, err := int16Count(len(arr), "float array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			binary.Write(buf, binary.BigEndian, math.Float32bits(e))
		}
		return nil
	case sfsDoubleArray:
		arr := v.Val.([]float64)
		n, err := int16Count(len(arr), "double array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, e := range arr {
			binary.Write(buf, binary.BigEndian, math.Float64bits(e))
		}
		return nil
	case sfsUtfStringArray:
		arr := v.Val.([]string)
		n, err := int16Count(len(arr), "utf string array items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, s := range arr {
			if err := writeUtfString(buf, s); err != nil {
				return err
			}
		}
		return nil
	case sfsObjectType:
		inner := v.Val.(*SFSObject)
		if inner == nil {
			// Mirrors round 15's decode/format-side nil guard (formatSFSValueRedacted's *SFSObject
			// case): the type assertion above succeeds (ok=true) for a nil pointer of the right
			// dynamic type, so without this check, `inner.keys` below would dereference a nil
			// pointer and panic. No current call site passes PutSFSObject(key, nil) -- this is
			// latent, defense-in-depth for a future mistake -- but writeTaggedValue/writeValuePayload
			// have no key name in scope at this point (only the value itself), so the error can't
			// name the offending key.
			return fmt.Errorf("sfsobject: nil *SFSObject value (sfsObjectType) cannot be encoded")
		}
		n, err := int16Count(len(inner.keys), "keys")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, k := range inner.keys {
			if err := writeUtfString(buf, k); err != nil {
				return err
			}
			if err := writeTaggedValue(buf, inner.values[k]); err != nil {
				return err
			}
		}
		return nil
	case sfsArrayType:
		inner := v.Val.(*SFSArray)
		if inner == nil {
			// Mirrors the sfsObjectType nil guard immediately above -- same nil-pointer-panic gap,
			// same reasoning, same "no key name in scope at this point" caveat.
			return fmt.Errorf("sfsobject: nil *SFSArray value (sfsArrayType) cannot be encoded")
		}
		n, err := int16Count(len(inner.items), "items")
		if err != nil {
			return err
		}
		writeInt16(buf, n)
		for _, iv := range inner.items {
			if err := writeTaggedValue(buf, iv); err != nil {
				return err
			}
		}
		return nil
	default:
		// Every SFSDataType tag decode supports has an encode case above, except sfsClass (19),
		// which is unused/unimplemented by the game itself (see the const block) and so has never
		// had a decode case to mirror either. Every other case here can only be reached via
		// programmatically-constructed SFSValues (Put*/Add* helpers all set a valid Type), so an
		// unsupported tag here means a genuine programmer/decode-desync bug, not untrusted server
		// data -- unlike the two encode-time size limits below, this one stays a panic.
		panic(fmt.Sprintf("sfsobject: unsupported encode type %d", v.Type))
	}
}

func writeInt16(buf *bytes.Buffer, v int16) { binary.Write(buf, binary.BigEndian, v) }
func writeInt32(buf *bytes.Buffer, v int32) { binary.Write(buf, binary.BigEndian, v) }
func writeInt64(buf *bytes.Buffer, v int64) { binary.Write(buf, binary.BigEndian, v) }

// int16Count converts a length to int16 for a wire count field, returning an error instead of
// silently wrapping into a wrong count -- or panicking, as this used to -- if the value is ever
// too large to represent. Reachable from server-controlled data (e.g. a collection built up from
// a paginated server response), so it must not crash the process.
func int16Count(n int, what string) (int16, error) {
	if n > 32767 {
		return 0, fmt.Errorf("sfsobject: too many %s to encode (%d, max 32767)", what, n)
	}
	return int16(n), nil
}

// int32Count converts a length to int32 for a wire count field, returning an error instead of
// silently wrapping into a wrong count if the value is ever too large to represent. Reachable
// from server-controlled data, so it must not crash the process.
func int32Count(n int, what string) (int32, error) {
	if n > math.MaxInt32 {
		return 0, fmt.Errorf("sfsobject: too many %s to encode (%d, max %d)", what, n, math.MaxInt32)
	}
	return int32(n), nil
}

// writeUtfString returns an error instead of panicking when s is too long to length-prefix with
// a 2-byte count -- reachable from server-controlled data (e.g. a batched join of server-supplied
// values), so it must not crash the process.
func writeUtfString(buf *bytes.Buffer, s string) error {
	b := []byte(s)
	if len(b) > 65535 {
		return fmt.Errorf("sfsobject: string too long to encode (%d bytes, max 65535)", len(b))
	}
	writeUint16(buf, uint16(len(b)))
	buf.Write(b)
	return nil
}
func writeUint16(buf *bytes.Buffer, v uint16) { binary.Write(buf, binary.BigEndian, v) }

// ---- Decoding ----

type sfsReader struct {
	data  []byte
	pos   int
	depth int
	nodes int
}

// maxNestDepth bounds how many levels of nested SFSArray/SFSObject readValuePayload will
// recurse into before returning a decode error instead of continuing -- real SFS2X payloads
// from this game have never needed anywhere close to this, and unbounded recursion here is a
// crash-the-process vector on a payload well under the existing frame-size cap.
const maxNestDepth = 64

// maxDecodedNodes bounds the total number of values a single decode may produce, independent of
// nesting depth or per-level fan-out -- an ordinary few-level-deep, wide-fan-out nested
// array/object can decode into an enormous number of total leaf nodes even while staying well
// within maxNestDepth and the wire-level maxFrameSize cap (a measured ~60MB wire payload
// decoding into multiple GB of heap via ordinary 3-level nesting). Chosen comfortably above
// anything the real ~313KB init payload has ever needed.
const maxDecodedNodes = 300_000

// chargeNodes adds n to r.nodes and errors if the running total exceeds maxDecodedNodes -- the
// same budget check readValuePayload already applies once per value via r.nodes++, but a
// primitive array (sfsBoolArray..sfsUtfStringArray) decodes its up-to-32767 elements directly via
// readByte/readInt16/etc. rather than recursively calling readValuePayload per element like the
// container types (sfsArrayType/sfsObjectType) do, so without this it would only ever cost 1
// toward the budget regardless of how many elements it actually contains -- letting many
// primitive-array fields, each cheap on the wire, amplify into a Go heap far larger than the
// wire-frame cap was meant to bound. sfsByteArray is the one exception among the 8 primitive-array
// types: it deliberately does NOT call this (see the comment on that case) since a Go []byte's
// memory cost is already a tight ~1:1 ratio with its wire cost, unlike the other 7 shapes.
func (r *sfsReader) chargeNodes(n int) error {
	r.nodes += n
	if r.nodes > maxDecodedNodes {
		return fmt.Errorf("sfsobject: decoded node count exceeds %d", maxDecodedNodes)
	}
	return nil
}

func (r *sfsReader) remaining() int { return len(r.data) - r.pos }

func (r *sfsReader) readByte() (byte, error) {
	if r.remaining() < 1 {
		return 0, fmt.Errorf("sfsobject: unexpected EOF reading byte")
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *sfsReader) readBytes(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("sfsobject: negative byte count: %d", n)
	}
	if r.remaining() < n {
		return nil, fmt.Errorf("sfsobject: unexpected EOF reading %d bytes (have %d)", n, r.remaining())
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

// readArrayCount reads a 2-byte element count for a fixed-element-type array
// and rejects a negative value (a flipped top bit in a corrupted or hostile
// packet) instead of letting it flow into make() and panic the process.
func (r *sfsReader) readArrayCount() (int16, error) {
	n, err := r.readInt16()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("sfsobject: array negative size: %d", n)
	}
	return n, nil
}

func (r *sfsReader) readInt16() (int16, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return 0, err
	}
	return int16(binary.BigEndian.Uint16(b)), nil
}
func (r *sfsReader) readUint16() (uint16, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return 0, err
	}
	return binary.BigEndian.Uint16(b), nil
}
func (r *sfsReader) readInt32() (int32, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return 0, err
	}
	return int32(binary.BigEndian.Uint32(b)), nil
}
func (r *sfsReader) readInt64() (int64, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b)), nil
}

func (r *sfsReader) readUtfString() (string, error) {
	n, err := r.readUint16()
	if err != nil {
		return "", err
	}
	b, err := r.readBytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *sfsReader) readTaggedValue() (SFSValue, error) {
	tag, err := r.readByte()
	if err != nil {
		return SFSValue{}, err
	}
	return r.readValuePayload(tag)
}

func (r *sfsReader) readValuePayload(tag byte) (SFSValue, error) {
	// Count every decoded value, not just containers -- leaf-node count is what actually
	// drives heap amplification for a wide-fan-out nested array/object (see maxDecodedNodes).
	r.nodes++
	if r.nodes > maxDecodedNodes {
		return SFSValue{}, fmt.Errorf("sfsobject: decoded node count exceeds %d", maxDecodedNodes)
	}
	switch tag {
	case sfsNull:
		return SFSValue{tag, nil}, nil
	case sfsBool:
		b, err := r.readByte()
		return SFSValue{tag, b != 0}, err
	case sfsByte:
		b, err := r.readByte()
		return SFSValue{tag, b}, err
	case sfsShort:
		v, err := r.readInt16()
		return SFSValue{tag, v}, err
	case sfsInt:
		v, err := r.readInt32()
		return SFSValue{tag, v}, err
	case sfsLong:
		v, err := r.readInt64()
		return SFSValue{tag, v}, err
	case sfsFloat:
		b, err := r.readBytes(4)
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, math.Float32frombits(binary.BigEndian.Uint32(b))}, nil
	case sfsDouble:
		b, err := r.readBytes(8)
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, math.Float64frombits(binary.BigEndian.Uint64(b))}, nil
	case sfsUtfString:
		s, err := r.readUtfString()
		return SFSValue{tag, s}, err
	case sfsText:
		n, err := r.readInt32()
		if err != nil {
			return SFSValue{}, err
		}
		if n < 0 {
			return SFSValue{}, fmt.Errorf("sfsobject: text negative size: %d", n)
		}
		b, err := r.readBytes(int(n))
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, string(b)}, nil
	case sfsBoolArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]bool, n)
		for i := range out {
			b, err := r.readByte()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = b != 0
		}
		return SFSValue{tag, out}, nil
	case sfsByteArray:
		// Unlike every other array type (which use a 2-byte GetTypedArraySize
		// count), ByteArray uses a bare 4-byte int count
		// (BinDecode_BYTE_ARRAY, Smartfox2xLw.decompiled.cs:7230-7238) --
		// confirmed the hard way: this 2-byte read silently misaligned
		// every subsequent field whenever a real byte-array value showed
		// up, which never happened in any packet small enough to hand-
		// inspect before the ~313KB init payload from a live capture.
		n, err := r.readInt32()
		if err != nil {
			return SFSValue{}, err
		}
		if n < 0 {
			return SFSValue{}, fmt.Errorf("sfsobject: byte array negative size: %d", n)
		}
		// No chargeNodes call here, unlike every other primitive-array case below: a Go []byte of n
		// elements occupies exactly n bytes plus one O(1) slice header (no per-element header
		// overhead, unlike e.g. []string's ~16-byte-per-element Go string headers), and the wire
		// encoding of a byte array is also exactly n bytes plus a small fixed header -- so wire-size
		// cost and Go-memory cost are already a tight ~1:1 ratio with no amplification.
		// maxFrameSize's existing wire-size cap already bounds Go-memory size for byte arrays with
		// no separate node-budget protection needed, exactly like sfsText (same 1:1 shape, no
		// chargeNodes call either) already correctly assumes. Charging here doesn't add real
		// protection; it just makes a legitimate multi-hundred-KB/multi-MB byte-array field fail
		// spuriously against the flat maxDecodedNodes budget.
		b, err := r.readBytes(int(n))
		if err != nil {
			return SFSValue{}, err
		}
		return SFSValue{tag, append([]byte(nil), b...)}, nil
	case sfsShortArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]int16, n)
		for i := range out {
			v, err := r.readInt16()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsIntArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]int32, n)
		for i := range out {
			v, err := r.readInt32()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsLongArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]int64, n)
		for i := range out {
			v, err := r.readInt64()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = v
		}
		return SFSValue{tag, out}, nil
	case sfsFloatArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]float32, n)
		for i := range out {
			b, err := r.readBytes(4)
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = math.Float32frombits(binary.BigEndian.Uint32(b))
		}
		return SFSValue{tag, out}, nil
	case sfsDoubleArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]float64, n)
		for i := range out {
			b, err := r.readBytes(8)
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = math.Float64frombits(binary.BigEndian.Uint64(b))
		}
		return SFSValue{tag, out}, nil
	case sfsUtfStringArray:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		if err := r.chargeNodes(int(n)); err != nil {
			return SFSValue{}, err
		}
		out := make([]string, n)
		for i := range out {
			s, err := r.readUtfString()
			if err != nil {
				return SFSValue{}, err
			}
			out[i] = s
		}
		return SFSValue{tag, out}, nil
	case sfsArrayType:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		r.depth++
		defer func() { r.depth-- }()
		if r.depth > maxNestDepth {
			return SFSValue{}, fmt.Errorf("sfsobject: nesting depth exceeds %d", maxNestDepth)
		}
		arr := NewSFSArray()
		for i := int16(0); i < n; i++ {
			v, err := r.readTaggedValue()
			if err != nil {
				return SFSValue{}, err
			}
			arr.items = append(arr.items, v)
		}
		return SFSValue{tag, arr}, nil
	case sfsObjectType:
		n, err := r.readArrayCount()
		if err != nil {
			return SFSValue{}, err
		}
		r.depth++
		defer func() { r.depth-- }()
		if r.depth > maxNestDepth {
			return SFSValue{}, fmt.Errorf("sfsobject: nesting depth exceeds %d", maxNestDepth)
		}
		obj := NewSFSObject()
		for i := int16(0); i < n; i++ {
			key, err := r.readUtfString()
			if err != nil {
				return SFSValue{}, err
			}
			vtag, err := r.readByte()
			if err != nil {
				return SFSValue{}, err
			}
			v, err := r.readValuePayload(vtag)
			if err != nil {
				return SFSValue{}, err
			}
			obj.put(key, v)
		}
		return SFSValue{tag, obj}, nil
	default:
		return SFSValue{}, fmt.Errorf("sfsobject: unsupported decode tag %d at pos %d", tag, r.pos-1)
	}
}

// DecodeObject parses a self-describing SFSObject blob (leading tag byte 18).
//
// Every real caller (conn.go, decode.go) hands this an exact-length frame body, so any bytes left
// over after the top-level object is fully decoded mean the encode/decode walk desynced somewhere
// -- the same class of silent misalignment the sfsByteArray count-width bug caused before it was
// caught (see the comment on that case above). Rather than risk repeating that, an unconsumed
// remainder is treated as a decode error instead of being silently accepted.
func DecodeObject(data []byte) (*SFSObject, error) {
	r := &sfsReader{data: data}
	tag, err := r.readByte()
	if err != nil {
		return nil, err
	}
	if tag != sfsObjectType {
		return nil, fmt.Errorf("sfsobject: expected top-level tag 18 (SFS_OBJECT), got %d", tag)
	}
	v, err := r.readValuePayload(tag)
	if err != nil {
		return nil, err
	}
	if rem := r.remaining(); rem > 0 {
		return nil, fmt.Errorf("sfsobject: %d trailing bytes after decoded object", rem)
	}
	return v.Val.(*SFSObject), nil
}
