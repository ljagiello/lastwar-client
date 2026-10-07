package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// finishedAt is a FINISHED task that spawned age ago.
func finishedAt(uuid, eventID int64, age time.Duration) *sfs.SFSObject {
	e := detectEv(uuid, eventID, radarStateFinished)
	e.PutLong("startTime", time.Now().Add(-age).UnixMilli())
	return e
}

// fullSlots is a level-16 slot list (12 shown slots): the given tasks, then unstarted kill-zombie
// tasks up to n.
func fullSlots(n int, tasks ...*sfs.SFSObject) []*sfs.SFSObject {
	for i := len(tasks); i < n; i++ {
		tasks = append(tasks, detectEv(int64(900+i), 205, 0))
	}
	return tasks
}

func TestRadarOverflowDoesNothingOnRadarDays(t *testing.T) {
	info := detectReply(16, 40, fullSlots(12, finishedAt(1, 100, time.Hour))...)
	conn, fake := startRadarFake(t, radarScoringDay(), []*sfs.SFSObject{info}, nil)
	if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := fake.cmds(); !slices.Equal(got, []string{"hero.event.info.get"}) {
		t.Errorf("sent %v on a radar day, want only the duel read", got)
	}
}

func TestRadarOverflowClaimsExactlyTheOverflow(t *testing.T) {
	// Level 16: 12 slots, stock cap 40, 13 per refresh. All 12 slots are taken and stock is 30,
	// so the next refresh brings 13 into a stock with room for 10: claim 3.
	tasks := fullSlots(12,
		finishedAt(1, 199, 9*time.Hour),    // rescue: never
		finishedAt(2, 410001, 9*time.Hour), // cockatrice guide: never
		finishedAt(3, 999999, 9*time.Hour), // not in the table: never
		finishedAt(4, 100, 1*time.Hour),
		finishedAt(5, 24000, 5*time.Hour),
		finishedAt(6, 101, 3*time.Hour),
		finishedAt(7, 102, 7*time.Hour),
		finishedAt(8, 103, 2*time.Hour),
	)
	for _, duel := range [][]*sfs.SFSObject{radarOffDay(), nil} { // an off day, and no duel day (Sunday)
		conn, fake := startRadarFake(t, duel, []*sfs.SFSObject{detectReply(16, 30, tasks...)}, nil)
		if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
			t.Fatalf("run: %v", err)
		}
		if got := radarUUIDs(fake, radarEventCmd); !slices.Equal(got, []int64{7, 5, 6}) {
			t.Errorf("claimed %v, want the three oldest eligible [7 5 6]", got)
		}
		for _, c := range fake.cmds() {
			if c != "hero.event.info.get" && c != radarInfoCmd && c != radarEventCmd {
				t.Errorf("radar-overflow sent %q", c)
			}
		}
	}
}

func TestRadarOverflowClaimsNothingWithoutOverflow(t *testing.T) {
	old := finishedAt(1, 100, 9*time.Hour)
	cases := []struct {
		name string
		info *sfs.SFSObject
	}{
		// 27 + 13 = 40: exactly at the cap.
		{"stock room", detectReply(16, 27, fullSlots(12, old)...)},
		// 9 of 12 slots used: 3 of the 13 fill slots, 30 + 10 = 40.
		{"free slots", detectReply(16, 30, fullSlots(9, old)...)},
	}
	for _, c := range cases {
		conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{c.info}, nil)
		if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
			t.Fatalf("%s: run: %v", c.name, err)
		}
		if got := radarUUIDs(fake, radarEventCmd); len(got) != 0 {
			t.Errorf("%s: claimed %v, want nothing", c.name, got)
		}
	}
}

func TestRadarOverflowClaimsNothingOnMissingInputs(t *testing.T) {
	old := finishedAt(1, 100, 9*time.Hour)
	noEventNum := detectReply(16, 50, fullSlots(12, old)...)
	d := sfs.NewSFSObject()
	d.PutInt("level", 16)
	noEventNum.PutSFSObject("detectInfo", d)
	cases := map[string]*sfs.SFSObject{
		"no eventNum":   noEventNum,
		"unknown level": detectReply(21, 50, fullSlots(12, old)...),
	}
	for name, info := range cases {
		conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{info}, nil)
		if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
			t.Fatalf("%s: run: %v", name, err)
		}
		if got := radarUUIDs(fake, radarEventCmd); len(got) != 0 {
			t.Errorf("%s: claimed %v, want nothing", name, got)
		}
	}
}

func TestRadarOverflowShortOfFinishedTasks(t *testing.T) {
	// Overflow 10 (stock 37), but only two claimable tasks: claim both, and the claim failure of
	// one is reported.
	info := detectReply(16, 37, fullSlots(12, finishedAt(1, 100, time.Hour), finishedAt(2, 101, 2*time.Hour))...)
	conn, fake := startRadarFake(t, radarOffDay(), []*sfs.SFSObject{info}, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
		if cmd == radarEventCmd && p.GetLong("uuid") == 2 {
			return evErr("999999")
		}
		return nil
	})
	if err := runRadarOverflow(conn, radarTestInit(110, true)); err == nil {
		t.Error("run = nil, want the failed claim")
	}
	if got := radarUUIDs(fake, radarEventCmd); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("claimed %v, want [2 1]", got)
	}
}

func TestRadarOverflowHoldsWhenTomorrowScoresAndTheRefreshIsAfterTheDayEnd(t *testing.T) {
	// Live 2026-10-06: Tuesday (Base Expansion), stock at its cap, and the next refresh 12 minutes
	// after Wednesday's radar day starts. The day-start run claims for points first, so nothing is
	// claimed today. On Saturday (Enemy Buster) the next day is the Sunday rest day, so the
	// overflow is claimed.
	tasks := fullSlots(12, finishedAt(1, 100, time.Hour), finishedAt(2, 101, 2*time.Hour))
	late := func(theme int32) ([]*sfs.SFSObject, *sfs.SFSObject) {
		end := evTestNow.Add(time.Hour)
		info := detectReply(16, 40, tasks...)
		dv, _ := info.Get("detectInfo")
		dv.Val.(*sfs.SFSObject).PutLong("nextRefreshTime", end.Add(12*time.Minute).UnixMilli())
		return []*sfs.SFSObject{duelEntry(theme, "90201", evTestNow.Add(-time.Hour), end)}, info
	}
	duel, info := late(DuelThemeBase)
	conn, fake := startRadarFake(t, duel, []*sfs.SFSObject{info}, nil)
	if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
		t.Fatal(err)
	}
	if got := radarUUIDs(fake, radarEventCmd); len(got) != 0 {
		t.Errorf("claimed %v on the day before a radar day, want none", got)
	}
	duel, info = late(DuelThemeEnemyBuster)
	conn, fake = startRadarFake(t, duel, []*sfs.SFSObject{info}, nil)
	if err := runRadarOverflow(conn, radarTestInit(110, true)); err != nil {
		t.Fatal(err)
	}
	if got := radarUUIDs(fake, radarEventCmd); !slices.Equal(got, []int64{2, 1}) {
		t.Errorf("claimed %v on Saturday, want the overflow [2 1]", got)
	}
}
