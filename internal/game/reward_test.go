package game

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// rewardFakeReq is one request the claim-feature fake server received.
type rewardFakeReq struct {
	Cmd    string
	Params *sfs.SFSObject
}

// rewardFake records what a feature sent to the fake server started by startRewardFake.
type rewardFake struct {
	mu   sync.Mutex
	reqs []rewardFakeReq
}

func (f *rewardFake) requests() []rewardFakeReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rewardFakeReq(nil), f.reqs...)
}

func (f *rewardFake) cmds() []string {
	var out []string
	for _, r := range f.requests() {
		out = append(out, r.Cmd)
	}
	return out
}

// startRewardFake is the shared fake game server for the claim-feature tests: a real TCP listener
// (session.StartFakeGameServer) whose handler records every extension request and answers it with
// reply(cmd, params) under the same cmd. A nil reply closes the connection, which the client then
// sees as a dead connection (a non-timeout net.Error) on that request.
func startRewardFake(t *testing.T, reply func(cmd string, p *sfs.SFSObject) *sfs.SFSObject) (*session.GameConn, *rewardFake) {
	t.Helper()
	f := &rewardFake{}
	addr := session.StartFakeGameServer(t, func(server *session.GameConn) {
		defer func() { _ = server.Close() }()
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			f.mu.Lock()
			f.reqs = append(f.reqs, rewardFakeReq{Cmd: msg.Cmd, Params: msg.Params})
			f.mu.Unlock()
			resp := reply(msg.Cmd, msg.Params)
			if resp == nil {
				return
			}
			if err := server.SendExtension(msg.Cmd, resp); err != nil {
				return
			}
		}
	})
	conn, err := session.DialGame(addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial fake server: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, f
}

// rewardOK is a success response granting 50 diamonds.
func rewardOK() *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()
	e := sfs.NewSFSObject()
	e.PutInt("type", 5)
	e.PutInt("value", 50)
	e.PutLong("total", 1050)
	arr.AddSFSObject(e)
	resp.PutSFSArray("reward", arr)
	return resp
}

// rewardErr is a response carrying errorCode.
func rewardErr(code string) *sfs.SFSObject {
	resp := sfs.NewSFSObject()
	resp.PutUtfString("errorCode", code)
	return resp
}

// rewardLogBuf is a goroutine-safe log sink for tests that assert on log lines.
type rewardLogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *rewardLogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *rewardLogBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// captureRewardLogs routes slog to a buffer for the rest of the test.
func captureRewardLogs(t *testing.T) *rewardLogBuf {
	t.Helper()
	buf := &rewardLogBuf{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })
	return buf
}

// rewardInit builds an Init whose Raw carries the given top-level fields.
func rewardInit(fill func(raw *sfs.SFSObject)) *Init {
	raw := sfs.NewSFSObject()
	fill(raw)
	return &Init{Raw: raw}
}

func TestDecodeRewardGrantsShapes(t *testing.T) {
	p := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()

	scalar := sfs.NewSFSObject() // diamonds: value = gained, total = new balance
	scalar.PutInt("type", 5)
	scalar.PutInt("value", 50)
	scalar.PutLong("total", 1050)
	arr.AddSFSObject(scalar)

	goods := sfs.NewSFSObject() // goods: value.count is the new total, rewardAdd the gain
	goods.PutInt("type", 7)
	gv := sfs.NewSFSObject()
	gv.PutUtfString("itemId", "200101")
	gv.PutInt("count", 12)
	gv.PutInt("rewardAdd", 2)
	goods.PutSFSObject("value", gv)
	arr.AddSFSObject(goods)

	goodsTotalOnly := sfs.NewSFSObject()
	goodsTotalOnly.PutInt("type", 7)
	gt := sfs.NewSFSObject()
	gt.PutInt("id", 200102)
	gt.PutInt("count", 9)
	goodsTotalOnly.PutSFSObject("value", gt)
	arr.AddSFSObject(goodsTotalOnly)

	hero := sfs.NewSFSObject()
	hero.PutInt("type", 25)
	hv := sfs.NewSFSObject()
	hv.PutInt("heroId", 1201)
	hv.PutInt("num", 1)
	hero.PutSFSObject("value", hv)
	arr.AddSFSObject(hero)

	zero := sfs.NewSFSObject() // dropped, as RewardManager does
	zero.PutInt("type", 1)
	zero.PutInt("value", 0)
	arr.AddSFSObject(zero)

	arr.AddInt(7) // not an object: skipped
	p.PutSFSArray("reward", arr)

	got := decodeRewardGrants(p)
	want := []string{"diamonds +50 (now 1050)", "item 200101 +2 (now 12)", "item 200102 (now 9)", "hero 1201 +1"}
	if len(got) != len(want) {
		t.Fatalf("decoded %d grants (%v), want %d", len(got), got, len(want))
	}
	for i, g := range got {
		if g.String() != want[i] {
			t.Errorf("grant %d = %q, want %q", i, g.String(), want[i])
		}
	}
}

func TestDescribeGrantsFallbacks(t *testing.T) {
	p := sfs.NewSFSObject()
	if got := describeGrants(p); got != "nothing" {
		t.Errorf("empty response = %q, want nothing", got)
	}
	res := sfs.NewSFSObject()
	res.PutLong("money", 12345)
	p.PutSFSObject("resource", res)
	if got := describeGrants(p); got != "resource balances money=12345" {
		t.Errorf("resource-only response = %q", got)
	}

	// Extra reward arrays are read when named.
	q := sfs.NewSFSObject()
	free := sfs.NewSFSArray()
	e := sfs.NewSFSObject()
	e.PutInt("type", 9999)
	e.PutLong("value", 3)
	free.AddSFSObject(e)
	q.PutSFSArray("freeReward", free)
	if got := describeGrants(q, "reward", "freeReward"); got != "type9999 +3" {
		t.Errorf("freeReward = %q, want type9999 +3", got)
	}
}

func TestClaimIntAcceptsWireVariants(t *testing.T) {
	o := sfs.NewSFSObject()
	o.PutInt("i", 7)
	o.PutLong("l", 1791252000000)
	o.PutDouble("d", 42)
	o.PutDouble("frac", 1.5)
	o.PutUtfString("s", " 99 ")
	o.PutUtfString("bad", "x")
	o.PutBool("b", true)
	for key, want := range map[string]int64{"i": 7, "l": 1791252000000, "d": 42, "s": 99} {
		if got, ok := claimInt(o, key); !ok || got != want {
			t.Errorf("claimInt(%s) = %d, %v; want %d", key, got, ok, want)
		}
	}
	for _, key := range []string{"frac", "bad", "b", "missing"} {
		if _, ok := claimInt(o, key); ok {
			t.Errorf("claimInt(%s) ok, want !ok", key)
		}
	}
	if s, ok := claimString(o, "i"); !ok || s != "7" {
		t.Errorf("claimString(int) = %q, %v", s, ok)
	}
	if b, ok := claimBool(o, "b"); !ok || !b {
		t.Errorf("claimBool(bool) = %v, %v", b, ok)
	}
	if b, ok := claimBool(o, "i"); !ok || !b {
		t.Errorf("claimBool(int 7) = %v, %v", b, ok)
	}
}

func TestClaimAndLogLogsGrantedOnlyOnSuccess(t *testing.T) {
	// 120289 is registered as benign for the daily stamina claim (feature_daily_stamina.go).
	logs := captureRewardLogs(t)
	replies := []*sfs.SFSObject{rewardOK(), rewardErr("120289"), rewardErr("999999")}
	conn, _ := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject {
		r := replies[0]
		replies = replies[1:]
		return r
	})

	if _, err := claimAndLog(conn, "test claim", dailyStaminaClaimCmd, sfs.NewSFSObject()); err != nil {
		t.Fatalf("success: %v", err)
	}
	if !strings.Contains(logs.String(), "test claim granted diamonds +50 (now 1050)") {
		t.Errorf("missing granted line in logs:\n%s", logs)
	}
	if _, err := claimAndLog(conn, "benign claim", dailyStaminaClaimCmd, sfs.NewSFSObject()); err != nil {
		t.Fatalf("benign: %v", err)
	}
	if _, err := claimAndLog(conn, "failed claim", dailyStaminaClaimCmd, sfs.NewSFSObject()); err == nil {
		t.Fatal("failure: want an error")
	}
	if out := logs.String(); strings.Contains(out, "benign claim granted") || strings.Contains(out, "failed claim granted") {
		t.Errorf("granted logged for an errorCode response:\n%s", out)
	}
}

func TestClaimEachStopsOnDeadConnection(t *testing.T) {
	calls := 0
	conn, _ := startRewardFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject {
		calls++
		if calls == 2 {
			return nil // drop the connection on the second request
		}
		return rewardErr("999999")
	})
	errs := claimEach([]int{1, 2, 3}, func(int) error {
		_, err := claimAndLog(conn, "loop claim", "reward.test.loop", sfs.NewSFSObject())
		return err
	})
	if len(errs) != 2 {
		t.Fatalf("got %d errors (%v), want 2: one failure, then the dead connection stops the loop", len(errs), errs)
	}
	if !session.ContainsNonTimeoutNetError(errs[1]) {
		t.Errorf("second error %v is not a dead-connection error", errs[1])
	}
}

func TestClaimLongArrayEncodesAsLongArray(t *testing.T) {
	o := sfs.NewSFSObject()
	o.PutValue("uuidList", claimLongArray([]int64{11, 22}))
	o.PutSFSArray("uuidArr", claimLongSFSArray([]int64{33}))
	b, err := sfs.EncodeObject(o)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := sfs.DecodeObject(b)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	v, _ := back.Get("uuidList")
	if v.Type != sfsLongArrayTag {
		t.Errorf("uuidList tag = %d, want LongArray (%d)", v.Type, sfsLongArrayTag)
	}
	if got, ok := v.Val.([]int64); !ok || len(got) != 2 || got[0] != 11 || got[1] != 22 {
		t.Errorf("uuidList = %#v", v.Val)
	}
	arr, _ := back.Get("uuidArr")
	items := arr.Val.(*sfs.SFSArray).Items()
	if len(items) != 1 || items[0].Type != sfs.SFSLong || items[0].Val.(int64) != 33 {
		t.Errorf("uuidArr = %#v", items)
	}
}

func TestDecodeRewardGrantsResourceItemAndNested(t *testing.T) {
	p := sfs.NewSFSObject()
	arr := sfs.NewSFSArray()

	resItem := sfs.NewSFSObject() // 27: count is the new total, addNum the gain
	resItem.PutInt("type", 27)
	rv := sfs.NewSFSObject()
	rv.PutInt("itemId", 630011)
	rv.PutInt("count", 40)
	rv.PutInt("addNum", 5)
	resItem.PutSFSObject("value", rv)
	arr.AddSFSObject(resItem)

	equip := sfs.NewSFSObject() // 38: the entry sits under value.changes[0]
	equip.PutInt("type", 38)
	ev := sfs.NewSFSObject()
	changes := sfs.NewSFSArray()
	ch := sfs.NewSFSObject()
	ch.PutInt("cfgId", 9001)
	ch.PutInt("changeNum", 3)
	changes.AddSFSObject(ch)
	ev.PutSFSArray("changes", changes)
	equip.PutSFSObject("value", ev)
	arr.AddSFSObject(equip)
	p.PutSFSArray("reward", arr)

	got := decodeRewardGrants(p)
	want := []string{"resource_item 630011 +5 (now 40)", "common_equip 9001 +3"}
	if len(got) != len(want) {
		t.Fatalf("decoded %v, want %v", got, want)
	}
	for i, g := range got {
		if g.String() != want[i] {
			t.Errorf("grant %d = %q, want %q", i, g.String(), want[i])
		}
	}
}
