package session

import (
	"errors"
	"fmt"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"slices"
	"time"
)

// This file holds the low-level "wire core" shared by the game session connection: the
// field-type validation helpers, the non-timeout net.Error classifier, and the envelope
// wait-loop primitives. It was factored out of the former flat internal/app package so that
// conn.go's own read/handshake loops (which use these) sit in the same package as their helpers.

// RequirePresentField reports whether o has field, logging a Warn with the raw entry (for
// diagnosability) and returning false if it's missing -- shared by every list-parsing code path
// (buildings, mail, visitors) that must tolerate a malformed/unexpected entry from the server
// without crashing or silently fabricating a zero-value id. An explicit sfs.SFSNull for field is
// treated the same as a missing field: Has() only reflects key presence, so a null-typed entry
// would otherwise slip past the guard and GetInt/GetLong/GetString would fall through to a
// zero value indistinguishable from a genuine one.
//
// Presence-only, by design: this does NOT check field's concrete decoded type, so it's only
// sufficient on its own for a caller that never feeds field to a typed accessor (GetLong/GetInt/
// GetString). As of this round, the only caller is RequireFieldType immediately below, which
// layers its own type check on top -- there is no call site anywhere in this codebase that invokes
// RequirePresentField directly for a hard, warn-on-any-absence reject.
//
// (mail.go's HasUnclaimedReward used to be cited here as that example, but that citation went
// stale twice over and is removed as of round 30: its "== 0" comparison keys a lookup off the
// value, so it never fit the presence-only pattern this paragraph describes even before round 30 --
// and round 30 additionally stopped it from calling RequirePresentField at all, since a
// genuinely-absent rewardStatus is the NORMAL case for notification-only mail, not something worth
// this function's unconditional Warn. It now checks presence directly and silently via a plain
// `Get`, warning only for the present-but-wrong-typed case via warnIfWrongTypedField (login.go) --
// see HasUnclaimedReward's own doc comment.)
//
// Every call site that DOES key a dedup map or lookup off a field read via a typed accessor
// (buildings.go/mail.go/alliance.go/visitors.go's uuid/uid/type/scienceId checks) must use
// RequireFieldType below instead -- see that function's doc comment for why.
func RequirePresentField(o *sfs.SFSObject, field, context string) bool {
	v, ok := o.Get(field)
	if !ok || v.Val == nil {
		slog.Warn("skipping "+context+" entry with no "+field+" field", "raw", o.String())
		return false
	}
	return true
}

// SFSFieldKind identifies which family of concrete decoded Go types a field must be one of for
// the caller's chosen typed accessor (GetLong/GetInt/GetString, sfsobject.go) to read it
// correctly instead of silently falling through to that accessor's own zero-value coercion. Each
// constant's accepted-type set mirrors the corresponding accessor's own type switch exactly, so
// RequireFieldType's check and the accessor's own behavior can never disagree.
type SFSFieldKind int

const (
	// SFSFieldKindLong mirrors GetLong's accepted set: int64, int32, int16, byte.
	SFSFieldKindLong SFSFieldKind = iota
	// SFSFieldKindInt mirrors GetInt's accepted set: int32, int16, byte, int64. This happens to be
	// the identical Go-type set as SFSFieldKindLong above (GetInt and GetLong both widen/narrow
	// across the same four integer types) -- kept as a separate constant anyway so call sites stay
	// self-documenting about which accessor they actually use, and so the two can diverge safely if
	// GetInt/GetLong's own accepted sets ever do.
	SFSFieldKindInt
	// SFSFieldKindString mirrors GetString's accepted set: string. Both sfs.SFSUtfString and sfs.SFSText
	// wire tags decode to Go's plain string type (sfsobject.go's decode switch), so there is no
	// further wire-tag-level distinction to make from the Go type alone.
	SFSFieldKindString
)

// SFSFieldKindAccepts reports whether val's concrete Go type is one kind's corresponding
// GetLong/GetInt/GetString accessor actually reads, rather than silently coercing to a zero
// value.
func SFSFieldKindAccepts(kind SFSFieldKind, val interface{}) bool {
	switch kind {
	case SFSFieldKindLong, SFSFieldKindInt:
		switch val.(type) {
		case int64, int32, int16, byte:
			return true
		}
	case SFSFieldKindString:
		switch val.(type) {
		case string:
			return true
		}
	}
	return false
}

// RequireFieldType is RequirePresentField's type-aware sibling: it reports whether o has field
// AND that field's concrete decoded Go type is one kind's accessor (GetLong/GetInt/GetString)
// actually accepts, logging a Warn and returning false otherwise -- treating a present-but-
// wrong-typed field exactly the same as an absent one, rather than letting it silently pass
// RequirePresentField's presence-only guard and then fall through to GetLong/GetInt/GetString's
// own zero-value coercion.
//
// Why this exists (round 28 audit): GetLong/GetInt/GetString (sfsobject.go) each silently return
// a zero value (int64(0)/int32(0)/"") for ANY non-nil value whose concrete Go type isn't in their
// accepted set -- indistinguishable from a genuine zero/empty value. RequirePresentField alone
// only ever checked presence, so a present-but-wrong-typed uuid/uid/type/scienceId field (e.g. the
// server sending a uuid as a string, or a type as a float) used to pass its guard and then
// silently coerce to zero once read via the typed accessor -- colliding with a genuinely-zero
// entry, or another wrong-typed one, in every dedup map/lookup keyed on it: login.go's
// dedupeBuildings/dedupeVisitors (the PRIMARY init-push path, called from Login() on every login,
// since both consume ParseInitBuildings/ParseInitVisitors' output directly), buildings.go's own
// seenBuildingUUIDs/seenVisitorUUIDs in FetchBuildings, mail.go's seenUIDs in ListMail (a
// PERMANENT reward loss on a wrong-typed uid, since rewardStatus is per-mail), mail.go's
// groupUnclaimedByType (silently merges into a genuine type=0 batch), and alliance.go's
// findRecommendedTech (returns scienceId=0, causing a real donate attempt against the wrong tech).
//
// Every call site that keys a dedup map or lookup off a field this codebase reads via GetLong/
// GetInt/GetString uses this instead of RequirePresentField for that reason -- see
// TestParseInitBuildingsWrongTypedUUIDIsRejected (buildings_visitors_test.go) and
// TestFindRecommendedTechWrongTypedScienceIdIsRejected (alliance_test.go) for the regression
// coverage proving a wrong-typed field is now rejected rather than silently coerced to zero.
func RequireFieldType(o *sfs.SFSObject, field, context string, kind SFSFieldKind) bool {
	if !RequirePresentField(o, field, context) {
		return false
	}
	v, _ := o.Get(field) // presence and non-nil-ness already confirmed by RequirePresentField above
	if !SFSFieldKindAccepts(kind, v.Val) {
		slog.Warn("skipping "+context+" entry with wrong-typed "+field+" field",
			"raw", o.StringRedacted(), "goType", fmt.Sprintf("%T", v.Val))
		return false
	}
	return true
}

// ContainsNonTimeoutNetError reports whether err's error tree contains a net.Error anywhere with
// Timeout() == false -- a genuine connection-level failure (connection reset, broken pipe, DNS
// failure, TLS error, etc.) -- as opposed to a net.Error with Timeout() == true (SendAndWait's
// ordinary "no matching response within DefaultCmdTimeout" per-item outcome). See CollectAll's
// round-22 doc comment above for why this exists: a plain `errors.As(err, &netErr) &&
// !netErr.Timeout()` check is not enough once err can be a multi-error errors.Join tree (which
// GreetVisitors/ClaimAllMail/ClaimAllianceGifts each return) -- errors.As stops at the FIRST
// net.Error its own depth-first walk finds, which can be a benign timeout even when a genuine
// non-timeout net.Error is sitting elsewhere in the same tree.
//
// This walks the same tree shape errors.As itself documents walking -- err itself, then repeatedly
// its Unwrap() error or Unwrap() []error method -- but does not stop at the first net.Error match:
// every node is checked directly (so a node that is itself both a net.Error and a wrapper, e.g. the
// standard library's *net.OpError, is correctly read via its own Timeout() rather than skipped over
// in favor of whatever it wraps), a benign (Timeout()==true) match does not end the search, and a
// multi-error join recurses into every branch, returning true the moment ANY one of them is a
// genuine non-timeout net.Error -- order-independent, unlike errors.As.
func ContainsNonTimeoutNetError(err error) bool {
	for err != nil {
		if netErr, ok := err.(net.Error); ok && !netErr.Timeout() {
			return true
		}
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			for _, sub := range x.Unwrap() {
				if ContainsNonTimeoutNetError(sub) {
					return true
				}
			}
			return false
		case interface{ Unwrap() error }:
			err = x.Unwrap()
		default:
			return false
		}
	}
	return false
}

// DeadlineExceededError is WaitFor's own wall-clock-deadline-elapsed outcome: the loop read at
// least one envelope (none of them matched pred), and the overall timeout ran out before another
// one arrived. It satisfies net.Error with Timeout()==true so callers built on the "SendAndWait's
// ordinary timeout outcome IS ITSELF a net.Error with Timeout()==true" premise (buildings.go,
// mail.go, visitors.go, alliance.go, interactive.go -- see their errors.As-against-net.Error
// checks) treat this exit the same benign way they already treat the OTHER exit from this
// function: a genuine per-read SetReadDeadline+ReadEnvelope timeout, which returns a real
// net.Error from the network layer itself. Before this type existed, this branch returned a bare
// fmt.Errorf, which is not a net.Error at all -- indistinguishable from a genuine dead-connection
// failure to every one of those callers' errors.As checks, even though it's exactly as benign as
// the per-read-timeout exit right below it in this same function.
type DeadlineExceededError struct{}

func (DeadlineExceededError) Error() string   { return "timed out waiting for matching envelope" }
func (DeadlineExceededError) Timeout() bool   { return true }
func (DeadlineExceededError) Temporary() bool { return false }

// MaxConsecutiveDecodeFailures bounds how many consecutive non-timeout, non-net.Error
// ReadEnvelope failures (e.g. a malformed/undecodable frame) any of this codebase's independent
// read loops will silently tolerate before giving up -- round-50 fix, closing a DoS gap the
// round-48/49 Warn+continue fixes introduced across all four such loops (WaitFor and
// waitForInitPush here, buildings.go's FetchBuildings, conn.go's DoHandshake): tolerating a
// SINGLE malformed frame is correct (sfs.ReadPacket has already fully consumed that frame's bytes
// before sfs.DecodeObject ever ran, so the stream stays in sync -- see those fixes' own doc
// comments), but tolerating an UNBOUNDED stream of them let a hostile peer burn CPU and log
// volume for the full remaining wall-clock window of every wait, with only the caller-supplied
// timeout (up to 45s for waitForInitPush) eventually stopping it. Mirrors interactive.go's own
// consecutiveScanErrors/controlPipeRetries pattern for the identical class of gap in the
// control-FIFO scanner. Kept small: a genuine peer only ever needs to survive ONE stray/
// malformed frame; two dozen in a row is already far more tolerance than any real scenario
// this was built for needs.
const MaxConsecutiveDecodeFailures = 20

// MaxNonMatchingEnvelopesPerWait bounds how many successfully-decoded but NOT-the-awaited-thing
// envelopes any of this codebase's independent read loops will process in a single wait call
// before giving up -- round-53 fix, closing a DoS gap the round-48/49/50/51 decode-error-survival
// fixes never addressed: those fixes only bound consecutive DECODE FAILURES (MaxConsecutiveDecodeFailures
// above), but a hostile peer that streams well-formed, successfully-decoded, simply-irrelevant
// pushes for the duration of a wait window resets that counter to 0 on every single one and is
// never slowed down by it. Each such push still costs a full sfs.ReadPacket/sfs.DecodeObject/AsExtension
// cycle, and several of these loops' own "observed/skipped" diagnostics (buildings.go's
// FetchBuildings default/push.queue.add/push.build.queue.info cases, conn.go's DoHandshake)
// unconditionally format and log the full decoded content at Info level -- not gated behind
// -log-level=debug -- so an unbounded stream of them also produces an unbounded volume of
// Info-level log lines and StringRedacted() formatting cost, for the full duration of every wait
// window (up to 45s for waitForInitPush).
//
// Treated as benign (reuses DeadlineExceededError's net.Error/Timeout()==true shape), not fatal
// (unlike MaxConsecutiveDecodeFailures' sfs.DeadConnError): a long stream of well-formed-but-irrelevant
// traffic is not, on its own, evidence the connection itself is dead the way a stream of
// undecodable garbage is -- it might simply be a very busy connection that hasn't yet sent the
// awaited response. Only incremented on envelopes that do NOT advance real state (a skipped/
// unrecognized push, a non-extension envelope) -- legitimate, repeatedly-matching traffic (e.g.
// buildings.go's own push.add.building/push.init.build cases, which can legitimately arrive many
// times) is deliberately NOT counted against this cap, since counting it risked prematurely giving
// up on a genuinely busy but healthy connection. Set high enough that no legitimate traffic
// pattern this codebase has ever observed should plausibly reach it.
const MaxNonMatchingEnvelopesPerWait = 1000

// WaitFor reads envelopes until pred matches or timeout elapses, logging
// everything it skips past along the way.
func WaitFor(conn *GameConn, timeout time.Duration, pred func(*Envelope) bool) (*Envelope, error) {
	deadline := time.Now().Add(timeout)
	consecutiveDecodeFailures := 0
	nonMatchingEnvelopes := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, DeadlineExceededError{}
		}
		_ = conn.conn.SetReadDeadline(time.Now().Add(remaining))
		env, err := conn.ReadEnvelope()
		if err != nil {
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				// A genuine per-read timeout -- return it immediately, unchanged from this
				// function's original behavior. Do NOT merely `continue` here: a caller-
				// injected timeout net.Error (e.g. a test double simulating a read timeout)
				// may not actually consume real wall-clock time the way a genuine socket
				// deadline would, so looping back to the top-of-loop remaining<=0 check
				// could busy-spin for the rest of the window instead of returning promptly.
				return nil, err
			}
			if ContainsNonTimeoutNetError(err) {
				return nil, err
			}
			// Round-49 fix: a plain, non-net.Error ReadEnvelope failure (e.g. a
			// sfs.DecodeObject parse failure on one malformed/unrelated push) means
			// sfs.ReadPacket already fully consumed that frame's bytes off the wire
			// before sfs.DecodeObject ever ran -- the stream stays in sync, so this is
			// not evidence the connection is dead, mirroring waitForInitPush's
			// identical round-48 fix. Previously ANY such error aborted the caller's
			// single, non-retried wait outright -- login.go's Login and
			// crossserver.go's DoCrossServerLogin's raw login-response waits, and
			// every SendAndWait/WaitForCmd caller across mail.go/alliance.go/
			// visitors.go/buildings.go/interactive.go -- even though the genuinely
			// awaited response/push might arrive on the very next read.
			consecutiveDecodeFailures++
			if consecutiveDecodeFailures > MaxConsecutiveDecodeFailures {
				// sfs.DeadConnError (packet.go): round-51 fix -- see waitForInitPush's identical
				// fix above for the full MAJOR-finding rationale. This give-up error is never
				// itself a net.Error by construction (reached only after both the
				// Timeout()==true check and ContainsNonTimeoutNetError(err) above already
				// ruled that out), so without this wrap, every SendAndWait/WaitForCmd caller's
				// ContainsNonTimeoutNetError-based "abort on dead connection" check (mail.go's
				// ClaimAllMail, buildings.go's CollectAll, main.go's
				// shouldAbortBeforeInteractive, ...) silently misclassified it as benign.
				return nil, sfs.NewDeadConnError(fmt.Errorf("WaitFor: %d consecutive malformed/undecodable envelopes, giving up: %w", consecutiveDecodeFailures, err))
			}
			slog.Warn("WaitFor: failed to read/decode an envelope while waiting; continuing to wait, not treating this as a dead connection", "error", err, "consecutiveDecodeFailures", consecutiveDecodeFailures)
			continue
		}
		consecutiveDecodeFailures = 0
		if pred(env) {
			return env, nil
		}
		nonMatchingEnvelopes++
		if nonMatchingEnvelopes > MaxNonMatchingEnvelopesPerWait {
			return nil, fmt.Errorf("WaitFor: %d well-formed but non-matching envelopes processed, giving up: %w", nonMatchingEnvelopes, DeadlineExceededError{})
		}
		if msg, ok := env.AsExtension(); ok {
			slog.Debug("skipped push while waiting", "cmd", msg.Cmd, "params", msg.Params.StringRedacted())
		}
	}
}

// WaitForCmd waits for an extension message whose cmd matches any of wantCmds.
func WaitForCmd(conn *GameConn, timeout time.Duration, wantCmds ...string) (*ExtensionMessage, error) {
	env, err := WaitFor(conn, timeout, func(e *Envelope) bool {
		msg, ok := e.AsExtension()
		if !ok {
			return false
		}
		return slices.Contains(wantCmds, msg.Cmd)
	})
	if err != nil {
		return nil, err
	}
	msg, _ := env.AsExtension()
	return msg, nil
}

// WarnIfWrongTypedField logs a Warn when o has field present (non-nil) but its concrete decoded
// Go type isn't one kind's corresponding GetLong/GetInt/GetString accessor (sfsobject.go) actually
// reads -- distinct from field being genuinely absent, which every call site already, correctly,
// treats as "nothing to do" and stays silent about. Unlike buildings.go's requireFieldType (which
// collapses missing-vs-wrong-typed into the same Warn+reject, appropriate for a hard list-entry
// reject), this only adds a diagnostic for the wrong-typed case: an absent field must not itself
// log anything.
//
// context names the caller for the log line's benefit -- deliberately generic wording (not
// "...persist...", which fit only this function's original login.go call sites, all genuinely
// persistence reads): round 30 reused this same helper for mail.go call sites that have nothing to
// do with persistence (HasUnclaimedReward's rewardStatus classification, ListMail's lastMailTime
// pagination cursor), and the old hardcoded "persist" wording produced nonsensical log lines like
// "skipping mail reward status persist: rewardStatus field present but wrong-typed" there. Fixed
// (round 31) by dropping the persistence-specific word from the template entirely.
func WarnIfWrongTypedField(o *sfs.SFSObject, field, context string, kind SFSFieldKind) {
	v, ok := o.Get(field)
	if !ok || v.Val == nil {
		return
	}
	if !SFSFieldKindAccepts(kind, v.Val) {
		slog.Warn("skipping "+context+": "+field+" field present but wrong-typed",
			"field", field, "goType", fmt.Sprintf("%T", v.Val))
	}
}
