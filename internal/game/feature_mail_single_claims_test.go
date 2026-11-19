package game

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// startMailPlanFake answers chat.get.system.mails with one page of mails on its push command and
// every other request with success under its own command, recording what it received.
func startMailPlanFake(t *testing.T, mails []*sfs.SFSObject) (*session.GameConn, *evFake) {
	t.Helper()
	client, server := session.NewPipeGameConnPair(t)
	f := &evFake{}
	go func() {
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, evReq{msg.Cmd, msg.Params})
			f.mu.Unlock()
			cmd, resp := msg.Cmd, evOK()
			if msg.Cmd == "chat.get.system.mails" {
				cmd, resp = "push.chat.get.system.mails", sfs.NewSFSObject()
				arr := sfs.NewSFSArray()
				for _, m := range mails {
					arr.AddSFSObject(m)
				}
				resp.PutSFSArray("msg", arr)
				resp.PutBool("more", false)
			}
			if err := server.SendExtension(cmd, resp); err != nil {
				return
			}
		}
	}()
	return client, f
}

// mailPlanMails is a mailbox with one unclaimed type-112 mail and every near miss: a claimed and a
// saved 112, a translation-rating 134, and an ordinary activity mail.
func mailPlanMails() []*sfs.SFSObject {
	want := newTestMailObj("m112", mailCantClaimAllType, 0)
	want.PutUtfString("title", `{"h":{"title":"mail_title_112"}}`)
	want.PutUtfString("contents", `{"b":{"reward":{"rewardVersion":"1.0.380","rewardLevel":25,"rewardInfo":[]}}}`)
	claimed := newTestMailObj("m112-claimed", mailCantClaimAllType, 1)
	saved := newTestMailObj("m112-saved", mailCantClaimAllType, 0)
	saved.PutInt("saveFlag", 1)
	return []*sfs.SFSObject{want, claimed, saved, newTestMailObj("m134", 134, 0), newTestMailObj("m4", 2, 0)}
}

// TestMailSingleClaimsPlanLogsType112AndClaimsNothing is MASTER 1.0.370 G53 step 1: an unsaved
// type-112 (CANT_CLAIM_ALL) mail with rewardStatus 0 is logged with its uid, title and the reward's
// rewardVersion / rewardLevel, and nothing is claimed: whether these mails may be claimed waits for
// a live sample. Saved, claimed and other-type mail is not logged.
func TestMailSingleClaimsPlanLogsType112AndClaimsNothing(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	conn, fake := startMailPlanFake(t, mailPlanMails())
	if err := runMailSingleClaimsPlan(conn, evInit(30)); err != nil {
		t.Fatal(err)
	}
	if got := fake.cmds(); len(got) != 1 || got[0] != "chat.get.system.mails" {
		t.Errorf("sent %v, want only the mail list read (log only, no claim)", got)
	}
	var lines []string // the feature's own lines (the session also logs the list reply)
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, mailSingleClaimsFeature+":") {
			lines = append(lines, line)
		}
	}
	out := strings.Join(lines, "\n")
	for _, want := range []string{"uid=m112 ", "mail_title_112", "hasRewardVersion=true", "rewardVersion=1.0.380",
		"rewardLevel=25", "hqLevel=30", "count=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"m112-claimed", "m112-saved", "m134", "uid=m4"} {
		if strings.Contains(out, not) {
			t.Errorf("logged %q, which is not an unclaimed unsaved type-112 mail:\n%s", not, out)
		}
	}
}

// TestMailSingleClaimsPlanReusesTheCoreMailList: after the core ClaimAllMail listed the mailbox on a
// connection, the plan reads that list instead of paging the mailbox again, and the 112 mail is in
// no claim.
func TestMailSingleClaimsPlanReusesTheCoreMailList(t *testing.T) {
	conn, fake := startMailPlanFake(t, mailPlanMails())
	if err := ClaimAllMail(conn); err != nil {
		t.Fatal(err)
	}
	if err := runMailSingleClaimsPlan(conn, evInit(30)); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.only("chat.get.system.mails")); n != 1 {
		t.Errorf("listed the mailbox %d times, want once", n)
	}
	for _, r := range fake.only("mail.reward.batch") {
		if strings.Contains(r.Params.GetString("uids"), "m112") {
			t.Errorf("mail.reward.batch %v claimed a type-112 mail", r.Params)
		}
	}
}

// TestMailContentsJoinsContentsArr: the reward JSON is read where MailInfo:ParseBaseData reads it:
// contentsArr joined with no separator, else contentsLocal, else contents (MailInfo.lua:96-105).
func TestMailContentsJoinsContentsArr(t *testing.T) {
	m := newTestMailObj("u", mailCantClaimAllType, 0)
	m.PutUtfString("contents", "contents")
	if got := mailContents(Mail{Raw: m}); got != "contents" {
		t.Errorf("contents only: %q", got)
	}
	m.PutUtfString("contentsLocal", "local")
	if got := mailContents(Mail{Raw: m}); got != "local" {
		t.Errorf("contentsLocal over contents: %q", got)
	}
	arr := sfs.NewSFSArray()
	arr.AddValue(sfs.SFSValue{Type: sfs.SFSUtfString, Val: `{"b":`})
	arr.AddValue(sfs.SFSValue{Type: sfs.SFSUtfString, Val: `{}}`})
	m.PutSFSArray("contentsArr", arr)
	if got := mailContents(Mail{Raw: m}); got != `{"b":{}}` {
		t.Errorf("contentsArr first, joined with no separator: %q", got)
	}
}

func TestMailSingleClaimsPlanIsOptInAndStaticOnly(t *testing.T) {
	f, ok := featureRegistry[mailSingleClaimsFeature]
	if !ok {
		t.Fatal("mail-single-claims-plan is not registered")
	}
	if f.DefaultOn {
		t.Error("DefaultOn; a new feature must be opt-in until validated live")
	}
	if !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("Summary = %q, want it to end with (static-only, not validated live)", f.Summary)
	}
}
