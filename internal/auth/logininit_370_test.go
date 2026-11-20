package auth

import (
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// loginInitServer answers the login.init active pull through reply and reports the request's
// params (minus _id) to keys.
func loginInitServer(t *testing.T, server *session.GameConn, reply *sfs.SFSObject, keys chan<- []string) {
	t.Helper()
	go func() {
		for {
			env, err := server.ReadEnvelope()
			if err != nil {
				return
			}
			msg, ok := env.AsExtension()
			if !ok || msg.Cmd != "login.init" {
				continue
			}
			keys <- session.ParamKeysWithoutID(msg.Params)
			if reply != nil {
				_ = server.SendExtension("login.init", reply)
			}
			return
		}
	}()
}

// TestWaitForInitPushLoginInitReplyIsTheInitData: the 1.0.370 login.init request carries only _id
// (Net/Msgs/LoginInitCommand.lua has an empty OnCreate), and its reply without errorCode is the init
// data (LoginInitCommand.CSHandleResponse, Assembly-CSharp.decompiled.cs:83023-83030; 1.0.370 audit
// B77). A reply with errorCode is not, and the wait goes on.
func TestWaitForInitPushLoginInitReplyIsTheInitData(t *testing.T) {
	for _, c := range []struct {
		name     string
		reply    func() *sfs.SFSObject
		wantInit bool
	}{
		{"reply with the data", sfs.NewSFSObject, true},
		{"reply with errorCode", func() *sfs.SFSObject {
			o := sfs.NewSFSObject()
			o.PutUtfString("errorCode", "E100")
			return o
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			keys := make(chan []string, 1)
			loginInitServer(t, server, c.reply(), keys)

			_, _, gotInit, err := waitForInitPush(client, 200*time.Millisecond)
			if err != nil {
				t.Fatalf("waitForInitPush: %v", err)
			}
			if gotInit != c.wantInit {
				t.Errorf("gotInit = %v, want %v", gotInit, c.wantInit)
			}
			select {
			case k := <-keys:
				if len(k) != 0 {
					t.Errorf("login.init params = %v, want none besides _id (no dataConfigMd5)", k)
				}
			default:
				t.Fatal("login.init was never sent")
			}
		})
	}
}
