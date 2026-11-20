package app

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// TestHandleInteractiveLineSkipsALateReply is WP-Z touch point 1 (MASTER B11, aud-transport/F3)
// for -interactive: after an earlier command's wait timed out, its late reply is still on the
// socket, and the next line's command of the same cmd must log its own reply, paired by `_id` as
// the client pairs them (Assembly-CSharp.decompiled.cs:82052-82056), not the earlier one.
func TestHandleInteractiveLineSkipsALateReply(t *testing.T) {
	client, server := session.NewPipeGameConnPair(t)
	const cmd = "some.command"
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
		}{{first, "late-reply"}, {second, "own-reply"}} {
			p := sfs.NewSFSObject()
			p.PutInt("_id", r.req.Params.GetInt("_id"))
			p.PutUtfString("which", r.which)
			_ = server.SendExtension(cmd, p)
		}
	}()
	if _, err := client.SendExtensionID(cmd, nil); err != nil {
		t.Fatalf("SendExtensionID: %v", err)
	}

	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(orig)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handleInteractiveLine(client, cmd+` {"key":"value"}`)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handleInteractiveLine did not return")
	}
	var received string
	for line := range strings.SplitSeq(buf.String(), "\n") {
		if strings.Contains(line, "received response") {
			received = line
		}
	}
	if !strings.Contains(received, "own-reply") || strings.Contains(received, "late-reply") {
		t.Errorf("logged response %q, want the reply to its own command\nlog:\n%s", received, buf.String())
	}
}
