package game

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"lastwar-client/internal/session"
	"lastwar-client/internal/sfs"
)

// Alliance Duel ("VS") scheduling. The duel runs one theme per server day (Mon radar, Tue base
// expansion, Wed science, Thu heroes, Fri total mobilization, Sat enemy buster, Sun rest), and each
// day's hero.event.info.get {activityId:"55000"} reply lists the score ids that earn points today.
// Features that earn duel points declare the score types they earn (Feature.Duel); a held feature
// (Feature.Hold) runs only on a day that scores one of them, so the work is saved for a day it
// counts. See DUEL.md for the data model, tables and evidence.

//go:generate go run ./genduelscores -in ${LASTWAR_TABLES}/score.json -version ${LASTWAR_TABLE_VERSION} -out duel_scores_gen.go

type duelScoreRow struct {
	typ    int32
	value  string
	points int64
}

// DuelScore names one kind of duel-scoring action: Type is the score table's type (ScoreType,
// EnumType.lua:15728-15738) and Value, when set, narrows it (the queue for speed-ups; empty matches
// any value).
type DuelScore struct {
	Type  int32
	Value string
}

// Score types and speed-up queues (EnumType.lua:15728-15744; DUEL.md §2).
const (
	DuelScoreTrainUnit       int32 = 4   // train one unit (value = unit tier item 3005-3015)
	DuelScoreRecruitHero     int32 = 42  // one hero recruitment
	DuelScoreSpeedUp         int32 = 51  // one minute of speed-up on the queue in Value
	DuelScoreRadarTask       int32 = 82  // complete one radar task
	DuelScoreURTruck         int32 = 98  // dispatch a UR trade truck
	DuelScoreURSecretTask    int32 = 99  // perform a UR secret task
	DuelScoreRecruitSurvivor int32 = 120 // recruit one survivor

	DuelQueueTraining = "4"
	DuelQueueResearch = "6"
	DuelQueueBuild    = "7"
	DuelQueueHeal     = "9"
)

// Duel themes: the day's heroevent row (TB:heroactivity#70000, DUEL.md §1.2).
const (
	DuelThemeRadar        int32 = 110000
	DuelThemeBase         int32 = 110001
	DuelThemeScience      int32 = 110002
	DuelThemeHeroes       int32 = 110003
	DuelThemeMobilization int32 = 110004
	DuelThemeEnemyBuster  int32 = 110005
)

var duelThemeNames = map[int32]string{
	DuelThemeRadar:        "Radar Training",
	DuelThemeBase:         "Base Expansion",
	DuelThemeScience:      "Age of Science",
	DuelThemeHeroes:       "Train Heroes",
	DuelThemeMobilization: "Total Mobilization",
	DuelThemeEnemyBuster:  "Enemy Buster",
}

// DuelDay is today's duel entry.
type DuelDay struct {
	Theme    int32
	ActID    string
	ScoreIDs []int32
	Targets  []int64
	Score    int64     // userScore.score, today's points so far
	End      time.Time // the entry's endtime, the duel day boundary
}

// ThemeName is the theme's English name, or its id when unknown.
func (d *DuelDay) ThemeName() string {
	if d == nil {
		return "none"
	}
	if n, ok := duelThemeNames[d.Theme]; ok {
		return n
	}
	return strconv.Itoa(int(d.Theme))
}

// Scores reports whether today's duel awards points for score type typ (and, when value is
// non-empty, that value), the way the client gates its duel prompts by score type rather than by
// theme (AllianceCompeteDataManager.lua:631-654). A nil day scores nothing.
func (d *DuelDay) Scores(typ int32, value string) bool {
	if d == nil {
		return false
	}
	for _, id := range d.ScoreIDs {
		r, ok := duelScoreTable[id]
		if ok && r.typ == typ && (value == "" || r.value == value) {
			return true
		}
	}
	return false
}

// NextTarget returns the lowest chest threshold above today's score, or 0 when every chest is
// reached.
func (d *DuelDay) NextTarget() int64 {
	if d == nil {
		return 0
	}
	for _, t := range d.Targets {
		if t > d.Score {
			return t
		}
	}
	return 0
}

// TodayDuel returns today's duel entry, or nil when no duel day is running now (Sunday, the ready
// window, not in an alliance, HQ too low) or its info could not be read; callers must treat nil as
// "nothing scores today". It reuses the activity sweep's cached hero.event.info.get reply, so it
// costs at most one request per run. Unlike allianceDuelCurrentEvent it never falls back to a stale
// entry: holding work on the wrong day is the failure this exists to prevent.
func TodayDuel(conn *session.GameConn, in *Init) *DuelDay {
	if in == nil || in.Raw == nil {
		return nil
	}
	s := Activities(conn, in)
	for _, a := range s.Open(actTypeAllianceCompete) {
		info, err := s.EventInfo(a)
		if err != nil {
			slog.Warn("alliance duel: could not read today's duel; duel-held features stay held", "activity", a.ID, "error", err)
			return nil
		}
		if d := parseDuelDay(info, evNow()); d != nil {
			return d
		}
	}
	return nil
}

// parseDuelDay picks the eventList entry of type 15 whose begintime <= now < endtime.
func parseDuelDay(info *sfs.SFSObject, now time.Time) *DuelDay {
	ms := now.UnixMilli()
	for _, e := range evObjects(info, "eventList") {
		if t, ok := evNum(e, "t"); !ok || t != allianceDuelEventType {
			continue
		}
		begin, ok1 := evNum(e, "begintime")
		end, ok2 := evNum(e, "endtime")
		if !ok1 || !ok2 || ms < begin || ms >= end {
			continue
		}
		d := &DuelDay{End: time.UnixMilli(end)}
		if theme, ok := evNum(e, "eventId"); ok {
			d.Theme = int32(theme)
		}
		d.ActID = evString(e, "actId")
		if d.ActID == "" {
			d.ActID = evString(e, "id")
		}
		for _, f := range strings.Split(evString(e, "score"), "|") {
			if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 32); err == nil {
				d.ScoreIDs = append(d.ScoreIDs, int32(n))
			}
		}
		for _, f := range strings.Split(evString(e, "target"), "|") {
			if n, err := strconv.ParseInt(strings.TrimSpace(f), 10, 64); err == nil {
				d.Targets = append(d.Targets, n)
			}
		}
		d.Score, _ = evNum(evObject(e, "userScore"), "score")
		return d
	}
	return nil
}

// LogDuelStatus writes the one line the operator watches: today's theme, points and next chest.
func LogDuelStatus(conn *session.GameConn, in *Init) {
	d := TodayDuel(conn, in)
	if d == nil {
		slog.Info("alliance duel: no duel day running now")
		return
	}
	slog.Info("alliance duel", "theme", d.ThemeName(), "themeId", d.Theme, "score", d.Score,
		"nextChest", d.NextTarget(), "dayEnds", d.End.UTC().Format(time.RFC3339), "scoringIds", len(d.ScoreIDs))
}

// DuelPolicy decides, for one feature, whether today's run is held for the duel. The session
// config's duelPolicy map overrides a feature's default: "hold" or "always".
func duelHeld(conn *session.GameConn, in *Init, f Feature, policy map[string]string) (bool, string) {
	hold := f.Hold
	if p, ok := policy[f.Name]; ok {
		hold = p == "hold"
	}
	if !hold || len(f.Duel) == 0 {
		return false, ""
	}
	d := TodayDuel(conn, in)
	if d == nil {
		return true, "no Alliance Duel day is running now"
	}
	for _, s := range f.Duel {
		if d.Scores(s.Type, s.Value) {
			return false, ""
		}
	}
	return true, "today's duel theme (" + d.ThemeName() + ") doesn't score it"
}
