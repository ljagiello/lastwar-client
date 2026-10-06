package game

import (
	"slices"
	"strings"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func helpEntry(kv ...any) *sfs.SFSObject { return obj(append([]any{"helpId", "h"}, kv...)...) }

func TestOpenHelpRequests(t *testing.T) {
	cases := []struct {
		name   string
		own    string
		arr    []*sfs.SFSObject
		listed int
		open   int
	}{
		{"empty list", "1001", nil, 0, 0},
		{"own request only", "1001", []*sfs.SFSObject{helpEntry("senderId", "1001", "nowcount", 0, "maxcount", 5)}, 1, 0},
		{"another member's open request", "1001", []*sfs.SFSObject{helpEntry("senderId", "2002", "nowcount", 1, "maxcount", 5)}, 1, 1},
		{"another member's finished request", "1001", []*sfs.SFSObject{helpEntry("senderId", "2002", "nowcount", 5, "maxcount", 5)}, 1, 0},
		{"senderId Long vs uid string", "1001", []*sfs.SFSObject{helpEntry("senderId", int64(1001), "nowcount", 0, "maxcount", 5)}, 1, 0},
		// CheckStats recomputes stats from senderId, so a server stats=0 on someone else's request
		// does not hide it.
		{"senderId wins over server stats", "1001", []*sfs.SFSObject{helpEntry("senderId", "2002", "stats", 0, "nowcount", 0, "maxcount", 5)}, 1, 1},
		{"own uid unknown: server stats=0 is own", "", []*sfs.SFSObject{helpEntry("senderId", "2002", "stats", 0, "nowcount", 0, "maxcount", 5)}, 1, 0},
		{"own uid unknown: server stats=1", "", []*sfs.SFSObject{helpEntry("senderId", "2002", "stats", 1, "nowcount", 0, "maxcount", 5)}, 1, 1},
		{"no maxcount counts as open", "1001", []*sfs.SFSObject{helpEntry("senderId", "2002")}, 1, 1},
		{"no helpId is skipped", "1001", []*sfs.SFSObject{obj("senderId", "2002", "nowcount", 0, "maxcount", 5)}, 0, 0},
		{"mixed", "1001", []*sfs.SFSObject{
			helpEntry("senderId", "1001", "nowcount", 0, "maxcount", 5),
			helpEntry("senderId", "2002", "nowcount", 5, "maxcount", 5),
			helpEntry("senderId", "3003", "nowcount", 2, "maxcount", 5),
		}, 3, 1},
	}
	for _, c := range cases {
		resp := sfs.NewSFSObject()
		if c.arr != nil {
			resp = obj("helpArr", c.arr)
		}
		listed, open := openHelpRequests(resp, c.own)
		if listed != c.listed || open != c.open {
			t.Errorf("%s: openHelpRequests = (%d, %d), want (%d, %d)", c.name, listed, open, c.listed, c.open)
		}
	}
}

func TestHelpAllianceMembersGating(t *testing.T) {
	withUID := &Init{Raw: obj("user", obj("uid", "1001"))}
	cases := []struct {
		name      string
		in        *Init
		list      *sfs.SFSObject
		want      []string
		wantError bool
	}{
		{"no init: blind help-all", &Init{}, nil, []string{"al.help.all"}, false},
		{"only own request", withUID, obj("helpArr", []*sfs.SFSObject{helpEntry("senderId", "1001", "nowcount", 0, "maxcount", 5)}),
			[]string{"al.show.help"}, false},
		{"another member's open request", withUID, obj("helpArr", []*sfs.SFSObject{helpEntry("senderId", "2002", "nowcount", 0, "maxcount", 5)}),
			[]string{"al.show.help", "al.help.all"}, false},
		{"help list fails", withUID, obj("errorCode", "E1"), []string{"al.show.help"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				if msg.Cmd == "al.show.help" {
					return "", c.list
				}
				return "", nil
			})
			err := helpAllianceMembers(client, c.in)
			if (err != nil) != c.wantError {
				t.Errorf("helpAllianceMembers() error = %v, want error %v", err, c.wantError)
			}
			if got := srv.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("requests = %v, want %v", got, c.want)
			}
			if helps := srv.requests("al.help.all"); len(helps) == 1 && helps[0].Params.GetLong("cmdBaseTime") <= 0 {
				t.Error("al.help.all sent without cmdBaseTime")
			}
		})
	}
}

func TestClaimAllianceGiftsGating(t *testing.T) {
	gifts := &Init{Raw: obj("allianceNewMail", 3)}
	list := func(kv ...any) *sfs.SFSObject { return obj(kv...) }
	const (
		ls  = "alliance.reward.list"
		all = "alliance.reward.allreceive"
	)
	cases := []struct {
		name      string
		in        *Init
		list      *sfs.SFSObject
		wantCmds  []string
		wantTypes []int32
		wantError bool
	}{
		{"no init: both types blind", &Init{}, nil, []string{all, all}, []int32{1, 2}, false},
		{"no gifts waiting", &Init{Raw: obj("allianceNewMail", 0)}, nil, nil, nil, false},
		{"gift level 20, both waiting", gifts, list("onLevel", 20, "info", obj("redPoint1", 2, "redPoint2", 1)),
			[]string{ls, all, all}, []int32{1, 2}, false},
		{"gift level exactly 15", gifts, list("onLevel", 15, "info", obj("redPoint1", 1, "redPoint2", 1)),
			[]string{ls, all, all}, []int32{1, 2}, false},
		{"gift level 14: Regular only", gifts, list("onLevel", 14, "info", obj("redPoint1", 2, "redPoint2", 1)),
			[]string{ls, all}, []int32{2}, false},
		{"gift level unknown: Regular only", gifts, list("info", obj("redPoint1", 2, "redPoint2", 1)),
			[]string{ls, all}, []int32{2}, false},
		{"no Regular waiting", gifts, list("onLevel", 20, "info", obj("redPoint1", 1, "redPoint2", 0)),
			[]string{ls, all}, []int32{1}, false},
		{"nothing waiting per type", gifts, list("onLevel", 20, "info", obj("redPoint1", 0, "redPoint2", 0)),
			[]string{ls}, nil, false},
		{"no counts in the list: both claimed", gifts, list("onLevel", 20),
			[]string{ls, all, all}, []int32{1, 2}, false},
		{"no allianceNewMail in init: list anyway", &Init{Raw: sfs.NewSFSObject()}, list("onLevel", 3),
			[]string{ls, all}, []int32{2}, false},
		{"list fails: Regular only", gifts, list("errorCode", "E1"), []string{ls, all}, []int32{2}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				if msg.Cmd == ls {
					return "", c.list
				}
				return "", obj("receiveResult", 1)
			})
			err := claimAllianceGifts(client, c.in)
			if (err != nil) != c.wantError {
				t.Errorf("claimAllianceGifts() error = %v, want error %v", err, c.wantError)
			}
			if got := srv.cmds(); !slices.Equal(got, c.wantCmds) {
				t.Errorf("requests = %v, want %v", got, c.wantCmds)
			}
			var types []int32
			for _, m := range srv.requests(all) {
				types = append(types, m.Params.GetInt("type"))
			}
			if !slices.Equal(types, c.wantTypes) {
				t.Errorf("claimed types = %v, want %v", types, c.wantTypes)
			}
			for _, m := range srv.requests(ls) {
				if m.Params.GetInt("index") != 0 || m.Params.GetInt("len") != 1000 {
					t.Errorf("alliance.reward.list params = %v, want index=0 len=1000", m.Params)
				}
			}
			if c.wantError && !strings.Contains(err.Error(), "E1") {
				t.Errorf("error = %v, want the list's errorCode", err)
			}
		})
	}
}
