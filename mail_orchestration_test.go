package main

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
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
