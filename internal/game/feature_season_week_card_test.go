package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// seasonWeekCardActivity is the S6 season week card entry (type 228) open around evTestNow and
// started at start.
func seasonWeekCardActivity(start time.Time) *sfs.SFSObject {
	a := evActivity(1200111)
	a.PutLong("startTime", start.UnixMilli())
	return a
}

// seasonWeekCardInit is an S6 init with the season week card activity started two days ago.
func seasonWeekCardInit() *Init {
	return evWithSeason(evInit(30, seasonWeekCardActivity(evTestNow.Add(-48*time.Hour))), 6)
}

// seasonWeekCardReply answers lw.season.week.card.info with the card fields
// SeasonPeriodicCardData:ParseData reads; a zero time leaves its key out.
func seasonWeekCardReply(cardID int32, end, daily, free time.Time) func(string, *sfs.SFSObject) *sfs.SFSObject {
	return func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd != seasonWeekCardInfoCmd {
			return evOK()
		}
		r := sfs.NewSFSObject()
		r.PutInt("card_id", cardID)
		for k, t := range map[string]time.Time{"end_time": end, "daily_reward": daily, "free_reward": free} {
			if !t.IsZero() {
				r.PutLong(k, t.UnixMilli())
			}
		}
		return r
	}
}

func TestSeasonWeekCardID(t *testing.T) {
	withEvNow(t, evTestNow)
	in := evInit(30)
	dayStart, ok := in.ServerDayStart(evTestNow)
	if !ok {
		t.Fatal("no server day")
	}
	if off := seasonServerOffset(dayStart); off != -2*time.Hour {
		t.Fatalf("offset of a 02:00 UTC reset = %v, want -2h (UTC-2)", off)
	}
	if off := seasonServerOffset(time.Date(2026, 10, 5, 22, 0, 0, 0, time.UTC)); off != 2*time.Hour {
		t.Errorf("offset of a 22:00 UTC reset = %v, want +2h", off)
	}
	act := func(id int32, start time.Time) Activity {
		raw := evActivity(id)
		raw.PutLong("startTime", start.UnixMilli())
		return Activity{ID: id, Type: actTypeSeasonPeriodicCard, Raw: raw}
	}
	for name, c := range map[string]struct {
		a    Activity
		want int32
	}{
		// Server midnight of 2026-07-29 is 02:00 UTC.
		"started the server day before the swap date": {act(1200111, time.Date(2026, 7, 29, 1, 59, 0, 0, time.UTC)), 10007},
		"started on the swap date":                    {act(1200111, time.Date(2026, 7, 29, 2, 0, 0, 0, time.UTC)), 10013},
		"S1 card, started after the swap":             {act(1000021, evTestNow.Add(-48*time.Hour)), 10008},
		"S1 card, started before the swap":            {act(1000021, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)), 10002},
		"no start time keeps para":                    {Activity{ID: 1200052, Raw: sfs.NewSFSObject()}, 10006},
	} {
		got, ok := seasonWeekCardID(c.a, dayStart)
		if !ok || got != c.want {
			t.Errorf("%s: card %d (ok %v), want %d", name, got, ok, c.want)
		}
	}
	if _, ok := seasonWeekCardID(act(1200101, evTestNow), dayStart); ok {
		t.Error("an activity outside the type-228 table must not get a card id")
	}
}

func TestSeasonWeekCardClaimsFreeChestWithoutCard(t *testing.T) {
	withEvNow(t, evTestNow)
	conn, fake := startEvFake(t, seasonWeekCardReply(10013, time.Time{}, time.Time{}, time.Time{}))
	if err := runSeasonWeekCard(conn, seasonWeekCardInit()); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fake.cmds(), ","); got != "lw.season.week.card.info,lw.season.week.card.free.reward" {
		t.Fatalf("sent %s, want the info read and the free chest only (no card bought)", got)
	}
	for _, r := range fake.requests() {
		if v, _ := r.Params.Get("card_id"); v.Type != sfs.SFSInt || r.Params.GetInt("card_id") != 10013 {
			t.Errorf("%s card_id = %#v, want Int 10013 (the swapped card of 1200111)", r.Cmd, v)
		}
	}
}

func TestSeasonWeekCardClaimsBoughtCardDailyFirst(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	conn, fake := startEvFake(t, seasonWeekCardReply(10013, evTestNow.Add(72*time.Hour), yesterday, yesterday))
	if err := runSeasonWeekCard(conn, seasonWeekCardInit()); err != nil {
		t.Fatal(err)
	}
	want := "lw.season.week.card.info,lw.season.week.card.daily.reward,lw.season.week.card.free.reward"
	if got := strings.Join(fake.cmds(), ","); got != want {
		t.Fatalf("sent %s\nwant %s", got, want)
	}
}

func TestSeasonWeekCardSkips(t *testing.T) {
	withEvNow(t, evTestNow)
	today := evTestTomorrow.Add(-time.Hour)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	switchOff := seasonWeekCardInit()
	dc := sfs.NewSFSObject()
	dc.PutInt(freeChestSwitch, 0)
	switchOff.Raw.PutSFSObject("dataConfig", dc)
	for name, c := range map[string]struct {
		in    *Init
		reply func(string, *sfs.SFSObject) *sfs.SFSObject
		want  string
	}{
		"not in a season": {evInit(30, seasonWeekCardActivity(evTestNow.Add(-48*time.Hour))),
			seasonWeekCardReply(10013, time.Time{}, time.Time{}, time.Time{}), ""},
		"no season week card activity": {evWithSeason(evInit(30, evActivity(1200101)), 6),
			seasonWeekCardReply(10013, time.Time{}, time.Time{}, time.Time{}), ""},
		"HQ below the activity's level 4": {evWithSeason(evInit(3, seasonWeekCardActivity(evTestNow.Add(-48*time.Hour))), 6),
			seasonWeekCardReply(10013, time.Time{}, time.Time{}, time.Time{}), ""},
		"both claimed today": {seasonWeekCardInit(),
			seasonWeekCardReply(10013, evTestNow.Add(72*time.Hour), today, today), "lw.season.week.card.info"},
		"card expired, free chest claimed today": {seasonWeekCardInit(),
			seasonWeekCardReply(10013, evTestNow.Add(-time.Hour), yesterday, today), "lw.season.week.card.info"},
		"reply for another card": {seasonWeekCardInit(),
			seasonWeekCardReply(10007, time.Time{}, time.Time{}, time.Time{}), "lw.season.week.card.info"},
		"free_chest_receive off": {switchOff,
			seasonWeekCardReply(10013, time.Time{}, time.Time{}, time.Time{}), "lw.season.week.card.info"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, fake := startEvFake(t, c.reply)
			if err := runSeasonWeekCard(conn, c.in); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(fake.cmds(), ","); got != c.want {
				t.Errorf("sent %q, want %q", got, c.want)
			}
		})
	}
}

func TestSeasonWeekCardErrorsAreNotBenign(t *testing.T) {
	withEvNow(t, evTestNow)
	yesterday := evTestTomorrow.Add(-25 * time.Hour)
	base := seasonWeekCardReply(10013, evTestNow.Add(72*time.Hour), yesterday, yesterday)
	conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == seasonWeekCardDailyCmd {
			return evErr("120289")
		}
		return base(cmd, p)
	})
	err := runSeasonWeekCard(conn, seasonWeekCardInit())
	if err == nil || !strings.Contains(err.Error(), "120289") {
		t.Errorf("err = %v, want the daily's 120289: no code is benign before a live reply", err)
	}
	if n := len(fake.only(seasonWeekCardFreeCmd)); n != 1 {
		t.Errorf("free chest sent %d times, want 1: the daily's failure doesn't stop the free claim", n)
	}

	conn2, fake2 := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject { return evErr("E5") })
	if err := runSeasonWeekCard(conn2, seasonWeekCardInit()); err == nil || !strings.Contains(err.Error(), "E5") {
		t.Errorf("err = %v, want the info read's E5", err)
	}
	if got := strings.Join(fake2.cmds(), ","); got != "lw.season.week.card.info" {
		t.Errorf("after a failed info read sent %s", got)
	}
}

func TestSeasonWeekCardIsOptIn(t *testing.T) {
	f, ok := featureRegistry[seasonWeekCardFeature]
	if !ok {
		t.Fatal("season-week-card is not registered")
	}
	if f.DefaultOn || !strings.HasSuffix(f.Summary, "(static-only, not validated live)") {
		t.Errorf("feature = %+v, want DefaultOn false and the static-only summary", f)
	}
}
