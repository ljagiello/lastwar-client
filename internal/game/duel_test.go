package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// duelEntry builds one eventList entry like hero.event.info.get {activityId:"70000"} returns live,
// with the day window in st/et.
func duelEntry(theme int32, score string, begin, end time.Time) *sfs.SFSObject {
	e := sfs.NewSFSObject()
	e.PutInt("t", allianceDuelEventType)
	e.PutInt("eventId", theme)
	e.PutUtfString("actId", "act-1")
	e.PutUtfString("score", score)
	e.PutUtfString("target", "40000|150000|540000")
	e.PutLong("st", begin.UnixMilli())
	e.PutLong("et", end.UnixMilli())
	us := sfs.NewSFSObject()
	us.PutLong("score", 60000)
	e.PutSFSObject("userScore", us)
	return e
}

func duelInfo(entries ...*sfs.SFSObject) *sfs.SFSObject {
	arr := sfs.NewSFSArray()
	for _, e := range entries {
		arr.AddSFSObject(e)
	}
	r := sfs.NewSFSObject()
	r.PutSFSArray("eventList", arr)
	return r
}

func TestParseDuelDayStrictAndScores(t *testing.T) {
	now := evTestNow
	radar := duelEntry(DuelThemeRadar, "90402|90401|90000", now.Add(-time.Hour), now.Add(time.Hour))
	stale := duelEntry(DuelThemeBase, "90201|90202", now.Add(-25*time.Hour), now.Add(-time.Hour))

	d := parseDuelDay(duelInfo(stale, radar), now)
	if d == nil || d.Theme != DuelThemeRadar || d.ThemeName() != "Radar Training" || d.ActID != "act-1" {
		t.Fatalf("parseDuelDay = %+v, want today's radar entry", d)
	}
	if !d.Scores(DuelScoreRadarTask, "") || d.Scores(DuelScoreSpeedUp, DuelQueueBuild) {
		t.Error("radar day must score radar tasks and not construction speed-ups")
	}
	if d.Score != 60000 || d.NextTarget() != 150000 {
		t.Errorf("score/next = %d/%d, want 60000/150000", d.Score, d.NextTarget())
	}
	// Only a stale entry: no fallback, nothing scores.
	if d := parseDuelDay(duelInfo(stale), now); d != nil {
		t.Errorf("a stale entry must not count as today: %+v", d)
	}
	var none *DuelDay
	if none.Scores(DuelScoreRadarTask, "") || none.NextTarget() != 0 || none.ThemeName() != "none" {
		t.Error("a nil day must score nothing")
	}
	base := parseDuelDay(duelInfo(duelEntry(DuelThemeBase, "90201|90202|90203", now.Add(-time.Hour), now.Add(time.Hour))), now)
	if !base.Scores(DuelScoreSpeedUp, DuelQueueBuild) || base.Scores(DuelScoreSpeedUp, DuelQueueResearch) {
		t.Error("base expansion must score construction speed-ups only")
	}
	// The client's own begintime/endtime pair (ActivityEventInfo.lua:69-74) is read too.
	old := sfs.NewSFSObject()
	old.PutInt("t", allianceDuelEventType)
	old.PutInt("eventId", DuelThemeScience)
	old.PutUtfString("score", "90301")
	if d := parseDuelDay(duelInfo(old), now); d != nil {
		t.Errorf("an entry with no window must not count as today: %+v", d)
	}
	old.PutLong("begintime", now.Add(-time.Hour).UnixMilli())
	old.PutLong("endtime", now.Add(time.Hour).UnixMilli())
	if d := parseDuelDay(duelInfo(old), now); d == nil || d.Theme != DuelThemeScience {
		t.Errorf("a begintime/endtime entry must parse: %+v", d)
	}
}

// duelActivity is the duel's init entry as the live push carries it: id 55000 (the activity row)
// with activityid "70000" (the heroactivity row), the id hero.event.info.get must ask for.
func duelActivity() *sfs.SFSObject {
	a := evActivity(55000)
	a.PutUtfString("activityid", "70000")
	return a
}

// duelFake serves an open duel activity whose info reply is the given entries. Like the live
// server, it answers a request for the table id "55000" with a bare success and no eventList.
func duelFake(t *testing.T, entries ...*sfs.SFSObject) (*session.GameConn, *Init) {
	t.Helper()
	withEvNow(t, evTestNow)
	conn, _ := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == "hero.event.info.get" && p.GetString("activityId") == "70000" {
			return duelInfo(entries...)
		}
		return evOK()
	})
	return conn, evInit(30, duelActivity())
}

func TestDuelHeldPolicy(t *testing.T) {
	radarDay := duelEntry(DuelThemeRadar, "90402", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
	baseDay := duelEntry(DuelThemeBase, "90201", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
	radar := Feature{Name: "radar", Duel: []DuelScore{{Type: DuelScoreRadarTask}}, Hold: true}
	always := Feature{Name: "free", Duel: []DuelScore{{Type: DuelScoreRecruitHero}}}
	plain := Feature{Name: "plain", Hold: true}

	conn, in := duelFake(t, radarDay)
	if held, _ := duelHeld(conn, in, radar, nil); held {
		t.Error("radar claims must run on a radar-scoring day")
	}

	conn, in = duelFake(t, baseDay)
	if held, why := duelHeld(conn, in, radar, nil); !held || !strings.Contains(why, "Base Expansion") {
		t.Errorf("radar claims must be held on Base Expansion (held=%v, why=%q)", held, why)
	}
	if held, _ := duelHeld(conn, in, radar, map[string]string{"radar": "always"}); held {
		t.Error("duelPolicy always must override the hold")
	}
	if held, _ := duelHeld(conn, in, always, nil); held {
		t.Error("a scoring feature without Hold must always run")
	}
	if held, _ := duelHeld(conn, in, plain, nil); held {
		t.Error("a feature that scores nothing must never be held")
	}

	conn, in = duelFake(t) // no duel day running (e.g. Sunday)
	if held, why := duelHeld(conn, in, radar, nil); !held || !strings.Contains(why, "no Alliance Duel") {
		t.Errorf("with no duel day a held feature must stay held (held=%v, why=%q)", held, why)
	}
}

func TestRunFeaturesDuelGate(t *testing.T) {
	baseDay := duelEntry(DuelThemeBase, "90201", evTestNow.Add(-time.Hour), evTestNow.Add(time.Hour))
	conn, in := duelFake(t, baseDay)
	ran := withRegistry(t,
		Feature{Name: "radar", DefaultOn: true, Duel: []DuelScore{{Type: DuelScoreRadarTask}}, Hold: true},
		Feature{Name: "speedups", DefaultOn: true, Duel: []DuelScore{{Type: DuelScoreSpeedUp, Value: DuelQueueBuild}}, Hold: true},
	)
	if err := RunFeatures(conn, in, FeatureConfig{}, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(*ran, ","); got != "speedups" {
		t.Errorf("ran %q on Base Expansion, want only speedups (radar held)", got)
	}
	*ran = nil
	if err := RunFeatures(conn, in, FeatureConfig{}, "radar"); err == nil || !strings.Contains(err.Error(), "-run-anyway") {
		t.Errorf("-run of a held feature must be refused, got %v", err)
	}
	if len(*ran) != 0 {
		t.Errorf("a refused -run must not run the feature, ran %v", *ran)
	}
	if err := RunFeatures(conn, in, FeatureConfig{Force: true}, "radar"); err != nil || strings.Join(*ran, ",") != "radar" {
		t.Errorf("-run-anyway must run it: err=%v ran=%v", err, *ran)
	}
}

func TestRadarClaimsIsHeldForRadarDays(t *testing.T) {
	f := featureRegistry["radar-claims"]
	if !f.Hold || len(f.Duel) != 1 || f.Duel[0].Type != DuelScoreRadarTask {
		t.Errorf("radar-claims must be held for radar-scoring days: %+v", f)
	}
}

func TestDuelTablesAreTables40413(t *testing.T) {
	// MASTER B53, B52, B54: duel_scores_gen.go is generated from tables 40413 (genduelscores).
	if len(duelScoreTable) != 1486 || duelScoreTable[350] != (duelScoreRow{typ: 416, value: "5005", points: 2}) {
		t.Errorf("score: %d rows, #350 %+v; want 1486 and the 40413 row {416, 5005, 2}", len(duelScoreTable), duelScoreTable[350])
	}
	if len(duelHeroTable) != 398 || duelHeroTable[50006] != (duelHeroRow{quality: 5, maxRank: 26, maxHonorLevel: 600}) {
		t.Errorf("lw_hero: %d rows, #50006 %+v; want 398 and {5, 26, 600}", len(duelHeroTable), duelHeroTable[50006])
	}
	if r := duelRecruitTable["401100"]; len(duelRecruitTable) != 30 || r.typ != 9 || duelRecruitTable["301112"].timeType != "10" {
		t.Errorf("officer_recruit: %d rows, 401100 %+v; want 30, type 9, and 301112 with time_type 10", len(duelRecruitTable), r)
	}
}

func TestDuelSeasonDay(t *testing.T) {
	// SeasonUtil.GetSeasonDay: (now - seasonStartTime) // 86400000 + 1, from init
	// playerServerSeasonInfo (SeasonInfoTemplate.lua:23, 660-661).
	at := func(kv ...any) *Init {
		in := evInit(10)
		in.Raw.PutSFSObject("playerServerSeasonInfo", duelObj(kv...))
		return in
	}
	start := evTestNow.Add(-10*24*time.Hour - time.Second)
	for name, c := range map[string]struct {
		in          *Init
		season, day int64
		ok          bool
	}{
		"day 11":        {at("seasonId", 4, "seasonStartTime", start.UnixMilli()), 4, 11, true},
		"first day":     {at("seasonId", 2, "seasonStartTime", evTestNow.UnixMilli()), 2, 1, true},
		"no start time": {at("seasonId", 4), 4, 0, false},
		"start time 0":  {at("seasonId", 4, "seasonStartTime", int64(0)), 4, 0, false},
		"season 0":      {at("seasonId", 0, "seasonStartTime", start.UnixMilli()), 0, 0, false},
		"no season":     {evInit(10), 0, 0, false},
	} {
		season, day, ok := duelSeasonDay(c.in, evTestNow)
		if season != c.season || ok != c.ok || (ok && day != c.day) {
			t.Errorf("%s: season %d day %d ok %v, want %d %d %v", name, season, day, ok, c.season, c.day, c.ok)
		}
	}
}
