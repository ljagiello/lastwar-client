package game

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

func TestAllianceDonateAllIsRegisteredOptIn(t *testing.T) {
	f, ok := featureRegistry["alliance-donate-all"]
	if !ok {
		t.Fatal("alliance-donate-all is not registered")
	}
	if f.DefaultOn {
		t.Error("alliance-donate-all must be DefaultOn: false (it spends resources)")
	}
	if !strings.HasPrefix(f.Summary, "OPT-IN:") {
		t.Errorf("Summary %q must say opt-in", f.Summary)
	}
}

// TestDonateAllAllianceTech scripts science.data.refresh (tech 555 recommended), then
// al.science.refreshNum and each al.science.donate reply from a list of useNum values (-1 = no
// useNum field, -2 = errorCode 120471, -3 = a genuine errorCode), and checks how many donations go
// out.
func TestDonateAllAllianceTech(t *testing.T) {
	cases := []struct {
		name        string
		recommended bool
		useNums     []int // refreshNum reply first, then one per donate reply
		wantDonates int
		wantError   bool
	}{
		{"three charges", true, []int{3, 2, 1, 0}, 3, false},
		{"no charges", true, []int{0}, 0, false},
		{"charges used up mid-run (120471)", true, []int{2, -2}, 1, false},
		{"donate reply without useNum", true, []int{5, -1}, 1, false},
		{"refreshNum without useNum", true, []int{-1}, 0, false},
		{"genuine donate failure", true, []int{3, 2, -3}, 2, true},
		{"capped at the pool size", true, []int{50}, allianceDonateMaxPerRun, false},
		{"nothing recommended", false, nil, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			var mu sync.Mutex
			step := 0
			charges := func() *sfs.SFSObject {
				mu.Lock()
				defer mu.Unlock()
				n := c.useNums[len(c.useNums)-1]
				if step < len(c.useNums) {
					n = c.useNums[step]
				}
				step++
				switch n {
				case -1:
					return obj("maxNum", 30)
				case -2:
					return obj("errorCode", "120471")
				case -3:
					return obj("errorCode", "999999")
				}
				return obj("maxNum", 30, "useNum", n, "scienceId", 555)
			}
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				switch msg.Cmd {
				case "science.data.refresh":
					state := int32(0)
					if c.recommended {
						state = 1
					}
					return "", allianceScienceRefreshResponse(allianceScienceEntry(111, 0), allianceScienceEntry(555, state))
				case "al.science.refreshNum", "al.science.donate":
					return "", charges()
				}
				return "", nil
			})

			err := donateAllAllianceTech(client)
			if (err != nil) != c.wantError {
				t.Errorf("donateAllAllianceTech() error = %v, want error %v", err, c.wantError)
			}
			donates := srv.requests("al.science.donate")
			if len(donates) != c.wantDonates {
				t.Errorf("sent %d al.science.donate, want %d (requests %v)", len(donates), c.wantDonates, srv.cmds())
			}
			for _, m := range donates {
				if m.Params.GetInt("scienceId") != 555 || m.Params.GetInt("option") != 1 {
					t.Errorf("donate params = %v, want scienceId=555 option=1", m.Params)
				}
			}
			refresh := srv.requests("al.science.refreshNum")
			if c.recommended && (len(refresh) != 1 || refresh[0].Params.GetInt("scienceId") != 555) {
				t.Errorf("al.science.refreshNum requests = %v, want one with scienceId=555", refresh)
			}
			if !c.recommended && !slices.Equal(srv.cmds(), []string{"science.data.refresh"}) {
				t.Errorf("requests = %v, want only science.data.refresh", srv.cmds())
			}
		})
	}
}
