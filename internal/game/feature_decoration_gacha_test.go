package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

func decorationReply(score int32, claimed []int32, pity, wishScore, wishItem int32, wishNum bool) *sfs.SFSObject {
	r := sfs.NewSFSObject()
	r.PutInt("totalScore", score)
	r.PutSFSArray("boxRewards", eventBoxObjs([]string{"index", "target"}, []int32{10, 100}, []int32{20, 300}, []int32{30, 600}))
	r.PutSFSArray("boxReceive", eventBoxInts(claimed...))
	r.PutInt("pity", pity)
	r.PutInt("wishScore", wishScore)
	r.PutInt("wishItem", wishItem)
	if wishNum {
		r.PutInt("wishNum", 1)
	}
	return r
}

// G30: a box whose target totalScore reached and whose own index is not in boxReceive sends
// decoration.receive.box {activityId: Int, index: Int} (ActivityDecorationGachaData.lua:135-159);
// a full pity with a wish chosen sends decoration.receive.wish {activityId: Int} (:278-284, 348-356).
func TestDecorationGachaBoxesAndWish(t *testing.T) {
	withEvNow(t, evTestNow)
	for _, c := range []struct {
		name  string
		reply *sfs.SFSObject
		want  string
	}{
		{"box 20 due, wish full", decorationReply(400, []int32{10}, 50, 50, 7001, true), "decoration.receive.box:20,decoration.receive.wish"},
		{"wish not full", decorationReply(400, []int32{10, 20}, 50, 49, 7001, true), ""},
		{"no wish chosen", decorationReply(0, nil, 50, 60, 0, true), ""},
		{"wish without wishNum", decorationReply(0, nil, 50, 60, 7001, false), ""},
		{"pity 0", decorationReply(0, nil, 0, 60, 7001, true), ""},
	} {
		conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
			if cmd == "hero.event.info.get" {
				return c.reply
			}
			return evOK()
		})
		if err := runDecorationGacha(conn, evInit(30, evActivity(98630))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, r := range fake.requests() {
			if v, _ := r.Params.Get("activityId"); r.Cmd != "hero.event.info.get" && v.Type != sfs.SFSInt {
				t.Errorf("%s: %s activityId %#v, want Int", c.name, r.Cmd, v)
			}
			switch r.Cmd {
			case decorationBoxCmd:
				got = append(got, r.Cmd+":"+evString(r.Params, "index"))
			case decorationWishCmd:
				got = append(got, r.Cmd)
			}
		}
		if s := strings.Join(got, ","); s != c.want {
			t.Errorf("%s: claims %q, want %q", c.name, s, c.want)
		}
	}
}

// G30: no claim command is benign; an errorCode stops the feature.
func TestDecorationGachaErrorStops(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" {
			return decorationReply(700, nil, 50, 50, 7001, true)
		}
		return evErr(evAlreadyExecuted)
	})
	err := runDecorationGacha(conn, evInit(30, evActivity(98630)))
	if err == nil || !strings.Contains(err.Error(), evAlreadyExecuted) {
		t.Errorf("err = %v, want the %s failure", err, evAlreadyExecuted)
	}
	if n := len(fake.requests()); n != 2 {
		t.Errorf("sent %d requests, want the read and one claim", n)
	}
}
