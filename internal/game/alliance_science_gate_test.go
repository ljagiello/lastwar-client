package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// techEntry is a science.data.refresh allianceScience entry with the client's field names
// (AllianceScienceData.lua:62-143): scienceId and state 1, then any of currentPro, needPro, level,
// finishTime, lowRes and lowNum.
func techEntry(id int32, kv ...any) *sfs.SFSObject {
	return obj(append([]any{"scienceId", id, "state", 1}, kv...)...)
}

// inAllianceInit is an init push showing the alliance al-1, plus kv.
func inAllianceInit(kv ...any) *Init {
	return &Init{Raw: obj(append([]any{"alliance", obj("uid", "al-1")}, kv...)...)}
}

func TestAllianceTechBlocked(t *testing.T) {
	now := time.Now()
	wood := func(n int64) allianceLedger { return newAllianceLedger(&Init{Raw: obj("resource", obj("wood", n))}) }
	cases := []struct {
		name   string
		entry  *sfs.SFSObject
		ledger allianceLedger
		want   string
	}{
		{"nothing but the id is unknown, so nothing gates", techEntry(555), nil, ""},
		{"live sample: 4951850 / 8000000", techEntry(10011900, "currentPro", int64(4951850), "needPro", int64(8000000)), nil, ""},
		{"full", techEntry(10011900, "currentPro", int64(8000000), "needPro", int64(8000000)), nil, "fully donated"},
		{"over full (a crit donation)", techEntry(10011900, "currentPro", int64(8100000), "needPro", int64(8000000)), nil, "fully donated"},
		{"progress unknown", techEntry(10011900, "needPro", int64(8000000)), nil, ""},
		// needPro <= 0 falls back to the template at scienceId + level: row 10011201 is 300000.
		{"needPro 0: template target reached", techEntry(10011200, "currentPro", 300000, "needPro", 0, "level", 1), nil, "fully donated"},
		{"needPro 0: template target not reached", techEntry(10011200, "currentPro", 299999, "needPro", 0, "level", 1), nil, ""},
		{"needPro missing: template target reached", techEntry(10011200, "currentPro", 300000, "level", 1), nil, "fully donated"},
		{"needPro 0, level unknown", techEntry(10011200, "currentPro", 99999999, "needPro", 0), nil, ""},
		{"needPro 0, no template row", techEntry(555, "currentPro", 99999999, "needPro", 0, "level", 1), nil, ""},
		{"max level (10)", techEntry(10011200, "level", 10, "currentPro", 0, "needPro", 100), nil, "max level"},
		{"below max level", techEntry(10011200, "level", 9, "currentPro", 0, "needPro", 100), nil, ""},
		{"one-level tech at level 1", techEntry(10011100, "level", 1), nil, "max level"},
		{"level of a tech not in the table", techEntry(555, "level", 99), nil, ""},
		{"being researched", techEntry(10011900, "finishTime", now.Add(time.Hour).UnixMilli()), nil, "being researched"},
		{"research finished", techEntry(10011900, "finishTime", now.Add(-time.Hour).UnixMilli()), nil, ""},
		{"too little wood", techEntry(10011900, "lowRes", 2, "lowNum", 500), wood(499), "too few resources"},
		{"just enough wood", techEntry(10011900, "lowRes", 2, "lowNum", 500), wood(500), ""},
		{"wood balance unknown", techEntry(10011900, "lowRes", 2, "lowNum", 500), newAllianceLedger(&Init{Raw: obj()}), ""},
		{"cost unknown", techEntry(10011900, "lowRes", 2), wood(0), ""},
		{"a resource type with no init key", techEntry(10011900, "lowRes", 99, "lowNum", 500), wood(0), ""},
		{"flint (13)", techEntry(10011900, "lowRes", 13, "lowNum", 500),
			newAllianceLedger(&Init{Raw: obj("resource", obj("flint", int64(10)))}), "too few resources"},
	}
	for _, c := range cases {
		tech := &allianceTech{id: c.entry.GetInt("scienceId")}
		tech.update(c.entry)
		if got := tech.blocked(now.UnixMilli(), c.ledger); got != c.want {
			t.Errorf("%s: blocked = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAllianceTechUpdateKeepsMissingFields pins the ParseData rule a donate reply relies on: a
// message without a field leaves the known value alone, and only a message for this tech counts.
func TestAllianceTechUpdateKeepsMissingFields(t *testing.T) {
	tech := &allianceTech{id: 10011900}
	tech.update(techEntry(10011900, "currentPro", 100, "needPro", 1000, "level", 3))
	tech.apply(obj("scienceId", 10011900, "useNum", 5, "maxNum", 30)) // the live reply's shape
	if !tech.hasCurrentPro || tech.currentPro != 100 || tech.needPro != 1000 || !tech.hasLevel || tech.level != 3 {
		t.Fatalf("a reply without progress fields changed the record: %+v", tech)
	}
	tech.apply(obj("scienceId", 111, "currentPro", 1000))
	if tech.currentPro != 100 {
		t.Errorf("another tech's message changed the record: %+v", tech)
	}
	tech.apply(obj("currentPro", 1000))
	if tech.currentPro != 100 {
		t.Errorf("a message without scienceId changed the record: %+v", tech)
	}
	tech.apply(obj("scienceId", 10011900, "currentPro", 1000))
	if tech.currentPro != 1000 || tech.blocked(0, nil) != "fully donated" {
		t.Errorf("this tech's progress was not applied: %+v", tech)
	}
}

// TestDonateRecommendedAllianceTechGating runs collectCore's donation against a scripted tree and
// checks which requests go out.
func TestDonateRecommendedAllianceTechGating(t *testing.T) {
	const (
		refresh = "science.data.refresh"
		donate  = "al.science.donate"
	)
	open := techEntry(10011900, "currentPro", int64(4951850), "needPro", int64(8000000), "level", 3, "lowRes", 2, "lowNum", 400)
	full := techEntry(10011900, "currentPro", int64(8000000), "needPro", int64(8000000), "level", 3)
	cases := []struct {
		name string
		in   *Init
		tech *sfs.SFSObject
		want []string
	}{
		{"no init: donates", nil, open, []string{refresh, donate}},
		{"no init: a full tech is still skipped", nil, full, []string{refresh}},
		{"in an alliance", inAllianceInit(), open, []string{refresh, donate}},
		{"full", inAllianceInit(), full, []string{refresh}},
		{"not in an alliance", &Init{Raw: obj("user", obj("uid", "1001"))}, open, nil},
		{"empty alliance object", &Init{Raw: obj("alliance", obj())}, open, nil},
		{"science switch off", inAllianceInit("dataConfig", obj(allianceScienceEntrance, 0)), open, nil},
		{"science switch garbled", inAllianceInit("dataConfig", obj(allianceScienceEntrance, "x")), open, nil},
		{"science switch on", inAllianceInit("dataConfig", obj(allianceScienceEntrance, "1")), open, []string{refresh, donate}},
		{"science switch absent", inAllianceInit("dataConfig", obj("other_switch", 0)), open, []string{refresh, donate}},
		{"too little wood", inAllianceInit("resource", obj("wood", int64(399))), open, []string{refresh}},
		{"enough wood", inAllianceInit("resource", obj("wood", int64(400))), open, []string{refresh, donate}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				if msg.Cmd == refresh {
					return "", allianceScienceRefreshResponse(allianceScienceEntry(111, 0), c.tech)
				}
				return "", obj("scienceId", 10011900, "useNum", 4, "maxNum", 30)
			})
			if err := donateRecommendedAllianceTech(client, c.in); err != nil {
				t.Fatalf("donateRecommendedAllianceTech() = %v", err)
			}
			if got := srv.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("requests = %v, want %v", got, c.want)
			}
			for _, m := range srv.requests(donate) {
				if m.Params.GetInt("scienceId") != 10011900 || m.Params.GetInt("option") != 1 {
					t.Errorf("donate params = %v", m.Params)
				}
			}
		})
	}
}

// donateStep is the fake server's answer to one al.science.donate: pushes sent before the reply,
// then the reply.
type donateStep struct {
	pushes []*sfs.SFSObject // each sent as push.al.science
	reply  *sfs.SFSObject
}

// serveDonations answers science.data.refresh with tech, al.science.refreshNum with useNum, and
// the n-th al.science.donate with steps[n] (the last step repeats).
func serveDonations(t *testing.T, server *session.GameConn, tech *sfs.SFSObject, useNum int, steps []donateStep) *scriptedServer {
	t.Helper()
	s := &scriptedServer{}
	go func() {
		n := 0
		for {
			msg, err := session.ReadNextExtension(server)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.reqs = append(s.reqs, msg)
			s.mu.Unlock()
			var out []*sfs.SFSObject
			cmds := []string{msg.Cmd}
			switch msg.Cmd {
			case "science.data.refresh":
				out = []*sfs.SFSObject{allianceScienceRefreshResponse(tech)}
			case "al.science.refreshNum":
				out = []*sfs.SFSObject{obj("scienceId", tech.GetInt("scienceId"), "useNum", useNum, "maxNum", 30)}
			case "al.science.donate":
				st := steps[min(n, len(steps)-1)]
				n++
				cmds = nil
				for _, p := range st.pushes {
					out, cmds = append(out, p), append(cmds, allianceSciencePushCmd)
				}
				out, cmds = append(out, st.reply), append(cmds, msg.Cmd)
			default:
				out = []*sfs.SFSObject{obj()}
			}
			for i, o := range out {
				if err := server.SendExtension(cmds[i], o); err != nil {
					return
				}
			}
		}
	}()
	return s
}

// TestDonateAllAllianceTechGates checks alliance-donate-all's progress, research and resource
// gates before and inside the loop, and that a reply without progress fields never stops it.
func TestDonateAllAllianceTechGates(t *testing.T) {
	const id = 10011900
	reply := func(useNum int, kv ...any) *sfs.SFSObject {
		return obj(append([]any{"scienceId", id, "useNum", useNum, "maxNum", 30}, kv...)...)
	}
	// The live reply's shape: no currentPro, needPro or level (docs/live-validation.mdx:422).
	countdown := []donateStep{{reply: reply(2)}, {reply: reply(1)}, {reply: reply(0)}}
	open := techEntry(id, "currentPro", 100, "needPro", 1000, "level", 3)
	cases := []struct {
		name        string
		in          *Init
		tech        *sfs.SFSObject
		useNum      int
		steps       []donateStep
		wantDonates int
		wantCmds    []string
	}{
		{"replies without progress fields keep going", inAllianceInit(), open, 3, countdown, 3, nil},
		{"full before the loop: no charges read", inAllianceInit(), techEntry(id, "currentPro", 1000, "needPro", 1000), 3, countdown, 0,
			[]string{"science.data.refresh"}},
		{"researching before the loop", inAllianceInit(), techEntry(id, "finishTime", time.Now().Add(time.Hour).UnixMilli()), 3, countdown, 0,
			[]string{"science.data.refresh"}},
		{"a reply fills the tech", inAllianceInit(), open, 5,
			[]donateStep{{reply: reply(4, "currentPro", 500)}, {reply: reply(3, "currentPro", 1000)}, {reply: reply(2)}}, 2, nil},
		{"a push fills the tech", inAllianceInit(), open, 5,
			[]donateStep{{pushes: []*sfs.SFSObject{obj("scienceId", id, "currentPro", 1000, "needPro", 1000)}, reply: reply(4)}, {reply: reply(3)}}, 1, nil},
		{"a push starts the research", inAllianceInit(), open, 5,
			[]donateStep{{pushes: []*sfs.SFSObject{obj("scienceId", id, "finishTime", time.Now().Add(time.Hour).UnixMilli())}, reply: reply(4)}}, 1, nil},
		{"a push for another tech is ignored", inAllianceInit(), open, 3,
			[]donateStep{{pushes: []*sfs.SFSObject{obj("scienceId", 111, "currentPro", 1000, "needPro", 1000)}, reply: reply(2)}, {reply: reply(1)}, {reply: reply(0)}}, 3, nil},
		{"init wood runs out", inAllianceInit("resource", obj("wood", int64(1000))),
			techEntry(id, "currentPro", 0, "needPro", 1000000, "lowRes", 2, "lowNum", 400), 5, []donateStep{{reply: reply(4)}}, 2, nil},
		{"a reply's resource sets the balance", inAllianceInit("resource", obj("wood", int64(1000))),
			techEntry(id, "currentPro", 0, "needPro", 1000000, "lowRes", 2, "lowNum", 400), 5,
			[]donateStep{{reply: reply(4, "resource", obj("wood", int64(100)))}, {reply: reply(3)}}, 1, nil},
		{"not in an alliance", &Init{Raw: obj()}, open, 3, countdown, 0, []string{}},
		{"no init: replies without progress fields keep going", nil, open, 3, countdown, 3, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveDonations(t, server, c.tech, c.useNum, c.steps)
			if err := donateAllAllianceTech(client, c.in); err != nil {
				t.Fatalf("donateAllAllianceTech() = %v", err)
			}
			if got := len(srv.requests("al.science.donate")); got != c.wantDonates {
				t.Errorf("sent %d al.science.donate, want %d (requests %v)", got, c.wantDonates, srv.cmds())
			}
			if c.wantCmds != nil && !slices.Equal(srv.cmds(), c.wantCmds) {
				t.Errorf("requests = %v, want %v", srv.cmds(), c.wantCmds)
			}
		})
	}
}
