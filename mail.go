package main

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
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

func (m Mail) Uid() string              { return m.Raw.GetString("uid") }
func (m Mail) Type() int32              { return m.Raw.GetInt("type") }
func (m Mail) RewardStatus() int32      { return m.Raw.GetInt("rewardStatus") }
func (m Mail) HasUnclaimedReward() bool { return m.RewardStatus() == 0 }

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
func ListMail(conn *GameConn) []Mail {
	const reqCmd = "chat.get.system.mails"
	const pushCmd = "push.chat.get.system.mails"
	const maxPages = 20

	var all []Mail
	clientseq := ""
	reqTime := int64(0)
	first := true

	for page := 0; page < maxPages; page++ {
		params := NewSFSObject()
		params.PutUtfString("clientseq", clientseq)
		params.PutLong("time", reqTime)
		params.PutInt("count", 100)
		params.PutBool("isAll", true)
		if first {
			params.PutUtfString("firstCmd", "YES")
		}
		if err := conn.SendExtension(reqCmd, params); err != nil {
			slog.Error("list mail send failed", "error", err)
			return all
		}
		msg, err := waitForCmd(conn, 8*time.Second, pushCmd)
		if err != nil {
			slog.Error("list mail no response", "error", err)
			return all
		}
		v, ok := msg.Params.Get("msg")
		if ok {
			if arr, ok := v.Val.(*SFSArray); ok {
				for _, item := range arr.items {
					if mo, ok := item.Val.(*SFSObject); ok {
						all = append(all, Mail{Raw: mo})
					}
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
		clientseq = msg.Params.GetString("lastUid")
		reqTime = msg.Params.GetLong("lastMailTime")
		first = false
	}
	return all
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
func ClaimAllMail(conn *GameConn) {
	mail := ListMail(conn)
	if len(mail) == 0 {
		slog.Info("no mail found")
		return
	}

	allUIDs := make([]string, len(mail))
	for i, m := range mail {
		allUIDs[i] = m.Uid()
	}
	const readBatchSize = 100
	for i := 0; i < len(allUIDs); i += readBatchSize {
		end := min(i+readBatchSize, len(allUIDs))
		readParams := NewSFSObject()
		readParams.PutUtfString("uids", strings.Join(allUIDs[i:end], ","))
		if err := conn.SendExtension("mail.read.status.betch", readParams); err != nil {
			slog.Error("mail read-status send failed", "error", err)
			continue
		}
		readMsg, err := waitForCmd(conn, 8*time.Second, "mail.read.status.betch")
		if err != nil {
			slog.Error("mail read-status no response", "error", err)
			continue
		}
		logCommandResult(fmt.Sprintf("mail read-status response (batch %d, size %d)", i, end-i), readMsg)
	}
	slog.Info("marked mail as read", "count", len(allUIDs))

	byType := make(map[int32][]string)
	for _, m := range mail {
		if m.HasUnclaimedReward() {
			byType[m.Type()] = append(byType[m.Type()], m.Uid())
		}
	}
	if len(byType) == 0 {
		slog.Info("no unclaimed mail rewards found", "totalMail", len(mail))
		return
	}
	for mailType, uids := range byType {
		slog.Info("claiming mail reward", "type", mailType, "count", len(uids))
		rewardParams := NewSFSObject()
		rewardParams.PutUtfString("uids", strings.Join(uids, ","))
		rewardParams.PutInt("type", mailType)
		if err := conn.SendExtension("mail.reward.batch", rewardParams); err != nil {
			slog.Error("mail reward-batch send failed", "error", err)
			continue
		}
		msg, err := waitForCmd(conn, 8*time.Second, "mail.reward.batch")
		if err != nil {
			slog.Error("mail reward-batch no response", "error", err)
			continue
		}
		logCommandResult(fmt.Sprintf("mail reward-batch response (type %d)", mailType), msg)
	}
}
