package game

import (
	"slices"
	"testing"
	"testing/synctest"
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
		if got := tech.blocked(now.UnixMilli(), c.ledger, false); got != c.want {
			t.Errorf("%s: blocked = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestAllianceTechSeasonTab pins MASTER B35: a tab-3 tech of lw_alliance_science_tab (40413:
// 10031100-10032600) is refused out of season (AlScienceDonateInfo.lua:426-430); tabs 1 and 2 and
// unknown ids are not.
func TestAllianceTechSeasonTab(t *testing.T) {
	if tableVersion != "40413" || allianceScienceTab[10031100] != 3 || allianceScienceTab[10032600] != 3 ||
		allianceScienceTab[10011100] != 1 || allianceScienceTab[10021100] != 2 {
		t.Fatalf("generated tab column: version %s, %d %d %d %d", tableVersion, allianceScienceTab[10031100],
			allianceScienceTab[10032600], allianceScienceTab[10011100], allianceScienceTab[10021100])
	}
	n := 0
	for _, tab := range allianceScienceTab {
		if tab == allianceScienceSeasonTab {
			n++
		}
	}
	if n != 16 || len(allianceScienceTab) != 46 {
		t.Errorf("%d tab-3 techs of %d, want 16 of 46", n, len(allianceScienceTab))
	}
	for _, c := range []struct {
		id          int32
		outOfSeason bool
		want        string
	}{
		{10031100, true, "season tech (tab 3) out of season"},
		{10031100, false, ""},
		{10011900, true, ""},
		{10021100, true, ""},
		{555, true, ""},
	} {
		tech := &allianceTech{id: c.id}
		if got := tech.blocked(time.Now().UnixMilli(), nil, c.outOfSeason); got != c.want {
			t.Errorf("tech %d outOfSeason %v: blocked = %q, want %q", c.id, c.outOfSeason, got, c.want)
		}
	}
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	for _, c := range []struct {
		name string
		in   *Init
		want bool
	}{
		{"no init", nil, false},
		{"no raw", &Init{}, false},
		{"no season info", inAllianceInit(), true},
		{"season open", inAllianceInit("playerServerSeasonInfo", obj("open", 1, "seasonEndTime", future)), false},
		{"season ended", inAllianceInit("playerServerSeasonInfo", obj("open", 1, "seasonEndTime", time.Now().Add(-time.Hour).UnixMilli())), true},
		{"season not open", inAllianceInit("playerServerSeasonInfo", obj("open", 0, "seasonEndTime", future)), true},
	} {
		if got := allianceScienceOutOfSeason(c.in); got != c.want {
			t.Errorf("%s: out of season = %v, want %v", c.name, got, c.want)
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
	if tech.currentPro != 1000 || tech.blocked(0, nil, false) != "fully donated" {
		t.Errorf("this tech's progress was not applied: %+v", tech)
	}
}

// TestDonateRecommendedAllianceTechGating runs collectCore's donation against a scripted tree and
// checks which requests go out.
func TestDonateRecommendedAllianceTechGating(t *testing.T) {
	const (
		refresh = "science.data.refresh"
		num     = "al.science.refreshNum"
		donate  = "al.science.donate"
	)
	open := techEntry(10011900, "currentPro", int64(4951850), "needPro", int64(8000000), "level", 3, "lowRes", 2, "lowNum", 400)
	full := techEntry(10011900, "currentPro", int64(8000000), "needPro", int64(8000000), "level", 3)
	season := techEntry(10031100, "currentPro", 0, "needPro", int64(8000000), "level", 0)
	inSeason := obj("open", 1, "seasonEndTime", time.Now().Add(24*time.Hour).UnixMilli())
	cases := []struct {
		name   string
		in     *Init
		tech   *sfs.SFSObject
		useNum int
		want   []string
	}{
		{"no init: donates", nil, open, 4, []string{refresh, num, donate}},
		{"no init: a full tech is still skipped", nil, full, 4, []string{refresh}},
		{"in an alliance", inAllianceInit(), open, 4, []string{refresh, num, donate}},
		{"full", inAllianceInit(), full, 4, []string{refresh}},
		{"not in an alliance", &Init{Raw: obj("user", obj("uid", "1001"))}, open, 4, nil},
		{"empty alliance object", &Init{Raw: obj("alliance", obj())}, open, 4, nil},
		{"science switch off", inAllianceInit("dataConfig", obj(allianceScienceEntrance, 0)), open, 4, nil},
		{"science switch garbled", inAllianceInit("dataConfig", obj(allianceScienceEntrance, "x")), open, 4, nil},
		{"science switch on", inAllianceInit("dataConfig", obj(allianceScienceEntrance, "1")), open, 4, []string{refresh, num, donate}},
		{"science switch absent", inAllianceInit("dataConfig", obj("other_switch", 0)), open, 4, []string{refresh, num, donate}},
		{"too little wood", inAllianceInit("resource", obj("wood", int64(399))), open, 4, []string{refresh}},
		{"enough wood", inAllianceInit("resource", obj("wood", int64(400))), open, 4, []string{refresh, num, donate}},
		// MASTER B32: the charges are read first; with none left nothing is donated.
		{"no charges left", inAllianceInit(), open, 0, []string{refresh, num}},
		{"no init, no charges left", nil, open, 0, []string{refresh, num}},
		{"charge read without useNum", inAllianceInit(), open, -1, []string{refresh, num}},
		// MASTER B35: a tab-3 tech only in season.
		{"season tech out of season", inAllianceInit(), season, 4, []string{refresh}},
		{"season tech in season", inAllianceInit("playerServerSeasonInfo", inSeason), season, 4, []string{refresh, num, donate}},
		{"season tech without init", nil, season, 4, []string{refresh, num, donate}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			id := c.tech.GetInt("scienceId")
			srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
				if msg.Cmd == refresh {
					return "", allianceScienceRefreshResponse(allianceScienceEntry(111, 0), c.tech)
				}
				if c.useNum < 0 {
					return "", obj("scienceId", id, "maxNum", 30)
				}
				return "", obj("scienceId", id, "useNum", c.useNum, "maxNum", 30)
			})
			if err := donateRecommendedAllianceTech(client, c.in); err != nil {
				t.Fatalf("donateRecommendedAllianceTech() = %v", err)
			}
			if got := srv.cmds(); !slices.Equal(got, c.want) {
				t.Errorf("requests = %v, want %v", got, c.want)
			}
			for _, m := range append(srv.requests(donate), srv.requests(num)...) {
				if m.Params.GetInt("scienceId") != id {
					t.Errorf("%s params = %v, want scienceId %d", m.Cmd, m.Params, id)
				}
			}
			for _, m := range srv.requests(donate) {
				if m.Params.GetInt("option") != 1 {
					t.Errorf("donate params = %v", m.Params)
				}
			}
		})
	}
}

// TestDonateRecommendedAllianceTechStops pins how the core donation reads a refused donation:
// 120471 (charges used up) and 120836 (the live day-reset answer, MASTER B33) end the run's
// donations without an error, any other errorCode is returned.
func TestDonateRecommendedAllianceTechStops(t *testing.T) {
	open := techEntry(10011900, "currentPro", 0, "needPro", int64(8000000), "level", 3)
	for _, c := range []struct {
		code    string
		wantErr bool
	}{{"120471", false}, {"120836", false}, {"120020", true}} {
		t.Run(c.code, func(t *testing.T) {
			client, server := session.NewPipeGameConnPair(t)
			srv := serveDonations(t, server, open, 5, []donateStep{{reply: obj("errorCode", c.code)}})
			in := inAllianceInit()
			err := donateRecommendedAllianceTech(client, in)
			if (err != nil) != c.wantErr {
				t.Fatalf("donateRecommendedAllianceTech() = %v, want error %v", err, c.wantErr)
			}
			// alliance-donate-all in the same run sends nothing more after a refusal.
			if err := donateAllAllianceTech(client, in); err != nil {
				t.Fatalf("donateAllAllianceTech() = %v", err)
			}
			if got := srv.cmds(); !slices.Equal(got, []string{"science.data.refresh", "al.science.refreshNum", "al.science.donate"}) {
				t.Errorf("requests = %v", got)
			}
		})
	}
}

// TestDonationGateAfterTheChargeRead checks that the charge read's reply, which the client applies
// to the tech (AlScienceNumFreshMessage.lua:10-19), is gated on before the first donation: a reply
// showing the tech full stops it.
func TestDonationGateAfterTheChargeRead(t *testing.T) {
	const id = 10011900
	client, server := session.NewPipeGameConnPair(t)
	srv := serveScripted(server, func(msg *session.ExtensionMessage) (string, *sfs.SFSObject) {
		if msg.Cmd == "science.data.refresh" {
			return "", allianceScienceRefreshResponse(techEntry(id, "currentPro", 0, "needPro", 1000, "level", 3))
		}
		return "", obj("scienceId", id, "useNum", 5, "maxNum", 30, "currentPro", 1000, "needPro", 1000)
	})
	if err := donateRecommendedAllianceTech(client, inAllianceInit()); err != nil {
		t.Fatal(err)
	}
	if got := srv.cmds(); !slices.Equal(got, []string{"science.data.refresh", "al.science.refreshNum"}) {
		t.Errorf("requests = %v; a full tech is not donated to", got)
	}
}

// TestAllianceDonateUnavailableIsBenign checks that 120836 is registered for al.science.donate
// only: a SendAndWait of the donation is not a failure, of another command it is.
func TestAllianceDonateUnavailableIsBenign(t *testing.T) {
	conn, _ := startEvFake(t, func(string, *sfs.SFSObject) *sfs.SFSObject { return evErr(allianceDonateUnavailableCode) })
	if _, err := session.SendAndWait(conn, "donate", allianceDonateCmd, sfs.NewSFSObject()); err != nil {
		t.Errorf("120836 is not benign for %s: %v", allianceDonateCmd, err)
	}
	if _, err := session.SendAndWait(conn, "charges", allianceScienceNumCmd, sfs.NewSFSObject()); err == nil {
		t.Errorf("120836 must stay a failure for %s", allianceScienceNumCmd)
	}
}

// TestCoreDonationSharesTheRunWithDonateAll is MASTER B32's -collect sequence: the core donation
// and alliance-donate-all on one (conn, init) fetch the tree and read the charges once, and the
// feature continues from the core's donation until the charges run out.
func TestCoreDonationSharesTheRunWithDonateAll(t *testing.T) {
	const id = 10011900
	reply := func(useNum int) *sfs.SFSObject { return obj("scienceId", id, "useNum", useNum, "maxNum", 30) }
	open := techEntry(id, "currentPro", 0, "needPro", int64(8000000), "level", 3)
	client, server := session.NewPipeGameConnPair(t)
	srv := serveDonations(t, server, open, 3, []donateStep{{reply: reply(2)}, {reply: reply(1)}, {reply: reply(0)}})
	in := inAllianceInit()
	if err := donateRecommendedAllianceTech(client, in); err != nil {
		t.Fatal(err)
	}
	if got := srv.cmds(); !slices.Equal(got, []string{"science.data.refresh", "al.science.refreshNum", "al.science.donate"}) {
		t.Fatalf("core requests = %v", got)
	}
	if err := donateAllAllianceTech(client, in); err != nil {
		t.Fatal(err)
	}
	want := []string{"science.data.refresh", "al.science.refreshNum", "al.science.donate", "al.science.donate", "al.science.donate"}
	if got := srv.cmds(); !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v", got, want)
	}
	// Another init (a new login) starts over.
	if err := donateAllAllianceTech(client, inAllianceInit()); err != nil {
		t.Fatal(err)
	}
	if got := srv.cmds(); len(got) != len(want)+3 || got[len(want)] != "science.data.refresh" || got[len(want)+1] != "al.science.refreshNum" {
		t.Errorf("a new init must fetch the tree and the charges again: %v", got)
	}
}

// TestDonationOverADeadConnectionEndsTheRunsDonations is the dead-connection branch of
// allianceScienceRun.failed (final-r3 prod N2): the server goes away at the core's first
// al.science.donate, so the wait fails with a non-timeout net.Error (sfs.DeadConnError). That is
// not a timeout (waitTimedOut), so the run's donations stop with "donation failed", and
// alliance-donate-all neither reads the charges again nor tries to send.
func TestDonationOverADeadConnectionEndsTheRunsDonations(t *testing.T) {
	const id = 10011900
	synctest.Test(t, func(t *testing.T) {
		client, server := session.NewPipeGameConnPair(t)
		srv := &scriptedServer{}
		go func() {
			for {
				msg, err := session.ReadNextExtension(server)
				if err != nil {
					return
				}
				srv.mu.Lock()
				srv.reqs = append(srv.reqs, msg)
				srv.mu.Unlock()
				var out *sfs.SFSObject
				switch msg.Cmd {
				case "science.data.refresh":
					out = allianceScienceRefreshResponse(techEntry(id, "currentPro", 0, "needPro", int64(8000000), "level", 3))
				case "al.science.refreshNum":
					out = obj("scienceId", id, "useNum", 5, "maxNum", 30)
				default: // al.science.donate: the connection dies instead of answering
					_ = server.Close()
					return
				}
				if v, ok := msg.Params.Get("_id"); ok {
					out.PutValue("_id", v)
				}
				if err := server.SendExtension(msg.Cmd, out); err != nil {
					return
				}
			}
		}()
		in := inAllianceInit()
		errCore := donateRecommendedAllianceTech(client, in)
		if !session.ContainsNonTimeoutNetError(errCore) {
			t.Fatalf("core donation error = %v, want a dead connection", errCore)
		}
		run := cachedAllianceScience(client, in)
		if run == nil || run.stop != "donation failed" {
			t.Fatalf("run state = %+v, want donations stopped with \"donation failed\"", run)
		}
		if err := donateAllAllianceTech(client, in); err != nil {
			t.Errorf("donateAllAllianceTech() = %v, want nil: the run's donations already ended", err)
		}
		if run.stop != "donation failed" || !run.chargesRead {
			t.Errorf("after alliance-donate-all: stop %q, chargesRead %v; want the stop kept and no charge re-read",
				run.stop, run.chargesRead)
		}
		want := []string{"science.data.refresh", "al.science.refreshNum", "al.science.donate"}
		if got := srv.cmds(); !slices.Equal(got, want) {
			t.Errorf("requests = %v, want %v", got, want)
		}
	})
}

// TestDonateAllRecoversFromATimeoutInTheCore is the -collect sequence when the server leaves one
// request of the core donation unanswered: a timed-out charge read or donation does not end the
// run's donations, so alliance-donate-all reads the charges again (a timed-out donation may have
// gone through) and donates. An errorCode answer to the charge read still ends them. It runs in a
// synctest bubble, so the 8 s DefaultCmdTimeout passes on the fake clock.
func TestDonateAllRecoversFromATimeoutInTheCore(t *testing.T) {
	const id = 10011900
	for _, c := range []struct {
		name      string
		withhold  string // the first request of this cmd gets no reply
		chargeErr bool   // al.science.refreshNum is answered with a genuine errorCode
		want      []string
	}{
		{"charge read times out", "al.science.refreshNum", false,
			[]string{"science.data.refresh", "al.science.refreshNum", "al.science.refreshNum", "al.science.donate"}},
		{"first donation times out", "al.science.donate", false,
			[]string{"science.data.refresh", "al.science.refreshNum", "al.science.donate", "al.science.refreshNum", "al.science.donate"}},
		{"charge read refused", "", true,
			[]string{"science.data.refresh", "al.science.refreshNum"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client, server := session.NewPipeGameConnPair(t)
				srv := &scriptedServer{}
				go func() {
					held := false
					for {
						msg, err := session.ReadNextExtension(server)
						if err != nil {
							return
						}
						srv.mu.Lock()
						srv.reqs = append(srv.reqs, msg)
						srv.mu.Unlock()
						if msg.Cmd == c.withhold && !held {
							held = true
							continue
						}
						var out *sfs.SFSObject
						switch msg.Cmd {
						case "science.data.refresh":
							out = allianceScienceRefreshResponse(techEntry(id, "currentPro", 0, "needPro", int64(8000000), "level", 3))
						case "al.science.refreshNum":
							out = obj("scienceId", id, "useNum", 5, "maxNum", 30)
							if c.chargeErr {
								out = obj("errorCode", "999999")
							}
						case "al.science.donate":
							out = obj("scienceId", id, "useNum", 0, "maxNum", 30)
						default:
							out = obj()
						}
						if v, ok := msg.Params.Get("_id"); ok {
							out.PutValue("_id", v)
						}
						if err := server.SendExtension(msg.Cmd, out); err != nil {
							return
						}
					}
				}()
				in := inAllianceInit()
				errCore := donateRecommendedAllianceTech(client, in)
				if errCore == nil || session.ContainsNonTimeoutNetError(errCore) {
					t.Errorf("core donation error = %v, want a timeout or errorCode error", errCore)
				}
				if err := donateAllAllianceTech(client, in); err != nil {
					t.Errorf("donateAllAllianceTech() = %v", err)
				}
				if got := srv.cmds(); !slices.Equal(got, c.want) {
					t.Errorf("requests = %v, want %v", got, c.want)
				}
			})
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
