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
