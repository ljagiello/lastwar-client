package session

import (
	"sync"
	"testing"

	"lastwar-client/internal/sfs"
)

// readIDs reads n extension requests off server and returns their `_id` values in arrival order,
// failing the test if any request has no Int `_id`.
func readIDs(t *testing.T, server *GameConn, n int) []int32 {
	t.Helper()
	ids := make([]int32, 0, n)
	for range n {
		msg, err := ReadNextExtension(server)
		if err != nil {
			t.Fatalf("read request %d: %v", len(ids)+1, err)
		}
		v, ok := msg.Params.Get("_id")
		if !ok {
			t.Fatalf("request %q has no _id: %s", msg.Cmd, msg.Params.StringRedacted())
		}
		id, ok := v.Val.(int32)
		if !ok || v.Type != sfs.SFSInt {
			t.Fatalf("request %q _id = %v (%T, type %d), want an SFS Int", msg.Cmd, v.Val, v.Val, v.Type)
		}
		ids = append(ids, id)
	}
	return ids
}

// sendAll sends each cmd in order from a goroutine (net.Pipe is synchronous, so the reader must
// run concurrently) and returns a channel that yields the first send error, or nil.
func sendAll(client *GameConn, sends func(*GameConn) error) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- sends(client) }()
	return errc
}

// TestSendExtensionNumbersRequestsAfterLogin covers MASTER.md 5.1 #19: every extension request
// carries an Int `_id` from one per-connection sequence that starts at 2, because Login took 1.
func TestSendExtensionNumbersRequestsAfterLogin(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	errc := sendAll(client, func(c *GameConn) error {
		for _, cmd := range []string{"lw.pve.idle.reward", "al.help.all", "vip.add.login.score"} {
			if err := c.SendExtension(cmd, nil); err != nil {
				return err
			}
		}
		return nil
	})
	got := readIDs(t, server, 3)
	if err := <-errc; err != nil {
		t.Fatalf("SendExtension: %v", err)
	}
	want := []int32{2, 3, 4}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("_id sequence = %v, want %v", got, want)
		}
	}
}

// TestSendExtensionKeepsExplicitIDAndAdvances: login.go's login.init fallback sets `_id` 2 itself.
// That value must reach the wire unchanged, and the sequence must still advance past it so the
// next request is 3, not a second 2.
func TestSendExtensionKeepsExplicitIDAndAdvances(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	errc := sendAll(client, func(c *GameConn) error {
		initReq := sfs.NewSFSObject()
		initReq.PutInt("_id", 2)
		initReq.PutUtfString("dataConfigMd5", "")
		if err := c.SendExtension("login.init", initReq); err != nil {
			return err
		}
		custom := sfs.NewSFSObject()
		custom.PutInt("_id", 77)
		if err := c.SendExtension("operator.cmd", custom); err != nil {
			return err
		}
		return c.SendExtension("visitor.list", nil)
	})
	got := readIDs(t, server, 3)
	if err := <-errc; err != nil {
		t.Fatalf("SendExtension: %v", err)
	}
	if want := []int32{2, 77, 4}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("_id sequence = %v, want %v (explicit ids kept, counter still consumed)", got, want)
	}
}

// TestSendExtensionDoesNotModifyCallerParams: the `_id` goes on a copy, so a params object reused
// for a retry gets a fresh id each time instead of carrying the first send's id forward as if the
// caller had set it, and the caller never sees a key it didn't put.
func TestSendExtensionDoesNotModifyCallerParams(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	params := sfs.NewSFSObject()
	params.PutLong("uuid", 12345)

	errc := sendAll(client, func(c *GameConn) error {
		if err := c.SendExtension("building.production.collect", params); err != nil {
			return err
		}
		return c.SendExtension("building.production.collect", params)
	})
	first, err := ReadNextExtension(server)
	if err != nil {
		t.Fatalf("read first request: %v", err)
	}
	second, err := ReadNextExtension(server)
	if err != nil {
		t.Fatalf("read second request: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("SendExtension: %v", err)
	}

	if params.Has("_id") {
		t.Errorf("caller's params gained an _id: %s", params.StringRedacted())
	}
	if keys := params.Keys(); len(keys) != 1 || keys[0] != "uuid" {
		t.Errorf("caller's params keys = %v, want [uuid]", keys)
	}
	if got := first.Params.GetLong("uuid"); got != 12345 {
		t.Errorf("first request uuid = %d, want 12345", got)
	}
	if a, b := first.Params.GetInt("_id"), second.Params.GetInt("_id"); a != 2 || b != 3 {
		t.Errorf("reused params got _id %d then %d, want 2 then 3", a, b)
	}
	if keys := first.Params.Keys(); len(keys) != 2 || keys[0] != "uuid" || keys[1] != "_id" {
		t.Errorf("wire params keys = %v, want [uuid _id] (_id appended, like PutInt on the real client's object)", keys)
	}
}

// TestSendExtensionSequenceIsPerConnection: the real client's FutureManager.reset() zeroes the
// counter on reconnect. Here every dial makes a new GameConn, so each starts again at 2.
func TestSendExtensionSequenceIsPerConnection(t *testing.T) {
	for range 2 {
		client, server := NewPipeGameConnPair(t)
		errc := sendAll(client, func(c *GameConn) error { return c.SendExtension("al.help.all", nil) })
		got := readIDs(t, server, 1)
		if err := <-errc; err != nil {
			t.Fatalf("SendExtension: %v", err)
		}
		if got[0] != 2 {
			t.Fatalf("first request on a new connection has _id %d, want 2", got[0])
		}
	}
}

// TestSendExtensionConcurrentIDsAreUnique: SendExtension can be called from more than one
// goroutine (the interactive FIFO and a collect loop share a conn), so ids must never repeat.
func TestSendExtensionConcurrentIDsAreUnique(t *testing.T) {
	client, server := NewPipeGameConnPair(t)

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_ = client.SendExtension("al.help.all", nil)
		}()
	}
	got := readIDs(t, server, n)
	wg.Wait()

	seen := make(map[int32]bool, n)
	for _, id := range got {
		if id < 2 || id > n+1 || seen[id] {
			t.Fatalf("ids %v: %d is out of range 2..%d or repeated", got, id, n+1)
		}
		seen[id] = true
	}
}
