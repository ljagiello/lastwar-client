package session

import (
	"testing"

	"lastwar-client/internal/sfs"
)

// TestShumeiIDKeepsTheLastGoodPush: the client caches the shumeiId of a push.shumei.id unless the
// push carries an errorCode or an empty id (PushShumeiIdMessage.lua:10-18), whichever loop reads
// it; the push still reaches the caller.
func TestShumeiIDKeepsTheLastGoodPush(t *testing.T) {
	client, server := NewPipeGameConnPair(t)
	if got := client.ShumeiID(); got != "" {
		t.Fatalf("ShumeiID before any push = %q, want empty", got)
	}
	var nilConn *GameConn
	if got := nilConn.ShumeiID(); got != "" {
		t.Errorf("nil conn ShumeiID = %q", got)
	}

	pushes := []func() *sfs.SFSObject{
		func() *sfs.SFSObject {
			p := sfs.NewSFSObject()
			p.PutUtfString("shumeiId", "made-up-shumei-1")
			return p
		},
		func() *sfs.SFSObject {
			p := sfs.NewSFSObject()
			p.PutUtfString("errorCode", "E100")
			p.PutUtfString("shumeiId", "made-up-refused")
			return p
		},
		func() *sfs.SFSObject { p := sfs.NewSFSObject(); p.PutUtfString("shumeiId", ""); return p },
		func() *sfs.SFSObject { return sfs.NewSFSObject() },
		func() *sfs.SFSObject { p := sfs.NewSFSObject(); p.PutUtfString("shumeiId", "made-up-other"); return p },
	}
	want := []string{"made-up-shumei-1", "made-up-shumei-1", "made-up-shumei-1", "made-up-shumei-1", "made-up-other"}
	for i, mk := range pushes {
		done := make(chan error, 1)
		go func() { done <- server.SendExtension("push.shumei.id", mk()) }()
		env, err := client.ReadEnvelope()
		if err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if msg, ok := env.AsExtension(); !ok || msg.Cmd != "push.shumei.id" {
			t.Fatalf("push %d: read %+v, want the push passed through to the caller", i, env)
		}
		if got := client.ShumeiID(); got != want[i] {
			t.Errorf("after push %d ShumeiID = %q, want %q", i, got, want[i])
		}
	}

	// Another command carrying a shumeiId is not the push.
	done := make(chan error, 1)
	go func() {
		p := sfs.NewSFSObject()
		p.PutUtfString("shumeiId", "made-up-not-a-push")
		done <- server.SendExtension("push.shumei.other", p)
	}()
	if _, err := client.ReadEnvelope(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := client.ShumeiID(); got != "made-up-other" {
		t.Errorf("ShumeiID after another command = %q, want made-up-other", got)
	}
}
