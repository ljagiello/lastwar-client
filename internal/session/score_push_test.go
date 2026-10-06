package session

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func TestScorePushIsLoggedWithTheLastCommand(t *testing.T) {
	var buf bytes.Buffer
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(orig)

	client, server := NewPipeGameConnPair(t)
	done := make(chan error, 1)
	go func() {
		if _, err := ReadNextExtension(server); err != nil {
			done <- err
			return
		}
		push := sfs.NewSFSObject()
		push.PutInt("type", 15)
		push.PutLong("addScore", 10000)
		done <- server.SendExtension(scorePushCmd, push)
	}()
	if err := client.SendExtension("receive.detect.event.reward", sfs.NewSFSObject()); err != nil {
		t.Fatal(err)
	}
	env, err := client.ReadEnvelope()
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if msg, ok := env.AsExtension(); !ok || msg.Cmd != scorePushCmd {
		t.Fatalf("read %+v, want the score push passed through to the caller", env)
	}
	out := buf.String()
	for _, want := range []string{"activity score obtained", "afterCmd=receive.detect.event.reward", "addScore=10000"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q is missing %q", out, want)
		}
	}
}
