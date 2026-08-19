package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestMailObj builds the raw *SFSObject shape ListMail parses each `msg` array entry into
// (see requirePresentField's "uid" requirement and the Mail accessors in mail.go).
func newTestMailObj(uid string, mailType, rewardStatus int32) *SFSObject {
	o := NewSFSObject()
	o.PutUtfString("uid", uid)
	o.PutInt("type", mailType)
	o.PutInt("rewardStatus", rewardStatus)
	return o
}

// TestListMailPaginates checks ListMail's pagination loop end to end: a first page with
// `more: true` must trigger a second request seeded from the first page's `lastUid`/
// `lastMailTime`, carrying no `firstCmd` field (only the cold-start request sets that, per
// ListMail's own doc comment), and the final page's `more: false` must stop the loop. The parsed
// result across both pages must match, in order.
func TestListMailPaginates(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type gotReq struct {
		clientseq string
		time      int64
		hasFirst  bool
	}
	var gotReqs []gotReq

	done := make(chan struct{})
	go func() {
		defer close(done)

		readReq := func() (*ExtensionMessage, bool) {
			env, err := server.ReadEnvelope()
			if err != nil {
				return nil, false
			}
			msg, ok := env.AsExtension()
			if !ok {
				return nil, false
			}
			_, hasFirst := msg.Params.Get("firstCmd")
			gotReqs = append(gotReqs, gotReq{msg.Params.GetString("clientseq"), msg.Params.GetLong("time"), hasFirst})
			return msg, true
		}

		msg, ok := readReq()
		if !ok {
			return
		}
		if msg.Cmd != "chat.get.system.mails" {
			t.Errorf("page 1 Cmd = %q, want chat.get.system.mails", msg.Cmd)
		}
		resp1 := NewSFSObject()
		arr1 := NewSFSArray()
		arr1.AddSFSObject(newTestMailObj("uid-1", 3, 0))
		arr1.AddSFSObject(newTestMailObj("uid-2", 4, 1))
		resp1.PutSFSArray("msg", arr1)
		resp1.PutBool("more", true)
		resp1.PutUtfString("lastUid", "uid-2")
		resp1.PutLong("lastMailTime", 555)
		if err := server.SendExtension("push.chat.get.system.mails", resp1); err != nil {
			return
		}

		msg, ok = readReq()
		if !ok {
			return
		}
		if msg.Cmd != "chat.get.system.mails" {
			t.Errorf("page 2 Cmd = %q, want chat.get.system.mails", msg.Cmd)
		}
		resp2 := NewSFSObject()
		arr2 := NewSFSArray()
		arr2.AddSFSObject(newTestMailObj("uid-3", 9, 0))
		resp2.PutSFSArray("msg", arr2)
		resp2.PutBool("more", false)
		if err := server.SendExtension("push.chat.get.system.mails", resp2); err != nil {
			return
		}
	}()

	got, err := ListMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished both pages")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	wantUids := []string{"uid-1", "uid-2", "uid-3"}
	if len(got) != len(wantUids) {
		t.Fatalf("got %d mail entries, want %d", len(got), len(wantUids))
	}
	for i, m := range got {
		if m.Uid() != wantUids[i] {
			t.Errorf("mail[%d].Uid() = %q, want %q", i, m.Uid(), wantUids[i])
		}
	}

	if len(gotReqs) != 2 {
		t.Fatalf("fake server saw %d requests, want 2", len(gotReqs))
	}
	if !gotReqs[0].hasFirst || gotReqs[0].clientseq != "" || gotReqs[0].time != 0 {
		t.Errorf("page 1 request = %+v, want firstCmd present, clientseq=\"\", time=0 (cold start)", gotReqs[0])
	}
	if gotReqs[1].hasFirst {
		t.Errorf("page 2 request has firstCmd set, want it absent -- only the cold-start request sets it")
	}
	if gotReqs[1].clientseq != "uid-2" || gotReqs[1].time != 555 {
		t.Errorf("page 2 request = %+v, want clientseq=\"uid-2\" time=555 (page 1's lastUid/lastMailTime)", gotReqs[1])
	}
}

// TestListMailStopsOnMissingLastUid is the regression test for the round-12 fix to ListMail's
// pagination loop: a response that claims `more: true` but omits `lastUid` must not be treated as
// a valid cursor to keep paginating on. Before the fix, `msg.Params.GetString("lastUid")` silently
// fell through to "" -- indistinguishable from the cold-start clientseq -- so ListMail would
// re-send the exact same first-page request again (and again, up to maxPages times) instead of
// stopping. The fake server here answers exactly one request and then goes silent, so a reverted
// fix would make ListMail try to send a second request that nothing ever answers: SendExtension's
// 10s write deadline (conn.go's writeTimeout) would eventually error it out, but this test's own
// 3s bound catches that long before then and fails with a clear message instead of just going
// slow.
func TestListMailStopsOnMissingLastUid(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		msg, ok := env.AsExtension()
		if !ok {
			return
		}
		if msg.Cmd != "chat.get.system.mails" {
			t.Errorf("Cmd = %q, want chat.get.system.mails", msg.Cmd)
		}
		resp := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(newTestMailObj("uid-1", 3, 0))
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", true)
		// Deliberately no PutUtfString("lastUid", ...) call: `more: true` with a genuinely
		// missing lastUid is exactly the malformed shape under test.
		resp.PutLong("lastMailTime", 999)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
		// Intentionally does not read a second request -- see the test's own doc comment for why
		// that's the point: a correct ListMail never sends one.
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	var got []Mail
	var err error
	listDone := make(chan struct{})
	go func() {
		defer close(listDone)
		got, err = ListMail(client)
	}()

	select {
	case <-listDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ListMail never returned -- it looped on the missing lastUid cursor instead of stopping pagination")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Uid() != "uid-1" {
		t.Fatalf("got %v, want exactly the one page-1 mail entry (uid-1) -- pagination must stop after page 1", got)
	}
	if logged := buf.String(); !strings.Contains(logged, "lastUid") {
		t.Errorf("expected a warning mentioning lastUid when more=true but lastUid is missing, got log:\n%s", logged)
	}
}

// TestListMailWarnsOnMaxPagesTruncation is the regression test for the round-14 fix to ListMail's
// pagination loop: if the loop exhausts all maxPages requests while the server's last response
// still reported more=true (with a perfectly valid lastUid each time -- this is NOT the
// lastUid-missing anomaly TestListMailStopsOnMissingLastUid covers), the collected mail list is
// silently truncated. Before the fix, this exit path fell straight through to `return all, nil`
// with zero logging, unlike the lastUid-missing early-exit which already warns. The fake server
// here always answers with more=true and a fresh, incrementing lastUid, for every one of the
// maxPages(=20) requests ListMail is allowed to send, so the loop can only stop by running out of
// pages -- never by seeing more=false or a missing lastUid. This confirms both (a) ListMail still
// returns after exactly maxPages requests instead of looping forever, and (b) it now emits a
// warning identifying itself as a maxPages truncation, mentioning maxPages and how much mail was
// collected before it gave up.
func TestListMailWarnsOnMaxPagesTruncation(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const maxPages = 20 // must match ListMail's own unexported maxPages constant

	var reqCount int
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for page := 0; page < maxPages; page++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok {
				return
			}
			if msg.Cmd != "chat.get.system.mails" {
				t.Errorf("page %d Cmd = %q, want chat.get.system.mails", page, msg.Cmd)
			}
			reqCount++
			resp := NewSFSObject()
			arr := NewSFSArray()
			arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-page%d", page), 3, 0))
			resp.PutSFSArray("msg", arr)
			resp.PutBool("more", true) // always more -- the server never runs out on its own
			resp.PutUtfString("lastUid", fmt.Sprintf("cursor-%d", page))
			resp.PutLong("lastMailTime", int64(page))
			if err := server.SendExtension("push.chat.get.system.mails", resp); err != nil {
				return
			}
		}
		// Intentionally does not read a (maxPages+1)th request -- see the test's own doc comment
		// for why that's the point: a correct ListMail stops after exactly maxPages requests.
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	var got []Mail
	var err error
	listDone := make(chan struct{})
	go func() {
		defer close(listDone)
		got, err = ListMail(client)
	}()

	select {
	case <-listDone:
	case <-time.After(3 * time.Second):
		t.Fatal("ListMail never returned -- it should stop after exactly maxPages requests")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if reqCount != maxPages {
		t.Fatalf("fake server saw %d requests, want exactly maxPages=%d", reqCount, maxPages)
	}
	if len(got) != maxPages {
		t.Fatalf("got %d mail entries, want exactly maxPages=%d (one per page)", len(got), maxPages)
	}

	logged := buf.String()
	if !strings.Contains(logged, "maxPages") {
		t.Errorf("expected a warning mentioning maxPages when pagination is truncated by the page cap, got log:\n%s", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("%d", maxPages)) {
		t.Errorf("expected the warning to include the maxPages value (%d), got log:\n%s", maxPages, logged)
	}
	if !strings.Contains(logged, "collectedSoFar") || !strings.Contains(logged, fmt.Sprintf("%d", len(got))) {
		t.Errorf("expected the warning to include collectedSoFar=%d, got log:\n%s", len(got), logged)
	}
}

// mailBatchServer is the shared fake-server shape for the ClaimAllMail batching tests below: it
// answers exactly one ListMail request with all of mails in a single page, then answers
// wantReadBatches read-status batches and wantRewardBatches reward-claim batches, recording each
// batch's uids (split back out of the joined "uids" string) so the test can assert the batch
// boundaries and that no uid was dropped or duplicated.
func mailBatchServer(t *testing.T, server *GameConn, mails []*SFSObject, wantReadBatches, wantRewardBatches int) (readBatches, rewardBatches [][]string, rewardType int32) {
	t.Helper()

	env, err := server.ReadEnvelope()
	if err != nil {
		t.Errorf("read list-mail request: %v", err)
		return nil, nil, 0
	}
	msg, ok := env.AsExtension()
	if !ok {
		t.Errorf("list-mail request: AsExtension() = false")
		return nil, nil, 0
	}
	if msg.Cmd != "chat.get.system.mails" {
		t.Errorf("list-mail request: Cmd = %q, want chat.get.system.mails", msg.Cmd)
		return nil, nil, 0
	}
	listResp := NewSFSObject()
	arr := NewSFSArray()
	for _, mo := range mails {
		arr.AddSFSObject(mo)
	}
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)
	if err := server.SendExtension("push.chat.get.system.mails", listResp); err != nil {
		t.Errorf("send list-mail response: %v", err)
		return nil, nil, 0
	}

	readUids := func(cmd string) []string {
		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read %s request: %v", cmd, err)
			return nil
		}
		msg, ok := env.AsExtension()
		if !ok {
			t.Errorf("%s request: AsExtension() = false", cmd)
			return nil
		}
		if msg.Cmd != cmd {
			t.Errorf("%s request: Cmd = %q, want %s", cmd, msg.Cmd, cmd)
			return nil
		}
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendExtension(cmd, resp); err != nil {
			t.Errorf("send %s response: %v", cmd, err)
			return nil
		}
		uids := msg.Params.GetString("uids")
		if uids == "" {
			return nil
		}
		return strings.Split(uids, ",")
	}

	for i := 0; i < wantReadBatches; i++ {
		readBatches = append(readBatches, readUids("mail.read.status.betch"))
	}
	for i := 0; i < wantRewardBatches; i++ {
		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read mail.reward.batch request: %v", err)
			return readBatches, rewardBatches, rewardType
		}
		msg, ok := env.AsExtension()
		if !ok {
			t.Errorf("mail.reward.batch request: AsExtension() = false")
			return readBatches, rewardBatches, rewardType
		}
		if msg.Cmd != "mail.reward.batch" {
			t.Errorf("mail.reward.batch request: Cmd = %q, want mail.reward.batch", msg.Cmd)
			return readBatches, rewardBatches, rewardType
		}
		rewardType = msg.Params.GetInt("type")
		resp := NewSFSObject()
		resp.PutBool("success", true)
		if err := server.SendExtension("mail.reward.batch", resp); err != nil {
			t.Errorf("send mail.reward.batch response: %v", err)
			return readBatches, rewardBatches, rewardType
		}
		uids := msg.Params.GetString("uids")
		var batch []string
		if uids != "" {
			batch = strings.Split(uids, ",")
		}
		rewardBatches = append(rewardBatches, batch)
	}
	return readBatches, rewardBatches, rewardType
}

// assertBatchesCoverExactly checks that the given batches, concatenated in order, contain exactly
// wantUids with no uid dropped, duplicated, or reordered -- and that the individual batch sizes
// match wantSizes, proving the split happened at the expected boundary rather than some other one.
func assertBatchesCoverExactly(t *testing.T, label string, batches [][]string, wantSizes []int, wantUids []string) {
	t.Helper()
	if len(batches) != len(wantSizes) {
		t.Fatalf("%s: got %d batches, want %d", label, len(batches), len(wantSizes))
	}
	var flat []string
	for i, b := range batches {
		if len(b) != wantSizes[i] {
			t.Errorf("%s: batch %d size = %d, want %d", label, i, len(b), wantSizes[i])
		}
		flat = append(flat, b...)
	}
	if len(flat) != len(wantUids) {
		t.Fatalf("%s: got %d total uids across batches, want %d", label, len(flat), len(wantUids))
	}
	for i, uid := range wantUids {
		if flat[i] != uid {
			t.Errorf("%s: uid[%d] = %q, want %q (dropped/duplicated/reordered uid)", label, i, flat[i], uid)
		}
	}
}

// TestClaimAllMailItemCountBatching forces the readBatchSize=100 item-count cap: 101 same-type
// unclaimed mail entries with short uids (well under the byte cap) must split into a 100-item
// batch followed by a 1-item batch, for both the read-status loop and the reward-claim loop.
func TestClaimAllMailItemCountBatching(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const total = 101
	const mailType = int32(7)
	var wantUids []string
	var mails []*SFSObject
	for i := 0; i < total; i++ {
		uid := fmt.Sprintf("uid-%03d", i)
		wantUids = append(wantUids, uid)
		mails = append(mails, newTestMailObj(uid, mailType, 0)) // rewardStatus=0: unclaimed
	}

	var readBatches, rewardBatches [][]string
	var rewardType int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		readBatches, rewardBatches, rewardType = mailBatchServer(t, server, mails, 2, 2)
	}()

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all batches")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardType != mailType {
		t.Errorf("reward-batch type = %d, want %d", rewardType, mailType)
	}
	assertBatchesCoverExactly(t, "read-status", readBatches, []int{100, 1}, wantUids)
	assertBatchesCoverExactly(t, "reward-claim", rewardBatches, []int{100, 1}, wantUids)
}

// TestClaimAllMailByteLengthBatching forces the maxUIDsBytes=60000 byte-length cap well under the
// 100-item count cap: 11 same-type unclaimed mail entries with 6499-byte uids (6500 bytes each
// once the joining comma is counted) must split 9-then-2, since a 10th uid would push the batch
// to 65000 > 60000 bytes. Exercises both the read-status loop and the reward-claim loop, which
// share the same cap after this round's fix.
func TestClaimAllMailByteLengthBatching(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const total = 11
	const uidLen = 6499
	const mailType = int32(5)
	var wantUids []string
	var mails []*SFSObject
	for i := 0; i < total; i++ {
		prefix := fmt.Sprintf("%05d-", i)
		uid := prefix + strings.Repeat("x", uidLen-len(prefix))
		if len(uid) != uidLen {
			t.Fatalf("test setup bug: uid %d has length %d, want %d", i, len(uid), uidLen)
		}
		wantUids = append(wantUids, uid)
		mails = append(mails, newTestMailObj(uid, mailType, 0)) // rewardStatus=0: unclaimed
	}

	var readBatches, rewardBatches [][]string
	var rewardType int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		readBatches, rewardBatches, rewardType = mailBatchServer(t, server, mails, 2, 2)
	}()

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all batches")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardType != mailType {
		t.Errorf("reward-batch type = %d, want %d", rewardType, mailType)
	}
	assertBatchesCoverExactly(t, "read-status", readBatches, []int{9, 2}, wantUids)
	assertBatchesCoverExactly(t, "reward-claim", rewardBatches, []int{9, 2}, wantUids)
}

// TestClaimAllMailProcessesPartialMailOnListPageFailure is the regression test for the round-16
// fix to ClaimAllMail's handling of a ListMail error: ListMail deliberately returns whatever mail
// it already collected before a mid-pagination sendAndWait failure (see its own doc comment), not
// nil, specifically so a transient failure on a later page doesn't have to cost the caller
// already-identified earlier mail. Before the fix, ClaimAllMail returned immediately on
// `err != nil` without ever looking at the `mail` slice ListMail still handed back, discarding
// page 1's fully-fetched mail and claiming nothing for the run.
//
// The fake server here answers page 1 of chat.get.system.mails normally (more=true, two mail
// entries, one with an unclaimed reward), then answers page 2 with a genuine (non-benign)
// errorCode instead of a valid page -- a real sendAndWait failure via classifyResponse's
// outcomeFailure path, not the lastUid-missing anomaly or a benign no-op. ClaimAllMail must still
// mark page 1's two mail entries as read and claim its one unclaimed reward, and its returned
// error must mention the page-2 failure.
func TestClaimAllMailProcessesPartialMailOnListPageFailure(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	readReq := func(wantCmd string) *ExtensionMessage {
		t.Helper()
		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read %s request: %v", wantCmd, err)
			return nil
		}
		msg, ok := env.AsExtension()
		if !ok {
			t.Errorf("%s request: AsExtension() = false", wantCmd)
			return nil
		}
		if msg.Cmd != wantCmd {
			t.Errorf("request Cmd = %q, want %s", msg.Cmd, wantCmd)
		}
		return msg
	}

	var readStatusUIDs, rewardUIDs []string
	var rewardType int32
	done := make(chan struct{})
	go func() {
		defer close(done)

		// Page 1: succeeds, two mail entries, more=true so ListMail queues a second page.
		if readReq("chat.get.system.mails") == nil {
			return
		}
		resp1 := NewSFSObject()
		arr1 := NewSFSArray()
		arr1.AddSFSObject(newTestMailObj("uid-1", 3, 0)) // rewardStatus=0: unclaimed reward
		arr1.AddSFSObject(newTestMailObj("uid-2", 4, 1)) // rewardStatus=1: already claimed/no reward
		resp1.PutSFSArray("msg", arr1)
		resp1.PutBool("more", true)
		resp1.PutUtfString("lastUid", "uid-2")
		resp1.PutLong("lastMailTime", 555)
		if err := server.SendExtension("push.chat.get.system.mails", resp1); err != nil {
			return
		}

		// Page 2: a genuine (non-benign) failure -- classifyResponse's outcomeFailure path, not
		// the lastUid-missing anomaly TestListMailStopsOnMissingLastUid covers.
		if readReq("chat.get.system.mails") == nil {
			return
		}
		resp2 := NewSFSObject()
		resp2.PutUtfString("errorCode", "999999") // not in benignErrorCodes: a genuine failure
		if err := server.SendExtension("push.chat.get.system.mails", resp2); err != nil {
			return
		}

		// ClaimAllMail must still process page 1's two mail entries despite the page-2 failure.
		if msg := readReq("mail.read.status.betch"); msg != nil {
			if uids := msg.Params.GetString("uids"); uids != "" {
				readStatusUIDs = strings.Split(uids, ",")
			}
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", resp); err != nil {
				return
			}
		}
		if msg := readReq("mail.reward.batch"); msg != nil {
			rewardType = msg.Params.GetInt("type")
			if uids := msg.Params.GetString("uids"); uids != "" {
				rewardUIDs = strings.Split(uids, ",")
			}
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all expected requests")
	}

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (page 2's genuine list-mail failure must be reported)")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("ClaimAllMail() error = %v, want it to mention the page-2 failure's errorCode 999999", err)
	}

	wantUIDs := []string{"uid-1", "uid-2"}
	if len(readStatusUIDs) != len(wantUIDs) {
		t.Fatalf("read-status uids = %v, want %v -- page 1's mail must still be marked read despite the page-2 failure", readStatusUIDs, wantUIDs)
	}
	for i, uid := range wantUIDs {
		if readStatusUIDs[i] != uid {
			t.Errorf("read-status uids[%d] = %q, want %q", i, readStatusUIDs[i], uid)
		}
	}

	if len(rewardUIDs) != 1 || rewardUIDs[0] != "uid-1" {
		t.Fatalf("reward-claim uids = %v, want [\"uid-1\"] -- page 1's one unclaimed reward must still be claimed", rewardUIDs)
	}
	if rewardType != 3 {
		t.Errorf("reward-claim type = %d, want 3 (uid-1's type)", rewardType)
	}
}

// TestClaimAllMailReadStatusFailureLoggingDoesNotOverclaim is the regression test for the
// round-16 fix to ClaimAllMail's "marked mail as read" log line: it used to log the full mail
// count unconditionally right after the read-status batch loop, regardless of whether any batch
// in that loop actually failed -- so it always claimed the full count succeeded even when it
// didn't (the failure still surfaced via the function's final errors.Join, just not from this
// specific log line). The fake server here answers ListMail's single page normally, then answers
// the one read-status batch with a genuine (non-benign) failure. The success-claiming "marked
// mail as read" log line must not appear.
func TestClaimAllMailReadStatusFailureLoggingDoesNotOverclaim(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)

		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read list-mail request: %v", err)
			return
		}
		msg, ok := env.AsExtension()
		if !ok || msg.Cmd != "chat.get.system.mails" {
			t.Errorf("list-mail request malformed: %+v ok=%v", msg, ok)
			return
		}
		resp := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(newTestMailObj("uid-1", 3, 1)) // rewardStatus=1: no reward to claim
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", false)
		if err := server.SendExtension("push.chat.get.system.mails", resp); err != nil {
			return
		}

		env, err = server.ReadEnvelope()
		if err != nil {
			t.Errorf("read mail.read.status.betch request: %v", err)
			return
		}
		msg, ok = env.AsExtension()
		if !ok || msg.Cmd != "mail.read.status.betch" {
			t.Errorf("read-status request malformed: %+v ok=%v", msg, ok)
			return
		}
		readResp := NewSFSObject()
		readResp.PutUtfString("errorCode", "999999") // genuine failure, not benign
		if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
			return
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all expected requests")
	}

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the read-status batch got a genuine failure)")
	}

	if logged := buf.String(); strings.Contains(logged, "marked mail as read") {
		t.Errorf("log claims \"marked mail as read\" (a completed fact) despite the read-status batch failing:\n%s", logged)
	}
}

// recordingConn is a minimal net.Conn whose Write appends every byte to buf and whose Read always
// fails with io.EOF (it is never actually read from in this file -- Read only exists to satisfy the
// net.Conn interface). It exists purely to obtain the exact wire-format bytes SendExtension/
// EncodeObject produce for a canned response, without needing a live goroutine-synchronized
// net.Pipe round trip: wrap it in a bare &GameConn{conn: rec} (SendExtension/SendEnvelope only ever
// touch GameConn.conn, never GameConn.reader, so a nil reader is fine here) and call SendExtension
// on that to fill buf, then hand buf.Bytes() to scriptedNetErrConn below.
type recordingConn struct {
	buf bytes.Buffer
}

func (c *recordingConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *recordingConn) Write(p []byte) (int, error)      { return c.buf.Write(p) }
func (c *recordingConn) Close() error                     { return nil }
func (c *recordingConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *recordingConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *recordingConn) SetDeadline(time.Time) error      { return nil }
func (c *recordingConn) SetReadDeadline(time.Time) error  { return nil }
func (c *recordingConn) SetWriteDeadline(time.Time) error { return nil }

// scriptedNetErrConn serves pre-recorded bytes (typically produced via recordingConn above, wired
// through a real SendExtension call so the bytes are genuinely wire-format-correct) for as many
// Read calls as it takes to drain them, then permanently flips to returning fakeNetError (borrowed
// from buildings_orchestration_test.go's fakeNetErrConn/fakeNetError/fakeNetAddr -- same package,
// so directly visible here) for every Read call after that.
//
// This is what lets TestClaimAllMailAbortsRemainingBatchesOnNetError below script "the ListMail
// round trip succeeds, then the connection goes dead" without either (a) a live net.Pipe server
// goroutine that would need to duck out mid-conversation in a way that still produces a genuine
// net.Error rather than a plain io.ErrClosedPipe/io.EOF (closing a net.Pipe end does the latter,
// not the former -- see FetchBuildings' own real-I/O-error-vs-net.Error distinction in
// buildings.go), or (b) waiting out a real defaultCmdTimeout (conn.go's plain 8*time.Second const,
// not overridable from a test -- see TestSendAndWaitTimeoutNoResponse's doc comment in
// conn_wait_test.go for why that's established as too slow for this test file's fast/deterministic
// bar). Writes always succeed and are counted, exactly like fakeNetErrConn, so a test can prove
// exactly how many requests were sent before ClaimAllMail's net.Error early-abort fired.
type scriptedNetErrConn struct {
	mu     sync.Mutex
	remain []byte
	writes int
}

func (c *scriptedNetErrConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.remain) == 0 {
		return 0, fakeNetError{}
	}
	n := copy(p, c.remain)
	c.remain = c.remain[n:]
	return n, nil
}

func (c *scriptedNetErrConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(p), nil
}

func (c *scriptedNetErrConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *scriptedNetErrConn) Close() error                     { return nil }
func (c *scriptedNetErrConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *scriptedNetErrConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *scriptedNetErrConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedNetErrConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedNetErrConn) SetWriteDeadline(time.Time) error { return nil }

// TestClaimAllMailAbortsRemainingBatchesOnNetError is the round-17 regression test for Fix 1:
// ClaimAllMail's read-status batch loop (mail.go) must check for a net.Error and break instead of
// attempting every remaining batch, mirroring CollectAll's identical check in buildings.go (see
// TestCollectAllAbortsRemainingActionsOnNetError, buildings_orchestration_test.go). It must also
// skip the reward-claim loop entirely once that happens, rather than attempting it against an
// already-known-dead connection.
//
// Unlike TestCollectAllAbortsRemainingActionsOnNetError, this can't just hand ClaimAllMail a
// fakeNetErrConn whose every Read fails from the very first call: ClaimAllMail's first network
// action is ListMail, and that has to genuinely succeed (returning real mail) before there's
// anything for the read-status/reward-claim batch loops to even iterate over. So this uses
// scriptedNetErrConn instead: the ListMail round trip gets a real, valid canned response (150
// same-type unclaimed-reward mail entries -- enough that batchByCountAndBytes' readBatchSize=100
// item cap splits them into two read-status batches, 100 then 50, and would likewise split the
// reward-claim loop's one distinct type into two batches if that loop were ever reached), and every
// Read after that (i.e., every batch call, in either loop) fails immediately with a net.Error.
//
// If the fix fires correctly, exactly 2 writes happen: the ListMail request, then the first (and
// only) read-status batch request, which fails and breaks the loop before batch 2 is ever
// attempted -- and the reward-claim loop (which would otherwise find all 150 mail entries
// unclaimed, same type) never starts at all.
//
// Mutation check, isolating each half of the fix: dropping only the read-status loop's `break`
// (while leaving `readAbortedByNetErr = true` and the post-loop skip-reward-loop check both intact)
// still attempts read-status batch 2 -- which also fails with a net.Error and re-sets the same
// already-true flag -- before the skip check correctly skips the reward loop, so this shows up as
// writeCount()==3, not 2. Dropping only the post-loop `if readAbortedByNetErr { return ... }` skip
// (while leaving both loops' own per-batch break intact) lets groupUnclaimedByType run and the
// reward loop start, but that loop's own net.Error break still limits it to exactly one attempted
// batch, so this also shows up as writeCount()==3. Reverting the whole fix back to the old
// unconditional-append-no-break shape in both loops shows up as writeCount()==5 (list-mail + both
// read-status batches + both reward-claim batches, every one of which independently fails against
// the same always-erroring connection but none of which stops the loop).
func TestClaimAllMailAbortsRemainingBatchesOnNetError(t *testing.T) {
	rec := &recordingConn{}
	recorder := &GameConn{conn: rec}

	const total = 150
	const mailType = int32(3)
	listResp := NewSFSObject()
	arr := NewSFSArray()
	for i := 0; i < total; i++ {
		arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-%03d", i), mailType, 0)) // rewardStatus=0: unclaimed
	}
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)
	if err := recorder.SendExtension("push.chat.get.system.mails", listResp); err != nil {
		t.Fatalf("build canned list-mail response: %v", err)
	}

	fake := &scriptedNetErrConn{remain: rec.buf.Bytes()}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the read-status batch call fails with a net.Error)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a net.Error (the failure that triggered the abort)", err)
	}
	if got := fake.writeCount(); got != 2 {
		t.Errorf("fake connection saw %d writes, want exactly 2 (list-mail request + first read-status batch only -- ClaimAllMail should have aborted before read-status batch 2 or any reward-claim batch)", got)
	}
}

// TestClaimAllMailSkipsReadStatusOnListMailNetError is the round-18 regression test for
// ClaimAllMail's ListMail-net.Error check (mail.go): immediately after the ListMail call,
// ClaimAllMail now checks whether ListMail's own returned error is itself a net.Error and, if so,
// skips straight to returning errors.Join(errs...) -- rather than falling through to the
// len(mail) == 0 check and, since ListMail deliberately returns whatever partial mail it already
// collected before a mid-pagination net.Error (see ListMail's own doc comment), proceeding into the
// read-status batch loop and issuing at least one more sendAndWait against the already-known-dead
// connection before that loop's own separate net.Error check (proven by
// TestClaimAllMailAbortsRemainingBatchesOnNetError above) would have caught it one batch too late.
//
// An ordinary decoded errorCode failure (not a net.Error) on ListMail must still fall through to
// process any partial mail normally -- that's TestClaimAllMailProcessesPartialMailOnListPageFailure's
// existing coverage above, deliberately left unchanged by this fix.
//
// Uses recordingConn/scriptedNetErrConn the same way TestClaimAllMailAbortsRemainingBatchesOnNetError
// does, but scripted the other way around: page 1 of chat.get.system.mails gets a real, valid canned
// response carrying one unclaimed-reward mail entry with more=true (so ListMail queues a second
// page), and every Read after that -- i.e., page 2's round trip -- fails immediately with a
// net.Error. ListMail therefore returns that one already-collected mail entry alongside the
// net.Error it hit fetching page 2, exactly the "partial mail + a net.Error" shape this fix must
// react to.
//
// If the fix fires correctly, exactly 2 writes happen: the page-1 and page-2
// chat.get.system.mails requests (both inside ListMail itself). No mail.read.status.betch request
// is ever sent, despite ClaimAllMail having one real, already-known mail uid in hand that the old
// (pre-fix) code would have tried to mark read anyway.
//
// Mutation check: reverting the fix (removing the net.Error check right after the ListMail call,
// or removing its early return) makes ClaimAllMail fall through to the read-status batch loop,
// which then issues one more write (the read-status batch request) before its own separate
// net.Error check aborts it -- showing up as writeCount() == 3, not 2.
func TestClaimAllMailSkipsReadStatusOnListMailNetError(t *testing.T) {
	rec := &recordingConn{}
	recorder := &GameConn{conn: rec}

	page1Resp := NewSFSObject()
	arr := NewSFSArray()
	arr.AddSFSObject(newTestMailObj("uid-1", 3, 0)) // rewardStatus=0: unclaimed reward
	page1Resp.PutSFSArray("msg", arr)
	page1Resp.PutBool("more", true)
	page1Resp.PutUtfString("lastUid", "uid-1")
	page1Resp.PutLong("lastMailTime", 555)
	if err := recorder.SendExtension("push.chat.get.system.mails", page1Resp); err != nil {
		t.Fatalf("build canned page-1 list-mail response: %v", err)
	}

	fake := &scriptedNetErrConn{remain: rec.buf.Bytes()}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (page 2's list-mail round trip fails with a net.Error)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a net.Error (the ListMail failure that triggered the skip)", err)
	}
	if got := fake.writeCount(); got != 2 {
		t.Errorf("fake connection saw %d writes, want exactly 2 (page-1 and page-2 chat.get.system.mails requests only -- ClaimAllMail should have skipped straight to returning after ListMail's own net.Error, without issuing any read-status batch call)", got)
	}
}

// TestClaimAllMailClaimsRewardsForEachDistinctType is the round-17 regression test for Fix 2: every
// existing ClaimAllMail test's fixture data happens to produce at most one distinct type in byType,
// so the intended cross-type behavior of the per-mail-type reward-claim loop (mail.go, iterating
// byType) had zero direct test coverage even though it looked correct by inspection. This uses two
// distinct unclaimed-reward mail types (3 and 9) and asserts a separate mail.reward.batch request --
// carrying the correct type field and exactly that type's uids, in original list order -- is sent
// for each, proving the loop genuinely iterates every entry in byType rather than, say, sending only
// one batch total or merging every type's uids into a single request.
func TestClaimAllMailClaimsRewardsForEachDistinctType(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	mails := []*SFSObject{
		newTestMailObj("t3-a", 3, 0),
		newTestMailObj("t3-b", 3, 0),
		newTestMailObj("t9-a", 9, 0),
		newTestMailObj("t9-b", 9, 0),
		newTestMailObj("t9-c", 9, 0),
	}
	wantByType := map[int32][]string{
		3: {"t3-a", "t3-b"},
		9: {"t9-a", "t9-b", "t9-c"},
	}

	gotByType := make(map[int32][]string)
	done := make(chan struct{})
	go func() {
		defer close(done)

		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read list-mail request: %v", err)
			return
		}
		msg, ok := env.AsExtension()
		if !ok || msg.Cmd != "chat.get.system.mails" {
			t.Errorf("list-mail request malformed: %+v ok=%v", msg, ok)
			return
		}
		listResp := NewSFSObject()
		arr := NewSFSArray()
		for _, mo := range mails {
			arr.AddSFSObject(mo)
		}
		listResp.PutSFSArray("msg", arr)
		listResp.PutBool("more", false)
		if err := server.SendExtension("push.chat.get.system.mails", listResp); err != nil {
			return
		}

		// One read-status batch covering all 5 uids (well under readBatchSize=100).
		env, err = server.ReadEnvelope()
		if err != nil {
			t.Errorf("read read-status request: %v", err)
			return
		}
		if msg, ok = env.AsExtension(); !ok || msg.Cmd != "mail.read.status.betch" {
			t.Errorf("read-status request malformed: %+v ok=%v", msg, ok)
			return
		}
		readResp := NewSFSObject()
		readResp.PutBool("success", true)
		if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
			return
		}

		// Two reward-claim batches, one per distinct type -- byType map iteration order is not
		// guaranteed by Go, so this reads whichever type comes first without assuming an order.
		for i := 0; i < len(wantByType); i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read mail.reward.batch request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.reward.batch" {
				t.Errorf("mail.reward.batch request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			mailType := msg.Params.GetInt("type")
			var uids []string
			if s := msg.Params.GetString("uids"); s != "" {
				uids = strings.Split(s, ",")
			}
			gotByType[mailType] = uids
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all expected requests")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if len(gotByType) != len(wantByType) {
		t.Fatalf("server saw mail.reward.batch requests for %d distinct types, want %d: got %v", len(gotByType), len(wantByType), gotByType)
	}
	for mailType, wantUids := range wantByType {
		gotUids, ok := gotByType[mailType]
		if !ok {
			t.Errorf("no mail.reward.batch request seen for type %d", mailType)
			continue
		}
		if len(gotUids) != len(wantUids) {
			t.Errorf("type %d: got uids %v, want %v", mailType, gotUids, wantUids)
			continue
		}
		for i, uid := range wantUids {
			if gotUids[i] != uid {
				t.Errorf("type %d uids[%d] = %q, want %q", mailType, i, gotUids[i], uid)
			}
		}
	}
}

// TestClaimAllMailRewardLoopContinuesAcrossTypesAfterBusinessError proves the reward-claim loop's
// existing no-short-circuit-on-business-errors behavior (an ordinary decoded errorCode failure gets
// appended to errs with no break, unlike this round's new net.Error break -- see mail.go's
// ClaimAllMail) actually holds ACROSS distinct mail types, not merely within one type's own internal
// batches the way TestClaimAllMailItemCountBatching/TestClaimAllMailByteLengthBatching already cover
// for a single type. The fake server answers list-mail and read-status normally, then fails
// whichever mail.reward.batch request it receives first with a genuine (non-benign) errorCode --
// byType map iteration order is randomized by Go, so this deliberately does not assume which of the
// two types' requests arrives first -- and answers the second (whichever type that turns out to be)
// with success. Both requests must still be sent (proving the loop didn't stop after the first
// type's failure), and the aggregated error must mention the failure.
func TestClaimAllMailRewardLoopContinuesAcrossTypesAfterBusinessError(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	mails := []*SFSObject{
		newTestMailObj("t3-a", 3, 0),
		newTestMailObj("t9-a", 9, 0),
	}

	var seenTypes []int32
	done := make(chan struct{})
	go func() {
		defer close(done)

		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read list-mail request: %v", err)
			return
		}
		if msg, ok := env.AsExtension(); !ok || msg.Cmd != "chat.get.system.mails" {
			t.Errorf("list-mail request malformed")
			return
		}
		listResp := NewSFSObject()
		arr := NewSFSArray()
		for _, mo := range mails {
			arr.AddSFSObject(mo)
		}
		listResp.PutSFSArray("msg", arr)
		listResp.PutBool("more", false)
		if err := server.SendExtension("push.chat.get.system.mails", listResp); err != nil {
			return
		}

		env, err = server.ReadEnvelope()
		if err != nil {
			t.Errorf("read read-status request: %v", err)
			return
		}
		if msg, ok := env.AsExtension(); !ok || msg.Cmd != "mail.read.status.betch" {
			t.Errorf("read-status request malformed")
			return
		}
		readResp := NewSFSObject()
		readResp.PutBool("success", true)
		if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
			return
		}

		for i := 0; i < 2; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read mail.reward.batch request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.reward.batch" {
				t.Errorf("mail.reward.batch request %d malformed", i)
				return
			}
			seenTypes = append(seenTypes, msg.Params.GetInt("type"))
			resp := NewSFSObject()
			if i == 0 {
				resp.PutUtfString("errorCode", "999999") // genuine failure, not benign -- whichever type this is
			} else {
				resp.PutBool("success", true)
			}
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server never finished all expected requests")
	}

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the first reward-batch request got a genuine failure)")
	}
	if !strings.Contains(err.Error(), "999999") {
		t.Errorf("ClaimAllMail() error = %v, want it to mention the reward-batch failure's errorCode 999999", err)
	}
	if len(seenTypes) != 2 {
		t.Fatalf("server saw %d mail.reward.batch requests, want 2 -- the loop must not stop after the first type's failure", len(seenTypes))
	}
	if seenTypes[0] == seenTypes[1] {
		t.Fatalf("both mail.reward.batch requests used the same type %d, want one request each for types 3 and 9", seenTypes[0])
	}
	gotTypes := map[int32]bool{seenTypes[0]: true, seenTypes[1]: true}
	if !gotTypes[3] || !gotTypes[9] {
		t.Errorf("mail.reward.batch types seen = %v, want exactly {3, 9}", seenTypes)
	}
}
