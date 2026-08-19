package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
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
	Raw *SFSObject
}

func (m Mail) Uid() string         { return m.Raw.GetString("uid") }
func (m Mail) Type() int32         { return m.Raw.GetInt("type") }
func (m Mail) RewardStatus() int32 { return m.Raw.GetInt("rewardStatus") }

// HasUnclaimedReward reports whether this mail has a reward still waiting to be claimed.
// `rewardStatus == 0` is the confirmed "unclaimed" value (docs/live-validation.mdx's Mail
// section), but GetInt can't distinguish a real 0 from a genuinely-absent field -- both come back
// as the int32 zero value. That conflation matters here specifically: notification-only mail
// (alliance markers, battle reports, and similar -- see ClaimAllMail's doc comment) never carries
// a reward at all, and plausibly omits the rewardStatus key entirely rather than sending an
// explicit 0. Treating a missing key as "unclaimed" would misclassify that mail as having a
// reward it doesn't have. So this checks presence via Get first (same explicit-null-vs-missing
// guard used by requirePresentField/findRecommendedTech's scienceId handling in alliance.go) and
// only then compares the value -- a genuinely-absent rewardStatus field reads as "no reward"
// (false), not "unclaimed".
func (m Mail) HasUnclaimedReward() bool {
	v, ok := m.Raw.Get("rewardStatus")
	if !ok || v.Val == nil {
		return false
	}
	return m.RewardStatus() == 0
}

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
func ListMail(conn *GameConn) ([]Mail, error) {
	const reqCmd = "chat.get.system.mails"
	const pushCmd = "push.chat.get.system.mails"
	const maxPages = 20
	const mailListPageSize = 100
	// mailListRawItemCap bounds how many RAW entries in a single page's `msg` response array the
	// loop below will examine, independent of mailListPageSize (100, the requested page-size hint)
	// and maxPages (20) -- both of those bound round-trip COUNT only, not the size of any single
	// page's response array, which is otherwise bounded only by sfsobject.go's much larger
	// maxDecodedNodes=300,000 decode budget. Without this, a malformed entry (not an *SFSObject, or
	// missing the required "uid" field via requirePresentField) hits a `continue` that doesn't
	// advance any output-count-based cap, since it never reaches the append -- the same gap
	// visitors.go's ParseInitVisitors closed in round 26 for visitor.list, applied here to mail.list
	// pages. requirePresentField itself logs a Warn per malformed entry, so a hostile/misbehaving
	// peer responding to a single mail.list page with a huge malformed array would otherwise force
	// full scan-and-log cost regardless of the requested page size. Set comfortably above
	// mailListPageSize=100 -- a legitimate server response may reasonably vary somewhat from the
	// exact requested page size -- but still finite and well below the decode-level ceiling.
	const mailListRawItemCap = 1000

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

	for page := 0; page < maxPages; page++ {
		truncated = false
		params := NewSFSObject()
		params.PutUtfString("clientseq", clientseq)
		params.PutLong("time", reqTime)
		params.PutInt("count", mailListPageSize)
		params.PutBool("isAll", true)
		if first {
			params.PutUtfString("firstCmd", "YES")
		}
		msg, err := sendAndWait(conn, fmt.Sprintf("list mail (page %d)", page), reqCmd, params, pushCmd)
		if err != nil {
			return all, err
		}
		v, ok := msg.Params.Get("msg")
		if ok {
			if arr, ok := v.Val.(*SFSArray); ok {
				if len(arr.items) > mailListRawItemCap {
					slog.Warn("list mail: page response array longer than raw-item scan cap; truncating scan", "page", page, "arrayLen", len(arr.items), "cap", mailListRawItemCap)
				}
				for i, item := range arr.items {
					if i >= mailListRawItemCap {
						break
					}
					mo, ok := item.Val.(*SFSObject)
					if !ok {
						continue
					}
					if !requirePresentField(mo, "uid", "mail") {
						continue
					}
					m := Mail{Raw: mo}
					uid := m.Uid()
					if seenUIDs[uid] {
						continue
					}
					seenUIDs[uid] = true
					all = append(all, m)
				}
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
		clientseq = lastUid
		// lastMailTime is lastUid's sibling cursor field, read the same way and forwarded the
		// same way into the next page's request -- but GetLong can't tell a missing/null
		// lastMailTime apart from a legitimate explicit 0 (both silently return int64(0)),
		// unlike GetString's "" which is never a legitimate mail uid. Left unguarded, a
		// response with a valid lastUid but a missing lastMailTime would silently reset
		// reqTime to the same value as the cold-start request while clientseq keeps advancing
		// normally -- the exact failure shape the lastUid check above exists to prevent, just
		// on its sibling field. Impact is bounded (seenUIDs dedupes any re-fetched mail,
		// maxPages caps the loop), so this doesn't abort pagination like the lastUid check
		// does, but it's still worth surfacing so an operator can tell a run hit this instead
		// of quietly assuming it's a legitimate mail timestamped at the epoch.
		if v, ok := msg.Params.Get("lastMailTime"); !ok || v.Val == nil {
			slog.Warn("list mail: response reported more=true but lastMailTime is missing/null, reqTime will reset to 0 for the next page instead of the real cursor value", "page", page, "collectedSoFar", len(all))
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
func ClaimAllMail(conn *GameConn) error {
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
	// string-length limit -- past that, sfsobject.go's encoder (writeUtfString) now returns a
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
	for _, batch := range batchByCountAndBytes(allUIDs, readBatchSize, maxUIDsBytes) {
		readParams := NewSFSObject()
		readParams.PutUtfString("uids", strings.Join(batch, ","))
		_, err := sendAndWait(conn, fmt.Sprintf("mail read-status (batch %d, size %d)", offset, len(batch)), "mail.read.status.betch", readParams)
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
rewardLoop:
	for mailType, uids := range byType {
		slog.Info("claiming mail reward", "type", mailType, "count", len(uids))
		offset := 0
		for _, batch := range batchByCountAndBytes(uids, readBatchSize, maxUIDsBytes) {
			rewardParams := NewSFSObject()
			rewardParams.PutUtfString("uids", strings.Join(batch, ","))
			rewardParams.PutInt("type", mailType)
			_, err := sendAndWait(conn, fmt.Sprintf("mail reward-batch (type %d, batch %d, size %d)", mailType, offset, len(batch)), "mail.reward.batch", rewardParams)
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
// rewardStatus applies here to type: a reward-bearing mail whose `type` field is genuinely absent
// or explicitly null would otherwise fall through m.Type()'s GetInt to the int32 zero value,
// indistinguishable from a real type=0, and get silently bucketed into (and later sent as) a
// `mail.reward.batch {type:0, ...}` request the server may not recognize. So this checks presence
// via the shared requirePresentField guard (same one ListMail already uses for uid, and
// alliance.go/buildings.go use for scienceId/uuid) before trusting m.Type(), and skips -- with a
// warning -- any reward-bearing mail whose type is missing or explicitly null, rather than
// defaulting it into a type=0 batch.
func groupUnclaimedByType(mail []Mail) map[int32][]string {
	byType := make(map[int32][]string)
	for _, m := range mail {
		if !m.HasUnclaimedReward() {
			continue
		}
		if !requirePresentField(m.Raw, "type", "mail reward") {
			continue
		}
		byType[m.Type()] = append(byType[m.Type()], m.Uid())
	}
	return byType
}
