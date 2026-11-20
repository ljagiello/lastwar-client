package game

import (
	"strings"
	"testing"
	"time"

	"lastwar-client/internal/sfs"
)

// The rule-8 stops of five season features (wp10-r1 #2): with two claims due, a refused first
// claim is the last request of that claim loop, and the run reports it. Each case's fixture is its
// feature's own happy-path fixture with the claim command refused.
func TestSeasonClaimsStopOnTheFirstRefusal(t *testing.T) {
	wednesday := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		now   time.Time
		claim string
		run   func(t *testing.T, refuse string) (*evFake, error)
	}{
		{"season-achievements grade", evTestNow, seasonAchieveRewardCmd, func(t *testing.T, refuse string) (*evFake, error) {
			// gradeValue 80000 reaches 50000 and 75000; 25000 is claimed.
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case refuse:
					return evErr("E1")
				case seasonAchieveInfoCmd:
					r := obj("achievementId", p.GetInt("achievementId"), "gradeValue", int64(80000), "gradesInfo",
						[]*sfs.SFSObject{obj("grade", 25000), obj("grade", 50000), obj("grade", 75000)})
					r.PutIntArray("userRewards", []int32{25000})
					return r
				}
				return evOK()
			})
			in := evWithSeason(evInit(30), 6)
			in.Raw.PutSFSObject("user", obj("allianceId", ""))
			return fake, runSeasonAchievements(conn, in)
		}},
		{"gold-tree-prayer day", wednesday, goldTreeCardCmd, func(t *testing.T, refuse string) (*evFake, error) {
			// Tuesday and Wednesday are due (TestGoldTreePrayer).
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case refuse:
					return evErr("E1")
				case goldTreeViewCmd:
					return goldTreeView(wednesday.Add(-time.Hour), wednesday.Add(-30*time.Hour), wednesday.Add(100*time.Hour))
				}
				return evOK()
			})
			act := evActivity(1200021)
			act.PutLong("endTime", wednesday.Add(72*time.Hour).UnixMilli())
			in := evWithSeason(evInit(30, act), 4)
			in.Raw.PutLong("tomorrow", time.Date(2026, 10, 8, 2, 0, 0, 0, time.UTC).Unix())
			return fake, runGoldTreePrayer(conn, in)
		}},
		{"bloody-night-tasks task", evTestNow, bloodyNightRewardCmd, func(t *testing.T, refuse string) (*evFake, error) {
			// Tasks 31 and 32 are claimable (TestBloodyNightTasks).
			ok := bloodyNightReplies(812, 1002)
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				if cmd == refuse {
					return evErr("E1")
				}
				return ok(cmd, p)
			})
			return fake, runBloodyNightTasks(conn, bloodyNightInit())
		}},
		{"jungle-trial-rewards task", evTestNow, jungleTrialRewardCmd, func(t *testing.T, refuse string) (*evFake, error) {
			// No box is earned; tasks 5 and 6 are claimable.
			ok := jungleTrialReplies(3, 3, 1)
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case refuse:
					return evErr("E1")
				case jungleTrialTaskCmd:
					return obj("seasonMonsterEventTaskArr", []*sfs.SFSObject{obj("taskId", 5, "state", 1), obj("taskId", 6, "state", 1)})
				}
				return ok(cmd, p)
			})
			return fake, runJungleTrialRewards(conn, evInit(30, evActivity(1200106)))
		}},
		{"cross-throne-rewards server win", evTestNow, crossThroneWinCmd, func(t *testing.T, refuse string) (*evFake, error) {
			// The own server won and box index 1 is due as well: nothing follows the refused win.
			conn, fake := startEvFake(t, func(cmd string, p *sfs.SFSObject) *sfs.SFSObject {
				switch cmd {
				case refuse:
					return evErr("E1")
				case crossThroneScheduleCmd:
					return crossThroneSchedule(4, nil, nil)
				case crossThroneFightCmd:
					return obj("curVsRound", []*sfs.SFSObject{obj("serverId", 812, "win", 1)},
						"user", seasonScoreUser(250, []int64{100, 200, 300}, []int32{0}, "winServerReward", 0))
				}
				return obj("reward", []*sfs.SFSObject{})
			})
			in := evInit(30)
			in.Raw.PutSFSObject("user", obj("serverId", 812))
			return fake, runCrossThroneRewards(conn, in)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			withEvNow(t, c.now)
			fake, err := c.run(t, c.claim)
			if err == nil {
				t.Error("run = nil, want the refused claim reported")
			}
			cmds := fake.cmds()
			if n := len(fake.only(c.claim)); n != 1 || cmds[len(cmds)-1] != c.claim {
				t.Errorf("sent %s; want exactly one %s, and nothing after it", strings.Join(cmds, ","), c.claim)
			}
		})
	}
}
