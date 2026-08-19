package main

import (
	"errors"
	"fmt"
	"log/slog"
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

	var all []Mail
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
				for _, item := range arr.items {
					mo, ok := item.Val.(*SFSObject)
					if !ok {
						continue
					}
					if !requirePresentField(mo, "uid", "mail") {
						continue
					}
					all = append(all, Mail{Raw: mo})
				}
			}
		}
		more := false
		if mv, ok := msg.Params.Get("more"); ok {
			if b, ok := mv.Val.(bool); ok {
				more = b
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
func ClaimAllMail(conn *GameConn) error {
	mail, err := ListMail(conn)
	if err != nil {
		return fmt.Errorf("list mail: %w", err)
	}
	if len(mail) == 0 {
		slog.Info("no mail found")
		return nil
	}

	var errs []error

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
	for _, batch := range batchByCountAndBytes(allUIDs, readBatchSize, maxUIDsBytes) {
		readParams := NewSFSObject()
		readParams.PutUtfString("uids", strings.Join(batch, ","))
		_, err := sendAndWait(conn, fmt.Sprintf("mail read-status (batch %d, size %d)", offset, len(batch)), "mail.read.status.betch", readParams)
		errs = append(errs, err)
		offset += len(batch)
	}
	slog.Info("marked mail as read", "count", len(allUIDs))

	byType := groupUnclaimedByType(mail)
	if len(byType) == 0 {
		slog.Info("no unclaimed mail rewards found", "totalMail", len(mail))
		return errors.Join(errs...)
	}
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
