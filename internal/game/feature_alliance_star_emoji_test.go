package game

import (
	"strings"
	"testing"

	"lastwar-client/internal/sfs"
)

// emojiInfo is an emojiThumbsRewardInfo object (AllianceStarManager.lua:413-423).
func emojiInfo(canReward bool, claimedIndex, count int32) *sfs.SFSObject {
	o := sfs.NewSFSObject()
	o.PutBool("canReward", canReward)
	o.PutInt("claimedIndex", claimedIndex)
	o.PutInt("count", count)
	return o
}

func TestAllianceStarEmojiBoxes(t *testing.T) {
	withEvNow(t, evTestNow)
	cases := []struct {
		name    string
		first   *sfs.SFSObject   // in the ceremony reply; nil leaves it out
		replies []*sfs.SFSObject // emojiThumbsRewardInfo of each claim reply
		code    string           // errorCode of every claim reply
		want    int
		wantErr bool
	}{
		{"two boxes", emojiInfo(true, 0, 6), []*sfs.SFSObject{emojiInfo(true, 1, 6), emojiInfo(false, 2, 6)}, "", 2, false},
		{"nothing earned", emojiInfo(false, 0, 0), nil, "", 0, false},
		{"no info", nil, nil, "", 0, false},
		{"bounded at three", emojiInfo(true, 0, 10), []*sfs.SFSObject{emojiInfo(true, 1, 10)}, "", 3, false},
		{"a reply without info stops", emojiInfo(true, 0, 6), []*sfs.SFSObject{nil}, "", 1, false},
		{"a refusal stops", emojiInfo(true, 0, 6), nil, "E_EMOJI", 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := 0
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case allianceStarActivityCmd:
					r := sfs.NewSFSObject()
					r.PutBool("hasCeremony", true)
					return r
				case allianceStarInfoCmd:
					r := starCeremony(true, nil)
					if c.first != nil {
						r.PutSFSObject("emojiThumbsRewardInfo", c.first)
					}
					return r
				case allianceStarEmojiCmd:
					if c.code != "" {
						return evErr(c.code)
					}
					r := sfs.NewSFSObject()
					r.PutSFSArray("rewardInfo", freebieReward())
					if info := c.replies[min(n, len(c.replies)-1)]; info != nil {
						r.PutSFSObject("emojiThumbsRewardInfo", info)
					}
					n++
					return r
				}
				return evErr("unexpected command")
			})
			err := runAllianceStarEmoji(conn, evAllianceInit("alliance_star"))
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want error %v", err, c.wantErr)
			}
			if got := len(fake.only(allianceStarEmojiCmd)); got != c.want {
				t.Errorf("sent %d emoji claims, want %d (%v)", got, c.want, fake.cmds())
			}
			for _, r := range fake.only(allianceStarEmojiCmd) {
				if len(r.Params.Keys()) > 1 { // only the session's _id
					t.Errorf("emoji claim params %s", r.Params.StringRedacted())
				}
			}
			for _, cmd := range fake.cmds() {
				if strings.Contains(cmd, "thumbs") {
					t.Errorf("sent %s", cmd)
				}
			}
		})
	}
}

func TestAllianceStarEmojiGates(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evOK() })
	for _, in := range []*Init{nil, {}, evAllianceInit(), evInit(30)} {
		if err := runAllianceStarEmoji(conn, in); err != nil {
			t.Error(err)
		}
	}
	if len(fake.cmds()) != 0 {
		t.Errorf("sent %v without the ceremony", fake.cmds())
	}
	f, ok := featureRegistry[allianceStarEmojiFeature]
	if !ok || f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("registered %v, DefaultOn %v, summary %q", ok, f.DefaultOn, f.Summary)
	}
}
