package game

import (
	"slices"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// WP-Z touch point 4 (MASTER B72, cov-dc-social-city/F6): the client reads a building's upgrade end
// from the build queue that occupies it when there is one (BuildQueueManager.lua:18-55,
// BuildQueueInfo.lua:43-52), so alliance-call-help and duel-speedups read it through
// buildingUpgradeTime, as finished-timers does.

// withBuildQueue replaces init's queue_new with entries.
func withBuildQueue(in *Init, entries ...*sfs.SFSObject) *Init {
	qs := sfs.NewSFSArray()
	for _, e := range entries {
		qs.AddSFSObject(e)
	}
	in.Raw.PutSFSArray("queue_new", qs)
	return in
}

// TestAllianceCallHelpReadsTheBuildQueueUpgradeTime: the building's own uT has passed, but the
// build queue on it runs another hour, so the upgrade bubble is up and help is asked; a queue that
// says the upgrade has ended asks nothing although the building's own uT is still ahead.
func TestAllianceCallHelpReadsTheBuildQueueUpgradeTime(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name    string
		ownUT   time.Time
		queueUT time.Time
		want    []helpSent
	}{
		{"queue still running", now.Add(-time.Minute), now.Add(time.Hour), []helpSent{{4, 0, 0, ""}}},
		{"queue ended", now.Add(time.Hour), now.Add(-time.Minute), nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := callHelpInit(timerInit{
				buildings: []timerBuilding{{uuid: 4, bID: 10202000, lv: 5, uT: c.ownUT}},
				soldiers:  -1,
			}, true)
			withBuildQueue(in, buildQueueEntry(0, "4", c.queueUT.UnixMilli()))
			sent, err := runCallHelp(t, in, rewardOK())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if !slices.Equal(sent, c.want) {
				t.Errorf("sent %v, want %v", sent, c.want)
			}
		})
	}
}

// TestDuelSpeedupsReadsTheBuildQueueUpgradeTime: a building whose own uT is unset but whose build
// queue runs another hour is an upgrade job; one whose queue says it ended is none.
func TestDuelSpeedupsReadsTheBuildQueueUpgradeTime(t *testing.T) {
	now := evTestNow.UnixMilli()
	for _, c := range []struct {
		name    string
		ownUT   int64
		queueUT int64
		want    int
	}{
		{"queue still running", 0, now + time.Hour.Milliseconds(), 1},
		{"queue ended", now + time.Hour.Milliseconds(), now - time.Minute.Milliseconds(), 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := NewTestBuildingSFS(11, BuildingIronMine, 5)
			if c.ownUT > 0 {
				b.PutLong("uT", c.ownUT)
			}
			raw := sfs.NewSFSObject()
			bs := sfs.NewSFSArray()
			bs.AddSFSObject(b)
			raw.PutSFSArray("building_new", bs)
			in := withBuildQueue(&Init{Raw: raw}, buildQueueEntry(0, "11", c.queueUT))
			n := 0
			for _, j := range duelSpeedupsJobs(in, now) {
				if j.kind == duelSpeedupBuild && j.uuid == 11 {
					n++
					if j.endMs != c.queueUT {
						t.Errorf("job ends at %d, want the queue's %d", j.endMs, c.queueUT)
					}
				}
			}
			if n != c.want {
				t.Errorf("%d upgrade jobs for building 11, want %d", n, c.want)
			}
		})
	}
}
