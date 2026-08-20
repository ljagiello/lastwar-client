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

// TestListMailStopsOnOversizedLastUid is the round-46 regression test for the MAJOR finding that
// ListMail's pagination cursor (the server-supplied lastUid field) had no length cap before being
// re-sent verbatim as the next page's clientseq -- unlike the per-entry mail uid field, which
// round 45's maxMailUidLen guard already covers. lastUid gets re-encoded via PutUtfString
// (writeUtfString's own 65535-byte hard cap) on the NEXT page request, but GetString can't
// distinguish the 65535-byte-capped sfsUtfString wire tag from the far larger sfsText tag, so an
// oversized lastUid used to cause a purely local encode failure that sendStageError (conn.go)
// deliberately classifies the same as a genuine dead connection -- silently aborting the rest of
// ClaimAllMail and every other -collect action scheduled after it, even though the connection is
// healthy. Sends a page-1 response with more=true and a lastUid one byte over maxMailUidLen
// (tagged sfsText, constructed via SFSObject.put directly since PutUtfString itself would fail to
// encode a >65535-byte string), mirroring round 45's TestListMailSkipsOversizedUidField technique,
// and proves ListMail stops pagination after page 1 (never sending a second request) instead of
// looping into an unencodable cursor, keeping the page-1 mail already collected.
func TestListMailStopsOnOversizedLastUid(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	oversizedLastUid := strings.Repeat("a", maxMailUidLen+1)

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
		resp.put("lastUid", SFSValue{sfsText, oversizedLastUid})
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
		t.Fatal("ListMail never returned -- it looped on the oversized lastUid cursor instead of stopping pagination")
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
	if logged := buf.String(); !strings.Contains(logged, "lastUid exceeds the mail uid length cap") {
		t.Errorf("expected a warning about the oversized lastUid, got log:\n%s", logged)
	}
}

// TestListMailWarnsOnMissingLastMailTime is the regression test for this round's fix to ListMail's
// pagination loop: lastMailTime is lastUid's sibling cursor field (read the line right after lastUid
// and forwarded the same way into the next page's `time` request field), but unlike GetString's ""
// zero value -- which is never a legitimate mail uid, so TestListMailStopsOnMissingLastUid's check
// can safely treat it as "missing" -- GetLong's int64(0) zero value IS indistinguishable from a
// legitimate cold-start `time`. Before this fix, a response with a valid lastUid but a missing
// lastMailTime silently reset reqTime to 0 with zero diagnostic, while clientseq kept advancing
// normally: an asymmetric, harder-to-notice version of the exact anomaly the lastUid check already
// guards against. This does not abort pagination (bounded impact: seenUIDs dedupes, maxPages caps
// the loop -- see mail.go's comment), so, unlike TestListMailStopsOnMissingLastUid, the fake server
// here does answer a second request; the fix is only observable via (a) the second request's `time`
// coming back 0 despite page 1 reporting `more: true` with a valid lastUid, and (b) a new warning
// naming lastMailTime.
func TestListMailWarnsOnMissingLastMailTime(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type gotReq struct {
		clientseq string
		time      int64
	}
	var gotReqs []gotReq

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		readReq := func() (*ExtensionMessage, bool) {
			env, err := server.ReadEnvelope()
			if err != nil {
				return nil, false
			}
			msg, ok := env.AsExtension()
			if !ok {
				return nil, false
			}
			gotReqs = append(gotReqs, gotReq{msg.Params.GetString("clientseq"), msg.Params.GetLong("time")})
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
		resp1.PutSFSArray("msg", arr1)
		resp1.PutBool("more", true)
		resp1.PutUtfString("lastUid", "uid-2")
		// Deliberately no PutLong("lastMailTime", ...) call: `more: true` with a valid lastUid but
		// a genuinely missing lastMailTime is exactly the malformed shape under test.
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
		_ = server.SendExtension("push.chat.get.system.mails", resp2)
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
		t.Fatal("ListMail never returned")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished both pages")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	wantUids := []string{"uid-1", "uid-3"}
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
	if gotReqs[1].clientseq != "uid-2" {
		t.Errorf("page 2 request clientseq = %q, want %q (page 1's lastUid) -- the missing lastMailTime must not stop clientseq advancing", gotReqs[1].clientseq, "uid-2")
	}
	if gotReqs[1].time != 0 {
		t.Errorf("page 2 request time = %d, want 0 -- lastMailTime was missing from page 1's response so reqTime resets to GetLong's zero value", gotReqs[1].time)
	}
	if logged := buf.String(); !strings.Contains(logged, "lastMailTime") {
		t.Errorf("expected a warning mentioning lastMailTime when more=true but lastMailTime is missing, got log:\n%s", logged)
	}
}

// TestListMailWarnsOnWrongTypedLastMailTime is the regression test for round 29's fix to ListMail's
// lastMailTime guard: the previous guard (`!ok || v.Val == nil`) only caught a genuinely missing or
// explicit-null lastMailTime, not a present-but-wrong-typed one. GetLong silently coerces ANY
// non-nil value whose concrete Go type isn't in its accepted set (int64/int32/int16/byte) to
// int64(0) -- indistinguishable from a legitimate cold-start `time` -- so a wrong-typed lastMailTime
// used to reset reqTime to 0 with zero diagnostic signal, exactly like the missing/null case
// TestListMailWarnsOnMissingLastMailTime covers, but without even a warning. The fake server here
// sends lastMailTime as a string instead of a long; a correct ListMail must still advance clientseq
// normally, reset reqTime to 0 for the next page, and now also log a warning naming lastMailTime.
func TestListMailWarnsOnWrongTypedLastMailTime(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	type gotReq struct {
		clientseq string
		time      int64
	}
	var gotReqs []gotReq

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)

		readReq := func() (*ExtensionMessage, bool) {
			env, err := server.ReadEnvelope()
			if err != nil {
				return nil, false
			}
			msg, ok := env.AsExtension()
			if !ok {
				return nil, false
			}
			gotReqs = append(gotReqs, gotReq{msg.Params.GetString("clientseq"), msg.Params.GetLong("time")})
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
		resp1.PutSFSArray("msg", arr1)
		resp1.PutBool("more", true)
		resp1.PutUtfString("lastUid", "uid-2")
		resp1.PutUtfString("lastMailTime", "not-a-long") // wrong SFS type: lastMailTime must be a Long
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
		_ = server.SendExtension("push.chat.get.system.mails", resp2)
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
		t.Fatal("ListMail never returned")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished both pages")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	wantUids := []string{"uid-1", "uid-3"}
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
	if gotReqs[1].clientseq != "uid-2" {
		t.Errorf("page 2 request clientseq = %q, want %q (page 1's lastUid) -- the wrong-typed lastMailTime must not stop clientseq advancing", gotReqs[1].clientseq, "uid-2")
	}
	if gotReqs[1].time != 0 {
		t.Errorf("page 2 request time = %d, want 0 -- lastMailTime was wrong-typed in page 1's response so reqTime resets to GetLong's zero value", gotReqs[1].time)
	}
	logged := buf.String()
	if !strings.Contains(logged, "lastMailTime") {
		t.Errorf("expected a warning mentioning lastMailTime when more=true but lastMailTime is wrong-typed, got log:\n%s", logged)
	}
	if !strings.Contains(logged, "wrong-typed") {
		t.Errorf("expected the warning to identify the field as wrong-typed (not missing/null), got log:\n%s", logged)
	}
	// Round-31 regression: this call site used to also call warnIfWrongTypedField (login.go)
	// alongside the specific message above, producing a redundant SECOND "level=WARN" line for the
	// one malformed field -- contradicting round 30's own stated intent that the specific message
	// be this call site's sole diagnostic. Counting occurrences (not just presence) is what would
	// have caught that regression.
	if n := strings.Count(logged, "level=WARN"); n != 1 {
		t.Errorf("expected exactly 1 WARN log line for the single wrong-typed lastMailTime field, got %d:\n%s", n, logged)
	}
}

// TestListMailWarnsOnNonBoolMoreField is the regression test for this round's fix to ListMail's
// pagination loop: a response whose `more` field is present but not a bool must not be silently
// treated as more=false with zero diagnostic. Before the fix, `mv.Val.(bool)`'s failed assertion
// left the local `more` variable at its zero value (false) with no logging at all, so the loop
// broke exactly as if the server had genuinely said "no more pages" -- indistinguishable in the
// logs from a legitimate final page. The adjacent lastUid-missing check (see
// TestListMailStopsOnMissingLastUid above) already warns for the equivalent anomaly on its own
// field; this fix makes the `more` field's own type-assertion failure warn too. The fake server
// here sends `more` as a string instead of a bool, so a correct ListMail must stop after page 1
// (identical outward behavior to more=false) while now also logging a warning that names the
// `more` field.
func TestListMailWarnsOnNonBoolMoreField(t *testing.T) {
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
		resp.PutUtfString("more", "yes") // wrong-typed: server-shape anomaly under test, not a bool
		resp.PutUtfString("lastUid", "cursor-1")
		resp.PutLong("lastMailTime", 999)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
		// Intentionally does not read a second request -- a correct ListMail treats the wrong-typed
		// more field as more=false and never sends one.
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
		t.Fatal("ListMail never returned -- it should treat the non-bool more field as more=false and stop")
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
	if logged := buf.String(); !strings.Contains(logged, "more") {
		t.Errorf("expected a warning mentioning the more field when it is present but not a bool, got log:\n%s", logged)
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

// TestListMailAggregateCeilingStopsAcrossPages is the round-40 regression test for the MAJOR
// finding that ListMail's pagination loop had no aggregate ceiling on the total number of Mail
// entries accumulated across ALL pages: mailListRawItemCap only bounds a single page's raw-item
// SCAN cost, and maxPages/mailListPageSize only bound round-trip COUNT, not aggregate size. Before
// this fix, a hostile peer answering every page with mailListRawItemCap(1000) valid-shaped entries
// and always more=true could inflate `all` to up to maxPages*mailListRawItemCap=20,000 entries.
//
// The fake server here answers each page with exactly mailListRawItemCap distinct-uid entries and
// always more=true -- maxAggregateMailPerFetch(2000) / mailListRawItemCap(1000) = exactly 2 pages
// are needed to reach the cap, so a correctly-fixed ListMail must stop requesting further pages
// right there: the fake server only reads and answers 2 requests (deliberately does not offer a
// 3rd), proving the loop stops itself BEFORE sending a page that would only be discarded, not just
// that the final output happens to be truncated.
func TestListMailAggregateCeilingStopsAcrossPages(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	pagesNeeded := maxAggregateMailPerFetch / mailListRawItemCap // exactly 2, by construction

	var reqCount int
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for page := 0; page < pagesNeeded; page++ {
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
			for i := 0; i < mailListRawItemCap; i++ {
				arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-p%d-%d", page, i), 3, 0))
			}
			resp.PutSFSArray("msg", arr)
			resp.PutBool("more", true) // always more -- the aggregate cap, not the server, must stop this
			resp.PutUtfString("lastUid", fmt.Sprintf("cursor-%d", page))
			resp.PutLong("lastMailTime", int64(page))
			if err := server.SendExtension("push.chat.get.system.mails", resp); err != nil {
				return
			}
		}
		// Intentionally does not read a (pagesNeeded+1)th request -- the point of this test is that
		// a correctly-fixed ListMail never sends one, having already reached the aggregate cap.
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
		t.Fatal("ListMail never returned -- it should stop once the aggregate ceiling is reached")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if reqCount != pagesNeeded {
		t.Fatalf("fake server saw %d requests, want exactly %d -- ListMail must stop BEFORE requesting a page it would only discard", reqCount, pagesNeeded)
	}
	if len(got) != maxAggregateMailPerFetch {
		t.Fatalf("got %d mail entries, want exactly %d (maxAggregateMailPerFetch)", len(got), maxAggregateMailPerFetch)
	}

	logged := buf.String()
	if !strings.Contains(logged, "aggregate mail count across this fetch reached the upper bound") {
		t.Errorf("expected a warning identifying the aggregate ceiling as the stop reason, got log:\n%s", logged)
	}
}

// TestListMailAggregateCeilingSinglePageOvershootDoesNotExceedCap is the round-41 regression test
// for the MINOR finding that TestListMailAggregateCeilingStopsAcrossPages above only exercises
// page-aligned chunks landing exactly on maxAggregateMailPerFetch's boundary (2 pages of exactly
// mailListRawItemCap=1000 each, summing to exactly 2000) -- confirmed via mutation testing that
// the per-append guard inside ListMail's inner item loop (mail.go) can be deleted entirely without
// this test (or the full suite) catching it, since for that specific page-aligned construction the
// outer loop-top check alone produces identical observable behavior. This sends a DELIBERATELY
// misaligned split instead: each page is individually capped at mailListRawItemCap=1000 by its own
// unrelated raw-item-scan guard, so getting a genuine MID-PAGE overshoot requires at least one page
// under 1000 positioned before the boundary-crossing page -- 500, then 1000, then 1000 (500+1000=
// 1500, still under the cap; the third page's own 1000 items would push the running total to 2500
// without the per-append guard) -- so only that guard, not the loop-top check alone, can stop
// accumulation mid-page-3 at exactly 2000, not 2500.
func TestListMailAggregateCeilingSinglePageOvershootDoesNotExceedCap(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	// Each entry must be <= mailListRawItemCap (1000) -- a page's own raw-item-scan cap would
	// otherwise silently truncate it first, defeating this test's deliberate misalignment.
	pageSizes := []int{500, 1000, 1000} // running totals: 500, 1500, then 2500 without the fix

	var reqCount int
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for page, size := range pageSizes {
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
			for i := 0; i < size; i++ {
				arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-p%d-%d", page, i), 3, 0))
			}
			resp.PutSFSArray("msg", arr)
			resp.PutBool("more", true) // always more -- the aggregate cap, not the server, must stop this
			resp.PutUtfString("lastUid", fmt.Sprintf("cursor-%d", page))
			resp.PutLong("lastMailTime", int64(page))
			if err := server.SendExtension("push.chat.get.system.mails", resp); err != nil {
				return
			}
		}
		// Intentionally does not read a 3rd request -- a correctly-fixed ListMail must stop
		// requesting further pages once the aggregate cap is reached mid-page-1.
	}()

	got, err := ListMail(client)

	select {
	case <-serverDone:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if reqCount != len(pageSizes) {
		t.Fatalf("fake server saw %d requests, want exactly %d -- ListMail must stop BEFORE requesting a 3rd page it would only discard", reqCount, len(pageSizes))
	}
	if len(got) != maxAggregateMailPerFetch {
		t.Errorf("got %d mail entries, want exactly %d (maxAggregateMailPerFetch) -- the per-append guard must stop accumulation mid-page, not just at page boundaries", len(got), maxAggregateMailPerFetch)
	}
}

// TestListMailDedupesUIDAcrossPages is the regression test for ListMail's seenUIDs guard (mail.go):
// ListMail's own doc comment already flags real uncertainty about the pagination cursor's true
// semantics, so if the server's cursor ever repeats the same mail uid across two pages, ListMail
// must only return it once rather than letting a duplicate flow unfiltered into ClaimAllMail (where
// groupUnclaimedByType would bucket it twice and put it twice into a single mail.reward.batch
// request's comma-joined uids field for a reward-bearing mail). The fake server here answers page 1
// with two mail entries, then page 2 with one brand-new uid PLUS a resend of page 1's second uid --
// simulating a cursor that unexpectedly repeats a boundary entry -- and more=false to stop there.
// ListMail must return exactly the three distinct uids, in first-sighting order, with the repeated
// uid appearing only once.
func TestListMailDedupesUIDAcrossPages(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)

		env, err := server.ReadEnvelope()
		if err != nil {
			t.Errorf("read page 1 request: %v", err)
			return
		}
		if msg, ok := env.AsExtension(); !ok || msg.Cmd != "chat.get.system.mails" {
			t.Errorf("page 1 request malformed")
			return
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

		env, err = server.ReadEnvelope()
		if err != nil {
			t.Errorf("read page 2 request: %v", err)
			return
		}
		if msg, ok := env.AsExtension(); !ok || msg.Cmd != "chat.get.system.mails" {
			t.Errorf("page 2 request malformed")
			return
		}
		resp2 := NewSFSObject()
		arr2 := NewSFSArray()
		// uid-2 is a repeat of page 1's second entry -- the same cursor-repeats-a-uid scenario
		// this test exercises -- followed by a genuinely new uid-3.
		arr2.AddSFSObject(newTestMailObj("uid-2", 4, 1))
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
		t.Fatalf("got %d mail entries %v, want %d (uid-2 repeated across pages must be deduped to a single entry)", len(got), uidsOf(got), len(wantUids))
	}
	for i, m := range got {
		if m.Uid() != wantUids[i] {
			t.Errorf("mail[%d].Uid() = %q, want %q", i, m.Uid(), wantUids[i])
		}
	}
}

// uidsOf is a small test helper for failure messages: returns the uids of a []Mail slice in order.
func uidsOf(mail []Mail) []string {
	uids := make([]string, len(mail))
	for i, m := range mail {
		uids[i] = m.Uid()
	}
	return uids
}

// TestListMailCapsRawItemsExaminedPerPage is the round-27 regression test for ListMail's raw-item
// scan cap (mail.go's mailListRawItemCap): mailListPageSize (100, the requested page-size hint) and
// maxPages (20) only bound round-trip COUNT, not the size of any single page's response array,
// which is otherwise bounded only by sfsobject.go's much larger maxDecodedNodes=300,000 decode
// budget. Before this fix, a malformed entry (missing the required "uid" field) hit a `continue`
// that didn't advance any output-count-based cap, since it never reached the append -- the same
// gap-class visitors.go's ParseInitVisitors closed in round 26 for visitor.list (see
// TestParseInitVisitorsCapsRawItemsExaminedNotJustValidOutput).
//
// The fake server here answers a single page with far more entries than mailListRawItemCap, every
// one of them missing "uid" so none inflate the valid-output count. Since every entry is malformed,
// len(got) stays 0 throughout the scan either way -- so counting the "skipping mail entry with no
// uid field" warnings actually logged, rather than just asserting len(got), is what makes this test
// capable of catching an unbounded-scan regression at all: len(got) would be 0 either way, fixed or
// broken.
//
// Mutation check: reverting the loop's `for i, item := range arr.items { if i >= mailListRawItemCap
// { break }; ... }` in mail.go back to a plain `for _, item := range arr.items { ... }` makes this
// test fail with a logged-warning count of wantMalformed instead of mailListRawItemCap.
func TestListMailCapsRawItemsExaminedPerPage(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	// mailListRawItemCap is now package-scoped (mail.go, round 28), so this references ListMail's
	// own real constant directly -- no local re-declaration to keep in sync by hand anymore.
	wantMalformed := mailListRawItemCap * 5 // far more malformed entries than the cap

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
		resp := NewSFSObject()
		arr := NewSFSArray()
		for i := 0; i < wantMalformed; i++ {
			mo := NewSFSObject()
			mo.PutInt("type", 3) // deliberately no "uid" field
			arr.AddSFSObject(mo)
		}
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", false)
		if err := server.SendExtension("push.chat.get.system.mails", resp); err != nil {
			t.Errorf("send list-mail response: %v", err)
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got, err := ListMail(client)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListMail() returned %d mail entries, want 0 (every entry in this test is malformed -- missing uid)", len(got))
	}

	logged := buf.String()
	gotWarnings := strings.Count(logged, "skipping mail entry with no uid field")
	if gotWarnings != mailListRawItemCap {
		t.Errorf("ListMail logged %d \"missing uid\" warnings, want exactly %d (the cap on RAW items examined per page, not just valid ones appended) -- input had %d malformed entries; the loop must stop scanning after the first %d regardless of how many turned out valid", gotWarnings, mailListRawItemCap, wantMalformed, mailListRawItemCap)
	}
	if !strings.Contains(logged, "page response array longer than raw-item scan cap") {
		t.Errorf("expected a warning about the page response array exceeding the raw-item scan cap, got log:\n%s", logged)
	}
}

// TestListMailRawItemCapBoundary is the round-28 boundary-condition regression test for
// mailListRawItemCap: TestListMailCapsRawItemsExaminedPerPage above overshoots the cap by 5x, so it
// would not catch a production ">"-to-">=" regression in the truncation-warning condition
// (`len(arr.items) > mailListRawItemCap`) -- an off-by-one there would either warn one item too
// early, or worse, silently drop the last legitimate mail of an exactly-at-cap page without ever
// logging why. This drives both sides of that boundary directly with well-formed, uniquely-uid'd
// entries, so len(got) itself -- not just a logged-warning count -- proves whether the cap fired at
// the right point.
func TestListMailRawItemCapBoundary(t *testing.T) {
	sendPageAndList := func(t *testing.T, n int) ([]Mail, string) {
		t.Helper()
		client, server := newPipeGameConnPair(t)

		done := make(chan struct{})
		go func() {
			defer close(done)
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			if _, ok := env.AsExtension(); !ok {
				return
			}
			resp := NewSFSObject()
			arr := NewSFSArray()
			for i := 0; i < n; i++ {
				arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-%d", i), 3, 0))
			}
			resp.PutSFSArray("msg", arr)
			resp.PutBool("more", false)
			_ = server.SendExtension("push.chat.get.system.mails", resp)
		}()

		var buf bytes.Buffer
		orig := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		got, err := ListMail(client)
		slog.SetDefault(orig)

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("fake server never finished")
		}
		if err != nil {
			t.Fatalf("ListMail() = %v, want nil", err)
		}
		return got, buf.String()
	}

	t.Run("exactly cap items: all parsed, no truncation warning", func(t *testing.T) {
		got, logged := sendPageAndList(t, mailListRawItemCap)
		if len(got) != mailListRawItemCap {
			t.Fatalf("ListMail() returned %d mail entries, want exactly %d (the cap, all well-formed)", len(got), mailListRawItemCap)
		}
		if strings.Contains(logged, "longer than raw-item scan cap") {
			t.Errorf("unexpected truncation warning at exactly-cap boundary:\n%s", logged)
		}
	})

	t.Run("cap+1 items: truncation warning fires, only cap parsed", func(t *testing.T) {
		got, logged := sendPageAndList(t, mailListRawItemCap+1)
		if len(got) != mailListRawItemCap {
			t.Fatalf("ListMail() returned %d mail entries, want exactly %d (cap+1 input must still truncate to the cap)", len(got), mailListRawItemCap)
		}
		if !strings.Contains(logged, "page response array longer than raw-item scan cap; truncating") {
			t.Errorf("expected a truncation warning at cap+1, got:\n%s", logged)
		}
	})
}

// TestListMailWrongTypedUIDIsRejected is the round-28 regression test for requireFieldType
// (buildings.go), exercised here at ListMail's own uid guard: before this round's fix,
// requirePresentField only checked presence, never that uid's concrete decoded SFS type actually
// matched what Mail.Uid() (GetString) accepts. A present-but-wrong-typed uid (e.g. the server
// sending it as an Int instead of a UtfString) used to silently pass that presence-only guard and
// then coerce to "" via GetString's own zero-value fallback -- colliding with any other wrong-typed
// entry in seenUIDs, and worse, feeding ClaimAllMail's read-status/reward-claim batches a uid=""
// that doesn't identify any real mail -- a PERMANENT loss of the real mail's reward, since
// rewardStatus is per-mail.
//
// The input here has one wrong-typed-uid entry and one genuine, well-typed entry -- proving exactly
// one mail comes back (the genuine one), and a "wrong-typed uid" warning, not a "missing uid" one,
// is logged for the other.
//
// Mutation check: reverting ListMail's `requireFieldType(mo, "uid", "mail", sfsFieldKindString)`
// back to `requirePresentField(mo, "uid", "mail")` makes this test fail with 2 mail entries instead
// of 1 (the wrong-typed one silently coerced to uid=""), and no "wrong-typed" warning logged.
func TestListMailWrongTypedUIDIsRejected(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if _, ok := env.AsExtension(); !ok {
			return
		}
		wrongTyped := NewSFSObject()
		wrongTyped.PutInt("uid", 12345) // wrong SFS type: a mail uid must be a UtfString
		wrongTyped.PutInt("type", 3)

		resp := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(wrongTyped)
		arr.AddSFSObject(newTestMailObj("real-uid-1", 3, 0))
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", false)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got, err := ListMail(client)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Uid() != "real-uid-1" {
		t.Fatalf("ListMail() = %v, want exactly 1 entry (uid=real-uid-1) -- the wrong-typed uid entry must be rejected, not silently coerced to uid=\"\"", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping mail entry with wrong-typed uid field") {
		t.Errorf("expected a wrong-typed-uid warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping mail entry with no uid field") {
		t.Errorf("wrong-typed uid must log as wrong-typed, not as missing -- got log:\n%s", logged)
	}
}

// TestListMailSkipsOversizedUidField is the round-45 regression test for the MAJOR finding that a
// mail entry's uid field arriving oversized (e.g. tagged sfsText instead of sfsUtfString -- sfsText
// has no 65535-byte encode-side cap, unlike sfsUtfString) used to pass through ListMail's
// type-only requireFieldType guard unchanged, then later cause a PURELY LOCAL writeUtfString
// encode failure when ClaimAllMail re-batched it into a mail.read.status.betch/mail.reward.batch
// request. sendStageError (conn.go) deliberately, by design, classifies that local encode failure
// the same as a genuine dead connection, so this single malformed mail entry used to silently
// abort the rest of ClaimAllMail and, via CollectAll's containsNonTimeoutNetError abort logic
// (buildings.go), every other -collect action scheduled after it in the same run. Sends one mail
// entry with a uid one byte over maxMailUidLen (tagged sfsText, constructed via SFSObject.put
// directly since PutUtfString itself would fail to encode a >65535-byte string) alongside one
// well-formed mail entry, and proves ListMail skips only the oversized one (with a Warn), keeping
// the well-formed entry intact -- closing the gap at its source instead of only softening the
// downstream misclassification.
func TestListMailSkipsOversizedUidField(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	oversizedUID := strings.Repeat("a", maxMailUidLen+1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if _, ok := env.AsExtension(); !ok {
			return
		}
		oversized := NewSFSObject()
		oversized.put("uid", SFSValue{sfsText, oversizedUID})
		oversized.PutInt("type", 3)

		resp := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(oversized)
		arr.AddSFSObject(newTestMailObj("real-uid-1", 3, 0))
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", false)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got, err := ListMail(client)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Uid() != "real-uid-1" {
		t.Fatalf("ListMail() = %v, want exactly 1 entry (uid=real-uid-1) -- the oversized-uid entry must be skipped, not appended", got)
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping mail entry with oversized uid field") {
		t.Errorf("expected an oversized-uid warning, got log:\n%s", logged)
	}
}

// TestListMailAcceptsUidExactlyAtLenCap is TestListMailSkipsOversizedUidField's boundary
// counterpart: a uid of exactly maxMailUidLen bytes must NOT be skipped -- maxMailUidLen matches
// writeUtfString's own hard limit exactly (a strict `> 65535`, not `>=`), so any uid this check
// accepts is guaranteed re-encodable later by ClaimAllMail's batching.
func TestListMailAcceptsUidExactlyAtLenCap(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	exactUID := strings.Repeat("a", maxMailUidLen)

	done := make(chan struct{})
	go func() {
		defer close(done)
		env, err := server.ReadEnvelope()
		if err != nil {
			return
		}
		if _, ok := env.AsExtension(); !ok {
			return
		}
		resp := NewSFSObject()
		arr := NewSFSArray()
		arr.AddSFSObject(newTestMailObj(exactUID, 3, 0))
		resp.PutSFSArray("msg", arr)
		resp.PutBool("more", false)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got, err := ListMail(client)
	slog.SetDefault(orig)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Uid() != exactUID {
		t.Fatalf("ListMail() returned %d entries, want exactly 1 with the exactly-at-cap uid intact (the boundary value, not over the cap)", len(got))
	}
	if logged := buf.String(); strings.Contains(logged, "skipping mail entry with oversized uid field") {
		t.Errorf("expected NO truncation warning for an exactly-at-cap uid, got log:\n%s", logged)
	}
}

// TestGroupUnclaimedByTypeWrongTypedTypeFieldIsRejected is the round-28 regression test for
// requireFieldType (buildings.go), exercised here at groupUnclaimedByType's own type guard: before
// this round's fix, requirePresentField only checked presence, never that type's concrete decoded
// SFS type actually matched what Mail.Type() (GetInt) accepts. A present-but-wrong-typed type
// (e.g. the server sending it as a UtfString instead of an Int) used to silently pass that
// presence-only guard and then coerce to int32(0) via GetInt's own zero-value fallback -- merging
// that mail into a genuinely-type=0 batch it doesn't belong to.
//
// The input here has one reward-bearing mail with a wrong-typed type field and a separate,
// genuinely-well-typed type=0 reward-bearing mail -- proving these two are no longer conflated:
// exactly one distinct type (0) with exactly the genuine mail's uid must come back.
//
// Mutation check: reverting groupUnclaimedByType's `requireFieldType(m.Raw, "type", "mail reward",
// sfsFieldKindInt)` back to `requirePresentField(m.Raw, "type", "mail reward")` makes this test
// fail with both uids merged under byType[0].
func TestGroupUnclaimedByTypeWrongTypedTypeFieldIsRejected(t *testing.T) {
	wrongTyped := NewSFSObject()
	wrongTyped.PutUtfString("uid", "wrong-type-1")
	wrongTyped.PutUtfString("type", "not-an-int") // wrong SFS type: type must be an Int
	wrongTyped.PutInt("rewardStatus", 0)

	genuineZero := NewSFSObject()
	genuineZero.PutUtfString("uid", "genuine-zero-1")
	genuineZero.PutInt("type", 0)
	genuineZero.PutInt("rewardStatus", 0)

	mail := []Mail{{Raw: wrongTyped}, {Raw: genuineZero}}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	got := groupUnclaimedByType(mail)
	slog.SetDefault(orig)

	if len(got) != 1 {
		t.Fatalf("got %d distinct types, want 1 (only the genuine type=0 entry -- the wrong-typed one must be rejected, not merged into the same type=0 batch)", len(got))
	}
	if len(got[0]) != 1 || got[0][0] != "genuine-zero-1" {
		t.Errorf("type 0: got %v, want [genuine-zero-1] -- wrong-type-1 (wrong-typed type field) must not appear", got[0])
	}

	logged := buf.String()
	if !strings.Contains(logged, "skipping mail reward entry with wrong-typed type field") {
		t.Errorf("expected a wrong-typed-type warning, got log:\n%s", logged)
	}
	if strings.Contains(logged, "skipping mail reward entry with no type field") {
		t.Errorf("wrong-typed type must log as wrong-typed, not as missing -- got log:\n%s", logged)
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

// TestBatchByCountAndBytesExactByteBoundary is the round-47 regression test for the MINOR finding
// that batchByCountAndBytes' byte-budget guard (mail.go: `batchBytes+len(uids[end])+1 <= maxBytes`)
// had no test pinning its exact boundary -- TestClaimAllMailByteLengthBatching above is the closest
// existing coverage, but its uid lengths (6499 bytes, batches topping out at 58500 of the real
// maxUIDsBytes=60000) never land the running total on maxBytes or maxBytes+1 exactly, leaving a
// margin of 1500-6500 bytes on either side. Calls batchByCountAndBytes directly (it's already a
// standalone, network-free function -- no fake server needed) with uid lengths engineered so the
// running total after the second uid lands EXACTLY at maxBytes (must still be admitted into the
// first batch) or exactly maxBytes+1 (must be excluded, starting a new batch).
func TestBatchByCountAndBytesExactByteBoundary(t *testing.T) {
	const maxBytes = 20
	const maxCount = 10
	uid1 := strings.Repeat("a", 7) // 7 bytes; +1 joining comma -> running total 8

	t.Run("second uid lands exactly at maxBytes: both uids share one batch", func(t *testing.T) {
		// 8 (running total after uid1) + 11 (uid2) + 1 (comma) == 20 == maxBytes exactly.
		uid2 := strings.Repeat("b", 11)
		got := batchByCountAndBytes([]string{uid1, uid2}, maxCount, maxBytes)
		if len(got) != 1 {
			t.Fatalf("got %d batches, want 1 (both uids must share a batch at exactly maxBytes)", len(got))
		}
		if len(got[0]) != 2 || got[0][0] != uid1 || got[0][1] != uid2 {
			t.Errorf("batch = %v, want both uids together", got)
		}
	})

	t.Run("second uid is one byte past maxBytes: starts a new batch", func(t *testing.T) {
		// 8 (running total after uid1) + 12 (uid2) + 1 (comma) == 21 == maxBytes+1.
		uid2 := strings.Repeat("b", 12)
		got := batchByCountAndBytes([]string{uid1, uid2}, maxCount, maxBytes)
		if len(got) != 2 {
			t.Fatalf("got %d batches, want 2 (uid2 one byte over must start a new batch, not truncate)", len(got))
		}
		if len(got[0]) != 1 || got[0][0] != uid1 {
			t.Errorf("batch[0] = %v, want [uid1] alone", got[0])
		}
		if len(got[1]) != 1 || got[1][0] != uid2 {
			t.Errorf("batch[1] = %v, want [uid2] alone", got[1])
		}
	})
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
// Read calls as it takes to drain them, then permanently flips to returning fakeNetError{}
// (borrowed from buildings_orchestration_test.go's fakeNetErrConn/fakeNetError/fakeNetAddr -- same
// package, so directly visible here) for every Read call after that.
//
// fakeNetError{} (the zero value, timeout: false) is a genuine connection-level failure
// (Timeout()==false) -- see fakeNetError's own doc comment (buildings_orchestration_test.go) for
// why that's the right default here: a permanently-dead connection, not a transient per-action
// timeout, so a bare scriptedNetErrConn is exactly what the "should still abort" tests need (e.g.
// TestClaimAllMailAbortsRemainingBatchesOnNetError). A Timeout()==true net.Error sandwiched
// between otherwise-successful responses (needed by the round-21 "must NOT abort" tests below) is
// beyond what this single-permanently-dead-tail shape can express -- see sequencedConn further
// down for that.
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

// connTurn is one scripted step for sequencedConn below: either a chunk of pre-recorded response
// bytes (served for as many Read calls as it takes to drain, exactly like scriptedNetErrConn's
// single chunk) or, if err is non-nil, a single error returned in place of bytes.
type connTurn struct {
	bytes []byte
	err   error
}

// encodeResponse builds the exact wire-format bytes SendExtension/EncodeObject produce for a canned
// cmd/resp pair, the same recordingConn-based technique TestClaimAllMailAbortsRemainingBatchesOnNetError
// and friends already use for scriptedNetErrConn's single chunk -- factored out here because
// sequencedConn below needs several independently-encoded chunks, not just one.
func encodeResponse(t *testing.T, cmd string, resp *SFSObject) []byte {
	t.Helper()
	rec := &recordingConn{}
	recorder := &GameConn{conn: rec}
	if err := recorder.SendExtension(cmd, resp); err != nil {
		t.Fatalf("build canned %s response: %v", cmd, err)
	}
	return rec.buf.Bytes()
}

// sequencedConn generalizes scriptedNetErrConn to a sequence of turns, each either real
// response bytes or an error, consumed strictly in order. scriptedNetErrConn alone can only script
// "N real responses, then permanently dead" -- it cannot put a transient per-action failure in the
// MIDDLE of an otherwise-successful sequence. sequencedConn exists specifically for the round-21
// regression tests below, which need exactly that: e.g. "list-mail succeeds, read-status batch 1
// fails with a Timeout()==true net.Error, read-status batch 2 succeeds, both reward-claim batches
// succeed" -- proving a per-action timeout doesn't abort the batch loops it sits inside, unlike a
// genuine (non-timeout) net.Error. Writes always succeed and are counted, same as scriptedNetErrConn.
type sequencedConn struct {
	mu     sync.Mutex
	turns  []connTurn
	writes int
}

func (c *sequencedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.turns) > 0 {
		turn := &c.turns[0]
		if turn.err != nil {
			c.turns = c.turns[1:]
			return 0, turn.err
		}
		if len(turn.bytes) == 0 {
			c.turns = c.turns[1:]
			continue
		}
		n := copy(p, turn.bytes)
		turn.bytes = turn.bytes[n:]
		return n, nil
	}
	// Ran out of scripted turns -- a correctly-behaving ClaimAllMail should never issue more
	// requests than a test scripted for, so this only fires when a test's own expectations are
	// wrong; io.EOF mirrors recordingConn's unread-from-Read stub.
	return 0, io.EOF
}

func (c *sequencedConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.writes++
	c.mu.Unlock()
	return len(p), nil
}

func (c *sequencedConn) writeCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes
}

func (c *sequencedConn) Close() error                     { return nil }
func (c *sequencedConn) LocalAddr() net.Addr              { return fakeNetAddr{} }
func (c *sequencedConn) RemoteAddr() net.Addr             { return fakeNetAddr{} }
func (c *sequencedConn) SetDeadline(time.Time) error      { return nil }
func (c *sequencedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sequencedConn) SetWriteDeadline(time.Time) error { return nil }

// TestClaimAllMailAbortsRemainingBatchesOnNetError is the round-17 regression test for Fix 1:
// ClaimAllMail's read-status batch loop (mail.go) must check for a net.Error and break instead of
// attempting every remaining batch, mirroring CollectAll's identical check in buildings.go (see
// TestCollectAllAbortsRemainingActionsOnNetError, buildings_orchestration_test.go). It must also
// skip the reward-claim loop entirely once that happens, rather than attempting it against an
// already-known-dead connection.
//
// Round-21 update: scriptedNetErrConn's permanent post-drain failure is fakeNetError{} -- the zero
// value, timeout: false -- which per the round-21 fix to fakeNetError (buildings_orchestration_test.go)
// is a genuine non-timeout connection-level failure, exactly what "should still abort" needs. A prior
// version of this test relied on fakeNetError{} meaning Timeout()==true, which -- per the round-21
// fix to ClaimAllMail itself -- is no longer grounds for an abort at all; see
// TestClaimAllMailReadStatusContinuesAfterTimeout below for that (now fakeNetError{timeout: true})
// case's own coverage. Only a genuine, non-timeout net.Error still aborts the remaining batches.
//
// Unlike TestCollectAllAbortsRemainingActionsOnNetError, this can't just hand ClaimAllMail a
// fakeNetErrConn whose every Read fails from the very first call: ClaimAllMail's first network
// action is ListMail, and that has to genuinely succeed (returning real mail) before there's
// anything for the read-status/reward-claim batch loops to even iterate over. So this uses
// scriptedNetErrConn instead: the ListMail round trip gets a real, valid canned response (150
// same-type unclaimed-reward mail entries -- enough that batchByCountAndBytes' readBatchSize=100
// item cap splits them into two read-status batches, 100 then 50, and would likewise split the
// reward-claim loop's one distinct type into two batches if that loop were ever reached), and every
// Read after that (i.e., every batch call, in either loop) fails immediately with a non-timeout
// net.Error.
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
	if !errors.As(err, &netErr) || netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a non-timeout net.Error (the failure that triggered the abort)", err)
	}
	if got := fake.writeCount(); got != 2 {
		t.Errorf("fake connection saw %d writes, want exactly 2 (list-mail request + first read-status batch only -- ClaimAllMail should have aborted before read-status batch 2 or any reward-claim batch)", got)
	}
}

// TestClaimAllMailAbortsRemainingBatchesOnRealGracefulClose is the round-25 regression-safety-gap
// closer for TestClaimAllMailAbortsRemainingBatchesOnNetError above -- see that test's sibling,
// TestCollectAllAbortsRemainingActionsOnRealGracefulClose (buildings_orchestration_test.go), for the
// full rationale: scriptedNetErrConn's post-drain tail is fakeNetError{}, an already-a-net.Error
// fake, never exercising the real bare-io.EOF-through-ReadPacket-through-deadConnError conversion
// path (packet.go's wrapIfClosed/deadConnError, round 24).
//
// Reuses sequencedConn/connTurn (this file, above) rather than introducing a new fixture type:
// sequencedConn's per-turn err field already supports scripting any error mid-sequence, so a turn
// carrying err: io.EOF -- what a real net.Conn actually produces for a peer's graceful close, not a
// synthetic net.Error stand-in -- after the ListMail response bytes is exactly what's needed here,
// fed through a real GameConn exactly like conn_wait_test.go's
// TestReadEnvelopeGracefulCloseIsNonTimeoutNetError. Same setup and expected shape as
// TestClaimAllMailAbortsRemainingBatchesOnNetError: 150 same-type unclaimed mail (splitting the
// read-status loop into two batches), then a real io.EOF on the first read-status batch's read.
// Exactly 2 writes: the ListMail request, then the one read-status batch request that hits the real
// io.EOF and aborts before batch 2 or the reward-claim loop are ever attempted.
func TestClaimAllMailAbortsRemainingBatchesOnRealGracefulClose(t *testing.T) {
	const total = 150
	const mailType = int32(3)
	listResp := NewSFSObject()
	arr := NewSFSArray()
	for i := 0; i < total; i++ {
		arr.AddSFSObject(newTestMailObj(fmt.Sprintf("uid-%03d", i), mailType, 0)) // rewardStatus=0: unclaimed
	}
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)

	fake := &sequencedConn{turns: []connTurn{
		{bytes: encodeResponse(t, "push.chat.get.system.mails", listResp)},
		{err: io.EOF},
	}}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the read-status batch call hits a real io.EOF)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Fatalf("ClaimAllMail() error = %v, want it to wrap a net.Error (deadConnError, via packet.go's wrapIfClosed)", err)
	}
	if netErr.Timeout() {
		t.Errorf("ClaimAllMail() error's net.Error has Timeout()==true, want false (a graceful close is a genuine dead connection, not an ordinary timeout)")
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
// existing coverage above, deliberately left unchanged by this fix. Since the round-21 fix, the same
// is now true of a Timeout()==true net.Error on ListMail -- see
// TestClaimAllMailProcessesPartialMailOnListPageTimeout below -- so this test relies on
// scriptedNetErrConn's default permanent failure, fakeNetError{} (the zero value, timeout: false --
// per the round-21 fix to fakeNetError, buildings_orchestration_test.go), to keep proving the
// genuinely-dead-connection case.
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
	if !errors.As(err, &netErr) || netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a non-timeout net.Error (the ListMail failure that triggered the skip)", err)
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

// TestClaimAllMailRewardLoopCapsDistinctTypes is the round-41 regression test for the MAJOR
// finding that ClaimAllMail's reward-claim loop had no cap on the number of distinct mail `type`
// buckets it would issue sequential mail.reward.batch requests for -- unlike buildings.go's
// CollectAll (maxCollectibleBuildingsPerRun=300) and visitors.go's GreetVisitors
// (maxVisitorsUpperBound=300), both explicitly sized to bound worst-case wall-clock to ~40 minutes
// against a peer that never responds. Sends maxMailRewardTypesPerRun+1 (301) distinct-type mail
// entries, each with a single unclaimed reward, and proves the reward-claim loop only issues
// exactly maxMailRewardTypesPerRun (300) mail.reward.batch requests, not 301 -- the fake server
// only reads and answers that many, so if the client tried to send a 301st, the whole exchange
// would hang and this test would time out rather than pass.
func TestClaimAllMailRewardLoopCapsDistinctTypes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const totalTypes = maxMailRewardTypesPerRun + 1 // 301 distinct types, one unclaimed mail each
	const readBatchSize = 100                       // must match ClaimAllMail's own unexported readBatchSize constant

	mails := make([]*SFSObject, totalTypes)
	for i := 0; i < totalTypes; i++ {
		mails[i] = newTestMailObj(fmt.Sprintf("uid-%d", i), int32(i), 0)
	}

	var rewardBatchCount int
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

		numReadBatches := (totalTypes + readBatchSize - 1) / readBatchSize
		for i := 0; i < numReadBatches; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			if msg, ok = env.AsExtension(); !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			readResp := NewSFSObject()
			readResp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
				return
			}
		}

		// Exactly maxMailRewardTypesPerRun expected -- the 301st type must never even be sent,
		// since truncation happens client-side before the loop starts, not as an early-abort
		// mid-loop.
		for i := 0; i < maxMailRewardTypesPerRun; i++ {
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
			rewardBatchCount++
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely tried to send more than maxMailRewardTypesPerRun reward-claim batches")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardBatchCount != maxMailRewardTypesPerRun {
		t.Errorf("server saw %d mail.reward.batch requests, want exactly %d (maxMailRewardTypesPerRun)", rewardBatchCount, maxMailRewardTypesPerRun)
	}
	if logged := buf.String(); !strings.Contains(logged, "distinct unclaimed mail reward types exceeds sanity ceiling") {
		t.Errorf("expected a warning about truncating the reward-claim loop, got log:\n%s", logged)
	}
}

// TestClaimAllMailRewardLoopExactlyAtCapDoesNotTruncate is the round-42 regression test for the
// MINOR finding that TestClaimAllMailRewardLoopCapsDistinctTypes above only proves the cap fires
// at maxMailRewardTypesPerRun+1 -- it never exercises the boundary itself, so a regression
// tightening mail.go's `len(mailTypes) > maxMailRewardTypesPerRun` to an off-by-one `>=` would
// silently drop the 300th legitimate distinct mail-reward type with zero test signal. Confirmed
// via mutation testing: that exact `>=` tightening passed TestClaimAllMailRewardLoopCapsDistinctTypes
// unchanged (301 trips both the correct and mutant check identically). Sends exactly
// maxMailRewardTypesPerRun distinct types and proves ALL of them get a reward-claim batch, with
// no truncation warning logged.
func TestClaimAllMailRewardLoopExactlyAtCapDoesNotTruncate(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const totalTypes = maxMailRewardTypesPerRun // exactly at the cap -- must NOT be truncated
	const readBatchSize = 100                   // must match ClaimAllMail's own unexported readBatchSize constant

	mails := make([]*SFSObject, totalTypes)
	for i := 0; i < totalTypes; i++ {
		mails[i] = newTestMailObj(fmt.Sprintf("uid-%d", i), int32(i), 0)
	}

	var rewardBatchCount int
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

		numReadBatches := (totalTypes + readBatchSize - 1) / readBatchSize
		for i := 0; i < numReadBatches; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			if msg, ok = env.AsExtension(); !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			readResp := NewSFSObject()
			readResp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
				return
			}
		}

		for i := 0; i < totalTypes; i++ {
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
			rewardBatchCount++
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely truncated below maxMailRewardTypesPerRun")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardBatchCount != totalTypes {
		t.Errorf("server saw %d mail.reward.batch requests, want exactly %d (maxMailRewardTypesPerRun, the boundary value)", rewardBatchCount, totalTypes)
	}
	if logged := buf.String(); strings.Contains(logged, "distinct unclaimed mail reward types exceeds sanity ceiling") {
		t.Errorf("expected NO truncation warning for exactly-at-cap input, got log:\n%s", logged)
	}
}

// TestClaimAllMailReadStatusLoopCapsBatchCount is the round-44 regression test for the MAJOR
// finding that batchByCountAndBytes' batches, unlike maxAggregateMailPerFetch (total mail item
// count) and maxMailRewardTypesPerRun (distinct type count), had no cap on the NUMBER of batches
// -- since batchByCountAndBytes always admits at least one uid per batch even if that uid alone
// exceeds maxUIDsBytes, a peer returning mail with maximal-length uids can force every batch down
// to a single item, turning maxAggregateMailPerFetch(2000) items into up to 2000 sequential
// mail.read.status.betch round trips instead of the ~20 batches (2000/readBatchSize=100) normal
// pagination produces. Sends maxMailBatchesPerLoop+1 (301) mail entries, each with an oversized
// (30001-byte, comfortably over maxUIDsBytes/2=30000) uid forcing singleton batching, and proves
// the fake server sees only maxMailBatchesPerLoop mail.read.status.betch requests, not 301 -- if
// ClaimAllMail tried to send a 301st, the fake server (which only answers that many) would hang
// the test instead of passing it. All mail is already-claimed (rewardStatus=1) so the reward-claim
// loop is entirely out of scope for this test.
func TestClaimAllMailReadStatusLoopCapsBatchCount(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const totalMails = maxMailBatchesPerLoop + 1 // 301 mail entries, each forcing its own batch
	const oversizedUIDLen = 30001                // > maxUIDsBytes/2, forces singleton batches

	mails := make([]*SFSObject, totalMails)
	for i := 0; i < totalMails; i++ {
		uid := fmt.Sprintf("%0*d", oversizedUIDLen, i) // unique, fixed-length oversized uid
		mails[i] = newTestMailObj(uid, 0, 1)           // rewardStatus=1: already claimed
	}

	var readBatchCount int
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

		// Exactly maxMailBatchesPerLoop expected -- the 301st batch must never even be sent, since
		// truncation happens client-side before the loop starts, not as an early-abort mid-loop.
		for i := 0; i < maxMailBatchesPerLoop; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			readBatchCount++
			readResp := NewSFSObject()
			readResp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely tried to send more than maxMailBatchesPerLoop read-status batches")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if readBatchCount != maxMailBatchesPerLoop {
		t.Errorf("server saw %d mail.read.status.betch requests, want exactly %d (maxMailBatchesPerLoop)", readBatchCount, maxMailBatchesPerLoop)
	}
	if logged := buf.String(); !strings.Contains(logged, "mail batch count exceeds sanity ceiling") {
		t.Errorf("expected a warning about truncating the batch loop, got log:\n%s", logged)
	}
}

// TestClaimAllMailReadStatusLoopBatchCountExactlyAtCapDoesNotTruncate is
// TestClaimAllMailReadStatusLoopCapsBatchCount's boundary counterpart: sends exactly
// maxMailBatchesPerLoop oversized-uid mail entries (each still forcing its own singleton batch)
// and proves ALL of them get a read-status batch, with no truncation warning logged -- closing the
// same off-by-one gap round 42's sibling boundary tests closed for maxRedirects/
// maxRedirectHops/maxMailRewardTypesPerRun/maxCollectibleBuildingsPerRun/maxVisitorsUpperBound.
func TestClaimAllMailReadStatusLoopBatchCountExactlyAtCapDoesNotTruncate(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const totalMails = maxMailBatchesPerLoop // exactly at the cap -- must NOT be truncated
	const oversizedUIDLen = 30001

	mails := make([]*SFSObject, totalMails)
	for i := 0; i < totalMails; i++ {
		uid := fmt.Sprintf("%0*d", oversizedUIDLen, i)
		mails[i] = newTestMailObj(uid, 0, 1)
	}

	var readBatchCount int
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

		for i := 0; i < totalMails; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			readBatchCount++
			readResp := NewSFSObject()
			readResp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", readResp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely truncated below maxMailBatchesPerLoop")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if readBatchCount != totalMails {
		t.Errorf("server saw %d mail.read.status.betch requests, want exactly %d (maxMailBatchesPerLoop, the boundary value)", readBatchCount, totalMails)
	}
	if logged := buf.String(); strings.Contains(logged, "mail batch count exceeds sanity ceiling") {
		t.Errorf("expected NO truncation warning for exactly-at-cap input, got log:\n%s", logged)
	}
}

// TestClaimAllMailRewardLoopCapsTotalBatchesAcrossTypes is the round-46 regression test for the
// MAJOR finding that ClaimAllMail's reward-claim loop had no cap on the TOTAL number of
// mail.reward.batch round trips summed across all distinct mail types in one run --
// maxMailRewardTypesPerRun bounds distinct TYPE count, and maxMailBatchesPerLoop bounds batches
// PER TYPE (reset fresh for every type via truncateMailBatches), but neither bounds their product:
// a hostile peer can spread mail entries across many distinct types, each with an oversized uid
// forcing singleton batching, so that NO single type ever reaches maxMailBatchesPerLoop's own
// per-type truncation, yet the SUM across all types still runs into the hundreds -- the exact
// "~4.4 hours instead of ~40 minutes" threat maxMailBatchesPerLoop's own doc comment describes,
// but which only holds per type. Sends 301 mail entries (one over maxMailRewardBatchesPerRun)
// spread across 43 distinct types (7 entries each -- both comfortably under the type-count and
// per-type-batch-count caps on their own), each with an oversized uid forcing its own singleton
// batch, and proves the fake server sees only maxMailRewardBatchesPerRun mail.reward.batch
// requests, not 301 -- if ClaimAllMail tried to send a 301st, the fake server (which only answers
// that many) would hang the test instead of passing it.
func TestClaimAllMailRewardLoopCapsTotalBatchesAcrossTypes(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const numTypes = 43
	const entriesPerType = 7
	const totalMails = numTypes * entriesPerType // 301 -- one over maxMailRewardBatchesPerRun
	const oversizedUIDLen = 30001                // > maxUIDsBytes/2, forces singleton batches

	mails := make([]*SFSObject, 0, totalMails)
	uidCounter := 0
	for typ := 0; typ < numTypes; typ++ {
		for i := 0; i < entriesPerType; i++ {
			uid := fmt.Sprintf("%0*d", oversizedUIDLen, uidCounter)
			uidCounter++
			mails = append(mails, newTestMailObj(uid, int32(typ), 0)) // rewardStatus=0: unclaimed
		}
	}

	var rewardBatchCount int
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

		// Read-status loop: every uid here is oversized, forcing singleton batching, so the
		// round-44 maxMailBatchesPerLoop fix already truncates this to exactly 300 -- established,
		// separately-tested behavior (TestClaimAllMailReadStatusLoopCapsBatchCount), not what this
		// test is about, but the fake server must still answer this many for ClaimAllMail to reach
		// the reward-claim loop at all.
		for i := 0; i < maxMailBatchesPerLoop; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", resp); err != nil {
				return
			}
		}

		// Reward-claim loop: exactly maxMailRewardBatchesPerRun expected, summed across all 43
		// types -- the 301st batch must never even be sent, since truncation happens mid-loop as
		// soon as the running total hits the cap, not as a post-hoc discard.
		for i := 0; i < maxMailRewardBatchesPerRun; i++ {
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
			rewardBatchCount++
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely tried to send more than maxMailRewardBatchesPerRun reward-claim batches")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardBatchCount != maxMailRewardBatchesPerRun {
		t.Errorf("server saw %d mail.reward.batch requests, want exactly %d (maxMailRewardBatchesPerRun)", rewardBatchCount, maxMailRewardBatchesPerRun)
	}
	if logged := buf.String(); !strings.Contains(logged, "total mail.reward.batch round trips across all types exceeds sanity ceiling") {
		t.Errorf("expected a warning about the aggregate reward-batch ceiling, got log:\n%s", logged)
	}
}

// TestClaimAllMailRewardLoopTotalBatchesExactlyAtCapDoesNotTruncate is
// TestClaimAllMailRewardLoopCapsTotalBatchesAcrossTypes's boundary counterpart: sends exactly
// maxMailRewardBatchesPerRun oversized-uid mail entries (each still forcing its own singleton
// batch), spread across 60 distinct types (well under maxMailRewardTypesPerRun, and only 5
// batches/type -- well under maxMailBatchesPerLoop), and proves ALL of them get a reward-claim
// batch, with no aggregate-ceiling warning logged.
func TestClaimAllMailRewardLoopTotalBatchesExactlyAtCapDoesNotTruncate(t *testing.T) {
	client, server := newPipeGameConnPair(t)

	const numTypes = 60
	const entriesPerType = 5
	const totalMails = numTypes * entriesPerType // exactly maxMailRewardBatchesPerRun (300)
	const oversizedUIDLen = 30001

	if totalMails != maxMailRewardBatchesPerRun {
		t.Fatalf("test construction bug: totalMails = %d, want exactly maxMailRewardBatchesPerRun (%d)", totalMails, maxMailRewardBatchesPerRun)
	}

	mails := make([]*SFSObject, 0, totalMails)
	uidCounter := 0
	for typ := 0; typ < numTypes; typ++ {
		for i := 0; i < entriesPerType; i++ {
			uid := fmt.Sprintf("%0*d", oversizedUIDLen, uidCounter)
			uidCounter++
			mails = append(mails, newTestMailObj(uid, int32(typ), 0))
		}
	}

	var rewardBatchCount int
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

		for i := 0; i < maxMailBatchesPerLoop; i++ {
			env, err := server.ReadEnvelope()
			if err != nil {
				t.Errorf("read read-status request %d: %v", i, err)
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "mail.read.status.betch" {
				t.Errorf("read-status request %d malformed: %+v ok=%v", i, msg, ok)
				return
			}
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.read.status.betch", resp); err != nil {
				return
			}
		}

		for i := 0; i < totalMails; i++ {
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
			rewardBatchCount++
			resp := NewSFSObject()
			resp.PutBool("success", true)
			if err := server.SendExtension("mail.reward.batch", resp); err != nil {
				return
			}
		}
	}()

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(orig)

	err := ClaimAllMail(client)

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("fake server never finished all expected requests -- ClaimAllMail likely truncated below maxMailRewardBatchesPerRun")
	}

	if err != nil {
		t.Fatalf("ClaimAllMail() = %v, want nil", err)
	}
	if rewardBatchCount != totalMails {
		t.Errorf("server saw %d mail.reward.batch requests, want exactly %d (maxMailRewardBatchesPerRun, the boundary value)", rewardBatchCount, totalMails)
	}
	if logged := buf.String(); strings.Contains(logged, "total mail.reward.batch round trips across all types exceeds sanity ceiling") {
		t.Errorf("expected NO aggregate-ceiling warning for exactly-at-cap input, got log:\n%s", logged)
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

// TestClaimAllMailNetErrorOnFirstTypeAbortsSecondType is the round-19 regression test for the
// rewardLoop label itself (mail.go): the net.Error break inside the reward-claim loop's inner batch
// loop is `break rewardLoop`, explicitly labeled so it exits BOTH the inner per-batch loop and the
// outer per-mail-type loop over byType in one step -- not a plain unlabeled `break`, which would only
// stop the CURRENT type's remaining batches and let the outer `for mailType, uids := range byType`
// loop carry on into the next, still-unprocessed type. Every existing test that drives a net.Error
// through this loop (TestClaimAllMailAbortsRemainingBatchesOnNetError) uses fixture data with only
// one distinct mail type, so weakening the label to a plain break would go completely undetected
// there: with only one type in byType, "abort every other type" and "abort just this type's
// remaining batches" are indistinguishable outcomes. Likewise, the two tests that already use
// multiple distinct types (TestClaimAllMailClaimsRewardsForEachDistinctType and
// TestClaimAllMailRewardLoopContinuesAcrossTypesAfterBusinessError) only ever inject an ordinary
// decoded business errorCode, never a net.Error, so neither exercises the break at all. This test
// closes that gap: two distinct unclaimed-reward mail types (3 and 9, one mail entry each so each
// type's reward-claim loop is exactly one batch), with the underlying connection going net.Error-dead
// starting exactly at the first reward-claim batch response -- i.e. whichever of the two types Go's
// (randomized) map iteration visits first.
//
// Uses recordingConn/scriptedNetErrConn the same way TestClaimAllMailAbortsRemainingBatchesOnNetError
// does, but with two chained canned responses instead of one: a real, valid list-mail response (both
// mail entries, more=false) followed by a real, valid read-status-batch success response, so both the
// ListMail round trip and the single read-status batch genuinely succeed before the reward-claim
// loop is ever reached. Every Read after those two responses are exhausted -- i.e. the response to
// the very first reward-claim batch request, regardless of which type it belongs to -- fails
// immediately with a net.Error. As with the other scriptedNetErrConn-based tests above, this relies
// on the default permanent failure being fakeNetError{} (the zero value, timeout: false -- per the
// round-21 fix to fakeNetError, buildings_orchestration_test.go): since the round-21 fix to
// ClaimAllMail, a Timeout()==true net.Error here would NOT abort the loop at all, so this test's own
// genuinely-dead-connection scenario needs a non-timeout failure to still exercise the labeled break
// -- the Timeout()==true/no-abort case for this exact loop is covered separately by
// TestClaimAllMailRewardLoopContinuesAfterTimeout below.
//
// If the labeled break fires correctly, exactly 3 writes happen: the list-mail request, the
// read-status batch request, and the first type's reward-claim batch request -- which fails and
// aborts the whole rewardLoop before the second type's batch is ever attempted. A weakened plain
// `break` would instead let the outer loop continue into the second type after the first type's
// single-batch inner loop exits, issuing that second type's reward-claim batch request too (which
// also fails against the same already-dead connection, but only after being attempted) -- showing up
// as writeCount()==4, not 3.
func TestClaimAllMailNetErrorOnFirstTypeAbortsSecondType(t *testing.T) {
	rec := &recordingConn{}
	recorder := &GameConn{conn: rec}

	listResp := NewSFSObject()
	arr := NewSFSArray()
	arr.AddSFSObject(newTestMailObj("t3-a", 3, 0)) // rewardStatus=0: unclaimed
	arr.AddSFSObject(newTestMailObj("t9-a", 9, 0)) // rewardStatus=0: unclaimed
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)
	if err := recorder.SendExtension("push.chat.get.system.mails", listResp); err != nil {
		t.Fatalf("build canned list-mail response: %v", err)
	}

	readResp := NewSFSObject()
	readResp.PutBool("success", true)
	if err := recorder.SendExtension("mail.read.status.betch", readResp); err != nil {
		t.Fatalf("build canned read-status response: %v", err)
	}

	fake := &scriptedNetErrConn{remain: rec.buf.Bytes()}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the first type's reward-claim batch fails with a net.Error)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a non-timeout net.Error (the failure that triggered the rewardLoop abort)", err)
	}
	if got := fake.writeCount(); got != 3 {
		t.Errorf("fake connection saw %d writes, want exactly 3 (list-mail + read-status batch + first type's reward-claim batch only -- a net.Error on the first mail type's reward batch must abort the SECOND, not-yet-started type too, not just the first type's remaining batches)", got)
	}
}

// TestClaimAllMailProcessesPartialMailOnListPageTimeout is the round-21 regression test for the
// ListMail-net.Error check right after the ListMail call in ClaimAllMail (mail.go, Fix site 1): a
// Timeout()==true net.Error -- sendAndWait's ordinary "no response within defaultCmdTimeout"
// outcome -- must NOT be treated as proof the connection is dead. Before the round-21 fix, this
// check fired on ANY net.Error, so a single slow page-2 response would make ClaimAllMail skip
// straight to returning, discarding page 1's already-collected mail (see
// TestClaimAllMailSkipsReadStatusOnListMailNetError's own non-timeout coverage of the still-should-
// skip case this must NOT regress).
//
// Uses sequencedConn to script page 1 succeeding with one unclaimed-reward mail entry
// (more=true, queuing page 2), then page 2's response failing with fakeNetError{timeout: true}
// (borrowed directly from buildings_orchestration_test.go -- Timeout()==true, standing in for an
// ordinary slow response), followed by genuinely successful read-status and reward-claim batch
// responses for that one already-collected mail entry.
//
// If the fix holds, ClaimAllMail falls through past the ListMail timeout and still attempts both
// the read-status and reward-claim batches for page 1's mail, so exactly 4 writes happen: the
// page-1 and page-2 chat.get.system.mails requests, one mail.read.status.betch request, and one
// mail.reward.batch request. A reverted fix would stop at exactly 2 writes (page 1 + page 2 only),
// identical to TestClaimAllMailSkipsReadStatusOnListMailNetError's writeCount -- the two tests
// together prove the site now distinguishes Timeout()==true from a genuine net.Error.
func TestClaimAllMailProcessesPartialMailOnListPageTimeout(t *testing.T) {
	page1Resp := NewSFSObject()
	arr := NewSFSArray()
	arr.AddSFSObject(newTestMailObj("uid-1", 3, 0)) // rewardStatus=0: unclaimed reward
	page1Resp.PutSFSArray("msg", arr)
	page1Resp.PutBool("more", true)
	page1Resp.PutUtfString("lastUid", "uid-1")
	page1Resp.PutLong("lastMailTime", 555)

	readResp := NewSFSObject()
	readResp.PutBool("success", true)

	rewardResp := NewSFSObject()
	rewardResp.PutBool("success", true)

	fake := &sequencedConn{turns: []connTurn{
		{bytes: encodeResponse(t, "push.chat.get.system.mails", page1Resp)},
		{err: fakeNetError{timeout: true}}, // page 2: Timeout()==true, standing in for an ordinary slow response
		{bytes: encodeResponse(t, "mail.read.status.betch", readResp)},
		{bytes: encodeResponse(t, "mail.reward.batch", rewardResp)},
	}}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (page 2's list-mail round trip times out)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a Timeout()==true net.Error (the page-2 timeout that must NOT have aborted the run)", err)
	}
	if got := fake.writeCount(); got != 4 {
		t.Errorf("fake connection saw %d writes, want exactly 4 (page-1 + page-2 list-mail, plus the read-status and reward-claim batches for page 1's already-collected mail -- a Timeout()==true net.Error on ListMail must not skip them)", got)
	}
}

// TestClaimAllMailReadStatusContinuesAfterTimeout is the round-21 regression test for the
// read-status batch loop's net.Error check in ClaimAllMail (mail.go, Fix site 2): a
// Timeout()==true net.Error on one batch must NOT abort the remaining read-status batches, and
// must NOT skip the reward-claim loop afterward (readAbortedByNetErr must stay false). Before the
// round-21 fix, this check fired on ANY net.Error, so one slow read-status response would abort
// every other independent batch/loop still pending -- exactly the bug
// TestClaimAllMailAbortsRemainingBatchesOnNetError's own non-timeout scenario must still catch.
//
// Uses 101 same-type unclaimed-reward mail entries -- enough that batchByCountAndBytes'
// readBatchSize=100 item cap splits both the read-status loop and the reward-claim loop into two
// batches each (100 then 1), mirroring TestClaimAllMailItemCountBatching's fixture shape. Scripted
// via sequencedConn: list-mail succeeds, read-status batch 1 (100 uids) fails with a
// Timeout()==true net.Error, read-status batch 2 (1 uid) succeeds, and both reward-claim batches
// succeed.
//
// If the fix holds, all 5 requests are attempted (list-mail + 2 read-status batches + 2
// reward-claim batches), so exactly 5 writes happen. A reverted fix would break out of the
// read-status loop after batch 1's timeout (skipping read-status batch 2) and skip the
// reward-claim loop entirely, showing up as writeCount()==2.
func TestClaimAllMailReadStatusContinuesAfterTimeout(t *testing.T) {
	const total = 101
	const mailType = int32(7)
	var mails []*SFSObject
	for i := 0; i < total; i++ {
		mails = append(mails, newTestMailObj(fmt.Sprintf("uid-%03d", i), mailType, 0)) // rewardStatus=0: unclaimed
	}
	listResp := NewSFSObject()
	arr := NewSFSArray()
	for _, mo := range mails {
		arr.AddSFSObject(mo)
	}
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)

	readSuccess := NewSFSObject()
	readSuccess.PutBool("success", true)

	rewardSuccess := NewSFSObject()
	rewardSuccess.PutBool("success", true)

	fake := &sequencedConn{turns: []connTurn{
		{bytes: encodeResponse(t, "push.chat.get.system.mails", listResp)},
		{err: fakeNetError{timeout: true}},                                // read-status batch 1 (100 uids): Timeout()==true
		{bytes: encodeResponse(t, "mail.read.status.betch", readSuccess)}, // read-status batch 2 (1 uid)
		{bytes: encodeResponse(t, "mail.reward.batch", rewardSuccess)},    // reward-claim batch 1 (100 uids)
		{bytes: encodeResponse(t, "mail.reward.batch", rewardSuccess)},    // reward-claim batch 2 (1 uid)
	}}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (read-status batch 1 times out)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a Timeout()==true net.Error (read-status batch 1's timeout)", err)
	}
	if got := fake.writeCount(); got != 5 {
		t.Errorf("fake connection saw %d writes, want exactly 5 (list-mail + both read-status batches + both reward-claim batches -- a Timeout()==true net.Error on read-status batch 1 must not abort batch 2 or skip the reward-claim loop)", got)
	}
}

// TestClaimAllMailRewardLoopContinuesAfterTimeout is the round-21 regression test for the
// reward-claim loop's labeled net.Error break in ClaimAllMail (mail.go, Fix site 3): a
// Timeout()==true net.Error on one mail type's reward-claim batch must NOT trigger `break
// rewardLoop` -- the outer loop must still visit every other still-unprocessed type in byType.
// Before the round-21 fix, this check fired on ANY net.Error, so one slow reward-claim response
// would abort every other type's reward claim too -- exactly the bug
// TestClaimAllMailNetErrorOnFirstTypeAbortsSecondType's own non-timeout scenario must still catch
// (that test now relies on scriptedNetErrConn's default fakeNetError{} (timeout: false) to keep
// proving the genuinely-dead-connection case).
//
// Two distinct unclaimed-reward mail types (3 and 9), one mail entry each, so each type's
// reward-claim loop is exactly one batch. Both types send their request under the identical cmd
// name "mail.reward.batch" (mail.go's ClaimAllMail), so sequencedConn's turns don't need to know
// which type Go's randomized map iteration visits first: whichever reward-claim batch request
// arrives first gets the Timeout()==true error turn, and whichever arrives second gets the success
// turn.
//
// If the labeled break correctly requires !netErr.Timeout(), all 4 requests are attempted
// (list-mail + read-status + both types' reward-claim batches), so exactly 4 writes happen. A
// reverted fix would abort the whole rewardLoop after the first type's timeout, leaving the second
// type's batch never attempted -- showing up as writeCount()==3, identical to
// TestClaimAllMailNetErrorOnFirstTypeAbortsSecondType's own (correct, non-timeout) writeCount.
func TestClaimAllMailRewardLoopContinuesAfterTimeout(t *testing.T) {
	listResp := NewSFSObject()
	arr := NewSFSArray()
	arr.AddSFSObject(newTestMailObj("t3-a", 3, 0)) // rewardStatus=0: unclaimed
	arr.AddSFSObject(newTestMailObj("t9-a", 9, 0)) // rewardStatus=0: unclaimed
	listResp.PutSFSArray("msg", arr)
	listResp.PutBool("more", false)

	readSuccess := NewSFSObject()
	readSuccess.PutBool("success", true)

	rewardSuccess := NewSFSObject()
	rewardSuccess.PutBool("success", true)

	fake := &sequencedConn{turns: []connTurn{
		{bytes: encodeResponse(t, "push.chat.get.system.mails", listResp)},
		{bytes: encodeResponse(t, "mail.read.status.betch", readSuccess)},
		{err: fakeNetError{timeout: true}},                             // first type's reward-claim batch (whichever type is visited first): Timeout()==true
		{bytes: encodeResponse(t, "mail.reward.batch", rewardSuccess)}, // second type's reward-claim batch
	}}
	client := &GameConn{conn: fake, reader: bufio.NewReaderSize(fake, 4096)}

	err := ClaimAllMail(client)

	if err == nil {
		t.Fatal("ClaimAllMail() = nil, want a non-nil error (the first type's reward-claim batch times out)")
	}
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Errorf("ClaimAllMail() error = %v, want it to wrap a Timeout()==true net.Error (the first type's reward-claim timeout)", err)
	}
	if got := fake.writeCount(); got != 4 {
		t.Errorf("fake connection saw %d writes, want exactly 4 (list-mail + read-status + both types' reward-claim batches -- a Timeout()==true net.Error on the first type's batch must not abort the second, not-yet-started type)", got)
	}
}

// TestListMailWarnsOnWrongTypedMsgField is the regression test for the round-39 fix to ListMail's
// msg-field handling: a response where msg is present but not an *SFSArray (a server-shape anomaly,
// distinct from msg being altogether absent) used to be silently treated the same as absent, with
// no diagnostic signal. It must now log a warning identifying the anomaly, while still completing
// pagination safely (the page yields zero mail entries, and the wrong-typed more field default
// applies exactly as it would for an absent msg).
func TestListMailWarnsOnWrongTypedMsgField(t *testing.T) {
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
		resp.PutUtfString("msg", "not-an-array") // wrong-typed: server-shape anomaly under test
		resp.PutUtfString("lastUid", "cursor-1")
		resp.PutLong("lastMailTime", 999)
		_ = server.SendExtension("push.chat.get.system.mails", resp)
		// Intentionally does not read a second request -- a correct ListMail treats the missing
		// (because wrong-typed) more field as more=false and never sends one.
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
		t.Fatal("ListMail never returned -- it should treat the wrong-typed msg field as yielding zero entries and stop")
	}

	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("fake server goroutine never finished")
	}

	if err != nil {
		t.Fatalf("ListMail() = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want zero mail entries -- the wrong-typed msg field must not fabricate entries", got)
	}
	if logged := buf.String(); !strings.Contains(logged, "response's msg field is present but not an array") {
		t.Errorf("expected a warning mentioning the wrong-typed msg field, got log:\n%s", logged)
	}
}
