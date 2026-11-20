package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// letterInit is an Init whose items array holds {uuid, itemId, count, otherPara} entries, the
// shape ItemInfo:UpdateInfo reads; an empty otherPara leaves the key out.
func letterInit(items ...[3]string) *Init {
	in := evInit(30)
	a := sfs.NewSFSArray()
	for i, it := range items {
		e := sfs.NewSFSObject()
		e.PutUtfString("uuid", "uuid-"+it[0]+"-"+string(rune('a'+i)))
		e.PutUtfString("itemId", it[0])
		e.PutInt("count", 1)
		if it[1] == "0" {
			e.PutInt("count", 0)
		}
		if it[2] != "" {
			e.PutUtfString("otherPara", it[2])
		}
		a.AddSFSObject(e)
	}
	in.Raw.PutSFSArray("items", a)
	return in
}

func TestLetterRewardShown(t *testing.T) {
	for in, want := range map[string]bool{
		``:                                 false,
		`not json`:                         false,
		`{"param":{"a":1}}`:                true,
		`{"state":null}`:                   true,
		`{"state":0,"param":{}}`:           true,
		`{"state":"0"}`:                    true,
		`{"state":1}`:                      false,
		`{"state":"1"}`:                    false,
		`{"state":true}`:                   false,
		`[1,2]`:                            false,
		`{"state":0,"expireTime":1700000}`: true,
	} {
		if got := letterRewardShown(in); got != want {
			t.Errorf("letterRewardShown(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestLettersClaimEachUnclaimedLetterOnce(t *testing.T) {
	in := letterInit(
		[3]string{"670003", "1", `{"state":0}`},
		[3]string{"670003", "1", `{"state":0}`}, // a second stack: claimed once
		[3]string{"654125", "1", `{"state":0}`}, // VIP 18 invitation: never
		[3]string{"670004", "1", `{"state":1}`}, // claimed
		[3]string{"670005", "1", ``},            // no otherPara: the window shows no button
		[3]string{"670006", "0", `{}`},          // none left in the bag
		[3]string{"200001", "1", `{}`},          // not a letter
		[3]string{"654120", "1", `{"param":{}}`},
	)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		r := evOK()
		r.PutUtfString("itemId", p.GetString("itemId"))
		return r
	})
	if err := runLetters(conn, in); err != nil {
		t.Fatal(err)
	}
	var sent []string
	for _, r := range fake.only(letterReceiveCmd) {
		if v, _ := r.Params.Get("itemId"); v.Type != sfs.SFSUtfString {
			t.Errorf("itemId type = %d, want UtfString", v.Type)
		}
		sent = append(sent, r.Params.GetString("itemId"))
	}
	if !slices.Equal(sent, []string{"670003", "654120"}) {
		t.Errorf("claimed %v, want 670003 and 654120", sent)
	}
}

func TestLettersStopOnRefusal(t *testing.T) {
	in := letterInit([3]string{"670002", "1", `{}`}, [3]string{"670003", "1", `{}`})
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr("E300001") })
	err := runLetters(conn, in)
	if err == nil || !strings.Contains(err.Error(), "E300001") {
		t.Errorf("err = %v, want the refusal reported", err)
	}
	if got := fake.cmds(); len(got) != 1 {
		t.Errorf("sent %v, want the first claim only", got)
	}
}
