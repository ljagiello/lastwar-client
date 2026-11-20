package game

import (
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// WP-Z touch point 1 (MASTER B11, aud-transport/F3): the request helpers that wait by hand pair a
// reply by its `_id`, as the client does (Assembly-CSharp.decompiled.cs:82052-82056), so a late
// reply to an earlier request of the same cmd whose wait timed out is not taken for the pending
// one's.

// lateThenOwnReply makes server read two requests of cmd and then answer the first one late,
// then the second, each echoing its request's `_id` and carrying "which" = "late" / "own".
func lateThenOwnReply(t *testing.T, server *session.GameConn, cmd string) {
	t.Helper()
	go func() {
		first, err := session.ReadNextExtension(server)
		if err != nil {
			return
		}
		second, err := session.ReadNextExtension(server)
		if err != nil {
			return
		}
		for _, r := range []struct {
			req   *session.ExtensionMessage
			which string
		}{{first, "late"}, {second, "own"}} {
			p := sfs.NewSFSObject()
			p.PutInt("_id", r.req.Params.GetInt("_id"))
			p.PutUtfString("which", r.which)
			_ = server.SendExtension(cmd, p)
		}
	}()
}

// earlierRequest sends a first request of cmd whose reply the test never waits for, standing in
// for one whose wait timed out.
func earlierRequest(t *testing.T, client *session.GameConn, cmd string) {
	t.Helper()
	if _, err := client.SendExtensionID(cmd, nil); err != nil {
		t.Fatalf("SendExtensionID: %v", err)
	}
}

func TestSendAllianceDonationSkipsALateReply(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	lateThenOwnReply(t, server, allianceDonateCmd)
	earlierRequest(t, client, allianceDonateCmd)
	reply, err := sendAllianceDonation(client, &allianceTech{id: 10011100})
	if err != nil {
		t.Fatal(err)
	}
	if got := reply.Params.GetString("which"); got != "own" {
		t.Errorf("sendAllianceDonation took the %q reply, want its own", got)
	}
}

func TestDuelSpendGuardExchangeSkipsALateReply(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	const cmd = "hero.lottery"
	lateThenOwnReply(t, server, cmd)
	earlierRequest(t, client, cmd)
	g := &duelSpendGuard{conn: client, feature: "duel-recruit-tickets"}
	reply, err := g.exchange("test", cmd, sfs.NewSFSObject())
	if err != nil {
		t.Fatal(err)
	}
	if got := reply.Params.GetString("which"); got != "own" {
		t.Errorf("exchange took the %q reply, want its own", got)
	}
}

func TestFwRequestSkipsALateReply(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	const cmd = "al.rank"
	lateThenOwnReply(t, server, cmd)
	earlierRequest(t, client, cmd)
	params, code, err := fwRequest(client, "test", cmd, sfs.NewSFSObject())
	if err != nil || code != "" {
		t.Fatalf("fwRequest = %v, %q", err, code)
	}
	if got := params.GetString("which"); got != "own" {
		t.Errorf("fwRequest took the %q reply, want its own", got)
	}
}
