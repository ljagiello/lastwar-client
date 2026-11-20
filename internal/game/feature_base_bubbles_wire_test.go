package game

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// The package-A wire tests (PR #80 round 3, finding 2) pin each opt-in feature's requests as
// literals taken from the 1.0.370 client: the command (Net/Config/MsgDefines.lua), the param keys
// in the order its message's OnCreate puts them, each param's SFS type tag and every constant the
// feature sends. They name no Go constant of the feature, so a typo in one fails here.

// aSent is one request a feature sent.
type aSent struct {
	cmd    string
	params *sfs.SFSObject
}

// aWireParam is one expected param: key, SFS type tag and value.
type aWireParam struct {
	key string
	typ byte
	val any
}

// aWireReq is one expected request. scan marks a request another part of the client sends (the
// shared alliance tile scan): only its command is checked.
type aWireReq struct {
	cmd    string
	params []aWireParam
	scan   bool
}

func aEvSent(f *evFake) []aSent {
	var out []aSent
	for _, r := range f.requests() {
		out = append(out, aSent{r.Cmd, r.Params})
	}
	return out
}

func aRewardSent(f *rewardFake) []aSent {
	var out []aSent
	for _, r := range f.requests() {
		out = append(out, aSent{r.Cmd, r.Params})
	}
	return out
}

func aVisitorSent(reqs []visitorRequest) []aSent {
	var out []aSent
	for _, r := range reqs {
		out = append(out, aSent(r))
	}
	return out
}

// aCheckWire checks that sent is exactly want: the commands in order and, for each request but a
// scan one, its param keys in put order (the server's _id aside), each param's type tag (GetInt
// would also accept a Long) and its value.
func aCheckWire(t *testing.T, sent []aSent, want []aWireReq) {
	t.Helper()
	var got, exp []string
	for _, s := range sent {
		got = append(got, s.cmd)
	}
	for _, w := range want {
		exp = append(exp, w.cmd)
	}
	if !slices.Equal(got, exp) {
		t.Errorf("sent %v, want %v", got, exp)
		return
	}
	for i, w := range want {
		if w.scan {
			continue
		}
		p := sent[i].params
		var keys []string
		for _, wp := range w.params {
			keys = append(keys, wp.key)
		}
		if got := session.ParamKeysWithoutID(p); !slices.Equal(got, keys) {
			t.Errorf("request %d %s: keys %v, want %v", i, w.cmd, got, keys)
		}
		for _, wp := range w.params {
			v, ok := p.Get(wp.key)
			if !ok || v.Type != wp.typ || v.Val != wp.val {
				t.Errorf("request %d %s: %s = %#v (type %d, present %v), want %#v of type %d", i, w.cmd, wp.key, v.Val, v.Type, ok,
					wp.val, wp.typ)
			}
		}
	}
}

// TestBaseBubblesWire runs base-bubbles on bbBlockReply and bbInit with Thanksgiving 99302 and
// Valentine 99137 open (table activity 40413: types 321 and 332), answering by literal command.
//   - valentine.owner.base.reward (MsgDefines.lua:2331; sent at BuildingGetRewardBubbleEffect.lua:220)
//     {activityId: Int, statusId: Int} (ValentineOwnerBaseRewardMessage.lua:4-8): lw_status 520526's
//     activity_id 99137 and the status on the player's own HQ.
//   - thanksgiving.info (MsgDefines.lua:1687; sent at ActivityListDataManager.lua:439)
//     {activityId: Int} (ThanksgivingInfoMessage.lua:4-7).
//   - thanksgiving.receive.base.reward (MsgDefines.lua:1701; sent at BuildingRewardBubbleEffect.lua:218)
//     {activityId: Int, otherUid: UtfString, statusId: Int} (ThanksgivingReceiveBaseRewardMessage.lua:4-9):
//     the type2-31 status 500521 on ally A's HQ.
func TestBaseBubblesWire(t *testing.T) {
	withEvNow(t, evTestNow)
	withFwPace(t)
	conn, fake := startBbFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "thanksgiving.info":
			return bbInfoReply(p)
		case "thanksgiving.receive.base.reward", "valentine.owner.base.reward":
			return fwClaimOK(p)
		}
		return evErr("unexpected command")
	})
	if err := runBaseBubbles(conn, bbInit(3, 99302, 99137)); err != nil {
		t.Fatal(err)
	}
	aCheckWire(t, aEvSent(fake), []aWireReq{
		{cmd: "al.rank", scan: true},
		{cmd: "world.get.block", scan: true},
		{cmd: "world.get.block", scan: true},
		{cmd: "valentine.owner.base.reward", params: []aWireParam{
			{"activityId", sfs.SFSInt, int32(99137)}, {"statusId", sfs.SFSInt, int32(520526)}}},
		{cmd: "thanksgiving.info", params: []aWireParam{{"activityId", sfs.SFSInt, int32(99302)}}},
		{cmd: "thanksgiving.receive.base.reward", params: []aWireParam{
			{"activityId", sfs.SFSInt, int32(99302)}, {"otherUid", sfs.SFSUtfString, fwAllyA}, {"statusId", sfs.SFSInt, int32(500521)}}},
	})
}

// aBbBlock is a world.get.block reply holding the given HQ points (pbHQStatus).
func aBbBlock(hqs ...[]byte) *sfs.SFSObject {
	pts := sfs.NewSFSArray()
	for _, hq := range hqs {
		pts.AddByteArray(hq)
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("points", pts)
	r.PutLong("timeStamp", evTestNow.UnixMilli())
	return r
}

// aBbInit is bbInit built field by field: the activities open, init activityReceiveArr, the day's
// thanksgiving_times maps (each {"<activityId>": n}) and, when expire is set, thanksgiving_times_expire
// 14 hours after evTestNow.
func aBbInit(acts []int32, received []string, times []map[string]int32, expire bool) *Init {
	in := fwTestInit()
	m := in.Object("activity")
	for _, id := range acts {
		m.PutSFSObject(strconv.Itoa(int(id)), evActivity(id))
	}
	setBbReceived(in, received...)
	tt := sfs.NewSFSArray()
	for _, mp := range times {
		o := sfs.NewSFSObject()
		for k, n := range mp {
			o.PutInt(k, n)
		}
		tt.AddSFSObject(o)
	}
	in.Raw.PutSFSArray("thanksgiving_times", tt)
	if expire {
		in.Raw.PutLong("thanksgiving_times_expire", evTestNow.Add(14*time.Hour).UnixMilli())
	}
	return in
}

// aBbClaims runs base-bubbles against block and in, answering thanksgiving.info with bbInfoReply and
// each claim with claim(n, p) (n counting claims from 1), and returns the claims sent after the
// scan: "info", "tg <statusId>" or "val <statusId>".
func aBbClaims(t *testing.T, block *sfs.SFSObject, in *Init, claim func(n int, p *sfs.SFSObject) *sfs.SFSObject) []string {
	t.Helper()
	blocks := fwBlockAnswer(t, block)
	n := 0
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		switch cmd {
		case "al.rank":
			return fwRankReply()
		case "world.get.block":
			return blocks()
		case "thanksgiving.info":
			return bbInfoReply(p)
		case "thanksgiving.receive.base.reward", "valentine.owner.base.reward":
			n++
			return claim(n, p)
		}
		return evErr("unexpected command")
	})
	if err := runBaseBubbles(conn, in); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range fake.requests() {
		switch r.Cmd {
		case "thanksgiving.info":
			got = append(got, "info")
		case "thanksgiving.receive.base.reward":
			got = append(got, "tg "+strconv.Itoa(int(r.Params.GetInt("statusId"))))
		case "valentine.owner.base.reward":
			got = append(got, "val "+strconv.Itoa(int(r.Params.GetInt("statusId"))))
		}
	}
	return got
}
