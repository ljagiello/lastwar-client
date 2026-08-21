package game

import (
	"errors"
	"fmt"
	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
	"log/slog"
	"net"
	"slices"
	"strings"
)

// Mail is one inbox entry. Confirmed live via a real packet capture of the
// actual game client tapping "Claim All" across several mailbox category
// tabs (Alliance, Event, Season, Regular). Two things this contradicted
// from a naive first read of the command catalog:
//
//  1. Mail uids are STRING GUIDs (e.g.
//     "1f9f236259ff4ae285bf5b20bdac2586"), unlike every other uuid this
//     client deals with (buildings, visitors), which are int64s.
//  2. Reward-claiming is split into categories (`type`), and scoped per
//     category, not global -- the real client's "Claim All" button sent
//     one `mail.reward.batch` call per tab, each with only that tab's
//     mail uids and that tab's `type` value (observed live: 3, 4, and 9
//     for three different tabs -- the exact type<->tab-name mapping was
//     not determined, but it doesn't need to be: this just groups by
//     whatever `type` each mail object already reports). Marking mail as
//     read (`mail.read.status.betch`) is NOT scoped by type at all, and
//     is not conditional on having a reward either -- see ClaimAllMail.
type Mail struct {
	Raw *sfs.SFSObject
}

func (m Mail) Uid() string         { return m.Raw.GetString("uid") }
func (m Mail) Type() int32         { return m.Raw.GetInt("type") }
func (m Mail) RewardStatus() int32 { return m.Raw.GetInt("rewardStatus") }

// HasUnclaimedReward reports whether this mail has a reward still waiting to be claimed.
// `rewardStatus == 0` is the confirmed "unclaimed" value (docs/live-validation.mdx's Mail
// section), but GetInt can't distinguish a real 0 from a genuinely-absent OR wrong-typed field --
// all three silently coerce to the int32 zero value. That conflation matters here specifically:
// notification-only mail (alliance markers, battle reports, and similar -- see ClaimAllMail's doc
// comment) never carries a reward at all, and plausibly omits the rewardStatus key entirely rather
// than sending an explicit 0. Treating a missing (or wrong-typed) key as "unclaimed" would
// misclassify that mail as having a reward it doesn't have.
//
// Fixed (round 29): this used to check presence only (`v, ok := m.Raw.Get("rewardStatus"); !ok ||
// v.Val == nil`) before comparing m.RewardStatus() == 0 -- guarding the missing/explicit-null case
// but not the present-but-wrong-typed one. GetInt silently coerces ANY non-nil value whose concrete
// Go type isn't in its accepted set (int32/int16/byte/int64) to int32(0), not just a missing field,
// so a present-but-wrong-typed rewardStatus (e.g. sent as a string or float) passed the old guard,
// coerced to 0 via GetInt, and the "== 0" comparison DETERMINISTICALLY (every time, not merely a
// collision risk) misclassified it as unclaimed -- feeding groupUnclaimedByType, which would then
// bucket that mail's uid into a real mail.reward.batch request for a reward that may not exist.
//
// Fixed (round 30): the round-29 fix above routed this through requireFieldType (buildings.go),
// which delegates to requirePresentField -- and requirePresentField unconditionally logs a Warn for
// ANY missing field, not just a wrong-typed one. But a genuinely-absent rewardStatus is the NORMAL,
// EXPECTED case for notification-only mail (see this doc comment's second paragraph, and
// ClaimAllMail's doc comment, which cites a live capture where all 21 visible mail items lacked
// rewardStatus) -- not an anomaly worth a warning. Routing it through requireFieldType meant every
// single routine notification-only mail item logged a spurious Warn. Now mirrors visitors.go's
// ParseInitVisitors maxNum handling: presence/non-nil is checked directly and silently (no warning
// -- this is the expected case) before falling through to false, and session.WarnIfWrongTypedField
// (login.go) is used instead of requireFieldType so a Warn fires ONLY for the present-but-wrong-
// typed case, which remains rejected (not misclassified as unclaimed) exactly as the round-29 fix
// intended. See TestHasUnclaimedRewardWrongTypedRewardStatusIsNotMisclassified (now also covering
// the absent case logs nothing) for the regression coverage.
func (m Mail) HasUnclaimedReward() bool {
	v, ok := m.Raw.Get("rewardStatus")
	if !ok || v.Val == nil {
		// Genuinely absent: the normal case for notification-only mail. Silent -- no warning --
		// matching ParseInitVisitors' maxNum handling (visitors.go) for the identical
		// absent-is-expected reason.
		return false
	}
	session.WarnIfWrongTypedField(m.Raw, "rewardStatus", "mail reward status", session.SFSFieldKindInt)
	if !session.SFSFieldKindAccepts(session.SFSFieldKindInt, v.Val) {
		return false
	}
	return m.RewardStatus() == 0
}

// mailListRawItemCap bounds how many RAW entries in a single page's `msg` response array ListMail's
// pagination loop below will examine, independent of ListMail's own mailListPageSize (100, the
// requested page-size hint) and maxPages (20) -- both of those bound round-trip COUNT only, not the
// size of any single page's response array, which is otherwise bounded only by sfsobject.go's much
// larger sfs.MaxDecodedNodes=300,000 decode budget. Without this, a malformed entry (not an *sfs.SFSObject,
// or missing/wrong-typed the required "uid" field via requireFieldType) hits a `continue` that
// doesn't advance any output-count-based cap, since it never reaches the append -- the same gap
// visitors.go's ParseInitVisitors closed in round 26 for visitor.list, applied here to mail.list
// pages. requireFieldType itself logs a Warn per malformed entry, so a hostile/misbehaving peer
// responding to a single mail.list page with a huge malformed array would otherwise force full
// scan-and-log cost regardless of the requested page size. Set comfortably above
// mailListPageSize=100 -- a legitimate server response may reasonably vary somewhat from the exact
// requested page size -- but still finite and well below the decode-level ceiling.
//
// Package-scoped (round 28), matching buildings.go's maxRawBuildingItemsPerPush and alliance.go's
// allianceScienceRawItemCap -- this used to be a function-scoped const inside ListMail itself, the
// only one of this codebase's three raw-item-scan caps declared that way, purely for consistency
// with its two siblings (no functional difference either way, since ListMail was and remains its
// only reader).
const mailListRawItemCap = 1000

// maxAggregateMailPerFetch bounds the TOTAL number of Mail entries ListMail's pagination loop
// below will accumulate into `all` across ALL pages, independent of mailListRawItemCap (which
// only bounds a single page's raw-item SCAN cost) and maxPages/mailListPageSize (which only bound
// round-trip COUNT, not aggregate size). Round-40 fix: before this existed, a hostile -cs-ip peer
// could answer each of up to maxPages(20) page requests with mailListRawItemCap(1000) valid-shaped
// mail entries and always report more=true with a valid, distinct lastUid/lastMailTime to keep
// pagination advancing -- accumulating up to 20*1000=20,000 retained *sfs.SFSObject-backed Mail
// entries with zero aggregate cap anywhere in this function, the same "aggregate ceiling missing"
// gap buildings.go (MaxAggregateBuildingsPerFetch) and visitors.go (MaxVisitorsUpperBound) both
// already close for their own accumulators. ClaimAllMail then walks the full uncapped result,
// forcing a correspondingly unbounded number of mail.read.status.betch/mail.reward.batch round
// trips (batchByCountAndBytes' readBatchSize=100). Set to maxPages(20)*mailListPageSize(100) --
// the legitimate/intended maximum under normal well-behaved pagination -- generously large enough
// that no real inbox should ever hit it, in the same spirit as MaxAggregateBuildingsPerFetch/
// MaxVisitorsUpperBound's own sizing rationale.
const maxAggregateMailPerFetch = 2000

// maxMailRewardTypesPerRun is a defensive, non-protocol-guessing sanity ceiling on how many
// distinct mail `type` buckets ClaimAllMail's reward-claim loop below will issue
// mail.reward.batch requests for in one run -- the same category of ceiling as buildings.go's
// maxCollectibleBuildingsPerRun and visitors.go's MaxVisitorsUpperBound, applied to this loop's
// item COUNT for the identical reason: each iteration can cost up to a full defaultCmdTimeout
// (8s, conn.go) against a peer that simply never responds, so 300 * 8s bounds the worst case at
// ~40 minutes instead of an unbounded hang, matching those two constants' own value and rationale
// exactly.
const maxMailRewardTypesPerRun = 300

// maxMailBatchesPerLoop is a defensive, non-protocol-guessing sanity ceiling on how many
// mail.read.status.betch/mail.reward.batch round trips ClaimAllMail's two batch loops below will
// each issue -- round-44 fix, closing a gap on a distinct axis from maxAggregateMailPerFetch (total
// mail ITEM count) and maxMailRewardTypesPerRun (distinct TYPE count): neither bounds the number of
// BATCHES batchByCountAndBytes produces. batchByCountAndBytes always admits at least one uid per
// batch even if that uid alone exceeds maxUIDsBytes (see its own doc comment), and a mail uid is a
// server-supplied string bounded only by the wire format's 65535-byte string-length limit -- well
// over maxUIDsBytes(60000) -- so a peer returning mail entries with maximal-length uids can force
// every batch down to a single item, turning maxAggregateMailPerFetch(2000) items into up to 2000
// sequential round trips instead of the ~20 batches (2000/readBatchSize=100) normal pagination
// produces. Same category of ceiling, and the same value/rationale, as maxMailRewardTypesPerRun
// above and buildings.go's maxCollectibleBuildingsPerRun/visitors.go's MaxVisitorsUpperBound: each
// iteration can cost up to a full defaultCmdTimeout (8s, conn.go) against a peer that simply never
// responds, so 300 * 8s bounds the worst case at ~40 minutes instead of up to ~4.4 hours.
const maxMailBatchesPerLoop = 300

// maxMailRewardBatchesPerRun bounds the TOTAL number of mail.reward.batch round trips
// ClaimAllMail's reward-claim loop will issue, SUMMED ACROSS ALL distinct mail types in one run --
// round-46 fix, closing a gap on a third axis distinct from maxMailRewardTypesPerRun (distinct
// TYPE count) and maxMailBatchesPerLoop (batches PER TYPE, reset fresh for every type): neither of
// those two caps bounds the sum across the whole outer loop. A hostile peer can spread up to
// maxAggregateMailPerFetch(2000) unclaimed-reward mail entries across up to
// maxMailRewardTypesPerRun(300) distinct types (~6-7 entries per type) each with a uid long
// enough (up to maxMailUidLen=65535, still well over maxUIDsBytes=60000) to force
// batchByCountAndBytes into one uid per batch -- with only ~6-7 batches per type, no single type
// ever reaches maxMailBatchesPerLoop's own 300-batch truncation, but the SUM across 300 types
// still reaches roughly 2000 mail.reward.batch round trips, each able to cost up to a full
// defaultCmdTimeout (8s, conn.go) against a peer that simply stalls -- ~4.4 hours, exactly the
// threat maxMailBatchesPerLoop's own doc comment above claims is bounded to "~40 minutes instead
// of up to ~4.4 hours", a claim that only holds per type. Same value/rationale as its siblings.
const maxMailRewardBatchesPerRun = 300

// maxMailUidLen bounds how long a single mail entry's `uid` field may be before ListMail's
// per-entry loop below skips it (with a Warn) instead of appending it to `all` -- round-45 fix.
// GetString (sfsobject.go) makes no distinction between the sfs.SFSUtfString wire tag (length-capped
// at 65535 bytes by readUtfString's own 2-byte length prefix) and the sfs.SFSText wire tag (bounded
// only by packet.go's sfs.MaxFrameSize, via a 4-byte length prefix) -- both decode to a plain Go
// string, and requireFieldType's sfsFieldKindString check accepts either shape identically. So a
// uid arriving tagged sfs.SFSText (rather than the presumably-expected sfs.SFSUtfString) could previously
// be arbitrarily long, well past the wire format's own 65535-byte string-length limit.
// batchByCountAndBytes always admits at least one uid per batch even if that uid alone exceeds its
// own maxUIDsBytes budget (see its own doc comment), so an oversized uid became its own singleton
// batch and reached sfs.WriteUtfString (sfsobject.go) when ClaimAllMail re-encoded it into a
// mail.read.status.betch/mail.reward.batch request's "uids" field -- sfs.WriteUtfString hard-errors for
// any string over 65535 bytes, a PURELY LOCAL encode failure with no involvement of the network at
// all. That local error is wrapped in sendStageError (conn.go), whose Timeout() is hardcoded false
// by design (see its own doc comment: this is intentional, covering "a local encode error from
// deeper in SendExtension/SendEnvelope" alongside genuine write failures) -- so ClaimAllMail's own
// net.Error checks misclassified this single malformed mail entry as a dead connection, aborting
// the rest of ClaimAllMail, and via CollectAll's identical containsNonTimeoutNetError abort logic
// (buildings.go), every other -collect action scheduled after it in the same run. Set at exactly
// the wire format's own hard limit -- any uid this function would otherwise accept is guaranteed
// re-encodable by sfs.WriteUtfString later, closing the gap at its source instead of only softening the
// downstream misclassification.
const maxMailUidLen = 65535

// ListMail fetches the account's mail via `chat.get.system.mails`,
// following the real client's own request shape
// (extracted/lua_decompiled/5018_Net_Msgs_Mail_MailGetMutiMessage.lua:
// `clientseq`, `time`, `count`, `firstCmd`, `isAll`) and paginating via
// the response's `more`/`lastUid`/`lastMailTime` fields until the server
// reports no more pages or a safety cap is hit.
//
// One real gap: the captured request already had a non-empty `clientseq`/
// `time` matching a specific already-known mail uid, because the real
// client was mid-session with a warm local mail cache built up over a
// long history of `push.mail` notifications -- the exact cold-start
// values (fresh `clientseq`/`time`) were never directly observed. This
// sends `time: 0`, `clientseq: ""` on the first call, matching
// `firstCmd: "YES"` (isFirst) exactly as captured, on the theory that's
// the correct "give me everything" cold-start shape -- confirmed working
// insofar as it returns real mail (see CollectAll's live test), but
// whether it truly returns the account's FULL history or just a recent
// window has not been independently verified against the real client.
func ListMail(conn *session.GameConn) ([]Mail, error) {
	const reqCmd = "chat.get.system.mails"
	const pushCmd = "push.chat.get.system.mails"
	const maxPages = 20
	const mailListPageSize = 100

	var all []Mail
	// seenUIDs dedupes mail uids across pages, mirroring FetchBuildings' seenBuildingUUIDs/
	// appendBuilding pattern (buildings.go): this function's own doc comment above already flags
	// real uncertainty about the pagination cursor's true semantics (the cold-start clientseq/time
	// values were never independently confirmed against the real client), so if the server's
	// cursor ever repeats a uid across two pages -- e.g. an off-by-one boundary re-send -- that
	// duplicate must not flow into the returned slice twice. Left unguarded, ClaimAllMail's
	// groupUnclaimedByType would bucket the same uid twice under its type and put it twice into a
	// single mail.reward.batch request's comma-joined uids field for that mail if it has an
	// unclaimed reward -- unverified server-side behavior for a duplicate uid in one batch
	// request, and not worth risking when a defensive guard is this cheap.
	seenUIDs := make(map[string]bool)
	clientseq := ""
	reqTime := int64(0)
	first := true
	// truncated tracks whether the loop is currently mid-pagination (the most recent response
	// had more=true and a usable lastUid, so another page was queued up) versus having stopped
	// for a "real" reason (more=false, or the lastUid-missing anomaly below, both of which break
	// out of the loop after resetting this back to false). It's reset at the top of every
	// iteration and only set true again at the bottom once a page has been fully consumed and
	// the next request is queued -- so if the for-loop instead exits because page reached
	// maxPages, whatever truncated was left as by the final iteration tells us whether that exit
	// happened while the server still had more mail to give (see the warning after the loop).
	truncated := false

	for page := range maxPages {
		// Round-40 fix: stop pagination entirely once the aggregate ceiling is reached, instead
		// of continuing to request (and immediately discard) further pages -- see
		// maxAggregateMailPerFetch's own doc comment for the full threat this closes.
		if len(all) >= maxAggregateMailPerFetch {
			slog.Warn("list mail: aggregate mail count across this fetch reached the upper bound; stopping early", "mailCount", len(all), "cap", maxAggregateMailPerFetch)
			break
		}
		truncated = false
		params := sfs.NewSFSObject()
		params.PutUtfString("clientseq", clientseq)
		params.PutLong("time", reqTime)
		params.PutInt("count", mailListPageSize)
		params.PutBool("isAll", true)
		if first {
			params.PutUtfString("firstCmd", "YES")
		}
		msg, err := session.SendAndWait(conn, fmt.Sprintf("list mail (page %d)", page), reqCmd, params, pushCmd)
		if err != nil {
			return all, err
		}
		v, ok := msg.Params.Get("msg")
		if ok {
			if arr, ok := v.Val.(*sfs.SFSArray); ok {
				if len(arr.Items()) > mailListRawItemCap {
					slog.Warn("list mail: page response array longer than raw-item scan cap; truncating scan", "page", page, "arrayLen", len(arr.Items()), "cap", mailListRawItemCap)
				}
				for i, item := range arr.Items() {
					if i >= mailListRawItemCap {
						break
					}
					// Round-40 fix: also enforce the aggregate ceiling PER APPEND, not just at the
					// outer loop's top -- mirrors buildings.go's appendBuilding/appendVisitor round-
					// 39/40 fix for the identical single-page-overshoot shape: without this, one page
					// carrying up to mailListRawItemCap(1000) valid-shaped entries could append all
					// 1000 in a single iteration before the loop-top check (above) was ever
					// re-consulted, overshooting maxAggregateMailPerFetch(2000) by up to that much.
					if len(all) >= maxAggregateMailPerFetch {
						break
					}
					mo, ok := item.Val.(*sfs.SFSObject)
					if !ok {
						continue
					}
					if !session.RequireFieldType(mo, "uid", "mail", session.SFSFieldKindString) {
						continue
					}
					m := Mail{Raw: mo}
					uid := m.Uid()
					if len(uid) > maxMailUidLen {
						// See maxMailUidLen's own doc comment: an oversized uid (e.g. arriving
						// tagged sfs.SFSText instead of sfs.SFSUtfString) can never be successfully
						// re-encoded by ClaimAllMail's batching later, so skip it here instead of
						// letting that later local encode failure be misclassified as a dead
						// connection and abort unrelated -collect work.
						slog.Warn("skipping mail entry with oversized uid field", "page", page, "uidLen", len(uid), "cap", maxMailUidLen)
						continue
					}
					if seenUIDs[uid] {
						continue
					}
					seenUIDs[uid] = true
					all = append(all, m)
				}
			} else {
				// Round-39 fix: present-but-wrong-typed used to be silently indistinguishable from
				// genuinely-absent, unlike this same function's own "more" field guard two lines
				// below, which already warns on the identical anomaly shape. Diagnostic only --
				// pagination still stops safely either way (via "more" defaulting to false below).
				slog.Warn("list mail: response's msg field is present but not an array", "page", page, "type", fmt.Sprintf("%T", v.Val))
			}
		}
		more := false
		if mv, ok := msg.Params.Get("more"); ok {
			if b, ok := mv.Val.(bool); ok {
				more = b
			} else {
				slog.Warn("list mail: response's more field is present but not a bool, treating as more=false and stopping pagination", "page", page, "collectedSoFar", len(all))
			}
		}
		if !more {
			break
		}
		// A response claiming more=true must also carry a genuine lastUid to seed the next
		// page's clientseq -- without this check, a missing/wrong-typed lastUid falls through
		// GetString's zero value ("") identical to the cold-start clientseq, which would send
		// the exact same request again and loop on a stale cursor (up to maxPages times) instead
		// of making forward progress. Treat that as a server-shape anomaly worth stopping for,
		// not silently retrying.
		lastUid := msg.Params.GetString("lastUid")
		if lastUid == "" {
			slog.Warn("list mail: response reported more=true but lastUid is missing/empty, stopping pagination instead of looping on a stale cursor", "page", page, "collectedSoFar", len(all))
			break
		}
		// Round-46 fix: lastUid gets re-sent verbatim as the next page's clientseq via
		// PutUtfString (sfs.WriteUtfString's own 65535-byte hard cap), but GetString can't
		// distinguish the 65535-byte-capped sfs.SFSUtfString wire tag from the far larger sfs.SFSText
		// tag -- the identical wire-tag-equivalence gap round 45 closed for the per-entry mail
		// uid field (maxMailUidLen, above). Left unguarded here, an oversized lastUid would
		// cause a purely local encode failure on the NEXT page request that sendStageError
		// (conn.go) deliberately classifies the same as a genuine dead connection, aborting the
		// rest of ClaimAllMail and, via CollectAll's containsNonTimeoutNetError abort, every
		// other -collect action scheduled after it -- even though the connection itself is
		// healthy. Treated the same as a missing/empty lastUid: stop pagination with whatever
		// was already collected, rather than sending an unencodable cursor.
		if len(lastUid) > maxMailUidLen {
			slog.Warn("list mail: response's lastUid exceeds the mail uid length cap, stopping pagination instead of re-sending an unencodable cursor", "page", page, "lastUidLen", len(lastUid), "cap", maxMailUidLen, "collectedSoFar", len(all))
			break
		}
		clientseq = lastUid
		// lastMailTime is lastUid's sibling cursor field, read the same way and forwarded the
		// same way into the next page's request -- but GetLong can't tell a missing/null/
		// wrong-typed lastMailTime apart from a legitimate explicit 0 (all three silently
		// coerce to int64(0)), unlike GetString's "" which is never a legitimate mail uid. Left
		// unguarded, a response with a valid lastUid but a missing, null, or wrong-typed
		// lastMailTime would silently reset reqTime to the same value as the cold-start request
		// while clientseq keeps advancing normally -- the exact failure shape the lastUid check
		// above exists to prevent, just on its sibling field. Impact is bounded (seenUIDs
		// dedupes any re-fetched mail, maxPages caps the loop), so this doesn't abort
		// pagination like the lastUid check does, but it's still worth surfacing so an operator
		// can tell a run hit this instead of quietly assuming it's a legitimate mail timestamped
		// at the epoch.
		//
		// Fixed (round 29): this used to check missing/null only (`!ok || v.Val == nil`), the
		// same presence-only shape requirePresentField uses -- so a PRESENT-BUT-WRONG-TYPED
		// lastMailTime (e.g. sent as a string) silently passed this guard and then coerced to 0
		// via GetLong with zero diagnostic signal, the same conflation HasUnclaimedReward's own
		// round-29 fix addresses for rewardStatus (see its doc comment above).
		//
		// Fixed (round 30): the round-29 fix routed this through requireFieldType (buildings.go),
		// the hard-reject/skip-an-entry pattern -- but this call site never actually skips
		// anything (it's inside the pagination loop over the whole page response, not a
		// per-array-entry loop), so requireFieldType's own "skipping list mail page entry with
		// no/wrong-typed lastMailTime field" log line was actively misleading (nothing is
		// skipped), and doubled up with the more specific warning immediately below into a
		// redundant double-Warn on every wrong-typed value.
		//
		// Fixed again (round 31): round 30's fix intended for the specific "missing/null/
		// wrong-typed" diagnostic below to be this call site's SOLE warning, but still also called
		// session.WarnIfWrongTypedField (login.go) alongside it -- which itself unconditionally warns on
		// the wrong-typed case, reintroducing the exact double-Warn round 30's own doc comment
		// (above) claimed to have eliminated. Presence and type are now checked directly and
		// silently via a plain Get + sfsFieldKindAccepts, with NO call to session.WarnIfWrongTypedField --
		// the specific message below is genuinely this call site's only warning for both the
		// missing and wrong-typed cases. See TestListMailWarnsOnWrongTypedLastMailTime (updated to
		// assert exactly one Warn line) and TestListMailWarnsOnMissingLastMailTime.
		lastMailTimeV, lastMailTimePresent := msg.Params.Get("lastMailTime")
		lastMailTimeOK := lastMailTimePresent && lastMailTimeV.Val != nil && session.SFSFieldKindAccepts(session.SFSFieldKindLong, lastMailTimeV.Val)
		if !lastMailTimeOK {
			slog.Warn("list mail: response reported more=true but lastMailTime is missing/null/wrong-typed, reqTime will reset to 0 for the next page instead of the real cursor value", "page", page, "collectedSoFar", len(all))
		}
		reqTime = msg.Params.GetLong("lastMailTime")
		first = false
		truncated = true
	}
	// If the loop above ran out of pages (maxPages) while the last-seen response still reported
	// more=true and a usable lastUid, that's a silent truncation: the account has more mail than
	// this run collected, and the caller (ClaimAllMail, and beyond it the -collect flow) has no
	// other way to notice. Mirror the lastUid-missing warning above so an operator can tell a run
	// was cut short and roughly how much mail was left uncollected.
	if truncated {
		slog.Warn("list mail: reached maxPages while server still reported more mail available, stopping pagination -- collected mail is truncated", "maxPages", maxPages, "collectedSoFar", len(all))
	}
	return all, nil
}

// ClaimAllMail lists the account's mail (see ListMail), marks every mail
// found as read, and separately claims rewards for whichever mail still
// has one -- confirmed live as two genuinely independent actions, not one.
//
// Bug fixed here: an earlier version only marked-as-read the subset of
// mail with `HasUnclaimedReward() == true`, silently skipping every
// notification-only mail (alliance markers, battle reports, and similar)
// that never carries a reward at all. That left their unread badges
// showing forever, since this function never touched them. A live
// capture of the real client's own "Claim All" on a Regular-category tab
// (all battle reports, no gift icons in the UI) showed exactly this
// asymmetry: it sent `mail.read.status.betch` for all 21 visible mail
// uids, but no matching `mail.reward.batch` call at all -- confirming
// mark-as-read is unconditional, independent of whether a mail has a
// reward, while `Net.Msgs.Mail.MailReadStatusBatchMessage`'s own
// OnCreate confirms the wire request needs only `uids`, no `type` -- so
// every discovered mail can be marked read in one pass regardless of
// category, unlike the reward claim below.
//
// Bug fixed here (round 16): ListMail deliberately returns whatever mail
// it already collected before a mid-pagination sendAndWait failure (see
// its own doc comment) rather than nil, specifically so a transient
// failure on e.g. page 3 of a multi-page mailbox doesn't have to cost the
// caller pages 1-2's worth of already-identified mail. An earlier version
// of this function threw that away anyway: it returned immediately on
// `err != nil` without ever looking at the `mail` ListMail still handed
// back, discarding every already-identified entry and claiming nothing
// for that run even though the data needed to do so was already fully in
// hand. Fixed by folding the ListMail error into `errs` (the same
// error-accumulation slice the batch-processing loops below already use)
// instead of returning early, so the rest of this function runs its
// normal batch-claiming logic against whatever partial `mail` slice
// ListMail managed to collect.
//
// Bug fixed here (round 17): the read-status and reward-claim batch loops below never checked
// whether a batch's sendAndWait failure was a net.Error -- a genuine connection-level failure
// (e.g. a silently dead/blackholed TCP connection), as opposed to a well-formed response carrying
// a decoded (possibly non-benign) errorCode. Without that check, a dead connection burned one full
// defaultCmdTimeout PER REMAINING BATCH instead of aborting immediately: ListMail's own maxPages
// cap alone means up to 20 pages' worth of 100-uid read-status batches, plus one reward-claim
// batch per distinct mail type in byType. Both loops now mirror CollectAll's identical
// errors.As-against-net.Error early-abort (buildings.go) exactly: append the triggering error to
// errs first (so the caller's aggregated error still reports what happened), then break. The
// reward-claim loop is additionally skipped in full if the read-status loop's break was a
// net.Error abort -- the connection is already known-dead at that point, so there is no reason to
// attempt any reward-claim batch at all. An ordinary decoded errorCode failure (not a net.Error)
// must still NOT abort either loop -- see TestClaimAllMailRewardLoopContinuesAcrossTypesAfterBusinessError.
//
// Bug fixed here (round 21): the round-17 net.Error check above was too broad. sendAndWait's
// ordinary "no matching response within defaultCmdTimeout (8s)" outcome IS ITSELF a net.Error with
// Timeout()==true (confirmed by conn_wait_test.go's TestWaitForTimeout) -- an expected, benign
// outcome on a perfectly healthy connection, not evidence anything is wrong. Treating it the same
// as a genuine connection-level failure (reset, broken pipe, DNS failure, TLS error -- all
// Timeout()==false) meant one slow response on ANY single batch aborted every other independent
// batch/type still waiting to be processed. All three net.Error checks below now additionally
// require !netErr.Timeout(): only a non-timeout net.Error still means the connection is known-dead
// and aborts the remaining work; a timeout net.Error falls through and is recorded in errs like any
// other per-action failure, exactly like FetchBuildings' own internal wait loop already treats
// Timeout()==true as the benign "stop waiting, move on" case (buildings.go) -- same distinction,
// opposite polarity, since this is a sequence of independent actions rather than one single wait.
func ClaimAllMail(conn *session.GameConn) error {
	var errs []error

	mail, err := ListMail(conn)
	if err != nil {
		errs = append(errs, fmt.Errorf("list mail: %w", err))
		// A NON-TIMEOUT net.Error here means the underlying connection is already known-dead --
		// ListMail's own pagination loop hit it mid-fetch (see ListMail's doc comment: it returns
		// whatever mail it already collected before the failure, not nil). Whatever partial `mail`
		// it handed back is deliberately NOT processed in that case: proceeding into the
		// read-status batch loop below would just burn one more defaultCmdTimeout issuing a batch
		// against a connection already known to be dead, mirroring the readAbortedByNetErr skip
		// further down. A net.Error with Timeout()==true -- sendAndWait's ordinary "no response
		// within defaultCmdTimeout" outcome -- is NOT evidence of a dead connection and, like an
		// ordinary decoded errorCode failure, must still fall through to process any partial mail
		// normally -- see TestClaimAllMailProcessesPartialMailOnListPageFailure and
		// TestClaimAllMailProcessesPartialMailOnListPageTimeout.
		var netErr net.Error
		if errors.As(err, &netErr) && !netErr.Timeout() {
			return errors.Join(errs...)
		}
	}
	if len(mail) == 0 {
		if err == nil {
			slog.Info("no mail found")
		}
		return errors.Join(errs...)
	}

	allUIDs := make([]string, len(mail))
	for i, m := range mail {
		allUIDs[i] = m.Uid()
	}
	// readBatchSize caps how many mail uids go into a single "mail.read.status.betch" or
	// "mail.reward.batch" request. maxUIDsBytes additionally caps the byte length of each
	// batch's joined "uids" string, keeping it safely under the wire format's 65535-byte
	// string-length limit -- past that, sfsobject.go's encoder (sfs.WriteUtfString) now returns a
	// clean error rather than panicking, but that's still a batch-encode failure that drops the
	// whole batch for this run, so it's worth avoiding rather than merely surviving.
	// readBatchSize alone isn't enough protection here: a mail uid is a server-supplied string
	// that can itself be up to 65535 bytes, so a handful of large uids -- or a mail backlog with
	// many same-type unclaimed rewards -- can blow the joined length even well under 100 items.
	// Shared by both batch loops below (via batchByCountAndBytes) since both send a comma-joined
	// "uids" field subject to the same limit.
	const (
		readBatchSize = 100
		maxUIDsBytes  = 60000
	)
	offset := 0
	readFailed := false
	// readAbortedByNetErr tracks whether the loop below stopped early because a batch failed with
	// a NON-TIMEOUT net.Error (the underlying connection is known-dead), as opposed to running out
	// of batches normally or stopping only after an ordinary decoded errorCode failure or a
	// Timeout()==true net.Error (sendAndWait's ordinary "no response within defaultCmdTimeout"
	// outcome), neither of which must abort the loop -- see CollectAll's identical distinction in
	// buildings.go. When true, the reward-claim loop further down is skipped entirely: attempting
	// it against an already-dead connection would just burn one more defaultCmdTimeout per
	// remaining batch for no benefit.
	readAbortedByNetErr := false
	for _, batch := range truncateMailBatches(batchByCountAndBytes(allUIDs, readBatchSize, maxUIDsBytes), "read-status") {
		readParams := sfs.NewSFSObject()
		readParams.PutUtfString("uids", strings.Join(batch, ","))
		_, err := session.SendAndWait(conn, fmt.Sprintf("mail read-status (batch %d, size %d)", offset, len(batch)), "mail.read.status.betch", readParams)
		if err != nil {
			readFailed = true
		}
		errs = append(errs, err)
		offset += len(batch)
		var netErr net.Error
		if errors.As(err, &netErr) && !netErr.Timeout() {
			readAbortedByNetErr = true
			break
		}
	}
	// Only claim the full count succeeded if every read-status batch above actually did -- a
	// failure still surfaces via this function's final errors.Join regardless, but this line
	// specifically must not overstate what happened. If any batch failed, log the attempt (not a
	// completed fact) instead, using offset (uids actually submitted in an attempted batch, which
	// still includes the batch that failed) rather than len(allUIDs) -- a net.Error abort above
	// means offset can now be less than the full identified count, and len(allUIDs) would overstate
	// how much was actually attempted in that case.
	if readFailed {
		slog.Warn("mark mail as read had failures", "attempted", offset)
	} else {
		slog.Info("marked mail as read", "count", len(allUIDs))
	}
	if readAbortedByNetErr {
		return errors.Join(errs...)
	}

	byType := groupUnclaimedByType(mail)
	if len(byType) == 0 {
		slog.Info("no unclaimed mail rewards found", "totalMail", len(mail))
		return errors.Join(errs...)
	}
	// mailTypes is collected and sorted before the loop below, mirroring PrintBuildings' identical
	// byType-key-sorting pattern (buildings.go) for the identical reproducibility reason: Go
	// deliberately randomizes map iteration order run-to-run, so iterating byType directly (as this
	// used to) made which alliance-mail types get attempted before a genuine net.Error abort
	// nondeterministic across identical runs. Sorting first makes two runs over identical unclaimed
	// mail byte-for-byte identical in which type's reward-claim batch is attempted, and aborted on,
	// first.
	mailTypes := make([]int32, 0, len(byType))
	for mailType := range byType {
		mailTypes = append(mailTypes, mailType)
	}
	slices.Sort(mailTypes)
	// Round-41 fix: mailTypes was previously unbounded, unlike every sibling sequential
	// network-call loop in this codebase (buildings.go's CollectAll, capped at
	// maxCollectibleBuildingsPerRun=300; visitors.go's GreetVisitors, capped at
	// MaxVisitorsUpperBound=300) -- both explicitly sized so a peer that simply never responds
	// bounds the loop's worst-case wall-clock to ~40 minutes (300 * defaultCmdTimeout=8s) instead
	// of hanging indefinitely. groupUnclaimedByType buckets purely by server-controlled `type`
	// values with no cap of its own, and ListMail's own maxAggregateMailPerFetch=2000 total-entry
	// ceiling is over 6x this loop's 300-iteration sanity margin -- a hostile peer answering with
	// up to 2000 unclaimed-reward mail entries, each a distinct type, could previously force up
	// to ~4.4 hours of sequential mail.reward.batch timeouts from a single crafted response.
	if len(mailTypes) > maxMailRewardTypesPerRun {
		slog.Warn("distinct unclaimed mail reward types exceeds sanity ceiling; truncating reward-claim loop",
			"count", len(mailTypes), "cap", maxMailRewardTypesPerRun)
		mailTypes = mailTypes[:maxMailRewardTypesPerRun]
	}
	// Round-46 fix: totalRewardBatches counts mail.reward.batch round trips SUMMED across every
	// distinct type in mailTypes, checked before each one is sent -- see
	// maxMailRewardBatchesPerRun's own doc comment for why maxMailRewardTypesPerRun (type count)
	// and maxMailBatchesPerLoop (batches per type, reset fresh each iteration) don't bound this on
	// their own.
	totalRewardBatches := 0
rewardLoop:
	for _, mailType := range mailTypes {
		uids := byType[mailType]
		slog.Info("claiming mail reward", "type", mailType, "count", len(uids))
		offset := 0
		for _, batch := range truncateMailBatches(batchByCountAndBytes(uids, readBatchSize, maxUIDsBytes), fmt.Sprintf("reward-claim type=%d", mailType)) {
			if totalRewardBatches >= maxMailRewardBatchesPerRun {
				slog.Warn("total mail.reward.batch round trips across all types exceeds sanity ceiling; stopping reward-claim loop",
					"totalBatches", totalRewardBatches, "cap", maxMailRewardBatchesPerRun)
				break rewardLoop
			}
			totalRewardBatches++
			rewardParams := sfs.NewSFSObject()
			rewardParams.PutUtfString("uids", strings.Join(batch, ","))
			rewardParams.PutInt("type", mailType)
			_, err := session.SendAndWait(conn, fmt.Sprintf("mail reward-batch (type %d, batch %d, size %d)", mailType, offset, len(batch)), "mail.reward.batch", rewardParams)
			errs = append(errs, err)
			offset += len(batch)
			var netErr net.Error
			if errors.As(err, &netErr) && !netErr.Timeout() {
				// A NON-TIMEOUT net.Error means the connection is known-dead: stop this type's
				// remaining batches AND every other still-unprocessed type in byType, same
				// reasoning as the read-status loop above and CollectAll's net.Error early-abort
				// (buildings.go). A labeled break is used (rather than a flag checked at the top
				// of the outer loop) since this needs to exit both the inner batch loop and the
				// outer per-type loop in one step. A Timeout()==true net.Error (sendAndWait's
				// ordinary "no response within defaultCmdTimeout" outcome) is NOT evidence of a
				// dead connection and falls through with err already appended to errs above, same
				// as an ordinary decoded errorCode failure -- see
				// TestClaimAllMailRewardLoopContinuesAfterTimeout.
				break rewardLoop
			}
		}
	}
	return errors.Join(errs...)
}

// truncateMailBatches caps batches at maxMailBatchesPerLoop, warning and dropping the remainder
// when it's exceeded -- see that constant's own doc comment for the full rationale. label
// distinguishes the read-status loop from a specific mail type's reward-claim loop in the
// resulting Warn, since both of ClaimAllMail's batch loops share this same truncation.
func truncateMailBatches(batches [][]string, label string) [][]string {
	if len(batches) > maxMailBatchesPerLoop {
		slog.Warn("mail batch count exceeds sanity ceiling; truncating", "loop", label, "batchCount", len(batches), "cap", maxMailBatchesPerLoop)
		batches = batches[:maxMailBatchesPerLoop]
	}
	return batches
}

// batchByCountAndBytes splits uids into consecutive batches, each capped at maxCount items and
// at maxBytes for the sum of (len(uid)+1) across the batch -- the "+1" accounting for the comma
// that will join them into a single wire string (see ClaimAllMail's readBatchSize/maxUIDsBytes
// doc comment for why both caps are needed). A batch always admits at least one uid even if that
// uid alone exceeds maxBytes, so no single oversized uid can stall the loop forever; the resulting
// over-limit batch is still expected to fail cleanly at encode time downstream rather than being
// silently dropped here.
func batchByCountAndBytes(uids []string, maxCount, maxBytes int) [][]string {
	var batches [][]string
	for i := 0; i < len(uids); {
		end := i
		batchBytes := 0
		for end < len(uids) && end-i < maxCount && (end == i || batchBytes+len(uids[end])+1 <= maxBytes) {
			batchBytes += len(uids[end]) + 1 // +1 for the joining comma
			end++
		}
		batches = append(batches, uids[i:end])
		i = end
	}
	return batches
}

// groupUnclaimedByType buckets mail with an unclaimed reward by its type field -- pulled out of
// ClaimAllMail as a standalone, network-free function so it can be unit tested without a live
// connection.
//
// Same GetInt-can't-distinguish-missing-from-zero conflation HasUnclaimedReward guards against for
// rewardStatus applies here to type: a reward-bearing mail whose `type` field is genuinely absent,
// explicitly null, or present with the wrong concrete SFS type (e.g. sent as a string) would
// otherwise fall through m.Type()'s GetInt to the int32 zero value, indistinguishable from a real
// type=0, and get silently bucketed into (and later sent as) a `mail.reward.batch {type:0, ...}`
// request the server may not recognize -- merging it into a genuinely-type=0 batch it doesn't
// belong to. So this checks presence AND type via the shared requireFieldType guard (same one
// ListMail already uses for uid, and alliance.go/buildings.go use for scienceId/uuid) before
// trusting m.Type(), and skips -- with a warning -- any reward-bearing mail whose type is missing,
// explicitly null, or wrong-typed, rather than defaulting it into a type=0 batch.
func groupUnclaimedByType(mail []Mail) map[int32][]string {
	byType := make(map[int32][]string)
	for _, m := range mail {
		if !m.HasUnclaimedReward() {
			continue
		}
		if !session.RequireFieldType(m.Raw, "type", "mail reward", session.SFSFieldKindInt) {
			continue
		}
		byType[m.Type()] = append(byType[m.Type()], m.Uid())
	}
	return byType
}
